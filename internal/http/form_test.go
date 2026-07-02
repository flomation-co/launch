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

func TestSanitiseOptionSubmissions_RadioAndDropdown_WhitelistEnforced(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "role", Type: "radio", Options: []formOption{
					{Label: "Admin", Value: "admin"},
					{Label: "Member", Value: "member"},
				}},
				{Name: "size", Type: "dropdown", Options: []formOption{
					{Label: "Small", Value: "s"},
					{Label: "Large", Value: "l"},
				}},
			},
		}},
	}

	// Valid choices pass through
	sanitised := sanitiseOptionSubmissions(map[string]interface{}{
		"role": "member",
		"size": "l",
	}, resolved)
	Expect(sanitised["role"]).To(Equal("member"))
	Expect(sanitised["size"]).To(Equal("l"))

	// Off-whitelist values are wiped to empty string
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{
		"role": "superuser",
		"size": "xxl",
	}, resolved)
	Expect(sanitised["role"]).To(Equal(""))
	Expect(sanitised["size"]).To(Equal(""))

	// Non-string types are also wiped
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{
		"role": 42,
		"size": []interface{}{"s"},
	}, resolved)
	Expect(sanitised["role"]).To(Equal(""))
	Expect(sanitised["size"]).To(Equal(""))
}

func TestSanitiseOptionSubmissions_Checkboxes_FiltersToWhitelist(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "features", Type: "checkboxes", Options: []formOption{
					{Label: "Alpha", Value: "alpha"},
					{Label: "Beta", Value: "beta"},
					{Label: "Gamma", Value: "gamma"},
				}},
			},
		}},
	}

	// All whitelisted survive, off-whitelist filtered, order preserved
	sanitised := sanitiseOptionSubmissions(map[string]interface{}{
		"features": []interface{}{"alpha", "unknown", "gamma", 42, "beta"},
	}, resolved)
	Expect(sanitised["features"]).To(Equal([]interface{}{"alpha", "gamma", "beta"}))

	// Non-array becomes empty array
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{
		"features": "alpha",
	}, resolved)
	Expect(sanitised["features"]).To(Equal([]interface{}{}))

	// Missing key becomes empty array too (defensive default)
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{}, resolved)
	Expect(sanitised["features"]).To(Equal([]interface{}{}))
}

func TestSanitiseOptionSubmissions_Ranking_FiltersDedupesPreservesOrder(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "priorities", Type: "ranking", Options: []formOption{
					{Label: "Speed", Value: "speed"},
					{Label: "Quality", Value: "quality"},
					{Label: "Cost", Value: "cost"},
				}},
			},
		}},
	}

	// Client posts a valid partial ranking; keep as-is.
	sanitised := sanitiseOptionSubmissions(map[string]interface{}{
		"priorities": []interface{}{"quality", "cost"},
	}, resolved)
	Expect(sanitised["priorities"]).To(Equal([]interface{}{"quality", "cost"}))

	// Client posts extras + duplicates + non-whitelisted values.
	// Expected: only whitelisted, first-occurrence-wins for dupes, order preserved.
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{
		"priorities": []interface{}{"cost", "hacked", "speed", "cost", "quality", 42, "speed"},
	}, resolved)
	Expect(sanitised["priorities"]).To(Equal([]interface{}{"cost", "speed", "quality"}))

	// Non-array becomes empty array (defensive default).
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{
		"priorities": "not-a-list",
	}, resolved)
	Expect(sanitised["priorities"]).To(Equal([]interface{}{}))
}

func TestSanitiseOptionSubmissions_NonOptionFieldsUntouched(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "name", Type: "text"},
				{Name: "role", Type: "radio", Options: []formOption{{Label: "A", Value: "a"}}},
			},
		}},
	}

	sanitised := sanitiseOptionSubmissions(map[string]interface{}{
		"name": "Andy",
		"role": "a",
	}, resolved)
	Expect(sanitised["name"]).To(Equal("Andy"))
	Expect(sanitised["role"]).To(Equal("a"))
}

func TestSanitiseOptionSubmissions_NoOptionFields_ShortCircuits(t *testing.T) {
	RegisterTestingT(t)

	// Forms with zero option-based fields should be returned as-is.
	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{{Name: "name", Type: "text"}},
		}},
	}
	input := map[string]interface{}{"name": "Andy", "extra": "unused"}
	sanitised := sanitiseOptionSubmissions(input, resolved)
	Expect(sanitised).To(Equal(input))
}

func TestStripDisplayOnlySubmissions_RemovesDisplayFieldValues(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "name", Type: "text"},
				{Name: "intro_header", Type: "section_header"},
				{Name: "spacer", Type: "divider"},
				{Name: "help", Type: "info_text", DisplayText: "Please fill in accurately."},
				{Name: "age", Type: "number"},
			},
		}},
	}

	// A well-behaved client sends nothing for display-only, but a hostile
	// or naive one might try to inject values. Both cases must strip.
	sanitised := stripDisplayOnlySubmissions(map[string]interface{}{
		"name":         "Andy",
		"intro_header": "hacked",
		"spacer":       true,
		"help":         "injected",
		"age":          42,
	}, resolved)

	Expect(sanitised).To(HaveKey("name"))
	Expect(sanitised).To(HaveKey("age"))
	Expect(sanitised).NotTo(HaveKey("intro_header"))
	Expect(sanitised).NotTo(HaveKey("spacer"))
	Expect(sanitised).NotTo(HaveKey("help"))
}

func TestStripDisplayOnlySubmissions_NoDisplayFields_ShortCircuits(t *testing.T) {
	RegisterTestingT(t)

	// Forms with zero display-only components should pass through unchanged.
	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{{Name: "name", Type: "text"}},
		}},
	}
	input := map[string]interface{}{"name": "Andy"}
	Expect(stripDisplayOnlySubmissions(input, resolved)).To(Equal(input))
}

func TestIsDisplayOnly_CoversExpectedTypes(t *testing.T) {
	RegisterTestingT(t)

	Expect(isDisplayOnly("section_header")).To(BeTrue())
	Expect(isDisplayOnly("divider")).To(BeTrue())
	Expect(isDisplayOnly("info_text")).To(BeTrue())

	// Anything not in the set is not display-only, including the
	// options-based types added in Phase A.
	Expect(isDisplayOnly("text")).To(BeFalse())
	Expect(isDisplayOnly("radio")).To(BeFalse())
	Expect(isDisplayOnly("checkboxes")).To(BeFalse())
	Expect(isDisplayOnly("dropdown")).To(BeFalse())
	Expect(isDisplayOnly("")).To(BeFalse())
}

func TestStripReadOnlySubmissions_SkipsStructuredTypes(t *testing.T) {
	RegisterTestingT(t)

	// A read_only: true on location or address should not overwrite the
	// nested-object response with a string. This is defence-in-depth —
	// the FormBuilder hides the read-only toggle for these types, but a
	// hand-authored form JSON could still set it.
	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "coords", Type: "location", ReadOnly: true, DefaultValue: "IGNORED"},
				{Name: "postal", Type: "address", ReadOnly: true, DefaultValue: "IGNORED"},
				{Name: "email", Type: "email", ReadOnly: true, DefaultValue: "andy@example.com"},
			},
		}},
	}

	submission := map[string]interface{}{
		"coords": map[string]interface{}{"lat": 51.5, "lng": -0.1, "accuracy": 20.0},
		"postal": map[string]interface{}{"line1": "10 Downing St", "city": "London"},
		"email":  "attacker@evil.com",
	}

	stripped := stripReadOnlySubmissions(submission, resolved)

	// Nested objects survive the strip untouched.
	coords, ok := stripped["coords"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(coords["lat"]).To(Equal(51.5))
	// String read-only still restored for regular types.
	Expect(stripped["email"]).To(Equal("andy@example.com"))
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
