package http

import (
	"net/http"
	"time"

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

	if t == nil {
		r, err := s.trigger.CreateTrigger(tr)
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("unable to create new trigger")
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}

		c.JSON(http.StatusCreated, r)
		return
	}

	err = s.trigger.UpdateTrigger(tr)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to update existing trigger")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	c.JSON(http.StatusOK, tr)
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
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	now := time.Now()
	t.DisabledAt = &now

	if err := s.trigger.UpdateTrigger(*t); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("unable to disable trigger")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	c.Status(http.StatusOK)
}
