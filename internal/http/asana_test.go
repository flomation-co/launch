package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func asanaSign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestAsanaVerifySignature(t *testing.T) {
	body := []byte(`{"events":[{"action":"changed"}]}`)
	secret := "hook-secret-from-handshake"
	good := asanaSign(body, secret)

	if !asanaVerifySignature(body, good, secret) {
		t.Fatal("valid signature should verify")
	}
	if asanaVerifySignature(body, good, "other-secret") {
		t.Fatal("must fail with the wrong secret")
	}
	if asanaVerifySignature([]byte(`{"events":[{"action":"deleted"}]}`), good, secret) {
		t.Fatal("must fail when the body is tampered")
	}
	if asanaVerifySignature(body, "", secret) {
		t.Fatal("empty signature must fail closed")
	}
	if asanaVerifySignature(body, "nothex!!", secret) {
		t.Fatal("malformed hex signature must fail closed")
	}
}
