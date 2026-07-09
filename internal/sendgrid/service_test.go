package sendgrid

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// testKey generates a P-256 key pair and returns the private key plus the
// Base64 PKIX public key, the form SendGrid returns when signing is enabled.
func testKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return key, base64.StdEncoding.EncodeToString(der)
}

// signEvent reproduces SendGrid's signature: Base64 ASN.1/DER ECDSA over
// sha256(timestamp || raw body).
func signEvent(t *testing.T, key *ecdsa.PrivateKey, timestamp string, body []byte) string {
	t.Helper()
	digest := sha256.New()
	digest.Write([]byte(timestamp))
	digest.Write(body)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest.Sum(nil))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func TestVerifySignature(t *testing.T) {
	key, pub := testKey(t)
	body := []byte(`[{"event":"delivered","email":"a@example.com","timestamp":1751961600}]`)
	timestamp := "1751961600"
	sig := signEvent(t, key, timestamp, body)

	if err := VerifySignature(pub, timestamp, sig, body); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Tampered body fails.
	if err := VerifySignature(pub, timestamp, sig, append(append([]byte{}, body...), ' ')); err == nil {
		t.Fatal("expected rejection for tampered body")
	}
	// Tampered timestamp fails (the timestamp is part of the signed message).
	if err := VerifySignature(pub, "1751961601", sig, body); err == nil {
		t.Fatal("expected rejection for tampered timestamp")
	}
	// A different key's signature fails.
	otherKey, _ := testKey(t)
	if err := VerifySignature(pub, timestamp, signEvent(t, otherKey, timestamp, body), body); err == nil {
		t.Fatal("expected rejection for signature from another key")
	}
}

func TestVerifySignatureFailsClosed(t *testing.T) {
	key, pub := testKey(t)
	body := []byte(`[{"event":"open"}]`)
	timestamp := "1751961600"
	sig := signEvent(t, key, timestamp, body)

	// Missing key / timestamp / signature all fail.
	if err := VerifySignature("", timestamp, sig, body); err == nil {
		t.Fatal("missing public key must fail closed")
	}
	if err := VerifySignature(pub, "", sig, body); err == nil {
		t.Fatal("missing timestamp must fail closed")
	}
	if err := VerifySignature(pub, timestamp, "", body); err == nil {
		t.Fatal("missing signature must fail closed")
	}
	// Garbage Base64 in either the key or the signature fails.
	if err := VerifySignature("!!!not-base64!!!", timestamp, sig, body); err == nil {
		t.Fatal("undecodable public key must fail closed")
	}
	if err := VerifySignature(pub, timestamp, "!!!not-base64!!!", body); err == nil {
		t.Fatal("undecodable signature must fail closed")
	}
	// Valid Base64 that is not a PKIX key fails.
	if err := VerifySignature(base64.StdEncoding.EncodeToString([]byte("not a key")), timestamp, sig, body); err == nil {
		t.Fatal("non-PKIX public key must fail closed")
	}
	// A PKIX key of the wrong algorithm fails.
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	edDER, err := x509.MarshalPKIXPublicKey(edPub)
	if err != nil {
		t.Fatalf("marshal ed25519 key: %v", err)
	}
	if err := VerifySignature(base64.StdEncoding.EncodeToString(edDER), timestamp, sig, body); err == nil {
		t.Fatal("non-ECDSA public key must fail closed")
	}
}

func TestParseEvents(t *testing.T) {
	events, err := ParseEvents([]byte(`[{"event":"delivered"},{"event":"open","email":"a@example.com"}]`))
	if err != nil {
		t.Fatalf("ParseEvents error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[1]["email"] != "a@example.com" {
		t.Errorf("event fields not carried: %v", events[1])
	}
	// A single object (not the documented array) and non-JSON both fail.
	if _, err := ParseEvents([]byte(`{"event":"delivered"}`)); err == nil {
		t.Fatal("non-array body must fail")
	}
	if _, err := ParseEvents([]byte(`not json`)); err == nil {
		t.Fatal("non-JSON body must fail")
	}
}

func TestMatchesFilter(t *testing.T) {
	ev := func(name, bounceType string) map[string]interface{} {
		m := map[string]interface{}{"event": name}
		if bounceType != "" {
			m["type"] = bounceType
		}
		return m
	}
	cases := []struct {
		name     string
		event    map[string]interface{}
		selected []string
		want     bool
	}{
		{"empty selection matches all", ev("delivered", ""), nil, true},
		{"selected event matches", ev("delivered", ""), []string{"delivered", "open"}, true},
		{"unselected event dropped", ev("click", ""), []string{"delivered", "open"}, false},
		{"spamreport wire name", ev("spamreport", ""), []string{"spamreport"}, true},
		{"blocked matches native blocked event", ev("blocked", ""), []string{"blocked"}, true},
		{"blocked matches bounce typed blocked", ev("bounce", "blocked"), []string{"blocked"}, true},
		{"plain bounce not matched by blocked", ev("bounce", "bounce"), []string{"blocked"}, false},
		{"untyped bounce not matched by blocked", ev("bounce", ""), []string{"blocked"}, false},
		{"dropped not matched by blocked", ev("dropped", ""), []string{"blocked"}, false},
		{"bounce selection matches blocked-typed bounce", ev("bounce", "blocked"), []string{"bounce"}, true},
	}
	for _, c := range cases {
		if got := MatchesFilter(c.event, c.selected); got != c.want {
			t.Errorf("%s: MatchesFilter(%v, %v) = %v, want %v", c.name, c.event, c.selected, got, c.want)
		}
	}
}

func TestEventOutputs(t *testing.T) {
	raw := []byte(`{"event":"bounce","email":"a@example.com","timestamp":1751961600,` +
		`"sg_event_id":"ev1","sg_message_id":"msg1","type":"blocked","reason":"550 5.1.1 user unknown",` +
		`"status":"5.1.1","tls":1,"category":["newsletter","promo"],"sg_machine_open":false}`)
	var event map[string]interface{}
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("test payload invalid: %v", err)
	}

	out := EventOutputs(event)
	want := map[string]string{
		"event":           "bounce",
		"email":           "a@example.com",
		"timestamp":       "1751961600",
		"sg_event_id":     "ev1",
		"sg_message_id":   "msg1",
		"type":            "blocked",
		"reason":          "550 5.1.1 user unknown",
		"status":          "5.1.1",
		"tls":             "1",
		"category":        `["newsletter","promo"]`,
		"sg_machine_open": "false",
		// Fields this event doesn't carry are pre-initialised to "".
		"response":  "",
		"attempt":   "",
		"url":       "",
		"useragent": "",
		"ip":        "",
	}
	for k, v := range want {
		if got := out[k]; got != v {
			t.Errorf("out[%q] = %#v, want %q", k, got, v)
		}
	}
	// body carries the single event's JSON, round-trippable.
	var roundTrip map[string]interface{}
	bodyStr, _ := out["body"].(string)
	if err := json.Unmarshal([]byte(bodyStr), &roundTrip); err != nil {
		t.Fatalf("body output is not valid JSON: %v", err)
	}
	if roundTrip["event"] != "bounce" {
		t.Errorf("body output does not carry the event, got %v", roundTrip["event"])
	}

	// A string category passes through unencoded, and every declared output is
	// present even for a near-empty event.
	out = EventOutputs(map[string]interface{}{"event": "processed", "category": "single"})
	if out["category"] != "single" {
		t.Errorf("string category = %#v, want %q", out["category"], "single")
	}
	declared := []string{
		"event", "email", "timestamp", "sg_message_id", "sg_event_id", "sg_machine_open",
		"type", "reason", "status", "response", "attempt", "category", "url",
		"useragent", "ip", "tls", "body",
	}
	for _, k := range declared {
		if _, ok := out[k]; !ok {
			t.Errorf("declared output %q missing", k)
		}
	}
}

func TestEventStr(t *testing.T) {
	if got := eventStr("abc"); got != "abc" {
		t.Errorf("string passthrough = %q", got)
	}
	if got := eventStr(float64(1751961600)); got != "1751961600" {
		t.Errorf("integer number = %q", got)
	}
	if got := eventStr(true); got != "true" {
		t.Errorf("bool = %q", got)
	}
	if got := eventStr(nil); got != "" {
		t.Errorf("nil = %q", got)
	}
}
