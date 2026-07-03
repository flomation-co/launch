package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	shopifywh "flomation.app/automate/launch/internal/shopify"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// handleShopifyWebhook handles an inbound Shopify webhook for a trigger.
// Shopify signs the RAW body with the app's API secret key (base64
// X-Shopify-Hmac-Sha256), so the body is read verbatim and verified before any
// parsing — mirroring handleProviderWebhookForTrigger but with Shopify's
// scheme, the app_secret config field, and topic-based filtering.
func (s *Service) handleShopifyWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Resolve the app secret from trigger data (a ${secrets.X} reference is
	// resolved to its plaintext value for the HMAC comparison).
	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	secretRef := triggerData["app_secret"]

	if secretRef == "" {
		log.WithFields(log.Fields{"id": id}).Warn("Shopify webhook has no app secret configured")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if strings.Contains(secretRef, "${") {
		resolved, resolveErr := s.trigger.ResolveVariables(id, []string{secretRef})
		if resolveErr == nil && resolved[secretRef] != "" {
			secretRef = resolved[secretRef]
		}
	}

	if err := shopifywh.VerifySignature(secretRef, body, c.Request); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Shopify webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data, err := shopifywh.ParseEvent(c.Request, body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("Shopify webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Topic filter (e.g. "orders/create,products/update"); empty matches all.
	topic, _ := data["topic"].(string)
	if !shopifywh.MatchesFilter(topic, triggerData["event_filter"]) {
		c.Status(http.StatusOK)
		return
	}

	// Carry __node_id so the executor injects event data into the correct
	// trigger node in multi-trigger flows.
	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Shopify webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
