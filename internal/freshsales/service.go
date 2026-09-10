// Package freshsales verifies and parses inbound Freshsales workflow webhooks
// for the Freshsales Webhook Trigger.
//
// SECURITY: Freshsales publishes no webhook signature, signing secret or HMAC —
// webhooks are configured in the customer's Workflows UI and arrive
// unauthenticated, exactly as Apollo's do. Authenticity therefore rests on two
// things: the unguessable /webhook/:id route, and an author-chosen shared
// secret compared in constant time.
//
// Freshsales does at least give the admin somewhere sensible to put that
// secret, which Apollo does not. A webhook action offers Token authentication
// and custom headers, so the secret is accepted three ways:
//
//	Authorization: Token <secret>          — its own Token auth field
//	X-Flomation-Webhook-Secret: <secret>   — its custom-headers field
//	?secret=<secret>                       — appended to the URL, last resort
//
// A future hardening step is to re-fetch the referenced record from the
// Freshsales API before firing, which would make a forged body inert.
package freshsales

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SecretHeader carries the shared secret when the customer uses the
// custom-headers field rather than Token auth.
//
// Freshsales refuses to run a workflow whose custom header name contains a
// space, so this name deliberately has none.
const SecretHeader = "X-Flomation-Webhook-Secret" // #nosec G101 — HTTP header name, not a credential

// tokenPrefix is what Freshsales' Token authentication puts in front of the
// value in the Authorization header.
const tokenPrefix = "Token "

// PresentedSecret extracts the secret from wherever the customer configured it.
//
// Returns "" when none is present, which the constant-time comparison below
// treats as a mismatch rather than a special case.
func PresentedSecret(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get("secret")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get(SecretHeader)); v != "" {
		return v
	}

	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return ""
	}
	// "Token abc123" is what Freshsales' Token auth sends. Accept a bare value
	// too, since an admin may paste the secret straight into the field.
	if len(auth) > len(tokenPrefix) && strings.EqualFold(auth[:len(tokenPrefix)], tokenPrefix) {
		return strings.TrimSpace(auth[len(tokenPrefix):])
	}
	if strings.Contains(auth, " ") {
		// Some other scheme (Basic, Bearer): not ours, do not guess.
		return ""
	}
	return auth
}

// VerifyAndParse checks the presented secret against the configured one in
// constant time, then parses the JSON body.
//
// A missing or empty configured secret is a failure, never a pass: an
// unauthenticated webhook must never fire a flow.
func VerifyAndParse(body []byte, r *http.Request, secret string) (map[string]interface{}, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("no webhook secret configured")
	}

	presented := PresentedSecret(r)
	// ConstantTimeCompare returns 0 when the lengths differ, so this also
	// covers the empty case without an early, timing-variable return.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
		return nil, fmt.Errorf("webhook secret mismatch")
	}

	payload := map[string]interface{}{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
	}
	return payload, nil
}

// EventToData flattens a Freshsales webhook payload into the outputs the
// executor's trigger surfaces.
//
// The payload shape is whatever the workflow author configured — Freshsales
// lets them choose the fields — so the identifying values are pulled up to
// stable keys where they can be found, and every top-level scalar is also
// spread as a bare key so a flow can reference anything that arrived.
func EventToData(payload map[string]interface{}, rawBody []byte) map[string]interface{} {
	eventType := firstNonEmpty(
		str(payload["event_type"]), str(payload["event"]),
		str(payload["type"]), str(payload["action"]),
	)
	entityType := firstNonEmpty(
		str(payload["entity_type"]), str(payload["entity"]), str(payload["module"]),
	)

	data := map[string]interface{}{
		"event_type":   eventType,
		"entity_type":  entityType,
		"record_id":    recordID(payload),
		"account_id":   firstNonEmpty(str(payload["account_id"]), str(payload["freshsales_account_id"])),
		"body":         string(rawBody),
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
		"content":      summarise(eventType, entityType),
	}

	for k, v := range payload {
		if _, exists := data[k]; exists {
			continue
		}
		switch v.(type) {
		case string, float64, bool:
			data[k] = v
		}
	}
	return data
}

// recordID finds the id of the record the event is about.
//
// Freshsales nests the record under its entity name when the author picks the
// whole object ({"contact":{"id":144,…}}), and puts an id at the top level when
// they pick individual fields, so both are checked.
func recordID(payload map[string]interface{}) string {
	for _, key := range []string{"id", "record_id", "entity_id", "object_id"} {
		if v := str(payload[key]); v != "" {
			return v
		}
	}
	for _, key := range []string{"contact", "sales_account", "account", "deal", "lead"} {
		if nested, ok := payload[key].(map[string]interface{}); ok {
			if v := str(nested["id"]); v != "" {
				return v
			}
		}
	}
	return ""
}

func summarise(eventType, entityType string) string {
	switch {
	case eventType != "" && entityType != "":
		return "Freshsales event: " + entityType + " " + eventType
	case eventType != "":
		return "Freshsales event: " + eventType
	case entityType != "":
		return "Freshsales event on " + entityType
	}
	return "Freshsales webhook received"
}

// MatchesFilter reports whether an event is allowed by a comma-separated
// filter. An empty filter matches everything.
//
// The filter is compared against the event type AND the "entity.event" pair, so
// an author can write either "created" or "contact.created" and get what they
// expect.
func MatchesFilter(eventType, entityType, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	combined := eventType
	if entityType != "" && eventType != "" {
		combined = entityType + "." + eventType
	}
	for _, f := range strings.Split(filter, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.EqualFold(f, eventType) || strings.EqualFold(f, combined) {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func str(v interface{}) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case nil:
		return ""
	default:
		return ""
	}
}
