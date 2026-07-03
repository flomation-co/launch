package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormaliseZendeskSubdomain(t *testing.T) {
	cases := map[string]string{
		"mycompany":                        "mycompany",
		"  mycompany  ":                    "mycompany",
		"mycompany.zendesk.com":            "mycompany",
		"https://mycompany.zendesk.com":    "mycompany",
		"https://mycompany.zendesk.com/":   "mycompany",
		"https://acme.zendesk.com/agent/x": "acme",
	}
	for in, want := range cases {
		if got := normaliseZendeskSubdomain(in); got != want {
			t.Errorf("normaliseZendeskSubdomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestZendeskConditions_Default(t *testing.T) {
	cond := zendeskConditions("")
	any, ok := cond["any"].([]map[string]interface{})
	if !ok || len(any) != 2 {
		t.Fatalf("expected default any-conditions of length 2, got %#v", cond)
	}
	// Invalid JSON falls back to the default too.
	if c := zendeskConditions("{not json"); c["any"] == nil {
		t.Fatalf("invalid conditions JSON should fall back to default, got %#v", c)
	}
	// Valid custom conditions are used verbatim.
	custom := zendeskConditions(`{"all":[{"field":"priority","operator":"is","value":"high"}]}`)
	if custom["all"] == nil {
		t.Fatalf("custom conditions not preserved: %#v", custom)
	}
}

func TestNumToID(t *testing.T) {
	if got := numToID(float64(360001)); got != "360001" {
		t.Errorf("numToID(float64) = %q", got)
	}
	if got := numToID("abc"); got != "abc" {
		t.Errorf("numToID(string) = %q", got)
	}
	if got := numToID(nil); got != "" {
		t.Errorf("numToID(nil) = %q", got)
	}
}

// TestZendeskDo_RequestShaping verifies the webhook-create request is shaped
// correctly (method, path, auth header, and envelope) against an httptest
// server standing in for Zendesk — the interaction we can't live-validate here.
func TestZendeskDo_RequestShaping(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"webhook":{"id":"01HXYZ"}}`))
	}))
	defer srv.Close()

	prev := zendeskHostOverride
	zendeskHostOverride = srv.URL
	defer func() { zendeskHostOverride = prev }()

	body := map[string]interface{}{
		"webhook": map[string]interface{}{
			"endpoint":      "https://public/webhook/tr-1",
			"subscriptions": []string{"conditional_ticket_events"},
		},
	}
	resp, status, err := zendeskDo("Basic abc", "acme", http.MethodPost, "/webhooks", body)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("zendeskDo status=%d err=%v", status, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v2/webhooks" {
		t.Fatalf("wrong method/path: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Basic abc" {
		t.Fatalf("auth header not forwarded: %q", gotAuth)
	}
	wh, _ := gotBody["webhook"].(map[string]interface{})
	if wh == nil || wh["endpoint"] != "https://public/webhook/tr-1" {
		t.Fatalf("request body not marshalled correctly: %#v", gotBody)
	}
	if wh, _ := resp["webhook"].(map[string]interface{}); wh == nil || wh["id"] != "01HXYZ" {
		t.Fatalf("response not parsed: %#v", resp)
	}
}
