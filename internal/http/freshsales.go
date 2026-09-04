package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	freshsaleswh "flomation.app/automate/launch/internal/freshsales"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleFreshsalesWebhook handles an inbound Freshsales workflow webhook.
//
// Freshsales sends no signature, so authenticity rests on the unguessable
// /webhook/:id route plus an author-chosen shared secret, compared in constant
// time. Mirrors handleApolloWebhook; the difference is that Freshsales offers
// Token authentication and custom headers in its webhook UI, so the secret has
// three accepted homes rather than two.
func (s *Service) handleFreshsalesWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	secretRef := triggerData["webhook_secret"]
	if secretRef == "" {
		log.WithFields(log.Fields{"id": id}).Warn("Freshsales webhook has no secret configured")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if strings.Contains(secretRef, "${") {
		if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
			secretRef = resolved[secretRef]
		}
	}

	payload, err := freshsaleswh.VerifyAndParse(body, c.Request, secretRef)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Freshsales webhook secret verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data := freshsaleswh.EventToData(payload, body)

	if !freshsaleswh.MatchesFilter(str(data["event_type"]), str(data["entity_type"]), triggerData["event_filter"]) {
		c.Status(http.StatusOK)
		return
	}

	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Freshsales webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
