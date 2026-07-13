package typeform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"testing"
)

const testSecret = "typeform_test_secret"

// signedRequest builds an *http.Request carrying a valid Typeform-Signature
// header over the given body, exactly as Typeform would sign it.
func signedRequest(body []byte, secret string) *http.Request {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	r, _ := http.NewRequest(http.MethodPost, "/webhook/abc", nil)
	r.Header.Set("Typeform-Signature", sig)
	return r
}

func samplePayload() []byte {
	return []byte(`{
		"event_id": "01ABCDEF",
		"event_type": "form_response",
		"form_response": {
			"form_id": "abc123",
			"token": "tok_789",
			"submitted_at": "2026-07-11T10:00:00Z",
			"answers": [
				{"field": {"id": "q1"}, "type": "text", "text": "Hello"}
			],
			"hidden": {"utm_source": "newsletter"},
			"definition": {"id": "abc123", "title": "Contact"}
		}
	}`)
}

func TestVerifyAndParse_Valid(t *testing.T) {
	body := samplePayload()
	r := signedRequest(body, testSecret)

	ev, err := VerifyAndParse(body, r, testSecret)
	if err != nil {
		t.Fatalf("expected valid signature, got error: %v", err)
	}
	if ev.EventID != "01ABCDEF" {
		t.Fatalf("expected event id 01ABCDEF, got %q", ev.EventID)
	}
	if ev.EventType != "form_response" {
		t.Fatalf("expected type form_response, got %q", ev.EventType)
	}
	if ev.FormResponse.FormID != "abc123" {
		t.Fatalf("expected form_id abc123, got %q", ev.FormResponse.FormID)
	}
	if ev.FormResponse.Token != "tok_789" {
		t.Fatalf("expected token tok_789, got %q", ev.FormResponse.Token)
	}
}

func TestVerifyAndParse_BadSignature(t *testing.T) {
	body := samplePayload()
	// Sign with a different secret than we verify against.
	r := signedRequest(body, "wrong_secret")

	if _, err := VerifyAndParse(body, r, testSecret); err == nil {
		t.Fatal("expected signature verification to fail, got nil error")
	}
}

func TestVerifyAndParse_MissingHeader(t *testing.T) {
	body := samplePayload()
	r, _ := http.NewRequest(http.MethodPost, "/webhook/abc", nil)

	if _, err := VerifyAndParse(body, r, testSecret); err == nil {
		t.Fatal("expected error for missing Typeform-Signature header, got nil")
	}
}

func TestVerifyAndParse_MalformedHeader(t *testing.T) {
	body := samplePayload()
	r, _ := http.NewRequest(http.MethodPost, "/webhook/abc", nil)
	// Missing the required "sha256=" prefix.
	r.Header.Set("Typeform-Signature", "deadbeef")

	if _, err := VerifyAndParse(body, r, testSecret); err == nil {
		t.Fatal("expected error for malformed signature header, got nil")
	}
}

func TestVerifyAndParse_TamperedBody(t *testing.T) {
	body := samplePayload()
	// Sign the original body, then verify a tampered body against it.
	r := signedRequest(body, testSecret)
	tampered := append([]byte(nil), body...)
	tampered[10] ^= 0xff

	if _, err := VerifyAndParse(tampered, r, testSecret); err == nil {
		t.Fatal("expected verification to fail for tampered body, got nil error")
	}
}

func TestEventToData(t *testing.T) {
	body := samplePayload()
	r := signedRequest(body, testSecret)
	ev, err := VerifyAndParse(body, r, testSecret)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	data := EventToData(ev)

	checks := map[string]string{
		"event_type":     "form_response",
		"event_id":       "01ABCDEF",
		"form_id":        "abc123",
		"response_token": "tok_789",
		"submitted_at":   "2026-07-11T10:00:00Z",
		"content":        "Typeform response for form abc123",
	}
	for k, want := range checks {
		if got, _ := data[k].(string); got != want {
			t.Errorf("EventToData[%q] = %q, want %q", k, got, want)
		}
	}

	answers, _ := data["answers"].(string)
	if answers == "" || answers == "[]" {
		t.Errorf("expected answers JSON string to be populated, got %q", answers)
	}
	if body := data["body"].(string); body == "" {
		t.Error("expected raw body to be populated")
	}
}

func TestEventToData_EmptyAnswers(t *testing.T) {
	ev := Event{EventType: "form_response", EventID: "x"}
	data := EventToData(ev)
	if got := data["answers"].(string); got != "[]" {
		t.Errorf("expected empty answers to default to \"[]\", got %q", got)
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
