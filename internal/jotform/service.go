// Package jotform parses inbound JotForm webhook events for the JotForm
// Webhook Trigger. Unlike Typeform/Stripe, JotForm webhooks are NOT
// HMAC-signed: they POST multipart/form-data carrying `formID`,
// `submissionID`, `rawRequest` (a JSON string of the submission answers) and
// `type`. Security therefore rests on the unguessable `/webhook/:id` UUID plus
// an OPTIONAL shared-secret token — if a `secret` is configured on the trigger,
// a matching `?token=` query param is required (constant-time compare); if not,
// the opaque id is the only guard.
package jotform

import (
	"crypto/hmac"
	"fmt"
	"net/http"
	"strings"
)

// Event is the parsed JotForm webhook payload. Only the fields the trigger
// surfaces are decoded; RawRequest is retained verbatim so EventToData can emit
// it as both `answers` and `body`.
type Event struct {
	FormID       string
	SubmissionID string
	RawRequest   string
	Type         string
}

// ParseMultipart reads a JotForm webhook's multipart/form-data body and
// extracts the fields the trigger surfaces. JotForm posts `formID`,
// `submissionID`, `rawRequest` (a JSON string) and `type`.
func ParseMultipart(r *http.Request) (Event, error) {
	// Bound the body before parsing so a hostile client cannot exhaust memory
	// with an oversized multipart upload. The body is already capped by
	// MaxBytesReader, so ParseMultipartForm cannot over-read.
	r.Body = http.MaxBytesReader(nil, r.Body, 8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil { // #nosec G120 -- body bounded by MaxBytesReader above

		return Event{}, fmt.Errorf("unable to parse JotForm multipart body: %w", err)
	}

	return Event{
		FormID:       r.FormValue("formID"),
		SubmissionID: r.FormValue("submissionID"),
		RawRequest:   r.FormValue("rawRequest"),
		Type:         r.FormValue("type"),
	}, nil
}

// EventToData flattens a JotForm event into the map the executor's JotForm
// Webhook Trigger surfaces as outputs. The keys here MUST match the trigger
// action's declared outputs.
func EventToData(ev Event) map[string]interface{} {
	eventType := ev.Type
	if eventType == "" {
		eventType = "submission"
	}

	return map[string]interface{}{
		"event_type":    eventType,
		"form_id":       ev.FormID,
		"submission_id": ev.SubmissionID,
		"answers":       ev.RawRequest,
		"body":          ev.RawRequest,
		"content":       "JotForm submission for form " + ev.FormID,
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

// TokenOK reports whether an inbound request is authorised. When no secret is
// configured the opaque webhook UUID is the only guard, so any request passes.
// When a secret IS configured the provided `?token=` must match it in constant
// time (hmac.Equal), thwarting timing attacks.
func TokenOK(configuredSecret, providedToken string) bool {
	if configuredSecret == "" {
		return true
	}
	return hmac.Equal([]byte(configuredSecret), []byte(providedToken))
}
