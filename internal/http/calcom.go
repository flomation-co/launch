package http

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"flomation.app/automate/launch"
	calcomwh "flomation.app/automate/launch/internal/calcom"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// calcomHTTPClient is used for the outbound Cal.com API calls that
// register/deregister webhook subscriptions.
var calcomHTTPClient = &http.Client{Timeout: 15 * time.Second}

// calcomAPIBaseURL is a var rather than a const so tests can point the
// subscription calls at an httptest server.
var calcomAPIBaseURL = "https://api.cal.com/v2"

// calcomAllEvents is the default event selection when the trigger config
// carries none (matching the Mailchimp/Calendly convention: empty = all).
var calcomAllEvents = []string{
	"BOOKING_CREATED",
	"BOOKING_RESCHEDULED",
	"BOOKING_CANCELLED",
	"BOOKING_REQUESTED",
	"BOOKING_REJECTED",
	"BOOKING_PAID",
	"BOOKING_NO_SHOW_UPDATED",
	"MEETING_STARTED",
	"MEETING_ENDED",
	"RECORDING_READY",
	"FORM_SUBMITTED",
}

// calcomStateKey is the trigger_state key holding the webhook id and signing
// secret created for a calcom-webhook trigger.
const calcomStateKey = "calcom_webhook"

// calcomWebhookState is what we persist per trigger: the webhook we created (so
// it can be removed on trigger delete), the secret Cal.com signs deliveries
// with, and the events/event type it was registered for (so a config change is
// detected and the webhook recreated).
type calcomWebhookState struct {
	WebhookID   string   `json:"webhook_id"`
	Secret      string   `json:"secret"`
	Events      []string `json:"events"`
	EventTypeID string   `json:"event_type_id"`
}

// calcomDo performs an authenticated Cal.com API request. fullURL may be a path
// (prefixed with the v2 base) or an absolute URL.
func calcomDo(token, method, fullURL string, body interface{}) (map[string]interface{}, int, error) {
	if strings.HasPrefix(fullURL, "/") {
		fullURL = calcomAPIBaseURL + fullURL
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, fullURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := calcomHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]interface{}
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			body := string(raw)
			if len(body) > 512 {
				body = body[:512]
			}
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": body}).Warn("calcom: non-JSON response body")
		}
	}
	return out, resp.StatusCode, nil
}

// calcomEvents parses the trigger's event selection, which resolveTriggerCreds
// hands us as a plain string. It may be a comma-separated list or a JSON array
// (depending on how the multi-select serialised); both are handled. An empty
// selection defaults to all supported events.
func calcomEvents(sel string) []string {
	sel = strings.TrimSpace(sel)
	var out []string
	if strings.HasPrefix(sel, "[") {
		var arr []string
		if json.Unmarshal([]byte(sel), &arr) == nil {
			for _, e := range arr {
				if t := strings.TrimSpace(e); t != "" {
					out = append(out, t)
				}
			}
		}
	} else {
		for _, p := range strings.Split(sel, ",") {
			if t := strings.TrimSpace(p); t != "" {
				out = append(out, t)
			}
		}
	}
	if len(out) == 0 {
		return calcomAllEvents
	}
	return out
}

func calcomSameEventSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, e := range a {
		set[e] = true
	}
	for _, e := range b {
		if !set[e] {
			return false
		}
	}
	return true
}

// loadCalcomState reads the persisted webhook state for a trigger. A missing
// row yields a nil state, not an error.
func (s *Service) loadCalcomState(triggerID string) *calcomWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("calcom trigger: unable to load state")
		return nil
	}
	raw, ok := rows[calcomStateKey]
	if !ok {
		return nil
	}
	var st calcomWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// registerCalcomWebhook auto-registers a Cal.com webhook pointing at
// {PublicURL}/webhook/{trigger_id}. It is idempotent — createTrigger runs on
// every flow save, so an existing webhook with the same event selection and
// event-type scope is left untouched; a changed selection recreates it. Errors
// are logged, never fatal (they must not fail the trigger upsert).
func (s *Service) registerCalcomWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeCalcomWebhook {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	token := creds["api_key"]
	if token == "" {
		log.WithField("trigger_id", tr.ID).Warn("calcom trigger: missing api_key; skipping webhook registration")
		return
	}
	events := calcomEvents(creds["events"])
	eventTypeID := strings.TrimSpace(creds["event_type_id"])
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since the last save: keep the existing webhook. Checked
	// before any API call so the common case costs no round-trip.
	state := s.loadCalcomState(tr.ID)
	if state != nil && state.WebhookID != "" && state.Secret != "" &&
		state.EventTypeID == eventTypeID && calcomSameEventSet(state.Events, events) {
		return
	}

	// The selection or scope changed (or the stored state is unusable): remove
	// the old webhook before creating its replacement. The callback URL is keyed
	// on the stable trigger ID, so a stale webhook would double-fire the flow.
	if state != nil && state.WebhookID != "" {
		if _, status, err := calcomDo(token, http.MethodDelete, "/webhooks/"+state.WebhookID, nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
			log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err}).Warn("calcom trigger: could not remove outdated webhook; skipping registration this pass")
			return
		}
	}

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("calcom trigger: could not generate signing secret")
		return
	}
	secret := hex.EncodeToString(secretBytes)

	body := map[string]interface{}{
		"subscriberUrl": callback,
		"triggers":      events,
		"active":        true,
		"secret":        secret,
	}
	if eventTypeID != "" {
		if n, err := strconv.Atoi(eventTypeID); err == nil {
			body["eventTypeId"] = n
		}
	}

	resp, status, err := calcomDo(token, http.MethodPost, "/webhooks", body)
	if err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err, "response": resp}).Warn("failed to register Cal.com webhook")
		return
	}
	data, _ := resp["data"].(map[string]interface{})
	webhookID, _ := data["id"].(string)
	if webhookID == "" {
		log.WithField("trigger_id", tr.ID).Warn("calcom trigger: webhook created but no id returned")
		return
	}

	stateJSON, _ := json.Marshal(calcomWebhookState{
		WebhookID:   webhookID,
		Secret:      secret,
		Events:      events,
		EventTypeID: eventTypeID,
	})
	if err := s.db.UpsertTriggerState(tr.ID, calcomStateKey, stateJSON); err != nil {
		// Without the secret every delivery would be rejected; remove the
		// orphaned webhook so the next save retries cleanly.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("calcom trigger: unable to persist webhook state; removing webhook")
		// Best-effort teardown of the webhook we just created: without persisted
		// state its secret is lost so deliveries would be rejected anyway. Return
		// values are intentionally discarded — the state error above is the signal.
		_, _, _ = calcomDo(token, http.MethodDelete, "/webhooks/"+webhookID, nil)
	}
}

// deregisterCalcomWebhook removes the webhook we registered for this trigger
// (best effort; logged, never fatal).
func (s *Service) deregisterCalcomWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeCalcomWebhook {
		return
	}
	state := s.loadCalcomState(tr.ID)
	if state == nil || state.WebhookID == "" {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	token := creds["api_key"]
	if token == "" {
		return
	}
	if _, status, err := calcomDo(token, http.MethodDelete, "/webhooks/"+state.WebhookID, nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err}).Warn("failed to deregister Cal.com webhook")
	}
	if err := s.db.DeleteTriggerState(tr.ID, calcomStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("calcom trigger: unable to delete webhook state")
	}
}

// handleCalcomWebhook handles an inbound Cal.com webhook for a trigger. Cal.com
// signs the RAW body (HMAC-SHA256 in the X-Cal-Signature-256 header) with the
// secret we supplied at registration, so the body is read verbatim and verified
// before any parsing. Called from handleWebhook after the trigger has been
// fetched and type-checked.
func (s *Service) handleCalcomWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	state := s.loadCalcomState(id)
	if state == nil {
		log.WithFields(log.Fields{"id": id}).Warn("Cal.com webhook has no registration state on record")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if err := calcomwh.VerifySignature(state.Secret, body, c.Request); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Cal.com webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data, err := calcomwh.ParseEvent(body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("Cal.com webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Event filter (e.g. "BOOKING_CREATED,BOOKING_CANCELLED"); empty matches
	// all. The webhook is already event-scoped, but the filter also guards
	// deliveries from a webhook created before a config change.
	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	event, _ := data["event_type"].(string)
	if !calcomwh.MatchesFilter(event, triggerData["events"]) {
		c.Status(http.StatusOK)
		return
	}

	// Carry __node_id so the executor injects event data into the correct
	// trigger node in multi-trigger flows.
	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Cal.com webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
