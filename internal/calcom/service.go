package calcom

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

// VerifySignature validates a Cal.com webhook. Cal.com signs the RAW request
// body with HMAC-SHA256 keyed by the secret supplied when the webhook was
// created, and sends the hex digest in the X-Cal-Signature-256 header. The
// comparison is constant-time.
//
// When no secret is on record (secret == "") verification is skipped and the
// delivery is accepted — Cal.com omits the signature header for secretless
// webhooks, and the platform always creates webhooks with a secret, so this
// path only matters for hand-created/legacy subscriptions.
func VerifySignature(secret string, body []byte, r *http.Request) error {
	if secret == "" {
		return nil
	}
	sig := r.Header.Get("X-Cal-Signature-256")
	if sig == "" {
		return fmt.Errorf("missing X-Cal-Signature-256 header")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(strings.TrimSpace(sig)), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// ParseEvent builds the trigger's output data from a Cal.com webhook. The
// standard envelope is {"triggerEvent", "createdAt", "payload": {...}}; for
// MEETING_STARTED / MEETING_ENDED the booking fields sit flat at the top level
// with no payload wrapper, so we fall back to the whole body as the payload in
// that case. The attendee name/email, booking uid and start time are lifted
// from the payload for direct wiring, and the raw body is preserved so
// downstream nodes can read any field.
func ParseEvent(body []byte) (map[string]interface{}, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid Cal.com webhook body: %w", err)
	}

	event, _ := raw["triggerEvent"].(string)
	createdAt, _ := raw["createdAt"].(string)

	// Standard events nest the booking under "payload"; MEETING_* events are
	// flat, so treat the whole body as the payload when no wrapper exists.
	payload, _ := raw["payload"].(map[string]interface{})
	if payload == nil {
		payload = raw
	}

	// event_type and event are deliberately the same value. "event_type" is the
	// canonical field every webhook trigger exposes for cross-integration flows
	// (and what MatchesFilter reads); "event" is kept alongside it as the
	// Cal.com-native name so a flow author familiar with Cal.com's docs finds it.
	data := map[string]interface{}{
		"event_type":   event, // used by MatchesFilter
		"event":        event,
		"created_at":   createdAt,
		"payload":      payload,
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}

	if uid, ok := payload["uid"].(string); ok {
		data["booking_uid"] = uid
	}
	if start, ok := payload["startTime"].(string); ok {
		data["start_time"] = start
	}
	// Attendees is an array of {name, email, ...}; surface the first.
	if attendees, ok := payload["attendees"].([]interface{}); ok && len(attendees) > 0 {
		if a, ok := attendees[0].(map[string]interface{}); ok {
			if v, ok := a["name"].(string); ok {
				data["attendee_name"] = v
			}
			if v, ok := a["email"].(string); ok {
				data["attendee_email"] = v
			}
		}
	}

	return data, nil
}

// MatchesFilter checks whether the Cal.com event matches the comma-separated
// selection (e.g. "BOOKING_CREATED,BOOKING_CANCELLED"). An empty selection
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
