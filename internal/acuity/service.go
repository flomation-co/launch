package acuity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VerifySignature validates an Acuity webhook. Acuity signs the RAW,
// form-urlencoded request body with HMAC-SHA256 keyed by the account's API key
// and sends the base64 digest in the X-Acuity-Signature header. The comparison
// is constant-time. (OAuth2 apps have no shared secret; this path is only used
// for API-key auth, which is all the trigger registers today.)
func VerifySignature(apiKey string, body []byte, r *http.Request) error {
	sig := r.Header.Get("X-Acuity-Signature")
	if sig == "" {
		return fmt.Errorf("missing X-Acuity-Signature header")
	}

	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(strings.TrimSpace(sig)), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// ParseEvent builds the trigger's output data from an Acuity webhook. The body
// is form-urlencoded and carries only identifiers: `action` (the event string),
// `id` (appointment or order id), `calendarID` and `appointmentTypeID` (for
// appointment events). The full object is resolved separately (see the http
// handler's resolveData path) since Acuity sends IDs only.
func ParseEvent(body []byte) (map[string]interface{}, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("invalid Acuity webhook body: %w", err)
	}

	action := values.Get("action")

	// event_type and event are deliberately the same value. "event_type" is the
	// canonical field every webhook trigger exposes (and what MatchesFilter
	// reads); "event" is the Acuity-native name a flow author expects.
	data := map[string]interface{}{
		"event_type":          action, // used by MatchesFilter
		"event":               action,
		"object_id":           values.Get("id"),
		"calendar_id":         values.Get("calendarID"),
		"appointment_type_id": values.Get("appointmentTypeID"),
		"body":                string(body),
		"triggered_at":        time.Now().UTC().Format(time.RFC3339),
	}
	return data, nil
}

// ResourcePath returns the API path to resolve the full object for an event.
// appointment.* → /appointments/{id}; order.* → /orders/{id}. Returns "" for
// unknown event families.
func ResourcePath(action, id string) string {
	if id == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(action, "appointment."):
		return "/appointments/" + url.PathEscape(id)
	case strings.HasPrefix(action, "order."):
		return "/orders/" + url.PathEscape(id)
	}
	return ""
}

// MatchesFilter checks whether the Acuity event matches the comma-separated
// selection (e.g. "appointment.scheduled,appointment.canceled"). An empty
// selection matches all.
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
