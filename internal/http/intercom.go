package http

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 — Intercom mandates HMAC-SHA1 for webhook signatures; not our choice
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"flomation.app/automate/launch"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Intercom webhook trigger — Flomation's first MANUALLY-registered trigger.
// Intercom webhook subscriptions can only be configured by the user in the
// Intercom Developer Hub UI (there is no public subscription API), so there is
// no registerIntercomWebhook/deregisterIntercomWebhook here — launch only
// receives. The user pastes {PublicURL}/webhook/{trigger_id} into their app
// under Configure → Webhooks and picks the topics to send.
//
// Intercom validates the endpoint URL with a HEAD request when the user saves
// it; the shared HEAD /webhook/:id route (handleWebhookHead, added for Trello)
// answers that with a 200. Deliveries must be acknowledged within 5 seconds,
// so the flow is fired asynchronously.

// intercomVerifySignature checks Intercom's "X-Hub-Signature" header against
// "sha1=" + hex(HMAC-SHA1(client_secret, raw body)), using a constant-time
// comparison. A missing/malformed header fails closed. Intercom mandates SHA-1
// here — it is not a security choice of ours (see the #nosec on the import).
func intercomVerifySignature(body []byte, header, secret string) bool {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "sha1=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, "sha1="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// intercomShouldFire decides whether a delivery fires the flow: "ping" topics
// never fire (Intercom's test button + periodic keep-alives), and a configured
// topic filter drops every other topic. Both cases are still acknowledged with
// a 200 — a non-2xx would make Intercom retry and eventually disable the URL.
func intercomShouldFire(topic, filter string) bool {
	if topic == "ping" {
		return false
	}
	if filter != "" && filter != topic {
		return false
	}
	return true
}

// handleIntercomWebhook handles an inbound Intercom webhook for a trigger.
// Called from handleWebhook after the trigger has been fetched and
// type-checked.
//
// When the trigger's optional client_secret is configured, the delivery's
// X-Hub-Signature is verified against the raw body before any parsing; without
// a secret, security rests on the unguessable trigger-id in the callback URL
// (the same model as the Monday/Trello-without-secret triggers).
func (s *Service) handleIntercomWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	creds := s.resolveTriggerCreds(tr.ID)
	if creds == nil {
		// A nil map means the trigger re-fetch or its config parse FAILED — not
		// that no client_secret is configured. Proceeding would silently skip
		// signature verification (and blank the topic filter), accepting forged
		// payloads on a trigger that has a secret set. Fail closed with a 503:
		// Intercom retries deliveries, so a transient hiccup self-heals.
		log.WithField("id", id).Warn("Intercom webhook: could not resolve trigger config")
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	if secret := strings.TrimSpace(creds["client_secret"]); secret != "" {
		if !intercomVerifySignature(body, c.GetHeader("X-Hub-Signature"), secret) {
			log.WithField("id", id).Warn("Intercom webhook signature verification failed")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}

	var payload map[string]interface{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			log.WithFields(log.Fields{"id": id, "error": err}).Error("Intercom webhook parse failed")
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
	}

	topic := intercomEventStr(payload["topic"])
	if filter := strings.TrimSpace(creds["topic_filter"]); !intercomShouldFire(topic, filter) {
		log.WithFields(log.Fields{"id": id, "topic": topic, "filter": filter}).Debug("Intercom webhook acknowledged without firing")
		c.Status(http.StatusOK)
		return
	}

	out := intercomEventOutputs(payload, body)

	var triggerData map[string]interface{}
	_ = json.Unmarshal(tr.Data, &triggerData)
	if nodeID := asString(triggerData["__node_id"]); nodeID != "" {
		out["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, out); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Intercom webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}

// intercomEventOutputs flattens a notification_event envelope
// ({type, topic, app_id, data: {item: {...}}, created_at}) into the trigger
// node's outputs. Besides the generic item_type/item_id pair it fills
// best-effort convenience ids (conversation_id, ticket_id, contact_id,
// company_id, admin_id) so flows can wire "the conversation" or "the contact"
// without digging into the raw JSON.
func intercomEventOutputs(payload map[string]interface{}, raw []byte) map[string]interface{} {
	out := map[string]interface{}{
		"body":            string(raw),
		"topic":           intercomEventStr(payload["topic"]),
		"app_id":          intercomEventStr(payload["app_id"]),
		"created_at":      intercomEventStr(payload["created_at"]),
		"item_type":       "",
		"item_id":         "",
		"conversation_id": "",
		"ticket_id":       "",
		"contact_id":      "",
		"company_id":      "",
		"admin_id":        "",
	}

	data, _ := payload["data"].(map[string]interface{})
	item, _ := data["item"].(map[string]interface{})
	if item == nil {
		return out
	}

	itemType := intercomEventStr(item["type"])
	itemID := intercomEventStr(item["id"])
	out["item_type"] = itemType
	out["item_id"] = itemID

	switch itemType {
	case "conversation":
		out["conversation_id"] = itemID
		out["contact_id"] = intercomConversationContactID(item)
	case "ticket":
		out["ticket_id"] = itemID
	case "contact", "user", "lead":
		out["contact_id"] = itemID
	case "company":
		out["company_id"] = itemID
	}

	// The assigned teammate, when the item carries one (conversations and
	// tickets expose admin_assignee_id; some payloads nest an assignee object).
	if v := intercomEventStr(item["admin_assignee_id"]); v != "" {
		out["admin_id"] = v
	} else if assignee, ok := item["assignee"].(map[string]interface{}); ok {
		if v := intercomEventStr(assignee["id"]); v != "" {
			out["admin_id"] = v
		}
	}

	return out
}

// intercomConversationContactID pulls the customer's contact id off a
// conversation item, trying the contacts list first ({type: "contact.list",
// contacts: [...]} — or a plain array in older payloads) and falling back to
// the source author. Best effort; "" when absent.
func intercomConversationContactID(item map[string]interface{}) string {
	var list []interface{}
	switch contacts := item["contacts"].(type) {
	case map[string]interface{}:
		list, _ = contacts["contacts"].([]interface{})
	case []interface{}:
		list = contacts
	}
	if len(list) > 0 {
		if first, ok := list[0].(map[string]interface{}); ok {
			if v := intercomEventStr(first["id"]); v != "" {
				return v
			}
		}
	}
	if source, ok := item["source"].(map[string]interface{}); ok {
		if author, ok := source["author"].(map[string]interface{}); ok {
			// Only trust the author when it is the customer side — on
			// admin-initiated conversations (conversation.admin.single.created)
			// the teammate is the source author, and an admin id must not land
			// in contact_id.
			switch intercomEventStr(author["type"]) {
			case "user", "lead", "contact":
				return intercomEventStr(author["id"])
			}
		}
	}
	return ""
}

// intercomEventStr stringifies an Intercom event field. Some ids and every
// created_at come through as JSON numbers (float64); integers are rendered
// without a decimal point. Strings pass through; anything else yields "".
func intercomEventStr(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}
