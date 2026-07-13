package stripe

import (
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/client" //nolint:staticcheck // per-call client for multi-tenant key isolation; migration to stripe.Client is a cross-repo concern (billing/executor use the same pattern)
)

// CheckoutParams are the inputs to a hosted Checkout Session for a native
// form payment. AmountMinor is the charge in the currency's smallest unit
// (pence, cents, whole yen) — the caller is responsible for the major→minor
// conversion so this layer stays a thin Stripe wrapper.
type CheckoutParams struct {
	AmountMinor int64
	Currency    string
	ProductName string
	SuccessURL  string
	CancelURL   string
}

// CreateFormCheckoutSession creates a Stripe hosted Checkout Session in
// payment mode and returns the redirect URL and session id.
//
// Key isolation is critical: a per-call *client.API is built via sc.Init with
// the caller-supplied secret key. The package-level stripe.Key is NEVER set —
// the executor / launch are multi-tenant and one form's key must never leak
// into another's request.
func CreateFormCheckoutSession(secretKey string, p CheckoutParams) (url string, sessionID string, err error) {
	sc := &client.API{}     //nolint:staticcheck // deprecated client.API — see import
	sc.Init(secretKey, nil) //nolint:staticcheck // deprecated sc.Init — see import

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String(p.Currency),
				UnitAmount: stripe.Int64(p.AmountMinor),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String(p.ProductName),
				},
			},
			Quantity: stripe.Int64(1),
		}},
		SuccessURL: stripe.String(p.SuccessURL),
		CancelURL:  stripe.String(p.CancelURL),
	}

	session, err := sc.CheckoutSessions.New(params) //nolint:staticcheck // deprecated client method — see import
	if err != nil {
		return "", "", err
	}
	return session.URL, session.ID, nil
}

// RetrieveCheckoutPaymentStatus fetches a Checkout Session and returns its
// payment_status ("paid", "unpaid", "no_payment_required"). The caller must
// require "paid" before firing the form's flow. Uses a per-call client keyed
// by the caller's secret key (never the package-level stripe.Key).
func RetrieveCheckoutPaymentStatus(secretKey, sessionID string) (string, error) {
	sc := &client.API{}     //nolint:staticcheck // deprecated client.API — see import
	sc.Init(secretKey, nil) //nolint:staticcheck // deprecated sc.Init — see import

	session, err := sc.CheckoutSessions.Get(sessionID, nil) //nolint:staticcheck // deprecated client method — see import
	if err != nil {
		return "", err
	}
	return string(session.PaymentStatus), nil
}
