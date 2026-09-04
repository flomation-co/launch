package freshsales

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func req(t *testing.T, target string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// TestPresentedSecret covers the three places a Freshsales admin can put the
// secret, and the places we must NOT read one from.
func TestPresentedSecret(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		headers map[string]string
		want    string
	}{
		{"query parameter", "/webhook/x?secret=abc", nil, "abc"},
		{"custom header", "/webhook/x", map[string]string{SecretHeader: "abc"}, "abc"},
		{"token auth", "/webhook/x", map[string]string{"Authorization": "Token abc"}, "abc"},
		{"token auth, odd casing", "/webhook/x", map[string]string{"Authorization": "token abc"}, "abc"},
		{"bare value pasted into the auth field", "/webhook/x", map[string]string{"Authorization": "abc"}, "abc"},
		{"query wins over header", "/webhook/x?secret=fromquery", map[string]string{SecretHeader: "fromheader"}, "fromquery"},
		{"whitespace trimmed", "/webhook/x", map[string]string{"Authorization": "Token   abc  "}, "abc"},

		// Not ours: do not guess a secret out of another scheme.
		{"basic auth ignored", "/webhook/x", map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, ""},
		{"bearer ignored", "/webhook/x", map[string]string{"Authorization": "Bearer sometoken"}, ""},
		{"nothing presented", "/webhook/x", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PresentedSecret(req(t, c.target, c.headers)); got != c.want {
				t.Errorf("PresentedSecret = %q, want %q", got, c.want)
			}
		})
	}
}

// TestVerifyAndParse_RejectsUnauthenticated is the load-bearing test: nothing
// unauthenticated may fire a flow.
func TestVerifyAndParse_RejectsUnauthenticated(t *testing.T) {
	body := []byte(`{"event_type":"created"}`)

	cases := []struct {
		name       string
		configured string
		headers    map[string]string
		target     string
	}{
		{"no secret configured", "", map[string]string{"Authorization": "Token abc"}, "/webhook/x"},
		{"configured secret is whitespace", "   ", map[string]string{"Authorization": "Token abc"}, "/webhook/x"},
		{"nothing presented", "abc", nil, "/webhook/x"},
		{"wrong secret", "abc", map[string]string{"Authorization": "Token wrong"}, "/webhook/x"},
		{"prefix of the real secret", "abcdef", map[string]string{"Authorization": "Token abc"}, "/webhook/x"},
		{"secret with the right length but wrong bytes", "abcdef", map[string]string{"Authorization": "Token abcdeg"}, "/webhook/x"},
		{"basic auth carrying the secret", "abc", map[string]string{"Authorization": "Basic abc"}, "/webhook/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := VerifyAndParse(body, req(t, c.target, c.headers), c.configured); err == nil {
				t.Error("expected verification to fail, but it passed")
			}
		})
	}
}

func TestVerifyAndParse_AcceptsMatchingSecret(t *testing.T) {
	body := []byte(`{"event_type":"created","contact":{"id":144}}`)
	payload, err := VerifyAndParse(body, req(t, "/webhook/x", map[string]string{"Authorization": "Token s3cret"}), "s3cret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["event_type"] != "created" {
		t.Errorf("payload not parsed: %v", payload)
	}
}

func TestVerifyAndParse_RejectsMalformedBody(t *testing.T) {
	_, err := VerifyAndParse([]byte("not json"), req(t, "/webhook/x?secret=s", nil), "s")
	if err == nil {
		t.Error("expected a parse error")
	}
}

// TestEventToData_FindsTheRecord covers both payload shapes: a whole nested
// object, and individually chosen top-level fields.
func TestEventToData_FindsTheRecord(t *testing.T) {
	cases := []struct {
		name     string
		payload  map[string]interface{}
		wantID   string
		wantType string
	}{
		{"nested contact", map[string]interface{}{
			"event_type": "created", "entity_type": "contact",
			"contact": map[string]interface{}{"id": float64(144)},
		}, "144", "created"},
		{"nested sales account", map[string]interface{}{
			"event": "updated", "sales_account": map[string]interface{}{"id": float64(9)},
		}, "9", "updated"},
		{"flat id", map[string]interface{}{
			"action": "won", "id": float64(77),
		}, "77", "won"},
		{"string id", map[string]interface{}{"id": "abc"}, "abc", ""},
		{"nothing identifiable", map[string]interface{}{"noise": true}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := EventToData(c.payload, []byte(`{}`))
			if data["record_id"] != c.wantID {
				t.Errorf("record_id = %v, want %q", data["record_id"], c.wantID)
			}
			if data["event_type"] != c.wantType {
				t.Errorf("event_type = %v, want %q", data["event_type"], c.wantType)
			}
			if data["body"] == nil || data["triggered_at"] == nil {
				t.Error("body and triggered_at must always be present")
			}
		})
	}
}

// TestEventToData_SpreadsScalars keeps the bare-key behaviour the other
// triggers have, without letting it clobber the stable keys.
func TestEventToData_SpreadsScalars(t *testing.T) {
	data := EventToData(map[string]interface{}{
		"event_type": "created",
		"deal_value": float64(25000),
		"is_won":     true,
		"owner":      "Ada",
		"nested":     map[string]interface{}{"ignored": true},
	}, []byte(`{}`))

	if data["deal_value"] != float64(25000) || data["is_won"] != true || data["owner"] != "Ada" {
		t.Errorf("top-level scalars should be spread: %v", data)
	}
	if _, ok := data["nested"]; ok {
		t.Error("nested objects should not be spread as bare keys")
	}
	if data["event_type"] != "created" {
		t.Error("stable keys must not be clobbered")
	}
}

func TestMatchesFilter(t *testing.T) {
	cases := []struct {
		filter, event, entity string
		want                  bool
	}{
		{"", "created", "contact", true},
		{"   ", "created", "contact", true},
		{"created", "created", "contact", true},
		{"contact.created", "created", "contact", true},
		{"deal.won,contact.created", "created", "contact", true},
		{"CONTACT.CREATED", "created", "contact", true},
		{"deal.won", "created", "contact", false},
		{"updated", "created", "contact", false},
		{",,", "created", "contact", false},
	}
	for _, c := range cases {
		if got := MatchesFilter(c.event, c.entity, c.filter); got != c.want {
			t.Errorf("MatchesFilter(%q, %q, %q) = %v, want %v", c.event, c.entity, c.filter, got, c.want)
		}
	}
}
