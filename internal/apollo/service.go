// Package apollo verifies and parses inbound Apollo.io webhook events for the
// Apollo Webhook Trigger.
//
// SECURITY: Apollo publishes NO webhook signature, signing secret or handshake
// in its developer docs — webhooks are configured in the Apollo UI and arrive
// unauthenticated. Authenticity therefore rests on two things: the unguessable
// /webhook/:id route, and an author-chosen shared secret that must be presented
// on the request (query param `secret` or the X-Flomation-Webhook-Secret
// header) and is compared in constant time. Absent a real signature this is the
// strongest guarantee available; a future hardening step is to re-fetch the
// referenced record from Apollo's API before firing.
package apollo

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SecretHeader is the fallback header carrying the shared secret when it is not
// supplied as the `secret` query parameter.
const SecretHeader = "X-Flomation-Webhook-Secret" // #nosec G101 — HTTP header name, not a credential

// VerifyAndParse checks the presented shared secret against the configured one
// in constant time, then parses the JSON body. A missing/empty configured
// secret is treated as a failure (never accept an unauthenticated webhook).
func VerifyAndParse(body []byte, r *http.Request, secret string) (map[string]interface{}, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("no webhook secret configured")
	}

	presented := r.URL.Query().Get("secret")
	if presented == "" {
		presented = r.Header.Get(SecretHeader)
	}
	// ConstantTimeCompare returns 0 when lengths differ, so it also covers the
	// empty-presented case without an early, timing-variable return.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
		return nil, fmt.Errorf("webhook secret mismatch")
	}

	payload := map[string]interface{}{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
	}
	return payload, nil
}

// EventToData flattens an Apollo webhook payload into the map the executor's
// Apollo Webhook Trigger surfaces as outputs. Apollo's payload shapes vary by
// event, so the well-known identifying fields are pulled up to stable keys and
// every top-level scalar is also spread as a bare key for downstream reference.
func EventToData(payload map[string]interface{}, rawBody []byte) map[string]interface{} {
	eventType := firstNonEmpty(str(payload["event_type"]), str(payload["type"]), str(payload["webhook_name"]))

	data := map[string]interface{}{
		"event_type":   eventType,
		"event_id":     firstNonEmpty(str(payload["id"]), str(payload["event_id"])),
		"record_id":    firstNonEmpty(str(payload["record_id"]), str(payload["object_id"]), str(payload["contact_id"]), str(payload["account_id"])),
		"body":         string(rawBody),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
		"content":      "Apollo event: " + eventType,
	}

	// Spread top-level scalar keys as bare outputs (without clobbering the
	// stable keys above), mirroring the database-row trigger.
	for k, v := range payload {
		if _, exists := data[k]; exists {
			continue
		}
		switch v.(type) {
		case string, float64, bool:
			data[k] = v
		}
	}
	return data
}

// MatchesFilter reports whether eventType is allowed by a comma-separated filter
// (e.g. "contact.created,account.created"). Empty filter = all.
func MatchesFilter(eventType, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	for _, f := range strings.Split(filter, ",") {
		if strings.EqualFold(strings.TrimSpace(f), eventType) {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func str(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
