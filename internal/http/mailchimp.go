package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flomation.app/automate/launch"

	log "github.com/sirupsen/logrus"

	"github.com/gin-gonic/gin"
)

// mailchimpHTTPClient is used for the outbound Mailchimp Marketing API calls
// that register/deregister audience webhooks.
var mailchimpHTTPClient = &http.Client{Timeout: 15 * time.Second}

var (
	mailchimpAllEvents  = []string{"subscribe", "unsubscribe", "profile", "cleaned", "upemail", "campaign"}
	mailchimpAllSources = []string{"user", "admin", "api"}
)

// mailchimpDatacenter extracts the datacenter suffix from a Mailchimp API key
// (the segment after the final '-', e.g. "...-us6" -> "us6").
func mailchimpDatacenter(apiKey string) (string, error) {
	i := strings.LastIndex(apiKey, "-")
	if i < 0 || i == len(apiKey)-1 {
		return "", fmt.Errorf("invalid Mailchimp API key: missing datacenter suffix")
	}
	return apiKey[i+1:], nil
}

// mailchimpBaseURL builds the datacenter-scoped Marketing API root for a key.
func mailchimpBaseURL(apiKey string) (string, error) {
	dc, err := mailchimpDatacenter(apiKey)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s.api.mailchimp.com/3.0", dc), nil
}

// mailchimpDo performs an authenticated Mailchimp Marketing API request and
// returns the decoded JSON body and status code.
func mailchimpDo(apiKey, method, fullURL string, body interface{}) (map[string]interface{}, int, error) {
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
	req.Header.Set("Authorization", "apikey "+apiKey)
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := mailchimpHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]interface{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out, resp.StatusCode, nil
}

func mailchimpSplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// mailchimpSelMap turns a comma-separated selection into Mailchimp's
// {name:true} map; an empty selection enables all of the defaults.
func mailchimpSelMap(csv string, all []string) map[string]bool {
	sel := mailchimpSplitCSV(csv)
	m := map[string]bool{}
	if len(sel) == 0 {
		for _, e := range all {
			m[e] = true
		}
		return m
	}
	for _, e := range sel {
		m[e] = true
	}
	return m
}

func mailchimpCSVContains(csv, val string) bool {
	for _, e := range mailchimpSplitCSV(csv) {
		if e == val {
			return true
		}
	}
	return false
}

// registerMailchimpWebhook auto-registers a webhook on the trigger's Mailchimp
// audience, pointing at {PublicURL}/webhook/{trigger_id}. It is idempotent —
// createTrigger runs on every flow save, so an existing webhook for our
// callback URL is left untouched. Errors are logged, never fatal (they must
// not fail the trigger upsert).
func (s *Service) registerMailchimpWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeMailchimpWebhook {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	apiKey, listID := creds["api_key"], creds["list_id"]
	if apiKey == "" || listID == "" {
		log.WithField("trigger_id", tr.ID).Warn("mailchimp trigger: missing api_key/list_id; skipping webhook registration")
		return
	}
	base, err := mailchimpBaseURL(apiKey)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("mailchimp trigger: invalid API key; skipping webhook registration")
		return
	}
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)
	listURL := fmt.Sprintf("%s/lists/%s/webhooks", base, url.PathEscape(listID))

	// List existing webhooks (high page size) to stay idempotent — createTrigger
	// fires on every flow save. If we can't list them, skip creating this pass
	// rather than risk a duplicate; the next save retries.
	existing, status, err := mailchimpDo(apiKey, http.MethodGet, listURL+"?count=1000", nil)
	if err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err}).Warn("mailchimp trigger: could not list existing webhooks; skipping registration this pass")
		return
	}
	if mailchimpWebhookExists(existing, callback) {
		return
	}

	body := map[string]interface{}{
		"url":     callback,
		"events":  mailchimpSelMap(creds["events"], mailchimpAllEvents),
		"sources": mailchimpSelMap(creds["sources"], mailchimpAllSources),
	}
	if _, status, err := mailchimpDo(apiKey, http.MethodPost, listURL, body); err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err}).Warn("failed to register Mailchimp webhook")
	}
}

// deregisterMailchimpWebhook removes the webhook we registered for this trigger
// (best effort; logged, never fatal).
func (s *Service) deregisterMailchimpWebhook(tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeMailchimpWebhook {
		return
	}
	creds := s.resolveTriggerCreds(tr.ID)
	apiKey, listID := creds["api_key"], creds["list_id"]
	if apiKey == "" || listID == "" {
		return
	}
	base, err := mailchimpBaseURL(apiKey)
	if err != nil {
		return
	}
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)
	listURL := fmt.Sprintf("%s/lists/%s/webhooks", base, url.PathEscape(listID))
	existing, _, err := mailchimpDo(apiKey, http.MethodGet, listURL, nil)
	if err != nil {
		return
	}
	hooks, _ := existing["webhooks"].([]interface{})
	for _, h := range hooks {
		hm, ok := h.(map[string]interface{})
		if !ok {
			continue
		}
		if u, _ := hm["url"].(string); u == callback {
			if id, _ := hm["id"].(string); id != "" {
				_, _, _ = mailchimpDo(apiKey, http.MethodDelete, listURL+"/"+url.PathEscape(id), nil)
			}
		}
	}
}

func mailchimpWebhookExists(resp map[string]interface{}, callback string) bool {
	hooks, _ := resp["webhooks"].([]interface{})
	for _, h := range hooks {
		if hm, ok := h.(map[string]interface{}); ok {
			if u, _ := hm["url"].(string); u == callback {
				return true
			}
		}
	}
	return false
}

// handleMailchimpWebhook handles inbound Mailchimp audience webhooks. Mailchimp
// validates a new webhook by GETting the URL (ack with 200), then POSTs
// form-encoded event payloads. Called from handleWebhook after the trigger has
// been fetched and type-checked.
func (s *Service) handleMailchimpWebhook(c *gin.Context, tr *launch.Trigger) {
	// Validation handshake: Mailchimp GETs the URL when the webhook is created.
	if c.Request.Method == http.MethodGet {
		c.Status(http.StatusOK)
		return
	}

	if err := c.Request.ParseForm(); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("mailchimp webhook: unable to parse form body")
		c.Status(http.StatusOK)
		return
	}
	form := c.Request.PostForm
	eventType := form.Get("type")

	creds := s.resolveTriggerCreds(tr.ID)

	// Event filter: drop events not selected in the trigger config.
	if filter := creds["events"]; filter != "" && !mailchimpCSVContains(filter, eventType) {
		c.Status(http.StatusOK)
		return
	}

	// Audience guard: drop events from a different list than the one configured.
	// The callback URL is keyed on the stable trigger ID, so a webhook left on a
	// previously-configured audience (after the trigger is re-pointed) could
	// otherwise keep firing the flow.
	if want := creds["list_id"]; want != "" {
		if got := form.Get("data[list_id]"); got != "" && got != want {
			c.Status(http.StatusOK)
			return
		}
	}

	body := make(map[string]interface{}, len(form))
	for k, v := range form {
		if len(v) > 0 {
			body[k] = v[0]
		}
	}

	data := map[string]interface{}{
		"event_type":   eventType,
		"list_id":      form.Get("data[list_id]"),
		"email":        form.Get("data[email]"),
		"fired_at":     form.Get("fired_at"),
		"body":         body,
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}
	if nodeID := creds["__node_id"]; nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire mailchimp webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
