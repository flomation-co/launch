package main

import (
	"time"

	"flomation.app/automate/launch/internal/agent"
	"flomation.app/automate/launch/internal/config"
	emailtrigger "flomation.app/automate/launch/internal/email"
	"flomation.app/automate/launch/internal/embedding"
	gitpoll "flomation.app/automate/launch/internal/git/poll"
	"flomation.app/automate/launch/internal/google"
	drivepoll "flomation.app/automate/launch/internal/google/drivepoll"
	"flomation.app/automate/launch/internal/http"
	linkedinpoll "flomation.app/automate/launch/internal/linkedin"
	"flomation.app/automate/launch/internal/metrics"
	"flomation.app/automate/launch/internal/persistence"
	s3trigger "flomation.app/automate/launch/internal/s3"
	"flomation.app/automate/launch/internal/schedule"
	"flomation.app/automate/launch/internal/telegram"
	"flomation.app/automate/launch/internal/trigger"
	"flomation.app/automate/launch/internal/twilio"
	"flomation.app/automate/launch/internal/version"
	log "github.com/sirupsen/logrus"
)

func main() {
	log.WithFields(log.Fields{
		"version": version.Version,
		"hash":    version.GetHash(),
		"date":    version.BuiltDate,
	}).Info("Starting Flomation Launch Service")

	cfg, err := config.LoadConfig("config.json")
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to load config")
		return
	}

	log.Info("running database migrations")
	if err := persistence.CheckAndUpdate(cfg); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to run migrations")
		return
	}

	db, err := persistence.NewService(cfg)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to start persistence service")
		return
	}

	if cfg.Metrics.Enabled {
		metrics.StartCollector(db.DB(), 30*time.Second)
	}

	t := trigger.NewService(cfg, db)

	_ = gitpoll.NewService(cfg, db, t)
	log.Info("git poll service started")

	_ = schedule.NewService(cfg, t, db)
	log.Info("schedule service started")

	_ = s3trigger.NewService(cfg, db, t)
	log.Info("s3 trigger service started")

	// Email trigger service is started after agent service (needs agent ref)
	var emailSvc *emailtrigger.Service

	telegramSvc := telegram.NewService(cfg.PublicURL)
	log.Info("telegram service started")

	twilioSvc := twilio.NewService(cfg.PublicURL)
	log.Info("twilio service started")

	// Pollers (commitment, pending action, retention) have been migrated
	// to the API service (Phase 2). They now run API-side with direct DB
	// access instead of HTTP round-trips from Launch.

	var embedProvider embedding.Provider
	if cfg.Embedding != nil && cfg.Embedding.Enabled {
		region := cfg.Embedding.Region
		if region == "" {
			region = "us-east-1"
		}
		var err error
		embedProvider, err = embedding.NewBedrockProvider(region, cfg.Embedding.ModelID, cfg.Embedding.Dimensions, cfg.Embedding.AccessKeyID, cfg.Embedding.SecretKey)
		if err != nil {
			log.WithError(err).Warn("failed to initialise embedding provider — semantic retrieval disabled")
		} else {
			log.Info("embedding provider started (Bedrock Titan)")
		}
	}

	agentSvc := agent.NewService(cfg, db, t, telegramSvc, twilioSvc, embedProvider)
	log.Info("agent service started")

	emailSvc = emailtrigger.NewService(cfg, db, t, agentSvc)
	_ = emailSvc
	log.Info("email trigger service started")

	_ = linkedinpoll.NewService(cfg, db, t)
	log.Info("LinkedIn poll service started")

	var googleSvc *google.Service
	if cfg.Google != nil && cfg.Google.ClientID != "" {
		googleSvc = google.NewService(cfg)
		log.Info("google calendar oauth service started")

		_ = drivepoll.NewService(cfg, db, t, googleSvc)
		log.Info("Google Drive poll service started")
	}

	r, err := http.NewService(cfg, t, agentSvc, googleSvc, db)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to create http service")
		return
	}

	// Wire the Facebook page index into the agent service so agent channels
	// can register/unregister with the webhook demuxer.
	agentSvc.SetFacebookManager(r.FacebookPageIndex())

	log.Fatal(r.Listen())
}
