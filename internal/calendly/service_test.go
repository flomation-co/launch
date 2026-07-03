package calendly

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
)

// sign computes the expected header signature. It concatenates timestamp+"."
// in one Write where VerifySignature uses two — deliberately, as HMAC is
// stream-based and chunking doesn't affect the digest; the asymmetry also
// guards against both sides sharing a chunking mistake.
func sign(key, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func requestWithSignature(header string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/webhook/x", nil)
	if header != "" {
		r.Header.Set("Calendly-Webhook-Signature", header)
	}
	return r
}

func TestVerifySignatureValid(t *testing.T) {
	body := []byte(`{"event":"invitee.created"}`)
	key := "signing-key"
	ts := "1720000000"
	header := fmt.Sprintf("t=%s,v1=%s", ts, sign(key, ts, body))

	if err := VerifySignature(key, body, requestWithSignature(header)); err != nil {
		t.Fatalf("expected valid signature, got %v", err)
	}
}

func TestVerifySignatureRejectsTamperedBody(t *testing.T) {
	key := "signing-key"
	ts := "1720000000"
	header := fmt.Sprintf("t=%s,v1=%s", ts, sign(key, ts, []byte(`{"event":"invitee.created"}`)))

	err := VerifySignature(key, []byte(`{"event":"invitee.canceled"}`), requestWithSignature(header))
	if err == nil {
		t.Fatal("expected signature mismatch")
	}
}

func TestVerifySignatureRejectsWrongKey(t *testing.T) {
	body := []byte(`{}`)
	ts := "1720000000"
	header := fmt.Sprintf("t=%s,v1=%s", ts, sign("other-key", ts, body))

	if err := VerifySignature("signing-key", body, requestWithSignature(header)); err == nil {
		t.Fatal("expected signature mismatch")
	}
}

func TestVerifySignatureMissingOrMalformedHeader(t *testing.T) {
	if err := VerifySignature("k", []byte(`{}`), requestWithSignature("")); err == nil {
		t.Fatal("expected error for missing header")
	}
	if err := VerifySignature("k", []byte(`{}`), requestWithSignature("garbage")); err == nil {
		t.Fatal("expected error for malformed header")
	}
}

func TestParseEvent(t *testing.T) {
	body := []byte(`{
		"created_at": "2026-07-03T10:00:00.000000Z",
		"created_by": "https://api.calendly.com/users/U1",
		"event": "invitee.created",
		"payload": {
			"name": "Jane Doe",
			"email": "jane@example.com",
			"scheduled_event": {"start_time": "2026-07-04T09:00:00Z"}
		}
	}`)
	data, err := ParseEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if data["event_type"] != "invitee.created" || data["event"] != "invitee.created" {
		t.Fatalf("event not parsed: %v", data)
	}
	if data["invitee_name"] != "Jane Doe" || data["invitee_email"] != "jane@example.com" {
		t.Fatalf("invitee not lifted: %v", data)
	}
	if data["event_start_time"] != "2026-07-04T09:00:00Z" {
		t.Fatalf("start time not lifted: %v", data)
	}
	if data["body"] == "" || data["triggered_at"] == "" {
		t.Fatalf("raw body / triggered_at missing: %v", data)
	}
}

func TestParseEventMalformed(t *testing.T) {
	if _, err := ParseEvent([]byte(`{not json`)); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestMatchesFilter(t *testing.T) {
	if !MatchesFilter("invitee.created", "") {
		t.Fatal("empty filter must match all")
	}
	if !MatchesFilter("invitee.created", "invitee.created,invitee.canceled") {
		t.Fatal("expected match")
	}
	if MatchesFilter("routing_form_submission.created", "invitee.created") {
		t.Fatal("expected no match")
	}
	if !MatchesFilter("invitee.canceled", " invitee.created , invitee.canceled ") {
		t.Fatal("expected match with whitespace")
	}
}
