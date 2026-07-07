package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// asanaAPIBase is the fixed Asana REST root used to register/deregister
// webhooks. Asana is a single public host (app.asana.com), never
// caller-supplied, so there is NO SSRF surface and the client needs no dial
// Control / metadata-IP guard. Kept in sync with the executor + api.
const asanaAPIBase = "https://app.asana.com/api/1.0"

// asanaHTTPClient issues the outbound Asana REST calls. The register call blocks
// until Asana completes its handshake against our callback (see
// registerAsanaWebhook), so the timeout is generous.
var asanaHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
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

// asanaStateKey is the trigger_state key holding the webhook for an
// asana-webhook trigger.
const asanaStateKey = "asana_webhook"

// asanaWebhookState is what we persist per trigger. Note the two-phase lifecycle:
// the Secret is written first, by the inbound HANDSHAKE (which arrives before the
// POST /webhooks call returns and so before the GID is known); the GID/Resource/
// Target are written afterwards, once registration completes.
type asanaWebhookState struct {
	GID      string `json:"gid"`
	Resource string `json:"resource"`
	Target   string `json:"target"`
	// Secret is the X-Hook-Secret Asana generated during the handshake. Every
	// later delivery is signed with HMAC-SHA256(secret, body) in X-Hook-Signature.
	Secret string `json:"secret,omitempty"`
}

// asanaCreds is the resolved connection for an Asana trigger.
type asanaCreds struct {
	token    string
	resource string
}

// resolveAsanaCreds pulls the trigger's Asana connection. ok is false when a
// required part (token, resource) is missing.
func (s *Service) resolveAsanaCreds(tr *launch.Trigger) (asanaCreds, bool) {
	c := s.resolveTriggerCreds(tr.ID)
	token := strings.TrimSpace(c["access_token"])
	resource := strings.TrimSpace(c["resource"])
	if token == "" || resource == "" {
		return asanaCreds{}, false
	}
	return asanaCreds{token: token, resource: resource}, true
}

// asanaDo performs an authenticated Asana REST request. bodyData, when non-nil,
// is wrapped in Asana's {"data": ...} envelope.
func asanaDo(ctx context.Context, token, method, path string, bodyData map[string]interface{}) (map[string]interface{}, int, error) {
	var rdr io.Reader
	if bodyData != nil {
		b, err := json.Marshal(map[string]interface{}{"data": bodyData})
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, asanaAPIBase+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := asanaHTTPClient.Do(req)
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
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": b}).Warn("asana: non-JSON response body")
		}
	}
	if resp.StatusCode >= 300 && len(raw) > 0 {
		b := string(raw)
		if len(b) > 512 {
			b = b[:512]
		}
		log.WithFields(log.Fields{"status": resp.StatusCode, "body": b}).Warn("asana: API error response")
	}
	return out, resp.StatusCode, nil
}

// loadAsanaState reads the persisted webhook for a trigger. A missing row yields
// a nil state, not an error.
func (s *Service) loadAsanaState(triggerID string) *asanaWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("asana trigger: unable to load state")
		return nil
	}
	raw, ok := rows[asanaStateKey]
	if !ok {
		return nil
	}
	var st asanaWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// deleteAsanaWebhook removes the given webhook from Asana (best effort). A 404 is
// treated as already-gone.
func deleteAsanaWebhook(ctx context.Context, token, gid string) {
	if gid == "" {
		return
	}
	if _, status, err := asanaDo(ctx, token, http.MethodDelete, "/webhooks/"+url.PathEscape(gid), nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
		log.WithFields(log.Fields{"gid": gid, "status": status, "error": err}).Warn("asana trigger: failed to delete webhook")
	}
}

// registerAsanaWebhook auto-registers one Asana webhook watching the selected
// resource, pointing at {PublicURL}/webhook/{trigger_id}. Idempotent — an
// unchanged resource + target is left untouched; a changed one recreates it.
//
// HANDSHAKE: POST /webhooks blocks while Asana sends a POST to our callback with
// an X-Hook-Secret header that handleAsanaWebhook must echo back within seconds.
// That handler persists the secret to this trigger's state BEFORE the POST call
// returns, so once we have the gid we re-load the state (now carrying the secret)
// and store gid+resource+target alongside it. Errors are logged, never fatal.
func (s *Service) registerAsanaWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeAsanaWebhook {
		return
	}
	cr, ok := s.resolveAsanaCreds(tr)
	if !ok {
		log.WithField("trigger_id", tr.ID).Warn("asana trigger: missing access token / resource; skipping webhook registration")
		return
	}
	target := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since the last save: keep the existing webhook. Checked
	// before any REST call — createTrigger runs on every flow save.
	state := s.loadAsanaState(tr.ID)
	if state != nil && state.GID != "" && state.Resource == cr.resource && state.Target == target {
		return
	}
	// Config changed (or stored state is unusable): remove the old webhook first.
	if state != nil && state.GID != "" {
		deleteAsanaWebhook(ctx, cr.token, state.GID)
	}

	// POST /webhooks triggers Asana's synchronous handshake against our callback.
	// handleAsanaWebhook stores the X-Hook-Secret under this trigger's state and
	// echoes it; only then does this call return with the webhook gid.
	resp, status, err := asanaDo(ctx, cr.token, http.MethodPost, "/webhooks", map[string]interface{}{
		"resource": cr.resource,
		"target":   target,
	})
	if err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err, "response": resp}).Warn("failed to register Asana webhook")
		return
	}
	gid := ""
	if data, ok := resp["data"].(map[string]interface{}); ok {
		gid = asString(data["gid"])
	}
	if gid == "" {
		log.WithField("trigger_id", tr.ID).Warn("asana trigger: webhook created but no gid returned")
		return
	}

	// Re-load the state written by the handshake so we keep the secret, then add
	// the gid/resource/target.
	secret := ""
	if st := s.loadAsanaState(tr.ID); st != nil {
		secret = st.Secret
	}
	stateJSON, _ := json.Marshal(asanaWebhookState{GID: gid, Resource: cr.resource, Target: target, Secret: secret})
	if err := s.db.UpsertTriggerState(tr.ID, asanaStateKey, stateJSON); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("asana trigger: unable to persist state; removing webhook")
		deleteAsanaWebhook(ctx, cr.token, gid)
	}
}

// deregisterAsanaWebhook removes the webhook we registered for this trigger.
func (s *Service) deregisterAsanaWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeAsanaWebhook {
		return
	}
	state := s.loadAsanaState(tr.ID)
	if state == nil || state.GID == "" {
		return
	}
	cr, ok := s.resolveAsanaCreds(tr)
	if !ok {
		return
	}
	deleteAsanaWebhook(ctx, cr.token, state.GID)
	if err := s.db.DeleteTriggerState(tr.ID, asanaStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("asana trigger: unable to delete state")
	}
}

// handleAsanaWebhook handles an inbound Asana webhook for a trigger. It serves
// two roles:
//
//  1. HANDSHAKE — when Asana registers a webhook it immediately POSTs the
//     callback with an "X-Hook-Secret" header (and no events). We must persist
//     that secret and echo it back in the response "X-Hook-Secret" header with a
//     200, within seconds, or registration fails. The secret is stored keyed on
//     the trigger id (from the URL), which is known even though the webhook gid
//     is not yet.
//
//  2. DELIVERY — every later POST carries "X-Hook-Signature" (hex HMAC-SHA256 of
//     the raw body, keyed by the secret) and a {"events":[...]} body. The
//     signature is verified (constant-time, fail-closed) before the flow fires.
func (s *Service) handleAsanaWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// --- Handshake ---
	if hookSecret := c.GetHeader("X-Hook-Secret"); hookSecret != "" {
		// Persist the secret BEFORE responding: registerAsanaWebhook re-reads this
		// state after its POST /webhooks returns, and Asana only returns to that
		// call after it receives our echoed 200 — so the DB write must be visible
		// by then. Merge with any existing state to avoid clobbering a gid.
		st := s.loadAsanaState(id)
		if st == nil {
			st = &asanaWebhookState{}
		}
		st.Secret = hookSecret
		if stateJSON, merr := json.Marshal(st); merr == nil {
			if uerr := s.db.UpsertTriggerState(id, asanaStateKey, stateJSON); uerr != nil {
				log.WithFields(log.Fields{"id": id, "error": uerr}).Error("asana trigger: failed to persist handshake secret")
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}
		}
		c.Header("X-Hook-Secret", hookSecret)
		c.Status(http.StatusOK)
		return
	}

	// --- Delivery ---
	state := s.loadAsanaState(id)
	secret := ""
	if state != nil {
		secret = state.Secret
	}
	if secret != "" {
		if !asanaVerifySignature(body, c.GetHeader("X-Hook-Signature"), secret) {
			log.WithFields(log.Fields{"id": id}).Warn("Asana webhook signature verification failed")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}

	var payload struct {
		Events []map[string]interface{} `json:"events"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			log.WithFields(log.Fields{"id": id, "error": err}).Error("Asana webhook parse failed")
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
	}
	// A delivery may batch several events; surface the first one's summary fields
	// and hand the full raw body to the flow for power users.
	out := map[string]interface{}{"body": string(body)}
	if len(payload.Events) > 0 {
		ev := payload.Events[0]
		out["action"] = asString(ev["action"])
		out["created_at"] = asString(ev["created_at"])
		out["user"] = asString(ev["user"])
		if res, ok := ev["resource"].(map[string]interface{}); ok {
			out["resource_id"] = asString(res["gid"])
			out["resource_type"] = asString(res["resource_type"])
		}
	}

	var triggerData map[string]interface{}
	_ = json.Unmarshal(tr.Data, &triggerData)
	if nodeID := asString(triggerData["__node_id"]); nodeID != "" {
		out["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, out); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Asana webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}

// asanaVerifySignature checks an Asana "X-Hook-Signature" header (hex-encoded
// HMAC-SHA256 of the raw body keyed by the hook secret) using a constant-time
// comparison. A missing/malformed header fails closed.
func asanaVerifySignature(body []byte, header, secret string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	want, err := hex.DecodeString(header)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}
