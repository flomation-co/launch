package http

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	. "github.com/onsi/gomega"
)

// TestResolveFormAmountMinor_UsesComputeOutput asserts a payment field with a
// value_source derives its amount by running the compute flow with the draft
// answers and reading the chosen output key — the car-park case.
func TestResolveFormAmountMinor_UsesComputeOutput(t *testing.T) {
	RegisterTestingT(t)

	api := &fakeAPI{pollsBeforeDone: 0, result: map[string]interface{}{"price": "12.50"}}
	srv := api.server()
	defer srv.Close()

	s := &Service{formData: newTestResolver(srv.URL)}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)

	comp := formComponent{
		Name:        "pay",
		Type:        "payment",
		Currency:    "gbp",
		ValueSource: "flow-price",
		ValueOutput: "price",
	}
	amt, err := s.resolveFormAmountMinor(c, formDefinition{}, comp, map[string]interface{}{"plate": "AB12CDE"})
	Expect(err).ToNot(HaveOccurred())
	Expect(amt).To(Equal(int64(1250))) // £12.50 → 1250 pence
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(1)))
}

// TestResolveFormAmountMinor_ComputeDefaultsToFieldName asserts that with no
// value_output the field's own name is the output key read from the flow.
func TestResolveFormAmountMinor_ComputeDefaultsToFieldName(t *testing.T) {
	RegisterTestingT(t)

	api := &fakeAPI{pollsBeforeDone: 0, result: map[string]interface{}{"pay": "5"}}
	srv := api.server()
	defer srv.Close()

	s := &Service{formData: newTestResolver(srv.URL)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)

	comp := formComponent{Name: "pay", Type: "payment", Currency: "gbp", ValueSource: "flow-x"}
	amt, err := s.resolveFormAmountMinor(c, formDefinition{}, comp, map[string]interface{}{})
	Expect(err).ToNot(HaveOccurred())
	Expect(amt).To(Equal(int64(500)))
}

// TestResolveFormAmountMinor_ComputeInvalidOutputRejected asserts a non-numeric
// or missing compute output is rejected (amountToMinorUnits errors) rather than
// silently charging zero.
func TestResolveFormAmountMinor_ComputeInvalidOutputRejected(t *testing.T) {
	RegisterTestingT(t)

	api := &fakeAPI{pollsBeforeDone: 0, result: map[string]interface{}{"other": "12.50"}}
	srv := api.server()
	defer srv.Close()

	s := &Service{formData: newTestResolver(srv.URL)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)

	comp := formComponent{Name: "pay", Type: "payment", Currency: "gbp", ValueSource: "flow-x", ValueOutput: "price"}
	_, err := s.resolveFormAmountMinor(c, formDefinition{}, comp, map[string]interface{}{})
	Expect(err).To(HaveOccurred()) // "price" absent → empty amount → rejected
}

// TestResolveComputed_CacheKeyVariesByInputs asserts the compute cache key is
// answer-dependent: same flow + same inputs is served from cache; different
// inputs (or a different flow) run a fresh execution.
func TestResolveComputed_CacheKeyVariesByInputs(t *testing.T) {
	RegisterTestingT(t)

	api := &fakeAPI{pollsBeforeDone: 0, result: map[string]interface{}{"price": "10"}}
	srv := api.server()
	defer srv.Close()
	r := newTestResolver(srv.URL)

	r.ResolveComputed("flow-1", map[string]interface{}{"plate": "AB12"}, 0)
	r.ResolveComputed("flow-1", map[string]interface{}{"plate": "AB12"}, 0) // identical → cached
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(1)))

	r.ResolveComputed("flow-1", map[string]interface{}{"plate": "XY99"}, 0) // different inputs → new run
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(2)))

	r.ResolveComputed("flow-2", map[string]interface{}{"plate": "AB12"}, 0) // different flow → new run
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(3)))
}

func TestResolveComputed_NilAndEmptyShortCircuit(t *testing.T) {
	RegisterTestingT(t)

	var nilResolver *formDataResolver
	Expect(nilResolver.ResolveComputed("flow-1", nil, 0)).To(BeNil())

	api := &fakeAPI{}
	srv := api.server()
	defer srv.Close()
	Expect(newTestResolver(srv.URL).ResolveComputed("", nil, 0)).To(BeNil())
	Expect(atomic.LoadInt32(&api.executeCount)).To(Equal(int32(0)))
}

// TestComputeFieldComponent_RequiresValueSource is the /compute authorisation
// check: a caller may only run a flow the author bound to a field via
// value_source. A field with no value_source (or no such field) is rejected —
// the handler 400s on ok=false, so an arbitrary flow id can never be run.
func TestComputeFieldComponent_RequiresValueSource(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "plate", Type: "text"},
		{Name: "price", Type: "text", ValueSource: "flow-9"},
	}}}}

	// A plain field with no value_source is NOT computable → 400.
	_, ok := computeFieldComponent(def, "plate")
	Expect(ok).To(BeFalse())

	// A field the author marked value_source is computable.
	comp, ok := computeFieldComponent(def, "price")
	Expect(ok).To(BeTrue())
	Expect(comp.ValueSource).To(Equal("flow-9"))

	// An unknown field name is rejected (no existence leak).
	_, ok = computeFieldComponent(def, "nope")
	Expect(ok).To(BeFalse())
}

func TestComputeOutputKey_DefaultsToName(t *testing.T) {
	RegisterTestingT(t)

	Expect(computeOutputKey(formComponent{Name: "pay"})).To(Equal("pay"))
	Expect(computeOutputKey(formComponent{Name: "pay", ValueOutput: "price"})).To(Equal("price"))
	Expect(computeOutputKey(formComponent{Name: "pay", ValueOutput: "  "})).To(Equal("pay"))
}

// TestStripComputedSubmissions_RemovesClientValue asserts a computed field's
// client-supplied value never survives into the trigger data — it is stripped
// exactly like a display-only field.
func TestStripComputedSubmissions_RemovesClientValue(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "plate", Type: "text"},
		{Name: "price", Type: "payment", ValueSource: "flow-1"},
		{Name: "quote", Type: "text", ValueSource: "flow-2"},
	}}}}
	resolved := resolveFormForRender(def, substitutionContext{})

	body := map[string]interface{}{
		"plate": "AB12CDE",
		"price": "0.01",   // computed (payment) — must be stripped
		"quote": "forged", // computed (display) — must be stripped
	}
	out := stripComputedSubmissions(body, resolved)

	Expect(out).To(HaveKeyWithValue("plate", "AB12CDE"))
	_, hasPrice := out["price"]
	Expect(hasPrice).To(BeFalse())
	_, hasQuote := out["quote"]
	Expect(hasQuote).To(BeFalse())
}

// TestSanitiseFormSubmission_StripsComputed confirms the shared submit pipeline
// (used by submitForm and the payment finalise path) drops a computed field's
// client value.
func TestSanitiseFormSubmission_StripsComputed(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "plate", Type: "text"},
		{Name: "quote", Type: "text", ValueSource: "flow-2"},
	}}}}
	resolved := resolveFormForRender(def, substitutionContext{})
	final := sanitiseFormSubmission(map[string]interface{}{"plate": "AB12", "quote": "forged"}, resolved)

	Expect(final).To(HaveKeyWithValue("plate", "AB12"))
	_, hasQuote := final["quote"]
	Expect(hasQuote).To(BeFalse())
}

// TestComputeRateLimiter_BurstThenBlock asserts the per-key token bucket allows
// the burst then blocks, and that separate keys have independent buckets.
func TestComputeRateLimiter_BurstThenBlock(t *testing.T) {
	RegisterTestingT(t)

	// Negligible refill so the sequence is deterministic within the test.
	l := newComputeRateLimiter(0.0001, 3)

	Expect(l.allow("form-a")).To(BeTrue())  // bucket starts full (3)
	Expect(l.allow("form-a")).To(BeTrue())  // 2
	Expect(l.allow("form-a")).To(BeTrue())  // 1
	Expect(l.allow("form-a")).To(BeFalse()) // exhausted

	// A different form is unaffected.
	Expect(l.allow("form-b")).To(BeTrue())
}
