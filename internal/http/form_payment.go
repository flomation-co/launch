package http

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"

	launch "flomation.app/automate/launch"
	stripewh "flomation.app/automate/launch/internal/stripe"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// defaultPaymentSecretRef is the ${secrets.X} reference used to resolve the
// Stripe secret key when a payment field leaves PaymentSecret blank.
const defaultPaymentSecretRef = "${secrets.stripe_secret_key}"

// createFormPaymentIntentBody is the (minimal) request body: only the draft
// submission id. The amount is NEVER accepted from the client — it is resolved
// server-side from the form definition so a hostile client cannot pay less.
type createFormPaymentIntentBody struct {
	SubmissionID string `json:"submission_id"`
}

// createFormPaymentIntent creates a Stripe hosted Checkout Session for a form
// that carries a payment field and returns its redirect URL. Validation and
// verification order:
//
//  1. id is a valid UUID and names a live form trigger.
//  2. the form actually has a payment component (else 400).
//  3. the login gate (require_login) is honoured, as on submit.
//  4. the request body carries a valid submission_id UUID.
//  5. a live draft exists for that id and belongs to THIS trigger.
//  6. the amount is resolved SERVER-SIDE (literal or ${data.X}) and converted
//     to minor units — the client never supplies it.
//  7. the Stripe secret key is resolved server-side (decrypted by the API) and
//     sanity-checked; it is never returned or logged.
//  8. the Checkout Session is created and its id recorded on the draft
//     (draft → finalising) BEFORE the URL is handed back, so the completion
//     callback can bind the returned session to this exact draft.
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

	comp, ok := paymentComponent(def)
	if !ok {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

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

	// The draft must exist and belong to this trigger. GetFormDraftAny returns
	// it in any state so a retry (the user cancelled Stripe and came back)
	// works: a draft in 'draft' or 'finalising' may (re)start payment; a
	// 'fired' draft has already completed and must not pay again.
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
	if draft.Status != "draft" && draft.Status != "finalising" {
		// Already fired (or an unexpected state) — nothing to pay for.
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

	// Resolve the Stripe secret key via the API (secrets are decrypted
	// server-side and never leave the API except through this controlled
	// resolve). It is used only to build the per-call Stripe client; it is
	// never returned to the browser nor logged.
	secretRef := strings.TrimSpace(comp.PaymentSecret)
	if secretRef == "" {
		secretRef = defaultPaymentSecretRef
	}
	secretKey := s.trigger.ResolveString(id, secretRef)
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
	successURL := fmt.Sprintf("%s/form/%s/complete?submission_id=%s&session_id={CHECKOUT_SESSION_ID}", base, id, sid)
	cancelURL := fmt.Sprintf("%s/form/%s?submission_id=%s", base, id, sid)

	url, sessionID, err := stripewh.CreateFormCheckoutSession(secretKey, stripewh.CheckoutParams{
		AmountMinor: amountMinor,
		Currency:    strings.ToLower(strings.TrimSpace(comp.Currency)),
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

	// Record the session id on the draft BEFORE returning the URL, so the
	// completion callback can require draft.payment_ref == session_id. Fail
	// closed if we can't record it — better to not redirect than to redirect
	// to a session we can never verify.
	marked, merr := s.db.MarkFormDraftFinalising(sid, sessionID)
	if merr != nil {
		log.WithError(merr).Error("form payment: unable to mark draft finalising")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if !marked {
		// Raced with a submit/complete that fired the draft — abort the payment.
		c.AbortWithStatus(http.StatusConflict)
		return
	}

	c.JSON(http.StatusOK, gin.H{"url": url})
}

// completeFormPayment is the success_url Stripe redirects the browser to after
// Checkout. It verifies the session is genuinely PAID and bound to the stored
// draft, then finalises the draft exactly once. Verification order:
//
//  1. id is a valid UUID naming a live form trigger; the form has a payment
//     component.
//  2. submission_id (UUID) and session_id query params are present.
//  3. the draft exists (any status) and belongs to this trigger.
//  4. an already-'fired' draft short-circuits to a success page (idempotent —
//     a duplicate callback must not fire twice).
//  5. the callback session_id MUST equal the draft's stored payment_ref — a
//     forged/mismatched session can never fire.
//  6. the Stripe session's payment_status MUST be "paid"; otherwise the user
//     is bounced back to the form to retry (draft stays 'finalising').
//  7. the draft is atomically claimed (finalising → fired); the first claimer
//     fires the flow, a late duplicate sees success.
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

	comp, ok := paymentComponent(def)
	if !ok {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	sid := c.Query("submission_id")
	sessionID := c.Query("session_id")
	if uuid.Validate(sid) != nil || sessionID == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

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

	// Idempotency: the flow already ran for this draft — show success without
	// re-verifying or re-firing.
	if draft.Status == "fired" {
		s.renderFormPaymentSuccess(c, def)
		return
	}

	// The callback session must be the one we handed off to. A mismatched or
	// forged session id can never fire the flow.
	if !draft.PaymentRef.Valid || draft.PaymentRef.String != sessionID {
		log.WithField("trigger_id", id).Warn("form payment complete: session id does not match stored payment ref")
		c.Redirect(http.StatusSeeOther, fmt.Sprintf("/form/%s?submission_id=%s", id, sid))
		return
	}

	// Resolve the secret key and confirm the session is genuinely PAID.
	secretRef := strings.TrimSpace(comp.PaymentSecret)
	if secretRef == "" {
		secretRef = defaultPaymentSecretRef
	}
	secretKey := s.trigger.ResolveString(id, secretRef)
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
		// Not paid — let the visitor retry from the form (draft still live).
		c.Redirect(http.StatusSeeOther, fmt.Sprintf("/form/%s?submission_id=%s", id, sid))
		return
	}

	// PAID. Atomically claim the finalising draft; the first caller fires.
	claimed, payload, ferr := s.db.FireFormDraft(sid, []string{"finalising"})
	if ferr != nil {
		log.WithError(ferr).Error("form payment complete: unable to claim draft")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if !claimed {
		// A concurrent callback already fired it — idempotent success.
		s.renderFormPaymentSuccess(c, def)
		return
	}

	if err := s.fireFormDraftPayload(c, tr, def, payload); err != nil {
		log.WithError(err).Error("form payment complete: unable to finalise draft payload")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	s.renderFormPaymentSuccess(c, def)
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
		amount := answerString(out[computeOutputKey(comp)])
		return amountToMinorUnits(amount, comp.Currency)
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

// fireFormDraftPayload unmarshals a claimed draft's stored answers, runs the
// exact same sanitisation pipeline as submitForm, and triggers the flow. The
// pipeline is security-critical: the draft payload is client-authored (via
// autosave), so option whitelists, display-only/read-only stripping and
// hidden-branch stripping must all be applied before the answers reach the
// flow. Note: the original form's URL query string is NOT carried across the
// Stripe redirect, so a read-only default sourced from ${query.X} re-bakes to
// empty here — a minor fidelity loss that preserves every security property
// (client values for read-only fields are still discarded).
func (s *Service) fireFormDraftPayload(c *gin.Context, tr *launch.Trigger, def formDefinition, payload json.RawMessage) error {
	body := map[string]interface{}{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &body); err != nil {
			return err
		}
	}
	// A stored draft never carries the fire-once marker, but strip defensively.
	delete(body, "__submission_id")

	cookie, _ := c.Cookie("flomation-token")
	token := extractSessionToken(c.GetHeader("Authorization"), cookie)
	userID := s.resolveSessionUser(token)

	ctx := substitutionContext{QueryParams: queryParamsMap(c)}
	if userID != "" {
		if vars, verr := s.loadUserVariables(userID); verr == nil {
			ctx.UserVariables = vars
		}
	}
	var dataOutputs map[string]interface{}
	if def.DataSource != nil && def.DataSource.FlowID != "" {
		if formUsesDataNamespace(def) || formHasDynamicOptions(def) {
			dataOutputs = s.formData.ResolveRaw(def.DataSource.FlowID, def.DataSource.TimeoutSeconds)
			ctx.DataVariables = flattenOutputs(dataOutputs)
		}
	}
	if formHasDynamicOptions(def) {
		def = bakeDynamicOptions(def, dataOutputs)
	}
	resolved := resolveFormForRender(def, ctx)
	final := sanitiseFormSubmission(body, resolved)
	if userID != "" {
		final["user_id"] = userID
	}

	go func() {
		if err := s.trigger.TriggerAs(tr, final, userID); err != nil {
			log.WithError(err).Error("unable to execute form payment trigger")
		}
	}()
	return nil
}

// renderFormPaymentSuccess shows the post-payment thank-you. When the form is
// configured to redirect on submit and the target is a safe http(s) URL, the
// browser is sent there; otherwise a minimal self-contained success page is
// rendered (using the author's success message when set).
func (s *Service) renderFormPaymentSuccess(c *gin.Context, def formDefinition) {
	msg := "Your response has been submitted successfully."
	if def.Submit != nil {
		if def.Submit.OnSubmit == "redirect" {
			if u := safeHTTPURL(def.Submit.RedirectURL); u != "" {
				c.Redirect(http.StatusSeeOther, u)
				return
			}
		}
		if strings.TrimSpace(def.Submit.SuccessMessage) != "" {
			msg = def.Submit.SuccessMessage
		}
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(paymentSuccessHTML(msg)))
}

// looksLikeStripeSecret reports whether a resolved value looks like a Stripe
// secret ("sk_…") or restricted ("rk_…") key. It exists only to distinguish
// "resolved to a real key" from "resolved to empty or an unresolved ${...}
// placeholder" — it neither logs nor returns the key.
func looksLikeStripeSecret(k string) bool {
	k = strings.TrimSpace(k)
	return strings.HasPrefix(k, "sk_") || strings.HasPrefix(k, "rk_")
}

// safeHTTPURL returns u only if it is an absolute http(s) URL, else "".
// Prevents an author-configured redirect from becoming a javascript:/data:
// vector on the server-rendered success page.
func safeHTTPURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return ""
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

// paymentSuccessHTML renders the minimal success page. The message is
// author-controlled, so it is HTML-escaped before interpolation.
func paymentSuccessHTML(message string) string {
	return `<!DOCTYPE html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Payment received</title>` +
		`<style>body{font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#161019;color:#fff;text-align:center;padding:1rem;}` +
		`.card{max-width:32rem;}.icon{font-size:3rem;}h2{margin:.5rem 0;}p{color:#c9c2d1;}</style></head>` +
		`<body><div class="card"><div class="icon">&#x2705;</div><h2>Thank you!</h2><p>` +
		html.EscapeString(message) + `</p></div></body></html>`
}
