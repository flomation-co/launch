package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	apollowh "flomation.app/automate/launch/internal/apollo"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleApolloWebhook handles an inbound Apollo.io webhook for a trigger. Apollo
// sends no signature, so authenticity rests on the unguessable /webhook/:id
// route plus an author-chosen shared secret presented as `?secret=` (or the
// X-Flomation-Webhook-Secret header) and compared in constant time. Mirrors
// handleStripeWebhook, swapping signature verification for the token check.
func (s *Service) handleApolloWebhook(c *gin.Context, tr *launch.Trigger) {
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
		log.WithFields(log.Fields{"id": id}).Warn("Apollo webhook has no secret configured")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if strings.Contains(secretRef, "${") {
		if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
			secretRef = resolved[secretRef]
		}
	}

	payload, err := apollowh.VerifyAndParse(body, c.Request, secretRef)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Apollo webhook secret verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data := apollowh.EventToData(payload, body)

	// Event-type filter (e.g. "contact.created,account.created"); empty matches all.
	if !apollowh.MatchesFilter(str(data["event_type"]), triggerData["event_filter"]) {
		c.Status(http.StatusOK)
		return
	}

	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Apollo webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}

func str(v interface{}) string {
	if sv, ok := v.(string); ok {
		return sv
	}
	return ""
}
