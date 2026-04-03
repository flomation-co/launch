package main

import (
	"flomation.app/automate/launch/internal/agent"
	"flomation.app/automate/launch/internal/config"
	gitpoll "flomation.app/automate/launch/internal/git/poll"
	"flomation.app/automate/launch/internal/http"
	s3trigger "flomation.app/automate/launch/internal/s3"
	"flomation.app/automate/launch/internal/schedule"
	"flomation.app/automate/launch/internal/persistence"
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

	agentSvc := agent.NewService(cfg, db, t)
	log.Info("agent service started")

	r, err := http.NewService(cfg, t, agentSvc)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to create http service")
		return
	}

	log.Fatal(r.Listen())
}
