// Package surveymonkey parses inbound SurveyMonkey webhook events for the
// SurveyMonkey Webhook Trigger. Unlike Typeform/Stripe, SurveyMonkey webhooks
// are NOT HMAC-signed: they POST a JSON body describing the event (event_type,
// object_type, object_id and a nested resources object carrying survey_id,
// collector_id and respondent_id). Security therefore rests on the unguessable
// `/webhook/:id` UUID plus an OPTIONAL shared-secret token — if a `secret` is
// configured on the trigger, a matching `?token=` query param is required
// (constant-time compare); if not, the opaque id is the only guard.
package surveymonkey

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"strings"
)

// Event is the parsed SurveyMonkey webhook payload. Only the fields the trigger
// surfaces are decoded; the full raw body is emitted separately by EventToData.
type Event struct {
	EventType  string    `json:"event_type"`
	ObjectType string    `json:"object_type"`
	ObjectID   string    `json:"object_id"`
	Resources  Resources `json:"resources"`
}

// Resources is the nested `resources` object identifying the survey, collector
// and respondent the event relates to.
type Resources struct {
	SurveyID     string `json:"survey_id"`
	CollectorID  string `json:"collector_id"`
	RespondentID string `json:"respondent_id"`
}

// Parse json-unmarshals a SurveyMonkey webhook body into an Event.
func Parse(body []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return Event{}, fmt.Errorf("unable to parse SurveyMonkey payload: %w", err)
	}
	return ev, nil
}

// EventToData flattens a SurveyMonkey event into the map the executor's
// SurveyMonkey Webhook Trigger surfaces as outputs. The keys here MUST match the
// trigger action's declared outputs.
func EventToData(ev Event, rawBody []byte) map[string]interface{} {
	return map[string]interface{}{
		"event_type":  ev.EventType,
		"object_type": ev.ObjectType,
		"object_id":   ev.ObjectID,
		"survey_id":   ev.Resources.SurveyID,
		"response_id": ev.Resources.RespondentID,
		"body":        string(rawBody),
		"content":     "SurveyMonkey " + ev.EventType + " on survey " + ev.Resources.SurveyID,
	}
}

// MatchesFilter reports whether surveyID is allowed by an optional survey_id
// filter. An empty filter matches all surveys.
func MatchesFilter(surveyID, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	return filter == surveyID
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
