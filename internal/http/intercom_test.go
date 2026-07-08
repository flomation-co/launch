package http

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 — test mirrors Intercom's mandated HMAC-SHA1 scheme
	"encoding/hex"
	"encoding/json"
	"testing"
)

// intercomSign reproduces Intercom's signature: "sha1=" + hex(HMAC-SHA1(secret, body)).
func intercomSign(body []byte, secret string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	return "sha1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestIntercomVerifySignature(t *testing.T) {
	body := []byte(`{"type":"notification_event","topic":"conversation.user.created"}`)
	secret := "app-client-secret"
	good := intercomSign(body, secret)

	if !intercomVerifySignature(body, good, secret) {
		t.Fatal("valid signature should verify")
	}
	// Wrong secret.
	if intercomVerifySignature(body, good, "other-secret") {
		t.Fatal("signature must fail with the wrong secret")
	}
	// Tampered body.
	if intercomVerifySignature([]byte(`{"topic":"contact.deleted"}`), good, secret) {
		t.Fatal("signature must fail when the body is tampered")
	}
	// Missing / malformed headers fail closed.
	if intercomVerifySignature(body, "", secret) {
		t.Fatal("empty signature must fail closed")
	}
	if intercomVerifySignature(body, "sha1=not-hex!!", secret) {
		t.Fatal("malformed hex signature must fail closed")
	}
	// Missing "sha1=" prefix fails closed even when the digest itself matches.
	if intercomVerifySignature(body, good[len("sha1="):], secret) {
		t.Fatal("signature without the sha1= prefix must fail closed")
	}
}

func TestIntercomShouldFire(t *testing.T) {
	// Ping never fires, filtered or not.
	if intercomShouldFire("ping", "") {
		t.Fatal("ping must not fire")
	}
	if intercomShouldFire("ping", "ping") {
		t.Fatal("ping must not fire even when explicitly filtered for")
	}
	// No filter fires on everything else.
	if !intercomShouldFire("conversation.user.created", "") {
		t.Fatal("unfiltered topic should fire")
	}
	// Filter fires only on the matching topic.
	if !intercomShouldFire("ticket.created", "ticket.created") {
		t.Fatal("matching filter should fire")
	}
	if intercomShouldFire("ticket.closed", "ticket.created") {
		t.Fatal("mismatched filter must not fire")
	}
}

func TestIntercomEventOutputs_Conversation(t *testing.T) {
	// A trimmed real-shaped notification_event: numeric app_id/created_at ids
	// prove the stringifier, contacts list + admin_assignee_id prove the
	// convenience ids.
	raw := []byte(`{
		"type": "notification_event",
		"topic": "conversation.admin.assigned",
		"app_id": "abc123",
		"created_at": 1751961600,
		"data": {
			"item": {
				"type": "conversation",
				"id": "987654",
				"admin_assignee_id": 4242,
				"contacts": {"type": "contact.list", "contacts": [{"type": "contact", "id": "60d0b1c3"}]},
				"source": {"author": {"type": "user", "id": "ignored-when-contacts-present"}}
			}
		}
	}`)
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("test payload invalid: %v", err)
	}

	out := intercomEventOutputs(payload, raw)
	want := map[string]string{
		"topic":           "conversation.admin.assigned",
		"app_id":          "abc123",
		"created_at":      "1751961600",
		"item_type":       "conversation",
		"item_id":         "987654",
		"conversation_id": "987654",
		"contact_id":      "60d0b1c3",
		"admin_id":        "4242",
		"ticket_id":       "",
		"company_id":      "",
	}
	for k, v := range want {
		if got := out[k]; got != v {
			t.Errorf("out[%q] = %#v, want %q", k, got, v)
		}
	}
	if out["body"] != string(raw) {
		t.Error("body output should carry the raw JSON verbatim")
	}
}

func TestIntercomEventOutputs_ItemKinds(t *testing.T) {
	build := func(itemType, itemID string) map[string]interface{} {
		return map[string]interface{}{
			"topic": "x",
			"data":  map[string]interface{}{"item": map[string]interface{}{"type": itemType, "id": itemID}},
		}
	}
	if out := intercomEventOutputs(build("ticket", "t1"), nil); out["ticket_id"] != "t1" {
		t.Errorf("ticket item should fill ticket_id, got %#v", out["ticket_id"])
	}
	for _, kind := range []string{"contact", "user", "lead"} {
		if out := intercomEventOutputs(build(kind, "c1"), nil); out["contact_id"] != "c1" {
			t.Errorf("%s item should fill contact_id, got %#v", kind, out["contact_id"])
		}
	}
	if out := intercomEventOutputs(build("company", "co1"), nil); out["company_id"] != "co1" {
		t.Errorf("company item should fill company_id, got %#v", out["company_id"])
	}
	// Contact fallback: no contacts list, a customer-typed source author fills
	// contact_id.
	conv := map[string]interface{}{
		"topic": "conversation.user.created",
		"data": map[string]interface{}{"item": map[string]interface{}{
			"type": "conversation", "id": "c9",
			"source": map[string]interface{}{"author": map[string]interface{}{"type": "lead", "id": "au1"}},
		}},
	}
	if out := intercomEventOutputs(conv, nil); out["contact_id"] != "au1" {
		t.Errorf("conversation without contacts list should fall back to source author, got %#v", out["contact_id"])
	}
	// Admin-initiated conversations carry the teammate as the source author —
	// their id must NOT be reported as the contact.
	adminConv := map[string]interface{}{
		"topic": "conversation.admin.single.created",
		"data": map[string]interface{}{"item": map[string]interface{}{
			"type": "conversation", "id": "c10",
			"source": map[string]interface{}{"author": map[string]interface{}{"type": "admin", "id": "11037910"}},
		}},
	}
	if out := intercomEventOutputs(adminConv, nil); out["contact_id"] != "" {
		t.Errorf("admin source author must not populate contact_id, got %#v", out["contact_id"])
	}
	// A payload without data.item still yields every key.
	out := intercomEventOutputs(map[string]interface{}{"topic": "ping"}, nil)
	if out["item_type"] != "" || out["item_id"] != "" {
		t.Errorf("itemless payload should yield empty item fields, got %#v", out)
	}
}

func TestIntercomEventStr(t *testing.T) {
	if got := intercomEventStr("abc"); got != "abc" {
		t.Errorf("string passthrough = %q", got)
	}
	if got := intercomEventStr(float64(1751961600)); got != "1751961600" {
		t.Errorf("integer number = %q", got)
	}
	if got := intercomEventStr(nil); got != "" {
		t.Errorf("nil = %q", got)
	}
}
