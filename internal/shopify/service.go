package shopify

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

// VerifySignature validates a Shopify webhook. The X-Shopify-Hmac-Sha256 header
// is the base64-encoded HMAC-SHA256 of the RAW request body, keyed with the
// app's API secret key. Unlike GitHub (hex, "sha256=" prefix) Shopify uses bare
// base64. The comparison is constant-time.
func VerifySignature(appSecret string, body []byte, r *http.Request) error {
	sig := r.Header.Get("X-Shopify-Hmac-Sha256")
	if sig == "" {
		return fmt.Errorf("missing X-Shopify-Hmac-Sha256 header")
	}

	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// ParseEvent builds the trigger's output data from a Shopify webhook. The
// topic, shop domain, API version, and webhook id all come from X-Shopify-*
// headers; the resource id is lifted from the JSON body. The raw body is
// preserved so downstream nodes can read any field.
func ParseEvent(r *http.Request, body []byte) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"event_type":   r.Header.Get("X-Shopify-Topic"), // used by MatchesFilter
		"topic":        r.Header.Get("X-Shopify-Topic"),
		"shop_domain":  r.Header.Get("X-Shopify-Shop-Domain"),
		"api_version":  r.Header.Get("X-Shopify-API-Version"),
		"webhook_id":   r.Header.Get("X-Shopify-Webhook-Id"),
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}

	// The resource id is the top-level "id" of the webhook payload (order id,
	// product id, ...). Shopify sends it as a JSON number; render as an integer
	// string. A malformed/empty body isn't fatal — headers still drive the flow.
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

// MatchesFilter checks whether the Shopify topic matches the comma-separated
// filter (e.g. "orders/create,products/update"). An empty filter matches all.
func MatchesFilter(topic, filter string) bool {
	if strings.TrimSpace(filter) == "" {
		return true
	}
	for _, f := range strings.Split(filter, ",") {
		if strings.TrimSpace(f) == topic {
			return true
		}
	}
	return false
}
