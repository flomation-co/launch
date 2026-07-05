package http

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	woocommercewh "flomation.app/automate/launch/internal/woocommerce"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// woocommerceHTTPClient is used for the outbound WooCommerce REST calls that
// register/deregister webhooks. Unlike the Calendly/Acuity clients (which call
// fixed provider hosts), the WooCommerce store URL is caller-supplied, so this
// client is SSRF-hardened the same way as the api's option proxies: the dialer
// refuses link-local and cloud-metadata destinations (169.254.169.254 et al) on
// the address actually dialed, and cross-host redirects are refused. Loopback
// and private LAN ranges stay allowed — self-hosted stores commonly live there.
var woocommerceHTTPClient = &http.Client{
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
				if isWooCloudMetadataIP(ip) {
					return errors.New("cloud metadata addresses are not allowed")
				}
				return nil
			},
		}).DialContext,
	},
}

// wooBlockedMetadataIPs are instance-metadata addresses outside the link-local
// range (AWS IPv6 IMDS and Alibaba's 100.100.100.200). Private RFC1918/ULA
// ranges are deliberately NOT blocked — a self-hosted store legitimately lives
// there. Mirrors the api option-proxy's list; keep the two in sync.
var wooBlockedMetadataIPs = []net.IP{
	net.ParseIP("fd00:ec2::254"),
	net.ParseIP("100.100.100.200"),
}

func isWooCloudMetadataIP(ip net.IP) bool {
	for _, b := range wooBlockedMetadataIPs {
		if b != nil && ip.Equal(b) {
			return true
		}
	}
	return false
}

// woocommerceAPIPath is the WooCommerce REST API v3 prefix appended to the
// store URL.
const woocommerceAPIPath = "/wp-json/wc/v3"

// woocommerceAllEvents is the default topic selection when the trigger config
// carries none (matching the Acuity/Calendly convention: empty = all).
var woocommerceAllEvents = []string{
	"order.created", "order.updated", "order.deleted",
	"product.created", "product.updated", "product.deleted",
	"customer.created", "customer.updated", "customer.deleted",
	"coupon.created", "coupon.updated", "coupon.deleted",
}

// woocommerceStateKey is the trigger_state key holding the webhooks + signing
// secret created for a woocommerce-webhook trigger.
const woocommerceStateKey = "woocommerce_webhook"

// wooReg is a single registered webhook (WooCommerce webhooks are one-topic
// each, so a multi-topic trigger owns several).
type wooReg struct {
	Topic string `json:"topic"`
	ID    string `json:"id"`
}

// woocommerceWebhookState is what we persist per trigger: the webhooks we
// created (so they can be removed on delete), the shared secret WooCommerce
// signs deliveries with, and the topic set they cover (so a config change is
// detected and the webhooks recreated).
type woocommerceWebhookState struct {
	Webhooks []wooReg `json:"webhooks"`
	Secret   string   `json:"secret"`
	Events   []string `json:"events"`
	// Base is the store the webhooks were created on. Persisted so that
	// re-pointing the trigger at a different store (same event set) is detected
	// and the webhooks are recreated on the new store, rather than silently
	// leaving them on the old one.
	Base string `json:"base"`
}

// wooCreds is the resolved connection for a WooCommerce trigger.
type wooCreds struct {
	base    string // normalised store URL (scheme+host[+path], no trailing slash)
	key     string
	secret  string
	inQuery bool
	events  []string
}

// resolveWooCreds pulls and normalises the trigger's WooCommerce connection.
// ok is false when a required part is missing.
func (s *Service) resolveWooCreds(tr *launch.Trigger) (wooCreds, bool) {
	c := s.resolveTriggerCreds(tr.ID)
	base := woocommerceBaseURL(c["url"])
	key := strings.TrimSpace(c["consumer_key"])
	secret := strings.TrimSpace(c["consumer_secret"])
	if base == "" || key == "" || secret == "" {
		return wooCreds{}, false
	}
	return wooCreds{
		base:    base,
		key:     key,
		secret:  secret,
		inQuery: wooCredsInQuery(tr),
		events:  woocommerceEvents(c["events"]),
	}, true
}

// wooCredsInQuery reads the credentials_in_query flag. It is a boolean node
// input, and resolveTriggerCreds only surfaces string values (a JSON bool is
// dropped), so it is read from the raw trigger data — tolerating either a JSON
// bool or a "true"/"false" string.
func wooCredsInQuery(tr *launch.Trigger) bool {
	var raw map[string]interface{}
	if json.Unmarshal(tr.Data, &raw) != nil {
		return false
	}
	switch v := raw["credentials_in_query"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// woocommerceBaseURL normalises a pasted store URL to scheme+host[+path] with no
// trailing slash and no REST-API suffix, defaulting to https. Returns "" when
// the value is blank or not an http(s) URL (e.g. an unresolved ${...} ref).
//
// NOTE: this logic is intentionally duplicated in the api
// (woocommerceOptionsBaseURL) and the executor (NormaliseBaseURL) because those
// are separate Go modules and there is no shared package. Keep the three in sync
// — any change to the suffix-stripping/scheme-defaulting here should be mirrored.
func woocommerceBaseURL(raw string) string {
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
	for _, suffix := range []string{woocommerceAPIPath, "/wp-json/wc/v2", "/wp-json/wc/v1", "/wp-json"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	u.User = nil
	return u.Scheme + "://" + u.Host + path
}

// woocommerceEvents parses the trigger's topic selection (a plain string from
// resolveTriggerCreds — comma-separated or a JSON array). Empty → all topics.
func woocommerceEvents(sel string) []string {
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
		return woocommerceAllEvents
	}
	return out
}

func woocommerceSameEventSet(a, b []string) bool {
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

// wooDo performs an authenticated WooCommerce REST request against the store's
// /wp-json/wc/v3 API. path starts with "/" (e.g. "/webhooks"). Credentials go in
// the Basic auth header, or the query string when the trigger opted in.
func wooDo(ctx context.Context, cr wooCreds, method, path string, body interface{}) (map[string]interface{}, int, error) {
	fullURL := cr.base + woocommerceAPIPath + path
	if cr.inQuery {
		sep := "?"
		if strings.Contains(fullURL, "?") {
			sep = "&"
		}
		q := url.Values{}
		q.Set("consumer_key", cr.key)
		q.Set("consumer_secret", cr.secret)
		fullURL += sep + q.Encode()
	}

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
	if !cr.inQuery {
		req.SetBasicAuth(cr.key, cr.secret)
	}
	req.Header.Set("Accept", "application/json")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := woocommerceHTTPClient.Do(req)
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
			log.WithFields(log.Fields{"status": resp.StatusCode, "body": b}).Warn("woocommerce: non-JSON response body")
		}
	}
	return out, resp.StatusCode, nil
}

// loadWooState reads the persisted webhooks + secret for a trigger. A missing
// row yields a nil state, not an error.
func (s *Service) loadWooState(triggerID string) *woocommerceWebhookState {
	rows, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": triggerID, "error": err}).Warn("woocommerce trigger: unable to load state")
		return nil
	}
	raw, ok := rows[woocommerceStateKey]
	if !ok {
		return nil
	}
	var st woocommerceWebhookState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

// deleteWooWebhooks removes the given webhooks from the store (best effort).
func deleteWooWebhooks(ctx context.Context, cr wooCreds, regs []wooReg) {
	for _, r := range regs {
		if r.ID == "" {
			continue
		}
		if _, status, err := wooDo(ctx, cr, http.MethodDelete, "/webhooks/"+r.ID+"?force=true", nil); err != nil || (status >= 300 && status != http.StatusNotFound) {
			log.WithFields(log.Fields{"webhook_id": r.ID, "status": status, "error": err}).Warn("woocommerce trigger: failed to delete webhook")
		}
	}
}

// registerWooCommerceWebhook auto-registers one WooCommerce webhook per selected
// topic pointing at {PublicURL}/webhook/{trigger_id} (WooCommerce webhooks are
// single-topic). All of a trigger's webhooks share one signing secret we
// generate, so the inbound handler verifies against a single value. Idempotent —
// createTrigger runs on every flow save, so an unchanged topic set is left
// untouched; a changed set recreates every webhook. Errors are logged, never
// fatal (they must not fail the trigger upsert).
func (s *Service) registerWooCommerceWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeWooCommerceWebhook {
		return
	}
	cr, ok := s.resolveWooCreds(tr)
	if !ok {
		log.WithField("trigger_id", tr.ID).Warn("woocommerce trigger: missing store URL / consumer key / secret; skipping webhook registration")
		return
	}
	callback := fmt.Sprintf("%s/webhook/%s", s.config.PublicURL, tr.ID)

	// Nothing changed since the last save (and we have a usable secret): keep the
	// existing webhooks. Checked before any REST call — createTrigger runs on
	// every flow save and the common case must not cost a round-trip.
	state := s.loadWooState(tr.ID)
	if state != nil && len(state.Webhooks) > 0 && state.Secret != "" && state.Base == cr.base && woocommerceSameEventSet(state.Events, cr.events) {
		return
	}

	// The topic set changed (or stored state is unusable): remove the old
	// webhooks before creating replacements. The callback is keyed on the stable
	// trigger ID, so stale webhooks would double-fire the flow.
	if state != nil && len(state.Webhooks) > 0 {
		deleteWooWebhooks(ctx, cr, state.Webhooks)
	}

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("woocommerce trigger: could not generate signing secret")
		return
	}
	secret := hex.EncodeToString(secretBytes)

	var regs []wooReg
	for _, topic := range cr.events {
		body := map[string]interface{}{
			"name":         "Flomation: " + topic,
			"topic":        topic,
			"delivery_url": callback,
			"secret":       secret,
			"status":       "active",
		}
		resp, status, err := wooDo(ctx, cr, http.MethodPost, "/webhooks", body)
		if err != nil || status >= 300 {
			log.WithFields(log.Fields{"trigger_id": tr.ID, "topic": topic, "status": status, "error": err, "response": resp}).Warn("failed to register WooCommerce webhook")
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
			log.WithFields(log.Fields{"trigger_id": tr.ID, "topic": topic}).Warn("woocommerce trigger: webhook created but no id returned")
			continue
		}
		regs = append(regs, wooReg{Topic: topic, ID: id})
	}
	if len(regs) == 0 {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "requested_topics": cr.events}).Warn("woocommerce trigger: no webhooks could be registered (all topics failed)")
		return
	}
	registeredTopics := make([]string, 0, len(regs))
	for _, r := range regs {
		registeredTopics = append(registeredTopics, r.Topic)
	}
	// Summary log when only some topics registered, so a partial failure is
	// diagnosable at a glance (which topics are live vs missing) rather than
	// having to reconstruct it from the per-topic warnings above.
	if len(regs) < len(cr.events) {
		log.WithFields(log.Fields{
			"trigger_id":       tr.ID,
			"registered":       registeredTopics,
			"registered_count": len(regs),
			"requested_count":  len(cr.events),
			"requested_topics": cr.events,
		}).Warn("woocommerce trigger: partial webhook registration — some topics failed and will not fire")
	}

	// Persist the topics that ACTUALLY registered (not the requested set), so a
	// partial registration self-heals: the next save's change-check sees
	// registered != requested and recreates the missing webhooks.
	stateJSON, _ := json.Marshal(woocommerceWebhookState{Webhooks: regs, Secret: secret, Events: registeredTopics, Base: cr.base})
	if err := s.db.UpsertTriggerState(tr.ID, woocommerceStateKey, stateJSON); err != nil {
		// Without the secret every delivery would be rejected; remove the
		// just-created webhooks so the next save retries cleanly.
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Error("woocommerce trigger: unable to persist state; removing webhooks")
		deleteWooWebhooks(ctx, cr, regs)
	}
}

// deregisterWooCommerceWebhook removes every webhook we registered for this
// trigger (best effort; logged, never fatal).
func (s *Service) deregisterWooCommerceWebhook(ctx context.Context, tr *launch.Trigger) {
	if tr == nil || tr.Type != launch.TriggerTypeWooCommerceWebhook {
		return
	}
	state := s.loadWooState(tr.ID)
	if state == nil || len(state.Webhooks) == 0 {
		return
	}
	cr, ok := s.resolveWooCreds(tr)
	if !ok {
		return
	}
	deleteWooWebhooks(ctx, cr, state.Webhooks)
	if err := s.db.DeleteTriggerState(tr.ID, woocommerceStateKey); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).Warn("woocommerce trigger: unable to delete state")
	}
}

// handleWooCommerceWebhook handles an inbound WooCommerce webhook for a trigger.
// WooCommerce signs the RAW body (base64 HMAC-SHA256 in x-wc-webhook-signature)
// with the secret we supplied at registration, so the body is read verbatim and
// verified before any parsing. Called from handleWebhook after the trigger has
// been fetched and type-checked.
func (s *Service) handleWooCommerceWebhook(c *gin.Context, tr *launch.Trigger) {
	id := tr.ID

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// WooCommerce sends an unsigned "ping" to the delivery URL when a webhook is
	// first activated; it carries no x-wc-webhook-signature. Acknowledge it with
	// 200 (so WooCommerce keeps the webhook active) without firing the flow.
	if c.Request.Header.Get("x-wc-webhook-signature") == "" {
		log.WithFields(log.Fields{"id": id}).Debug("WooCommerce webhook ping (no signature) acknowledged")
		c.Status(http.StatusOK)
		return
	}

	state := s.loadWooState(id)
	if state == nil || state.Secret == "" {
		log.WithFields(log.Fields{"id": id}).Warn("WooCommerce webhook has no signing secret on record")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if err := woocommercewh.VerifySignature(state.Secret, body, c.Request); err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Warn("WooCommerce webhook signature verification failed")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	data, err := woocommercewh.ParseEvent(c.Request, body)
	if err != nil {
		log.WithFields(log.Fields{"id": id, "error": err}).Error("WooCommerce webhook parse failed")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Topic filter against the topics we actually registered (state.Events, the
	// RESOLVED selection). The config field triggerData["events"] can hold an
	// unresolved ${...} reference — registration resolves it, so filtering on the
	// raw field would drop every delivery when a variable is used. The webhook
	// set is already topic-scoped; this is a guard for deliveries from a webhook
	// created before a config change. Empty registered set matches all.
	topic, _ := data["topic"].(string)
	if !woocommercewh.MatchesFilter(topic, strings.Join(state.Events, ",")) {
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
			log.WithFields(log.Fields{"error": err}).Error("unable to fire WooCommerce webhook trigger")
		}
	}()

	c.Status(http.StatusOK)
}
