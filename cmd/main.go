package main

import (
	"flomation.app/automate/launch/internal/agent"
	"flomation.app/automate/launch/internal/commitment"
	"flomation.app/automate/launch/internal/config"
	emailtrigger "flomation.app/automate/launch/internal/email"
	"flomation.app/automate/launch/internal/google"
	gitpoll "flomation.app/automate/launch/internal/git/poll"
	"flomation.app/automate/launch/internal/http"
	"flomation.app/automate/launch/internal/persistence"
	s3trigger "flomation.app/automate/launch/internal/s3"
	"flomation.app/automate/launch/internal/schedule"
	"flomation.app/automate/launch/internal/telegram"
	"flomation.app/automate/launch/internal/trigger"
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

	_ = commitment.NewService(cfg, db)
	log.Info("commitment poller started")

	agentSvc := agent.NewService(cfg, db, t, telegramSvc)
	log.Info("agent service started")

	emailSvc = emailtrigger.NewService(cfg, db, t, agentSvc)
	_ = emailSvc
	log.Info("email trigger service started")

	var googleSvc *google.Service
	if cfg.Google != nil && cfg.Google.ClientID != "" {
		googleSvc = google.NewService(cfg)
		log.Info("google calendar oauth service started")
	}

	r, err := http.NewService(cfg, t, agentSvc, googleSvc, db)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to create http service")
		return
	}

	log.Fatal(r.Listen())
}
