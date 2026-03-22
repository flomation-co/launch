package http

import (
	"net/http"

	"flomation.app/automate/launch"

	log "github.com/sirupsen/logrus"

	"github.com/gin-gonic/gin"
)

func (s *Service) createTrigger(c *gin.Context) {
	id := c.Param("id")

	t, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to get trigger by ID")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var tr launch.Trigger
	if err := c.BindJSON(&tr); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to bind json")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Use the API's trigger ID so upsert works correctly
	tr.ID = id

	r, err := s.trigger.CreateTrigger(tr)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to upsert trigger")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if t == nil {
		c.JSON(http.StatusCreated, r)
	} else {
		c.JSON(http.StatusOK, r)
	}
}

func (s *Service) deleteTrigger(c *gin.Context) {
	id := c.Param("id")

	t, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to get trigger by ID")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	if t == nil {
		// Already deleted or never existed — treat as success
		c.Status(http.StatusOK)
		return
	}

	if err := s.trigger.RemoveTrigger(*t); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to delete trigger")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	c.Status(http.StatusOK)
}
