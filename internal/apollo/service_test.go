package apollo

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyAndParse_SecretInQuery(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook/abc?secret=s3cr3t", strings.NewReader(`{"event_type":"contact.created","id":"e1"}`))
	payload, err := VerifyAndParse([]byte(`{"event_type":"contact.created","id":"e1"}`), r, "s3cr3t")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if payload["event_type"] != "contact.created" {
		t.Fatalf("payload not parsed: %v", payload)
	}
}

func TestVerifyAndParse_SecretInHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook/abc", strings.NewReader(`{}`))
	r.Header.Set(SecretHeader, "tok")
	if _, err := VerifyAndParse([]byte(`{}`), r, "tok"); err != nil {
		t.Fatalf("header secret should verify: %v", err)
	}
}

func TestVerifyAndParse_Mismatch(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook/abc?secret=wrong", strings.NewReader(`{}`))
	if _, err := VerifyAndParse([]byte(`{}`), r, "right"); err == nil {
		t.Fatal("mismatched secret must fail")
	}
}

func TestVerifyAndParse_MissingPresented(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook/abc", strings.NewReader(`{}`))
	if _, err := VerifyAndParse([]byte(`{}`), r, "right"); err == nil {
		t.Fatal("absent presented secret must fail")
	}
}

func TestVerifyAndParse_NoConfiguredSecret(t *testing.T) {
	// An empty configured secret must never accept a webhook, even if the
	// request also presents an empty secret (which would otherwise "match").
	r := httptest.NewRequest("POST", "/webhook/abc?secret=", strings.NewReader(`{}`))
	if _, err := VerifyAndParse([]byte(`{}`), r, ""); err == nil {
		t.Fatal("empty configured secret must fail closed")
	}
}

func TestVerifyAndParse_BadJSON(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook/abc?secret=t", strings.NewReader(`not json`))
	if _, err := VerifyAndParse([]byte(`not json`), r, "t"); err == nil {
		t.Fatal("invalid JSON body must fail")
	}
}

func TestEventToData(t *testing.T) {
	payload := map[string]interface{}{
		"event_type": "account.created",
		"id":         "evt_9",
		"account_id": "acc_1",
		"name":       "Acme",
		"count":      float64(3),
		"nested":     map[string]interface{}{"x": 1}, // must NOT be spread
	}
	raw := []byte(`{"event_type":"account.created"}`)
	data := EventToData(payload, raw)

	if data["event_type"] != "account.created" {
		t.Errorf("event_type = %v", data["event_type"])
	}
	if data["event_id"] != "evt_9" {
		t.Errorf("event_id = %v", data["event_id"])
	}
	if data["record_id"] != "acc_1" {
		t.Errorf("record_id = %v (should fall back to account_id)", data["record_id"])
	}
	if data["body"] != string(raw) {
		t.Errorf("body not set to raw")
	}
	if data["content"] != "Apollo event: account.created" {
		t.Errorf("content = %v", data["content"])
	}
	// scalar spread, object not spread
	if data["name"] != "Acme" || data["count"] != float64(3) {
		t.Errorf("scalars not spread: %v", data)
	}
	if _, ok := data["nested"]; ok {
		t.Errorf("non-scalar was spread")
	}
}

func TestMatchesFilter(t *testing.T) {
	if !MatchesFilter("contact.created", "") {
		t.Error("empty filter should match all")
	}
	if !MatchesFilter("contact.created", "account.created, contact.created") {
		t.Error("listed event should match")
	}
	if MatchesFilter("deal.updated", "account.created,contact.created") {
		t.Error("unlisted event should not match")
	}
}
