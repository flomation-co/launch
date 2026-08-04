package http

import (
	"encoding/json"
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

// Regression: resolveFormForRender must preserve page-level VisibleIf.
// It previously rebuilt each page as formPage{Components: comps}, dropping
// VisibleIf, so every page rendered as always-visible (page-skip
// navigation never fired) even though component-level visibility worked.
func TestResolveFormForRender_PreservesPageVisibleIf(t *testing.T) {
	RegisterTestingT(t)

	pageRule := &visibilityRule{
		Match: "all",
		Rules: []visibilityClause{{Field: "choice", Op: "equals", Value: "Yes"}},
	}
	compRule := &visibilityRule{
		Match: "all",
		Rules: []visibilityClause{{Field: "choice", Op: "equals", Value: "No"}},
	}

	def := formDefinition{
		Pages: []formPage{
			{Components: []formComponent{{Name: "choice", Label: "Yes or No?"}}},
			{
				VisibleIf: pageRule,
				Components: []formComponent{
					{Name: "detail", Label: "Detail", VisibleIf: compRule},
				},
			},
		},
	}

	resolved := resolveFormForRender(def, substitutionContext{})

	// The whole point: the page rule survives the render resolution.
	Expect(resolved.Pages[1].VisibleIf).ToNot(BeNil(),
		"page-level VisibleIf must be preserved through resolveFormForRender")
	Expect(resolved.Pages[1].VisibleIf.Rules).To(HaveLen(1))
	Expect(resolved.Pages[1].VisibleIf.Rules[0].Field).To(Equal("choice"))
	Expect(resolved.Pages[1].VisibleIf.Rules[0].Value).To(Equal("Yes"))

	// Component-level visibility (which always worked) still survives.
	Expect(resolved.Pages[1].Components[0].VisibleIf).ToNot(BeNil())
	Expect(resolved.Pages[1].Components[0].VisibleIf.Rules[0].Value).To(Equal("No"))

	// A page with no rule stays nil (always visible).
	Expect(resolved.Pages[0].VisibleIf).To(BeNil())
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

func TestSanitiseOptionSubmissions_DisabledOptionsRejected(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "role", Type: "radio", Options: []formOption{
					{Label: "Admin", Value: "admin", Disabled: true},
					{Label: "Member", Value: "member"},
				}},
				{Name: "perks", Type: "checkboxes", Options: []formOption{
					{Label: "Gym", Value: "gym"},
					{Label: "Car", Value: "car", Disabled: true},
				}},
			},
		}},
	}

	// A disabled option is not a valid choice — a crafted radio submission of
	// its value is wiped, exactly like an off-whitelist value.
	sanitised := sanitiseOptionSubmissions(map[string]interface{}{
		"role":  "admin",
		"perks": []interface{}{"gym", "car"},
	}, resolved)
	Expect(sanitised["role"]).To(Equal(""))
	// The disabled checkbox value is filtered out; the enabled one survives.
	Expect(sanitised["perks"]).To(Equal([]interface{}{"gym"}))

	// The enabled radio option still passes through.
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{"role": "member"}, resolved)
	Expect(sanitised["role"]).To(Equal("member"))
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

func TestSanitiseOptionSubmissions_PictureChoice_Single_WhitelistEnforced(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "avatar", Type: "picture_choice", Multiple: false, Options: []formOption{
					{Label: "Cat", Value: "cat", Image: "https://example.com/cat.png"},
					{Label: "Dog", Value: "dog", Image: "https://example.com/dog.png"},
				}},
			},
		}},
	}

	// A whitelisted value survives.
	sanitised := sanitiseOptionSubmissions(map[string]interface{}{"avatar": "dog"}, resolved)
	Expect(sanitised["avatar"]).To(Equal("dog"))

	// An off-whitelist value becomes "".
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{"avatar": "hacked"}, resolved)
	Expect(sanitised["avatar"]).To(Equal(""))

	// A non-string (e.g. an array smuggled into a single-select) becomes "".
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{"avatar": []interface{}{"cat"}}, resolved)
	Expect(sanitised["avatar"]).To(Equal(""))
}

func TestSanitiseOptionSubmissions_PictureChoice_Multiple_FiltersToWhitelist(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "toppings", Type: "picture_choice", Multiple: true, Options: []formOption{
					{Label: "Cheese", Value: "cheese", Image: "https://example.com/cheese.png"},
					{Label: "Ham", Value: "ham", Image: "https://example.com/ham.png"},
					{Label: "Olives", Value: "olives", Image: "https://example.com/olives.png"},
				}},
			},
		}},
	}

	// Whitelisted entries survive in order; off-whitelist / wrong-type dropped.
	sanitised := sanitiseOptionSubmissions(map[string]interface{}{
		"toppings": []interface{}{"cheese", "pineapple", "olives", 42, "ham"},
	}, resolved)
	Expect(sanitised["toppings"]).To(Equal([]interface{}{"cheese", "olives", "ham"}))

	// A non-array (e.g. a bare string) becomes an empty array.
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{"toppings": "cheese"}, resolved)
	Expect(sanitised["toppings"]).To(Equal([]interface{}{}))
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

func TestMetaText_StripsUnresolvedTokensAndCollapsesWhitespace(t *testing.T) {
	RegisterTestingT(t)

	// Plain text passes through unchanged.
	Expect(metaText("Contact Us")).To(Equal("Contact Us"))
	// Empty stays empty.
	Expect(metaText("")).To(Equal(""))
	// Unresolved substitution tokens (e.g. secrets/flow that we never
	// expose to the public form viewer, or an unauthenticated crawler's
	// empty ${user.X}) are stripped so previews never show raw syntax.
	Expect(metaText("Welcome ${user.first_name}")).To(Equal("Welcome"))
	Expect(metaText("Order ${flow.id} for ${secrets.token}")).To(Equal("Order for"))
	// Newlines / tabs / runs of spaces collapse to a single space so the
	// value sits cleanly on one line in a <title> or og:description.
	Expect(metaText("Line one\nLine two\t\ttabbed   spaced")).To(Equal("Line one Line two tabbed spaced"))
	// Lightweight formatting markers are stripped (keeping the wrapped
	// text) so the preview shows clean prose, not raw *asterisks* / _under_.
	Expect(metaText("*Free* to attend, _limited_ places")).To(Equal("Free to attend, limited places"))
	// A lone/unpaired marker is left as-is (it wraps nothing).
	Expect(metaText("5 * 3 = 15")).To(Equal("5 * 3 = 15"))
	// Markers combine with newline collapsing and token stripping.
	Expect(metaText("*Welcome* ${user.first_name}\n_see below_")).To(Equal("Welcome see below"))
}

func TestOptionsFromOutput_StringsObjectsAndJunk(t *testing.T) {
	RegisterTestingT(t)

	// Array of strings → label == value.
	Expect(optionsFromOutput([]interface{}{"UK", "US"})).To(Equal([]formOption{
		{Label: "UK", Value: "UK"}, {Label: "US", Value: "US"},
	}))

	// Array of {label,value} objects.
	Expect(optionsFromOutput([]interface{}{
		map[string]interface{}{"label": "United Kingdom", "value": "gb"},
		map[string]interface{}{"name": "United States", "value": "us"}, // name alias
	})).To(Equal([]formOption{
		{Label: "United Kingdom", Value: "gb"},
		{Label: "United States", Value: "us"},
	}))

	// Object with only a label falls back to using it as the value too.
	Expect(optionsFromOutput([]interface{}{
		map[string]interface{}{"label": "Solo"},
	})).To(Equal([]formOption{{Label: "Solo", Value: "Solo"}}))

	// Non-array yields no options; empty array yields an empty (non-nil) list.
	Expect(optionsFromOutput("not an array")).To(BeNil())
	Expect(optionsFromOutput([]interface{}{})).To(Equal([]formOption{}))
}

func TestFormUsesDataNamespace(t *testing.T) {
	RegisterTestingT(t)

	// ${data.X} in a default value → true.
	usesInField := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "a", DefaultValue: "Hi ${data.name}"},
	}}}}
	Expect(formUsesDataNamespace(usesInField)).To(BeTrue())

	// ${data.X} in the title → true.
	Expect(formUsesDataNamespace(formDefinition{Title: "Welcome ${data.company}"})).To(BeTrue())

	// A form that only uses the flow for dynamic OPTIONS (no ${data.X}
	// scalar anywhere) → false, so the GET isn't blocked on the flow.
	optionsOnly := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "country", Type: "dropdown", OptionsSource: "countries"},
	}}}}
	Expect(formUsesDataNamespace(optionsOnly)).To(BeFalse())
	Expect(formHasDynamicOptions(optionsOnly)).To(BeTrue())
}

func TestBakeDynamicOptions_PopulatesAndEnforcesWhitelist(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "country", Type: "dropdown", OptionsSource: "countries"},
	}}}}
	outputs := map[string]interface{}{
		"countries": []interface{}{
			map[string]interface{}{"label": "United Kingdom", "value": "gb"},
			map[string]interface{}{"label": "United States", "value": "us"},
		},
	}

	baked := bakeDynamicOptions(def, outputs)
	Expect(baked.Pages[0].Components[0].Options).To(Equal([]formOption{
		{Label: "United Kingdom", Value: "gb"},
		{Label: "United States", Value: "us"},
	}))
	// Original is untouched (pure function).
	Expect(def.Pages[0].Components[0].Options).To(BeEmpty())

	// The baked options now feed the existing whitelist enforcement: a
	// submitted value in the list survives, one outside it is cleared.
	Expect(sanitiseOptionSubmissions(map[string]interface{}{"country": "gb"}, baked)).
		To(HaveKeyWithValue("country", "gb"))
	Expect(sanitiseOptionSubmissions(map[string]interface{}{"country": "atlantis"}, baked)).
		To(HaveKeyWithValue("country", ""))
}

func TestEvalVisibility_NilAndEmptyRuleAlwaysVisible(t *testing.T) {
	RegisterTestingT(t)

	Expect(evalVisibility(nil, map[string]interface{}{})).To(BeTrue())
	Expect(evalVisibility(&visibilityRule{Match: "all"}, map[string]interface{}{})).To(BeTrue())
}

func TestEvalVisibility_Operators(t *testing.T) {
	RegisterTestingT(t)

	values := map[string]interface{}{
		"plan":     "pro",
		"seats":    "5",
		"empty":    "",
		"topics":   []interface{}{"news", "sport"},
		"agreed":   true,
		"quantity": float64(3),
	}
	rule := func(op, field, val string) *visibilityRule {
		return &visibilityRule{Match: "all", Rules: []visibilityClause{{Field: field, Op: op, Value: val}}}
	}

	Expect(evalVisibility(rule("equals", "plan", "pro"), values)).To(BeTrue())
	Expect(evalVisibility(rule("equals", "plan", "free"), values)).To(BeFalse())
	Expect(evalVisibility(rule("not_equals", "plan", "free"), values)).To(BeTrue())
	Expect(evalVisibility(rule("contains", "plan", "r"), values)).To(BeTrue())
	Expect(evalVisibility(rule("not_contains", "plan", "z"), values)).To(BeTrue())
	Expect(evalVisibility(rule("starts_with", "plan", "pr"), values)).To(BeTrue())
	Expect(evalVisibility(rule("ends_with", "plan", "ro"), values)).To(BeTrue())
	Expect(evalVisibility(rule("empty", "empty", ""), values)).To(BeTrue())
	Expect(evalVisibility(rule("not_empty", "plan", ""), values)).To(BeTrue())
	Expect(evalVisibility(rule("empty", "plan", ""), values)).To(BeFalse())

	// one_of over a scalar and over an array field (checkboxes).
	Expect(evalVisibility(rule("one_of", "plan", "free, pro , scale"), values)).To(BeTrue())
	Expect(evalVisibility(rule("one_of", "plan", "free,scale"), values)).To(BeFalse())
	Expect(evalVisibility(rule("one_of", "topics", "music,sport"), values)).To(BeTrue())

	// contains against an array checks membership, not substring.
	Expect(evalVisibility(rule("contains", "topics", "news"), values)).To(BeTrue())
	Expect(evalVisibility(rule("contains", "topics", "new"), values)).To(BeFalse())

	// Boolean answers stringify to "true"/"false".
	Expect(evalVisibility(rule("equals", "agreed", "true"), values)).To(BeTrue())

	// Numeric comparisons; non-numeric operands never satisfy.
	Expect(evalVisibility(rule("greater_than", "seats", "3"), values)).To(BeTrue())
	Expect(evalVisibility(rule("less_than", "quantity", "5"), values)).To(BeTrue())
	Expect(evalVisibility(rule("greater_than", "plan", "3"), values)).To(BeFalse())

	// Unknown operator resolves to visible (never silently hides).
	Expect(evalVisibility(rule("bogus", "plan", "x"), values)).To(BeTrue())
}

func TestEvalVisibility_MatchAnyVsAll(t *testing.T) {
	RegisterTestingT(t)

	values := map[string]interface{}{"a": "1", "b": "2"}
	rules := []visibilityClause{
		{Field: "a", Op: "equals", Value: "1"},
		{Field: "b", Op: "equals", Value: "99"},
	}
	// all → AND → false (b fails); any → OR → true (a passes).
	Expect(evalVisibility(&visibilityRule{Match: "all", Rules: rules}, values)).To(BeFalse())
	Expect(evalVisibility(&visibilityRule{Match: "any", Rules: rules}, values)).To(BeTrue())
}

func TestStripHiddenSubmissions_RemovesHiddenComponents(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "plan"},
		{Name: "seats", VisibleIf: &visibilityRule{Match: "all", Rules: []visibilityClause{
			{Field: "plan", Op: "equals", Value: "enterprise"},
		}}},
	}}}}

	// plan != enterprise → seats hidden → its value is stripped.
	out := stripHiddenSubmissions(map[string]interface{}{"plan": "free", "seats": "10"}, def)
	Expect(out).To(HaveKeyWithValue("plan", "free"))
	Expect(out).NotTo(HaveKey("seats"))

	// plan == enterprise → seats visible → kept.
	out = stripHiddenSubmissions(map[string]interface{}{"plan": "enterprise", "seats": "10"}, def)
	Expect(out).To(HaveKeyWithValue("seats", "10"))
}

func TestStripHiddenSubmissions_FixedPointChain(t *testing.T) {
	RegisterTestingT(t)

	// C is shown only if B == "x"; B is shown only if A == "yes".
	// Submitting A = "no" must cascade: B is hidden (cleared), which in turn
	// hides C — even though the client posted a value for C.
	def := formDefinition{Pages: []formPage{{Components: []formComponent{
		{Name: "a"},
		{Name: "b", VisibleIf: &visibilityRule{Match: "all", Rules: []visibilityClause{{Field: "a", Op: "equals", Value: "yes"}}}},
		{Name: "c", VisibleIf: &visibilityRule{Match: "all", Rules: []visibilityClause{{Field: "b", Op: "equals", Value: "x"}}}},
	}}}}

	out := stripHiddenSubmissions(map[string]interface{}{"a": "no", "b": "x", "c": "keep-me?"}, def)
	Expect(out).To(HaveKeyWithValue("a", "no"))
	Expect(out).NotTo(HaveKey("b"))
	Expect(out).NotTo(HaveKey("c"))
}

func TestStripHiddenSubmissions_HiddenPageStripsAllItsAnswers(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{
		{Components: []formComponent{{Name: "role"}}},
		{
			VisibleIf:  &visibilityRule{Match: "all", Rules: []visibilityClause{{Field: "role", Op: "equals", Value: "admin"}}},
			Components: []formComponent{{Name: "admin_note"}, {Name: "admin_level"}},
		},
	}}

	// role != admin → the whole second page is hidden → both its answers go.
	out := stripHiddenSubmissions(map[string]interface{}{"role": "user", "admin_note": "n", "admin_level": "3"}, def)
	Expect(out).To(HaveKeyWithValue("role", "user"))
	Expect(out).NotTo(HaveKey("admin_note"))
	Expect(out).NotTo(HaveKey("admin_level"))

	// role == admin → page visible → answers kept.
	out = stripHiddenSubmissions(map[string]interface{}{"role": "admin", "admin_note": "n", "admin_level": "3"}, def)
	Expect(out).To(HaveKeyWithValue("admin_note", "n"))
	Expect(out).To(HaveKeyWithValue("admin_level", "3"))
}

func TestStripHiddenSubmissions_NoRules_ShortCircuits(t *testing.T) {
	RegisterTestingT(t)

	def := formDefinition{Pages: []formPage{{Components: []formComponent{{Name: "a"}, {Name: "b"}}}}}
	in := map[string]interface{}{"a": "1", "b": "2"}
	out := stripHiddenSubmissions(in, def)
	Expect(out).To(Equal(in))
}

func TestSanitiseOptionSubmissions_OpinionScale_WhitelistEnforced(t *testing.T) {
	RegisterTestingT(t)

	// opinion_scale is a single-select whose string value must be one of the
	// component's options — exactly like radio. Off-whitelist or wrong-typed
	// values are wiped to "".
	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "agree", Type: "opinion_scale", Options: []formOption{
					{Label: "Disagree", Value: "disagree"},
					{Label: "Neutral", Value: "neutral"},
					{Label: "Agree", Value: "agree"},
				}},
			},
		}},
	}

	// Whitelisted value survives.
	sanitised := sanitiseOptionSubmissions(map[string]interface{}{"agree": "neutral"}, resolved)
	Expect(sanitised["agree"]).To(Equal("neutral"))

	// Off-whitelist value is wiped.
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{"agree": "smash"}, resolved)
	Expect(sanitised["agree"]).To(Equal(""))

	// Non-string type is wiped.
	sanitised = sanitiseOptionSubmissions(map[string]interface{}{"agree": 3}, resolved)
	Expect(sanitised["agree"]).To(Equal(""))
}

func TestStripReadOnlySubmissions_SkipsContactName(t *testing.T) {
	RegisterTestingT(t)

	// contact_name produces a nested-object response; a hand-authored
	// read_only: true must NOT overwrite it with the string DefaultValue.
	resolved := formDefinition{
		Pages: []formPage{{
			Components: []formComponent{
				{Name: "contact", Type: "contact_name", ReadOnly: true, DefaultValue: "IGNORED"},
			},
		}},
	}

	submission := map[string]interface{}{
		"contact": map[string]interface{}{"first_name": "Ada", "last_name": "Lovelace", "email": "ada@example.com"},
	}

	stripped := stripReadOnlySubmissions(submission, resolved)

	contact, ok := stripped["contact"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(contact["first_name"]).To(Equal("Ada"))
	Expect(contact["email"]).To(Equal("ada@example.com"))
}

func matrixField(name, cellType string) formComponent {
	return formComponent{
		Name:     name,
		Type:     "matrix",
		CellType: cellType,
		MatrixRows: []formOption{
			{Label: "Speed", Value: "speed"},
			{Label: "Support", Value: "support"},
		},
		MatrixColumns: []formOption{
			{Label: "Poor", Value: "poor"},
			{Label: "OK", Value: "ok"},
			{Label: "Great", Value: "great"},
		},
	}
}

func TestSanitiseMatrixSubmissions_Radio_WhitelistEnforced(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{Components: []formComponent{matrixField("survey", "radio")}}},
	}

	// A valid answer passes through; an off-whitelist column value drops that
	// row; an off-whitelist row key is dropped entirely.
	sanitised := sanitiseMatrixSubmissions(map[string]interface{}{
		"survey": map[string]interface{}{
			"speed":   "great",   // valid → kept
			"support": "amazing", // bad column value → row dropped
			"unknown": "ok",      // bad row key → dropped
		},
	}, resolved)

	got, ok := sanitised["survey"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(got["speed"]).To(Equal("great"))
	_, hasSupport := got["support"]
	Expect(hasSupport).To(BeFalse())
	_, hasUnknown := got["unknown"]
	Expect(hasUnknown).To(BeFalse())

	// A non-string row value (wrong type) is dropped.
	sanitised = sanitiseMatrixSubmissions(map[string]interface{}{
		"survey": map[string]interface{}{"speed": 42},
	}, resolved)
	got, ok = sanitised["survey"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	_, hasSpeed := got["speed"]
	Expect(hasSpeed).To(BeFalse())

	// A non-object answer becomes an empty map.
	sanitised = sanitiseMatrixSubmissions(map[string]interface{}{
		"survey": "not an object",
	}, resolved)
	Expect(sanitised["survey"]).To(Equal(map[string]interface{}{}))

	// Missing key defaults to empty map.
	sanitised = sanitiseMatrixSubmissions(map[string]interface{}{}, resolved)
	Expect(sanitised["survey"]).To(Equal(map[string]interface{}{}))
}

func TestSanitiseMatrixSubmissions_Checkbox_FiltersToWhitelist(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{Components: []formComponent{matrixField("survey", "checkbox")}}},
	}

	sanitised := sanitiseMatrixSubmissions(map[string]interface{}{
		"survey": map[string]interface{}{
			// Whitelisted survive, off-whitelist filtered, dupes dropped,
			// column-definition order NOT required but preserved as submitted.
			"speed":   []interface{}{"poor", "unknown", "great", "poor", 42},
			"support": []interface{}{},     // empty array allowed
			"unknown": []interface{}{"ok"}, // bad row key → dropped
		},
	}, resolved)

	got, ok := sanitised["survey"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(got["speed"]).To(Equal([]interface{}{"poor", "great"}))
	Expect(got["support"]).To(Equal([]interface{}{}))
	_, hasUnknown := got["unknown"]
	Expect(hasUnknown).To(BeFalse())

	// A row present but not an array becomes an empty selection (key kept).
	sanitised = sanitiseMatrixSubmissions(map[string]interface{}{
		"survey": map[string]interface{}{"speed": "great"},
	}, resolved)
	got, ok = sanitised["survey"].(map[string]interface{})
	Expect(ok).To(BeTrue())
	Expect(got["speed"]).To(Equal([]interface{}{}))

	// A non-object answer becomes an empty map.
	sanitised = sanitiseMatrixSubmissions(map[string]interface{}{
		"survey": []interface{}{"nope"},
	}, resolved)
	Expect(sanitised["survey"]).To(Equal(map[string]interface{}{}))
}

func TestSanitiseMatrixSubmissions_NoMatrixFields_PassThrough(t *testing.T) {
	RegisterTestingT(t)

	resolved := formDefinition{
		Pages: []formPage{{Components: []formComponent{{Name: "text", Type: "text"}}}},
	}
	in := map[string]interface{}{"text": "hello"}
	Expect(sanitiseMatrixSubmissions(in, resolved)).To(Equal(in))
}

// TestAllowCopy_SurvivesParseAndRemarshal guards the render-time round-trip:
// the client authors allow_copy in the editor, it is stored as JSON, and on
// render launch parses it into formComponent and RE-MARSHALS it for the browser
// (service.go). If AllowCopy were missing from the struct, encoding/json would
// silently drop the key here and the Copy button would never appear.
func TestAllowCopy_SurvivesParseAndRemarshal(t *testing.T) {
	RegisterTestingT(t)

	raw := []byte(`{"title":"T","pages":[{"components":[` +
		`{"name":"ref","type":"text","allow_copy":true},` +
		`{"name":"plain","type":"text"}]}]}`)

	def, err := parseFormDefinition(raw)
	Expect(err).To(BeNil())
	Expect(def.Pages[0].Components[0].AllowCopy).To(BeTrue())
	Expect(def.Pages[0].Components[1].AllowCopy).To(BeFalse())

	// Re-marshal exactly as the render path does, then re-read: the flag must
	// still be present for the field that opted in, and omitted for the other.
	out, err := json.Marshal(def)
	Expect(err).To(BeNil())

	var back formDefinition
	Expect(json.Unmarshal(out, &back)).To(BeNil())
	Expect(back.Pages[0].Components[0].AllowCopy).To(BeTrue())
	Expect(back.Pages[0].Components[1].AllowCopy).To(BeFalse())

	// omitempty: the false field must not emit the key at all.
	Expect(string(out)).ToNot(ContainSubstring(`"name":"plain","type":"text","allow_copy"`))
}
