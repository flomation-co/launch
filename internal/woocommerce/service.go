// Package woocommerce holds the launch-side webhook helpers for WooCommerce
// triggers: signature verification, event parsing, and topic filtering. The
// registration lifecycle (creating/deleting the store's webhooks via the REST
// API) lives in internal/http/woocommerce.go alongside the other providers.
package woocommerce

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// VerifySignature validates a WooCommerce webhook delivery. The
// x-wc-webhook-signature header is the base64-encoded HMAC-SHA256 of the RAW
// request body, keyed with the secret we supplied when registering the webhook.
// Unlike GitHub (hex, "sha256=" prefix) WooCommerce uses bare base64, matching
// Shopify. The comparison is constant-time.
func VerifySignature(secret string, body []byte, r *http.Request) error {
	sig := r.Header.Get("x-wc-webhook-signature")
	if sig == "" {
		return fmt.Errorf("missing x-wc-webhook-signature header")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// ParseEvent builds the trigger's output data from a WooCommerce webhook. The
// topic, resource, event, webhook id, delivery id and source store all come
// from x-wc-webhook-* headers; the resource id is lifted from the JSON body.
// The raw body is preserved so downstream nodes can read any field.
func ParseEvent(r *http.Request, body []byte) (map[string]interface{}, error) {
	topic := r.Header.Get("x-wc-webhook-topic")
	// event_type and topic are deliberately the same value. "event_type" is the
	// canonical field every webhook trigger exposes for cross-integration flows
	// (and what MatchesFilter reads); "topic" is kept alongside it as the
	// WooCommerce-native name a flow author familiar with the docs will look for.
	data := map[string]interface{}{
		"event_type":   topic,
		"topic":        topic,
		"resource":     r.Header.Get("x-wc-webhook-resource"),
		"event":        r.Header.Get("x-wc-webhook-event"),
		"webhook_id":   r.Header.Get("x-wc-webhook-id"),
		"delivery_id":  r.Header.Get("x-wc-webhook-delivery-id"),
		"source":       r.Header.Get("x-wc-webhook-source"),
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}

	// The resource id is the top-level "id" of the payload (order/product/…).
	// WooCommerce sends it as a JSON number; render as an integer string. A
	// malformed/empty body isn't fatal — the headers still drive the flow.
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err == nil {
		if v, ok := raw["id"].(float64); ok {
			data["resource_id"] = fmt.Sprintf("%.0f", v)
		} else if v, ok := raw["id"].(string); ok {
			data["resource_id"] = v
		}
	}

	return data, nil
}

// MatchesFilter checks whether the WooCommerce topic matches the trigger's event
// selection. The selection arrives from resolveTriggerCreds as a plain string —
// either a JSON array (the multi-select's on-the-wire form, e.g.
// ["order.created","product.updated"]) or a comma-separated list. An empty
// selection matches all (the subscription is already topic-scoped; this filter
// also guards deliveries from a webhook created before a config change).
func MatchesFilter(topic, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	var events []string
	if strings.HasPrefix(filter, "[") {
		if json.Unmarshal([]byte(filter), &events) != nil {
			return true // unparseable selection → don't silently drop the event
		}
	} else {
		events = strings.Split(filter, ",")
	}
	for _, e := range events {
		if strings.TrimSpace(e) == topic {
			return true
		}
	}
	return false
}
