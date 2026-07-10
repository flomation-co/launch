package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	sentinel "github.com/flomation-co/sentinel-client"
	log "github.com/sirupsen/logrus"
)

// formDefinition mirrors the editor's FormDefinition shape. Pure superset of
// the older shape (no migrations) — older saved forms have nil RequireLogin
// and missing per-field flags which decode cleanly to zero values.
type formDefinition struct {
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Pages        []formPage `json:"pages"`
	RequireLogin bool       `json:"require_login,omitempty"`

	// DataSource, when set, names a flow that is run when the form loads;
	// its outputs become ${data.X} substitution values usable in labels,
	// placeholders and default values. Nil (the default for every existing
	// form) means no autofill. See formDataResolver for the caching model.
	DataSource *formDataSource `json:"data_source,omitempty"`

	// Submit configures what happens after a successful submission. Nil keeps
	// the default "Thank you!" card.
	Submit *formSubmit `json:"submit,omitempty"`
}

// formSubmit configures the post-submission experience.
type formSubmit struct {
	// SuccessMessage overrides the default "Your response has been submitted
	// successfully." text. Shown for the "message" and "restart" modes.
	SuccessMessage string `json:"success_message,omitempty"`
	// OnSubmit selects the behaviour: "message" (default — a thank-you card),
	// "restart" (reset the form for another response — kiosk loop), or
	// "redirect" (send the browser to RedirectURL).
	OnSubmit string `json:"on_submit,omitempty"`
	// RedirectURL is the destination for the "redirect" mode. The client only
	// follows http(s) URLs.
	RedirectURL string `json:"redirect_url,omitempty"`
	// RedirectDelaySeconds optionally holds the thank-you view for a moment
	// before redirecting. Zero redirects immediately.
	RedirectDelaySeconds int `json:"redirect_delay_seconds,omitempty"`
}

// formDataSource configures form-field autofill from a flow's outputs.
type formDataSource struct {
	// FlowID is the flow run to produce ${data.X} values. It executes via
	// its manual trigger with no input, so the result is identical for every
	// viewer — which is exactly why a burst of concurrent loads can share a
	// single cached execution.
	FlowID string `json:"flow_id"`
	// TimeoutSeconds bounds how long a form load waits for the flow on a
	// cache miss. Zero uses the resolver default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

type formPage struct {
	Components []formComponent `json:"components"`

	// VisibleIf conditionally shows/hides an entire page based on earlier
	// answers. Nil means always visible. When a page is hidden, navigation
	// skips it and the server strips every answer belonging to it.
	VisibleIf *visibilityRule `json:"visible_if,omitempty"`
}

type formComponent struct {
	Name         string       `json:"name"`
	Label        string       `json:"label"`
	Type         string       `json:"type"`
	Placeholder  string       `json:"placeholder"`
	Required     bool         `json:"required"`
	Order        int          `json:"order"`
	ReadOnly     bool         `json:"read_only,omitempty"`
	DefaultValue string       `json:"default_value,omitempty"`
	Options      []formOption `json:"options,omitempty"`

	// OptionsSource, when set on an option-based field (radio, dropdown,
	// checkboxes, ranking), names a key in the data-source flow's outputs
	// whose value is the option list. Options are fetched by the browser
	// after first paint (so a slow flow doesn't block the form) and, on
	// submit, baked into Options so the existing whitelist enforcement
	// applies. Empty means the static Options above are used.
	OptionsSource string `json:"options_source,omitempty"`

	// Numeric constraints — apply to number, slider, and rating types.
	// Pointer types distinguish "unset" from "zero", which matters:
	// Min=0 is a legitimate constraint (e.g. non-negative quantity)
	// that must survive round-tripping through JSON.
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Step        *float64 `json:"step,omitempty"`
	IntegerOnly bool     `json:"integer_only,omitempty"`
	Scale       int      `json:"scale,omitempty"`

	// Date/time bounds — ISO strings passed straight to the corresponding
	// HTML5 input's min/max attributes.
	MinDate string `json:"min_date,omitempty"`
	MaxDate string `json:"max_date,omitempty"`

	// Free paragraph text used by the info_text display-only component.
	// Renders as a paragraph beneath the form label rather than an input.
	DisplayText string `json:"display_text,omitempty"`

	// Precision hint for location components. Accepted values: "coarse"
	// (rounded to ~110m via 3 decimal places, client-side) or "fine"
	// (raw lat/lng from navigator.geolocation). Empty means fine.
	Precision string `json:"precision,omitempty"`

	// Upload-field constraints — apply to esignature, camera, and
	// file_upload types. AcceptMime uses HTML5 accept-attribute syntax
	// (comma-separated exact MIMEs or category/* wildcards).
	// MaxSizeBytes 0 means "use the global 25 MB cap".
	AcceptMime   string `json:"accept_mime,omitempty"`
	MaxSizeBytes int64  `json:"max_size_bytes,omitempty"`
	// AllowGallery lets a camera field also accept an already-taken
	// photo from the device's gallery instead of forcing capture.
	AllowGallery bool `json:"allow_gallery,omitempty"`

	// Recognition-field settings — apply to license_plate (and future
	// in-browser recognition fields). CaptureMode "manual" (default) shows a
	// tap-to-capture button; "auto" runs hands-off continuous detection.
	// AutoSubmit (auto only) submits the form on a confident recognition.
	// ConfidenceThreshold gates auto acceptance. PrivacyNotice is an
	// informational banner (not a gating consent checkbox); ShowPrivacyNotice
	// defaults to true (nil = show).
	CaptureMode         string   `json:"capture_mode,omitempty"`
	AutoSubmit          bool     `json:"auto_submit,omitempty"`
	ConfidenceThreshold *float64 `json:"confidence_threshold,omitempty"`
	PrivacyNotice       string   `json:"privacy_notice,omitempty"`
	ShowPrivacyNotice   *bool    `json:"show_privacy_notice,omitempty"`

	// VisibleIf, when set, makes this component conditionally visible based
	// on the answers to earlier fields. Nil means "always visible" (the
	// default for every existing form). The client evaluates the same rule
	// live to show/hide; the server re-evaluates on submit to strip values
	// for fields that should have been hidden — a hidden branch must never
	// smuggle answers into the trigger data.
	VisibleIf *visibilityRule `json:"visible_if,omitempty"`
}

// visibilityRule is a group of conditions combined with AND ("all") or OR
// ("any"). An empty / nil rule means the component is always visible.
type visibilityRule struct {
	Match string             `json:"match"` // "all" (AND) or "any" (OR)
	Rules []visibilityClause `json:"rules"`
}

// visibilityClause is a single comparison of one field's answer against a
// target value. Operators mirror the Switch action's vocabulary so authors
// meet one consistent set of comparisons across the product.
type visibilityClause struct {
	Field string `json:"field"` // name of the field whose answer is tested
	Op    string `json:"op"`    // equals, not_equals, contains, ...
	Value string `json:"value,omitempty"`
}

// Display-only component types produce no response and take no user input;
// they exist purely to structure the form (headings, dividers, help text).
// Kept as a set so future additions (e.g. images, embeds) drop straight in.
var displayOnlyTypes = map[string]struct{}{
	"section_header": {},
	"divider":        {},
	"info_text":      {},
}

func isDisplayOnly(t string) bool {
	_, ok := displayOnlyTypes[t]
	return ok
}

// formOption is a single choice in radio / checkboxes / dropdown fields.
// Value is what the trigger receives; Label is what the form viewer sees.
// Two-column shape mirrors the SelectProperty pattern used by action inputs.
type formOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// substitutionContext bundles the inputs to ${X} substitution at render time.
// Today's surface: ${user.X} from the logged-in user's profile, ${query.X}
// from URL query parameters. Secrets and env vars are deliberately NOT
// exposed in form output — they'd leak environment-scoped data to the
// public form viewer.
type substitutionContext struct {
	UserVariables map[string]string
	QueryParams   map[string]string
	// DataVariables holds ${data.X} values — the outputs of the form's
	// data-source flow, resolved (and cached) at render time. Nil when the
	// form has no data source.
	DataVariables map[string]string
}

var substitutionPattern = regexp.MustCompile(`\$\{([\w.-]+)\}`)

// formattingMarkers matches the lightweight *bold* / _italic_ markers the
// form description supports. metaText strips the markers (keeping the
// wrapped text) so social-preview crawlers see clean prose rather than
// literal asterisks and underscores. Mirrors the client-side
// renderRichText in form.html — the two must stay in step.
var boldMarker = regexp.MustCompile(`\*([^*\n]+)\*`)
var italicMarker = regexp.MustCompile(`_([^_\n]+)_`)

// applySubstitutions replaces ${user.X} / ${query.X} / ${data.X} references
// in s with values from ctx (${data.X} being the outputs of the form's
// data-source flow). Unknown references resolve to empty string (matching
// the executor's ${flow.X} / ${user.X} semantic). Other namespaces
// (${secrets.X}, ${env.X}, ${flow.X}, ${var.X}) are intentionally left
// in place — they're either irrelevant at form render time or would
// leak data we shouldn't render publicly.
func applySubstitutions(s string, ctx substitutionContext) string {
	if s == "" {
		return s
	}
	return substitutionPattern.ReplaceAllStringFunc(s, func(match string) string {
		inner := match[2 : len(match)-1]
		dot := strings.IndexByte(inner, '.')
		if dot <= 0 {
			return match
		}
		ns := inner[:dot]
		key := inner[dot+1:]
		switch ns {
		case "user":
			if ctx.UserVariables == nil {
				return ""
			}
			return ctx.UserVariables[key]
		case "query":
			if ctx.QueryParams == nil {
				return ""
			}
			return ctx.QueryParams[key]
		case "data":
			if ctx.DataVariables == nil {
				return ""
			}
			return ctx.DataVariables[key]
		default:
			return match
		}
	})
}

// metaText produces a clean value for the page <title> and Open Graph /
// Twitter meta tags rendered server-side (so link-preview crawlers, which
// don't run our JavaScript, see the form's real title and description).
// It strips any leftover ${...} substitution tokens — for a public /
// unauthenticated crawler ${user.X} resolves to empty and namespaces we
// deliberately never expose (${secrets.X}, ${flow.X}, ${var.X}) would
// otherwise leak raw into the preview — then collapses whitespace to a
// single line. Attribute escaping is handled by html/template at render
// time; this concerns presentation, not injection safety.
func metaText(s string) string {
	if s == "" {
		return s
	}
	s = substitutionPattern.ReplaceAllString(s, "")
	// Drop the lightweight formatting markers so previews show clean prose
	// (e.g. "*Free* to attend" → "Free to attend"), not raw markers.
	s = boldMarker.ReplaceAllString(s, "$1")
	s = italicMarker.ReplaceAllString(s, "$1")
	return strings.Join(strings.Fields(s), " ")
}

// resolveFormForRender walks the form definition, resolving all
// substitutable fields (label, placeholder, default_value) per
// component. Pure function — returns a new definition without mutating
// the input.
func resolveFormForRender(def formDefinition, ctx substitutionContext) formDefinition {
	resolved := def
	resolved.Title = applySubstitutions(def.Title, ctx)
	resolved.Description = applySubstitutions(def.Description, ctx)
	resolved.Pages = make([]formPage, len(def.Pages))
	for pi, page := range def.Pages {
		comps := make([]formComponent, len(page.Components))
		for ci, c := range page.Components {
			comp := c
			comp.Label = applySubstitutions(c.Label, ctx)
			comp.Placeholder = applySubstitutions(c.Placeholder, ctx)
			comp.DefaultValue = applySubstitutions(c.DefaultValue, ctx)
			comps[ci] = comp
		}
		resolved.Pages[pi] = formPage{Components: comps}
	}
	// Interpolate the post-submission thank-you message the same way as labels,
	// so it can reference ${user.X}/${data.X}/${query.X}. Copy the struct so we
	// never mutate the (possibly cached) source definition.
	if resolved.Submit != nil {
		sub := *resolved.Submit
		sub.SuccessMessage = applySubstitutions(sub.SuccessMessage, ctx)
		resolved.Submit = &sub
	}
	return resolved
}

// formUsesDataNamespace reports whether any substitutable string in the form
// references ${data.X}. handleForm uses this to decide whether to run the
// data-source flow synchronously at render (blocking the GET) — a form that
// only uses the flow for dynamic dropdown options skips the blocking resolve
// and lets the browser fetch the data after first paint.
func formUsesDataNamespace(def formDefinition) bool {
	if strings.Contains(def.Title, "${data.") || strings.Contains(def.Description, "${data.") {
		return true
	}
	for _, page := range def.Pages {
		for _, c := range page.Components {
			if strings.Contains(c.Label, "${data.") ||
				strings.Contains(c.Placeholder, "${data.") ||
				strings.Contains(c.DefaultValue, "${data.") {
				return true
			}
		}
	}
	return false
}

// formHasDynamicOptions reports whether any option-based field draws its
// options from the data-source flow (options_source set).
func formHasDynamicOptions(def formDefinition) bool {
	for _, page := range def.Pages {
		for _, c := range page.Components {
			if c.OptionsSource != "" {
				return true
			}
		}
	}
	return false
}

// optionsFromOutput normalises a data-source output value into a form option
// list. Accepts an array of strings (["a","b"] → label==value) or an array of
// objects ([{label,value}] / [{name,value}]). Anything else yields no options.
func optionsFromOutput(val interface{}) []formOption {
	arr, ok := val.([]interface{})
	if !ok {
		return nil
	}
	out := make([]formOption, 0, len(arr))
	for _, entry := range arr {
		switch e := entry.(type) {
		case string:
			out = append(out, formOption{Label: e, Value: e})
		case map[string]interface{}:
			label := answerString(e["label"])
			if label == "" {
				label = answerString(e["name"])
			}
			value := answerString(e["value"])
			if value == "" {
				value = label
			}
			if label == "" {
				label = value
			}
			if label == "" && value == "" {
				continue
			}
			out = append(out, formOption{Label: label, Value: value})
		}
	}
	return out
}

// bakeDynamicOptions returns a copy of def with each option_source field's
// Options populated from the resolved data-source outputs. Used on submit so
// the whitelist enforcement in sanitiseOptionSubmissions covers dynamically-
// sourced options exactly as it does static ones. Pure — does not mutate def.
func bakeDynamicOptions(def formDefinition, outputs map[string]interface{}) formDefinition {
	if len(outputs) == 0 {
		return def
	}
	baked := def
	baked.Pages = make([]formPage, len(def.Pages))
	for pi, page := range def.Pages {
		comps := make([]formComponent, len(page.Components))
		for ci, c := range page.Components {
			comp := c
			if comp.OptionsSource != "" {
				comp.Options = optionsFromOutput(outputs[comp.OptionsSource])
			}
			comps[ci] = comp
		}
		baked.Pages[pi] = formPage{Components: comps, VisibleIf: page.VisibleIf}
	}
	return baked
}

// sanitiseOptionSubmissions enforces the option whitelist for radio,
// dropdown, and checkboxes fields. A client-supplied value that isn't in
// the field's Options list is discarded — silently, matching the read-
// only override philosophy of "trust the definition, not the client".
//
//   - radio / dropdown: value must be a string matching an Options entry;
//     anything else (wrong string, wrong type, missing) becomes "".
//   - checkboxes: value must be an array; entries outside the whitelist
//     are filtered out; anything not-an-array becomes an empty array.
//
// Fields with no Options entries are passed through unchanged (older
// forms without option-based fields shouldn't be affected).
//
// Returns a new map; does not mutate the input.
func sanitiseOptionSubmissions(submission map[string]interface{}, resolved formDefinition) map[string]interface{} {
	specs := map[string]formComponent{}
	for _, page := range resolved.Pages {
		for _, c := range page.Components {
			if len(c.Options) > 0 && (c.Type == "radio" || c.Type == "dropdown" || c.Type == "checkboxes" || c.Type == "ranking") {
				specs[c.Name] = c
			}
		}
	}
	if len(specs) == 0 {
		return submission
	}

	out := make(map[string]interface{}, len(submission))
	for k, v := range submission {
		out[k] = v
	}

	for name, spec := range specs {
		whitelist := map[string]struct{}{}
		for _, o := range spec.Options {
			whitelist[o.Value] = struct{}{}
		}

		switch spec.Type {
		case "radio", "dropdown":
			s, ok := out[name].(string)
			if !ok {
				out[name] = ""
				continue
			}
			if _, hit := whitelist[s]; !hit {
				out[name] = ""
			}
		case "checkboxes":
			arr, ok := out[name].([]interface{})
			if !ok {
				out[name] = []interface{}{}
				continue
			}
			filtered := make([]interface{}, 0, len(arr))
			for _, entry := range arr {
				s, ok := entry.(string)
				if !ok {
					continue
				}
				if _, hit := whitelist[s]; hit {
					filtered = append(filtered, s)
				}
			}
			out[name] = filtered
		case "ranking":
			// Ranking is checkboxes-with-order + dedupe: the client
			// posts an ordered array, we filter to whitelisted values
			// and drop repeats (a valid ranking has each option
			// appear at most once). Order is preserved.
			arr, ok := out[name].([]interface{})
			if !ok {
				out[name] = []interface{}{}
				continue
			}
			seen := map[string]struct{}{}
			filtered := make([]interface{}, 0, len(arr))
			for _, entry := range arr {
				s, ok := entry.(string)
				if !ok {
					continue
				}
				if _, dupe := seen[s]; dupe {
					continue
				}
				if _, hit := whitelist[s]; hit {
					seen[s] = struct{}{}
					filtered = append(filtered, s)
				}
			}
			out[name] = filtered
		}
	}
	return out
}

// stripDisplayOnlySubmissions removes keys whose corresponding component
// is a display-only type (section_header, divider, info_text). These
// components exist to structure the form visually, not to collect input,
// so a client-supplied value for them is nonsense and must not appear
// in the trigger data map (where it would pollute ${trigger.X} names
// and confuse downstream flow authors).
//
// Returns a new map; does not mutate the input.
func stripDisplayOnlySubmissions(submission map[string]interface{}, resolved formDefinition) map[string]interface{} {
	displayNames := map[string]struct{}{}
	for _, page := range resolved.Pages {
		for _, c := range page.Components {
			if isDisplayOnly(c.Type) {
				displayNames[c.Name] = struct{}{}
			}
		}
	}
	if len(displayNames) == 0 {
		return submission
	}
	out := make(map[string]interface{}, len(submission))
	for k, v := range submission {
		if _, isDisplay := displayNames[k]; isDisplay {
			continue
		}
		out[k] = v
	}
	return out
}

// stripReadOnlySubmissions removes any keys from submission that
// correspond to read_only fields in the form definition. The baked-at-
// render default_value is what the trigger should receive — restored
// from the resolved form rather than trusting the client.
//
// Returns a new map; does not mutate the input.
func stripReadOnlySubmissions(submission map[string]interface{}, resolved formDefinition) map[string]interface{} {
	readOnly := map[string]string{}
	for _, page := range resolved.Pages {
		for _, c := range page.Components {
			// Structured types (location, address) produce a nested object
			// response and have no meaningful string DefaultValue — skipping
			// them here means a hand-authored read_only: true is ignored
			// rather than corrupting the response shape.
			if c.Type == "location" || c.Type == "address" {
				continue
			}
			if c.ReadOnly {
				readOnly[c.Name] = c.DefaultValue
			}
		}
	}

	out := make(map[string]interface{}, len(submission)+len(readOnly))
	for k, v := range submission {
		if _, isReadOnly := readOnly[k]; isReadOnly {
			continue
		}
		out[k] = v
	}
	for k, v := range readOnly {
		out[k] = v
	}
	return out
}

// evalVisibility reports whether a component with the given rule should be
// visible, given the current answers. A nil or empty rule is always
// visible (the default). "all" combines clauses with AND, "any" with OR.
// This is the Go twin of the identical evaluator in form.html — keep the
// two in lock-step so the server strips exactly what the client hid.
func evalVisibility(rule *visibilityRule, values map[string]interface{}) bool {
	if rule == nil || len(rule.Rules) == 0 {
		return true
	}
	if rule.Match == "any" {
		for _, c := range rule.Rules {
			if evalClause(c, values) {
				return true
			}
		}
		return false
	}
	// Default (and explicit "all") is AND.
	for _, c := range rule.Rules {
		if !evalClause(c, values) {
			return false
		}
	}
	return true
}

// evalClause evaluates a single condition against the answer map. An
// unrecognised operator resolves to true (visible) so an unknown rule
// never silently hides a field.
func evalClause(c visibilityClause, values map[string]interface{}) bool {
	raw := values[c.Field]
	target := c.Value
	switch c.Op {
	case "empty":
		return isEmptyAnswer(raw)
	case "not_empty":
		return !isEmptyAnswer(raw)
	case "equals":
		return answerString(raw) == target
	case "not_equals":
		return answerString(raw) != target
	case "contains":
		if arr, ok := raw.([]interface{}); ok {
			return answerArrayContains(arr, target)
		}
		return strings.Contains(answerString(raw), target)
	case "not_contains":
		if arr, ok := raw.([]interface{}); ok {
			return !answerArrayContains(arr, target)
		}
		return !strings.Contains(answerString(raw), target)
	case "starts_with":
		return strings.HasPrefix(answerString(raw), target)
	case "ends_with":
		return strings.HasSuffix(answerString(raw), target)
	case "one_of":
		options := splitCSV(target)
		if arr, ok := raw.([]interface{}); ok {
			for _, o := range options {
				if answerArrayContains(arr, o) {
					return true
				}
			}
			return false
		}
		s := answerString(raw)
		for _, o := range options {
			if s == o {
				return true
			}
		}
		return false
	case "greater_than":
		a, aok := parseNumber(answerString(raw))
		b, bok := parseNumber(target)
		return aok && bok && a > b
	case "less_than":
		a, aok := parseNumber(answerString(raw))
		b, bok := parseNumber(target)
		return aok && bok && a < b
	default:
		return true
	}
}

// isEmptyAnswer treats nil, "", and empty arrays as empty. Everything else
// (including false and 0) is a present answer.
func isEmptyAnswer(raw interface{}) bool {
	switch v := raw.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []interface{}:
		return len(v) == 0
	default:
		return false
	}
}

// answerString renders a scalar answer for string comparisons. Arrays are
// joined with commas; bools become "true"/"false"; whole-number floats
// (JSON's default numeric type) drop the trailing ".0".
func answerString(raw interface{}) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, e := range v {
			parts = append(parts, answerString(e))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprintf("%v", v)
	}
}

// answerArrayContains reports whether any entry of a multi-value answer
// (checkboxes / ranking) equals target.
func answerArrayContains(arr []interface{}, target string) bool {
	for _, e := range arr {
		if answerString(e) == target {
			return true
		}
	}
	return false
}

// splitCSV splits a comma-separated operator value, trimming surrounding
// whitespace and dropping empties. Used by the one_of operator.
func splitCSV(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// parseNumber parses a numeric answer for greater_than / less_than. Returns
// false when the value isn't a number, so a non-numeric field never
// satisfies a numeric comparison.
func parseNumber(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// stripHiddenSubmissions removes answers for components whose visibility
// rule evaluates to hidden given the submitted answers. It iterates to a
// fixed point: hiding one field removes its value, which can flip another
// field's condition that referenced it, so a single pass is insufficient.
// The iteration is bounded by the component count (each pass hides at least
// one field or stops), guaranteeing termination.
//
// Returns a new map; does not mutate the input.
func stripHiddenSubmissions(submission map[string]interface{}, resolved formDefinition) map[string]interface{} {
	// A "unit" is one hideable thing: a single conditional component, or a
	// whole conditional page (which owns the names of all its components).
	// Both are evaluated in the same loop so a page condition and a field
	// condition can each depend on the other's cleared value.
	type unit struct {
		names []string
		rule  *visibilityRule
	}
	var units []unit
	for _, page := range resolved.Pages {
		if page.VisibleIf != nil && len(page.VisibleIf.Rules) > 0 {
			names := make([]string, 0, len(page.Components))
			for _, c := range page.Components {
				names = append(names, c.Name)
			}
			units = append(units, unit{names: names, rule: page.VisibleIf})
		}
		for _, c := range page.Components {
			if c.VisibleIf != nil && len(c.VisibleIf.Rules) > 0 {
				units = append(units, unit{names: []string{c.Name}, rule: c.VisibleIf})
			}
		}
	}

	out := make(map[string]interface{}, len(submission))
	for k, v := range submission {
		out[k] = v
	}
	if len(units) == 0 {
		return out
	}

	for pass := 0; pass <= len(units); pass++ {
		changed := false
		for _, u := range units {
			if evalVisibility(u.rule, out) {
				continue
			}
			for _, n := range u.names {
				if _, present := out[n]; present {
					delete(out, n)
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	return out
}

// loadUserVariables fetches the user's ${user.X} variable map from the API.
// Returns nil + nil error when the user is unauthenticated, propagating
// to "no ${user.X} resolution" semantics rather than failing the render.
func (s *Service) loadUserVariables(userID string) (map[string]string, error) {
	if userID == "" {
		return nil, nil
	}
	url := strings.TrimRight(s.config.InternalAPIURL(), "/") + "/api/v1/internal/user/" + userID + "/variables"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.apiClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("user variables fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("user variables endpoint returned %d", resp.StatusCode)
	}
	var vars map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&vars); err != nil {
		return nil, fmt.Errorf("decode user variables: %w", err)
	}
	return vars, nil
}

// resolveSessionUser inspects the flomation-token cookie or Authorization
// header, returns the user_id and nil on success, or empty + nil if
// unauthenticated. Errors are logged and treated as unauthenticated so a
// flaky Sentinel call doesn't block a form load.
func (s *Service) resolveSessionUser(token string) string {
	if token == "" || s.config.Security.IdentityService == "" {
		return ""
	}
	userID, err := sentinel.GetUser(s.config.Security.IdentityService, token)
	if err != nil || userID == nil {
		log.WithFields(log.Fields{"error": err}).Debug("session token did not resolve to a user")
		return ""
	}
	return *userID
}

// extractSessionToken reads the session token from the flomation-token
// cookie first, then the Authorization: Bearer header. Mirrors the
// pattern used by Sentinel itself per CLAUDE.md.
func extractSessionToken(headerAuth string, cookieValue string) string {
	if cookieValue != "" {
		return cookieValue
	}
	parts := strings.SplitN(headerAuth, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

// parseFormDefinition decodes the trigger.Data bytes into a formDefinition.
// Returns the zero value on error so callers can fall through with a
// sensible empty form rather than crash.
func parseFormDefinition(data []byte) (formDefinition, error) {
	var def formDefinition
	if err := json.Unmarshal(data, &def); err != nil {
		return def, err
	}
	return def, nil
}
