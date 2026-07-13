package http

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"
	. "github.com/onsi/gomega"
)

// TestProjectDefinition_StripsSecrets is the security-critical read-path test:
// the public projection must never expose a secret or an internal flow id, and
// must instead surface the safe derived flags the SDK needs.
func TestProjectDefinition_StripsSecrets(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{
		Title:       "Pay for parking",
		Description: "desc",
		DataSource:  &formDataSource{FlowID: "flow-secret-id"},
		Pages: []formPage{{Components: []formComponent{
			{Name: "reg", Label: "Reg", Type: "text", Required: true},
			{
				Name: "pay", Label: "Charge", Type: "payment", Currency: "gbp",
				PaymentSecret: "${secrets.STRIPE_SECRET}",
				ValueSource:   "pricing-flow-id",
				ValueOutput:   "parking_charge",
			},
		}}},
	}

	proj := projectDefinition(def)
	// Round-trip through JSON so we assert on exactly what the client receives.
	raw, _ := json.Marshal(proj)
	blob := string(raw)

	// Secrets / internal ids must be ABSENT anywhere in the payload.
	for _, leak := range []string{
		"payment_secret", "STRIPE_SECRET",
		"value_source", "pricing-flow-id",
		"value_output", "parking_charge",
		"flow_id", "flow-secret-id",
	} {
		Expect(blob).ToNot(ContainSubstring(leak), "public projection leaked %q", leak)
	}

	// Safe, needed data must be PRESENT.
	Expect(proj["schema_version"]).To(Equal(embedSchemaVersion))
	Expect(proj["title"]).To(Equal("Pay for parking"))
	Expect(proj["has_data_source"]).To(Equal(true))

	// The payment/computed field is still present (label/type/currency) and
	// flagged computed so the SDK knows to fetch its value via /compute.
	var payload struct {
		Pages []struct {
			Components []map[string]interface{} `json:"components"`
		} `json:"pages"`
	}
	Expect(json.Unmarshal(raw, &payload)).To(Succeed())
	pay := payload.Pages[0].Components[1]
	Expect(pay["type"]).To(Equal("payment"))
	Expect(pay["currency"]).To(Equal("gbp"))
	Expect(pay["computed"]).To(Equal(true))
}

// TestEmbedRoutesRegister_NoConflict asserts the embed route shapes register on a
// fresh gin engine without a radix-tree conflict (gin panics on conflict at
// registration time, which a plain build won't catch).
func TestEmbedRoutesRegister_NoConflict(t *testing.T) {
	RegisterTestingT(t)

	gin.SetMode(gin.TestMode)
	Expect(func() {
		r := gin.New()
		noop := func(c *gin.Context) {}
		r.Use(func(c *gin.Context) { c.Next() })
		grp := r.Group("/v1/embed/form/:id", func(c *gin.Context) { c.Next() })
		grp.GET("/definition", noop)
		grp.GET("/data", noop)
		grp.POST("", noop)
		grp.POST("/compute", noop)
		grp.PUT("/submission/:sid", noop)
		grp.POST("/upload", noop)
		grp.POST("/session", noop)
		grp.POST("/payment-intent", noop)
		grp.GET("/field-states", noop)
		// Public completion route registered OUTSIDE the group (no gate).
		r.GET("/v1/embed/form/:id/complete", noop)
		r.OPTIONS("/v1/embed/form/:id/definition", noop)
		r.OPTIONS("/v1/embed/form/:id/data", noop)
		r.OPTIONS("/v1/embed/form/:id", noop)
		r.OPTIONS("/v1/embed/form/:id/compute", noop)
		r.OPTIONS("/v1/embed/form/:id/submission/:sid", noop)
		r.OPTIONS("/v1/embed/form/:id/upload", noop)
		r.OPTIONS("/v1/embed/form/:id/session", noop)
		r.OPTIONS("/v1/embed/form/:id/payment-intent", noop)
		r.OPTIONS("/v1/embed/form/:id/field-states", noop)
	}).ToNot(Panic())
}
