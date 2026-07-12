package http

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
)

func TestAmountToMinorUnits_TwoDecimalDefault(t *testing.T) {
	RegisterTestingT(t)

	// GBP / USD / EUR are 2-decimal: major → ×100.
	got, err := amountToMinorUnits("49.99", "gbp")
	Expect(err).ToNot(HaveOccurred())
	Expect(got).To(Equal(int64(4999)))

	got, err = amountToMinorUnits("10", "usd")
	Expect(err).ToNot(HaveOccurred())
	Expect(got).To(Equal(int64(1000)))

	// Rounding absorbs binary-float artefacts.
	got, err = amountToMinorUnits("0.10", "eur")
	Expect(err).ToNot(HaveOccurred())
	Expect(got).To(Equal(int64(10)))
}

func TestAmountToMinorUnits_ZeroDecimal(t *testing.T) {
	RegisterTestingT(t)

	// JPY has no minor unit — the major unit IS the smallest unit.
	got, err := amountToMinorUnits("1000", "jpy")
	Expect(err).ToNot(HaveOccurred())
	Expect(got).To(Equal(int64(1000)))

	// Case-insensitive currency lookup.
	got, err = amountToMinorUnits("500", "KRW")
	Expect(err).ToNot(HaveOccurred())
	Expect(got).To(Equal(int64(500)))
}

func TestAmountToMinorUnits_ThreeDecimal(t *testing.T) {
	RegisterTestingT(t)

	// KWD (Kuwaiti dinar) uses thousandths as its smallest unit.
	got, err := amountToMinorUnits("1.234", "kwd")
	Expect(err).ToNot(HaveOccurred())
	Expect(got).To(Equal(int64(1234)))

	got, err = amountToMinorUnits("5", "bhd")
	Expect(err).ToNot(HaveOccurred())
	Expect(got).To(Equal(int64(5000)))
}

func TestAmountToMinorUnits_RejectsInvalid(t *testing.T) {
	RegisterTestingT(t)

	for _, bad := range []string{"", "  ", "abc", "-5", "-0.01", "1.2.3", "12,34", "1e3", "£5", "+5"} {
		_, err := amountToMinorUnits(bad, "gbp")
		Expect(err).To(HaveOccurred(), "expected %q to be rejected", bad)
	}
}

func TestLooksLikeStripeSecret(t *testing.T) {
	RegisterTestingT(t)

	Expect(looksLikeStripeSecret("sk_live_abc123")).To(BeTrue())
	Expect(looksLikeStripeSecret("sk_test_abc123")).To(BeTrue())
	Expect(looksLikeStripeSecret("rk_live_abc123")).To(BeTrue())
	// An unresolved placeholder or empty value must not pass.
	Expect(looksLikeStripeSecret("${secrets.stripe_secret_key}")).To(BeFalse())
	Expect(looksLikeStripeSecret("")).To(BeFalse())
	Expect(looksLikeStripeSecret("pk_live_publishable")).To(BeFalse())
}

func TestPaymentComponent(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "email", Type: "email"},
		{Name: "pay", Type: "payment", Amount: "49.99", Currency: "gbp"},
	}}}}
	comp, ok := paymentComponent(def)
	Expect(ok).To(BeTrue())
	Expect(comp.Name).To(Equal("pay"))
	Expect(comp.Amount).To(Equal("49.99"))

	_, ok = paymentComponent(formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "email", Type: "email"},
	}}}})
	Expect(ok).To(BeFalse())
}

// TestIsFieldStateSatisfied covers the per-type submit-gate predicate: a
// payment field is satisfied ONLY once its recorded state is "complete" (set
// exclusively server-side after the Stripe paid + session-binding checks). A
// pending, empty, malformed, or unknown-type state is never satisfied.
func TestIsFieldStateSatisfied(t *testing.T) {
	RegisterTestingT(t)

	complete, _ := json.Marshal(paymentFieldState{Type: "payment", Status: "complete"})
	pending, _ := json.Marshal(paymentFieldState{Type: "payment", Status: "pending"})

	Expect(isFieldStateSatisfied("payment", complete)).To(BeTrue())
	Expect(isFieldStateSatisfied("payment", pending)).To(BeFalse())
	// Absent / empty / malformed state → not satisfied (must not let submit
	// through when there is nothing recorded).
	Expect(isFieldStateSatisfied("payment", nil)).To(BeFalse())
	Expect(isFieldStateSatisfied("payment", json.RawMessage(``))).To(BeFalse())
	Expect(isFieldStateSatisfied("payment", json.RawMessage(`{`))).To(BeFalse())
	// A stateful type we don't understand blocks submit (fails closed).
	Expect(isFieldStateSatisfied("esignature", complete)).To(BeFalse())
}

// TestResolvePaymentComponent covers field-scoped lookup (multi-payment forms),
// the empty-name fallback to the first payment field, and the miss cases.
func TestResolvePaymentComponent(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "deposit", Type: "payment", Amount: "10.00", Currency: "gbp"},
		{Name: "email", Type: "email"},
		{Name: "balance", Type: "payment", Amount: "90.00", Currency: "gbp"},
	}}}}

	// Named lookup picks the exact field, not just the first.
	comp, ok := resolvePaymentComponent(def, "balance")
	Expect(ok).To(BeTrue())
	Expect(comp.Name).To(Equal("balance"))
	Expect(comp.Amount).To(Equal("90.00"))

	// Empty name falls back to the first payment field (single-payment forms).
	comp, ok = resolvePaymentComponent(def, "")
	Expect(ok).To(BeTrue())
	Expect(comp.Name).To(Equal("deposit"))

	// A name that isn't a payment field (or doesn't exist) misses.
	_, ok = resolvePaymentComponent(def, "email")
	Expect(ok).To(BeFalse())
	_, ok = resolvePaymentComponent(def, "nope")
	Expect(ok).To(BeFalse())
}

// TestUnsatisfiedRequiredStatefulFields is the server submit gate: it names the
// required stateful fields that are visible but not yet satisfied, and nothing
// else. Optional fields, satisfied fields, and hidden fields never appear.
func TestUnsatisfiedRequiredStatefulFields(t *testing.T) {
	RegisterTestingT(t)

	complete, _ := json.Marshal(paymentFieldState{Type: "payment", Status: "complete"})

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "deposit", Type: "payment", Required: true},           // required, unsatisfied
		{Name: "tip", Type: "payment", Required: false},              // optional — never gates
		{Name: "balance", Type: "payment", Required: true},           // required, satisfied below
		{Name: "extra", Type: "payment", Required: true, VisibleIf: &visibilityRule{
			Match: "all",
			Rules: []visibilityClause{{Field: "wants_extra", Op: "equals", Value: "yes"}},
		}}, // required but hidden unless wants_extra == yes
	}}}}

	states := map[string]json.RawMessage{"balance": complete}
	answers := map[string]interface{}{} // wants_extra not "yes" → extra is hidden

	missing := unsatisfiedRequiredStatefulFields(def, answers, states)

	Expect(missing).To(ConsistOf("deposit"))           // only the visible, required, unsatisfied one
	Expect(missing).ToNot(ContainElement("tip"))       // optional
	Expect(missing).ToNot(ContainElement("balance"))   // satisfied
	Expect(missing).ToNot(ContainElement("extra"))     // hidden

	// Reveal the hidden field: now it must gate too.
	missing = unsatisfiedRequiredStatefulFields(def, map[string]interface{}{"wants_extra": "yes"}, states)
	Expect(missing).To(ConsistOf("deposit", "extra"))

	// Everything satisfied → empty (submit may proceed).
	depositDone, _ := json.Marshal(paymentFieldState{Type: "payment", Status: "complete"})
	all := map[string]json.RawMessage{"deposit": depositDone, "balance": complete}
	Expect(unsatisfiedRequiredStatefulFields(def, map[string]interface{}{}, all)).To(BeEmpty())
}

// TestSanitiseFormSubmission_StripsHiddenAndReadOnly asserts the shared
// finalise pipeline (used by the payment /complete path) strips a hidden field
// and restores a read-only field's baked value — identical to submitForm,
// which now calls the same helper.
func TestSanitiseFormSubmission_StripsHiddenAndReadOnly(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "name", Type: "text"},
		{Name: "plan", Type: "text", ReadOnly: true, DefaultValue: "pro"},
		{Name: "extra", Type: "text", VisibleIf: &visibilityRule{
			Match: "all",
			Rules: []visibilityClause{{Field: "name", Op: "equals", Value: "reveal"}},
		}},
	}}}}
	resolved := resolveFormForRender(def, substitutionContext{})

	body := map[string]interface{}{
		"name":  "Alice",       // ordinary field — kept
		"plan":  "hacked-free", // read-only — must be restored to the baked "pro"
		"extra": "smuggled",    // hidden (name != "reveal") — must be stripped
	}
	final := sanitiseFormSubmission(body, resolved)

	Expect(final["name"]).To(Equal("Alice"))
	Expect(final["plan"]).To(Equal("pro"))
	_, hasExtra := final["extra"]
	Expect(hasExtra).To(BeFalse())
}
