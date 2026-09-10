// Package heygen verifies and parses inbound HeyGen webhook events for the
// HeyGen Webhook Trigger.
//
// SECURITY: HeyGen's signing secret is scoped to a webhook *endpoint*
// registered via its API, whereas this trigger receives events at the per-video
// callback_url the author passes to a generate action. Rather than depend on
// that (endpoint-scoped, header/algorithm-unconfirmed) signature, authenticity
// rests on the same model as the Apollo trigger: the unguessable /webhook/:id
// route plus an author-chosen shared secret presented on the request (query
// param `secret` or the X-Flomation-Webhook-Secret header) and compared in
// constant time. A future hardening step is to re-fetch the referenced video
// from HeyGen's API before firing.
package heygen

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SecretHeader is the fallback header carrying the shared secret when it is not
// supplied as the `secret` query parameter.
const SecretHeader = "X-Flomation-Webhook-Secret" // #nosec G101 — HTTP header name, not a credential

// VerifyAndParse checks the presented shared secret against the configured one
// in constant time, then parses the JSON body. A missing/empty configured
// secret is treated as a failure (never accept an unauthenticated webhook).
func VerifyAndParse(body []byte, r *http.Request, secret string) (map[string]interface{}, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("no webhook secret configured")
	}

	presented := r.URL.Query().Get("secret")
	if presented == "" {
		presented = r.Header.Get(SecretHeader)
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
		return nil, fmt.Errorf("webhook secret mismatch")
	}

	payload := map[string]interface{}{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
	}
	return payload, nil
}

// EventToData flattens a HeyGen webhook payload into the map the executor's
// HeyGen Webhook Trigger surfaces as outputs. HeyGen wraps the specifics in an
// `event_data` object (e.g. {"event_type":"avatar_video.success",
// "event_data":{"video_id":"…","url":"…","callback_id":"…"}}), so the
// well-known identifying fields are pulled up to stable keys and every scalar
// (top-level and inside event_data) is also spread as a bare key.
func EventToData(payload map[string]interface{}, rawBody []byte) map[string]interface{} {
	eventType := firstNonEmpty(str(payload["event_type"]), str(payload["type"]))
	eventData, _ := payload["event_data"].(map[string]interface{})

	pick := func(key string) string {
		if v := str(payload[key]); v != "" {
			return v
		}
		if eventData != nil {
			return str(eventData[key])
		}
		return ""
	}

	data := map[string]interface{}{
		"event_type":   eventType,
		"video_id":     firstNonEmpty(pick("video_id"), pick("video_translate_id")),
		"video_url":    firstNonEmpty(pick("url"), pick("video_url")),
		"callback_id":  pick("callback_id"),
		"status":       pick("status"),
		"body":         string(rawBody),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
		"content":      "HeyGen event: " + eventType,
	}

	// Spread scalar keys (event_data first, then top-level) as bare outputs
	// without clobbering the stable keys above.
	spread := func(m map[string]interface{}) {
		for k, v := range m {
			if _, exists := data[k]; exists {
				continue
			}
			switch v.(type) {
			case string, float64, bool:
				data[k] = v
			}
		}
	}
	spread(eventData)
	spread(payload)
	return data
}

// MatchesFilter reports whether eventType is allowed by a comma-separated filter
// (e.g. "avatar_video.success,avatar_video.fail"). Empty filter = all.
func MatchesFilter(eventType, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	for _, f := range strings.Split(filter, ",") {
		if strings.EqualFold(strings.TrimSpace(f), eventType) {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func str(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
