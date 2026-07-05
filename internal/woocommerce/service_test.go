package woocommerce

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sign computes the base64 HMAC-SHA256 the way WooCommerce does, for fixtures.
func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func reqWith(sig, topic, resource, event, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/x", strings.NewReader(body))
	if sig != "" {
		r.Header.Set("x-wc-webhook-signature", sig)
	}
	if topic != "" {
		r.Header.Set("x-wc-webhook-topic", topic)
	}
	if resource != "" {
		r.Header.Set("x-wc-webhook-resource", resource)
	}
	if event != "" {
		r.Header.Set("x-wc-webhook-event", event)
	}
	r.Header.Set("x-wc-webhook-id", "12")
	r.Header.Set("x-wc-webhook-delivery-id", "d-99")
	r.Header.Set("x-wc-webhook-source", "https://store.example.com/")
	return r
}

func TestVerifySignature(t *testing.T) {
	secret := "abc123secret"
	body := `{"id":725,"status":"processing"}`

	if err := VerifySignature(secret, []byte(body), reqWith(sign(secret, body), "order.created", "order", "created", body)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySignature("wrong", []byte(body), reqWith(sign(secret, body), "order.created", "order", "created", body)); err == nil {
		t.Fatal("expected rejection for wrong secret")
	}
	// Tampered body fails.
	if err := VerifySignature(secret, []byte(body+" "), reqWith(sign(secret, body), "order.created", "order", "created", body)); err == nil {
		t.Fatal("expected rejection for tampered body")
	}
	// Missing signature header fails.
	if err := VerifySignature(secret, []byte(body), reqWith("", "order.created", "order", "created", body)); err == nil {
		t.Fatal("expected rejection for missing signature header")
	}
	// A hex signature (GitHub-style) must NOT pass — WooCommerce is base64.
	hexSig := "abcdef0123456789"
	if err := VerifySignature(secret, []byte(body), reqWith(hexSig, "order.created", "order", "created", body)); err == nil {
		t.Fatal("expected rejection for hex-encoded signature")
	}
}

func TestParseEvent(t *testing.T) {
	body := `{"id":725,"status":"processing"}`
	data, err := ParseEvent(reqWith("sig", "order.created", "order", "created", body), []byte(body))
	if err != nil {
		t.Fatalf("ParseEvent error: %v", err)
	}
	checks := map[string]string{
		"event_type":  "order.created",
		"topic":       "order.created",
		"resource":    "order",
		"event":       "created",
		"webhook_id":  "12",
		"delivery_id": "d-99",
		"source":      "https://store.example.com/",
		"resource_id": "725",
		"body":        body,
	}
	for k, want := range checks {
		if got, _ := data[k].(string); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := data["triggered_at"].(string); !ok {
		t.Error("triggered_at missing")
	}

	// String id and a malformed body are both tolerated.
	d2, _ := ParseEvent(reqWith("sig", "product.updated", "product", "updated", ""), []byte(`{"id":"gid://x"}`))
	if d2["resource_id"] != "gid://x" {
		t.Errorf("string resource_id not carried: %v", d2["resource_id"])
	}
	d3, _ := ParseEvent(reqWith("sig", "coupon.deleted", "coupon", "deleted", ""), []byte(`not json`))
	if _, present := d3["resource_id"]; present {
		t.Errorf("resource_id should be absent for a malformed body, got %v", d3["resource_id"])
	}
}

func TestMatchesFilter(t *testing.T) {
	cases := []struct {
		topic, filter string
		want          bool
	}{
		{"order.created", "", true},                                     // empty → all
		{"order.created", `["order.created","product.updated"]`, true},  // JSON-array form
		{"order.updated", `["order.created","product.updated"]`, false}, // not selected
		{"product.updated", "order.created,product.updated", true},      // CSV form
		{"coupon.created", "order.created, coupon.created ", true},      // CSV with spaces
		{"order.deleted", "order.created,product.updated", false},       // CSV, not selected
		{"coupon.deleted", `[oops not json`, true},                      // malformed JSON array → don't drop
	}
	for _, c := range cases {
		if got := MatchesFilter(c.topic, c.filter); got != c.want {
			t.Errorf("MatchesFilter(%q, %q) = %v, want %v", c.topic, c.filter, got, c.want)
		}
	}
}
