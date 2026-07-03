package calendly

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// VerifySignature validates a Calendly webhook. Calendly sends the signature
// in the Calendly-Webhook-Signature header as "t=<timestamp>,v1=<hex>", where
// v1 is the HMAC-SHA256 of "<timestamp>.<raw body>" keyed with the signing key
// supplied when the webhook subscription was created. The comparison is
// constant-time.
func VerifySignature(signingKey string, body []byte, r *http.Request) error {
	header := r.Header.Get("Calendly-Webhook-Signature")
	if header == "" {
		return fmt.Errorf("missing Calendly-Webhook-Signature header")
	}

	timestamp, sig := parseSignatureHeader(header)
	if timestamp == "" || sig == "" {
		return fmt.Errorf("malformed Calendly-Webhook-Signature header")
	}

	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// parseSignatureHeader splits "t=<timestamp>,v1=<signature>" into its parts.
// Unknown segments are ignored so Calendly can add scheme versions without
// breaking verification.
func parseSignatureHeader(header string) (timestamp, signature string) {
	for _, part := range strings.Split(header, ",") {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(k) {
		case "t":
			timestamp = strings.TrimSpace(v)
		case "v1":
			signature = strings.TrimSpace(v)
		}
	}
	return timestamp, signature
}

// ParseEvent builds the trigger's output data from a Calendly webhook. The
// envelope is {"created_at", "created_by", "event", "payload": {...}}; the
// invitee name/email and event start time are lifted from the payload for
// direct wiring, and the raw body is preserved so downstream nodes can read
// any field.
func ParseEvent(body []byte) (map[string]interface{}, error) {
	var envelope struct {
		CreatedAt string                 `json:"created_at"`
		Event     string                 `json:"event"`
		Payload   map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("invalid Calendly webhook body: %w", err)
	}

	// event_type and event are deliberately the same value. "event_type" is the
	// canonical field every webhook trigger exposes for cross-integration flows
	// (and what MatchesFilter reads); "event" is kept alongside it as the
	// Calendly-native name so a flow author familiar with Calendly's docs finds it.
	data := map[string]interface{}{
		"event_type":   envelope.Event, // used by MatchesFilter
		"event":        envelope.Event,
		"created_at":   envelope.CreatedAt,
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}
	if envelope.Payload != nil {
		data["payload"] = envelope.Payload
		if v, ok := envelope.Payload["name"].(string); ok {
			data["invitee_name"] = v
		}
		if v, ok := envelope.Payload["email"].(string); ok {
			data["invitee_email"] = v
		}
		// invitee.* payloads carry the booking under "scheduled_event".
		if ev, ok := envelope.Payload["scheduled_event"].(map[string]interface{}); ok {
			if v, ok := ev["start_time"].(string); ok {
				data["event_start_time"] = v
			}
		}
	}

	return data, nil
}

// MatchesFilter checks whether the Calendly event matches the comma-separated
// selection (e.g. "invitee.created,invitee.canceled"). An empty selection
// matches all.
func MatchesFilter(event, filter string) bool {
	if strings.TrimSpace(filter) == "" {
		return true
	}
	for _, f := range strings.Split(filter, ",") {
		if strings.TrimSpace(f) == event {
			return true
		}
	}
	return false
}
