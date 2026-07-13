package manualtrigger

import (
	"reflect"
	"sort"
	"testing"
)

func schema() []TriggerInput {
	return []TriggerInput{
		{Name: "name", Type: "string", Required: true},
		{Name: "age", Type: "integer", Required: true},
		{Name: "subscribe", Type: "boolean", Required: false},
		{Name: "plan", Type: "dropdown", Required: true, Options: []TriggerInputOption{
			{Label: "Basic", Value: "basic"},
			{Label: "Pro", Value: "pro"},
		}},
		{Name: "starts", Type: "date", Required: false},
	}
}

func sortedEqual(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

func TestValidate_AllGood(t *testing.T) {
	data := map[string]interface{}{
		"name":      "Ada",
		"age":       float64(42),
		"subscribe": true,
		"plan":      "pro",
		"starts":    "2026-07-12",
	}
	if got := ValidateTriggerInputs(schema(), data); len(got) != 0 {
		t.Fatalf("expected no offending fields, got %v", got)
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	data := map[string]interface{}{
		"age":  float64(42),
		"plan": "basic",
		// name absent, empty required string also offends
	}
	got := ValidateTriggerInputs(schema(), data)
	if !sortedEqual(got, []string{"name"}) {
		t.Fatalf("expected [name], got %v", got)
	}

	// Present but blank/whitespace counts as missing for a required field.
	data["name"] = "   "
	got = ValidateTriggerInputs(schema(), data)
	if !sortedEqual(got, []string{"name"}) {
		t.Fatalf("blank required: expected [name], got %v", got)
	}
}

func TestValidate_TypeMismatches(t *testing.T) {
	data := map[string]interface{}{
		"name":      "Ada",
		"age":       "not-a-number",
		"subscribe": "maybe",
		"plan":      "pro",
	}
	got := ValidateTriggerInputs(schema(), data)
	if !sortedEqual(got, []string{"age", "subscribe"}) {
		t.Fatalf("expected [age subscribe], got %v", got)
	}
}

func TestValidate_IntegerAcceptsNumericString(t *testing.T) {
	data := map[string]interface{}{
		"name": "Ada", "age": "42", "plan": "basic",
	}
	if got := ValidateTriggerInputs(schema(), data); len(got) != 0 {
		t.Fatalf("numeric string age should pass, got %v", got)
	}
}

func TestValidate_BooleanForms(t *testing.T) {
	for _, v := range []interface{}{true, false, "true", "FALSE", "True"} {
		d := map[string]interface{}{"name": "Ada", "age": float64(1), "plan": "basic", "subscribe": v}
		if got := ValidateTriggerInputs(schema(), d); len(got) != 0 {
			t.Fatalf("boolean value %v should pass, got %v", v, got)
		}
	}
}

func TestValidate_DropdownNotInOptions(t *testing.T) {
	data := map[string]interface{}{
		"name": "Ada", "age": float64(1), "plan": "enterprise",
	}
	got := ValidateTriggerInputs(schema(), data)
	if !sortedEqual(got, []string{"plan"}) {
		t.Fatalf("expected [plan], got %v", got)
	}
}

func TestValidate_DropdownNumericValueMatch(t *testing.T) {
	sch := []TriggerInput{{Name: "level", Type: "dropdown", Required: true, Options: []TriggerInputOption{
		{Label: "One", Value: "1"}, {Label: "Two", Value: "2"},
	}}}
	// A JSON number 2 must match the option value "2".
	if got := ValidateTriggerInputs(sch, map[string]interface{}{"level": float64(2)}); len(got) != 0 {
		t.Fatalf("numeric dropdown match should pass, got %v", got)
	}
	if got := ValidateTriggerInputs(sch, map[string]interface{}{"level": float64(9)}); !sortedEqual(got, []string{"level"}) {
		t.Fatalf("numeric dropdown miss should fail, got %v", got)
	}
}

func TestValidate_DateForms(t *testing.T) {
	for _, v := range []string{"2026-07-12", "2026-07-12T15:04:05Z", "12/07/2026", "2026-07-12T15:04"} {
		d := map[string]interface{}{"name": "Ada", "age": float64(1), "plan": "basic", "starts": v}
		if got := ValidateTriggerInputs(schema(), d); len(got) != 0 {
			t.Fatalf("date %q should pass, got %v", v, got)
		}
	}
	bad := map[string]interface{}{"name": "Ada", "age": float64(1), "plan": "basic", "starts": "not-a-date"}
	if got := ValidateTriggerInputs(schema(), bad); !sortedEqual(got, []string{"starts"}) {
		t.Fatalf("bad date should fail, got %v", got)
	}
}

func TestValidate_OptionalEmptySkipsTypeCheck(t *testing.T) {
	// starts is an optional date; absent or empty must not fail even
	// though "" is not a valid date.
	data := map[string]interface{}{"name": "Ada", "age": float64(1), "plan": "basic", "starts": ""}
	if got := ValidateTriggerInputs(schema(), data); len(got) != 0 {
		t.Fatalf("empty optional date should pass, got %v", got)
	}
}
