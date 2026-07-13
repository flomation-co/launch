// Package quickbooks verifies and parses inbound QuickBooks Online webhook
// (Change Data Capture) events for the QuickBooks Webhook Trigger.
//
// Intuit signs the raw request body with HMAC-SHA256 keyed by the app's
// verifier token and sends it base64-encoded in the intuit-signature header.
// A single webhook endpoint serves every company connected to the app, so the
// payload carries a realmId per notification; the trigger surfaces it and the
// changed entity so a flow can fetch the full record.
package quickbooks

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

// VerifySignature validates the intuit-signature header over the raw body using
// the app verifier token (HMAC-SHA256, base64). Uses a constant-time compare.
func VerifySignature(verifierToken string, body []byte, r *http.Request) error {
	sig := r.Header.Get("intuit-signature")
	if sig == "" {
		return fmt.Errorf("missing intuit-signature header")
	}
	mac := hmac.New(sha256.New, []byte(verifierToken))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// eventNotification mirrors the QBO CDC payload shape.
type payload struct {
	EventNotifications []struct {
		RealmID         string `json:"realmId"`
		DataChangeEvent struct {
			Entities []struct {
				Name        string `json:"name"`
				ID          string `json:"id"`
				Operation   string `json:"operation"`
				LastUpdated string `json:"lastUpdated"`
			} `json:"entities"`
		} `json:"dataChangeEvent"`
	} `json:"eventNotifications"`
}

// ParseEvent flattens the FIRST changed entity of a QBO webhook into the map
// the executor's QuickBooks Webhook Trigger surfaces as outputs. QBO batches
// notifications (multiple companies/entities per POST); the full payload is
// always included as `body` so a flow can iterate the rest if needed.
func ParseEvent(body []byte) (map[string]interface{}, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parse quickbooks webhook: %w", err)
	}

	data := map[string]interface{}{
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}

	for _, n := range p.EventNotifications {
		data["realm_id"] = n.RealmID
		for _, e := range n.DataChangeEvent.Entities {
			data["entity"] = e.Name
			data["entity_id"] = e.ID
			data["operation"] = e.Operation
			data["last_updated"] = e.LastUpdated
			// event_type is the canonical field the filter matches against,
			// e.g. "Customer.Create".
			data["event_type"] = e.Name + "." + e.Operation
			data["content"] = fmt.Sprintf("QuickBooks %s %s (id %s)", e.Name, strings.ToLower(e.Operation), e.ID)
			return data, nil
		}
	}

	data["content"] = "QuickBooks webhook received"
	return data, nil
}

// MatchesFilter reports whether eventType (e.g. "Customer.Create") is allowed by
// a comma-separated filter. An empty filter matches everything. Matching also
// accepts an entity-only filter entry (e.g. "Customer") so a flow can react to
// all operations on an entity.
func MatchesFilter(eventType, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	entity := eventType
	if i := strings.IndexByte(eventType, '.'); i >= 0 {
		entity = eventType[:i]
	}
	for _, f := range strings.Split(filter, ",") {
		f = strings.TrimSpace(f)
		if strings.EqualFold(f, eventType) || strings.EqualFold(f, entity) {
			return true
		}
	}
	return false
}
