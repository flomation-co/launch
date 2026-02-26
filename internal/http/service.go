package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"

	"flomation.app/automate/launch/internal/assets"

	"flomation.app/automate/launch"

	"flomation.app/automate/launch/internal/trigger"

	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/google"
	"flomation.app/automate/launch/internal/version"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

type Service struct {
	config  *config.Config
	engine  *gin.Engine
	google  *google.Service
	trigger *trigger.Service
}

func NewService(config *config.Config, google *google.Service, trigger *trigger.Service) (*Service, error) {
	s := Service{
		config:  config,
		engine:  gin.New(),
		google:  google,
		trigger: trigger,
	}

	templ := template.Must(template.ParseFS(assets.Templates, "files/form.html"))
	s.engine.SetHTMLTemplate(templ)

	if err := s.configure(); err != nil {
		return nil, err
	}

	return &s, nil
}

func (s *Service) configure() error {
	s.engine.GET("version", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"version": version.Version,
			"date":    version.BuiltDate,
			"hash":    version.GetHash(),
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

	s.engine.POST("/trigger/:id", s.createTrigger)

	// TODO: Temp
	s.engine.GET("/google/credential", func(c *gin.Context) {
		code := c.Query("code")
		if code == "" {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}

		state := c.Query("state")

		s.google.ReceiveAuthCode(google.TokenResponse{
			Code:  code,
			State: state,
		})

		c.Status(http.StatusOK)
	})

	return nil
}

func (s *Service) Listen() error {
	return s.engine.Run(fmt.Sprintf("%v:%v", s.config.HttpListenConfig.Address, s.config.HttpListenConfig.Port))
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
	cookies := c.Request.Cookies()
	// TODO: Extend this with Referrer, etc

	go func() {
		data := map[string]interface{}{
			"ip":         ip,
			"user_agent": userAgent,
			"cookies":    cookies,
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

	if tr.Type != launch.TriggerTypeWebhook {
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

	go func() {
		if err := s.trigger.Trigger(tr, nil); err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("unable to fire trigger")
		}
	}()

	c.Status(http.StatusOK)
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

	j, err := json.Marshal(tr.Data)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to marshal form data")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	c.HTML(http.StatusOK, "form.html", gin.H{
		"Form": strings.TrimSuffix(string(j)[1:], "\""),
	})
}
