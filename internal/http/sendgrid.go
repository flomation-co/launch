package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"flomation.app/automate/launch"
	sendgridwh "flomation.app/automate/launch/internal/sendgrid"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// sendgridHTTPClient is used for the outbound SendGrid API calls that
// register/deregister event webhooks. The hosts are fixed (never
// caller-supplied), so there is no SSRF surface.
var sendgridHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("sendgrid: too many redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			return errors.New("sendgrid: refusing cross-host redirect")
		}
		return nil
	},
}

// sendgridAPIBaseURL / sendgridEUAPIBaseURL are vars rather than consts so
// tests can point the webhook-settings calls at an httptest server. EU
// data-residency subusers must use the EU host — their keys are rejected on
// the global one. Kept in sync with the executor + api.
var sendgridAPIBaseURL = "https://api.sendgrid.com"
var sendgridEUAPIBaseURL = "https://api.eu.sendgrid.com"

// sendgridSettingsPath is the multi-webhook Event Webhook API root; individual
// webhooks live at {path}/{id} and their signing toggle at {path}/signed/{id}.
const sendgridSettingsPath = "/v3/user/webhooks/event/settings"

// sendgridStateKey is the trigger_state key holding the event webhook and
// signature-verification public key created for a sendgrid-webhook trigger.
const sendgridStateKey = "sendgrid_webhook"

// sendgridToggleFields are the event-type booleans the webhook-settings API
// accepts. Note the settings field is spam_report while the payload event name
// is spamreport, and that "blocked" has no toggle of its own.
var sendgridToggleFields = []string{
	"bounce", "click", "deferred", "delivered", "dropped",
	"group_resubscribe", "group_unsubscribe", "open", "processed",
	"spam_report", "unsubscribe",
}

// sendgridWebhookState is what we persist per trigger: the webhook we created
// (so it can be removed on delete), the public key SendGrid signs deliveries
// with, and the events/region/callback it was registered for (so a config
// change is detected and the webhook recreated).
type sendgridWebhookState struct {
	WebhookID   string   `json:"webhook_id"`
	PublicKey   string   `json:"public_key"`
	Events      []string `json:"events"`
	Region      string   `json:"region"`
	CallbackURL string   `json:"callback_url"`
}

// sendgridHostFor maps the trigger's region selection to the fixed API host
// ("" = global, "eu" = EU data residency).
func sendgridHostFor(region string) string {
	if region == "eu" {
		return sendgridEUAPIBaseURL
	}
	return sendgridAPIBaseURL
}

// sendgridRegion reads the trigger's region selection from the raw trigger
// data (it is a fixed dropdown value — "" or "eu" — never a secret ref, so it
// needs no resolveTriggerCreds pass).
func sendgridRegion(tr *launch.Trigger) string {
	var raw map[string]interface{}
	if json.Unmarshal(tr.Data, &raw) != nil {
		return ""
	}
	region, _ := raw["region"].(string)
	if strings.EqualFold(strings.TrimSpace(region), "eu") {
		return "eu"
	}
	return ""
}

// sendgridEvents parses the trigger's event selection (a plain string from
// resolveTriggerCreds — either the multi-select's JSON-array form or a
// comma-separated list). Empty (including the "All events" empty option)
// yields nil, meaning every event type: all settings toggles are enabled and
// inbound events are matched unfiltered.
func sendgridEvents(sel string) []string {
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
	return out
}

// sendgridEventToggles maps the selected payload event names onto the
// webhook-settings toggle booleans. An empty selection enables every toggle.
// The payload name "spamreport" maps to the "spam_report" settings field, and
// "blocked" — which has no toggle of its own (blocks are delivered under the
// bounce/dropped toggles) — enables both bounce and dropped.
func sendgridEventToggles(events []string) map[string]bool {
	toggles := make(map[string]bool, len(sendgridToggleFields))
	for _, f := range sendgridToggleFields {
		toggles[f] = len(events) == 0
	}
	for _, e := range events {
		switch e {
		case "spamreport":
			toggles["spam_report"] = true
		case "blocked":
			toggles["bounce"] = true
			toggles["dropped"] = true
		default:
			if _, ok := toggles[e]; ok {
				toggles[e] = true
			}
		}
	}
	return toggles
}

// sendgridStateCurrent reports whether the persisted state already covers the
// desired registration — same events (order-independent), region and callback,
// with a usable webhook id and public key — so the common unchanged flow save
// costs no SendGrid API calls.
func sendgridStateCurrent(state *sendgridWebhookState, events []string, region, callback string) bool {
	return state != nil && state.WebhookID != "" && state.PublicKey != "" &&
		state.Region == region && state.CallbackURL == callback &&
		sameEventSet(state.Events, events)
}

// sendgridDo performs an authenticated SendGrid API request. fullURL is the
// host (per region) plus path.
func sendgridDo(ctx context.Context, apiKey, method, fullURL string, body interface{}) (map[string]interface{}, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := sendgridHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]interface{}
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			b := string(raw)
			if len(b) > 512 {
				b = b[:512]
			}
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": b}).Warn("sendgrid: non-JSON response body")
		}
	}
	return out, resp.StatusCode, nil
}

// loadSendGridState reads the persisted webhook state for a trigger. A missing
// row yields a nil state, not an error.
func (s *Service) loadSendGridState(triggerID string) *sendgridWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("sendgrid trigger: unable to load state")
		return nil
	}
	raw, ok := rows[sendgridStateKey]
	if !ok {
		return nil
	}
	var st sendgridWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// sendgridCleanupCtx derives a context for compensating deletes that survives
// cancellation of the originating request — the compensation must run exactly
// when the original call may have died with the request.
func sendgridCleanupCtx(ctx context.Context) context.Context {
	// sendgridHTTPClient's own 15s timeout bounds the request; the context
	// only needs to shed the parent's cancellation.
	return context.WithoutCancel(ctx)
}

// deleteSendGridWebhook removes a webhook by id on the given region's host
// (best effort). Returns false when SendGrid refused the delete (non-404
// failure), so callers can skip recreating against a still-live webhook.
func deleteSendGridWebhook(ctx context.Context, apiKey, region, webhookID, triggerID string) bool {
	url := sendgridHostFor(region) + sendgridSettingsPath + "/" + webhookID
	_, status, err := sendgridDo(ctx, apiKey, http.MethodDelete, url, nil)
	if err == nil && (status < 300 || status == http.StatusNotFound) {
		return true
	}
	// An auth failure means the current key cannot manage that webhook at all
	// (key and region changed together, or the key moved accounts); retrying
	// can never succeed, and any deliveries from the orphaned webhook are
	// signed with its old key and fail verification — safe to proceed.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		log.WithFields(log.Fields{"trigger_id": triggerID, "webhook_id": webhookID, "status": status}).Warn("sendgrid trigger: not authorised to delete outdated webhook; proceeding without it")
		return true
	}
	log.WithFields(log.Fields{"trigger_id": triggerID, "webhook_id": webhookID, "status": status, "error": err}).Warn("sendgrid trigger: failed to delete webhook")
	return false
}

// registerSendGridWebhook auto-registers a SendGrid event webhook pointing at
// {PublicURL}/webhook/{trigger_id} with the selected event toggles, then
// enables signed delivery on it and persists the returned public key (there is
// no shared secret — deliveries are ECDSA-signed and verified with this key).
// Idempotent — createTrigger runs on every flow save, so an existing webhook
// with the same events/region is left untouched; a changed config recreates
// it. Errors are logged, never fatal (they must not fail the trigger upsert).
func (s *Service) registerSendGridWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeSendGridWebhook {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	apiKey := strings.TrimSpace(creds["api_key"])
	if apiKey == "" {
		log.WithField("trigger_id", tr.ID).Warn("sendgrid trigger: missing api_key; skipping webhook registration")
		return
	}
	region := sendgridRegion(tr)
	events := sendgridEvents(creds["events"])
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since the last save (and we hold a usable public key):
	// keep the existing webhook. Checked before any SendGrid API call —
	// createTrigger runs on every flow save and the common case must not cost
	// a round-trip.
	state := s.loadSendGridState(tr.ID)
	if sendgridStateCurrent(state, events, region, callback) {
		return
	}

	// The config changed (or the stored state is unusable): remove the old
	// webhook — on the region it was created on — before creating its
	// replacement. The callback is keyed on the stable trigger ID, so a stale
	// webhook would double-fire the flow.
	if state != nil && state.WebhookID != "" {
		if !deleteSendGridWebhook(ctx, apiKey, state.Region, state.WebhookID, tr.ID) {
			log.WithField("trigger_id", tr.ID).Warn("sendgrid trigger: could not remove outdated webhook; skipping registration this pass")
			return
		}
		// The old webhook is gone; drop the state describing it now so a
		// failure below cannot leave stale state that reads as current and
		// suppresses re-registration on the next save.
		if err := s.db.DeleteTriggerState(tr.ID, sendgridStateKey); err != nil {
			log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("sendgrid trigger: unable to clear outdated webhook state")
		}
	}

	body := map[string]interface{}{
		"enabled":       true,
		"url":           callback,
		"friendly_name": "Flomation " + tr.ID,
	}
	for field, on := range sendgridEventToggles(events) {
		body[field] = on
	}
	resp, status, err := sendgridDo(ctx, apiKey, http.MethodPost, sendgridHostFor(region)+sendgridSettingsPath, body)
	if err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err, "response": resp}).Warn("failed to register SendGrid event webhook")
		return
	}
	webhookID := asString(resp["id"])
	if webhookID == "" {
		log.WithField("trigger_id", tr.ID).Warn("sendgrid trigger: webhook created but no id returned")
		return
	}

	// Enable signed delivery on the new webhook (a SEPARATE per-webhook
	// endpoint); the response carries the verification public key. Without it
	// every delivery would be rejected, so a failure here removes the webhook
	// and the next save retries cleanly.
	signed, status, err := sendgridDo(ctx, apiKey, http.MethodPatch, sendgridHostFor(region)+sendgridSettingsPath+"/signed/"+webhookID, map[string]interface{}{"enabled": true})
	publicKey := asString(signed["public_key"])
	if err != nil || status >= 300 || publicKey == "" {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err, "response": signed}).Warn("sendgrid trigger: could not enable signed delivery; removing webhook")
		deleteSendGridWebhook(sendgridCleanupCtx(ctx), apiKey, region, webhookID, tr.ID)
		return
	}

	stateJSON, _ := json.Marshal(sendgridWebhookState{
		WebhookID:   webhookID,
		PublicKey:   publicKey,
		Events:      events,
		Region:      region,
		CallbackURL: callback,
	})
	if err := s.db.UpsertTriggerState(tr.ID, sendgridStateKey, stateJSON); err != nil {
		// Without the stored public key every delivery would be rejected;
		// remove the orphaned webhook so the next save retries cleanly.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("sendgrid trigger: unable to persist webhook state; removing webhook")
		deleteSendGridWebhook(sendgridCleanupCtx(ctx), apiKey, region, webhookID, tr.ID)
	}
}

// deregisterSendGridWebhook removes the event webhook we registered for this
// trigger (best effort; logged, never fatal).
func (s *Service) deregisterSendGridWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeSendGridWebhook {
		return
	}
	state := s.loadSendGridState(tr.ID)
	if state == nil || state.WebhookID == "" {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	apiKey := strings.TrimSpace(creds["api_key"])
	if apiKey == "" {
		return
	}
	deleteSendGridWebhook(ctx, apiKey, state.Region, state.WebhookID, tr.ID)
	if err := s.db.DeleteTriggerState(tr.ID, sendgridStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("sendgrid trigger: unable to delete webhook state")
	}
}

// handleSendGridWebhook handles an inbound SendGrid event webhook delivery for
// a trigger. SendGrid signs the RAW body (ASN.1/DER ECDSA over
// sha256(timestamp || body), Base64 in the X-Twilio-Email-Event-Webhook-*
// headers) with the key pair created when signed delivery was enabled at
// registration, so the body is read verbatim and verified before any parsing —
// no public key on record fails closed. Each delivery is a JSON array of
// events; the flow fires once per matched event, asynchronously (SendGrid
// expects a fast 2xx and retries non-2xx responses). Called from handleWebhook
// after the trigger has been fetched and type-checked.
func (s *Service) handleSendGridWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	state := s.loadSendGridState(id)
	if state == nil || state.PublicKey == "" {
		log.WithFields(log.Fields{"id": id}).Warn("SendGrid webhook has no public key on record")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if err := sendgridwh.VerifySignature(
		state.PublicKey,
		c.GetHeader("X-Twilio-Email-Event-Webhook-Timestamp"),
		c.GetHeader("X-Twilio-Email-Event-Webhook-Signature"),
		body,
	); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("SendGrid webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	events, err := sendgridwh.ParseEvents(body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("SendGrid webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var triggerData map[string]interface{}
	_ = json.Unmarshal(tr.Data, &triggerData)
	nodeID := asString(triggerData["__node_id"])

	// Event filter against state.Events (the RESOLVED selection registration
	// acted on) — the config field can hold an unresolved ${...} reference.
	// The webhook's toggles already scope what SendGrid sends; this guards
	// deliveries from a webhook created before a config change, and the
	// "blocked" selection (which has no toggle of its own). Empty = all. Zero
	// matched events still 200 — a non-2xx would make SendGrid retry the batch.
	for _, event := range events {
		if !sendgridwh.MatchesFilter(event, state.Events) {
			continue
		}
		out := sendgridwh.EventOutputs(event)
		// Carry __node_id so the executor injects event data into the correct
		// trigger node in multi-trigger flows.
		if nodeID != "" {
			out["__node_id"] = nodeID
		}
		go func() {
			if err := s.trigger.Trigger(tr, out); err != nil {
				log.WithFields(log.Fields{"error": err}).Error("unable to fire SendGrid webhook trigger")
			}
		}()
	}

	c.Status(http.StatusOK)
}
