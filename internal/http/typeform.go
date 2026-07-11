package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	typeformwh "flomation.app/automate/launch/internal/typeform"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleTypeformWebhook handles an inbound Typeform webhook for a trigger.
// Typeform signs the RAW body with HMAC-SHA256 keyed by the endpoint secret and
// delivers it in the `Typeform-Signature: sha256=<base64>` header, so the body
// is read verbatim and verified before any parsing — mirroring
// handleStripeWebhook with Typeform's scheme, the `secret` config field, and an
// optional form_id filter.
func (s *Service) handleTypeformWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	secretRef := triggerData["secret"]
	if secretRef == "" {
		log.WithFields(log.Fields{"id": id}).Warn("Typeform webhook has no secret configured")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if strings.Contains(secretRef, "${") {
		if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
			secretRef = resolved[secretRef]
		}
	}

	ev, err := typeformwh.VerifyAndParse(body, c.Request, secretRef)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Typeform webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// Optional form_id filter; empty matches all forms.
	if !typeformwh.MatchesFilter(ev.FormResponse.FormID, triggerData["form_id"]) {
		c.Status(http.StatusOK)
		return
	}

	data := typeformwh.EventToData(ev)
	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Typeform webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
