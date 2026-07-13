package quickbooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sign(token string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	token := "verifier-abc"
	body := []byte(`{"eventNotifications":[]}`)
	r := httptest.NewRequest(http.MethodPost, "/webhook/x", nil)
	r.Header.Set("intuit-signature", sign(token, body))
	if err := VerifySignature(token, body, r); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	// Wrong token → rejected.
	if err := VerifySignature("nope", body, r); err == nil {
		t.Fatal("expected invalid signature to be rejected")
	}

	// Missing header → rejected.
	r2 := httptest.NewRequest(http.MethodPost, "/webhook/x", nil)
	if err := VerifySignature(token, body, r2); err == nil {
		t.Fatal("expected missing header to be rejected")
	}
}

func TestParseEvent(t *testing.T) {
	body := []byte(`{"eventNotifications":[{"realmId":"9130347","dataChangeEvent":{"entities":[{"name":"Customer","id":"42","operation":"Create","lastUpdated":"2026-07-10T10:00:00Z"}]}}]}`)
	data, err := ParseEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if data["realm_id"] != "9130347" || data["entity"] != "Customer" || data["entity_id"] != "42" || data["operation"] != "Create" {
		t.Fatalf("unexpected parse: %+v", data)
	}
	if data["event_type"] != "Customer.Create" {
		t.Fatalf("event_type: %v", data["event_type"])
	}
}

func TestMatchesFilter(t *testing.T) {
	cases := []struct {
		event, filter string
		want          bool
	}{
		{"Customer.Create", "", true},
		{"Customer.Create", "Customer.Create", true},
		{"Customer.Create", "Invoice.Create,Customer.Create", true},
		{"Customer.Create", "Customer", true}, // entity-only filter
		{"Customer.Create", "Invoice", false},
		{"Customer.Update", "Customer.Create", false},
	}
	for _, c := range cases {
		if got := MatchesFilter(c.event, c.filter); got != c.want {
			t.Errorf("MatchesFilter(%q,%q)=%v want %v", c.event, c.filter, got, c.want)
		}
	}
}
