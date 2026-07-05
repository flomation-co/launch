package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	stripewh "flomation.app/automate/launch/internal/stripe"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleStripeWebhook handles an inbound Stripe webhook for a trigger. Stripe
// signs the RAW body (Stripe-Signature: t=…,v1=… over "{timestamp}.{body}"),
// so the body is read verbatim and verified via the official SDK before any
// parsing — mirroring handleShopifyWebhook with Stripe's scheme, the
// signing_secret config field, and event-type filtering.
func (s *Service) handleStripeWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	secretRef := triggerData["signing_secret"]
	if secretRef == "" {
		log.WithFields(log.Fields{"id": id}).Warn("Stripe webhook has no signing secret configured")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if strings.Contains(secretRef, "${") {
		if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
			secretRef = resolved[secretRef]
		}
	}

	ev, err := stripewh.VerifyAndParse(body, c.Request, secretRef)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Stripe webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// Event-type filter (e.g. "payment_intent.succeeded,invoice.paid"); empty matches all.
	if !stripewh.MatchesFilter(string(ev.Type), triggerData["event_filter"]) {
		c.Status(http.StatusOK)
		return
	}

	data := stripewh.EventToData(ev)
	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Stripe webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
