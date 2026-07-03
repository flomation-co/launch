package http

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"flomation.app/automate/launch"
	calendlywh "flomation.app/automate/launch/internal/calendly"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// calendlyHTTPClient is used for the outbound Calendly API calls that
// register/deregister webhook subscriptions.
var calendlyHTTPClient = &http.Client{Timeout: 15 * time.Second}

// calendlyAPIBaseURL is a var rather than a const so tests can point the
// subscription calls at an httptest server.
var calendlyAPIBaseURL = "https://api.calendly.com"

// calendlyAllEvents is the default event selection when the trigger config
// carries none (matching the Mailchimp convention: empty selection = all).
var calendlyAllEvents = []string{
	"invitee.created",
	"invitee.canceled",
	"invitee_no_show.created",
	"invitee_no_show.deleted",
	"routing_form_submission.created",
}

// calendlyStateKey is the trigger_state key holding the webhook subscription
// URI and signing key created for a calendly-webhook trigger.
const calendlyStateKey = "calendly_webhook"

// calendlyWebhookState is what we persist per trigger: the subscription we
// created (so it can be removed on trigger delete), the signing key Calendly
// signs deliveries with, and the events/scope it was registered for (so a
// config change is detected and the subscription recreated).
type calendlyWebhookState struct {
	SubscriptionURI string   `json:"subscription_uri"`
	SigningKey      string   `json:"signing_key"`
	Events          []string `json:"events"`
	Scope           string   `json:"scope"`
}

// calendlyDo performs an authenticated Calendly API request. fullURL may be a
// path (prefixed with the API base) or an absolute URI (Calendly addresses
// resources by URI, e.g. a subscription's uri field).
func calendlyDo(token, method, fullURL string, body interface{}) (map[string]interface{}, int, error) {
	if strings.HasPrefix(fullURL, "/") {
		fullURL = calendlyAPIBaseURL + fullURL
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
	resp, err := calendlyHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]interface{}
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			// Non-fatal (callers key off the status code), but log the raw
			// body so an unexpected non-JSON response is debuggable.
			body := string(raw)
			if len(body) > 512 {
				body = body[:512]
			}
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": body}).Warn("calendly: non-JSON response body")
		}
	}
	return out, resp.StatusCode, nil
}

// calendlyEvents parses the trigger's comma-separated event selection,
// defaulting to all supported events when empty.
func calendlyEvents(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return calendlyAllEvents
	}
	return out
}

func sameEventSet(a, b []string) bool {
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

// calendlyScopeURIs resolves the organisation and (for user scope) user URIs
// a subscription must be created with, via /users/me.
func calendlyScopeURIs(token, scope string) (organization, user string, err error) {
	me, status, err := calendlyDo(token, http.MethodGet, "/users/me", nil)
	if err != nil {
		return "", "", err
	}
	if status >= 300 {
		return "", "", fmt.Errorf("calendly /users/me returned %d", status)
	}
	resource, _ := me["resource"].(map[string]interface{})
	if resource == nil {
		return "", "", fmt.Errorf("calendly /users/me returned no resource")
	}
	organization, _ = resource["current_organization"].(string)
	user, _ = resource["uri"].(string)
	if organization == "" || (scope != "organization" && user == "") {
		return "", "", fmt.Errorf("calendly /users/me response missing organisation/user URI")
	}
	return organization, user, nil
}

// loadCalendlyState reads the persisted subscription state for a trigger.
// A missing row yields a nil state, not an error.
func (s *Service) loadCalendlyState(triggerID string) *calendlyWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("calendly trigger: unable to load state")
		return nil
	}
	raw, ok := rows[calendlyStateKey]
	if !ok {
		return nil
	}
	var st calendlyWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// registerCalendlyWebhook auto-registers a Calendly webhook subscription
// pointing at {PublicURL}/webhook/{trigger_id}. Calendly subscriptions can
// only be created via its API (there is no dashboard UI), so unlike the
// Shopify trigger the platform owns the full lifecycle. It is idempotent —
// createTrigger runs on every flow save, so an existing subscription with the
// same event selection is left untouched; a changed selection recreates the
// subscription. Errors are logged, never fatal (they must not fail the
// trigger upsert).
func (s *Service) registerCalendlyWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeCalendlyWebhook {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	token := creds["access_token"]
	if token == "" {
		log.WithField("trigger_id", tr.ID).Warn("calendly trigger: missing access_token; skipping webhook registration")
		return
	}
	scope := creds["scope"]
	if scope == "" {
		scope = "user"
	}
	events := calendlyEvents(creds["events"])
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// If we previously created a subscription with the same events and scope,
	// keep it. Checked before any Calendly API call — createTrigger runs on
	// every flow save, and the common case (nothing changed) must not cost a
	// round-trip to /users/me.
	state := s.loadCalendlyState(tr.ID)
	if state != nil && state.SubscriptionURI != "" && state.SigningKey != "" &&
		state.Scope == scope && sameEventSet(state.Events, events) {
		return
	}

	organization, user, err := calendlyScopeURIs(token, scope)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("calendly trigger: could not resolve scope URIs; skipping webhook registration")
		return
	}

	// The event selection or scope changed (or the stored state is unusable):
	// remove the old subscription before creating its replacement. Calendly
	// has no subscription-update call, and the callback URL is keyed on the
	// stable trigger ID, so a stale subscription would double-fire the flow.
	if state != nil && state.SubscriptionURI != "" {
		if _, status, err := calendlyDo(token, http.MethodDelete, state.SubscriptionURI, nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
			log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err}).Warn("calendly trigger: could not remove outdated subscription; skipping registration this pass")
			return
		}
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("calendly trigger: could not generate signing key")
		return
	}
	signingKey := hex.EncodeToString(keyBytes)

	body := map[string]interface{}{
		"url":          callback,
		"events":       events,
		"organization": organization,
		"scope":        scope,
		"signing_key":  signingKey,
	}
	if scope != "organization" {
		body["user"] = user
	}

	resp, status, err := calendlyDo(token, http.MethodPost, "/webhook_subscriptions", body)
	if err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err, "response": resp}).Warn("failed to register Calendly webhook subscription")
		return
	}
	resource, _ := resp["resource"].(map[string]interface{})
	subscriptionURI, _ := resource["uri"].(string)
	if subscriptionURI == "" {
		log.WithField("trigger_id", tr.ID).Warn("calendly trigger: subscription created but no URI returned")
		return
	}

	stateJSON, _ := json.Marshal(calendlyWebhookState{
		SubscriptionURI: subscriptionURI,
		SigningKey:      signingKey,
		Events:          events,
		Scope:           scope,
	})
	if err := s.db.UpsertTriggerState(tr.ID, calendlyStateKey, stateJSON); err != nil {
		// Without the signing key every delivery would be rejected; remove the
		// orphaned subscription so the next save retries cleanly.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("calendly trigger: unable to persist subscription state; removing subscription")
		_, _, _ = calendlyDo(token, http.MethodDelete, subscriptionURI, nil)
	}
}

// deregisterCalendlyWebhook removes the subscription we registered for this
// trigger (best effort; logged, never fatal).
func (s *Service) deregisterCalendlyWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeCalendlyWebhook {
		return
	}
	state := s.loadCalendlyState(tr.ID)
	if state == nil || state.SubscriptionURI == "" {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	token := creds["access_token"]
	if token == "" {
		return
	}
	if _, status, err := calendlyDo(token, http.MethodDelete, state.SubscriptionURI, nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err}).Warn("failed to deregister Calendly webhook subscription")
	}
	if err := s.db.DeleteTriggerState(tr.ID, calendlyStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("calendly trigger: unable to delete subscription state")
	}
}

// handleCalendlyWebhook handles an inbound Calendly webhook for a trigger.
// Calendly signs the RAW body (HMAC-SHA256 of "<timestamp>.<body>" in the
// Calendly-Webhook-Signature header) with the signing key we supplied at
// subscription time, so the body is read verbatim and verified before any
// parsing. Called from handleWebhook after the trigger has been fetched and
// type-checked.
func (s *Service) handleCalendlyWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	state := s.loadCalendlyState(id)
	if state == nil || state.SigningKey == "" {
		log.WithFields(log.Fields{"id": id}).Warn("Calendly webhook has no signing key on record")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if err := calendlywh.VerifySignature(state.SigningKey, body, c.Request); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Calendly webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data, err := calendlywh.ParseEvent(body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("Calendly webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Event filter (e.g. "invitee.created,invitee.canceled"); empty matches
	// all. The subscription is already event-scoped, but the filter also
	// guards deliveries from a subscription created before a config change.
	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	event, _ := data["event_type"].(string)
	if !calendlywh.MatchesFilter(event, triggerData["events"]) {
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
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Calendly webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
