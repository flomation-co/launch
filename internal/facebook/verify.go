// Package facebook provides Facebook webhook verification, event parsing,
// and Messenger API helpers for the Launch service.
package facebook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
)

// VerifySignature validates the HMAC-SHA256 signature in the X-Hub-Signature-256
// header against the request body using the Facebook App Secret.
func VerifySignature(appSecret string, body []byte, r *http.Request) error {
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}

	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}
