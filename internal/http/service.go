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
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"flomation.app/automate/launch/internal/assets"

	"flomation.app/automate/launch"

	"flomation.app/automate/launch/internal/agent"
	"flomation.app/automate/launch/internal/facebook"
	githubwh "flomation.app/automate/launch/internal/github"
	gitlabwh "flomation.app/automate/launch/internal/gitlab"
	"flomation.app/automate/launch/internal/google"
	mqttpkg "flomation.app/automate/launch/internal/mqtt"
	"flomation.app/automate/launch/internal/mtls"
	"flomation.app/automate/launch/internal/persistence"
	telegrampkg "flomation.app/automate/launch/internal/telegram"
	"flomation.app/automate/launch/internal/trigger"
	"flomation.app/automate/launch/internal/twilio"

	"flomation.app/automate/launch/internal/config"
	appmetrics "flomation.app/automate/launch/internal/metrics"
	"flomation.app/automate/launch/internal/version"
	"github.com/flomation-co/sentinel-client"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

// Form draft (autosave / fire-once submission) tuning.
const (
	// formDraftTTL is how long a server-side draft lives before it is purged.
	formDraftTTL = 24 * time.Hour
	// formDraftMaxBytes caps an autosave payload to keep a hostile client from
	// filling the drafts table with an oversized blob.
	formDraftMaxBytes = 256 * 1024
)

type Service struct {
	config             *config.Config
	engine             *gin.Engine
	internalEngine     *gin.Engine  // mTLS-only listener for internal routes
	apiClient          *http.Client // mTLS-capable client for internal API calls
	trigger            *trigger.Service
	agent              *agent.Service
	google             *google.Service
	telegram           *telegrampkg.Service
	db                 *persistence.Service
	facebookIndex      *facebook.PageIndex
	voiceCalls         *twilio.VoiceCallManager
	formData           *formDataResolver   // caches ${data.X} form autofill results
	mqtt               *mqttpkg.Service    // holds the broker subscriptions for MQTT triggers
	formComputeLimiter *computeRateLimiter // bounds /form/:id/compute per form
}

func NewService(config *config.Config, trigger *trigger.Service, agentSvc *agent.Service, googleSvc *google.Service, telegramSvc *telegrampkg.Service, db *persistence.Service, mqttSvc *mqttpkg.Service) (*Service, error) {
	gin.SetMode(gin.ReleaseMode)

	apiClient, err := mtls.ClientOrDefault(config.TLS, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("http: unable to create API client: %w", err)
	}

	s := Service{
		config:        config,
		engine:        gin.New(),
		apiClient:     apiClient,
		google:        googleSvc,
		telegram:      telegramSvc,
		db:            db,
		trigger:       trigger,
		agent:         agentSvc,
		facebookIndex: facebook.NewPageIndex(),
		voiceCalls:    twilio.NewVoiceCallManager(),
		formData:      newFormDataResolver(apiClient, config.InternalAPIURL()),
		mqtt:          mqttSvc,
		// ~5 compute requests/second per form with a small burst — enough for
		// live debounced typing, tight enough to stop a client spinning the
		// pricing flow.
		formComputeLimiter: newComputeRateLimiter(5, 10),
	}

	templ := template.Must(template.ParseFS(assets.Templates, "files/form.html"))
	s.engine.SetHTMLTemplate(templ)

	if err := s.configure(); err != nil {
		return nil, err
	}

	// Rebuild Facebook page index from existing triggers in the database
	s.rebuildFacebookIndex()

	return &s, nil
}

func corsMiddleware(c *gin.Context) {
	// Embed SDK routes manage their own per-origin CORS (see embedGate /
	// embedPreflight): they reflect a specific allowed Origin and permit writes.
	// The wildcard GET-only policy below would otherwise override them and block
	// cross-origin POST/PUT preflight.
	if strings.HasPrefix(c.Request.URL.Path, "/v1/embed/") {
		c.Next()
		return
	}

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
	if s.config.Metrics.Enabled {
		s.engine.Use(appmetrics.RequestMetricsMiddleware())
		s.engine.GET("metrics", appmetrics.IPRestrictionMiddleware(s.config.Metrics.AllowedIPs), gin.WrapH(promhttp.Handler()))
	}
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

		// Content-sniffing (http.DetectContentType) mislabels JavaScript
		// modules and WebAssembly as text/plain, which browsers refuse to
		// execute as ES modules / streaming-compile. Set the type by
		// extension for the asset kinds we serve (onnxruntime-web + models)
		// before falling back to sniffing.
		contentType := ""
		switch {
		case strings.HasSuffix(p, ".js"), strings.HasSuffix(p, ".mjs"):
			contentType = "text/javascript; charset=utf-8"
		case strings.HasSuffix(p, ".wasm"):
			contentType = "application/wasm"
		case strings.HasSuffix(p, ".onnx"):
			contentType = "application/octet-stream"
		case strings.HasSuffix(p, ".css"):
			contentType = "text/css; charset=utf-8"
		default:
			contentType = http.DetectContentType(b)
		}

		c.Data(http.StatusOK, contentType, b)
	})

	s.engine.GET("/webhook/:id", s.handleWebhook)
	s.engine.POST("/webhook/:id", s.handleWebhook)
	// Trello validates a webhook's callbackURL at registration by sending it a
	// HEAD request that must return 200, or the webhook is never created. Gin does
	// not auto-answer HEAD for a GET route, so register one explicitly. Any
	// provider that probes with HEAD is satisfied by this 200 — Intercom does the
	// same when the user saves the endpoint URL in its Developer Hub.
	s.engine.HEAD("/webhook/:id", s.handleWebhookHead)
	s.engine.GET("/qr/:id", s.handleQr)
	s.engine.GET("/form/:id", s.handleForm)
	s.engine.GET("/form/:id/data", s.handleFormData)
	// POST carries the answers gathered so far, so a page's data can depend on
	// earlier fields (resolved lazily on page entry).
	s.engine.POST("/form/:id/data", s.handleFormData)
	// Per-field flow compute for NON-payment display fields: runs the flow the
	// author bound to a field with the current answers as input and returns
	// its computed value. Payment amounts are computed server-side at checkout,
	// never here.
	s.engine.POST("/form/:id/compute", s.computeFormField)
	s.engine.GET("/form/:id/execution/:eid", s.handleFormExecution)
	s.engine.POST("/form/:id", s.submitForm)
	s.engine.PUT("/form/:id/submission/:sid", s.autosaveFormDraft)
	s.engine.POST("/form/:id/upload", s.uploadFormBlob)
	// Native form payments (Stripe hosted Checkout). payment-intent creates a
	// Checkout Session for a draft and returns its URL; complete is the
	// success_url Stripe redirects back to, verifies the session is PAID, then
	// finalises the draft (sanitise → fire-once → trigger).
	s.engine.POST("/form/:id/payment-intent", s.createFormPaymentIntent)
	s.engine.GET("/form/:id/complete", s.completeFormPayment)
	s.engine.GET("/image/:id", s.handleImageLoad)
	// Programmatic manual-trigger run (edge-facing, authenticated by the
	// trigger's optional run token — see handleManualRun).
	s.engine.POST("/trigger/:id/run", s.handleManualRun)

	// Embed SDK edge — publishable-key gated, per-origin CORS. Wraps the existing
	// form handlers so a third-party app can render/submit a form natively (no
	// iframe): the gate validates key + Origin + resource opt-in and reflects the
	// Origin, while the handlers still re-validate every write. The definition
	// endpoint returns a secret-stripped public projection.
	embedForm := s.engine.Group("/v1/embed/form/:id", s.embedFormGate())
	embedForm.GET("/definition", s.handleEmbedFormDefinition)
	embedForm.POST("/session", s.handleEmbedFormSession)
	embedForm.GET("/data", s.handleFormData)
	// POST /data runs the data-source flow WITH the answers so far, for a source
	// that depends on earlier fields (mirrors the public GET+POST /form/:id/data).
	// handleFormData branches on the method; the OPTIONS preflight + setEmbedCORS
	// already allow POST.
	embedForm.POST("/data", s.handleFormData)
	embedForm.GET("/execution/:eid", s.handleFormExecution)
	embedForm.POST("", s.submitForm)
	embedForm.POST("/compute", s.computeFormField)
	embedForm.PUT("/submission/:sid", s.autosaveFormDraft)
	embedForm.POST("/upload", s.uploadFormBlob)
	embedForm.POST("/payment-intent", s.createEmbedPaymentIntent)
	embedForm.GET("/field-states", s.getEmbedFieldStates)
	// Payment completion is PUBLIC — Stripe redirects the top-level browser here
	// with no key/origin, so it can't sit behind the embed gate. It is secured by
	// the Stripe session binding + the return_url stored at intent time.
	s.engine.GET("/v1/embed/form/:id/complete", s.completeEmbedPayment)
	// CORS preflight for the embed form routes (no key required — reflects the
	// Origin and advertises the allowed methods/headers).
	s.engine.OPTIONS("/v1/embed/form/:id/definition", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id/session", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id/data", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id/compute", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id/execution/:eid", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id/submission/:sid", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id/upload", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id/payment-intent", s.embedPreflight)
	s.engine.OPTIONS("/v1/embed/form/:id/field-states", s.embedPreflight)

	// Web Trigger invoke — call a flow over HTTP (any verb) and get its Web
	// Response back synchronously. Gated by the flow resource; each verb is
	// registered so a flow can branch on ${method}.
	embedFlow := s.engine.Group("/v1/embed/flow/:id", s.embedFlowGate())
	embedFlow.GET("/invoke", s.handleEmbedFlowInvoke)
	embedFlow.POST("/invoke", s.handleEmbedFlowInvoke)
	embedFlow.PUT("/invoke", s.handleEmbedFlowInvoke)
	embedFlow.PATCH("/invoke", s.handleEmbedFlowInvoke)
	embedFlow.DELETE("/invoke", s.handleEmbedFlowInvoke)
	s.engine.OPTIONS("/v1/embed/flow/:id/invoke", s.embedFlowPreflight)

	// Flomation Gateway — developer-defined HTTP APIs. ANY method + any sub-path
	// under a short api id; the handler resolves the API, matches the route, runs
	// the pluggable authenticator, and dispatches the endpoint's flow.
	s.engine.Any("/gateway/:apiId/*path", s.handleGateway)

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

	// Voice session WebSocket bridge (internal, called by API proxy → executor)
	internalRouter.GET("/internal/voice-session/:session_id", s.handleVoiceSessionInternal)
	internalRouter.POST("/internal/voice-session/:session_id/register", s.handleVoiceSessionRegister)

	// Agent inbound webhooks (edge-facing, no auth — validated by agent ID)
	// These stay on the public engine — external services hit them directly.
	// :id is a trigger_id for new registrations or an agent_id for
	// legacy. Handlers try trigger lookup first, fall back to agent.
	s.engine.POST("/webhook/agent/:agent_id", s.handleAgentWebhook)
	s.engine.POST("/webhook/telegram/:id", s.handleTelegramWebhook)
	s.engine.POST("/webhook/slack/:id", s.handleSlackWebhook)
	s.engine.POST("/webhook/teams/:id", s.handleTeamsWebhook)
	s.engine.POST("/webhook/twilio/sms/:id", s.handleTwilioSMSWebhook)
	s.engine.POST("/webhook/twilio/voice/:id", s.handleTwilioVoiceWebhook)
	s.engine.POST("/webhook/twilio/voice/:id/status", s.handleTwilioVoiceStatus)
	s.engine.GET("/ws/twilio/voice/:agent_id", s.handleTwilioVoiceWS)
	s.engine.GET("/ws/twilio/voice-outbound/:session_id", s.handleTwilioVoiceOutboundWS)

	// Facebook shared webhook (all page events routed by page ID)
	s.engine.GET("/webhook/facebook", s.handleFacebookVerification)
	s.engine.POST("/webhook/facebook", s.handleFacebookWebhook)

	// Slack interactivity — Block Kit button clicks, select menus, etc.
	// Configure in Slack App Settings → Interactivity & Shortcuts → Request URL.
	s.engine.POST("/slack/:agent_id/interact", s.handleSlackInteraction)

	// Human-in-the-Loop web click-link fallback (channel-agnostic). GET shows a
	// confirm page (so email link-prefetch can't auto-answer); POST commits.
	s.engine.GET("/respond/:token", s.handleHITLWebConfirm)
	s.engine.POST("/respond/:token", s.handleHITLWebRespond)

	// Google OAuth2 (public, browser-facing)
	s.engine.GET("/auth/google/callback", s.handleGoogleAuthCallback)
	s.engine.GET("/auth/google/identity", s.handleGoogleAuthInitiateIdentity)
	s.engine.GET("/auth/google/trigger/:trigger_id", s.handleGoogleAuthInitiateTrigger)
	s.engine.GET("/auth/google/:agent_user_id", s.handleGoogleAuthInitiate)

	s.engine.GET("/auth/microsoft/identity", s.handleMicrosoftAuthInitiateIdentity)
	s.engine.GET("/auth/microsoft/callback", s.handleMicrosoftAuthCallback)

	s.engine.GET("/auth/slack/identity", s.handleSlackAuthInitiateIdentity)
	s.engine.GET("/auth/slack/callback", s.handleSlackAuthCallback)

	s.engine.GET("/auth/facebook/identity", s.handleFacebookAuthInitiateIdentity)
	s.engine.GET("/auth/facebook/callback", s.handleFacebookAuthCallback)

	s.engine.GET("/auth/linkedin/identity", s.handleLinkedInAuthInitiateIdentity)
	s.engine.GET("/auth/linkedin/callback", s.handleLinkedInAuthCallback)

	return nil
}

// FacebookPageIndex returns the page index so callers (e.g. agent service)
// can register/unregister agent channels.
func (s *Service) FacebookPageIndex() *facebook.PageIndex {
	return s.facebookIndex
}

func (s *Service) Listen() error {
	if s.internalEngine != nil {
		go s.listenInternal()
	}
	go s.purgeExpiredFormDrafts()
	return s.engine.Run(fmt.Sprintf("%v:%v", s.config.HttpListenConfig.Address, s.config.HttpListenConfig.Port))
}

// purgeExpiredFormDrafts periodically deletes lapsed form drafts. Mirrors the
// simple ticker loop used by the schedule/S3 poll services.
func (s *Service) purgeExpiredFormDrafts() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		deleted, err := s.db.PurgeExpiredFormDrafts()
		if err != nil {
			log.WithError(err).Warn("unable to purge expired form drafts")
			continue
		}
		if deleted > 0 {
			log.WithField("deleted", deleted).Info("purged expired form drafts")
		}
	}
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
	case launch.TriggerTypeMailchimpWebhook:
		s.handleMailchimpWebhook(c, tr)
		return
	case launch.TriggerTypeShopifyWebhook:
		s.handleShopifyWebhook(c, tr)
		return
	case launch.TriggerTypeStripeWebhook:
		s.handleStripeWebhook(c, tr)
		return
	case launch.TriggerTypeApolloWebhook:
		s.handleApolloWebhook(c, tr)
		return
	case launch.TriggerTypeHeyGenWebhook:
		s.handleHeyGenWebhook(c, tr)
		return
	case launch.TriggerTypeQuickBooksWebhook:
		s.handleQuickBooksWebhook(c, tr)
		return
	case launch.TriggerTypeXeroWebhook:
		s.handleXeroWebhook(c, tr)
		return
	case launch.TriggerTypeCalendlyWebhook:
		s.handleCalendlyWebhook(c, tr)
		return

	case launch.TriggerTypeZendeskWebhook:
		s.handleZendeskWebhook(c, tr)
		return

	case launch.TriggerTypeCalcomWebhook:
		s.handleCalcomWebhook(c, tr)
		return

	case launch.TriggerTypeAcuityWebhook:
		s.handleAcuityWebhook(c, tr)
		return
	case launch.TriggerTypeWooCommerceWebhook:
		s.handleWooCommerceWebhook(c, tr)
		return
	case launch.TriggerTypeJiraWebhook:
		s.handleJiraWebhook(c, tr)
		return
	case launch.TriggerTypeTrelloWebhook:
		s.handleTrelloWebhook(c, tr)
		return
	case launch.TriggerTypeAsanaWebhook:
		s.handleAsanaWebhook(c, tr)
		return
	case launch.TriggerTypeMondayWebhook:
		s.handleMondayWebhook(c, tr)
		return
	case launch.TriggerTypeIntercomWebhook:
		s.handleIntercomWebhook(c, tr)
		return
	case launch.TriggerTypeSendGridWebhook:
		s.handleSendGridWebhook(c, tr)
		return
	case launch.TriggerTypeTypeformWebhook:
		s.handleTypeformWebhook(c, tr)
		return
	case launch.TriggerTypeJotformWebhook:
		s.handleJotformWebhook(c, tr)
		return
	case launch.TriggerTypeSurveyMonkeyWebhook:
		s.handleSurveyMonkeyWebhook(c, tr)
		return
	case launch.TriggerTypeAWXWebhook:
		s.handleAWXWebhook(c, tr)
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

// formSubmitMaxMemory bounds how much of a multipart form is buffered in memory
// while parsing (the rest spills to temp files, which we don't read this pass).
const formSubmitMaxMemory int64 = 10 << 20 // 10 MB

// isBrowserFormPost reports whether a form was submitted as a plain HTML form
// (application/x-www-form-urlencoded or multipart/form-data) rather than the
// JS/SDK JSON body. Such a submit expects an HTML/redirect response, not JSON.
func isBrowserFormPost(c *gin.Context) bool {
	ct := c.ContentType()
	return ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data"
}

// parseFormPostBody reads a plain HTML form submission (urlencoded or multipart)
// into the answer map. A field sent once becomes a string; a field sent multiple
// times (a checkbox group) becomes an array — the shape the sanitise pipeline
// already expects. Multipart FILE parts are ignored in this pass: structured /
// file fields (matrix, table, address, file upload, …) need the JS form or SDK.
func parseFormPostBody(c *gin.Context) map[string]interface{} {
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		_ = c.Request.ParseMultipartForm(formSubmitMaxMemory)
	} else {
		_ = c.Request.ParseForm()
	}
	out := make(map[string]interface{}, len(c.Request.PostForm))
	for k, vals := range c.Request.PostForm {
		if len(vals) == 1 {
			out[k] = vals[0]
			continue
		}
		arr := make([]interface{}, len(vals))
		for i, v := range vals {
			arr[i] = v
		}
		out[k] = arr
	}
	return out
}

// formSubmitRedirectDest returns where to send the browser after a plain HTML
// form submit: the author's configured redirect URL (redirect mode, http(s)
// only) if set, otherwise back to the hosted form with ?submitted=1 so it can
// show a success banner. Post/Redirect/Get — a refresh won't resubmit.
func formSubmitRedirectDest(id string, def formDefinition) string {
	if def.Submit != nil && def.Submit.OnSubmit == "redirect" {
		u := strings.TrimSpace(def.Submit.RedirectURL)
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			return u
		}
	}
	return fmt.Sprintf("/form/%s?submitted=1", id)
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

	def, _ := parseFormDefinition(tr.Data)

	cookie, _ := c.Cookie("flomation-token")
	token := extractSessionToken(c.GetHeader("Authorization"), cookie)
	userID := s.resolveSessionUser(token)
	if def.RequireLogin && userID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// A plain HTML form (urlencoded/multipart) posts field=value pairs and
	// expects a redirect, not JSON; the JS form and SDK post a JSON answer map.
	// Both feed the same sanitise + fire pipeline below (server-authoritative).
	browserForm := isBrowserFormPost(c)
	var body map[string]interface{}
	if browserForm {
		body = parseFormPostBody(c)
	} else if err := c.BindJSON(&body); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to bind json")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Fire-once claim. The client threads its draft submission id back in the
	// body; it must never reach the trigger data, so strip it first. When a
	// valid id is present we atomically claim the draft — the first submit
	// wins, a double-submit (or payment callback) sees an already-fired draft
	// and no-ops. A submit without an id (e.g. an older client) proceeds
	// unguarded, exactly as before.
	sid := extractSubmissionID(body)

	// Load the draft's per-field states (payment paid/pending, …) when a
	// submission id is present. Nil when there is no draft — which correctly
	// leaves any required stateful field UNSATISFIED in the gate below.
	var fieldStates map[string]json.RawMessage
	if sid != "" && uuid.Validate(sid) == nil {
		var serr error
		fieldStates, serr = s.db.GetFieldStates(sid)
		if serr != nil {
			log.WithError(serr).Error("unable to load field states for submit gate")
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
	}

	// Stateful-field submit gate — ALWAYS run (not only when a submission id is
	// present). A required stateful field (payment, and future out-of-band types)
	// must be satisfied — its state recorded server-side — before the flow can
	// fire. With no draft/state it is unsatisfied, so an ungated submit (e.g. an
	// embed POST without a session) of a required-payment form is rejected here
	// rather than firing unpaid. Runs before the fire-once claim so a rejected
	// submit does not consume the draft. fieldStates is reused below for
	// __field_states.
	if missing := unsatisfiedRequiredStatefulFields(def, body, fieldStates); len(missing) > 0 {
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error":  "required field not completed",
			"fields": missing,
		})
		return
	}

	if sid != "" && uuid.Validate(sid) == nil {
		claimed, _, err := s.db.FireFormDraft(sid, []string{"draft", "finalising"})
		if err != nil {
			log.WithError(err).Error("unable to claim form draft")
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		if !claimed {
			// Already fired — idempotent no-op, do NOT re-trigger.
			c.Status(http.StatusOK)
			return
		}
	}

	// Restore the baked-at-render values for read-only fields, ignoring
	// any client-supplied values. This is the security check that
	// matches the render-time guarantee — if the user saw "Name: Andy"
	// as read-only, the trigger receives "Andy" regardless of what the
	// HTTP body says.
	ctx := substitutionContext{QueryParams: queryParamsMap(c)}
	if userID != "" {
		if vars, err := s.loadUserVariables(userID); err == nil {
			ctx.UserVariables = vars
		}
	}
	// Re-resolve the data source on submit, WITH the submitted answers, so the
	// authoritative baking matches what the (answer-aware) client showed. Its
	// outputs are used two ways: as ${data.X} values so a read-only field bakes
	// the same value it displayed, and to bake dynamic option lists into the
	// definition so the whitelist below covers dynamically-sourced options
	// exactly as it does static ones. The resolver caches keyed on the answers,
	// so this reuses the page-enter execution in the common case.
	var dataOutputs map[string]interface{}
	if def.DataSource != nil && def.DataSource.FlowID != "" {
		if formUsesDataNamespace(def) || formHasDynamicOptions(def) {
			dataOutputs = s.formData.ResolveComputed(def.DataSource.FlowID, body, def.DataSource.TimeoutSeconds)
			ctx.DataVariables = flattenOutputs(dataOutputs)
		}
	}
	if formHasDynamicOptions(def) {
		def = bakeDynamicOptions(def, dataOutputs)
	}
	// A table populates its rows from its own value_source flow (per-field, like
	// every other computed field). Re-run each such flow at submit so the row
	// whitelist in sanitiseTableSubmissions is authoritative for computed rows.
	if formHasComputedTableRows(def) {
		def = bakeComputedTableRows(def, func(flowID string) map[string]interface{} {
			return s.formData.ResolveComputed(flowID, body, 0)
		})
	}
	// An option field populates its OPTIONS from its own value_source flow (the
	// field-level equivalent of options_source). Re-run each such flow at submit
	// so the option whitelist in sanitiseFormSubmission is authoritative.
	if formHasComputedOptions(def) {
		def = bakeComputedOptions(def, func(flowID string) map[string]interface{} {
			return s.formData.ResolveComputed(flowID, body, 0)
		})
	}
	resolved := resolveFormForRender(def, ctx)
	// Sanitisation pipeline (option whitelist → matrix whitelist → strip
	// display-only → restore read-only defaults → strip hidden). Read-only
	// stripping runs before the hidden strip so a read-only field can act as
	// a (baked) condition source; the hidden strip runs last so it sees every
	// other field's final value. Shared with the payment finalise path.
	final := sanitiseFormSubmission(body, resolved)
	if userID != "" {
		final["user_id"] = userID
	}
	// Carry the form trigger's flow node id so the executor injects the answers
	// into THIS trigger node — a flow may have several triggers (e.g. a Manual
	// trigger for testing alongside the Form trigger). The API sync stamps
	// __node_id onto the form trigger's data. Reserved key: routing only, never
	// injected as an answer.
	if nodeID := formTriggerNodeID(tr.Data); nodeID != "" {
		final["__node_id"] = nodeID
	}
	// Surface the per-field mid-state (what was paid/verified out-of-band) to
	// the flow so downstream steps can act on it. Prefixed like the fire-once
	// marker so it can never collide with a real answer key.
	if len(fieldStates) > 0 {
		final["__field_states"] = fieldStates
	}

	// Fire synchronously so we can hand the client the execution id — a form
	// whose tail is a read-only "result page" polls this execution for its
	// outputs (see handleFormExecution). The create call returns as soon as the
	// execution row exists; the flow itself runs asynchronously on a runner, so
	// this does not block on completion. On a fire failure we keep the historical
	// contract (submission accepted, error logged) and simply omit the id.
	executionID, terr := s.trigger.TriggerReturningExecution(tr, final, userID)
	if terr != nil {
		log.WithFields(log.Fields{
			"error": terr,
		}).Error("unable to execute trigger")
		// Historical contract: the submission is accepted even if the fire fails.
		if browserForm {
			c.Redirect(http.StatusSeeOther, formSubmitRedirectDest(id, def))
			return
		}
		c.Status(http.StatusOK)
		return
	}

	if browserForm {
		// Post/Redirect/Get for a plain HTML form — the browser follows a 303 to
		// the author's redirect URL, or back to the form with ?submitted=1.
		c.Redirect(http.StatusSeeOther, formSubmitRedirectDest(id, def))
		return
	}
	c.JSON(http.StatusOK, gin.H{"execution_id": executionID})
}

// handleFormExecution lets a form's read-only result page poll the single
// execution its submission created (see submitForm) for the flow outputs it
// displays. Serves both the native form (GET /form/:id/execution/:eid) and the
// embed SDK (GET /v1/embed/form/:id/execution/:eid, under the embed gate).
//
// Security: the execution MUST belong to this form's own flow
// (flo_id == trigger.FlowID). Without that check a caller holding a valid
// pk_/session could read ANY execution by guessing/reusing an id. A mismatch (or
// an unknown execution) is reported as 404 so we never confirm another flow's
// execution id. Returns {status, outputs?} — outputs present only once executed.
func (s *Service) handleFormExecution(c *gin.Context) {
	id := c.Param("id")
	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil || tr == nil || tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	eid := c.Param("eid")
	if uuid.Validate(eid) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	floID, status, outputs, ok := s.formData.FetchExecution(eid)
	if !ok || floID == "" || floID != tr.FlowID {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	resp := gin.H{"status": status}
	if outputs != nil {
		resp["outputs"] = outputs
		// Also expose the outputs mapped to computed FIELD names (via the same
		// computeOutputKey /compute uses). The embed projection strips
		// value_output, so the SDK can't map raw outputs itself — it fills its
		// result-page fields from `values` by name. The native form can use
		// either. Best-effort: a definition that won't parse just omits values.
		if def, perr := parseFormDefinition(tr.Data); perr == nil {
			values := map[string]interface{}{}
			for _, page := range def.Pages {
				for _, comp := range page.Components {
					if strings.TrimSpace(comp.ValueSource) == "" {
						continue
					}
					if v, has := outputs[computeOutputKey(comp)]; has {
						values[comp.Name] = v
					}
				}
			}
			resp["values"] = values
		}
	}
	c.JSON(http.StatusOK, resp)
}

// extractSubmissionID pops the client-supplied draft submission id out of a
// form submission body. It is removed unconditionally so it can never be
// forwarded into the trigger data as if it were a form answer.
func extractSubmissionID(body map[string]interface{}) string {
	sid, _ := body["__submission_id"].(string)
	delete(body, "__submission_id")
	return sid
}

// autosaveBodyStatus validates a raw autosave payload. It returns the HTTP
// status to send on failure (413 if it exceeds the cap, 400 if it is not a
// JSON object), or 0 when the body is acceptable. Pure so it can be unit
// tested without a database.
func autosaveBodyStatus(raw []byte) int {
	if len(raw) > formDraftMaxBytes {
		return http.StatusRequestEntityTooLarge
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return http.StatusBadRequest
	}
	return 0
}

// autosaveFormDraft persists an in-progress form's answers so the user can
// close the tab and resume later. It is deliberately dumb: it stores the raw
// JSON object verbatim (no sanitisation — that happens on submit) against a
// live draft. The payload is capped and never logged.
func (s *Service) autosaveFormDraft(c *gin.Context) {
	id := c.Param("id")
	if uuid.Validate(id) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil || tr == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	sid := c.Param("sid")
	if uuid.Validate(sid) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Read one byte past the cap so an over-limit body is detectable.
	raw, _ := io.ReadAll(io.LimitReader(c.Request.Body, formDraftMaxBytes+1))
	if status := autosaveBodyStatus(raw); status != 0 {
		c.AbortWithStatus(status)
		return
	}

	ok, err := s.db.SaveFormDraftPayload(sid, raw)
	if err != nil {
		log.WithError(err).Error("unable to save form draft payload")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Status(http.StatusNoContent)
}

// uploadFormBlob accepts a single file from an anonymous form
// submitter, validates it against the corresponding form field's
// declared MIME / size constraints, and proxies the upload to the
// API's trigger-scoped blob endpoint. Returns the resolved
// `flo:blob:...` token so the client can stash it into the form's
// submission payload alongside the other field values.
//
// Body: multipart/form-data
//   - field  (text)   the form_definition component name whose value
//     this file should become
//   - file   (file)   the raw bytes (eSignature PNG, camera photo,
//     file upload)
//
// Response: 201 with {blob_token, size, mime}
func (s *Service) uploadFormBlob(c *gin.Context) {
	id := c.Param("id")
	if id == "" || uuid.Validate(id) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil || tr == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	def, _ := parseFormDefinition(tr.Data)

	// 25 MB hard cap on incoming bytes to short-circuit DoS attempts
	// before we touch API storage. The API also enforces this, but
	// stopping earlier saves bandwidth. Leave headroom for the
	// multipart boundary + form fields.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, blobMaxUploadBytes+1024*1024)

	if err := c.Request.ParseMultipartForm(blobMaxUploadBytes); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "parse multipart: " + err.Error()})
		return
	}

	fieldName := strings.TrimSpace(c.PostForm("field"))
	if fieldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "field name required"})
		return
	}

	comp := findUploadComponent(def, fieldName)
	if comp == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no upload-capable field named " + fieldName})
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file part missing: " + err.Error()})
		return
	}

	// Per-field size cap wins if it's tighter than the global cap.
	// max_size_bytes: 0 (or missing) means "use the global cap".
	effectiveCap := int64(blobMaxUploadBytes)
	if comp.MaxSizeBytes > 0 && comp.MaxSizeBytes < effectiveCap {
		effectiveCap = comp.MaxSizeBytes
	}
	if fileHeader.Size > effectiveCap {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("file exceeds %d-byte cap", effectiveCap)})
		return
	}

	// Per-field MIME allowlist. accept_mime is a comma-separated list
	// following the HTML5 accept attribute convention: "image/*",
	// "image/png,image/jpeg", "application/pdf", etc.
	mime := fileHeader.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	if comp.AcceptMime != "" && !mimeMatchesAcceptList(mime, comp.AcceptMime) {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "mime " + mime + " not allowed by field"})
		return
	}

	// Forward the file to the API's trigger-scoped upload endpoint.
	// Scope is derived server-side from the flow, so we don't set the
	// org/owner header here.
	upstreamURL := fmt.Sprintf("%v/api/v1/internal/flo/%v/trigger/%v/upload",
		s.config.InternalAPIURL(), tr.FlowID, tr.ID)

	f, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open upload: " + err.Error()})
		return
	}
	defer func() { _ = f.Close() }()

	var proxyBody bytes.Buffer
	proxyWriter := multipart.NewWriter(&proxyBody)
	_ = proxyWriter.WriteField("mime", mime)
	_ = proxyWriter.WriteField("purpose", "inbound")
	part, err := proxyWriter.CreateFormFile("file", fileHeader.Filename)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "build proxy body: " + err.Error()})
		return
	}
	if _, err := io.Copy(part, f); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "copy body: " + err.Error()})
		return
	}
	if err := proxyWriter.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "close writer: " + err.Error()})
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, &proxyBody)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "build request: " + err.Error()})
		return
	}
	req.Header.Set("Content-Type", proxyWriter.FormDataContentType())

	resp, err := s.apiClient.Do(req)
	if err != nil {
		log.WithError(err).Error("form upload: forward to API failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream unreachable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Pass the API's response through verbatim — the shape already
	// matches what the client wants (blob_token, size, mime, handle).
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read upstream: " + err.Error()})
		return
	}
	c.Data(resp.StatusCode, "application/json", body)
}

// blobMaxUploadBytes mirrors the API's persistence.BlobMaxSizeBytes
// (25 MB). Kept as a local constant to avoid importing the whole api
// package from Launch just for one integer.
const blobMaxUploadBytes int64 = 25 * 1024 * 1024

// findUploadComponent walks the form definition for a component whose
// name matches and whose type is one of the upload-capable field types
// (eSignature, camera, file_upload, license_plate). Returns nil on no match
// so the handler can 400 without leaking existence of other fields.
func findUploadComponent(def formDefinition, name string) *formComponent {
	for _, page := range def.Pages {
		for i := range page.Components {
			c := &page.Components[i]
			if c.Name != name {
				continue
			}
			switch c.Type {
			case "esignature", "camera", "file_upload", "license_plate":
				return c
			}
			return nil
		}
	}
	return nil
}

// mimeMatchesAcceptList checks whether mime satisfies at least one
// pattern in the comma-separated accept list. Supports:
//   - Exact match: "image/png" matches "image/png"
//   - Wildcards:   "image/*" matches any image/* MIME
//   - Empty list:  treated as allow-all by the caller before calling
func mimeMatchesAcceptList(mime, acceptList string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	for _, pattern := range strings.Split(acceptList, ",") {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "/*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(mime, prefix) {
				return true
			}
		} else if pattern == mime {
			return true
		}
	}
	return false
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

	def, parseErr := parseFormDefinition(tr.Data)
	if parseErr != nil {
		log.WithFields(log.Fields{"error": parseErr}).Warn("could not parse form definition; serving raw")
	}

	// Login gate. If the form requires a session and none resolves, show
	// the "Please log in" landing card with a return-to URL that takes
	// the user straight back to this form after they authenticate.
	cookie, _ := c.Cookie("flomation-token")
	token := extractSessionToken(c.GetHeader("Authorization"), cookie)
	userID := s.resolveSessionUser(token)
	if def.RequireLogin && userID == "" {
		returnTo := s.config.Security.EditorURL
		c.HTML(http.StatusOK, "form.html", gin.H{
			"LoginRequired":   true,
			"Title":           def.Title,
			"MetaTitle":       metaText(def.Title),
			"MetaDescription": metaText(def.Description),
			"ReturnTo":        returnTo,
			"FormURL":         c.Request.URL.RequestURI(),
		})
		return
	}

	// Render-time substitution. Only logged-in users get ${user.X};
	// ${query.X} comes from the URL regardless of auth state.
	ctx := substitutionContext{QueryParams: queryParamsMap(c)}
	if userID != "" {
		if vars, err := s.loadUserVariables(userID); err == nil {
			ctx.UserVariables = vars
		} else {
			log.WithFields(log.Fields{"error": err}).Warn("failed to load user variables for form; rendering without ${user.X}")
		}
	}
	// ${data.X} is resolved LAZILY, per page. We deliberately do NOT run the
	// data-source flow at load — leaving ctx.DataVariables nil makes
	// applySubstitutions leave ${data.X} tokens intact for the browser to
	// resolve when the referencing page is entered (POST /form/:id/data with the
	// answers so far). This avoids firing the flow before the user has provided
	// the inputs an early page collects. PageNeedsData tells the client which
	// pages must fetch data on entry.
	resolved := resolveFormForRender(def, ctx)
	pageNeedsData := []bool{}
	if def.DataSource != nil && def.DataSource.FlowID != "" {
		pageNeedsData = pagesNeedingData(def)
	}
	pageNeedsDataJSON, _ := json.Marshal(pageNeedsData)

	// Draft submission. Resume an existing live draft when the client presents
	// a valid submission_id that belongs to this trigger; otherwise mint a
	// fresh draft. Draft persistence failure is non-fatal — the form still
	// renders and submits, just without autosave/resume/fire-once.
	submissionID := uuid.NewString()
	// The resume payload is base64-encoded (like the Form field below) and
	// atob+JSON.parse'd on the client. It is NEVER injected raw into the page:
	// the draft holds client-authored answers, so a template.JS() interpolation
	// would be a stored-XSS vector (a "</script>" in an answer would break out).
	// base64 has no HTML/JS metacharacters, so it is safe in a quoted string.
	resumePayload := ""
	// fieldStatesPayload carries the draft's per-field mid-state map (payment
	// paid/pending, …) to the client so each stateful field renders its current
	// state on load/resume — e.g. a paid field shows "Paid ✓" after the Stripe
	// round-trip. base64 for the same XSS-safety reason as the resume payload
	// (the map holds no HTML/JS metacharacters once encoded).
	fieldStatesPayload := ""
	resumed := false
	if q := c.Query("submission_id"); q != "" && uuid.Validate(q) == nil {
		if draft, derr := s.db.GetFormDraft(q); derr == nil && draft != nil && draft.TriggerID == id {
			submissionID = q
			resumed = true
			if len(draft.Payload) > 0 {
				resumePayload = base64.StdEncoding.EncodeToString(draft.Payload)
			}
			if len(draft.FieldStates) > 0 {
				fieldStatesPayload = base64.StdEncoding.EncodeToString(draft.FieldStates)
			}
		}
	}
	if !resumed {
		if cerr := s.db.CreateFormDraft(submissionID, id, tr.FlowID, formDraftTTL); cerr != nil {
			log.WithError(cerr).Warn("unable to create form draft")
		}
	}

	// Multi-lingual: resolve the viewer's language (?lang → Accept-Language →
	// default) so the server-rendered <title>/OG meta — which crawlers see
	// without running our JS — is in the right language. The client seeds its
	// active locale from this too (see SERVER_LANG in form.html).
	langs, defaultLang := formLanguages(resolved)
	lang := resolveLanguageFromHeader(c.Query("lang"), c.GetHeader("Accept-Language"), langs, defaultLang)
	metaTitle := metaText(tI18n(resolved.Title, resolved.TitleI18n, lang, defaultLang))
	metaDescription := metaText(tI18n(resolved.Description, resolved.DescriptionI18n, lang, defaultLang))

	resolvedBytes, _ := json.Marshal(resolved)
	c.HTML(http.StatusOK, "form.html", gin.H{
		"Form":            base64.StdEncoding.EncodeToString(resolvedBytes),
		"FormID":          id,
		"SubmissionID":    submissionID,
		"ResumePayload":   resumePayload,
		"FieldStates":     fieldStatesPayload,
		"MetaTitle":       metaTitle,
		"MetaDescription": metaDescription,
		"Lang":            lang,
		"PageNeedsData":   base64.StdEncoding.EncodeToString(pageNeedsDataJSON),
	})
}

// handleFormData runs the form's data-source flow (cached + de-duplicated)
// and returns its outputs as JSON. The browser calls this after first paint
// to populate dynamic dropdown options, so a slow flow never blocks the form
// GET. Honours the same login gate as the form itself; returns an empty
// object when the form has no data source or the flow fails.
func (s *Service) handleFormData(c *gin.Context) {
	id := c.Param("id")
	if id == "" || uuid.Validate(id) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if tr == nil || tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	def, _ := parseFormDefinition(tr.Data)

	// Same login gate as the form view: a login-required form must not leak
	// its data-source outputs to an unauthenticated caller.
	cookie, _ := c.Cookie("flomation-token")
	token := extractSessionToken(c.GetHeader("Authorization"), cookie)
	userID := s.resolveSessionUser(token)
	if def.RequireLogin && userID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if def.DataSource == nil || def.DataSource.FlowID == "" {
		c.JSON(http.StatusOK, gin.H{})
		return
	}

	// A POST carries the answers gathered so far; run the data flow WITH them so
	// a page's ${data.X} / dynamic options can depend on earlier fields. The
	// resolver caches keyed on the inputs, so repeated identical page-enters
	// collapse to one execution. A GET (no body) keeps the original inputless
	// behaviour for callers that don't need answer-aware data.
	var outputs map[string]interface{}
	if c.Request != nil && c.Request.Method == http.MethodPost {
		var body struct {
			Answers map[string]interface{} `json:"answers"`
		}
		_ = c.ShouldBindJSON(&body)
		if body.Answers == nil {
			body.Answers = map[string]interface{}{}
		}
		outputs = s.formData.ResolveComputed(def.DataSource.FlowID, body.Answers, def.DataSource.TimeoutSeconds)
	} else {
		outputs = s.formData.ResolveRaw(def.DataSource.FlowID, def.DataSource.TimeoutSeconds)
	}
	if outputs == nil {
		outputs = map[string]interface{}{}
	}
	c.JSON(http.StatusOK, outputs)
}

// queryParamsMap flattens c.Request.URL.Query() to a string→string map
// (the first value of each key wins). Used to source ${query.X}
// substitutions.
func queryParamsMap(c *gin.Context) map[string]string {
	if c.Request == nil {
		return nil
	}
	q := c.Request.URL.Query()
	out := make(map[string]string, len(q))
	for k, v := range q {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
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
