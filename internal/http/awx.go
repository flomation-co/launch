package http

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"flomation.app/automate/launch"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// AWX / Ansible Automation Platform webhook trigger.
//
// AWX has no "job finished" webhook in the usual sense. Its outbound mechanism is
// a NOTIFICATION TEMPLATE of notification_type "webhook", which is attached to a
// job template through three per-event sub-relations. (POST /job_templates/{id}/
// github|gitlab/ and webhook_key are the INBOUND direction — GitHub telling AWX
// to launch a job — and are a different subsystem entirely. Do not let their
// existence mislead: inbound is HMAC-signed, outbound is not.)
//
// ★ AWX OUTBOUND NOTIFICATIONS ARE COMPLETELY UNSIGNED. No HMAC, no signature
// header, no timestamp, no nonce, no replay protection — the whole delivery is
// one requests.post() with an optional auth tuple and an optional headers map.
// The two authenticity mechanisms available to us are therefore that headers map
// and HTTP Basic auth.
//
// ★ THE SECRET GOES IN `password`, NEVER IN `headers`. AWX encrypts exactly one
// field of a webhook notification_configuration — `password` — and reads it back
// as "$encrypted$". `headers` is an untyped object: it is never encrypted, never
// masked, and GET /notification_templates/{id}/ hands it back in PLAINTEXT to any
// AWX user with view access, as well as writing it unencrypted into every AWX
// backup and export. Putting the shared secret in a custom header — the obvious
// design — would leak it to every org auditor on the box. So we mint the secret
// into `password` (with a fixed `username`), AWX sends it as
// `Authorization: Basic base64(user:secret)` on every delivery, and `headers`
// carries nothing but an echo of the already-public trigger id for log
// correlation.
//
// ★ DELIVERY IS AT-MOST-ONCE. AWX's MAX_RETRIES applies only to following
// redirects. A 4xx, a 5xx, a timeout or a connection error marks the notification
// failed and AWX NEVER retries it — if launch is down, the event is lost for
// good. AWX also sets no HTTP timeout on the request, so a slow ingest pins an
// AWX dispatcher worker for the full task timeout. handleAWXWebhook therefore
// answers 200 immediately and does every slow thing (the callback verification
// and the flow dispatch) on a background goroutine.

// awxHTTPClient is used for the outbound AWX REST calls that register and
// deregister the notification template. The AWX base URL is caller-supplied, so
// this client is SSRF-hardened the same way as the WooCommerce/Jira clients (and
// the api's option proxies): the dialer refuses link-local and cloud-metadata
// destinations (169.254.169.254 et al) on the address ACTUALLY dialed, which
// closes DNS rebinding, and cross-host redirects are refused. Loopback and
// private LAN ranges stay allowed on purpose — a self-hosted AWX/AAP almost
// always lives there.
var awxHTTPClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: awxCheckRedirect,
	Transport: &http.Transport{
		DialContext: awxDialer.DialContext,
	},
}

// awxInsecureHTTPClient is awxHTTPClient with certificate verification switched
// off. It is selected ONLY when the trigger node ticked "Allow insecure TLS" —
// the escape hatch for the self-signed certificate a lab AWX ships with. The SSRF
// dialer and the redirect policy are identical; only the TLS trust decision
// changes.
var awxInsecureHTTPClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: awxCheckRedirect,
	Transport: &http.Transport{
		DialContext: awxDialer.DialContext,
		// #nosec G402 -- opt-in per trigger via the node's allow_insecure input, for
		// self-hosted AWX/AAP behind a self-signed certificate. Never the default.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

// awxDialer refuses link-local and cloud-metadata destinations on the address
// actually dialed (so a hostname that resolves to 169.254.169.254 is caught, not
// just a literal one).
var awxDialer = &net.Dialer{
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
		if isAWXCloudMetadataIP(ip) {
			return errors.New("cloud metadata addresses are not allowed")
		}
		return nil
	},
}

func awxCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("stopped after too many redirects")
	}
	if req.URL.Host != via[0].URL.Host {
		return errors.New("cross-host redirect not allowed")
	}
	return nil
}

// awxBlockedMetadataIPs are instance-metadata addresses outside the link-local
// range (AWS IPv6 IMDS and Alibaba's 100.100.100.200). Private RFC1918/ULA ranges
// are deliberately NOT blocked — a self-hosted AWX legitimately lives there.
// Mirrors the WooCommerce/Jira dialers' list; keep them in sync.
var awxBlockedMetadataIPs = []net.IP{
	net.ParseIP("fd00:ec2::254"),
	net.ParseIP("100.100.100.200"),
}

func isAWXCloudMetadataIP(ip net.IP) bool {
	for _, b := range awxBlockedMetadataIPs {
		if b != nil && ip.Equal(b) {
			return true
		}
	}
	return false
}

// awxStateKey is the trigger_state key holding the notification template we
// created for an awx-webhook trigger.
const awxStateKey = "awx_webhook"

// awxBasicUser is the username half of the Basic credential AWX is configured to
// present on every delivery. It is a fixed, non-secret marker — the secret is the
// password half. Constant so the inbound compare has something to check against
// without another round-trip.
const awxBasicUser = "flomation"

// awxTriggerHeader is the one custom header we set on the notification template.
// It carries the trigger id — which is already public, being the last path
// segment of the callback URL — purely so a delivery can be correlated in an AWX
// log. ★ NOTHING SECRET MAY EVER GO IN HERE: AWX stores and returns the headers
// map in plaintext.
const awxTriggerHeader = "X-Flomation-Trigger"

// awxCandidateRoots are the two places a controller API is ever served from:
// upstream AWX and AAP <= 2.4 use /api/v2/, AAP 2.5+ behind the platform gateway
// uses /api/controller/v2/.
var awxCandidateRoots = []string{"/api/v2/", "/api/controller/v2/"}

// The four LOGICAL events the operator picks from.
const (
	awxEventStarted    = "started"
	awxEventSuccessful = "successful"
	awxEventFailed     = "failed"
	awxEventCanceled   = "canceled"
)

// awxAllEvents is the default selection when the trigger config carries none
// (matching the WooCommerce/Jira convention: empty = all).
var awxAllEvents = []string{awxEventStarted, awxEventSuccessful, awxEventFailed, awxEventCanceled}

// awxRelationFor maps one of our four LOGICAL events onto the AWX notification
// relation that actually delivers it.
//
// AWX has only THREE events, and `error` is a CATCH-ALL:
// STATUS_TO_TEMPLATE_TYPE = {'succeeded': 'success', 'running': 'started',
// 'failed': 'error'}, and the call sites reduce every terminal state to
// 'succeeded' or 'failed'. So a failed job, an errored job AND a canceled job all
// fire the `error` template — there is no distinct "cancelled" webhook. `failed`
// and `canceled` therefore share one relation, and handleAWXWebhook discriminates
// them on the payload's status. Without that, someone who subscribed only to
// "Canceled" would be woken by every failure.
//
// ★ The relation is `notification_templates_error`, NOT `_failure`. (The
// confusion comes from the internal M2M related_name,
// %(class)s_notification_templates_for_errors, which never appears in a URL.)
var awxRelationFor = map[string]string{
	awxEventStarted:    "started",
	awxEventSuccessful: "success",
	awxEventFailed:     "error",
	awxEventCanceled:   "error",
}

// The two template kinds a notification can be attached to. Both expose the same
// three notification_templates_* sub-relations.
const (
	awxKindJobTemplate      = "job_template"
	awxKindWorkflowTemplate = "workflow_job_template"
)

// awxResourcePath maps the operator's template kind to its REST collection.
func awxResourcePath(kind string) string {
	if kind == awxKindWorkflowTemplate {
		return "workflow_job_templates"
	}
	return "job_templates"
}

// awxWebhookState is what we persist per trigger.
//
// TemplateID is the ONLY way to tear the notification template down again — it
// cannot be derived later, and deregistration runs after the trigger row is
// already on its way out, so it has to be here. Attached records the relations
// that ACTUALLY attached (not the ones requested), which makes a partial
// registration self-heal: the next save's change-check sees attached != wanted
// and rebuilds. Base and TemplateRefID make re-pointing the trigger at a
// different AWX (or a different job template) recreate the template there rather
// than orphaning it on the old one. Secret lets the inbound path verify a
// delivery without re-resolving the node's credentials on every request.
type awxWebhookState struct {
	TemplateID    string   `json:"template_id"`
	Kind          string   `json:"kind"`
	TemplateRefID string   `json:"template_ref_id"`
	Attached      []string `json:"attached"`
	Logical       []string `json:"logical"`
	Base          string   `json:"base"`
	Root          string   `json:"root"`
	OrgID         string   `json:"org_id"`
	Secret        string   `json:"secret"`
}

// awxCreds is the resolved connection for an AWX trigger.
type awxCreds struct {
	base       string // normalised AWX URL (scheme+host[+path], no trailing slash)
	method     string // "token" (default) or "basic"
	token      string
	username   string
	password   string
	insecure   bool
	apiPrefix  string // operator override for the API root; "" = auto-discover
	kind       string // awxKindJobTemplate | awxKindWorkflowTemplate
	templateID string // the job/workflow template the notification attaches to
	logical    []string
}

// client picks the hardened client, honouring the node's allow_insecure opt-in.
func (cr awxCreds) client() *http.Client {
	if cr.insecure {
		return awxInsecureHTTPClient
	}
	return awxHTTPClient
}

// resolveAwxCreds pulls and normalises the trigger's AWX connection. ok is false
// when a required part is missing.
//
// ⚠ resolveTriggerCreds only surfaces STRING values from trigger.Data — a JSON
// bool or number in the node config is silently DROPPED — so allow_insecure and
// skip_awx_verification are read straight off tr.Data with a bool|string
// type-switch (the wooCredsInQuery pattern).
func (s *Service) resolveAwxCreds(tr *launch.Trigger) (awxCreds, bool) {
	c := s.resolveTriggerCreds(tr.ID)
	if c == nil {
		// A nil map means the trigger re-fetch or its config parse FAILED — not
		// that nothing is configured. Reporting "not ok" is the fail-closed
		// answer: the caller logs and skips rather than acting on empty creds.
		return awxCreds{}, false
	}

	base := awxBaseURL(c["awx_url"])
	if base == "" {
		return awxCreds{}, false
	}

	cr := awxCreds{
		base:      base,
		method:    strings.TrimSpace(c["auth_method"]),
		token:     strings.TrimSpace(c["api_token"]),
		username:  strings.TrimSpace(c["awx_username"]),
		password:  strings.TrimSpace(c["awx_password"]),
		insecure:  awxBoolInput(tr, "allow_insecure"),
		apiPrefix: strings.TrimSpace(c["api_prefix"]),
		kind:      awxTemplateKind(c["template_kind"]),
		logical:   awxEvents(c["events"]),
	}

	// The two template pickers are mutually exclusive: the editor hides whichever
	// one the chosen kind does not use, so exactly one carries a value.
	if cr.kind == awxKindWorkflowTemplate {
		cr.templateID = strings.TrimSpace(c["workflow_template_id"])
	} else {
		cr.templateID = strings.TrimSpace(c["job_template_id"])
	}
	if cr.templateID == "" || strings.Contains(cr.templateID, "${") {
		return awxCreds{}, false
	}

	// Basic auth needs both halves; token auth (the default) needs the token.
	if cr.method == "basic" {
		if cr.username == "" || cr.password == "" {
			return awxCreds{}, false
		}
	} else if cr.token == "" {
		return awxCreds{}, false
	}

	return cr, true
}

// awxTemplateKind normalises the template_kind input. Empty means the default,
// a job template.
func awxTemplateKind(raw string) string {
	if strings.TrimSpace(raw) == awxKindWorkflowTemplate {
		return awxKindWorkflowTemplate
	}
	return awxKindJobTemplate
}

// awxBoolInput reads a boolean node input off the raw trigger data.
// resolveTriggerCreds only surfaces string values (a JSON bool is dropped), so
// booleans have to come from here — tolerating either a JSON bool or a
// "true"/"false" string, since a variable-bound checkbox arrives as the latter.
func awxBoolInput(tr *launch.Trigger, name string) bool {
	var raw map[string]interface{}
	if tr == nil || json.Unmarshal(tr.Data, &raw) != nil {
		return false
	}
	switch v := raw[name].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// awxBaseURL normalises a pasted AWX/AAP URL to scheme+host[+path] with no
// trailing slash and no REST-API suffix, defaulting to https. Returns "" when the
// value is blank or not an http(s) URL (e.g. an unresolved ${...} ref).
//
// NOTE: intentionally duplicated in the executor (awx.NormaliseBaseURL) and the
// api's option proxy — they are separate Go modules with no shared package. Keep
// the three in sync.
func awxBaseURL(raw string) string {
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
	for _, suffix := range []string{"/api/controller/v2", "/api/controller", "/api/v2", "/api"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	u.User = nil // drop any user:pass@ smuggled into the pasted URL
	return u.Scheme + "://" + u.Host + path
}

// awxEvents parses the trigger's event selection (a plain string from
// resolveTriggerCreds — comma-separated or a JSON array). Unknown names are
// dropped; an empty selection means all four.
func awxEvents(sel string) []string {
	sel = strings.TrimSpace(sel)
	var raw []string
	if strings.HasPrefix(sel, "[") {
		var arr []string
		if json.Unmarshal([]byte(sel), &arr) == nil {
			raw = arr
		}
	} else {
		raw = strings.Split(sel, ",")
	}

	seen := map[string]bool{}
	var out []string
	for _, e := range raw {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		if _, known := awxRelationFor[e]; !known {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	if len(out) == 0 {
		return awxAllEvents
	}
	return out
}

// awxRelations reduces a logical event selection to the unique set of AWX
// relations that has to be attached. `failed` and `canceled` both collapse onto
// `error`, so picking both attaches it once. Order is stable so the change-check
// and the tests can compare directly.
func awxRelations(logical []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, want := range []string{"started", "success", "error"} {
		for _, l := range logical {
			if awxRelationFor[l] == want && !seen[want] {
				seen[want] = true
				out = append(out, want)
			}
		}
	}
	return out
}

func awxSameEventSet(a, b []string) bool {
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

// awxEventStr stringifies an AWX payload field. Ids come through as JSON numbers
// (float64) — the shared asString returns "" for those — so integers are rendered
// without a decimal point. Strings pass through; anything else yields "".
func awxEventStr(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		return ""
	}
}

// awxRedact strips every secret this trigger knows about out of a string bound
// for a log line or an error: the AWX credential (the token, or the password and
// the base64 Basic blob AWX would be sent), plus any extra secret the caller
// holds — in practice the shared secret we mint for the notification template.
// Nothing reaches a log or an error message without passing through here.
//
// Short values are left alone: replacing a two-character password would turn the
// message into confetti without protecting anything worth protecting.
func awxRedact(cr awxCreds, msg string, extra ...string) string {
	secrets := append([]string{cr.token, cr.password}, extra...)
	for _, sec := range secrets {
		if len(sec) >= 8 {
			msg = strings.ReplaceAll(msg, sec, "[REDACTED]")
		}
	}
	if cr.username != "" && cr.password != "" {
		blob := base64.StdEncoding.EncodeToString([]byte(cr.username + ":" + cr.password))
		msg = strings.ReplaceAll(msg, blob, "[REDACTED]")
	}
	return msg
}

// awxNormalisePrefix forces a leading and a trailing slash onto an API prefix, so
// "api/v2", "/api/v2" and "/api/v2/" all mean the same thing.
func awxNormalisePrefix(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// awxDo performs an authenticated AWX REST request. root is the discovered API
// prefix (leading and trailing slash, e.g. "/api/v2/") and path is relative to it
// (e.g. "notification_templates/").
//
// ⚠ EVERY PATH MUST END IN A SLASH. Django's APPEND_SLASH 301s a slash-less POST,
// and Go's http.Client turns a redirected POST into a GET *and drops the body* —
// which surfaces as a mystifying empty-payload request that AWX rejects. The
// callers all comply; this is the reason why.
func awxDo(ctx context.Context, cr awxCreds, root, method, path string, body interface{}) (map[string]interface{}, int, error) {
	fullURL := cr.base + root + strings.TrimPrefix(path, "/")

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
	if cr.method == "basic" {
		req.SetBasicAuth(cr.username, cr.password)
	} else {
		req.Header.Set("Authorization", "Bearer "+cr.token)
	}
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := cr.client().Do(req)
	if err != nil {
		// The transport error can quote the request URL; it never carries the
		// credential (which lives in a header), but redact anyway — this string
		// is on its way to a log.
		return nil, 0, errors.New(awxRedact(cr, err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]interface{}
	if len(bytes.TrimSpace(raw)) > 0 {
		// An attach or a detach answers 204 with an empty body — not an error.
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			b := string(raw)
			if len(b) > 512 {
				b = b[:512]
			}
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": awxRedact(cr, b)}).Warn("awx: non-JSON response body")
		}
	}
	return out, resp.StatusCode, nil
}

// awxErrSnippet renders an AWX error response for a log line, redacted and
// truncated.
func awxErrSnippet(cr awxCreds, resp map[string]interface{}, extra ...string) string {
	if len(resp) == 0 {
		return ""
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > 512 {
		s = s[:512]
	}
	return awxRedact(cr, s, extra...)
}

// ---------------------------------------------------------------------------
// API root discovery
// ---------------------------------------------------------------------------

// awxResolveRoot discovers which prefix this deployment serves the controller API
// under: upstream AWX and AAP <= 2.4 use /api/v2/, AAP 2.5+ behind the platform
// gateway uses /api/controller/v2/. The operator can override it with the node's
// api_prefix input, which short-circuits the sweep entirely.
func awxResolveRoot(ctx context.Context, cr awxCreds) (string, error) {
	if p := strings.TrimSpace(cr.apiPrefix); p != "" {
		return awxNormalisePrefix(p), nil
	}

	for _, prefix := range awxProbeCandidates(ctx, cr) {
		_, status, err := awxDo(ctx, cr, prefix, http.MethodGet, "me/", nil)
		if err != nil {
			continue // unreachable — try the next candidate
		}
		switch {
		case status >= 200 && status < 300:
			return prefix, nil
		case status == http.StatusUnauthorized, status == http.StatusForbidden:
			// The prefix is RIGHT; the credential is not. Never sweep on — doing so
			// would report "API not found" for what is really a rejected token, and
			// send the operator hunting for the wrong bug.
			return "", fmt.Errorf("AWX rejected the credential at %s (HTTP %d)", prefix, status)
		}
	}
	return "", fmt.Errorf("could not find the AWX / AAP API at %s (tried %s)", cr.base, strings.Join(awxCandidateRoots, ", "))
}

// awxProbeCandidates orders the candidate roots using the unauthenticated /api/
// banner. It is an optimisation and a tie-breaker; the authenticated sweep in
// awxResolveRoot is what is authoritative.
func awxProbeCandidates(ctx context.Context, cr awxCreds) []string {
	body, ok := awxAPIBanner(ctx, cr)
	if !ok {
		return awxCandidateRoots
	}

	prefix := ""
	if versions, ok := body["available_versions"].(map[string]interface{}); ok {
		if v, ok := versions["v2"].(string); ok && v != "" {
			prefix = v
		}
	}
	if prefix == "" {
		if v, ok := body["current_version"].(string); ok && v != "" {
			prefix = v
		}
	}
	if prefix == "" {
		// No available_versions AND no current_version: the AAP 2.5+ platform
		// gateway. ★ We key on the ABSENCE of a field upstream AWX is known to
		// have, never on a gateway field name we would be guessing at — the
		// gateway is closed source and its root body is undocumented.
		prefix = "/api/controller/v2/"
	}

	prefix = awxNormalisePrefix(prefix)
	ordered := []string{prefix}
	for _, c := range awxCandidateRoots {
		if c != prefix {
			ordered = append(ordered, c)
		}
	}
	return ordered
}

// awxAPIBanner GETs {base}/api/ and returns the decoded banner. Any failure is
// reported as !ok rather than as an error: the caller just falls back to the
// unordered sweep.
func awxAPIBanner(ctx context.Context, cr awxCreds) (map[string]interface{}, bool) {
	body, status, err := awxDo(ctx, cr, "/api/", http.MethodGet, "", nil)
	if err != nil || status < 200 || status >= 300 || len(body) == 0 {
		return nil, false
	}
	return body, true
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// awxResolveOrg reads the organization the chosen template belongs to. The
// notification template we are about to create needs one.
//
// ★ DO NOT HARDCODE ORGANIZATION 1. `organization` is nullable to DRF but
// MANDATORY at the RBAC layer for everyone except a superuser, so omitting it
// gives a normal user a 403 PermissionDenied (not a helpful 400), and gives a
// superuser something worse: an ORPHAN template with organization: null. The
// orphan matters because AWX's unique_together ('organization', 'name') does not
// fire when organization is NULL — Postgres treats NULLs as distinct — so our
// name-based idempotency silently breaks and duplicate templates accumulate
// without limit.
//
// Job and workflow templates both expose `organization` directly (and again under
// summary_fields), so it resolves off the template the operator already picked —
// no org picker on the node.
func awxResolveOrg(ctx context.Context, cr awxCreds, root string) (string, error) {
	path := fmt.Sprintf("%s/%s/", awxResourcePath(cr.kind), url.PathEscape(cr.templateID))
	resp, status, err := awxDo(ctx, cr, root, http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("could not read the AWX template: %s", awxRedact(cr, err.Error()))
	}
	if status == http.StatusNotFound {
		return "", fmt.Errorf("AWX does not know %s %s", strings.ReplaceAll(cr.kind, "_", " "), cr.templateID)
	}
	if status >= 300 {
		return "", fmt.Errorf("AWX returned HTTP %d reading %s %s", status, strings.ReplaceAll(cr.kind, "_", " "), cr.templateID)
	}

	if org := awxEventStr(resp["organization"]); org != "" {
		return org, nil
	}
	if sf, ok := resp["summary_fields"].(map[string]interface{}); ok {
		if o, ok := sf["organization"].(map[string]interface{}); ok {
			if org := awxEventStr(o["id"]); org != "" {
				return org, nil
			}
		}
	}
	return "", errors.New("the selected AWX template belongs to no organization, so a notification template cannot be created for it (AWX requires one). Assign the template to an organization in AWX and save the flow again")
}

// awxTemplateName is the notification template's name in AWX.
//
// ★ IT MUST BE UNIQUE PER TRIGGER. Notification templates are org-scoped and
// name-unique within an org (their named_url is <name>++<Org>), so a bare
// "Flomation" would make the second flow on the same AWX collide with the first.
// The trigger id is what makes it unique, and it is also what lets a human
// looking at AWX work out which flow owns the thing.
func awxTemplateName(triggerID string) string {
	return "Flomation trigger " + triggerID
}

// awxIsNameConflict reports whether a 400 body is AWX complaining that the
// notification template name is already taken in this organization.
func awxIsNameConflict(resp map[string]interface{}) bool {
	b, err := json.Marshal(resp)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(b)), "already exists")
}

// awxNotificationConfig builds the notification_configuration for our webhook.
func awxNotificationConfig(triggerID, callback, secret string) map[string]interface{} {
	return map[string]interface{}{
		"url": callback,
		// ★ `headers` is REQUIRED — it has no default in WebhookBackend's
		// init_parameters, so omitting it is a flat 400 ("Missing required fields
		// for Notification Configuration: ['headers']"). And ★ NOTHING SECRET GOES
		// IN IT: AWX stores and returns headers in plaintext. The trigger id is
		// already public (it is the last path segment of the callback URL), so
		// echoing it here costs nothing and buys log correlation.
		"headers":     map[string]string{awxTriggerHeader: triggerID},
		"http_method": "POST",
		// Inverted semantics, despite the "Verify SSL" label in the AWX UI:
		// verify = not disable_ssl_verification. false therefore means AWX WILL
		// verify our TLS certificate. Note this is about AWX trusting LAUNCH's
		// certificate on the way in — the opposite direction from the node's
		// allow_insecure input, which is about launch trusting AWX's on the way
		// out. They are deliberately not wired together: a launch behind a
		// self-signed certificate needs that fixed at the ingress, not papered
		// over by silently weakening every delivery.
		"disable_ssl_verification": false,
		// ★ THE SECRET LIVES HERE. `password` is the ONLY field AWX encrypts (it
		// reads back as "$encrypted$"), and AWX turns username+password into
		// `Authorization: Basic base64(user:pass)` on every delivery — confirmed by
		// packet capture. handleAWXWebhook verifies exactly that header.
		"username": awxBasicUser,
		"password": secret,
	}
	// Deliberately NO custom `messages` body. The webhook backend's default is
	// {{ job_metadata }} — precisely the document we want. A custom body is
	// actively dangerous: format_body does json.loads(rendered) and falls back to
	// {} on a decode error, and AWX disabled the validation on save, so a
	// malformed template passes validation, passes /test/, and then silently POSTs
	// {} forever.
}

// awxRegister does the whole AWX side of registration: resolve the API root and
// the organization, mint the shared secret, create the notification template and
// attach it to the chosen job/workflow template for every selected event. It
// returns the state to persist.
//
// Free function rather than a method so the AWX conversation can be exercised
// against an httptest server with no database behind it.
func awxRegister(ctx context.Context, cr awxCreds, triggerID, callback string) (*awxWebhookState, error) {
	root, err := awxResolveRoot(ctx, cr)
	if err != nil {
		return nil, err
	}
	orgID, err := awxResolveOrg(ctx, cr, root)
	if err != nil {
		return nil, err
	}

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("could not generate a shared secret: %w", err)
	}
	secret := hex.EncodeToString(secretBytes)

	name := awxTemplateName(triggerID)
	body := map[string]interface{}{
		"name":                       name,
		"description":                "Auto-created by Flomation Automate. Do not edit or delete.",
		"organization":               orgID,
		"notification_type":          "webhook",
		"notification_configuration": awxNotificationConfig(triggerID, callback, secret),
	}

	resp, status, err := awxDo(ctx, cr, root, http.MethodPost, "notification_templates/", body)
	if err != nil {
		return nil, fmt.Errorf("could not create the AWX notification template: %s", awxRedact(cr, err.Error(), secret))
	}

	templateID := ""
	switch {
	case status >= 200 && status < 300:
		templateID = awxEventStr(resp["id"])
	case status == http.StatusBadRequest && awxIsNameConflict(resp):
		// A template with this name already exists in this org — our own, left
		// behind by a save whose state write did not land. Adopt it and rotate the
		// secret onto it rather than failing forever.
		templateID, err = awxAdoptTemplate(ctx, cr, root, name, orgID, triggerID, callback, secret)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("AWX rejected the notification template (HTTP %d) %s", status, awxErrSnippet(cr, resp, secret))
	}
	if templateID == "" {
		return nil, errors.New("AWX created the notification template but returned no id")
	}

	// Attach it to the template, once per unique relation. A partial attach is
	// kept, not rolled back: we persist what actually attached, and the next save
	// sees the shortfall and rebuilds.
	wanted := awxRelations(cr.logical)
	attached := awxAttach(ctx, cr, root, templateID, wanted)
	if len(attached) == 0 {
		// Nothing is listening, so the template is dead weight — and leaving it
		// behind would collide with the name on the next attempt. Take it away.
		awxDeleteTemplate(ctx, cr, root, templateID)
		return nil, fmt.Errorf("the AWX notification template could not be attached to %s %s for any selected event",
			strings.ReplaceAll(cr.kind, "_", " "), cr.templateID)
	}

	return &awxWebhookState{
		TemplateID:    templateID,
		Kind:          cr.kind,
		TemplateRefID: cr.templateID,
		Attached:      attached,
		Logical:       cr.logical,
		Base:          cr.base,
		Root:          root,
		OrgID:         orgID,
		Secret:        secret,
	}, nil
}

// awxAdoptTemplate takes over a notification template that already carries our
// name in this organization, rotating the shared secret onto it. Reached only
// when the create came back with the unique_together violation.
func awxAdoptTemplate(ctx context.Context, cr awxCreds, root, name, orgID, triggerID, callback, secret string) (string, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("organization", orgID)
	resp, status, err := awxDo(ctx, cr, root, http.MethodGet, "notification_templates/?"+q.Encode(), nil)
	if err != nil || status >= 300 {
		return "", fmt.Errorf("an AWX notification template named %q already exists but could not be read back (HTTP %d)", name, status)
	}

	results, _ := resp["results"].([]interface{})
	if len(results) == 0 {
		return "", fmt.Errorf("AWX says a notification template named %q already exists, but it is not readable with this credential", name)
	}
	first, _ := results[0].(map[string]interface{})
	templateID := awxEventStr(first["id"])
	if templateID == "" {
		return "", fmt.Errorf("an AWX notification template named %q already exists but has no id", name)
	}

	patch := map[string]interface{}{
		"notification_type":          "webhook",
		"notification_configuration": awxNotificationConfig(triggerID, callback, secret),
	}
	presp, pstatus, err := awxDo(ctx, cr, root, http.MethodPatch, "notification_templates/"+url.PathEscape(templateID)+"/", patch)
	if err != nil {
		return "", fmt.Errorf("could not rotate the secret on the existing AWX notification template: %s", awxRedact(cr, err.Error(), secret))
	}
	if pstatus >= 300 {
		return "", fmt.Errorf("AWX rejected the update to the existing notification template (HTTP %d) %s", pstatus, awxErrSnippet(cr, presp, secret))
	}
	return templateID, nil
}

// awxAttach attaches the notification template to the job/workflow template for
// each relation, returning the ones that actually took.
//
// The body's `id` must be an INTEGER and it must be PRESENT: POSTing to one of
// these sublists WITHOUT an id creates a brand-new notification template from the
// body and attaches that instead (201) — a silent duplicate-accumulation footgun
// on every typo. AWX answers 204 on success, and the attach is idempotent on its
// side.
func awxAttach(ctx context.Context, cr awxCreds, root, templateID string, relations []string) []string {
	id, err := awxTemplateIDNum(templateID)
	if err != nil {
		log.WithFields(log.Fields{"template_id": templateID, "error": err}).Warn("awx trigger: notification template id is not numeric; cannot attach")
		return nil
	}

	attached := make([]string, 0, len(relations))
	for _, rel := range relations {
		path := fmt.Sprintf("%s/%s/notification_templates_%s/", awxResourcePath(cr.kind), url.PathEscape(cr.templateID), rel)
		resp, status, err := awxDo(ctx, cr, root, http.MethodPost, path, map[string]interface{}{"id": id})
		if err != nil || status >= 300 {
			log.WithFields(log.Fields{
				"template_ref_id": cr.templateID,
				"relation":        rel,
				"status":          status,
				"error":           err,
				"response":        awxErrSnippet(cr, resp),
			}).Warn("awx trigger: failed to attach the notification template")
			continue
		}
		attached = append(attached, rel)
	}
	return attached
}

// awxDetach removes the notification template from each relation it was attached
// to. A template AWX no longer has is already gone, which is success.
//
// ⚠ NEVER disassociate through POST /organizations/{id}/notification_templates/:
// that view sets parent_key, so unattach_by_id calls sub.delete() and DELETES THE
// TEMPLATE OUTRIGHT. Only the _started/_success/_error sublists do a safe detach.
func awxDetach(ctx context.Context, cr awxCreds, root, templateID string, relations []string) {
	id, err := awxTemplateIDNum(templateID)
	if err != nil {
		return
	}
	for _, rel := range relations {
		path := fmt.Sprintf("%s/%s/notification_templates_%s/", awxResourcePath(cr.kind), url.PathEscape(cr.templateID), rel)
		resp, status, err := awxDo(ctx, cr, root, http.MethodPost, path, map[string]interface{}{"id": id, "disassociate": true})
		if err != nil || (status >= 300 && !awxAlreadyDetached(status, resp)) {
			log.WithFields(log.Fields{
				"template_ref_id": cr.templateID,
				"relation":        rel,
				"status":          status,
				"error":           err,
			}).Warn("awx trigger: failed to detach the notification template")
		}
	}
}

// awxAlreadyDetached reports whether a non-2xx detach is really AWX saying the
// notification template is already gone — which is success, not failure. Reached
// on a second teardown, and whenever an operator has removed the template in AWX
// by hand.
//
// ★ THE DETACH ANSWERS 400 HERE, NOT 404 — and the two AWX views disagree, which
// is exactly the sort of thing a hand-written fake will get wrong. The sublist
// resolves the body's `id` through get_object_or_400, so detaching a template that
// no longer exists comes back as
// 400 {"detail":"NotificationTemplate matching query does not exist."}, while
// DELETE /notification_templates/{id}/ on that same missing template is an
// ordinary DRF 404. Both mean already-gone. (Detaching a template that DOES exist
// but is not attached is a plain 204, so the missing-template 400 is the only
// already-gone shape the sublist has.) Verified against AWX 24.6.1; 404 is
// tolerated too, in case a later release makes the two views agree.
//
// Deliberately NOT a blanket "any 400 is fine": a malformed body is also a 400,
// and that one has to keep shouting.
func awxAlreadyDetached(status int, resp map[string]interface{}) bool {
	switch status {
	case http.StatusNotFound:
		return true
	case http.StatusBadRequest:
		return strings.Contains(strings.ToLower(awxEventStr(resp["detail"])), "does not exist")
	}
	return false
}

// awxDeleteTemplate removes the notification template from AWX. Deleting it also
// cascades away its attachments and its delivery log.
//
// Tolerates:
//   - 404 — already gone, which is success (AWX also answers 404, not 403, when
//     the credential may not delete it).
//   - 405 — "Delete not allowed while there are pending notifications": a
//     notification created in the last 8 hours is still pending, which is exactly
//     what happens when our own ingest was unreachable. Retried once; then left
//     alone. An orphaned template is inert — its callback URL is a trigger UUID
//     that now 404s — and blocking a flow delete over it would be worse.
func awxDeleteTemplate(ctx context.Context, cr awxCreds, root, templateID string) {
	if templateID == "" {
		return
	}
	path := "notification_templates/" + url.PathEscape(templateID) + "/"

	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		_, status, err := awxDo(ctx, cr, root, http.MethodDelete, path, nil)
		switch {
		case err == nil && (status < 300 || status == http.StatusNotFound):
			return
		case err == nil && status == http.StatusMethodNotAllowed:
			continue // a delivery is still pending — give it one more go
		default:
			log.WithFields(log.Fields{"template_id": templateID, "status": status, "error": err}).Warn("awx trigger: failed to delete the notification template")
			return
		}
	}
	log.WithFields(log.Fields{"template_id": templateID}).Warn("awx trigger: AWX would not delete the notification template (deliveries still pending); leaving it in place")
}

// awxTemplateIDNum parses the notification template id back to the integer AWX
// insists on in an attach/detach body. Strict on purpose (ParseInt, not Sscanf,
// which would happily take the 42 out of "42nonsense"): a body whose `id` is not
// what we think it is would attach the wrong template.
func awxTemplateIDNum(templateID string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(templateID), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("notification template id %q is not an integer", templateID)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Service wiring: register / deregister
// ---------------------------------------------------------------------------

// loadAwxState reads the persisted notification template for a trigger. A missing
// row yields a nil state, not an error.
func (s *Service) loadAwxState(triggerID string) *awxWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("awx trigger: unable to load state")
		return nil
	}
	raw, ok := rows[awxStateKey]
	if !ok {
		return nil
	}
	var st awxWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// awxStateCurrent reports whether the notification template we already have on
// record still matches the trigger's config exactly — in which case registration
// is a no-op and, crucially, costs NOTHING: createTrigger runs on every single
// flow save, so this predicate is what stops an unchanged flow from hammering the
// operator's AWX on every keystroke-to-save.
//
// Comparing Attached against the relations the current selection actually wants
// is what makes a PARTIAL registration self-heal: if only two of three relations
// attached last time, this returns false and the next save rebuilds.
func awxStateCurrent(state *awxWebhookState, cr awxCreds) bool {
	return state != nil &&
		state.TemplateID != "" &&
		state.Secret != "" &&
		state.Base == cr.base &&
		state.Kind == cr.kind &&
		state.TemplateRefID == cr.templateID &&
		awxSameEventSet(state.Logical, cr.logical) &&
		awxSameEventSet(state.Attached, awxRelations(cr.logical))
}

// registerAWXWebhook auto-registers a webhook notification template on the
// operator's AWX/AAP controller pointing at {PublicURL}/webhook/{trigger_id} and
// attaches it to the selected job (or workflow) template for every selected
// event.
//
// Idempotent: createTrigger runs on EVERY flow save, so an unchanged
// instance + template + event set is left completely untouched — the
// change-check happens before any HTTP round-trip. A changed one tears the old
// template down and builds a new one, because the callback is keyed on the stable
// trigger id and a stale template would double-fire the flow.
//
// Errors are logged, NEVER fatal: a registration failure must not fail the
// trigger upsert, or saving the flow itself breaks.
func (s *Service) registerAWXWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeAWXWebhook {
		return
	}
	cr, ok := s.resolveAwxCreds(tr)
	if !ok {
		log.WithField("trigger_id", tr.ID).Warn("awx trigger: missing AWX URL / credential / template; skipping webhook registration")
		return
	}
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since the last save, and we still hold a usable secret: keep
	// the existing template. Checked BEFORE any REST call — createTrigger runs on
	// every flow save and the common case must not cost a round-trip to AWX.
	state := s.loadAwxState(tr.ID)
	if awxStateCurrent(state, cr) {
		return
	}

	// The config changed (or the stored state is unusable): take the old template
	// down first. It is removed from the AWX it actually lives on — state.Base and
	// state.Root, not the freshly resolved ones — so that re-pointing the trigger
	// at a different controller does not orphan a template on the old one.
	if state != nil && state.TemplateID != "" {
		old := cr
		old.base = state.Base
		old.kind = state.Kind
		old.templateID = state.TemplateRefID
		root := state.Root
		if root == "" {
			root = awxCandidateRoots[0]
		}
		awxDetach(ctx, old, root, state.TemplateID, state.Attached)
		awxDeleteTemplate(ctx, old, root, state.TemplateID)
	}

	newState, err := awxRegister(ctx, cr, tr.ID, callback)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("awx trigger: could not register the notification template")
		return
	}

	if len(newState.Attached) < len(awxRelations(cr.logical)) {
		// Diagnosable at a glance rather than reconstructed from the per-relation
		// warnings above.
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"attached":   newState.Attached,
			"wanted":     awxRelations(cr.logical),
		}).Warn("awx trigger: partial registration — some events will not fire, and the next flow save will retry them")
	}

	stateJSON, err := json.Marshal(newState)
	if err == nil {
		err = s.db.UpsertTriggerState(tr.ID, awxStateKey, stateJSON)
	}
	if err != nil {
		// Without the persisted secret every delivery would 401 forever, and the
		// template id would be lost so it could never be cleaned up. Take the
		// template back out so the next save starts from a clean slate.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("awx trigger: unable to persist state; removing the notification template")
		awxDetach(ctx, cr, newState.Root, newState.TemplateID, newState.Attached)
		awxDeleteTemplate(ctx, cr, newState.Root, newState.TemplateID)
	}
}

// deregisterAWXWebhook detaches and then deletes the notification template we
// created for this trigger (best effort; logged, never fatal — a provider-side
// failure must never block the flow delete).
//
// Fires only from deleteTrigger — a DELETE /trigger/:id from the api when the
// trigger node is removed or the flow is deleted — and NOT on trigger disable. It
// runs BEFORE the trigger row goes away, which is exactly why the template id has
// to be in trigger_state: it cannot be derived once the row is gone.
func (s *Service) deregisterAWXWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeAWXWebhook {
		return
	}
	state := s.loadAwxState(tr.ID)
	if state == nil || state.TemplateID == "" {
		return
	}
	cr, ok := s.resolveAwxCreds(tr)
	if !ok {
		log.WithField("trigger_id", tr.ID).Warn("awx trigger: cannot resolve credentials to remove the notification template; it will be left behind in AWX")
		return
	}

	// Tear down against the AWX the template actually lives on, from the state, not
	// from a config that may since have been re-pointed elsewhere.
	cr.base = state.Base
	cr.kind = state.Kind
	cr.templateID = state.TemplateRefID
	root := state.Root
	if root == "" {
		root = awxCandidateRoots[0]
	}

	awxDetach(ctx, cr, root, state.TemplateID, state.Attached)
	awxDeleteTemplate(ctx, cr, root, state.TemplateID)

	if err := s.db.DeleteTriggerState(tr.ID, awxStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("awx trigger: unable to delete state")
	}
}

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

// awxVerifyBasic checks the Authorization header AWX presents on every delivery
// against the shared secret we minted at registration and stored in the
// notification template's (encrypted) password field.
//
// AWX cannot sign a payload — there is no signature API — so this Basic
// credential is the strongest authenticity check available, and the comparison is
// constant-time to close the timing oracle. It FAILS CLOSED: launch ALWAYS mints
// this secret at registration, so a missing one means the state was lost or the
// delivery is forged. (Unlike Jira/Monday, where an absent secret legitimately
// means "unsigned by design", there is no benign reading here.)
func awxVerifyBasic(header, secret string) bool {
	const prefix = "Basic "
	if secret == "" || len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return false
	}
	// Both halves in constant time, and both evaluated — no short-circuit that
	// would leak which one was wrong.
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(awxBasicUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(secret)) == 1
	return userOK && passOK
}

// awxLogicalForStatus maps an inbound payload's status back onto one of our four
// logical events, so a delivery that arrived through the shared `error` relation
// can be told apart: AWX fires that same relation for failed, errored AND
// canceled jobs.
func awxLogicalForStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "waiting", "running":
		return awxEventStarted
	case "successful":
		return awxEventSuccessful
	case "failed", "error":
		return awxEventFailed
	case "canceled", "cancelled":
		return awxEventCanceled
	}
	return ""
}

// awxInboundDecision is the whole authentication and filtering decision for an
// inbound AWX notification, as a pure function of the state we persisted at
// registration, the request's Authorization header and the raw body.
//
// It returns the status to answer with, and the event map to fire the flow with —
// a nil event means DO NOT FIRE, whatever the status. The three outcomes:
//
//   - (401, nil) — no state, no secret, or a bad/absent Basic credential.
//   - (200, nil) — authentic, but the operator did not subscribe to this event
//     (AWX's `error` relation carries failures AND cancellations, so a trigger
//     that only wants cancellations has to drop the failures itself). Acknowledged
//     so AWX does not log a delivery failure for something we ignored on purpose.
//   - (200, event) — fire.
func awxInboundDecision(state *awxWebhookState, authHeader string, body []byte) (int, map[string]interface{}) {
	if state == nil || state.Secret == "" {
		return http.StatusUnauthorized, nil
	}
	if !awxVerifyBasic(authHeader, state.Secret) {
		return http.StatusUnauthorized, nil
	}

	var payload map[string]interface{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return http.StatusBadRequest, nil
		}
	}

	status := awxEventStr(payload["status"])
	logical := awxLogicalForStatus(status)
	if logical == "" {
		// A status we do not model (a new AWX state, or a template someone attached
		// by hand). Acknowledged, not fired — guessing would be worse.
		return http.StatusOK, nil
	}

	subscribed := state.Logical
	if len(subscribed) == 0 {
		subscribed = awxAllEvents
	}
	if !awxContains(subscribed, logical) {
		return http.StatusOK, nil
	}

	return http.StatusOK, awxBuildEvent(payload, body, logical, status)
}

func awxContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// awxBuildEvent shapes the AWX job_metadata document into the trigger node's
// outputs.
func awxBuildEvent(payload map[string]interface{}, body []byte, logical, status string) map[string]interface{} {
	name := awxEventStr(payload["name"])
	jobID := awxEventStr(payload["id"])

	event := map[string]interface{}{
		"content":      awxSummary(name, jobID, logical, status),
		"event":        logical,
		"job_id":       jobID,
		"job_name":     name,
		"status":       status,
		"failed":       logical == awxEventFailed || logical == awxEventCanceled,
		"job_url":      awxEventStr(payload["url"]), // the AWX UI link, not the API path
		"created_by":   awxEventStr(payload["created_by"]),
		"started":      awxEventStr(payload["started"]),
		"finished":     awxEventStr(payload["finished"]), // null on a started event
		"traceback":    awxEventStr(payload["traceback"]),
		"inventory":    awxEventStr(payload["inventory"]),
		"project":      awxEventStr(payload["project"]),
		"playbook":     awxEventStr(payload["playbook"]),
		"limit":        awxEventStr(payload["limit"]),
		"extra_vars":   awxEventStr(payload["extra_vars"]), // a JSON *string*, not an object
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}
	if hosts, ok := payload["hosts"].(map[string]interface{}); ok {
		event["hosts"] = hosts
	}
	return event
}

// awxSummary is the human-readable one-liner the trigger node emits as `content`.
func awxSummary(name, jobID, logical, status string) string {
	if name == "" {
		name = "AWX job"
	}
	verb := map[string]string{
		awxEventStarted:    "started",
		awxEventSuccessful: "succeeded",
		awxEventFailed:     "failed",
		awxEventCanceled:   "was canceled",
	}[logical]
	if verb == "" {
		verb = "is " + status
	}
	if jobID == "" {
		return fmt.Sprintf("%s %s", name, verb)
	}
	return fmt.Sprintf("%s (job %s) %s", name, jobID, verb)
}

// awxDispatch fires the flow. It is a package-level variable purely so the
// inbound tests can observe a dispatch without standing up a trigger service; the
// production body is the one line below it.
var awxDispatch = func(s *Service, tr *launch.Trigger, event map[string]interface{}) {
	if err := s.trigger.Trigger(tr, event); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("unable to fire AWX webhook trigger")
	}
}

// handleAWXWebhook handles an inbound AWX notification. Called from handleWebhook
// once the trigger has been fetched and type-checked.
//
// ★ IT ANSWERS 200 IMMEDIATELY AND DOES EVERYTHING SLOW ON A GOROUTINE. AWX sets
// no HTTP timeout on a notification (python-requests blocks forever by default),
// so a slow ingest pins an AWX dispatcher worker for the full 1800s task timeout.
// The authentication decision is pure and instant; the optional callback
// verification — which costs a round-trip back to AWX — happens after the 200 has
// gone out, and simply declines to fire the flow if it fails. Nothing is lost by
// deferring it: AWX's delivery is AT-MOST-ONCE (it never retries, whatever we
// answer), so a 401 would have bought us nothing but a line in AWX's log at the
// cost of holding its worker open.
func (s *Service) handleAWXWebhook(c *gin.Context, tr *launch.Trigger) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	s.serveAWXWebhook(c, tr, s.loadAwxState(tr.ID), body)
}

// serveAWXWebhook is handleAWXWebhook with the trigger_state already loaded, so
// the authentication gate and the dispatch can be driven end to end in a test
// without a database.
func (s *Service) serveAWXWebhook(c *gin.Context, tr *launch.Trigger, state *awxWebhookState, body []byte) {
	status, event := awxInboundDecision(state, c.GetHeader("Authorization"), body)
	if event == nil {
		switch status {
		case http.StatusOK:
			// Authentic, but not an event this trigger subscribed to. Acknowledged so
			// AWX does not record a delivery failure for something we ignored on
			// purpose.
			c.Status(http.StatusOK)
		case http.StatusUnauthorized:
			log.WithField("id", tr.ID).Warn("AWX webhook: Basic credential verification failed")
			c.AbortWithStatus(status)
		default:
			c.AbortWithStatus(status)
		}
		return
	}

	// Carry __node_id so the executor injects the event data into the correct
	// trigger node in a multi-trigger flow.
	var triggerData map[string]interface{}
	_ = json.Unmarshal(tr.Data, &triggerData)
	if nodeID := asString(triggerData["__node_id"]); nodeID != "" {
		event["__node_id"] = nodeID
	}

	// The callback verification is defence in depth on top of the Basic credential,
	// and it costs a round-trip to AWX — so the credentials it needs are resolved
	// only when it is actually going to run. Whether it runs is read straight off
	// the trigger row we already hold, which costs nothing.
	verify := !awxBoolInput(tr, "skip_awx_verification")
	var cr awxCreds
	if verify {
		var ok bool
		if cr, ok = s.resolveAwxCreds(tr); !ok {
			// We cannot do the deeper check, but the delivery already presented the
			// Basic credential we minted, which is the authentication gate. Firing is
			// right: AWX delivery is at-most-once, so dropping the event over a
			// transient config-resolution blip would lose it for good.
			log.WithField("id", tr.ID).Warn("AWX webhook: cannot resolve credentials to confirm the notification against AWX; firing on the Basic credential alone")
			verify = false
		}
	}

	// Everything from here on is off the request goroutine. Note the fresh context:
	// gin cancels c.Request.Context() the moment the handler returns, so the
	// callback verification below would be cancelled before it ever connected.
	go func() {
		if verify {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := awxVerifyJob(ctx, cr, state, event); err != nil {
				log.WithFields(log.Fields{"id": tr.ID, "error": err}).
					Warn("AWX webhook: the notification could not be confirmed against AWX; not firing the flow")
				return
			}
		}
		awxDispatch(s, tr, event)
	}()

	c.Status(http.StatusOK)
}

// awxVerifyJob calls back to AWX with the node's own credential and confirms the
// notification told the truth. AWX's payload is a push we cannot cryptographically
// attribute — this is what turns it into a verified one, for the price of one GET.
// Controlled by the node's skip_awx_verification input (an opt-OUT, because the
// manifest does not harvest a boolean's default and a "verify, default true"
// checkbox would render unticked and silently mean false).
//
// It deliberately does NOT insist that a *started* notification still reads as
// running: a short playbook goes running -> successful in a few seconds, so by the
// time this callback lands the job has often already finished, and demanding an
// exact match would drop legitimate started events. For a started notification the
// job existing at all is the check. For a TERMINAL one — successful, failed,
// canceled — AWX must agree, and those are the ones a forger would want to fake.
func awxVerifyJob(ctx context.Context, cr awxCreds, state *awxWebhookState, event map[string]interface{}) error {
	jobID, _ := event["job_id"].(string)
	if jobID == "" {
		return errors.New("the notification carried no job id")
	}
	root := awxCandidateRoots[0]
	if state != nil && state.Root != "" {
		root = state.Root
	}

	// ★ PICK THE COLLECTION FROM THE TEMPLATE KIND. A workflow-template trigger
	// delivers a WORKFLOW job's id, and a workflow job lives at workflow_jobs/{id}/,
	// not jobs/{id}/. AWX's unified-job subclasses (Job, WorkflowJob, …) share ONE
	// globally-unique id sequence, so a GET against the wrong collection 404s on an
	// id that plainly exists — and this callback runs by default, so EVERY event of
	// a workflow-template trigger would be dropped as "AWX does not know job N". A
	// SLICED job template (job_slice_count > 1) also spawns a workflow job, so even a
	// job-template trigger can carry a workflow-job id; hence the 404 fallback to the
	// other collection before the job is finally declared missing.
	collections := []string{"jobs/", "workflow_jobs/"}
	if state != nil && state.Kind == awxKindWorkflowTemplate {
		collections = []string{"workflow_jobs/", "jobs/"}
	}

	var resp map[string]interface{}
	var status int
	for i, coll := range collections {
		var err error
		resp, status, err = awxDo(ctx, cr, root, http.MethodGet, coll+url.PathEscape(jobID)+"/", nil)
		if err != nil {
			return fmt.Errorf("could not reach AWX to confirm job %s: %s", jobID, awxRedact(cr, err.Error()))
		}
		// A 404 from this kind's own collection may just mean the job is the other
		// kind (a sliced job template spawns a workflow job); try the other before
		// giving up. Any non-404 — a success or a real error — is final.
		if status != http.StatusNotFound || i == len(collections)-1 {
			break
		}
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("AWX does not know job %s", jobID)
	}
	if status >= 300 {
		return fmt.Errorf("AWX returned HTTP %d confirming job %s", status, jobID)
	}

	logical, _ := event["event"].(string)
	if logical == awxEventStarted {
		return nil // the job exists; see the note above on the started race
	}

	claimed, _ := event["status"].(string)
	actual := awxEventStr(resp["status"])
	if !strings.EqualFold(actual, claimed) {
		return fmt.Errorf("job %s is %q at AWX but the notification claimed %q", jobID, actual, claimed)
	}
	return nil
}
