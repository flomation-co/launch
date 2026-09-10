package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	launch "flomation.app/automate/launch"
	stripewh "flomation.app/automate/launch/internal/stripe"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// defaultPaymentSecretRef is the ${secrets.X} reference used to resolve the
// Stripe secret key when a payment field leaves PaymentSecret blank.
const defaultPaymentSecretRef = "${secrets.stripe_secret_key}"

// statefulFieldTypes are field types whose value is resolved out-of-band (the
// user acts on the field, is redirected to an external service, and returns
// with THAT field's state updated) rather than typed in. Their satisfaction is
// tracked in the draft's field_states map, verified server-side. Payment is the
// first; future e-signature / ID-verify / OAuth-connect fields join here.
var statefulFieldTypes = map[string]struct{}{
	"payment": {},
}

// isStatefulFieldType reports whether a field type carries out-of-band state.
func isStatefulFieldType(t string) bool {
	_, ok := statefulFieldTypes[t]
	return ok
}

// paymentFieldState is the shape written into field_states for a payment field.
// It is opaque to the generic state machinery — only the payment handlers and
// isFieldStateSatisfied interpret it. "pending" means a Checkout Session was
// created and handed off; "complete" means Stripe confirmed the session paid
// and it was bound to this exact draft. AmountMinor/Currency are carried from
// intent → complete so the client can render "Paid ✓ · £X" without recomputing.
type paymentFieldState struct {
	Type        string `json:"type"`
	Status      string `json:"status"`
	SessionID   string `json:"session_id,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	Currency    string `json:"currency,omitempty"`
	PaidAt      string `json:"paid_at,omitempty"`
	// ReturnURL is where the embed completion handler sends the browser back to
	// (the developer's app). It is validated + stored at intent time — when the
	// request carries a gate-verified Origin — so the PUBLIC /complete callback
	// (which Stripe hits with no key/origin) never trusts a query-supplied URL.
	ReturnURL string `json:"return_url,omitempty"`
}

// isFieldStateSatisfied reports whether a stateful field's recorded state means
// it is "done" for the purpose of the submit gate. Dispatch is per-type; the
// state object is opaque to the generic gate. A future stateful type adds its
// own case here (and mirrors the check client-side). Payment is satisfied only
// once its state is "complete" — set exclusively by completeFormPayment after
// the Stripe paid + session-binding checks, so the client can never fake it.
func isFieldStateSatisfied(fieldType string, state json.RawMessage) bool {
	switch fieldType {
	case "payment":
		var st paymentFieldState
		if len(state) == 0 || json.Unmarshal(state, &st) != nil {
			return false
		}
		return st.Status == "complete"
	default:
		// Unknown stateful type — treat as not-satisfiable so a required field
		// of a type we don't understand blocks submit rather than passing.
		return false
	}
}

// unsatisfiedRequiredStatefulFields returns the names of required stateful
// fields (payment, …) that are VISIBLE for the given answers but whose recorded
// state is not yet satisfied. It is the server-authoritative submit gate: a
// crafted POST cannot bypass an unpaid required payment because satisfaction is
// read from the server-side field_states, never the body. Visibility is
// evaluated (mirroring the client) so a conditionally-hidden required stateful
// field never blocks submit. An empty result means submit may proceed.
func unsatisfiedRequiredStatefulFields(def formDefinition, answers map[string]interface{}, states map[string]json.RawMessage) []string {
	var missing []string
	// Table answers compare by their value_column for visibility rules.
	vis := tableComparableValues(def, answers)
	for _, page := range def.Pages {
		if page.VisibleIf != nil && len(page.VisibleIf.Rules) > 0 && !evalVisibility(page.VisibleIf, vis) {
			continue
		}
		for _, comp := range page.Components {
			if !comp.Required || !isStatefulFieldType(comp.Type) {
				continue
			}
			if comp.VisibleIf != nil && len(comp.VisibleIf.Rules) > 0 && !evalVisibility(comp.VisibleIf, vis) {
				continue
			}
			if !isFieldStateSatisfied(comp.Type, states[comp.Name]) {
				missing = append(missing, comp.Name)
			}
		}
	}
	return missing
}

// resolvePaymentComponent finds the payment field a request refers to. With an
// explicit field name it matches by Name (the multi-payment case); with an
// empty name it falls back to the first payment field (the common single-
// payment form). ok is false when there is no matching payment field.
func resolvePaymentComponent(def formDefinition, field string) (formComponent, bool) {
	if strings.TrimSpace(field) == "" {
		return paymentComponent(def)
	}
	for _, page := range def.Pages {
		for _, comp := range page.Components {
			if comp.Type == "payment" && comp.Name == field {
				return comp, true
			}
		}
	}
	return formComponent{}, false
}

// createFormPaymentIntentBody is the request body for a payment-intent: the
// draft submission id and the target payment field's name. The amount is NEVER
// accepted from the client — it is resolved server-side from the form
// definition so a hostile client cannot pay less.
type createFormPaymentIntentBody struct {
	SubmissionID string `json:"submission_id"`
	Field        string `json:"field"`
}

// createFormPaymentIntent creates a Stripe hosted Checkout Session for ONE
// payment field of an in-progress form and returns its redirect URL. Unlike a
// terminal "pay to submit", this is a mid-form action: the draft stays 'draft'
// throughout and only that field's state advances (→ pending). Verification
// order:
//
//  1. id is a valid UUID and names a live form trigger.
//  2. the request body carries a valid submission_id UUID and a field name.
//  3. that field names a payment component on the form (else 400).
//  4. the login gate (require_login) is honoured, as on submit.
//  5. a live draft exists for that id and belongs to THIS trigger, and is not
//     already fired.
//  6. the amount is resolved SERVER-SIDE (literal, ${data.X}, or a compute
//     flow) and converted to minor units — the client never supplies it.
//  7. the Stripe secret key is resolved server-side (decrypted by the API) and
//     sanity-checked; it is never returned or logged.
//  8. the Checkout Session is created and recorded as the field's PENDING state
//     (with the session id) BEFORE the URL is handed back, so the completion
//     callback can bind the returned session to this exact field + draft.
func (s *Service) createFormPaymentIntent(c *gin.Context) {
	id := c.Param("id")
	if id == "" || uuid.Validate(id) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if tr == nil || tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	def, _ := parseFormDefinition(tr.Data)

	// Login gate parity with submitForm — a require_login form must not start
	// a payment for an anonymous visitor.
	cookie, _ := c.Cookie("flomation-token")
	token := extractSessionToken(c.GetHeader("Authorization"), cookie)
	userID := s.resolveSessionUser(token)
	if def.RequireLogin && userID == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	var body createFormPaymentIntentBody
	if err := c.BindJSON(&body); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	sid := body.SubmissionID
	if uuid.Validate(sid) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	comp, ok := resolvePaymentComponent(def, body.Field)
	if !ok {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	field := comp.Name

	// The draft must exist and belong to this trigger. A 'fired' draft has
	// already been submitted — nothing left to pay for.
	draft, derr := s.db.GetFormDraftAny(sid)
	if derr != nil {
		log.WithError(derr).Error("form payment: unable to load draft")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if draft == nil || draft.TriggerID != id {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if draft.Status != "draft" {
		c.AbortWithStatus(http.StatusConflict)
		return
	}

	// The draft's autosaved answers feed a value_source pricing flow (the
	// amount is derived from what the visitor entered). They are client-
	// authored, but the flow — not the client — decides the price, and the
	// resulting amount is validated by amountToMinorUnits. An unparseable or
	// absent payload is treated as "no answers" (empty map), never an error.
	answers := map[string]interface{}{}
	if len(draft.Payload) > 0 {
		if uerr := json.Unmarshal(draft.Payload, &answers); uerr != nil {
			answers = map[string]interface{}{}
		}
	}
	delete(answers, "__submission_id")

	// Resolve the amount server-side. NEVER trust a client-supplied amount.
	amountMinor, err := s.resolveFormAmountMinor(c, def, comp, answers)
	if err != nil {
		log.WithError(err).Warn("form payment: invalid amount")
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payment amount"})
		return
	}
	if amountMinor <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payment amount"})
		return
	}
	currency := strings.ToLower(strings.TrimSpace(comp.Currency))

	// Resolve the Stripe secret key via the API (secrets are decrypted
	// server-side and never leave the API except through this controlled
	// resolve). It is used only to build the per-call Stripe client; it is
	// never returned to the browser nor logged.
	secretKey := s.resolvePaymentSecret(id, comp)
	if !looksLikeStripeSecret(secretKey) {
		log.WithField("trigger_id", id).Error("form payment: Stripe secret key did not resolve")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	productName := strings.TrimSpace(comp.Label)
	if productName == "" {
		productName = strings.TrimSpace(def.Title)
	}
	if productName == "" {
		productName = "Payment"
	}

	base := s.formPublicBaseURL(c)
	// {CHECKOUT_SESSION_ID} is a Stripe placeholder it substitutes on redirect.
	// success_url returns to the generic completion handler, scoped to THIS
	// field; cancel_url drops the user straight back onto the form.
	successURL := fmt.Sprintf("%s/form/%s/complete?submission_id=%s&field=%s&session_id={CHECKOUT_SESSION_ID}",
		base, id, sid, url.QueryEscape(field))
	cancelURL := fmt.Sprintf("%s/form/%s?submission_id=%s", base, id, sid)

	url, sessionID, err := stripewh.CreateFormCheckoutSession(secretKey, stripewh.CheckoutParams{
		AmountMinor: amountMinor,
		Currency:    currency,
		ProductName: productName,
		SuccessURL:  successURL,
		CancelURL:   cancelURL,
	})
	if err != nil {
		// Log server-side only; never surface Stripe internals (or the key).
		log.WithError(err).Error("form payment: Stripe Checkout Session create failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "unable to start payment"})
		return
	}

	// Record the session id as the field's PENDING state BEFORE returning the
	// URL, so the completion callback can require the returned session to match
	// this exact field's pending session. Fail closed if we can't record it —
	// better to not redirect than to redirect to a session we can never verify.
	pending, _ := json.Marshal(paymentFieldState{
		Type:        "payment",
		Status:      "pending",
		SessionID:   sessionID,
		AmountMinor: amountMinor,
		Currency:    currency,
	})
	marked, merr := s.db.SetFieldState(sid, field, pending)
	if merr != nil {
		log.WithError(merr).Error("form payment: unable to record pending field state")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if !marked {
		// The draft is no longer a live 'draft' (fired/expired) — abort payment.
		c.AbortWithStatus(http.StatusConflict)
		return
	}

	c.JSON(http.StatusOK, gin.H{"url": url})
}

// completeFormPayment is the success_url Stripe redirects the browser to after
// Checkout. It is the generic stateful-field return handler, dispatched by the
// field's type; for a payment field it verifies the session is genuinely PAID
// and bound to that field's pending state, marks the field COMPLETE, and 302s
// the user back to the form (same page, field now "Paid ✓") — it does NOT fire
// the flow. Verification order:
//
//  1. id is a valid UUID naming a live form trigger.
//  2. submission_id (UUID), field, and session_id query params are present and
//     field names a payment component.
//  3. the draft exists (any status) and belongs to this trigger.
//  4. an already-'complete' field short-circuits back to the form (idempotent —
//     a duplicate callback must not double anything).
//  5. the callback session_id MUST equal the field's pending session_id — a
//     forged/mismatched session can never complete the field.
//  6. the Stripe session's payment_status MUST be "paid"; otherwise the user is
//     bounced back to the form to retry (state stays 'pending').
//  7. the field's state is advanced to 'complete'.
func (s *Service) completeFormPayment(c *gin.Context) {
	id := c.Param("id")
	if id == "" || uuid.Validate(id) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if tr == nil || tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	def, _ := parseFormDefinition(tr.Data)

	sid := c.Query("submission_id")
	sessionID := c.Query("session_id")
	field := c.Query("field")
	if uuid.Validate(sid) != nil || sessionID == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	comp, ok := resolvePaymentComponent(def, field)
	if !ok {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	field = comp.Name
	backToForm := fmt.Sprintf("/form/%s?submission_id=%s&updated=%s", id, sid, url.QueryEscape(field))
	retryForm := fmt.Sprintf("/form/%s?submission_id=%s", id, sid)

	draft, derr := s.db.GetFormDraftAny(sid)
	if derr != nil {
		log.WithError(derr).Error("form payment complete: unable to load draft")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if draft == nil || draft.TriggerID != id {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	// Load the field's current state. The pending state (written at intent)
	// carries the session id we handed off and the amount to lock in.
	states, serr := s.db.GetFieldStates(sid)
	if serr != nil {
		log.WithError(serr).Error("form payment complete: unable to load field states")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	var st paymentFieldState
	if raw, present := states[field]; present && len(raw) > 0 {
		_ = json.Unmarshal(raw, &st)
	}

	// Idempotency: the field already completed — just return to the form.
	if st.Status == "complete" {
		c.Redirect(http.StatusSeeOther, backToForm)
		return
	}

	// The callback session must be the one we handed off for THIS field. A
	// mismatched or forged session id can never complete it.
	if st.SessionID == "" || st.SessionID != sessionID {
		log.WithField("trigger_id", id).Warn("form payment complete: session id does not match pending field state")
		c.Redirect(http.StatusSeeOther, retryForm)
		return
	}

	// Resolve the secret key and confirm the session is genuinely PAID.
	secretKey := s.resolvePaymentSecret(id, comp)
	if !looksLikeStripeSecret(secretKey) {
		log.WithField("trigger_id", id).Error("form payment complete: Stripe secret key did not resolve")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	status, err := stripewh.RetrieveCheckoutPaymentStatus(secretKey, sessionID)
	if err != nil {
		log.WithError(err).Error("form payment complete: Stripe session retrieve failed")
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	if status != "paid" {
		// Not paid — let the visitor retry from the form (state stays pending).
		c.Redirect(http.StatusSeeOther, retryForm)
		return
	}

	// PAID. Advance the field to 'complete', locking in the amount charged.
	complete, _ := json.Marshal(paymentFieldState{
		Type:        "payment",
		Status:      "complete",
		SessionID:   sessionID,
		AmountMinor: st.AmountMinor,
		Currency:    st.Currency,
		PaidAt:      time.Now().UTC().Format(time.RFC3339),
	})
	if marked, merr := s.db.SetFieldState(sid, field, complete); merr != nil {
		log.WithError(merr).Error("form payment complete: unable to record complete field state")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	} else if !marked {
		// The draft is no longer live (submitted/expired between paid and here).
		// The payment succeeded regardless; send the user back to the form,
		// which will reflect whatever state remains.
		log.WithField("trigger_id", id).Warn("form payment complete: draft no longer live when recording complete state")
	}

	c.Redirect(http.StatusSeeOther, backToForm)
}

// resolvePaymentSecret resolves the Stripe secret key for a payment field via
// the API's controlled ${secrets.X} resolve, defaulting the reference when the
// field leaves PaymentSecret blank. The key is used only to build a per-call
// Stripe client; it is never returned to the browser nor logged.
func (s *Service) resolvePaymentSecret(triggerID string, comp formComponent) string {
	secretRef := strings.TrimSpace(comp.PaymentSecret)
	if secretRef == "" {
		secretRef = defaultPaymentSecretRef
	}
	return s.trigger.ResolveString(triggerID, secretRef)
}

// resolveFormAmountMinor resolves the payment field's amount to minor units.
// The amount is ALWAYS resolved server-side — the client never supplies it.
// Resolution order:
//
//   - value_source set: the amount is COMPUTED by running the named flow with
//     the draft answers as input (${input.X}) and reading computeOutputKey from
//     its outputs. This is the car-park case — a pricing flow whose output is
//     the charge. It takes precedence over any literal/${data.X} amount.
//   - otherwise: a literal amount is used directly; an amount referencing a
//     variable (contains "${") is resolved against the form's data source
//     (${data.X}) and URL query params, as labels/read-only defaults do.
//
// The answers are the draft's autosaved submission — the same client-authored
// map submitForm sanitises. The flow, not the client, decides the price.
func (s *Service) resolveFormAmountMinor(c *gin.Context, def formDefinition, comp formComponent, answers map[string]interface{}) (int64, error) {
	if strings.TrimSpace(comp.ValueSource) != "" {
		out := s.formData.ResolveComputed(comp.ValueSource, answers, 0)
		key := computeOutputKey(comp)
		raw, ok := out[key]
		if !ok || answerString(raw) == "" {
			// The compute flow ran but produced nothing under the configured
			// output key. This is the common misconfiguration: value_output is
			// unset (so it defaults to the field name, which rarely matches the
			// flow's Set Output key). Fail loudly with the specifics — the
			// caller logs this — instead of a generic "invalid amount".
			available := make([]string, 0, len(out))
			for k := range out {
				available = append(available, k)
			}
			sort.Strings(available)
			return 0, fmt.Errorf("payment flow %s produced no value for output %q (field %q); available outputs: %v",
				comp.ValueSource, key, comp.Name, available)
		}
		return amountToMinorUnits(answerString(raw), comp.Currency)
	}

	amount := comp.Amount
	if strings.Contains(amount, "${") {
		ctx := substitutionContext{QueryParams: queryParamsMap(c)}
		if def.DataSource != nil && def.DataSource.FlowID != "" {
			dataOutputs := s.formData.ResolveRaw(def.DataSource.FlowID, def.DataSource.TimeoutSeconds)
			ctx.DataVariables = flattenOutputs(dataOutputs)
		}
		amount = applySubstitutions(amount, ctx)
	}
	return amountToMinorUnits(amount, comp.Currency)
}

// looksLikeStripeSecret reports whether a resolved value looks like a Stripe
// secret ("sk_…") or restricted ("rk_…") key. It exists only to distinguish
// "resolved to a real key" from "resolved to empty or an unresolved ${...}
// placeholder" — it neither logs nor returns the key.
func looksLikeStripeSecret(k string) bool {
	k = strings.TrimSpace(k)
	return strings.HasPrefix(k, "sk_") || strings.HasPrefix(k, "rk_")
}

// formPublicBaseURL returns the externally-reachable base URL used to build
// the Stripe success/cancel links back to this form. It prefers the configured
// PublicURL (e.g. "https://launch.flomation.app") and falls back to deriving
// scheme+host from the inbound request (honouring X-Forwarded-Proto behind a
// TLS-terminating proxy).
func (s *Service) formPublicBaseURL(c *gin.Context) string {
	if b := strings.TrimRight(s.config.PublicURL, "/"); b != "" {
		return b
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	return scheme + "://" + c.Request.Host
}
