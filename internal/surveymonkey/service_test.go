package surveymonkey

import "testing"

const samplePayload = `{
	"name": "Response Completed",
	"event_type": "response_completed",
	"object_type": "response",
	"object_id": "5981234567890",
	"event_datetime": "2026-07-11T10:00:00.000Z",
	"resources": {
		"survey_id": "2519087654321",
		"collector_id": "412345678",
		"respondent_id": "9987654321"
	}
}`

func TestParse(t *testing.T) {
	ev, err := Parse([]byte(samplePayload))
	if err != nil {
		t.Fatalf("expected valid parse, got error: %v", err)
	}
	if ev.EventType != "response_completed" {
		t.Errorf("expected event_type response_completed, got %q", ev.EventType)
	}
	if ev.ObjectType != "response" {
		t.Errorf("expected object_type response, got %q", ev.ObjectType)
	}
	if ev.ObjectID != "5981234567890" {
		t.Errorf("expected object_id 5981234567890, got %q", ev.ObjectID)
	}
	if ev.Resources.SurveyID != "2519087654321" {
		t.Errorf("expected survey_id 2519087654321, got %q", ev.Resources.SurveyID)
	}
	if ev.Resources.CollectorID != "412345678" {
		t.Errorf("expected collector_id 412345678, got %q", ev.Resources.CollectorID)
	}
	if ev.Resources.RespondentID != "9987654321" {
		t.Errorf("expected respondent_id 9987654321, got %q", ev.Resources.RespondentID)
	}
}

func TestParse_Invalid(t *testing.T) {
	if _, err := Parse([]byte("not-json")); err == nil {
		t.Fatal("expected error parsing an invalid JSON body, got nil")
	}
}

func TestEventToData(t *testing.T) {
	ev, err := Parse([]byte(samplePayload))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	data := EventToData(ev, []byte(samplePayload))

	checks := map[string]string{
		"event_type":  "response_completed",
		"object_type": "response",
		"object_id":   "5981234567890",
		"survey_id":   "2519087654321",
		"response_id": "9987654321",
		"body":        samplePayload,
		"content":     "SurveyMonkey response_completed on survey 2519087654321",
	}
	for k, want := range checks {
		if got, _ := data[k].(string); got != want {
			t.Errorf("EventToData[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestMatchesFilter(t *testing.T) {
	cases := []struct {
		surveyID string
		filter   string
		want     bool
	}{
		{"abc123", "", true},         // empty = all
		{"abc123", "abc123", true},   // exact
		{"abc123", " abc123 ", true}, // whitespace tolerant
		{"abc123", "other", false},   // mismatch
	}
	for _, c := range cases {
		if got := MatchesFilter(c.surveyID, c.filter); got != c.want {
			t.Errorf("MatchesFilter(%q, %q) = %v, want %v", c.surveyID, c.filter, got, c.want)
		}
	}
}

func TestTokenOK(t *testing.T) {
	cases := []struct {
		name     string
		secret   string
		provided string
		want     bool
	}{
		{"no secret configured accepts anything", "", "", true},
		{"no secret configured ignores provided token", "", "whatever", true},
		{"matching token accepts", "s3cr3t", "s3cr3t", true},
		{"mismatched token rejects", "s3cr3t", "wrong", false},
		{"missing token rejects when secret set", "s3cr3t", "", false},
	}
	for _, c := range cases {
		if got := TokenOK(c.secret, c.provided); got != c.want {
			t.Errorf("%s: TokenOK(%q, %q) = %v, want %v", c.name, c.secret, c.provided, got, c.want)
		}
	}
}
