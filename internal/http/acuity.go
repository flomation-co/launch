package http

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"flomation.app/automate/launch"
	acuitywh "flomation.app/automate/launch/internal/acuity"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// acuityHTTPClient is used for the outbound Acuity API calls that
// register/deregister webhook subscriptions and resolve objects.
var acuityHTTPClient = &http.Client{Timeout: 15 * time.Second}

// acuityAPIBaseURL is a var rather than a const so tests can point the calls at
// an httptest server.
var acuityAPIBaseURL = "https://acuityscheduling.com/api/v1"

// acuityAllEvents is the default event selection when the trigger config
// carries none (matching the Mailchimp/Calendly convention: empty = all).
var acuityAllEvents = []string{
	"appointment.scheduled",
	"appointment.rescheduled",
	"appointment.canceled",
	"appointment.changed",
	"order.completed",
}

// acuityStateKey is the trigger_state key holding the webhook subscriptions
// created for an acuity-webhook trigger.
const acuityStateKey = "acuity_webhook"

// acuityReg is a single webhook subscription (Acuity webhooks are one-event
// each, so a multi-event trigger owns several).
type acuityReg struct {
	Event string `json:"event"`
	ID    string `json:"id"`
}

// acuityWebhookState is what we persist per trigger: the subscriptions we
// created (so they can be removed on delete) and the event set they cover (so a
// config change is detected and the subscriptions recreated).
type acuityWebhookState struct {
	Webhooks []acuityReg `json:"webhooks"`
	Events   []string    `json:"events"`
}

// acuityDo performs an authenticated (HTTP Basic userID:apiKey) Acuity API
// request. fullURL may be a path (prefixed with the API base) or absolute.
func acuityDo(userID, apiKey, method, fullURL string, body interface{}) (map[string]interface{}, int, error) {
	if strings.HasPrefix(fullURL, "/") {
		fullURL = acuityAPIBaseURL + fullURL
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
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(userID+":"+apiKey)))
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := acuityHTTPClient.Do(req)
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
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": body}).Warn("acuity: non-JSON response body")
		}
	}
	return out, resp.StatusCode, nil
}

// acuityEvents parses the trigger's event selection (a plain string from
// resolveTriggerCreds — comma-separated or a JSON array). Empty → all events.
func acuityEvents(sel string) []string {
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
		return acuityAllEvents
	}
	return out
}

func acuitySameEventSet(a, b []string) bool {
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

// loadAcuityState reads the persisted subscriptions for a trigger. A missing
// row yields a nil state, not an error.
func (s *Service) loadAcuityState(triggerID string) *acuityWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("acuity trigger: unable to load state")
		return nil
	}
	raw, ok := rows[acuityStateKey]
	if !ok {
		return nil
	}
	var st acuityWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// deleteAcuityWebhooks removes the given subscriptions (best effort).
func (s *Service) deleteAcuityWebhooks(userID, apiKey string, regs []acuityReg) {
	for _, r := range regs {
		if r.ID == "" {
			continue
		}
		if _, status, err := acuityDo(userID, apiKey, http.MethodDelete, "/webhooks/"+r.ID, nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
			log.WithFields(log.Fields{"webhook_id": r.ID, "status": status, "error": err}).Warn("acuity trigger: failed to delete webhook")
		}
	}
}

// registerAcuityWebhook auto-registers one Acuity webhook per selected event
// pointing at {PublicURL}/webhook/{trigger_id} (Acuity subscriptions are
// single-event). Idempotent — createTrigger runs on every flow save, so an
// unchanged event set is left untouched; a changed set recreates all
// subscriptions. Errors are logged, never fatal.
func (s *Service) registerAcuityWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeAcuityWebhook {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	userID := creds["user_id"]
	apiKey := creds["api_key"]
	if userID == "" || apiKey == "" {
		log.WithField("trigger_id", tr.ID).Warn("acuity trigger: missing user_id/api_key; skipping webhook registration")
		return
	}
	events := acuityEvents(creds["events"])
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since the last save: keep the existing subscriptions.
	state := s.loadAcuityState(tr.ID)
	if state != nil && len(state.Webhooks) > 0 && acuitySameEventSet(state.Events, events) {
		return
	}

	// The selection changed (or stored state is unusable): remove the old
	// subscriptions before creating replacements. The callback is keyed on the
	// stable trigger ID, so stale subscriptions would double-fire the flow.
	if state != nil && len(state.Webhooks) > 0 {
		s.deleteAcuityWebhooks(userID, apiKey, state.Webhooks)
	}

	var regs []acuityReg
	for _, event := range events {
		body := map[string]interface{}{"target": callback, "event": event}
		resp, status, err := acuityDo(userID, apiKey, http.MethodPost, "/webhooks", body)
		if err != nil || status >= 300 {
			log.WithFields(log.Fields{"trigger_id": tr.ID, "event": event, "status": status, "error": err, "response": resp}).Warn("failed to register Acuity webhook")
			continue
		}
		id := ""
		switch v := resp["id"].(type) {
		case string:
			id = v
		case float64:
			id = fmt.Sprintf("%d", int64(v))
		}
		if id == "" {
			log.WithFields(log.Fields{"trigger_id": tr.ID, "event": event}).Warn("acuity trigger: webhook created but no id returned")
			continue
		}
		regs = append(regs, acuityReg{Event: event, ID: id})
	}
	if len(regs) == 0 {
		return
	}

	stateJSON, _ := json.Marshal(acuityWebhookState{Webhooks: regs, Events: events})
	if err := s.db.UpsertTriggerState(tr.ID, acuityStateKey, stateJSON); err != nil {
		// Roll back the just-created subscriptions so the next save retries cleanly.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("acuity trigger: unable to persist state; removing webhooks")
		s.deleteAcuityWebhooks(userID, apiKey, regs)
	}
}

// deregisterAcuityWebhook removes every subscription we registered for this
// trigger (best effort; logged, never fatal).
func (s *Service) deregisterAcuityWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeAcuityWebhook {
		return
	}
	state := s.loadAcuityState(tr.ID)
	if state == nil || len(state.Webhooks) == 0 {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	userID := creds["user_id"]
	apiKey := creds["api_key"]
	if userID == "" || apiKey == "" {
		return
	}
	s.deleteAcuityWebhooks(userID, apiKey, state.Webhooks)
	if err := s.db.DeleteTriggerState(tr.ID, acuityStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("acuity trigger: unable to delete state")
	}
}

// handleAcuityWebhook handles an inbound Acuity webhook for a trigger. Acuity
// signs the RAW form-urlencoded body (HMAC-SHA256, base64, X-Acuity-Signature)
// with the account API key, so the body is read verbatim and verified before
// any parsing. When the trigger's resolve_data is on (default), the full object
// is fetched via the API since Acuity delivers only IDs.
func (s *Service) handleAcuityWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	creds := s.resolveTriggerCreds(id)
	userID := creds["user_id"]
	apiKey := creds["api_key"]
	if apiKey == "" {
		log.WithFields(log.Fields{"id": id}).Warn("Acuity webhook has no api_key on record")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if err := acuitywh.VerifySignature(apiKey, body, c.Request); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Acuity webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data, err := acuitywh.ParseEvent(body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("Acuity webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Read the trigger config (events filter + resolve_data flag).
	var triggerData map[string]interface{}
	_ = json.Unmarshal(tr.Data, &triggerData)
	eventsFilter := asString(triggerData["events"])
	event, _ := data["event_type"].(string)
	if !acuitywh.MatchesFilter(event, eventsFilter) {
		c.Status(http.StatusOK)
		return
	}

	// resolve_data defaults to true unless explicitly false.
	resolve := true
	if v, ok := triggerData["resolve_data"].(bool); ok {
		resolve = v
	} else if s := asString(triggerData["resolve_data"]); s == "false" {
		resolve = false
	}
	if resolve && userID != "" {
		objID, _ := data["object_id"].(string)
		if path := acuitywh.ResourcePath(event, objID); path != "" {
			if obj, status, derr := acuityDo(userID, apiKey, http.MethodGet, path, nil); derr == nil && status < 300 && obj != nil {
				data["appointment"] = obj
			} else {
				log.WithFields(log.Fields{"id": id, "status": status, "error": derr}).Warn("acuity trigger: could not resolve full object")
			}
		}
	}

	if nodeID := asString(triggerData["__node_id"]); nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Acuity webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}

// asString coerces a JSON value to a string (empty for nil/non-strings).
func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
