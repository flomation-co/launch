// Package manualtrigger holds the shared schema types and input
// validation for programmatic manual-trigger runs. Keeping the schema
// and validator here (rather than inline in the HTTP handler) keeps the
// validation logic unit-testable without a running Gin engine.
//
// The validation semantics deliberately mirror the API service's
// manual-trigger input validation so a run rejected by Launch would be
// rejected by the API too (and vice versa):
//
//   - required fields must be present and non-empty
//   - integer fields must be numeric
//   - boolean fields must be a real bool or the string "true"/"false"
//   - dropdown fields must carry a value listed in the field's options
//   - date fields must be parseable
package manualtrigger

import (
	"strconv"
	"strings"
	"time"
)

// TriggerInputOption is a single dropdown option (label/value pair).
type TriggerInputOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// TriggerInput describes one declared input on a manual trigger. It is
// the Launch-side twin of the API's manual-trigger input schema and is
// unmarshalled from the trigger's registered Data blob.
type TriggerInput struct {
	Name        string               `json:"name"`
	Label       string               `json:"label"`
	Type        string               `json:"type"`
	Required    bool                 `json:"required"`
	Placeholder string               `json:"placeholder"`
	Value       string               `json:"value"`
	Options     []TriggerInputOption `json:"options"`
}

// dateLayouts is the set of layouts a date input may satisfy. Ordered
// most-specific first; the first that parses wins.
var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04", // HTML datetime-local
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02", // HTML date
	"02/01/2006", // DMY (British)
	"01/02/2006", // MDY
	time.RFC1123,
	time.RFC1123Z,
}

// ValidateTriggerInputs checks a submitted input map against the
// trigger's declared schema and returns the names of any offending
// fields. An empty (nil) return means the data is valid.
//
// Only declared inputs are validated; unknown keys in data are ignored
// (the executor treats the trigger data as an opaque map). Optional
// fields that are absent or empty pass without a type check.
func ValidateTriggerInputs(schema []TriggerInput, data map[string]interface{}) []string {
	var offending []string

	for _, in := range schema {
		raw, present := data[in.Name]
		empty := isEmpty(raw)

		if in.Required && (!present || empty) {
			offending = append(offending, in.Name)
			continue
		}

		// Optional & unsupplied/empty — nothing further to check.
		if empty {
			continue
		}

		if !valueMatchesType(in, raw) {
			offending = append(offending, in.Name)
		}
	}

	return offending
}

// isEmpty reports whether a submitted value counts as "not provided".
// Only nil and blank/whitespace strings are empty — a boolean false or
// a numeric zero is a legitimate answer and must not be treated as
// missing.
func isEmpty(raw interface{}) bool {
	if raw == nil {
		return true
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

// valueMatchesType checks a non-empty value against its declared type.
func valueMatchesType(in TriggerInput, raw interface{}) bool {
	switch strings.ToLower(strings.TrimSpace(in.Type)) {
	case "integer":
		return isNumeric(raw)
	case "boolean":
		return isBoolean(raw)
	case "dropdown":
		return inOptions(in.Options, raw)
	case "date":
		return isDate(raw)
	default:
		// string, text and any unknown type accept any non-empty value.
		return true
	}
}

// isNumeric reports whether raw represents a number, whether supplied
// as a JSON number or as a numeric string.
func isNumeric(raw interface{}) bool {
	switch v := raw.(type) {
	case float64:
		return true
	case float32:
		return true
	case int, int8, int16, int32, int64:
		return true
	case string:
		_, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil
	default:
		return false
	}
}

// isBoolean reports whether raw is a real bool or the string
// "true"/"false" (case-insensitive).
func isBoolean(raw interface{}) bool {
	switch v := raw.(type) {
	case bool:
		return true
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "false"
	default:
		return false
	}
}

// inOptions reports whether raw's string form matches one of the
// declared option values.
func inOptions(options []TriggerInputOption, raw interface{}) bool {
	got := stringify(raw)
	for _, o := range options {
		if o.Value == got {
			return true
		}
	}
	return false
}

// isDate reports whether raw's string form parses against any of the
// accepted date layouts.
func isDate(raw interface{}) bool {
	s := strings.TrimSpace(stringify(raw))
	if s == "" {
		return false
	}
	for _, layout := range dateLayouts {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// stringify renders a submitted value as a string for comparison
// (dropdown option matching, date parsing). JSON numbers arrive as
// float64; format them without a trailing ".0" or exponent so an
// integer option value like "3" matches a submitted 3.
func stringify(raw interface{}) string {
	switch v := raw.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}
