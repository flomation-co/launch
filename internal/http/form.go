package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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
}

type formPage struct {
	Components []formComponent `json:"components"`
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
}

var substitutionPattern = regexp.MustCompile(`\$\{([\w.-]+)\}`)

// applySubstitutions replaces ${user.X} / ${query.X} references in s with
// values from ctx. Unknown references resolve to empty string (matching
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
		default:
			return match
		}
	})
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
	return resolved
}

// sanitiseOptionSubmissions enforces the option whitelist for radio,
// dropdown, and checkboxes fields. A client-supplied value that isn't in
// the field's Options list is discarded — silently, matching the read-
// only override philosophy of "trust the definition, not the client".
//
// - radio / dropdown: value must be a string matching an Options entry;
//   anything else (wrong string, wrong type, missing) becomes "".
// - checkboxes: value must be an array; entries outside the whitelist
//   are filtered out; anything not-an-array becomes an empty array.
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
