// Package xero verifies and parses inbound Xero webhook events for the Xero
// Webhook Trigger.
//
// Xero signs the raw request body with HMAC-SHA256 keyed by the webhook signing
// key and sends it base64-encoded in the x-xero-signature header. Xero also
// runs an "intent to receive" check when a webhook is first configured: it
// posts payloads and requires HTTP 200 for a valid signature and 401 for an
// invalid one. That contract is satisfied by the handler responding 200 on a
// successful VerifySignature and 401 otherwise — no special-case code needed.
//
// Events are pointers (tenantId + resourceId + category/type), not full
// objects, so a flow typically follows up with a Xero action to fetch the
// changed record.
package xero

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// VerifySignature validates the x-xero-signature header over the raw body using
// the webhook signing key (HMAC-SHA256, base64). Uses a constant-time compare.
func VerifySignature(signingKey string, body []byte, r *http.Request) error {
	sig := r.Header.Get("x-xero-signature")
	if sig == "" {
		return fmt.Errorf("missing x-xero-signature header")
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

type payload struct {
	Events []struct {
		ResourceURL   string `json:"resourceUrl"`
		ResourceID    string `json:"resourceId"`
		TenantID      string `json:"tenantId"`
		TenantType    string `json:"tenantType"`
		EventCategory string `json:"eventCategory"`
		EventType     string `json:"eventType"`
		EventDateUtc  string `json:"eventDateUtc"`
	} `json:"events"`
}

// ParseEvent flattens the FIRST event of a Xero webhook into the map the
// executor's Xero Webhook Trigger surfaces as outputs. The full payload is
// always included as `body`. An empty events array (Xero's intent-to-receive
// probe) parses cleanly and simply carries no event fields.
func ParseEvent(body []byte) (map[string]interface{}, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parse xero webhook: %w", err)
	}

	data := map[string]interface{}{
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}

	if len(p.Events) > 0 {
		e := p.Events[0]
		data["tenant_id"] = e.TenantID
		data["resource_id"] = e.ResourceID
		data["resource_url"] = e.ResourceURL
		data["event_category"] = e.EventCategory
		data["operation"] = e.EventType
		// event_type is the canonical field the filter matches, e.g.
		// "CONTACT.UPDATE".
		data["event_type"] = e.EventCategory + "." + e.EventType
		data["content"] = fmt.Sprintf("Xero %s %s (id %s)", e.EventCategory, strings.ToLower(e.EventType), e.ResourceID)
	} else {
		data["content"] = "Xero webhook received"
	}
	return data, nil
}

// HasEvents reports whether the payload contains any events. Xero's
// intent-to-receive probe sends an empty events array with a valid signature —
// the handler should acknowledge it (200) but not fire a flow.
func HasEvents(body []byte) bool {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return false
	}
	return len(p.Events) > 0
}

// MatchesFilter reports whether eventType (e.g. "CONTACT.UPDATE") is allowed by
// a comma-separated filter. Empty filter matches all. A category-only entry
// (e.g. "CONTACT") matches every event type in that category.
func MatchesFilter(eventType, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	category := eventType
	if i := strings.IndexByte(eventType, '.'); i >= 0 {
		category = eventType[:i]
	}
	for _, f := range strings.Split(filter, ",") {
		f = strings.TrimSpace(f)
		if strings.EqualFold(f, eventType) || strings.EqualFold(f, category) {
			return true
		}
	}
	return false
}
