package http

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 — test mirrors Trello's mandated HMAC-SHA1 scheme
	"encoding/base64"
	"testing"
)

// trelloSign reproduces Trello's signature: base64(HMAC-SHA1(secret, body+callback)).
func trelloSign(body []byte, secret, callback string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	mac.Write([]byte(callback))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestTrelloVerifySignature(t *testing.T) {
	body := []byte(`{"action":{"type":"createCard"}}`)
	secret := "s3cr3t-api-secret"
	callback := "https://launch.example.com/webhook/abc123"
	good := trelloSign(body, secret, callback)

	if !trelloVerifySignature(body, good, secret, callback) {
		t.Fatal("valid signature should verify")
	}
	// Wrong secret.
	if trelloVerifySignature(body, good, "other", callback) {
		t.Fatal("signature must fail with the wrong secret")
	}
	// Wrong callback (Trello includes the callback URL in the HMAC).
	if trelloVerifySignature(body, good, secret, callback+"x") {
		t.Fatal("signature must fail with the wrong callback")
	}
	// Tampered body.
	if trelloVerifySignature([]byte(`{"action":{"type":"deleteCard"}}`), good, secret, callback) {
		t.Fatal("signature must fail when the body is tampered")
	}
	// Missing / malformed headers fail closed.
	if trelloVerifySignature(body, "", secret, callback) {
		t.Fatal("empty signature must fail closed")
	}
	if trelloVerifySignature(body, "not-base64!!", secret, callback) {
		t.Fatal("malformed base64 signature must fail closed")
	}
}
