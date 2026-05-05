package http

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"flomation.app/automate/launch/internal/assets"

	"flomation.app/automate/launch"

	"flomation.app/automate/launch/internal/agent"
	githubwh "flomation.app/automate/launch/internal/github"
	gitlabwh "flomation.app/automate/launch/internal/gitlab"
	"flomation.app/automate/launch/internal/google"
	"flomation.app/automate/launch/internal/mtls"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"

	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/version"
	"github.com/flomation-co/sentinel-client"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

type Service struct {
	config         *config.Config
	engine         *gin.Engine
	internalEngine *gin.Engine  // mTLS-only listener for internal routes
	apiClient      *http.Client // mTLS-capable client for internal API calls
	trigger        *trigger.Service
	agent          *agent.Service
	google         *google.Service
	db             *persistence.Service
}

func NewService(config *config.Config, trigger *trigger.Service, agentSvc *agent.Service, googleSvc *google.Service, db *persistence.Service) (*Service, error) {
	gin.SetMode(gin.ReleaseMode)

	apiClient, err := mtls.ClientOrDefault(config.TLS, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("http: unable to create API client: %w", err)
	}

	s := Service{
		config:    config,
		engine:    gin.New(),
		apiClient: apiClient,
		google:    googleSvc,
		db:        db,
		trigger:   trigger,
		agent:     agentSvc,
	}

	templ := template.Must(template.ParseFS(assets.Templates, "files/form.html"))
	s.engine.SetHTMLTemplate(templ)

	if err := s.configure(); err != nil {
		return nil, err
	}

	return &s, nil
}

func corsMiddleware(c *gin.Context) {
	c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
	c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	if c.Request.Method == "OPTIONS" {
		c.AbortWithStatus(204)
		return
	}

	c.Next()
}

func (s *Service) configure() error {
	s.engine.Use(corsMiddleware)

	s.engine.GET("version", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version": version.Version,
			"date":    version.BuiltDate,
			"hash":    version.GetHash(),
		})
	})

	s.engine.GET("health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	s.engine.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		b, err := assets.Templates.ReadFile("files" + p)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("unable to read file")
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}

		contentType := http.DetectContentType(b)

		c.Data(http.StatusOK, contentType, b)
	})

	s.engine.GET("/webhook/:id", s.handleWebhook)
	s.engine.POST("/webhook/:id", s.handleWebhook)
	s.engine.GET("/qr/:id", s.handleQr)
	s.engine.GET("/form/:id", s.handleForm)
	s.engine.POST("/form/:id", s.submitForm)
	s.engine.GET("/image/:id", s.handleImageLoad)

	// Internal routes — service-to-service calls from the API.
	// When mTLS is enabled, these register on a separate Gin engine
	// served on the internal port with client certificate verification.
	internalRouter := s.engine
	if s.config.TLS != nil && s.config.TLS.Enabled {
		gin.SetMode(gin.ReleaseMode)
		s.internalEngine = gin.New()
		internalRouter = s.internalEngine
	}

	// Trigger management (internal, called by API service)
	triggerAdmin := internalRouter.Group("/trigger")
	triggerAdmin.POST("/:id", s.createTrigger)
	triggerAdmin.DELETE("/:id", s.deleteTrigger)

	// Agent registration (internal, called by API service)
	agentAdmin := internalRouter.Group("/agent")
	agentAdmin.POST("/:id", s.registerAgent)
	agentAdmin.DELETE("/:id", s.deregisterAgent)

	// Identity verification dispatch (internal, called by API)
	internalRouter.POST("/internal/agent/:agent_id/verify-identity", s.handleVerifyIdentity)

	// Channel actions — typing indicators, etc. (internal, called by executor)
	internalRouter.POST("/internal/agent/:agent_id/channel-action", s.handleChannelAction)

	// Google token exchange (internal, called by executor tool actions)
	internalRouter.GET("/internal/google/tokens/trigger/:id", s.handleGoogleTokensTrigger)
	internalRouter.GET("/internal/google/tokens/:agent_user_id", s.handleGoogleTokens)

	// Agent inbound webhooks (edge-facing, no auth — validated by agent ID)
	// These stay on the public engine — external services hit them directly.
	s.engine.POST("/webhook/agent/:agent_id", s.handleAgentWebhook)
	s.engine.POST("/webhook/telegram/:agent_id", s.handleTelegramWebhook)
	s.engine.POST("/webhook/slack/:agent_id", s.handleSlackWebhook)

	// Slack interactivity — Block Kit button clicks, select menus, etc.
	// Configure in Slack App Settings → Interactivity & Shortcuts → Request URL.
	s.engine.POST("/slack/:agent_id/interact", s.handleSlackInteraction)

	// Google OAuth2 (public, browser-facing)
	s.engine.GET("/auth/google/callback", s.handleGoogleAuthCallback)
	s.engine.GET("/auth/google/trigger/:trigger_id", s.handleGoogleAuthInitiateTrigger)
	s.engine.GET("/auth/google/:agent_user_id", s.handleGoogleAuthInitiate)

	return nil
}

func (s *Service) Listen() error {
	if s.internalEngine != nil {
		go s.listenInternal()
	}
	return s.engine.Run(fmt.Sprintf("%v:%v", s.config.HttpListenConfig.Address, s.config.HttpListenConfig.Port))
}

// listenInternal starts the mTLS-protected internal listener on a
// separate port. Only clients presenting a valid certificate signed
// by the platform CA are accepted.
func (s *Service) listenInternal() {
	tlsCfg, err := mtls.NewServerTLSConfig(s.config.TLS)
	if err != nil {
		log.WithError(err).Fatal("unable to configure mTLS server")
	}

	addr := fmt.Sprintf("%v:%d", s.config.HttpListenConfig.Address, s.config.TLS.InternalPort)
	server := &http.Server{
		Addr:              addr,
		Handler:           s.internalEngine,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.WithFields(log.Fields{
		"address": addr,
	}).Info("starting mTLS internal listener")

	if err := server.ListenAndServeTLS(s.config.TLS.CertFile, s.config.TLS.KeyFile); err != nil && err != http.ErrServerClosed {
		log.WithError(err).Fatal("mTLS internal listener failed")
	}
}

func (s *Service) handleImageLoad(c *gin.Context) {
	id := c.Param("id")

	if err := uuid.Validate(id); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if tr == nil {
		log.WithFields(log.Fields{
			"id": id,
		}).Error("trigger ID not found")
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if tr.Type != launch.TriggerTypeImage {
		log.WithFields(log.Fields{
			"id":   id,
			"type": tr.Type,
		}).Error("mismatched trigger type")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	ip := c.ClientIP()
	userAgent := c.Request.UserAgent()
	referrer := c.Request.Referer()

	go func() {
		data := map[string]interface{}{
			"ip":           ip,
			"user_agent":   userAgent,
			"referrer":     referrer,
			"triggered_at": time.Now().UTC().Format(time.RFC3339),
		}

		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("unable to fire trigger")
		}
	}()

	//	TODO: Make this static, or allow users to alter the settings
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{
		R: 0,
		G: 0,
		B: 0,
		A: 0,
	})

	buf := new(bytes.Buffer)
	if err := png.Encode(buf, img); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	c.Data(http.StatusOK, "image/png", buf.Bytes())
}

func (s *Service) handleWebhook(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if err := uuid.Validate(id); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if tr == nil {
		log.WithFields(log.Fields{
			"id": id,
		}).Error("trigger ID not found")
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	// Route to provider-specific handler for GitLab/GitHub webhooks
	switch tr.Type {
	case launch.TriggerTypeGitLabWebhook, launch.TriggerTypeGitHubWebhook:
		s.handleProviderWebhookForTrigger(c, tr)
		return
	case launch.TriggerTypeWebhook:
		// Continue with generic webhook handling below
	default:
		log.WithFields(log.Fields{
			"id":   id,
			"type": tr.Type,
		}).Error("mismatched trigger type")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var data interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Warn("unable to bind json")
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("unable to fire trigger")
		}
	}()

	//	TODO: Allow responding to webhook from Flow output (sit and wait for it to complete/timeout)
	c.Status(http.StatusOK)
}

// handleProviderWebhookForTrigger handles GitLab and GitHub webhook triggers.
// Called from handleWebhook after the trigger has been fetched and type-checked.
func (s *Service) handleProviderWebhookForTrigger(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	// Determine provider from trigger type
	var provider string
	switch tr.Type {
	case launch.TriggerTypeGitLabWebhook:
		provider = "gitlab"
	case launch.TriggerTypeGitHubWebhook:
		provider = "github"
	default:
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Resolve the webhook secret from trigger data
	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	secretRef := triggerData["webhook_secret"]

	if secretRef != "" {
		if strings.Contains(secretRef, "${") {
			resolved, resolveErr := s.trigger.ResolveVariables(id, []string{secretRef})
			if resolveErr == nil && resolved[secretRef] != "" {
				secretRef = resolved[secretRef]
			}
		}

		switch provider {
		case "gitlab":
			if err := gitlabwh.VerifyToken(secretRef, c.Request); err != nil {
				log.WithFields(log.Fields{"id": id, "error": err}).Warn("GitLab webhook token verification failed")
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
		case "github":
			if err := githubwh.VerifySignature(secretRef, body, c.Request); err != nil {
				log.WithFields(log.Fields{"id": id, "error": err}).Warn("GitHub webhook signature verification failed")
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
		}
	}

	// Parse event based on provider
	var data map[string]interface{}
	var parseErr error
	switch provider {
	case "gitlab":
		data, parseErr = gitlabwh.ParseEvent(c.GetHeader("X-Gitlab-Event"), body)
	case "github":
		data, parseErr = githubwh.ParseEvent(c.GetHeader("X-GitHub-Event"), body)
	}
	if parseErr != nil {
		log.WithFields(log.Fields{"id": id, "provider": provider, "error": parseErr}).Error("webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Check event filter
	eventFilter := triggerData["event_filter"]
	eventType, _ := data["event_type"].(string)

	matches := false
	switch provider {
	case "gitlab":
		matches = gitlabwh.MatchesFilter(eventType, eventFilter)
	case "github":
		matches = githubwh.MatchesFilter(eventType, eventFilter)
	}
	if !matches {
		c.Status(http.StatusOK)
		return
	}

	// Carry __node_id from the trigger's stored config so the executor
	// can inject event data into the correct trigger node in multi-trigger flows.
	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err, "provider": provider}).Error("unable to fire webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}

func (s *Service) handleQr(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if err := uuid.Validate(id); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if tr == nil {
		log.WithFields(log.Fields{
			"id": id,
		}).Error("trigger ID not found")
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if tr.Type != launch.TriggerTypeQR {
		log.WithFields(log.Fields{
			"id":   id,
			"type": tr.Type,
		}).Error("mismatched trigger type")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data := map[string]interface{}{
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
		"ip":           c.ClientIP(),
		"user_agent":   c.Request.UserAgent(),
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("unable to fire trigger")
		}
	}()

	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Flomation</title><style>body{font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#161019;color:#fff;text-align:center;}</style></head><body><div><h2>QR Code Scanned</h2><p>Your request has been received.</p></div></body></html>`))
}

func (s *Service) submitForm(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if err := uuid.Validate(id); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if tr == nil {
		log.WithFields(log.Fields{
			"id": id,
		}).Error("trigger ID not found")
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if tr.Type != launch.TriggerTypeForm {
		log.WithFields(log.Fields{
			"id":   id,
			"type": tr.Type,
		}).Error("mismatched trigger type")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var body interface{}
	if err := c.BindJSON(&body); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to bind json")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	go func() {
		if err := s.trigger.Trigger(tr, body); err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("unable to execute trigger")
		}
	}()

	c.Status(http.StatusOK)
}

func (s *Service) handleForm(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if err := uuid.Validate(id); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("invalid trigger id")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if tr == nil {
		log.WithFields(log.Fields{
			"id": id,
		}).Error("trigger ID not found")
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if tr.Type != launch.TriggerTypeForm {
		log.WithFields(log.Fields{
			"id":   id,
			"type": tr.Type,
		}).Error("mismatched trigger type")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	c.HTML(http.StatusOK, "form.html", gin.H{
		"Form": base64.StdEncoding.EncodeToString(tr.Data),
	})
}

func (s *Service) jwtMiddleware(c *gin.Context) {
	header := c.GetHeader("Authorization")
	parts := strings.Split(header, " ")
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if s.config.Security.IdentityService == "" {
		log.Warn("identity service not configured — rejecting request")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	userID, err := sentinel.GetUser(s.config.Security.IdentityService, parts[1])
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to validate token with identity service")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	c.Set("account_id", *userID)
	c.Next()
}
