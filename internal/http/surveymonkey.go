package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	surveymonkeywh "flomation.app/automate/launch/internal/surveymonkey"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleSurveyMonkeyWebhook handles an inbound SurveyMonkey webhook for a
// trigger. SurveyMonkey webhooks are NOT HMAC-signed — they POST a JSON body
// describing the event. Security rests on the unguessable `/webhook/:id` UUID
// plus an OPTIONAL shared-secret: if a `secret` is configured on the trigger, a
// matching `?token=` query param is required (constant-time compare) else 401;
// with no secret the opaque id is the guard.
func (s *Service) handleSurveyMonkeyWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)

	// Optional shared-secret gate. Only enforced when a secret is configured.
	if secretRef := triggerData["secret"]; secretRef != "" {
		if strings.Contains(secretRef, "${") {
			if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
				secretRef = resolved[secretRef]
			}
		}
		if !surveymonkeywh.TokenOK(secretRef, c.Query("token")) {
			log.WithFields(log.Fields{"id": id}).Warn("SurveyMonkey webhook token verification failed")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}

	ev, err := surveymonkeywh.Parse(body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("unable to parse SurveyMonkey webhook body")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Optional survey_id filter; empty matches all surveys.
	if !surveymonkeywh.MatchesFilter(ev.Resources.SurveyID, triggerData["survey_id"]) {
		c.Status(http.StatusOK)
		return
	}

	data := surveymonkeywh.EventToData(ev, body)
	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire SurveyMonkey webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
