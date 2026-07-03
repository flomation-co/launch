package acuity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sign(apiKey string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := []byte("action=appointment.scheduled&id=123")
	key := "s3cr3t"

	req := httptest.NewRequest(http.MethodPost, "/webhook/x", nil)
	req.Header.Set("X-Acuity-Signature", sign(key, body))
	if err := VerifySignature(key, body, req); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	req.Header.Set("X-Acuity-Signature", "bogus")
	if err := VerifySignature(key, body, req); err == nil {
		t.Fatal("bad signature accepted")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/webhook/x", nil)
	if err := VerifySignature(key, body, req2); err == nil {
		t.Fatal("missing signature accepted")
	}
}

func TestParseEventFormEncoded(t *testing.T) {
	body := []byte("action=appointment.rescheduled&id=456&calendarID=7&appointmentTypeID=8")
	data, err := ParseEvent(body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if data["event_type"] != "appointment.rescheduled" || data["event"] != "appointment.rescheduled" {
		t.Fatalf("event wrong: %#v", data["event_type"])
	}
	if data["object_id"] != "456" {
		t.Fatalf("object_id wrong: %v", data["object_id"])
	}
	if data["calendar_id"] != "7" || data["appointment_type_id"] != "8" {
		t.Fatalf("id fields wrong: %#v / %#v", data["calendar_id"], data["appointment_type_id"])
	}
}

func TestResourcePath(t *testing.T) {
	if got := ResourcePath("appointment.scheduled", "123"); got != "/appointments/123" {
		t.Fatalf("appointment path wrong: %s", got)
	}
	if got := ResourcePath("order.completed", "9"); got != "/orders/9" {
		t.Fatalf("order path wrong: %s", got)
	}
	if got := ResourcePath("appointment.scheduled", ""); got != "" {
		t.Fatalf("empty id should yield empty path, got %s", got)
	}
	if got := ResourcePath("unknown.thing", "1"); got != "" {
		t.Fatalf("unknown family should yield empty path, got %s", got)
	}
}

func TestMatchesFilter(t *testing.T) {
	if !MatchesFilter("appointment.scheduled", "") {
		t.Fatal("empty filter should match all")
	}
	if !MatchesFilter("appointment.scheduled", "appointment.scheduled,order.completed") {
		t.Fatal("expected match")
	}
	if MatchesFilter("appointment.canceled", "appointment.scheduled,order.completed") {
		t.Fatal("unexpected match")
	}
}
