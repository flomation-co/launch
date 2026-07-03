package shopify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sign computes the base64 HMAC-SHA256 the way Shopify does, for test fixtures.
func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func reqWith(hmacHeader, topic, shop string, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/x", strings.NewReader(body))
	if hmacHeader != "" {
		r.Header.Set("X-Shopify-Hmac-Sha256", hmacHeader)
	}
	if topic != "" {
		r.Header.Set("X-Shopify-Topic", topic)
	}
	if shop != "" {
		r.Header.Set("X-Shopify-Shop-Domain", shop)
	}
	r.Header.Set("X-Shopify-API-Version", "2025-01")
	r.Header.Set("X-Shopify-Webhook-Id", "wh-123")
	return r
}

func TestVerifySignature(t *testing.T) {
	secret := "shpss_appsecret"
	body := `{"id":450789469,"email":"jane@example.com"}`

	// Valid signature passes.
	if err := VerifySignature(secret, []byte(body), reqWith(sign(secret, body), "orders/create", "s.myshopify.com", body)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	// Wrong secret fails.
	if err := VerifySignature("wrong", []byte(body), reqWith(sign(secret, body), "orders/create", "s", body)); err == nil {
		t.Fatal("expected rejection for wrong secret")
	}

	// Tampered body fails (signature computed over original, body changed).
	if err := VerifySignature(secret, []byte(body+" "), reqWith(sign(secret, body), "orders/create", "s", body)); err == nil {
		t.Fatal("expected rejection for tampered body")
	}

	// Missing header fails.
	if err := VerifySignature(secret, []byte(body), reqWith("", "orders/create", "s", body)); err == nil {
		t.Fatal("expected rejection for missing HMAC header")
	}

	// A hex signature (GitHub-style) must NOT pass — Shopify is base64.
	hexSig := "sha256=deadbeef"
	if err := VerifySignature(secret, []byte(body), reqWith(hexSig, "orders/create", "s", body)); err == nil {
		t.Fatal("expected rejection for hex-format signature")
	}
}

func TestParseEvent(t *testing.T) {
	body := `{"id":450789469,"email":"jane@example.com"}`
	data, err := ParseEvent(reqWith("sig", "orders/create", "flomation-dev.myshopify.com", body), []byte(body))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	for k, want := range map[string]string{
		"topic":       "orders/create",
		"event_type":  "orders/create",
		"shop_domain": "flomation-dev.myshopify.com",
		"api_version": "2025-01",
		"webhook_id":  "wh-123",
		"resource_id": "450789469",
		"body":        body,
	} {
		if got, _ := data[k].(string); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if data["triggered_at"] == nil {
		t.Error("triggered_at missing")
	}

	// A non-JSON body still yields header-driven data (no resource_id).
	data, err = ParseEvent(reqWith("sig", "app/uninstalled", "s", "not json"), []byte("not json"))
	if err != nil {
		t.Fatalf("parse of non-JSON body errored: %v", err)
	}
	if _, ok := data["resource_id"]; ok {
		t.Error("resource_id should be absent for a non-JSON body")
	}
	if data["topic"] != "app/uninstalled" {
		t.Error("topic should still come from the header")
	}
}

func TestMatchesFilter(t *testing.T) {
	if !MatchesFilter("orders/create", "") {
		t.Error("empty filter should match all")
	}
	if !MatchesFilter("orders/create", "products/update, orders/create") {
		t.Error("topic in list should match")
	}
	if MatchesFilter("orders/delete", "orders/create,products/update") {
		t.Error("topic not in list should not match")
	}
}
