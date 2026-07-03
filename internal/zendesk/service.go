// Package zendesk holds the pure, stateless helpers for the Zendesk webhook
// trigger: request signature verification, inbound event parsing, and the
// Support API auth-header builder. The subscription lifecycle (creating the
// Zendesk webhook + business rule) and inbound routing live in
// internal/http/zendesk.go; keeping the crypto/parse logic here makes it
// unit-testable without a live Service.
package zendesk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Signature headers Zendesk sends on every webhook delivery.
const (
	signatureHeader          = "X-Zendesk-Webhook-Signature"
	signatureTimestampHeader = "X-Zendesk-Webhook-Signature-Timestamp"
)

// AuthHeader builds the Authorization header value for a Support API request.
// An OAuth access token (bearer) takes precedence; otherwise the agent email +
// API token are HTTP Basic-encoded as "{email}/token:{api_token}". Returns ""
// when neither credential form is available.
func AuthHeader(email, apiToken, oauthToken string) string {
	if oauthToken != "" {
		return "Bearer " + oauthToken
	}
	if email != "" && apiToken != "" {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(email+"/token:"+apiToken))
	}
	return ""
}

// VerifySignature validates a Zendesk webhook delivery. Zendesk computes the
// signature as base64(HMAC-SHA256(signing_secret, timestamp + body)), where
// timestamp is the X-Zendesk-Webhook-Signature-Timestamp header and body is the
// raw request body; the base64 result is sent in X-Zendesk-Webhook-Signature.
// The comparison is constant-time.
func VerifySignature(signingSecret string, body []byte, r *http.Request) error {
	sig := r.Header.Get(signatureHeader)
	if sig == "" {
		return fmt.Errorf("missing %s header", signatureHeader)
	}
	timestamp := r.Header.Get(signatureTimestampHeader)
	if timestamp == "" {
		return fmt.Errorf("missing %s header", signatureTimestampHeader)
	}

	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(timestamp))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// ParseEvent builds the trigger's output data from a Zendesk webhook body. The
// body is the JSON message template rendered by the Zendesk business rule (see
// internal/http/zendesk.go's webhookMessage) — its keys already match the
// trigger node's output ports (ticket_id, subject, status, …). The parsed
// object is exposed as "payload", and the raw body is preserved so downstream
// nodes can read any field. A non-JSON body is not fatal: the raw body is still
// carried so the flow can inspect it.
func ParseEvent(body []byte) (map[string]interface{}, error) {
	data := map[string]interface{}{
		"body":         string(body),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err == nil && payload != nil {
		data["payload"] = payload
		for _, k := range []string{"ticket_id", "subject", "status", "priority", "requester_email", "description", "via"} {
			if v, ok := payload[k]; ok {
				data[k] = v
			}
		}
	}

	return data, nil
}
