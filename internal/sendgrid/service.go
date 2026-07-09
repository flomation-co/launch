// Package sendgrid holds the launch-side webhook helpers for SendGrid
// triggers: signed-event-webhook signature verification (ECDSA), event-batch
// parsing, event filtering, and output shaping. The registration lifecycle
// (creating/deleting the account's event webhook via the multi-webhook API)
// lives in internal/http/sendgrid.go alongside the other providers.
package sendgrid

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
)

// VerifySignature validates a SendGrid signed-event-webhook delivery. SendGrid
// sends an ASN.1/DER ECDSA signature (Base64, X-Twilio-Email-Event-Webhook-
// Signature) over SHA-256 of the timestamp header string immediately followed
// by the RAW request body, verifiable with the Base64 PKIX public key returned
// when signing was enabled on the webhook. Every failure mode — missing key,
// missing headers, undecodable key/signature, non-ECDSA key, digest mismatch —
// fails closed.
func VerifySignature(publicKey, timestamp, signature string, body []byte) error {
	if publicKey == "" {
		return fmt.Errorf("no public key configured")
	}
	if timestamp == "" {
		return fmt.Errorf("missing X-Twilio-Email-Event-Webhook-Timestamp header")
	}
	if signature == "" {
		return fmt.Errorf("missing X-Twilio-Email-Event-Webhook-Signature header")
	}

	keyDER, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil {
		return fmt.Errorf("malformed public key encoding: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(keyDER)
	if err != nil {
		return fmt.Errorf("malformed public key: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("public key is not an ECDSA key")
	}

	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("malformed signature encoding: %w", err)
	}

	digest := sha256.New()
	digest.Write([]byte(timestamp))
	digest.Write(body)
	if !ecdsa.VerifyASN1(pub, digest.Sum(nil), sig) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

// ParseEvents decodes a webhook delivery. Each POST body is a JSON ARRAY of
// event objects (a single delivery batches multiple events), so the flow is
// fired once per matched event, not once per delivery.
func ParseEvents(body []byte) ([]map[string]interface{}, error) {
	var events []map[string]interface{}
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("invalid SendGrid webhook body: %w", err)
	}
	return events, nil
}

// MatchesFilter checks whether a single event matches the trigger's selection
// (payload event names, e.g. "delivered", "bounce", "spamreport"). An empty
// selection matches all. "blocked" is special-cased: it has no webhook-settings
// toggle of its own and SendGrid reports blocks either as a native
// event=="blocked" or as event=="bounce" with type=="blocked", so selecting it
// matches both shapes.
func MatchesFilter(event map[string]interface{}, selected []string) bool {
	if len(selected) == 0 {
		return true
	}
	name, _ := event["event"].(string)
	bounceType, _ := event["type"].(string)
	for _, sel := range selected {
		if sel == name {
			return true
		}
		if sel == "blocked" && name == "bounce" && bounceType == "blocked" {
			return true
		}
	}
	return false
}

// EventOutputs shapes one event object from a delivery batch into the trigger
// node's outputs. Every declared output is pre-initialised to "" so flows can
// wire fields a given event type doesn't carry; numerics (timestamp, attempt,
// tls) and booleans (sg_machine_open) are stringified; category — which
// SendGrid sends as either a string or an array — is JSON-encoded when it is
// an array; body carries the single event's JSON (not the whole batch).
func EventOutputs(event map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"event":           "",
		"email":           "",
		"timestamp":       "",
		"sg_message_id":   "",
		"sg_event_id":     "",
		"sg_machine_open": "",
		"type":            "",
		"reason":          "",
		"status":          "",
		"response":        "",
		"attempt":         "",
		"category":        "",
		"url":             "",
		"useragent":       "",
		"ip":              "",
		"tls":             "",
		"body":            "",
	}
	// Output names match the payload field names for everything except body
	// (the whole event) and category (string-or-array).
	for k := range out {
		if k == "body" || k == "category" {
			continue
		}
		if s := eventStr(event[k]); s != "" {
			out[k] = s
		}
	}
	switch cat := event["category"].(type) {
	case string:
		out["category"] = cat
	case []interface{}:
		if b, err := json.Marshal(cat); err == nil {
			out["category"] = string(b)
		}
	}
	if b, err := json.Marshal(event); err == nil {
		out["body"] = string(b)
	}
	return out
}

// eventStr stringifies a SendGrid event field. timestamp/tls/attempt can come
// through as JSON numbers (float64) and sg_machine_open as a bool; integers
// are rendered without a decimal point. Strings pass through; anything else
// yields "".
func eventStr(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}
