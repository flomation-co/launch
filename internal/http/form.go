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
	Name         string `json:"name"`
	Label        string `json:"label"`
	Type         string `json:"type"`
	Placeholder  string `json:"placeholder"`
	Required     bool   `json:"required"`
	Order        int    `json:"order"`
	ReadOnly     bool   `json:"read_only,omitempty"`
	DefaultValue string `json:"default_value,omitempty"`
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
