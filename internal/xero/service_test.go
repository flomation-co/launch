package xero

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sign(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	key := "signing-key-xyz"
	body := []byte(`{"events":[],"firstEventSequence":0,"lastEventSequence":0}`)
	r := httptest.NewRequest(http.MethodPost, "/webhook/x", nil)
	r.Header.Set("x-xero-signature", sign(key, body))
	if err := VerifySignature(key, body, r); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySignature("wrong", body, r); err == nil {
		t.Fatal("expected invalid signature to be rejected")
	}
	r2 := httptest.NewRequest(http.MethodPost, "/webhook/x", nil)
	if err := VerifySignature(key, body, r2); err == nil {
		t.Fatal("expected missing header to be rejected")
	}
}

func TestHasEventsAndParse(t *testing.T) {
	// Intent-to-receive probe: valid signature, empty events → no fire.
	probe := []byte(`{"events":[],"firstEventSequence":0,"lastEventSequence":0}`)
	if HasEvents(probe) {
		t.Fatal("empty events should report HasEvents=false")
	}

	body := []byte(`{"events":[{"resourceUrl":"https://api.xero.com/api.xro/2.0/Contacts/abc","resourceId":"abc","tenantId":"ten-1","tenantType":"ORGANISATION","eventCategory":"CONTACT","eventType":"UPDATE","eventDateUtc":"2026-07-10T10:00:00Z"}]}`)
	if !HasEvents(body) {
		t.Fatal("expected HasEvents=true")
	}
	data, err := ParseEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if data["tenant_id"] != "ten-1" || data["resource_id"] != "abc" || data["event_category"] != "CONTACT" || data["operation"] != "UPDATE" {
		t.Fatalf("unexpected parse: %+v", data)
	}
	if data["event_type"] != "CONTACT.UPDATE" {
		t.Fatalf("event_type: %v", data["event_type"])
	}
}

func TestMatchesFilter(t *testing.T) {
	cases := []struct {
		event, filter string
		want          bool
	}{
		{"CONTACT.UPDATE", "", true},
		{"CONTACT.UPDATE", "CONTACT.UPDATE", true},
		{"CONTACT.UPDATE", "INVOICE.CREATE,CONTACT.UPDATE", true},
		{"CONTACT.UPDATE", "CONTACT", true}, // category-only
		{"CONTACT.UPDATE", "INVOICE", false},
	}
	for _, c := range cases {
		if got := MatchesFilter(c.event, c.filter); got != c.want {
			t.Errorf("MatchesFilter(%q,%q)=%v want %v", c.event, c.filter, got, c.want)
		}
	}
}
