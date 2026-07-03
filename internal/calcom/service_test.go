package calcom

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"triggerEvent":"BOOKING_CREATED"}`)
	secret := "s3cr3t"

	req := httptest.NewRequest(http.MethodPost, "/webhook/x", nil)
	req.Header.Set("X-Cal-Signature-256", signBody(secret, body))
	if err := VerifySignature(secret, body, req); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	// Wrong signature is rejected.
	req.Header.Set("X-Cal-Signature-256", "deadbeef")
	if err := VerifySignature(secret, body, req); err == nil {
		t.Fatal("bad signature accepted")
	}

	// Missing header with a secret set is rejected.
	req2 := httptest.NewRequest(http.MethodPost, "/webhook/x", nil)
	if err := VerifySignature(secret, body, req2); err == nil {
		t.Fatal("missing signature accepted")
	}

	// No secret on record → verification skipped.
	if err := VerifySignature("", body, req2); err != nil {
		t.Fatalf("secretless webhook should be accepted: %v", err)
	}
}

func TestParseEventStandardEnvelope(t *testing.T) {
	body := []byte(`{
		"triggerEvent":"BOOKING_CREATED",
		"createdAt":"2026-07-03T10:00:00.000Z",
		"payload":{
			"uid":"bk_123",
			"startTime":"2026-07-10T09:00:00Z",
			"attendees":[{"name":"Ada Lovelace","email":"ada@example.com"}]
		}
	}`)
	data, err := ParseEvent(body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if data["event_type"] != "BOOKING_CREATED" || data["event"] != "BOOKING_CREATED" {
		t.Fatalf("event fields wrong: %#v", data["event_type"])
	}
	if data["booking_uid"] != "bk_123" {
		t.Fatalf("booking_uid wrong: %v", data["booking_uid"])
	}
	if data["start_time"] != "2026-07-10T09:00:00Z" {
		t.Fatalf("start_time wrong: %v", data["start_time"])
	}
	if data["attendee_name"] != "Ada Lovelace" || data["attendee_email"] != "ada@example.com" {
		t.Fatalf("attendee fields wrong: %#v / %#v", data["attendee_name"], data["attendee_email"])
	}
}

// MEETING_ENDED delivers a flat body with no "payload" wrapper.
func TestParseEventFlatMeetingEnded(t *testing.T) {
	body := []byte(`{
		"triggerEvent":"MEETING_ENDED",
		"createdAt":"2026-07-10T09:30:00.000Z",
		"uid":"bk_999",
		"attendees":[{"name":"Bob","email":"bob@example.com"}]
	}`)
	data, err := ParseEvent(body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if data["event_type"] != "MEETING_ENDED" {
		t.Fatalf("event_type wrong: %v", data["event_type"])
	}
	if data["booking_uid"] != "bk_999" {
		t.Fatalf("flat booking_uid not lifted: %v", data["booking_uid"])
	}
	if data["attendee_email"] != "bob@example.com" {
		t.Fatalf("flat attendee not lifted: %v", data["attendee_email"])
	}
}

func TestMatchesFilter(t *testing.T) {
	if !MatchesFilter("BOOKING_CREATED", "") {
		t.Fatal("empty filter should match all")
	}
	if !MatchesFilter("BOOKING_CREATED", "BOOKING_CREATED,BOOKING_CANCELLED") {
		t.Fatal("expected match")
	}
	if MatchesFilter("MEETING_ENDED", "BOOKING_CREATED,BOOKING_CANCELLED") {
		t.Fatal("unexpected match")
	}
}
