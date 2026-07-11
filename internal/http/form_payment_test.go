package http

import (
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
