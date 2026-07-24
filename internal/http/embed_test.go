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

// TestProjectDefinition_ProjectsTableConfig asserts the table field's config
// keys survive the default-deny allowlist so the SDK can render the grid.
func TestProjectDefinition_ProjectsTableConfig(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{
		Title: "Choose a claim",
		Pages: []formPage{{Components: []formComponent{{
			Name:          "claim",
			Label:         "Claims",
			Type:          "table",
			SelectionMode: "single",
			ValueColumn:   "ref",
			PageSize:      10,
			Filterable:    true,
			ValueSource:   "rows-flow-id", // computed rows — flow id must NOT leak
			ValueOutput:   "claims",
			TableColumns:  []tableColumn{{Key: "ref", Label: "Reference", Clickable: true}},
			TableRows:     []map[string]interface{}{{"ref": "REF-1"}},
		}}}},
	}

	proj := projectDefinition(def)
	raw, _ := json.Marshal(proj)
	blob := string(raw)
	// The value_source flow id / output key are internal and must be absent.
	Expect(blob).ToNot(ContainSubstring("rows-flow-id"))
	Expect(blob).ToNot(ContainSubstring("value_source"))
	var payload struct {
		Pages []struct {
			Components []map[string]interface{} `json:"components"`
		} `json:"pages"`
	}
	Expect(json.Unmarshal(raw, &payload)).To(Succeed())
	comp := payload.Pages[0].Components[0]
	Expect(comp["type"]).To(Equal("table"))
	Expect(comp["selection_mode"]).To(Equal("single"))
	Expect(comp["value_column"]).To(Equal("ref"))
	Expect(comp["filterable"]).To(Equal(true))
	Expect(comp["table_columns"]).ToNot(BeNil())
	Expect(comp["table_rows"]).ToNot(BeNil())
	// A value_source table is flagged computed so the SDK fetches rows via /compute.
	Expect(comp["computed"]).To(Equal(true))
}

// TestProjectDefinition_SubstitutesUserVars is the regression for the embedded
// login-gated form: once the SDK forwards the session token, the definition
// endpoint must resolve ${user.X} before projecting — otherwise the field's
// default_value (e.g. a Username pre-filled with ${user.email}) reaches the SDK
// verbatim. handleEmbedFormDefinition composes resolveFormForRender then
// projectDefinition; this asserts that composition on the pure functions.
func TestProjectDefinition_SubstitutesUserVars(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{
		Title: "Self Start",
		Pages: []formPage{{Components: []formComponent{
			{Name: "username", Label: "Username", Type: "text", DefaultValue: "${user.email}"},
		}}},
	}

	// Authenticated: ${user.email} resolves in the projected default_value.
	ctx := substitutionContext{UserVariables: map[string]string{"email": "jane@dwp.gov.uk"}}
	proj := projectDefinition(resolveFormForRender(def, ctx))
	raw, _ := json.Marshal(proj)
	var payload struct {
		Pages []struct {
			Components []map[string]interface{} `json:"components"`
		} `json:"pages"`
	}
	Expect(json.Unmarshal(raw, &payload)).To(Succeed())
	Expect(payload.Pages[0].Components[0]["default_value"]).To(Equal("jane@dwp.gov.uk"))
	Expect(string(raw)).ToNot(ContainSubstring("${user.email}"))

	// Anonymous (no user vars): an unresolved ${user.X} collapses to empty (the
	// existing substitution contract) rather than leaking the raw token to the
	// client. The SDK still can't submit a require_login form until it presents a
	// token, at which point the field pre-fills.
	projAnon := projectDefinition(resolveFormForRender(def, substitutionContext{}))
	rawAnon, _ := json.Marshal(projAnon)
	Expect(string(rawAnon)).ToNot(ContainSubstring("${user."))
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
