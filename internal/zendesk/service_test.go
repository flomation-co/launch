package zendesk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthHeader(t *testing.T) {
	if got := AuthHeader("agent@acme.com", "tok", ""); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("agent@acme.com/token:tok")) {
		t.Fatalf("basic auth header mismatch: %q", got)
	}
	if got := AuthHeader("agent@acme.com", "tok", "bearer-x"); got != "Bearer bearer-x" {
		t.Fatalf("bearer should win: %q", got)
	}
	if got := AuthHeader("", "", ""); got != "" {
		t.Fatalf("expected empty header, got %q", got)
	}
}

func signBody(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"ticket_id":"42"}`)
	ts := "1700000000"

	r := httptest.NewRequest(http.MethodPost, "/webhook/abc", nil)
	r.Header.Set(signatureTimestampHeader, ts)
	r.Header.Set(signatureHeader, signBody(secret, ts, body))
	if err := VerifySignature(secret, body, r); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	// Tampered body must fail.
	if err := VerifySignature(secret, []byte(`{"ticket_id":"99"}`), r); err == nil {
		t.Fatal("expected tampered body to fail verification")
	}

	// Missing headers must fail.
	bare := httptest.NewRequest(http.MethodPost, "/webhook/abc", nil)
	if err := VerifySignature(secret, body, bare); err == nil {
		t.Fatal("expected missing signature header to fail")
	}
}

func TestParseEvent(t *testing.T) {
	body := []byte(`{"ticket_id":"42","subject":"Help","status":"open","priority":"high","requester_email":"a@b.com","description":"hi","via":"Web"}`)
	data, err := ParseEvent(body)
	if err != nil {
		t.Fatalf("ParseEvent errored: %v", err)
	}
	if data["ticket_id"] != "42" || data["subject"] != "Help" || data["status"] != "open" {
		t.Fatalf("lifted fields wrong: %#v", data)
	}
	if data["body"] != string(body) {
		t.Fatal("raw body not preserved")
	}
	if _, ok := data["payload"].(map[string]interface{}); !ok {
		t.Fatal("payload not set")
	}
	if data["triggered_at"] == "" {
		t.Fatal("triggered_at not set")
	}

	// Non-JSON body is tolerated (raw body still carried).
	data, err = ParseEvent([]byte("not json"))
	if err != nil {
		t.Fatalf("non-JSON body should not error: %v", err)
	}
	if data["body"] != "not json" {
		t.Fatal("raw non-JSON body not preserved")
	}
}
