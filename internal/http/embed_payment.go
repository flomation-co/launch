package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	launch "flomation.app/automate/launch"
	stripewh "flomation.app/automate/launch/internal/stripe"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// createEmbedPaymentIntentBody is the SDK's payment-intent request. return_url is
// the developer app's page to come back to after Checkout; it is validated
// against the (gate-verified) request Origin and stored, so the public /complete
// callback never trusts a caller-supplied URL.
type createEmbedPaymentIntentBody struct {
	SubmissionID string `json:"submission_id"`
	Field        string `json:"field"`
	ReturnURL    string `json:"return_url"`
}

// validReturnURL reports whether returnURL is an http(s) URL under the given
// (already gate-validated) origin — tying the post-payment redirect back to the
// same origin the SDK is embedded on, so it can't become an open redirect.
func validReturnURL(returnURL, origin string) bool {
	if returnURL == "" {
		return false
	}
	if !strings.HasPrefix(returnURL, "http://") && !strings.HasPrefix(returnURL, "https://") {
		return false
	}
	if origin != "" && !strings.HasPrefix(returnURL, origin) {
		return false
	}
	return true
}

// appendQuery adds key=value pairs to a URL, choosing ? or & correctly.
func appendQuery(u string, kv ...string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, url.QueryEscape(kv[i])+"="+url.QueryEscape(kv[i+1]))
	}
	if len(parts) == 0 {
		return u
	}
	return u + sep + strings.Join(parts, "&")
}

// createEmbedPaymentIntent starts a Stripe Checkout for a payment field of an
// embedded form and returns the redirect URL. It mirrors createFormPaymentIntent
// but: (1) it is behind the embed gate (key + origin already validated), and
// (2) it stores a gate-validated return_url so the public completion callback can
// send the browser back to the developer's app. The amount is resolved
// SERVER-SIDE from the draft answers — never trusted from the client.
func (s *Service) createEmbedPaymentIntent(c *gin.Context) {
	id := c.Param("id")
	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil || tr == nil || tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	def, _ := parseFormDefinition(tr.Data)

	var body createEmbedPaymentIntentBody
	if err := c.BindJSON(&body); err != nil || uuid.Validate(body.SubmissionID) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	sid := body.SubmissionID

	origin := c.GetHeader("Origin")
	if !validReturnURL(body.ReturnURL, origin) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "return_url must be an http(s) URL under the embedding origin"})
		return
	}

	comp, ok := resolvePaymentComponent(def, body.Field)
	if !ok {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	field := comp.Name

	draft, derr := s.db.GetFormDraftAny(sid)
	if derr != nil {
		log.WithError(derr).Error("embed payment: unable to load draft")
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

	answers := map[string]interface{}{}
	if len(draft.Payload) > 0 {
		if uerr := json.Unmarshal(draft.Payload, &answers); uerr != nil {
			answers = map[string]interface{}{}
		}
	}
	delete(answers, "__submission_id")

	amountMinor, err := s.resolveFormAmountMinor(c, def, comp, answers)
	if err != nil || amountMinor <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payment amount"})
		return
	}
	currency := strings.ToLower(strings.TrimSpace(comp.Currency))

	secretKey := s.resolvePaymentSecret(id, comp)
	if !looksLikeStripeSecret(secretKey) {
		log.WithField("trigger_id", id).Error("embed payment: Stripe secret key did not resolve")
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
	// success_url returns to the PUBLIC embed completion handler (Stripe hits it
	// with no key/origin); the return_url is read from the stored field state
	// there, not from this URL, so it can't be tampered with.
	successURL := fmt.Sprintf("%s/v1/embed/form/%s/complete?submission_id=%s&field=%s&session_id={CHECKOUT_SESSION_ID}",
		base, id, sid, url.QueryEscape(field))
	cancelURL := appendQuery(body.ReturnURL, "flo_payment", "cancelled")

	checkoutURL, sessionID, err := stripewh.CreateFormCheckoutSession(secretKey, stripewh.CheckoutParams{
		AmountMinor: amountMinor,
		Currency:    currency,
		ProductName: productName,
		SuccessURL:  successURL,
		CancelURL:   cancelURL,
	})
	if err != nil {
		log.WithError(err).Error("embed payment: Stripe Checkout Session create failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "unable to start payment"})
		return
	}

	pending, _ := json.Marshal(paymentFieldState{
		Type:        "payment",
		Status:      "pending",
		SessionID:   sessionID,
		AmountMinor: amountMinor,
		Currency:    currency,
		ReturnURL:   body.ReturnURL,
	})
	marked, merr := s.db.SetFieldState(sid, field, pending)
	if merr != nil {
		log.WithError(merr).Error("embed payment: unable to record pending field state")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if !marked {
		c.AbortWithStatus(http.StatusConflict)
		return
	}

	c.JSON(http.StatusOK, gin.H{"url": checkoutURL})
}

// completeEmbedPayment is the PUBLIC success_url Stripe redirects the browser to
// (no key/origin — Stripe is the caller). It verifies the session is genuinely
// PAID and bound to the field's pending state, marks the field complete, and
// 302s to the STORED return_url (the developer's app) with flo_paid so the SDK
// can resume and show the field paid. Security rests on the session binding +
// the return_url having been validated + stored at intent time.
func (s *Service) completeEmbedPayment(c *gin.Context) {
	id := c.Param("id")
	if uuid.Validate(id) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil || tr == nil || tr.Type != launch.TriggerTypeForm {
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

	states, serr := s.db.GetFieldStates(sid)
	if serr != nil {
		log.WithError(serr).Error("embed payment complete: unable to load field states")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	var st paymentFieldState
	if raw, present := states[field]; present && len(raw) > 0 {
		_ = json.Unmarshal(raw, &st)
	}
	// The return_url was validated + stored at intent; never trust the query.
	if !validReturnURL(st.ReturnURL, "") {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	paidURL := appendQuery(st.ReturnURL, "flo_paid", field, "submission_id", sid)
	retryURL := appendQuery(st.ReturnURL, "flo_payment", "failed")

	// Idempotent: already complete → back to the app.
	if st.Status == "complete" {
		c.Redirect(http.StatusSeeOther, paidURL)
		return
	}
	// Session must match the one we handed off for this field.
	if st.SessionID == "" || st.SessionID != sessionID {
		log.WithField("trigger_id", id).Warn("embed payment complete: session id does not match pending state")
		c.Redirect(http.StatusSeeOther, retryURL)
		return
	}

	secretKey := s.resolvePaymentSecret(id, comp)
	if !looksLikeStripeSecret(secretKey) {
		log.WithField("trigger_id", id).Error("embed payment complete: Stripe secret key did not resolve")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	status, err := stripewh.RetrieveCheckoutPaymentStatus(secretKey, sessionID)
	if err != nil {
		log.WithError(err).Error("embed payment complete: Stripe session retrieve failed")
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	if status != "paid" {
		c.Redirect(http.StatusSeeOther, retryURL)
		return
	}

	complete, _ := json.Marshal(paymentFieldState{
		Type:        "payment",
		Status:      "complete",
		SessionID:   sessionID,
		AmountMinor: st.AmountMinor,
		Currency:    st.Currency,
		ReturnURL:   st.ReturnURL,
		PaidAt:      time.Now().UTC().Format(time.RFC3339),
	})
	if _, merr := s.db.SetFieldState(sid, field, complete); merr != nil {
		log.WithError(merr).Error("embed payment complete: unable to record complete state")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Redirect(http.StatusSeeOther, paidURL)
}

// getEmbedFieldStates returns the draft's resumable state — the autosaved
// answers AND the per-field state map (payment paid/pending, …) — so the SDK can
// resume after a payment redirect: restore what the user had entered and render a
// paid field. Behind the embed gate. The draft is identified by the unguessable
// submission id and must belong to this form.
func (s *Service) getEmbedFieldStates(c *gin.Context) {
	id := c.Param("id")
	sid := c.Query("submission_id")
	if uuid.Validate(sid) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	draft, err := s.db.GetFormDraftAny(sid)
	if err != nil {
		log.WithError(err).Error("embed: unable to load draft for resume")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	answers := map[string]interface{}{}
	fieldStates := map[string]json.RawMessage{}
	if draft != nil && draft.TriggerID == id {
		if len(draft.Payload) > 0 {
			_ = json.Unmarshal(draft.Payload, &answers)
		}
		if len(draft.FieldStates) > 0 {
			_ = json.Unmarshal(draft.FieldStates, &fieldStates)
		}
	}
	// The fire-once marker is transport-only and must never resurface as an answer.
	delete(answers, "__submission_id")
	c.JSON(http.StatusOK, gin.H{"answers": answers, "field_states": fieldStates})
}
