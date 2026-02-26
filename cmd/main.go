package main

import (
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/google"
	"flomation.app/automate/launch/internal/http"
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
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to create trigger service")
		return
	}

	g, err := google.NewService(cfg)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to create google drive service")
		return
	}

	r, err := http.NewService(cfg, g, t)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to create http service")
		return
	}

	log.Fatal(r.Listen())
}
