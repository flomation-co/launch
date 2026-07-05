package stripe

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82/webhook"
)

const testSecret = "whsec_test_secret_key"

// signedRequest builds an *http.Request carrying a valid Stripe-Signature header
// over the given body, exactly as Stripe would sign it.
func signedRequest(body []byte, secret string, ts time.Time) *http.Request {
	sig := webhook.ComputeSignature(ts, body, secret)
	header := fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(sig))
	r, _ := http.NewRequest(http.MethodPost, "/webhook/abc", nil)
	r.Header.Set("Stripe-Signature", header)
	return r
}

func samplePayload() []byte {
	return []byte(`{
		"id": "evt_123",
		"type": "payment_intent.succeeded",
		"created": 1700000000,
		"data": {
			"object": {
				"id": "pi_456",
				"object": "payment_intent",
				"customer": "cus_789",
				"amount": 1999,
				"currency": "gbp",
				"status": "succeeded"
			}
		}
	}`)
}

func TestVerifyAndParse_Valid(t *testing.T) {
	body := samplePayload()
	r := signedRequest(body, testSecret, time.Now())

	ev, err := VerifyAndParse(body, r, testSecret)
	if err != nil {
		t.Fatalf("expected valid signature, got error: %v", err)
	}
	if ev.ID != "evt_123" {
		t.Fatalf("expected event id evt_123, got %q", ev.ID)
	}
	if string(ev.Type) != "payment_intent.succeeded" {
		t.Fatalf("expected type payment_intent.succeeded, got %q", ev.Type)
	}
}

func TestVerifyAndParse_BadSignature(t *testing.T) {
	body := samplePayload()
	// Sign with a different secret than we verify against.
	r := signedRequest(body, "whsec_wrong_secret", time.Now())

	if _, err := VerifyAndParse(body, r, testSecret); err == nil {
		t.Fatal("expected signature verification to fail, got nil error")
	}
}

func TestVerifyAndParse_MissingHeader(t *testing.T) {
	body := samplePayload()
	r, _ := http.NewRequest(http.MethodPost, "/webhook/abc", nil)

	if _, err := VerifyAndParse(body, r, testSecret); err == nil {
		t.Fatal("expected error for missing Stripe-Signature header, got nil")
	}
}

func TestEventToData(t *testing.T) {
	body := samplePayload()
	r := signedRequest(body, testSecret, time.Now())
	ev, err := VerifyAndParse(body, r, testSecret)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	data := EventToData(ev)

	checks := map[string]string{
		"event_type":  "payment_intent.succeeded",
		"event_id":    "evt_123",
		"object_type": "payment_intent",
		"object_id":   "pi_456",
		"customer_id": "cus_789",
		"currency":    "gbp",
		"status":      "succeeded",
		"amount":      "1999",
	}
	for k, want := range checks {
		if got := fmt.Sprintf("%v", data[k]); got != want {
			t.Errorf("EventToData[%q] = %q, want %q", k, got, want)
		}
	}
	if data["body"] == "" || data["body"] == nil {
		t.Error("expected raw body to be populated")
	}
	if _, ok := data["triggered_at"].(string); !ok {
		t.Error("expected triggered_at to be an RFC3339 string")
	}
}

func TestMatchesFilter(t *testing.T) {
	cases := []struct {
		eventType string
		filter    string
		want      bool
	}{
		{"payment_intent.succeeded", "", true},                                        // empty = all
		{"payment_intent.succeeded", "payment_intent.succeeded", true},                // exact
		{"invoice.paid", "payment_intent.succeeded,invoice.paid", true},               // CSV member
		{"invoice.paid", " payment_intent.succeeded , invoice.paid ", true},           // whitespace tolerant
		{"charge.refunded", "payment_intent.succeeded,invoice.paid", false},           // not listed
		{"PAYMENT_INTENT.SUCCEEDED", "payment_intent.succeeded", true},                // case-insensitive
	}
	for _, c := range cases {
		if got := MatchesFilter(c.eventType, c.filter); got != c.want {
			t.Errorf("MatchesFilter(%q, %q) = %v, want %v", c.eventType, c.filter, got, c.want)
		}
	}
}
