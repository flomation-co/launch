package jotform

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"
)

// multipartRequest builds an *http.Request carrying a JotForm-style
// multipart/form-data body with the given fields.
func multipartRequest(fields map[string]string) *http.Request {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	_ = w.Close()

	r, _ := http.NewRequest(http.MethodPost, "/webhook/abc", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func sampleFields() map[string]string {
	return map[string]string{
		"formID":       "2519087654321",
		"submissionID": "5981234567890",
		"rawRequest":   `{"q1_name":"Ada Lovelace","q2_email":"ada@example.com"}`,
		"type":         "WEB",
	}
}

func TestParseMultipart(t *testing.T) {
	r := multipartRequest(sampleFields())

	ev, err := ParseMultipart(r)
	if err != nil {
		t.Fatalf("expected valid multipart parse, got error: %v", err)
	}
	if ev.FormID != "2519087654321" {
		t.Errorf("expected form id 2519087654321, got %q", ev.FormID)
	}
	if ev.SubmissionID != "5981234567890" {
		t.Errorf("expected submission id 5981234567890, got %q", ev.SubmissionID)
	}
	if ev.Type != "WEB" {
		t.Errorf("expected type WEB, got %q", ev.Type)
	}
	if ev.RawRequest == "" {
		t.Error("expected rawRequest to be populated")
	}
}

func TestParseMultipart_NotMultipart(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/webhook/abc", bytes.NewBufferString("not-multipart"))
	r.Header.Set("Content-Type", "application/json")

	if _, err := ParseMultipart(r); err == nil {
		t.Fatal("expected error parsing a non-multipart body, got nil")
	}
}

func TestEventToData(t *testing.T) {
	r := multipartRequest(sampleFields())
	ev, err := ParseMultipart(r)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	data := EventToData(ev)

	checks := map[string]string{
		"event_type":    "WEB",
		"form_id":       "2519087654321",
		"submission_id": "5981234567890",
		"content":       "JotForm submission for form 2519087654321",
	}
	for k, want := range checks {
		if got, _ := data[k].(string); got != want {
			t.Errorf("EventToData[%q] = %q, want %q", k, got, want)
		}
	}

	answers, _ := data["answers"].(string)
	if answers == "" {
		t.Error("expected answers to be the rawRequest JSON string")
	}
	if body, _ := data["body"].(string); body != answers {
		t.Errorf("expected body to equal answers, got body=%q answers=%q", body, answers)
	}
}

func TestEventToData_DefaultEventType(t *testing.T) {
	data := EventToData(Event{FormID: "abc"})
	if got := data["event_type"].(string); got != "submission" {
		t.Errorf("expected empty type to default to \"submission\", got %q", got)
	}
}

func TestMatchesFilter(t *testing.T) {
	cases := []struct {
		formID string
		filter string
		want   bool
	}{
		{"abc123", "", true},         // empty = all
		{"abc123", "abc123", true},   // exact
		{"abc123", " abc123 ", true}, // whitespace tolerant
		{"abc123", "other", false},   // mismatch
	}
	for _, c := range cases {
		if got := MatchesFilter(c.formID, c.filter); got != c.want {
			t.Errorf("MatchesFilter(%q, %q) = %v, want %v", c.formID, c.filter, got, c.want)
		}
	}
}

func TestTokenOK(t *testing.T) {
	cases := []struct {
		name     string
		secret   string
		provided string
		want     bool
	}{
		{"no secret configured accepts anything", "", "", true},
		{"no secret configured ignores provided token", "", "whatever", true},
		{"matching token accepts", "s3cr3t", "s3cr3t", true},
		{"mismatched token rejects", "s3cr3t", "wrong", false},
		{"missing token rejects when secret set", "s3cr3t", "", false},
	}
	for _, c := range cases {
		if got := TokenOK(c.secret, c.provided); got != c.want {
			t.Errorf("%s: TokenOK(%q, %q) = %v, want %v", c.name, c.secret, c.provided, got, c.want)
		}
	}
}
