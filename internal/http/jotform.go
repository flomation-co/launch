package http

import (
	"encoding/json"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	jotformwh "flomation.app/automate/launch/internal/jotform"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleJotformWebhook handles an inbound JotForm webhook for a trigger.
// JotForm webhooks are NOT HMAC-signed — they POST multipart/form-data with
// `formID`, `submissionID`, `rawRequest` and `type`. Security rests on the
// unguessable `/webhook/:id` UUID plus an OPTIONAL shared-secret: if a `secret`
// is configured on the trigger, a matching `?token=` query param is required
// (constant-time compare) else 401; with no secret the opaque id is the guard.
func (s *Service) handleJotformWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)

	// Optional shared-secret gate. Only enforced when a secret is configured.
	if secretRef := triggerData["secret"]; secretRef != "" {
		if strings.Contains(secretRef, "${") {
			if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
				secretRef = resolved[secretRef]
			}
		}
		if !jotformwh.TokenOK(secretRef, c.Query("token")) {
			log.WithFields(log.Fields{"id": id}).Warn("JotForm webhook token verification failed")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}

	ev, err := jotformwh.ParseMultipart(c.Request)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("unable to parse JotForm webhook body")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Optional form_id filter; empty matches all forms.
	if !jotformwh.MatchesFilter(ev.FormID, triggerData["form_id"]) {
		c.Status(http.StatusOK)
		return
	}

	data := jotformwh.EventToData(ev)
	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire JotForm webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
