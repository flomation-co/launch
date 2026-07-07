package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 — Trello mandates HMAC-SHA1 for webhook signatures; not our choice
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flomation.app/automate/launch"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// trelloAPIBase is the fixed Trello REST root used to register/deregister
// webhooks. Trello is a single public host (api.trello.com), never
// caller-supplied, so — unlike the Jira/WooCommerce trigger clients — there is
// NO SSRF surface here and the HTTP client needs no dial Control / metadata-IP
// guard. Kept in sync with the executor's trello_common.APIBase and the api's
// trelloAPIBase.
const trelloAPIBase = "https://api.trello.com/1"

// trelloHTTPClient issues the outbound Trello REST calls. The host is a fixed
// public endpoint, so a plain client with a timeout is sufficient; cross-host
// redirects are still refused defensively.
var trelloHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after too many redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("cross-host redirect not allowed")
		}
		return nil
	},
}

// trelloStateKey is the trigger_state key holding the webhook created for a
// trello-webhook trigger.
const trelloStateKey = "trello_webhook"

// trelloWebhookState is what we persist per trigger: the Trello webhook id (so
// it can be deleted), the model it watches and the callback + secret it was
// created with (so a config change is detected and the webhook recreated).
type trelloWebhookState struct {
	ID       string `json:"id"`
	ModelID  string `json:"model_id"`
	Callback string `json:"callback"`
	// Secret is the Trello API secret registered for signature verification.
	// Persisted so the inbound handler can verify without re-resolving creds.
	Secret string `json:"secret,omitempty"`
}

// trelloCreds is the resolved connection for a Trello trigger.
type trelloCreds struct {
	key     string
	token   string
	modelID string
	secret  string // optional Trello API secret (HMAC-SHA1 signature verification)
}

// resolveTrelloCreds pulls the trigger's Trello connection. ok is false when a
// required part (key, token, model id) is missing.
func (s *Service) resolveTrelloCreds(tr *launch.Trigger) (trelloCreds, bool) {
	c := s.resolveTriggerCreds(tr.ID)
	key := strings.TrimSpace(c["api_key"])
	token := strings.TrimSpace(c["api_token"])
	modelID := strings.TrimSpace(c["model_id"])
	if key == "" || token == "" || modelID == "" {
		return trelloCreds{}, false
	}
	return trelloCreds{
		key:     key,
		token:   token,
		modelID: modelID,
		secret:  strings.TrimSpace(c["secret"]),
	}, true
}

// trelloDo performs an authenticated Trello REST request. path is the API path
// under trelloAPIBase (e.g. "/webhooks" or "/webhooks/{id}"); params carries any
// operation parameters — the key and token are appended here. There is no
// request body: Trello reads parameters from the query string.
func trelloDo(ctx context.Context, cr trelloCreds, method, path string, params url.Values) (map[string]interface{}, int, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("key", cr.key)
	params.Set("token", cr.token)
	fullURL := trelloAPIBase + path + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := trelloHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]interface{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out) // best-effort; Trello errors are often plain text
	}
	// Log the raw body on any error status. Trello returns validation failures as
	// a short plain-text body (e.g. "A webhook with that callback, model, and
	// token already exists"), which json.Unmarshal drops on the floor — without
	// this, a failed registration only surfaces "response: <nil>" at the call
	// site, hiding the actual reason. Bounded to 512 chars.
	if resp.StatusCode >= 300 && len(raw) > 0 {
		b := string(raw)
		if len(b) > 512 {
			b = b[:512]
		}
		log.WithFields(log.Fields{"status": resp.StatusCode, "body": b}).Warn("trello: API error response")
	}
	return out, resp.StatusCode, nil
}

// loadTrelloState reads the persisted webhook for a trigger. A missing row
// yields a nil state, not an error.
func (s *Service) loadTrelloState(triggerID string) *trelloWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("trello trigger: unable to load state")
		return nil
	}
	raw, ok := rows[trelloStateKey]
	if !ok {
		return nil
	}
	var st trelloWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// deleteTrelloWebhook removes the given webhook from Trello (best effort). A 404
// is treated as already-gone.
func deleteTrelloWebhook(ctx context.Context, cr trelloCreds, id string) {
	if id == "" {
		return
	}
	if _, status, err := trelloDo(ctx, cr, http.MethodDelete, "/webhooks/"+url.PathEscape(id), nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
		log.WithFields(log.Fields{"webhook_id": id, "status": status, "error": err}).Warn("trello trigger: failed to delete webhook")
	}
}

// registerTrelloWebhook auto-registers one Trello webhook watching the selected
// model, pointing at {PublicURL}/webhook/{trigger_id}. Idempotent — createTrigger
// runs on every flow save, so an unchanged model + callback + secret is left
// untouched; a changed one recreates the webhook. Errors are logged, never fatal.
//
// NOTE: Trello validates the callbackURL at registration by sending it a HEAD
// request that must return 200 — see handleWebhookHead. If PublicURL is not
// reachable from Trello, registration fails (logged, non-fatal).
func (s *Service) registerTrelloWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeTrelloWebhook {
		return
	}
	cr, ok := s.resolveTrelloCreds(tr)
	if !ok {
		log.WithField("trigger_id", tr.ID).Warn("trello trigger: missing api key / token / model id; skipping webhook registration")
		return
	}
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since the last save: keep the existing webhook. Checked
	// before any REST call — createTrigger runs on every flow save and the common
	// case must not cost a round-trip. The plain `==` compares values we own
	// (persisted state vs freshly resolved config), not untrusted input, so no
	// constant-time compare is needed here (unlike the signature check in
	// handleTrelloWebhook).
	state := s.loadTrelloState(tr.ID)
	if state != nil && state.ID != "" && state.ModelID == cr.modelID && state.Callback == callback && state.Secret == cr.secret {
		return
	}

	// The config changed (or stored state is unusable): remove the old webhook
	// before creating a replacement. The callback is keyed on the stable trigger
	// ID, so a stale webhook would double-fire the flow.
	if state != nil && state.ID != "" {
		deleteTrelloWebhook(ctx, cr, state.ID)
	}

	params := url.Values{}
	params.Set("idModel", cr.modelID)
	params.Set("callbackURL", callback)
	params.Set("description", "Flomation trigger "+tr.ID)
	resp, status, err := trelloDo(ctx, cr, http.MethodPost, "/webhooks", params)
	if err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err, "response": resp}).Warn("failed to register Trello webhook")
		return
	}
	id := asString(resp["id"])
	if id == "" {
		log.WithField("trigger_id", tr.ID).Warn("trello trigger: webhook created but no id returned")
		return
	}

	stateJSON, _ := json.Marshal(trelloWebhookState{ID: id, ModelID: cr.modelID, Callback: callback, Secret: cr.secret})
	if err := s.db.UpsertTriggerState(tr.ID, trelloStateKey, stateJSON); err != nil {
		// Without persisted state we can never deregister; remove the
		// just-created webhook so the next save retries cleanly.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("trello trigger: unable to persist state; removing webhook")
		deleteTrelloWebhook(ctx, cr, id)
	}
}

// deregisterTrelloWebhook removes the webhook we registered for this trigger
// (best effort; logged, never fatal).
func (s *Service) deregisterTrelloWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeTrelloWebhook {
		return
	}
	state := s.loadTrelloState(tr.ID)
	if state == nil || state.ID == "" {
		return
	}
	cr, ok := s.resolveTrelloCreds(tr)
	if !ok {
		return
	}
	deleteTrelloWebhook(ctx, cr, state.ID)
	if err := s.db.DeleteTriggerState(tr.ID, trelloStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("trello trigger: unable to delete state")
	}
}

// handleTrelloWebhook handles an inbound Trello webhook for a trigger.
//
// Signature verification: Trello signs each delivery with the header
// "X-Trello-Webhook: <base64(HMAC-SHA1(secret, body + callbackURL))>", where
// secret is the application's API secret. When the trigger stored a secret, a
// delivery with a missing or mismatched signature is rejected (401). When no
// secret is set the endpoint accepts the delivery and security rests on the
// unguessable trigger-id in the callback URL (consistent with the other webhook
// triggers, but the secret is recommended).
//
// Trello also issues a HEAD request to the callback at registration time — that
// is handled separately by handleWebhookHead (a 200), never reaching here.
func (s *Service) handleTrelloWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	state := s.loadTrelloState(id)

	// Resolve the signing secret + callback. Prefer the trigger's current config
	// (works whether the webhook was auto-registered by launch or created
	// manually on Trello), falling back to the persisted state.
	secret := ""
	callback := ""
	if cr, ok := s.resolveTrelloCreds(tr); ok {
		secret = cr.secret
	}
	if state != nil {
		if secret == "" {
			secret = state.Secret
		}
		callback = state.Callback
	}
	if callback == "" {
		callback = fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, id)
	}
	if secret != "" {
		if !trelloVerifySignature(body, c.GetHeader("X-Trello-Webhook"), secret, callback) {
			log.WithFields(log.Fields{"id": id}).Warn("Trello webhook signature verification failed")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}

	var data map[string]interface{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &data); err != nil {
			log.WithFields(log.Fields{"id": id, "error": err}).Error("Trello webhook parse failed")
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
	}
	if data == nil {
		data = map[string]interface{}{}
	}

	// Flatten the nested Trello payload ({action:{...}, model:{...}}) into the
	// trigger node's declared outputs. The action carries the event type and the
	// board/card/list ids in its `data`.
	out := map[string]interface{}{"body": string(body)}
	if model, ok := data["model"].(map[string]interface{}); ok {
		out["model_id"] = asString(model["id"])
	}
	if action, ok := data["action"].(map[string]interface{}); ok {
		out["action_type"] = asString(action["type"])
		out["action_id"] = asString(action["id"])
		out["date"] = asString(action["date"])
		if mc, ok := action["memberCreator"].(map[string]interface{}); ok {
			if name := asString(mc["fullName"]); name != "" {
				out["member"] = name
			} else {
				out["member"] = asString(mc["username"])
			}
		}
		if ad, ok := action["data"].(map[string]interface{}); ok {
			if b, ok := ad["board"].(map[string]interface{}); ok {
				out["board_id"] = asString(b["id"])
			}
			if cd, ok := ad["card"].(map[string]interface{}); ok {
				out["card_id"] = asString(cd["id"])
			}
			if l, ok := ad["list"].(map[string]interface{}); ok {
				out["list_id"] = asString(l["id"])
			}
		}
	}

	var triggerData map[string]interface{}
	_ = json.Unmarshal(tr.Data, &triggerData)
	// Carry __node_id so the executor injects event data into the correct trigger
	// node in multi-trigger flows.
	if nodeID := asString(triggerData["__node_id"]); nodeID != "" {
		out["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, out); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Trello webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}

// handleWebhookHead answers a HEAD probe against /webhook/:id with 200. Trello
// (and some other providers) verify a callback URL exists this way before
// creating a webhook; a non-200 aborts the registration. It deliberately does no
// trigger lookup or body work — the probe only needs the endpoint to be alive.
func (s *Service) handleWebhookHead(c *gin.Context) {
	c.Status(http.StatusOK)
}

// trelloVerifySignature checks a Trello "X-Trello-Webhook" header against
// base64(HMAC-SHA1(secret, body + callbackURL)), using a constant-time
// comparison. A missing/malformed header fails closed. Trello mandates SHA-1
// here — it is not a security choice of ours (see the #nosec on the import).
func trelloVerifySignature(body []byte, header, secret, callback string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	mac.Write([]byte(callback))
	return hmac.Equal(want, mac.Sum(nil))
}
