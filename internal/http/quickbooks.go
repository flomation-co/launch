package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	quickbookswh "flomation.app/automate/launch/internal/quickbooks"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleQuickBooksWebhook handles an inbound QuickBooks Online webhook (Change
// Data Capture) for a trigger. Intuit signs the RAW body with HMAC-SHA256 keyed
// by the app verifier token (intuit-signature header), so the body is read
// verbatim and verified before parsing — mirroring handleStripeWebhook with
// QuickBooks' scheme, the verifier_token config field, and event-type filtering.
func (s *Service) handleQuickBooksWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	secretRef := triggerData["verifier_token"]
	if secretRef == "" {
		log.WithFields(log.Fields{"id": id}).Warn("QuickBooks webhook has no verifier token configured")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if strings.Contains(secretRef, "${") {
		if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
			secretRef = resolved[secretRef]
		}
	}

	if err := quickbookswh.VerifySignature(secretRef, body, c.Request); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("QuickBooks webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data, err := quickbookswh.ParseEvent(body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("QuickBooks webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	eventType, _ := data["event_type"].(string)
	if !quickbookswh.MatchesFilter(eventType, triggerData["event_filter"]) {
		c.Status(http.StatusOK)
		return
	}

	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire QuickBooks webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
