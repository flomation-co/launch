package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"flomation.app/automate/launch"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// jiraHTTPClient is used for the outbound Jira REST calls that
// register/deregister webhooks. The Jira site URL is caller-supplied, so this
// client is SSRF-hardened the same way as the WooCommerce client (and the api's
// option proxies): the dialer refuses link-local and cloud-metadata
// destinations (169.254.169.254 et al) on the address actually dialed, and
// cross-host redirects are refused. Loopback and private LAN ranges stay
// allowed — self-hosted Jira Data Center instances commonly live there.
var jiraHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("stopped after too many redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			return errors.New("cross-host redirect not allowed")
		}
		return nil
	},
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil {
					return nil
				}
				if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
					return errors.New("link-local addresses are not allowed")
				}
				if isJiraCloudMetadataIP(ip) {
					return errors.New("cloud metadata addresses are not allowed")
				}
				return nil
			},
		}).DialContext,
	},
}

// jiraBlockedMetadataIPs are instance-metadata addresses outside the link-local
// range (AWS IPv6 IMDS and Alibaba's 100.100.100.200). Private RFC1918/ULA
// ranges are deliberately NOT blocked — a self-hosted Jira Data Center
// legitimately lives there. Mirrors the WooCommerce dialer's list; keep in sync.
var jiraBlockedMetadataIPs = []net.IP{
	net.ParseIP("fd00:ec2::254"),
	net.ParseIP("100.100.100.200"),
}

func isJiraCloudMetadataIP(ip net.IP) bool {
	for _, b := range jiraBlockedMetadataIPs {
		if b != nil && ip.Equal(b) {
			return true
		}
	}
	return false
}

// jiraWebhookAPIPath is the Jira classic-webhook REST endpoint. NOTE: the
// webhook API lives at /rest/webhooks/1.0, NOT under /rest/api/2.
const jiraWebhookAPIPath = "/rest/webhooks/1.0/webhook"

// jiraAllEvents is the default event selection when the trigger config carries
// none (matching the WooCommerce/Acuity convention: empty = all).
var jiraAllEvents = []string{
	"jira:issue_created", "jira:issue_updated", "jira:issue_deleted",
	"comment_created", "comment_updated", "comment_deleted",
}

// jiraIssueEvents / jiraCommentEvents back the operator-friendly "issues" /
// "comments" shortcuts in the events config.
var jiraIssueEvents = []string{"jira:issue_created", "jira:issue_updated", "jira:issue_deleted"}
var jiraCommentEvents = []string{"comment_created", "comment_updated", "comment_deleted"}

// jiraStateKey is the trigger_state key holding the webhook created for a
// jira-webhook trigger.
const jiraStateKey = "jira_webhook"

// jiraWebhookState is what we persist per trigger: the absolute self URL of the
// webhook we created (so it can be removed on delete), its parsed id, the event
// set + JQL filter it covers (so a config change is detected and the webhook
// recreated), and the site it was created on (so re-pointing the trigger at a
// different site recreates the webhook there).
type jiraWebhookState struct {
	Self   string   `json:"self"`
	ID     string   `json:"id"`
	Events []string `json:"events"`
	JQL    string   `json:"jql"`
	Base   string   `json:"base"`
}

// jiraCreds is the resolved connection for a Jira trigger.
type jiraCreds struct {
	base     string // normalised site URL (scheme+host[+path], no trailing slash)
	email    string
	apiToken string
	events   []string
	jql      string
}

// resolveJiraCreds pulls and normalises the trigger's Jira connection. ok is
// false when a required part is missing.
func (s *Service) resolveJiraCreds(tr *launch.Trigger) (jiraCreds, bool) {
	c := s.resolveTriggerCreds(tr.ID)
	base := jiraBaseURL(c["url"])
	email := strings.TrimSpace(c["email"])
	apiToken := strings.TrimSpace(c["api_token"])
	if base == "" || email == "" || apiToken == "" {
		return jiraCreds{}, false
	}
	return jiraCreds{
		base:     base,
		email:    email,
		apiToken: apiToken,
		events:   jiraEvents(c["events"]),
		jql:      strings.TrimSpace(c["jql"]),
	}, true
}

// jiraBaseURL normalises a pasted site URL to scheme+host[+path] with no
// trailing slash and no REST-API suffix, defaulting to https. Returns "" when
// the value is blank or not an http(s) URL (e.g. an unresolved ${...} ref).
func jiraBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || strings.Contains(s, "${") {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	path := strings.TrimRight(u.Path, "/")
	for _, suffix := range []string{jiraWebhookAPIPath, "/rest/webhooks/1.0", "/rest/api/2", "/rest/api/3", "/rest"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	u.User = nil
	return u.Scheme + "://" + u.Host + path
}

// jiraEvents parses the trigger's event selection (a plain string from
// resolveTriggerCreds). It accepts the operator-friendly shortcuts
// "all"/"issues"/"comments", or an explicit comma-separated / JSON-array list of
// Jira event names. Empty (or "all") → all six events.
func jiraEvents(sel string) []string {
	sel = strings.TrimSpace(sel)
	switch strings.ToLower(sel) {
	case "", "all":
		return jiraAllEvents
	case "issues":
		return jiraIssueEvents
	case "comments":
		return jiraCommentEvents
	}
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
		return jiraAllEvents
	}
	return out
}

func jiraSameEventSet(a, b []string) bool {
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

// jiraMatchesFilter reports whether a fired event is in the registered set.
// An empty registered set matches all.
func jiraMatchesFilter(event string, events []string) bool {
	if len(events) == 0 {
		return true
	}
	for _, e := range events {
		if e == event {
			return true
		}
	}
	return false
}

// jiraDo performs an authenticated Jira REST request. fullURL is an absolute
// URL (the register endpoint built from the site base, or an absolute webhook
// self URL for deletion). Credentials go in the HTTP Basic auth header
// (email:api_token).
func jiraDo(ctx context.Context, cr jiraCreds, method, fullURL string, body interface{}) (map[string]interface{}, int, error) {
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
	req.SetBasicAuth(cr.email, cr.apiToken)
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := jiraHTTPClient.Do(req)
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
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": b}).Warn("jira: non-JSON response body")
		}
	}
	return out, resp.StatusCode, nil
}

// loadJiraState reads the persisted webhook for a trigger. A missing row yields
// a nil state, not an error.
func (s *Service) loadJiraState(triggerID string) *jiraWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("jira trigger: unable to load state")
		return nil
	}
	raw, ok := rows[jiraStateKey]
	if !ok {
		return nil
	}
	var st jiraWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// deleteJiraWebhook removes the given webhook from the site (best effort). self
// is the absolute self URL Jira returned at creation; a 404 is treated as
// already-gone.
func deleteJiraWebhook(ctx context.Context, cr jiraCreds, self string) {
	if self == "" {
		return
	}
	if _, status, err := jiraDo(ctx, cr, http.MethodDelete, self, nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
		log.WithFields(log.Fields{"self": self, "status": status, "error": err}).Warn("jira trigger: failed to delete webhook")
	}
}

// registerJiraWebhook auto-registers one Jira classic webhook covering the
// selected events, pointing at {PublicURL}/webhook/{trigger_id}. Idempotent —
// createTrigger runs on every flow save, so an unchanged event set + JQL + site
// is left untouched; a changed one recreates the webhook. Errors are logged,
// never fatal (they must not fail the trigger upsert).
func (s *Service) registerJiraWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeJiraWebhook {
		return
	}
	cr, ok := s.resolveJiraCreds(tr)
	if !ok {
		log.WithField("trigger_id", tr.ID).Warn("jira trigger: missing site URL / email / api token; skipping webhook registration")
		return
	}
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since the last save: keep the existing webhook. Checked
	// before any REST call — createTrigger runs on every flow save and the
	// common case must not cost a round-trip.
	state := s.loadJiraState(tr.ID)
	if state != nil && state.Self != "" && state.Base == cr.base && state.JQL == cr.jql && jiraSameEventSet(state.Events, cr.events) {
		return
	}

	// The config changed (or stored state is unusable): remove the old webhook
	// before creating a replacement. The callback is keyed on the stable trigger
	// ID, so a stale webhook would double-fire the flow.
	if state != nil && state.Self != "" {
		deleteJiraWebhook(ctx, cr, state.Self)
	}

	body := map[string]interface{}{
		"name":        "Flomation trigger " + tr.ID,
		"url":         callback,
		"events":      cr.events,
		"excludeBody": false,
	}
	// The JQL filter is optional; omit the filter key entirely when blank.
	if cr.jql != "" {
		body["filters"] = map[string]interface{}{"issue-related-events-section": cr.jql}
	}

	resp, status, err := jiraDo(ctx, cr, http.MethodPost, cr.base+jiraWebhookAPIPath, body)
	if err != nil || status >= 300 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "status": status, "error": err, "response": resp}).Warn("failed to register Jira webhook")
		return
	}
	self := asString(resp["self"])
	if self == "" {
		log.WithFields(log.Fields{"trigger_id": tr.ID}).Warn("jira trigger: webhook created but no self URL returned")
		return
	}
	// The id is the last path segment of the self URL (Jira does not return a
	// separate id field on classic webhooks). Persisted for diagnostics.
	id := self
	if idx := strings.LastIndex(self, "/"); idx >= 0 && idx+1 < len(self) {
		id = self[idx+1:]
	}

	stateJSON, _ := json.Marshal(jiraWebhookState{Self: self, ID: id, Events: cr.events, JQL: cr.jql, Base: cr.base})
	if err := s.db.UpsertTriggerState(tr.ID, jiraStateKey, stateJSON); err != nil {
		// Without persisted state we can never deregister; remove the
		// just-created webhook so the next save retries cleanly.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("jira trigger: unable to persist state; removing webhook")
		deleteJiraWebhook(ctx, cr, self)
	}
}

// deregisterJiraWebhook removes the webhook we registered for this trigger
// (best effort; logged, never fatal).
func (s *Service) deregisterJiraWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeJiraWebhook {
		return
	}
	state := s.loadJiraState(tr.ID)
	if state == nil || state.Self == "" {
		return
	}
	cr, ok := s.resolveJiraCreds(tr)
	if !ok {
		return
	}
	deleteJiraWebhook(ctx, cr, state.Self)
	if err := s.db.DeleteTriggerState(tr.ID, jiraStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("jira trigger: unable to delete state")
	}
}

// handleJiraWebhook handles an inbound Jira webhook for a trigger. Jira classic
// webhooks are NOT signed by default, so the body is accepted as-is and handed
// to the flow (unlike WooCommerce, there is no signature to verify). Called from
// handleWebhook after the trigger has been fetched and type-checked.
func (s *Service) handleJiraWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	var data map[string]interface{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &data); err != nil {
			log.WithFields(log.Fields{"id": id, "error": err}).Error("Jira webhook parse failed")
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
	}
	if data == nil {
		data = map[string]interface{}{}
	}

	// Event filter against the events we actually registered. Jira reports the
	// fired event in the payload's webhookEvent field. The webhook is already
	// event-scoped, but this guards deliveries from a webhook created before a
	// config change. Empty registered set matches all.
	state := s.loadJiraState(id)
	event, _ := data["webhookEvent"].(string)
	if state != nil && !jiraMatchesFilter(event, state.Events) {
		c.Status(http.StatusOK)
		return
	}

	var triggerData map[string]interface{}
	_ = json.Unmarshal(tr.Data, &triggerData)

	// Carry __node_id so the executor injects event data into the correct
	// trigger node in multi-trigger flows.
	if nodeID := asString(triggerData["__node_id"]); nodeID != "" {
		data["__node_id"] = nodeID
	}

	go func() {
		if err := s.trigger.Trigger(tr, data); err != nil {
			log.WithFields(log.Fields{"error": err}).Error("unable to fire Jira webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
