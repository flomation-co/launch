package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	heygenwh "flomation.app/automate/launch/internal/heygen"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleHeyGenWebhook handles an inbound HeyGen completion webhook for a
// trigger. HeyGen posts to the per-video callback_url the author configured, so
// authenticity rests on the unguessable /webhook/:id route plus an author-chosen
// shared secret presented as `?secret=` (or the X-Flomation-Webhook-Secret
// header) and compared in constant time. Mirrors handleApolloWebhook.
func (s *Service) handleHeyGenWebhook(c *gin.Context, tr *launch.Trigger) {
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
		log.WithFields(log.Fields{"id": id}).Warn("HeyGen webhook has no secret configured")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if strings.Contains(secretRef, "${") {
		if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
			secretRef = resolved[secretRef]
		}
	}

	payload, err := heygenwh.VerifyAndParse(body, c.Request, secretRef)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("HeyGen webhook secret verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data := heygenwh.EventToData(payload, body)

	// Event-type filter (e.g. "avatar_video.success"); empty matches all.
	if !heygenwh.MatchesFilter(str(data["event_type"]), triggerData["event_filter"]) {
		c.Status(http.StatusOK)
		return
	}

	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire HeyGen webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
