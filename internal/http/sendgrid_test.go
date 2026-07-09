package http

import "testing"

func TestSendgridEventToggles(t *testing.T) {
	// Empty selection → every settings toggle enabled.
	all := sendgridEventToggles(nil)
	if len(all) != len(sendgridToggleFields) {
		t.Fatalf("expected %d toggles, got %d", len(sendgridToggleFields), len(all))
	}
	for field, on := range all {
		if !on {
			t.Errorf("empty selection should enable %q", field)
		}
	}

	// A specific selection enables only its toggles.
	got := sendgridEventToggles([]string{"delivered", "open"})
	if !got["delivered"] || !got["open"] {
		t.Error("selected events should be enabled")
	}
	for _, field := range []string{"bounce", "click", "deferred", "dropped", "processed", "spam_report", "unsubscribe", "group_unsubscribe", "group_resubscribe"} {
		if got[field] {
			t.Errorf("unselected toggle %q should be disabled", field)
		}
	}

	// The payload event name "spamreport" maps to the "spam_report" settings
	// field; the wire name must not leak into the settings body.
	got = sendgridEventToggles([]string{"spamreport"})
	if !got["spam_report"] {
		t.Error("spamreport selection should enable the spam_report toggle")
	}
	if _, present := got["spamreport"]; present {
		t.Error("spamreport must not be sent as a settings field")
	}

	// "blocked" has no toggle of its own — it enables bounce AND dropped
	// (blocks arrive under either, disambiguated by the inbound filter).
	got = sendgridEventToggles([]string{"blocked"})
	if !got["bounce"] || !got["dropped"] {
		t.Error("blocked selection should enable both the bounce and dropped toggles")
	}
	if got["delivered"] || got["open"] {
		t.Error("blocked selection should not enable unrelated toggles")
	}
	if _, present := got["blocked"]; present {
		t.Error("blocked must not be sent as a settings field")
	}
}

func TestSendgridEvents(t *testing.T) {
	// Empty (or the "All events" empty option) → nil, meaning all.
	if got := sendgridEvents(""); len(got) != 0 {
		t.Errorf("empty selection should yield no filter, got %v", got)
	}
	if got := sendgridEvents(`[""]`); len(got) != 0 {
		t.Errorf("the All-events empty option should yield no filter, got %v", got)
	}
	// JSON-array form (the multi-select's on-the-wire shape).
	got := sendgridEvents(`["delivered","bounce"]`)
	if len(got) != 2 || got[0] != "delivered" || got[1] != "bounce" {
		t.Errorf("JSON-array parse wrong: %v", got)
	}
	// CSV form with spaces.
	got = sendgridEvents("open, click ")
	if len(got) != 2 || got[0] != "open" || got[1] != "click" {
		t.Errorf("CSV parse wrong: %v", got)
	}
}

func TestSendgridStateCurrent(t *testing.T) {
	base := func() *sendgridWebhookState {
		return &sendgridWebhookState{
			WebhookID:   "wh-1",
			PublicKey:   "pk",
			Events:      []string{"delivered", "bounce"},
			Region:      "",
			CallbackURL: "https://launch.example.com/webhook/t1",
		}
	}
	events := []string{"bounce", "delivered"} // order-independent
	callback := "https://launch.example.com/webhook/t1"

	if !sendgridStateCurrent(base(), events, "", callback) {
		t.Error("unchanged config should be detected as current")
	}
	if sendgridStateCurrent(nil, events, "", callback) {
		t.Error("missing state is never current")
	}
	st := base()
	st.WebhookID = ""
	if sendgridStateCurrent(st, events, "", callback) {
		t.Error("state without a webhook id is not current")
	}
	st = base()
	st.PublicKey = ""
	if sendgridStateCurrent(st, events, "", callback) {
		t.Error("state without a public key is not current")
	}
	if sendgridStateCurrent(base(), []string{"delivered"}, "", callback) {
		t.Error("a changed event selection is not current")
	}
	if sendgridStateCurrent(base(), events, "eu", callback) {
		t.Error("a changed region is not current")
	}
	if sendgridStateCurrent(base(), events, "", "https://launch.example.com/webhook/t2") {
		t.Error("a changed callback URL is not current")
	}
}

func TestSendgridHostFor(t *testing.T) {
	if got := sendgridHostFor(""); got != sendgridAPIBaseURL {
		t.Errorf("global region host = %q", got)
	}
	if got := sendgridHostFor("eu"); got != sendgridEUAPIBaseURL {
		t.Errorf("eu region host = %q", got)
	}
}
