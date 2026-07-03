package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"flomation.app/automate/launch"
	zendeskwh "flomation.app/automate/launch/internal/zendesk"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// zendeskHTTPClient is used for the outbound Zendesk API calls that
// register/deregister the webhook connector and its business-rule trigger.
var zendeskHTTPClient = &http.Client{Timeout: 15 * time.Second}

// zendeskHostOverride, when non-empty, replaces the per-subdomain
// https://{sub}.zendesk.com base so tests can point the registration calls at
// an httptest server.
var zendeskHostOverride = ""

// zendeskStateKey is the trigger_state key holding the Zendesk webhook +
// trigger IDs and signing secret created for a zendesk-webhook trigger.
const zendeskStateKey = "zendesk_webhook"

// zendeskWebhookMessage is the JSON body template the Zendesk business rule
// renders and POSTs to our callback. Its keys match the trigger node's output
// ports (see internal/zendesk.ParseEvent and the executor trigger action).
const zendeskWebhookMessage = `{"ticket_id":"{{ticket.id}}","subject":"{{ticket.title}}","status":"{{ticket.status}}","priority":"{{ticket.priority}}","requester_email":"{{ticket.requester.email}}","description":"{{ticket.description}}","via":"{{ticket.via}}"}`

// zendeskWebhookState is what we persist per trigger: the Zendesk webhook
// connector and business-rule trigger we created (so both can be removed on
// trigger delete), the signing secret Zendesk signs deliveries with, and the
// conditions/subdomain it was registered for (so a config change is detected
// and the pair recreated).
type zendeskWebhookState struct {
	WebhookID     string `json:"webhook_id"`
	TriggerID     string `json:"trigger_id"`
	SigningSecret string `json:"signing_secret"`
	Conditions    string `json:"conditions"`
	Subdomain     string `json:"subdomain"`
}

// normaliseZendeskSubdomain reduces whatever the user pasted to the bare
// account handle so only "https://{handle}.zendesk.com" is ever assembled.
func normaliseZendeskSubdomain(sub string) string {
	sub = strings.TrimSpace(sub)
	sub = strings.TrimPrefix(sub, "https://")
	sub = strings.TrimPrefix(sub, "http://")
	sub = strings.TrimRight(sub, "/")
	sub = strings.TrimSuffix(sub, ".zendesk.com")
	if i := strings.IndexAny(sub, "/.?#:@"); i >= 0 {
		sub = sub[:i]
	}
	return sub
}

// zendeskURL builds a full Support API URL. Webhook connector endpoints
// (/webhooks...) are addressed without a .json suffix; everything else
// (/triggers.json) carries it — the caller passes the exact path.
func zendeskURL(subdomain, path string) string {
	if zendeskHostOverride != "" {
		return zendeskHostOverride + "/api/v2" + path
	}
	return "https://" + subdomain + ".zendesk.com/api/v2" + path
}

// zendeskDo performs an authenticated Zendesk Support API request.
func zendeskDo(authHeader, subdomain, method, path string, body interface{}) (map[string]interface{}, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, zendeskURL(subdomain, path), rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := zendeskHTTPClient.Do(req)
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
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": body}).Warn("zendesk: non-JSON response body")
		}
	}
	return out, resp.StatusCode, nil
}

// zendeskConditions parses the trigger's optional conditions JSON, defaulting to
// firing on every ticket creation and update when none is supplied. Zendesk
// business rules require at least one condition.
func zendeskConditions(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		var cond map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &cond); err == nil && len(cond) > 0 {
			return cond
		}
		log.Warn("zendesk trigger: conditions is not a valid JSON object; using the create/update default")
	}
	return map[string]interface{}{
		"any": []map[string]interface{}{
			{"field": "update_type", "operator": "is", "value": "Create"},
			{"field": "update_type", "operator": "is", "value": "Change"},
		},
	}
}

// loadZendeskState reads the persisted webhook/trigger state for a trigger.
// A missing row yields a nil state, not an error.
func (s *Service) loadZendeskState(triggerID string) *zendeskWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("zendesk trigger: unable to load state")
		return nil
	}
	raw, ok := rows[zendeskStateKey]
	if !ok {
		return nil
	}
	var st zendeskWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// registerZendeskWebhook auto-registers the Zendesk webhook connector and its
// business-rule trigger, both pointing at {PublicURL}/webhook/{trigger_id}.
// This is the two-object dance the Zendesk API requires: a "webhook" carries
// the endpoint + signing secret, and a "trigger" fires it on matching ticket
// events. It is idempotent — createTrigger runs on every flow save, so an
// unchanged config is left untouched; a changed subdomain/conditions recreates
// the pair. Errors are logged, never fatal (they must not fail the upsert).
func (s *Service) registerZendeskWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeZendeskWebhook {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	subdomain := normaliseZendeskSubdomain(creds["subdomain"])
	if subdomain == "" {
		log.WithField("trigger_id", tr.ID).Warn("zendesk trigger: missing subdomain; skipping webhook registration")
		return
	}
	auth := zendeskwh.AuthHeader(creds["email"], creds["api_token"], creds["oauth_token"])
	if auth == "" {
		log.WithField("trigger_id", tr.ID).Warn("zendesk trigger: missing credentials; skipping webhook registration")
		return
	}
	conditions := strings.TrimSpace(creds["conditions"])
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since we last registered — skip before any API call.
	state := s.loadZendeskState(tr.ID)
	if state != nil && state.WebhookID != "" && state.TriggerID != "" && state.SigningSecret != "" &&
		state.Subdomain == subdomain && state.Conditions == conditions {
		return
	}

	// Config changed (or stored state is unusable): tear down the old pair
	// before creating the replacement so the flow can't double-fire.
	if state != nil {
		s.deleteZendeskRemote(auth, state.Subdomain, state)
	}

	// 1. Create the webhook connector.
	webhookBody := map[string]interface{}{
		"webhook": map[string]interface{}{
			"name":           "Flomation " + tr.ID,
			"endpoint":       callback,
			"http_method":    "POST",
			"request_format": "json",
			"status":         "active",
			"subscriptions":  []string{"conditional_ticket_events"},
		},
	}
	resp, status, err := zendeskDo(auth, subdomain, http.MethodPost, "/webhooks", webhookBody)
	if err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err, "response": resp}).Warn("failed to create Zendesk webhook")
		return
	}
	webhook, _ := resp["webhook"].(map[string]interface{})
	webhookID, _ := webhook["id"].(string)
	if webhookID == "" {
		log.WithField("trigger_id", tr.ID).Warn("zendesk trigger: webhook created but no id returned")
		return
	}

	// 2. Fetch the signing secret used to verify inbound deliveries.
	secResp, secStatus, err := zendeskDo(auth, subdomain, http.MethodGet, "/webhooks/"+webhookID+"/signing_secret", nil)
	signingSecret := ""
	if secObj, ok := secResp["signing_secret"].(map[string]interface{}); ok {
		signingSecret, _ = secObj["secret"].(string)
	}
	if err != nil || secStatus >= 300 || signingSecret == "" {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": secStatus, "error": err}).Warn("zendesk trigger: could not fetch signing secret; removing orphaned webhook")
		_, _, _ = zendeskDo(auth, subdomain, http.MethodDelete, "/webhooks/"+webhookID, nil)
		return
	}

	// 3. Create the business-rule trigger that fires the webhook.
	triggerBody := map[string]interface{}{
		"trigger": map[string]interface{}{
			"title":      "Flomation webhook: " + tr.ID,
			"conditions": zendeskConditions(conditions),
			"actions": []map[string]interface{}{
				{"field": "notification_webhook", "value": []interface{}{webhookID, zendeskWebhookMessage}},
			},
		},
	}
	trResp, trStatus, err := zendeskDo(auth, subdomain, http.MethodPost, "/triggers.json", triggerBody)
	if err != nil || trStatus >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": trStatus, "error": err, "response": trResp}).Warn("zendesk trigger: could not create business rule; removing orphaned webhook")
		_, _, _ = zendeskDo(auth, subdomain, http.MethodDelete, "/webhooks/"+webhookID, nil)
		return
	}
	ztrigger, _ := trResp["trigger"].(map[string]interface{})
	zendeskTriggerID := numToID(ztrigger["id"])
	if zendeskTriggerID == "" {
		log.WithField("trigger_id", tr.ID).Warn("zendesk trigger: business rule created but no id returned; removing orphaned webhook")
		_, _, _ = zendeskDo(auth, subdomain, http.MethodDelete, "/webhooks/"+webhookID, nil)
		return
	}

	stateJSON, _ := json.Marshal(zendeskWebhookState{
		WebhookID:     webhookID,
		TriggerID:     zendeskTriggerID,
		SigningSecret: signingSecret,
		Conditions:    conditions,
		Subdomain:     subdomain,
	})
	if err := s.db.UpsertTriggerState(tr.ID, zendeskStateKey, stateJSON); err != nil {
		// Without the signing secret every delivery would be rejected; remove the
		// orphaned pair so the next save retries cleanly.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("zendesk trigger: unable to persist state; removing webhook + trigger")
		_, _, _ = zendeskDo(auth, subdomain, http.MethodDelete, "/triggers/"+zendeskTriggerID+".json", nil)
		_, _, _ = zendeskDo(auth, subdomain, http.MethodDelete, "/webhooks/"+webhookID, nil)
	}
}

// deregisterZendeskWebhook removes the webhook + trigger we registered for this
// trigger (best effort; logged, never fatal).
func (s *Service) deregisterZendeskWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeZendeskWebhook {
		return
	}
	state := s.loadZendeskState(tr.ID)
	if state == nil {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	auth := zendeskwh.AuthHeader(creds["email"], creds["api_token"], creds["oauth_token"])
	if auth != "" {
		s.deleteZendeskRemote(auth, state.Subdomain, state)
	}
	if err := s.db.DeleteTriggerState(tr.ID, zendeskStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("zendesk trigger: unable to delete state")
	}
}

// deleteZendeskRemote deletes the business-rule trigger then the webhook
// connector (order matters: a webhook still referenced by a trigger can't be
// removed). Both are best-effort; a 404 means it's already gone.
func (s *Service) deleteZendeskRemote(auth, subdomain string, state *zendeskWebhookState) {
	if state.TriggerID != "" {
		if _, status, err := zendeskDo(auth, subdomain, http.MethodDelete, "/triggers/"+state.TriggerID+".json", nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
			log.WithFields(log.Fields{"status": status, "error": err}).Warn("zendesk trigger: could not delete business rule")
		}
	}
	if state.WebhookID != "" {
		if _, status, err := zendeskDo(auth, subdomain, http.MethodDelete, "/webhooks/"+state.WebhookID, nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
			log.WithFields(log.Fields{"status": status, "error": err}).Warn("zendesk trigger: could not delete webhook")
		}
	}
}

// handleZendeskWebhook handles an inbound Zendesk webhook for a trigger. Zendesk
// signs the delivery (base64 HMAC-SHA256 of timestamp+body) with the signing
// secret we fetched at registration time, so the raw body is read verbatim and
// verified before any parsing. Called from handleWebhook after the trigger has
// been fetched and type-checked.
func (s *Service) handleZendeskWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	// Zendesk's rendered webhook message is tiny (~200 bytes); cap the read
	// well below that ceiling so an oversized POST can't balloon memory.
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	state := s.loadZendeskState(id)
	if state == nil || state.SigningSecret == "" {
		log.WithFields(log.Fields{"id": id}).Warn("Zendesk webhook has no signing secret on record")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if err := zendeskwh.VerifySignature(state.SigningSecret, body, c.Request); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("Zendesk webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data, err := zendeskwh.ParseEvent(body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("Zendesk webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Carry __node_id so the executor injects event data into the correct
	// trigger node in multi-trigger flows.
	var triggerData map[string]string
	_ = json.Unmarshal(tr.Data, &triggerData)
	if nodeID := triggerData["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Zendesk webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}

// numToID renders a Zendesk numeric ID (which JSON-decodes to float64) as a
// clean integer string, leaving string IDs untouched.
func numToID(v interface{}) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return strconv.FormatInt(int64(n), 10)
	default:
		return ""
	}
}
