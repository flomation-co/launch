package http

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestApplySubstitutions_UserNamespace(t *testing.T) {
	RegisterTestingT(t)

	ctx := substitutionContext{
		UserVariables: map[string]string{
			"first_name":   "Andy",
			"full_address": "10 Downing St\nLondon\nSW1A 2AA",
		},
	}

	Expect(applySubstitutions("Hello ${user.first_name}", ctx)).To(Equal("Hello Andy"))
	Expect(applySubstitutions("${user.full_address}", ctx)).To(Equal("10 Downing St\nLondon\nSW1A 2AA"))
	// Missing field resolves to empty
	Expect(applySubstitutions("X ${user.missing} Y", ctx)).To(Equal("X  Y"))
	// Empty input stays empty
	Expect(applySubstitutions("", ctx)).To(Equal(""))
}

func TestApplySubstitutions_QueryNamespace(t *testing.T) {
	RegisterTestingT(t)

	ctx := substitutionContext{
		QueryParams: map[string]string{"ref": "alice", "campaign": "spring2026"},
	}

	Expect(applySubstitutions("Referred by ${query.ref}", ctx)).To(Equal("Referred by alice"))
	Expect(applySubstitutions("${query.campaign}/${query.ref}", ctx)).To(Equal("spring2026/alice"))
	// Missing key empty
	Expect(applySubstitutions("${query.utm_source}", ctx)).To(Equal(""))
}

func TestApplySubstitutions_UnknownNamespaceLeftAlone(t *testing.T) {
	RegisterTestingT(t)

	ctx := substitutionContext{
		UserVariables: map[string]string{"first_name": "Andy"},
	}

	// ${secrets.X}, ${env.X}, ${flow.X} aren't render-time concepts —
	// leave the literal in place so a downstream consumer (or post-
	// submission flow execution) can resolve them. Anything we don't
	// recognise stays untouched.
	Expect(applySubstitutions("${secrets.X}", ctx)).To(Equal("${secrets.X}"))
	Expect(applySubstitutions("${env.X}", ctx)).To(Equal("${env.X}"))
	Expect(applySubstitutions("${flow.id}", ctx)).To(Equal("${flow.id}"))
}

func TestApplySubstitutions_NilContext(t *testing.T) {
	RegisterTestingT(t)

	// Nil maps must not panic
	Expect(applySubstitutions("${user.first_name}", substitutionContext{})).To(Equal(""))
	Expect(applySubstitutions("${query.ref}", substitutionContext{})).To(Equal(""))
}

func TestResolveFormForRender_SubstitutesLabelPlaceholderDefault(t *testing.T) {
	RegisterTestingT(t)

	ctx := substitutionContext{
		UserVariables: map[string]string{
			"first_name": "Andy",
			"city":       "London",
		},
	}

	def := formDefinition{
		Title:       "Welcome ${user.first_name}",
		Description: "Confirm your details",
		Pages: []formPage{{
			Components: []formComponent{
				{
					Name:         "city",
					Label:        "City (${user.first_name}'s home)",
					Placeholder:  "Your city",
					DefaultValue: "${user.city}",
				},
			},
		}},
	}

	resolved := resolveFormForRender(def, ctx)

	Expect(resolved.Title).To(Equal("Welcome Andy"))
	Expect(resolved.Pages[0].Components[0].Label).To(Equal("City (Andy's home)"))
	Expect(resolved.Pages[0].Components[0].DefaultValue).To(Equal("London"))
	// Input definition should be untouched
	Expect(def.Title).To(Equal("Welcome ${user.first_name}"))
}

func TestStripReadOnlySubmissions_OverridesClientValues(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "name", ReadOnly: false},
				{Name: "email", ReadOnly: true, DefaultValue: "andy@example.com"},
				{Name: "agreed", ReadOnly: true, DefaultValue: "true"},
			},
		}},
	}

	// Client submits its own version of email (tampered) AND extra fields.
	submission := map[string]interface{}{
		"name":   "Andy",
		"email":  "attacker@evil.com",
		"agreed": "false",
	}

	stripped := stripReadOnlySubmissions(submission, resolved)

	Expect(stripped["name"]).To(Equal("Andy"))
	Expect(stripped["email"]).To(Equal("andy@example.com"))
	Expect(stripped["agreed"]).To(Equal("true"))
}

func TestExtractSessionToken_CookiePreferredOverHeader(t *testing.T) {
	RegisterTestingT(t)

	Expect(extractSessionToken("Bearer abc", "")).To(Equal("abc"))
	Expect(extractSessionToken("", "cookie-token")).To(Equal("cookie-token"))
	// Cookie wins when both present
	Expect(extractSessionToken("Bearer abc", "cookie-token")).To(Equal("cookie-token"))
	Expect(extractSessionToken("", "")).To(Equal(""))
	Expect(extractSessionToken("malformed", "")).To(Equal(""))
}
