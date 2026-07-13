package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	xerowh "flomation.app/automate/launch/internal/xero"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleXeroWebhook handles an inbound Xero webhook for a trigger. Xero signs
// the RAW body with HMAC-SHA256 keyed by the webhook signing key
// (x-xero-signature header), so the body is read verbatim and verified before
// parsing — mirroring handleStripeWebhook with Xero's scheme, the signing_key
// config field, and event-type filtering.
//
// Xero's "intent to receive" check is honoured implicitly: a valid signature
// returns 200 and an invalid one returns 401 (which is exactly what Xero probes
// for). The probe carries an empty events array, so it verifies and acks with
// 200 without firing a flow.
func (s *Service) handleXeroWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	secretRef := triggerData["signing_key"]
	if secretRef == "" {
		log.WithFields(log.Fields{"id": id}).Warn("Xero webhook has no signing key configured")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if strings.Contains(secretRef, "${") {
		if resolved, rerr := s.trigger.ResolveVariables(id, []string{secretRef}); rerr == nil && resolved[secretRef] != "" {
			secretRef = resolved[secretRef]
		}
	}

	if err := xerowh.VerifySignature(secretRef, body, c.Request); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Xero webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// Intent-to-receive probe (or any empty batch): signature valid, ack 200,
	// but there's nothing to fire.
	if !xerowh.HasEvents(body) {
		c.Status(http.StatusOK)
		return
	}

	data, err := xerowh.ParseEvent(body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("Xero webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	eventType, _ := data["event_type"].(string)
	if !xerowh.MatchesFilter(eventType, triggerData["event_filter"]) {
		c.Status(http.StatusOK)
		return
	}

	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Xero webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
