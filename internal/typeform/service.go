// Package typeform verifies and parses inbound Typeform webhook events for the
// Typeform Webhook Trigger. Typeform signs the raw request body with an
// HMAC-SHA256 keyed by the endpoint secret and delivers it in the
// `Typeform-Signature` header as `sha256=<base64>`.
package typeform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Event is the parsed Typeform webhook payload. Only the fields the trigger
// surfaces are decoded; the full raw body is retained on `raw` so EventToData
// can emit it verbatim.
type Event struct {
	EventID      string       `json:"event_id"`
	EventType    string       `json:"event_type"`
	FormResponse FormResponse `json:"form_response"`

	// raw is the verbatim request body, retained for the `body` output.
	raw []byte
}

// FormResponse is the `form_response` object nested in the event payload.
type FormResponse struct {
	FormID      string            `json:"form_id"`
	Token       string            `json:"token"`
	SubmittedAt string            `json:"submitted_at"`
	Answers     json.RawMessage   `json:"answers"`
	Hidden      map[string]string `json:"hidden"`
	Definition  json.RawMessage   `json:"definition"`
}

// VerifyAndParse verifies the Typeform-Signature over the raw body against the
// endpoint secret and returns the parsed event. The header is of the form
// `sha256=<base64>` where the digest is HMAC-SHA256(rawBody, secret). The
// prefix is stripped, the value base64-decoded, and compared with the locally
// computed digest in constant time via hmac.Equal.
func VerifyAndParse(body []byte, r *http.Request, secret string) (Event, error) {
	header := r.Header.Get("Typeform-Signature")
	if header == "" {
		return Event{}, fmt.Errorf("missing Typeform-Signature header")
	}

	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return Event{}, fmt.Errorf("unexpected Typeform-Signature format")
	}
	provided, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return Event{}, fmt.Errorf("invalid Typeform-Signature encoding: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	if !hmac.Equal(provided, expected) {
		return Event{}, fmt.Errorf("Typeform-Signature verification failed")
	}

	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return Event{}, fmt.Errorf("unable to parse Typeform payload: %w", err)
	}
	ev.raw = body
	return ev, nil
}

// EventToData flattens a Typeform event into the map the executor's Typeform
// Webhook Trigger surfaces as outputs. The keys here MUST match the trigger
// action's declared outputs.
func EventToData(ev Event) map[string]interface{} {
	answers := "[]"
	if len(ev.FormResponse.Answers) > 0 {
		answers = string(ev.FormResponse.Answers)
	}

	return map[string]interface{}{
		"event_type":     ev.EventType,
		"event_id":       ev.EventID,
		"form_id":        ev.FormResponse.FormID,
		"response_token": ev.FormResponse.Token,
		"submitted_at":   ev.FormResponse.SubmittedAt,
		"answers":        answers,
		"body":           string(ev.raw),
		"content":        "Typeform response for form " + ev.FormResponse.FormID,
	}
}

// MatchesFilter reports whether formID is allowed by an optional form_id
// filter. An empty filter matches all forms.
func MatchesFilter(formID, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	return filter == formID
}
