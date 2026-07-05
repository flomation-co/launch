// Package stripe verifies and parses inbound Stripe webhook events for the
// Stripe Webhook Trigger. Signature verification reuses the official SDK's
// webhook package (the same mechanism the billing service uses).
package stripe

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"
)

// VerifyAndParse verifies the Stripe-Signature over the raw body against the
// endpoint signing secret and returns the parsed event. IgnoreAPIVersionMismatch
// mirrors the billing service so an account on a different API version still
// verifies.
func VerifyAndParse(body []byte, r *http.Request, signingSecret string) (stripe.Event, error) {
	sig := r.Header.Get("Stripe-Signature")
	if sig == "" {
		return stripe.Event{}, fmt.Errorf("missing Stripe-Signature header")
	}
	return webhook.ConstructEventWithOptions(body, sig, signingSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
}

// EventToData flattens a Stripe event into the map the executor's Stripe
// Webhook Trigger surfaces as outputs. The `data.object` fields that exist on
// most payment/subscription/invoice objects are pulled up to top-level keys;
// the full object JSON is included as `body`.
func EventToData(ev stripe.Event) map[string]interface{} {
	data := map[string]interface{}{
		"event_type":   ev.Type,
		"event_id":     ev.ID,
		"triggered_at": time.Unix(ev.Created, 0).UTC().Format(time.RFC3339),
	}

	obj := map[string]interface{}{}
	if ev.Data != nil {
		if ev.Data.Object != nil {
			obj = ev.Data.Object
		}
		if len(ev.Data.Raw) > 0 {
			data["body"] = string(ev.Data.Raw)
		}
	}

	data["object_type"] = str(obj["object"])
	data["object_id"] = str(obj["id"])
	data["customer_id"] = str(obj["customer"])
	data["currency"] = str(obj["currency"])
	data["status"] = str(obj["status"])
	if a, ok := obj["amount"].(float64); ok {
		data["amount"] = strconv.FormatInt(int64(a), 10)
	}
	data["content"] = "Stripe event: " + ev.Type
	return data
}

// MatchesFilter reports whether eventType is allowed by a comma-separated
// filter (e.g. "payment_intent.succeeded,invoice.paid"). Empty filter = all.
func MatchesFilter(eventType, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	for _, f := range strings.Split(filter, ",") {
		if strings.EqualFold(strings.TrimSpace(f), eventType) {
			return true
		}
	}
	return false
}

func str(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
