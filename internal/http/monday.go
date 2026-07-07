package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"flomation.app/automate/launch"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// mondayAPIBase is the fixed Monday.com GraphQL endpoint used to register/
// deregister webhooks. The host is a constant (never caller-supplied), so there
// is NO SSRF surface. Kept in sync with the executor + api.
const mondayAPIBase = "https://api.monday.com/v2"
const mondayAPIVersion = "2023-10"

var mondayHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
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

const mondayStateKey = "monday_webhook"

// mondayWebhookState is what we persist per trigger: the Monday webhook id (so
// it can be deleted) and the board/event it watches (so a config change is
// detected and the webhook recreated). Monday webhooks are UNSIGNED, so there is
// no secret to store — security rests on the unguessable trigger-id callback URL
// (the same model as the Trello/Jira triggers without a signing secret).
type mondayWebhookState struct {
	WebhookID string `json:"webhook_id"`
	BoardID   string `json:"board_id"`
	Event     string `json:"event"`
}

// mondayCreds is the resolved connection for a Monday trigger.
type mondayCreds struct {
	token   string
	boardID string
	event   string
}

func (s *Service) resolveMondayCreds(tr *launch.Trigger) (mondayCreds, bool) {
	c := s.resolveTriggerCreds(tr.ID)
	token := strings.TrimSpace(c["api_token"])
	boardID := strings.TrimSpace(c["board_id"])
	event := strings.TrimSpace(c["event"])
	if token == "" || boardID == "" || event == "" {
		return mondayCreds{}, false
	}
	return mondayCreds{token: token, boardID: boardID, event: event}, true
}

// mondayGraphQL POSTs a GraphQL query/mutation with the Bearer token and returns
// the decoded `data` object. A GraphQL error (200 + errors array) becomes a Go
// error.
func mondayGraphQL(ctx context.Context, token, query string, variables map[string]interface{}) (map[string]interface{}, error) {
	payload := map[string]interface{}{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mondayAPIBase, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("API-Version", mondayAPIVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := mondayHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("monday: failed to read response: %w", err)
	}
	var env struct {
		Data   map[string]interface{} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("monday: unparseable response (status %d)", resp.StatusCode)
	}
	if len(env.Errors) > 0 {
		return nil, fmt.Errorf("monday GraphQL error: %s", env.Errors[0].Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("monday API error (%d)", resp.StatusCode)
	}
	return env.Data, nil
}

func (s *Service) loadMondayState(triggerID string) *mondayWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("monday trigger: unable to load state")
		return nil
	}
	raw, ok := rows[mondayStateKey]
	if !ok {
		return nil
	}
	var st mondayWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

func deleteMondayWebhook(ctx context.Context, token, webhookID string) {
	if webhookID == "" {
		return
	}
	if _, err := mondayGraphQL(ctx, token, `mutation ($id: ID!) { delete_webhook (id: $id) { id } }`,
		map[string]interface{}{"id": webhookID}); err != nil {
		log.WithFields(log.Fields{"webhook_id": webhookID, "error": err}).Warn("monday trigger: failed to delete webhook")
	}
}

// registerMondayWebhook auto-registers one Monday webhook for the selected board
// + event, pointing at {PublicURL}/webhook/{trigger_id}. Idempotent — an
// unchanged board+event is left untouched; a changed one recreates it.
//
// HANDSHAKE: the create_webhook mutation makes Monday POST the callback with a
// {"challenge": "..."} body that handleMondayWebhook must echo back with a 200,
// which is why that handler checks for a challenge first. Errors are logged,
// never fatal.
func (s *Service) registerMondayWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeMondayWebhook {
		return
	}
	cr, ok := s.resolveMondayCreds(tr)
	if !ok {
		log.WithField("trigger_id", tr.ID).Warn("monday trigger: missing token / board / event; skipping webhook registration")
		return
	}
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	state := s.loadMondayState(tr.ID)
	if state != nil && state.WebhookID != "" && state.BoardID == cr.boardID && state.Event == cr.event {
		return
	}
	if state != nil && state.WebhookID != "" {
		deleteMondayWebhook(ctx, cr.token, state.WebhookID)
	}

	data, err := mondayGraphQL(ctx, cr.token, `mutation ($boardId: ID!, $url: String!, $event: WebhookEventType!) {
		create_webhook (board_id: $boardId, url: $url, event: $event) { id board_id }
	}`, map[string]interface{}{"boardId": cr.boardID, "url": callback, "event": cr.event})
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("failed to register Monday webhook")
		return
	}
	webhookID := ""
	if wh, ok := data["create_webhook"].(map[string]interface{}); ok {
		webhookID = asString(wh["id"])
	}
	if webhookID == "" {
		log.WithField("trigger_id", tr.ID).Warn("monday trigger: webhook created but no id returned")
		return
	}

	stateJSON, _ := json.Marshal(mondayWebhookState{WebhookID: webhookID, BoardID: cr.boardID, Event: cr.event})
	if err := s.db.UpsertTriggerState(tr.ID, mondayStateKey, stateJSON); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("monday trigger: unable to persist state; removing webhook")
		deleteMondayWebhook(ctx, cr.token, webhookID)
	}
}

func (s *Service) deregisterMondayWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeMondayWebhook {
		return
	}
	state := s.loadMondayState(tr.ID)
	if state == nil || state.WebhookID == "" {
		return
	}
	cr, ok := s.resolveMondayCreds(tr)
	if !ok {
		return
	}
	deleteMondayWebhook(ctx, cr.token, state.WebhookID)
	if err := s.db.DeleteTriggerState(tr.ID, mondayStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("monday trigger: unable to delete state")
	}
}

// handleMondayWebhook handles an inbound Monday webhook for a trigger. It serves
// two roles:
//
//  1. HANDSHAKE — at registration Monday POSTs {"challenge": "<token>"} and
//     expects the same challenge echoed back in the response body with a 200.
//     This is detected first and answered without firing the flow.
//
//  2. DELIVERY — every later POST is {"event": {...}}. Monday personal-token
//     webhooks are UNSIGNED, so there is nothing to verify; security rests on the
//     unguessable trigger-id in the callback URL. The event is flattened into the
//     trigger node's outputs and the flow fires.
func (s *Service) handleMondayWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var payload map[string]interface{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			log.WithFields(log.Fields{"id": id, "error": err}).Error("Monday webhook parse failed")
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
	}

	// --- Handshake ---
	if challenge, ok := payload["challenge"]; ok {
		c.JSON(http.StatusOK, gin.H{"challenge": challenge})
		return
	}

	// --- Delivery ---
	// Monday's event ids (boardId, pulseId, userId) arrive as JSON NUMBERS, not
	// strings, so asString would drop them — use mondayEventStr which stringifies
	// integers cleanly.
	out := map[string]interface{}{"body": string(body)}
	if event, ok := payload["event"].(map[string]interface{}); ok {
		out["event_type"] = mondayEventStr(event["type"])
		out["board_id"] = mondayEventStr(event["boardId"])
		out["item_id"] = mondayEventStr(event["pulseId"])
		out["column_id"] = mondayEventStr(event["columnId"])
		out["user_id"] = mondayEventStr(event["userId"])
	}

	var triggerData map[string]interface{}
	_ = json.Unmarshal(tr.Data, &triggerData)
	if nodeID := asString(triggerData["__node_id"]); nodeID != "" {
		out["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, out); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Monday webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}

// mondayEventStr stringifies a Monday event field. Ids come through as JSON
// numbers (float64); integers are rendered without a decimal point. Strings pass
// through; anything else yields "".
func mondayEventStr(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}
