package salesforcepoll

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(soqlTimeLayout, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return v
}

// THE test. Salesforce datetimes are second-granular and ties are real — two
// Accounts in the validation org share 2026-07-22T12:31:12.000 exactly, because
// anything touching several records at once stamps them all in the same second.
//
// dbrow's approach (advance the cursor, ask for `> watermark`) is correct only
// when the cursor is unique per row. Here it would drop every record sharing the
// boundary second after the first, permanently and silently — the worst class of
// bug this trigger could have, because nothing surfaces it: no error, no retry,
// the record simply never fires.
func TestQueryComparisonIsInclusiveSoTiesAreNotDropped(t *testing.T) {
	w := mustTime(t, "2026-07-22T12:31:12Z")
	q := buildQuery("Account", []string{"Id", "Name", "SystemModstamp"}, "SystemModstamp", "", w, true)

	if !strings.Contains(q, "SystemModstamp >= 2026-07-22T12:31:12Z") {
		t.Errorf("the comparison must be INCLUSIVE (>=) or same-second records are lost: %q", q)
	}
	if strings.Contains(q, "SystemModstamp > 2026-07-22T12:31:12Z") &&
		!strings.Contains(q, ">= 2026-07-22T12:31:12Z") {
		t.Errorf("exclusive comparison found — this silently drops tied records: %q", q)
	}
	// Id is the tie-break, so the order within a shared second is stable across
	// polls; without it the same second could be paged differently each time.
	if !strings.Contains(q, "ORDER BY SystemModstamp ASC, Id ASC") {
		t.Errorf("ordering must break ties on Id for a stable scan: %q", q)
	}
	// A SOQL datetime literal is unquoted; quoting it is rejected as malformed.
	if strings.Contains(q, "'2026-07-22") {
		t.Errorf("SOQL datetime literals must not be quoted: %q", q)
	}
}

// The watermark is absent until the first baseline stores one; an object that was
// empty then must not produce a WHERE clause at all.
func TestNoWatermarkMeansNoCondition(t *testing.T) {
	q := buildQuery("Lead", []string{"Id"}, "CreatedDate", "", time.Time{}, false)
	if strings.Contains(q, "WHERE") {
		t.Errorf("without a watermark there is nothing to filter on: %q", q)
	}
}

// The operator's filter is a SOQL fragment, so an OR inside it must not be able
// to escape our watermark condition and widen the query to the whole object —
// which would re-fire the entire table on every poll.
func TestOperatorFilterIsParenthesisedAgainstOrEscape(t *testing.T) {
	w := mustTime(t, "2026-07-22T12:31:12Z")
	q := buildQuery("Lead", []string{"Id"}, "SystemModstamp", "Status = 'Open' OR Status = 'Working'", w, true)

	if !strings.Contains(q, "AND (Status = 'Open' OR Status = 'Working')") {
		t.Errorf("the operator's filter must be parenthesised: %q", q)
	}
	// Prove the shape: watermark AND (their filter), never watermark AND x OR y.
	if strings.Contains(q, "AND Status = 'Open' OR") {
		t.Errorf("un-parenthesised OR would widen the query to the whole object: %q", q)
	}
}

func TestFieldListAlwaysCarriesIdAndTheCursor(t *testing.T) {
	fields, err := fieldList("", "SystemModstamp")
	if err != nil {
		t.Fatalf("the default field list must be valid: %v", err)
	}
	joined := strings.Join(fields, ",")
	if fields[0] != "Id" {
		t.Errorf("Id must lead the select: %v", fields)
	}
	if !strings.Contains(joined, "SystemModstamp") {
		t.Errorf("the cursor field must be selected or the poller cannot read it back: %v", fields)
	}

	// An operator naming the cursor field themselves must not get it twice —
	// duplicate fields are a SOQL error, not a harmless repetition.
	fields, err = fieldList("Name, SystemModstamp", "SystemModstamp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n := 0
	for _, f := range fields {
		if f == "SystemModstamp" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("SystemModstamp appears %d times, want 1: %v", n, fields)
	}

	// Case differences are the same field to Salesforce.
	fields, _ = fieldList("id, NAME", "SystemModstamp")
	ids := 0
	for _, f := range fields {
		if strings.EqualFold(f, "Id") {
			ids++
		}
	}
	if ids != 1 {
		t.Errorf("Id must not be duplicated by case: %v", fields)
	}
}

func TestFieldAndObjectNamesAreValidated(t *testing.T) {
	// A relationship field is one dot deep and legitimately wanted.
	if !validFieldName("Owner.Email") {
		t.Error("a relationship field must be allowed")
	}
	for _, bad := range []string{
		"Name, Id",          // a second field smuggled through one entry
		"Name FROM Account", // clause injection
		"Name'",             // quote escape
		"a.b.c",             // deeper than Salesforce allows here
		"(SELECT Id)",       // sub-select
		"",                  // empty
		"1Name",             // must start with a letter
	} {
		if validFieldName(bad) {
			t.Errorf("field name %q must be refused", bad)
		}
	}
	for _, good := range []string{"Account", "My_Object__c"} {
		if !validObjectName(good) {
			t.Errorf("object name %q must be allowed", good)
		}
	}
	for _, bad := range []string{"Account WHERE Id != null", "Account,Contact", "Acc'ount", "", "_Account", "a.b"} {
		if validObjectName(bad) {
			t.Errorf("object name %q must be refused", bad)
		}
	}
}

func TestFilterRefusesWhatCouldRewriteOurClause(t *testing.T) {
	for _, bad := range []string{
		"Status = 'Open",                 // unclosed quote
		"Id != null LIMIT 1",             // their LIMIT would override ours
		"Id != null ORDER BY Name",       // their ORDER BY breaks the cursor scan
		"Id IN (SELECT Id FROM Account)", // sub-select reaches another object
		"Id != null -- ignore the rest",  // comment could hide our watermark
		"Id != null /* comment */",       //
		"Id != null OFFSET 5",            // paging we do not control
	} {
		if err := validateWhere(bad); err == nil {
			t.Errorf("filter %q must be refused", bad)
		}
	}
	for _, good := range []string{
		"",
		"Status = 'Open'",
		"Amount > 1000 AND IsWon = false",
		"Name LIKE 'Acme%'",
		"Status = 'It''s open'", // escaped quote — balanced, so allowed
	} {
		if err := validateWhere(good); err != nil {
			t.Errorf("filter %q must be allowed, got %v", good, err)
		}
	}
}

// Launch polls on a schedule holding a live access token. An instance URL that
// has been tampered with would quietly forward that token to somebody else's
// server, on a timer, with nobody watching — so the host is pinned here and not
// only in the editor.
func TestInstanceURLIsPinnedToSalesforceHosts(t *testing.T) {
	for _, bad := range []string{
		"https://evil.example.com",
		"http://mycompany.my.salesforce.com",          // plaintext
		"https://user:pw@mycompany.my.salesforce.com", // credentials in the URL
		"https://salesforce.com.evil.example",         // suffix-lookalike
		"https://.salesforce.com",                     // empty label
		"",
	} {
		if _, err := normaliseInstanceURL(bad); err == nil {
			t.Errorf("instance URL %q must be refused", bad)
		}
	}
	for _, good := range []string{
		"https://mycompany.my.salesforce.com",
		"mycompany.my.salesforce.com", // scheme added for them
		"https://MYCOMPANY.my.salesforce.com/services/data/v62.0/",
	} {
		got, err := normaliseInstanceURL(good)
		if err != nil {
			t.Fatalf("instance URL %q must be allowed, got %v", good, err)
		}
		if !strings.HasPrefix(got, "https://") || strings.Contains(got[8:], "/") {
			t.Errorf("normalised URL should be scheme+host only, got %q", got)
		}
		if strings.ToLower(got) != got {
			t.Errorf("host should be lowercased, got %q", got)
		}
	}
}

// Salesforce renders datetimes as 2026-07-26T13:50:38.000+0000 — note the offset
// has no colon, so it is NOT RFC3339 and time.RFC3339 alone fails to parse it.
func TestRecordStampParsesSalesforcesOwnFormat(t *testing.T) {
	rec := map[string]interface{}{"SystemModstamp": "2026-07-26T13:50:38.000+0000"}
	got, ok := recordStamp(rec, "SystemModstamp")
	if !ok {
		t.Fatal("Salesforce's own datetime format must parse")
	}
	if want := mustTime(t, "2026-07-26T13:50:38Z"); !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
	if _, ok := recordStamp(map[string]interface{}{}, "SystemModstamp"); ok {
		t.Error("a missing stamp must report not-ok rather than the zero time")
	}
}

func TestIntervalDefaultsToFifteenMinutesAndCannotGoBelowTheFloor(t *testing.T) {
	// The default is the number almost everyone runs, and it spends the customer's
	// API allowance: 15m is 96 calls a day per trigger, 60s would be 1,440.
	if got := parseInterval(""); got != 15*time.Minute {
		t.Errorf("blank interval must default to 15m, got %v", got)
	}
	if got := parseInterval("not a duration"); got != 15*time.Minute {
		t.Errorf("unparseable interval must default to 15m, got %v", got)
	}
	// Assert the CONSTANT, not just that clamping reaches it. Comparing
	// parseInterval("1s") against MinPollInterval is self-referential: drop the
	// floor to a second and the test still passes while the API budget quietly
	// blows up. The floor is a product decision, so pin the number.
	if MinPollInterval != time.Minute {
		t.Errorf("MinPollInterval is %v, want 1m — a lower floor lets one trigger spend 1,440+ API calls a day", MinPollInterval)
	}
	if got := parseInterval("1s"); got != MinPollInterval {
		t.Errorf("a sub-floor interval must clamp to %v, got %v", MinPollInterval, got)
	}
	// Same reasoning for the default, which is the number almost everyone runs.
	if TriggerDefaultInterval != 15*time.Minute {
		t.Errorf("TriggerDefaultInterval is %v, want 15m", TriggerDefaultInterval)
	}
	if got := parseInterval("30m"); got != 30*time.Minute {
		t.Errorf("a longer interval must be honoured, got %v", got)
	}
}

func TestCursorFieldFallsBackToTheEventThatCannotMissEdits(t *testing.T) {
	if got := (triggerConfig{Event: "CreatedDate"}).cursorField(); got != "CreatedDate" {
		t.Errorf("got %q", got)
	}
	// A blank or unrecognised event must never reach the query verbatim, and the
	// fallback should be the one that cannot miss an edit.
	for _, ev := range []string{"", "nonsense", "Id FROM Account"} {
		if got := (triggerConfig{Event: ev}).cursorField(); got != "SystemModstamp" {
			t.Errorf("event %q gave cursor field %q, want SystemModstamp", ev, got)
		}
	}
}

func TestFiredSetRoundTripsThroughState(t *testing.T) {
	ids := []string{"001aaa", "001bbb"}
	raw, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	got := readFired(map[string]json.RawMessage{firedKey: raw})
	if len(got) != 2 || !got["001aaa"] || !got["001bbb"] {
		t.Errorf("fired set did not round-trip: %v", got)
	}
	// Absent or malformed state must read as "nothing fired yet", never nil-panic.
	if got := readFired(map[string]json.RawMessage{}); len(got) != 0 {
		t.Errorf("absent state should be empty, got %v", got)
	}
	if got := readFired(map[string]json.RawMessage{firedKey: json.RawMessage(`"not a list"`)}); len(got) != 0 {
		t.Errorf("malformed state should be empty, got %v", got)
	}
}

func TestCursorRoundTripsThroughState(t *testing.T) {
	raw, _ := json.Marshal("2026-07-22T12:31:12Z")
	got, ok := readCursor(map[string]json.RawMessage{cursorKey: raw})
	if !ok || !got.Equal(mustTime(t, "2026-07-22T12:31:12Z")) {
		t.Errorf("cursor did not round-trip: %v ok=%v", got, ok)
	}
	for _, bad := range []json.RawMessage{json.RawMessage(`""`), json.RawMessage(`"nonsense"`), json.RawMessage(`123`)} {
		if _, ok := readCursor(map[string]json.RawMessage{cursorKey: bad}); ok {
			t.Errorf("malformed cursor %s must read as absent", bad)
		}
	}
}

// Salesforce's error envelope is a JSON ARRAY, which is easy to get wrong: a
// .get() on the parsed body throws instead of reporting the error.
func TestFirstErrorMessageHandlesBothSalesforceShapes(t *testing.T) {
	arr := []byte(`[{"message":"Session expired or invalid","errorCode":"INVALID_SESSION_ID"}]`)
	if got := firstErrorMessage(arr); !strings.Contains(got, "INVALID_SESSION_ID") {
		t.Errorf("array envelope not read: %q", got)
	}
	obj := []byte(`{"error":"invalid_grant","error_description":"expired access/refresh token"}`)
	if got := firstErrorMessage(obj); !strings.Contains(got, "invalid_grant") {
		t.Errorf("object envelope not read: %q", got)
	}
	if got := firstErrorMessage([]byte("<html>502</html>")); got == "" {
		t.Error("an unrecognised body must still produce something loggable")
	}
}

// Dan asked whether baseline writes the fired-ID set as well as the watermark.
// It does — and this is the query behind it, which had no coverage.
//
// It matters because the watermark comparison is INCLUSIVE. Baseline stores the
// newest timestamp, so without also recording the ids already sitting at that
// exact second, the first real poll would re-fire every one of them. In this
// org that would be twelve Accounts on the very first run.
func TestIdsAtQueriesOneExactSecondAndExtractsTheIds(t *testing.T) {
	var gotSOQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSOQL = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"totalSize": 3, "done": true,
			"records": []map[string]interface{}{
				{"Id": "001aaa"}, {"Id": "001bbb"}, {"Id": "001ccc"},
			},
		})
	}))
	defer srv.Close()

	s := &Service{client: srv.Client()}
	stamp := mustTime(t, "2026-07-22T12:31:12Z")
	ids, err := s.idsAt(srv.URL, "tok", triggerConfig{Object: "Account"}, "SystemModstamp", stamp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// EQUALITY on the exact second — not >= — because this is asking "who is
	// already at the boundary", not "what is new".
	if !strings.Contains(gotSOQL, "SystemModstamp = 2026-07-22T12:31:12Z") {
		t.Errorf("must query the exact second: %q", gotSOQL)
	}
	if len(ids) != 3 || !ids["001aaa"] || !ids["001bbb"] || !ids["001ccc"] {
		t.Errorf("all ids at that second must be captured, got %v", ids)
	}
}

// The operator's filter has to apply here too. Without it, baseline would record
// ids that the poll query itself will never return, and the suppression set
// would be quietly wrong.
func TestIdsAtHonoursTheOperatorFilter(t *testing.T) {
	var gotSOQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSOQL = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"totalSize": 0, "done": true, "records": []map[string]interface{}{}})
	}))
	defer srv.Close()

	s := &Service{client: srv.Client()}
	cfg := triggerConfig{Object: "Lead", Where: "Status = 'Open'"}
	if _, err := s.idsAt(srv.URL, "tok", cfg, "CreatedDate", mustTime(t, "2026-07-22T12:31:12Z")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Asserted as two independent properties rather than one exact string, so a
	// change in how the builder spaces or parenthesises does not turn this into a
	// formatting test. What must hold is: the filter reached the query at all, and
	// it is grouped so an OR inside it cannot escape the surrounding AND.
	if !strings.Contains(gotSOQL, "Status = 'Open'") {
		t.Errorf("the operator's filter must reach the query: %q", gotSOQL)
	}
	if !strings.Contains(gotSOQL, "(Status = 'Open')") {
		t.Errorf("the filter must be grouped, or an OR inside it escapes the surrounding condition: %q", gotSOQL)
	}
}

// A failure here must be reported to the caller, not swallowed — baseline logs a
// warning and accepts that the first poll may re-fire, which is a decision it can
// only make if it is told.
func TestIdsAtReportsFailureRatherThanReturningAnEmptySet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{"message": "Session expired", "errorCode": "INVALID_SESSION_ID"},
		})
	}))
	defer srv.Close()

	s := &Service{client: srv.Client()}
	ids, err := s.idsAt(srv.URL, "tok", triggerConfig{Object: "Account"}, "SystemModstamp", mustTime(t, "2026-07-22T12:31:12Z"))
	if err == nil {
		t.Fatal("a failed lookup must report an error, or baseline cannot know the set is incomplete")
	}
	if ids != nil {
		t.Errorf("a failed lookup must not return a partial set that would suppress real records: %v", ids)
	}
}
