package schedule

import (
	"encoding/json"
	"testing"
)

// TestFlexBool_AcceptsBothShapes is the regression for a live schedule that
// never fired.
//
// exclude_bank_holidays is the only ConnectionTypeBoolean input on the schedule
// trigger, and it was parsed as a string. json.Unmarshal fails on the WHOLE
// config when one field disagrees, and checkTrigger returns on that error — so
// a single boolean stopped the entire schedule silently, every fifteen seconds,
// forever. Live trigger de7417f6 had "exclude_bank_holidays": false and had
// never fired: the box was not even ticked.
func TestFlexBool_AcceptsBothShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"real boolean true", `true`, true},
		{"real boolean false", `false`, false},
		{"string true", `"true"`, true},
		{"string false", `"false"`, false},
		{"string TRUE", `"TRUE"`, true},
		{"padded string", `"  true  "`, true},
		{"one", `"1"`, true},
		{"zero", `"0"`, false},
		{"yes", `"yes"`, true},
		{"empty string", `""`, false},
		{"null", `null`, false},
		{"unrecognised word", `"perhaps"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var f FlexBool
			if err := json.Unmarshal([]byte(c.raw), &f); err != nil {
				t.Fatalf("unmarshal %s: %v", c.raw, err)
			}
			if f.Bool() != c.want {
				t.Errorf("%s -> %v, want %v", c.raw, f.Bool(), c.want)
			}
		})
	}
}

func TestFlexBool_RejectsNonsense(t *testing.T) {
	var f FlexBool
	if err := json.Unmarshal([]byte(`{"not":"a bool"}`), &f); err == nil {
		t.Error("an object should not parse as a boolean")
	}
	if err := json.Unmarshal([]byte(`[1,2]`), &f); err == nil {
		t.Error("an array should not parse as a boolean")
	}
}

// TestScheduleConfig_ParsesBothStoredShapes is the test that would have caught
// this: the whole config, exactly as the two live shapes store it.
func TestScheduleConfig_ParsesBothStoredShapes(t *testing.T) {
	// The shape that broke: a real boolean, as trigger de7417f6 stored it.
	broken := `{
		"id":"de7417f6-5b1a-4192-ade9-e70a75f9b4db",
		"mode":"daily","unit":"","interval":"","timezone":"",
		"__node_id":"be2b09fa-61f7-4016-a8d1-e3b7faada2c8",
		"time_of_day":"08:00","days_of_week":"",
		"exclude_bank_holidays":false
	}`
	// The shape that worked: a string, as the other four live triggers store it.
	working := `{
		"mode":"daily","time_of_day":"08:00","timezone":"Europe/London",
		"exclude_bank_holidays":"true"
	}`

	for name, raw := range map[string]string{"boolean": broken, "string": working} {
		t.Run(name, func(t *testing.T) {
			var cfg ScheduleConfig
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatalf("config must parse, got: %v", err)
			}
			if cfg.Mode != "daily" || cfg.TimeOfDay != "08:00" {
				t.Errorf("the rest of the config must survive: %+v", cfg)
			}
		})
	}

	// And the values are read correctly, not merely parsed.
	var a, b ScheduleConfig
	_ = json.Unmarshal([]byte(broken), &a)
	_ = json.Unmarshal([]byte(working), &b)
	if a.ExcludeBankHolidays.Bool() {
		t.Error("a stored false must mean do not exclude")
	}
	if !b.ExcludeBankHolidays.Bool() {
		t.Error(`a stored "true" must mean exclude`)
	}
}

// TestScheduleConfig_RoundTripsAsBoolean confirms anything Launch writes back
// comes out in the shape the executor declares.
func TestScheduleConfig_RoundTripsAsBoolean(t *testing.T) {
	var cfg ScheduleConfig
	if err := json.Unmarshal([]byte(`{"mode":"daily","exclude_bank_holidays":"true"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var reparsed map[string]interface{}
	if err := json.Unmarshal(out, &reparsed); err != nil {
		t.Fatal(err)
	}
	if reparsed["exclude_bank_holidays"] != true {
		t.Errorf("should marshal as a real boolean, got %#v", reparsed["exclude_bank_holidays"])
	}
}
