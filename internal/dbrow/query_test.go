package dbrow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidIdentifier(t *testing.T) {
	valid := []string{"id", "created_at", "_hidden", "Col1", "a1_b2"}
	for _, s := range valid {
		if !validIdentifier(s) {
			t.Errorf("expected %q to be a valid identifier", s)
		}
	}
	// Anything that could break out of an identifier must be rejected — this is
	// the injection guard, since identifiers can't be bound as parameters.
	invalid := []string{"", "1col", "a b", "a;b", "a-b", "a.b", `a"b`, "a`b", "a)b", "a'b", "drop table", "id--", "*"}
	for _, s := range invalid {
		if validIdentifier(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestValidTableName(t *testing.T) {
	valid := []string{"orders", "public.orders", "myschema.my_table"}
	for _, s := range valid {
		if !validTableName(s) {
			t.Errorf("expected %q to be a valid table name", s)
		}
	}
	invalid := []string{"", "a.b.c", "orders; drop table users", "public..orders", ".orders", "orders.", "public.orders--"}
	for _, s := range invalid {
		if validTableName(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestQuoteIdentifier(t *testing.T) {
	cases := []struct {
		dialect, ident, want string
	}{
		{dialectPostgres, "orders", `"orders"`},
		{dialectPostgres, "public.orders", `"public"."orders"`},
		{dialectMySQL, "orders", "`orders`"},
		{dialectMySQL, "public.orders", "`public`.`orders`"},
		{dialectSQLServer, "orders", "[orders]"},
		{dialectSQLServer, "public.orders", "[public].[orders]"},
	}
	for _, c := range cases {
		if got := quoteIdentifier(c.dialect, c.ident); got != c.want {
			t.Errorf("quoteIdentifier(%q, %q) = %q, want %q", c.dialect, c.ident, got, c.want)
		}
	}
}

func TestPlaceholder(t *testing.T) {
	cases := []struct {
		dialect string
		n       int
		want    string
	}{
		{dialectPostgres, 1, "$1"},
		{dialectPostgres, 2, "$2"},
		{dialectMySQL, 1, "?"},
		{dialectMySQL, 5, "?"},
		{dialectSQLServer, 1, "@p1"},
		{dialectSQLServer, 3, "@p3"},
	}
	for _, c := range cases {
		if got := placeholder(c.dialect, c.n); got != c.want {
			t.Errorf("placeholder(%q, %d) = %q, want %q", c.dialect, c.n, got, c.want)
		}
	}
}

func TestBuildDSN(t *testing.T) {
	// Postgres, verify maps to sslmode=verify-full and creds are URL-escaped.
	drv, dsn, err := buildDSN(dialectPostgres, "db.local", "5432", "us er", "p@ss", "app", "verify")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drv != "postgres" {
		t.Errorf("driver = %q, want postgres", drv)
	}
	if !strings.Contains(dsn, "sslmode=verify-full") {
		t.Errorf("expected verify-full in %q", dsn)
	}
	if !strings.Contains(dsn, "us+er") || !strings.Contains(dsn, "p%40ss") {
		t.Errorf("expected escaped credentials in %q", dsn)
	}

	// MySQL, require maps to tls=skip-verify and parseTime is on.
	_, dsn, err = buildDSN(dialectMySQL, "db.local", "3306", "root", "secret", "app", "require")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(dsn, "tls=skip-verify") || !strings.Contains(dsn, "parseTime=true") {
		t.Errorf("unexpected mysql dsn %q", dsn)
	}
	if !strings.HasPrefix(dsn, "root:secret@tcp(db.local:3306)/app") {
		t.Errorf("unexpected mysql dsn prefix %q", dsn)
	}

	// SQL Server, disable maps to encrypt=disable.
	drv, dsn, err = buildDSN(dialectSQLServer, "db.local", "1433", "sa", "secret", "app", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drv != "sqlserver" {
		t.Errorf("driver = %q, want sqlserver", drv)
	}
	if !strings.Contains(dsn, "encrypt=disable") || !strings.Contains(dsn, "database=app") {
		t.Errorf("unexpected sqlserver dsn %q", dsn)
	}

	// Unknown dialect and missing fields error out.
	if _, _, err := buildDSN("db2", "h", "1", "u", "p", "d", ""); err == nil {
		t.Error("expected error for unknown dialect")
	}
	if _, _, err := buildDSN(dialectPostgres, "", "5432", "u", "p", "d", ""); err == nil {
		t.Error("expected error for missing host")
	}
}

func TestBuildMaxQuery(t *testing.T) {
	if got := buildMaxQuery(dialectPostgres, "orders", "id", ""); got != `SELECT MAX("id") FROM "orders"` {
		t.Errorf("unexpected max query: %q", got)
	}
	got := buildMaxQuery(dialectMySQL, "public.orders", "id", "status")
	want := "SELECT MAX(`id`) FROM `public`.`orders` WHERE `status` = ?"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildSelectQuery(t *testing.T) {
	// Postgres with cursor + filter: $1 then $2.
	got := buildSelectQuery(dialectPostgres, "orders", "id", "status", true, 500)
	want := `SELECT * FROM "orders" WHERE "id" > $1 AND "status" = $2 ORDER BY "id" ASC LIMIT 500`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}

	// No cursor (empty baseline), filter becomes $1.
	got = buildSelectQuery(dialectPostgres, "orders", "id", "status", false, 10)
	want = `SELECT * FROM "orders" WHERE "status" = $1 ORDER BY "id" ASC LIMIT 10`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}

	// No cursor, no filter: unfiltered.
	got = buildSelectQuery(dialectMySQL, "orders", "id", "", false, 5)
	want = "SELECT * FROM `orders` ORDER BY `id` ASC LIMIT 5"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}

	// SQL Server uses TOP, no LIMIT.
	got = buildSelectQuery(dialectSQLServer, "orders", "id", "", true, 500)
	want = "SELECT TOP (500) * FROM [orders] WHERE [id] > @p1 ORDER BY [id] ASC"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestBuildArgs(t *testing.T) {
	if got := buildArgs(true, "42", "status", "paid"); len(got) != 2 || got[0] != "42" || got[1] != "paid" {
		t.Errorf("unexpected args %v", got)
	}
	if got := buildArgs(false, "42", "status", "paid"); len(got) != 1 || got[0] != "paid" {
		t.Errorf("unexpected args %v", got)
	}
	if got := buildArgs(true, "42", "", ""); len(got) != 1 || got[0] != "42" {
		t.Errorf("unexpected args %v", got)
	}
	if got := buildArgs(false, "", "", ""); len(got) != 0 {
		t.Errorf("expected no args, got %v", got)
	}
}

func TestNormaliseValue(t *testing.T) {
	if got := normaliseValue([]byte("hello")); got != "hello" {
		t.Errorf("byte slice: got %v", got)
	}
	ts := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	if got := normaliseValue(ts); got != "2026-07-16T10:00:00Z" {
		t.Errorf("time: got %v", got)
	}
	if got := normaliseValue(int64(42)); got != int64(42) {
		t.Errorf("int passthrough: got %v", got)
	}
	if got := normaliseValue(nil); got != nil {
		t.Errorf("nil passthrough: got %v", got)
	}
}

func TestCursorToString(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},
		{"abc", "abc"},
		{[]byte("42"), "42"},
		{int64(42), "42"},
		{float64(3.5), "3.5"},
		{true, "true"},
		{time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC), "2026-07-16T10:00:00Z"},
	}
	for _, c := range cases {
		if got := cursorToString(c.in); got != c.want {
			t.Errorf("cursorToString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReadCursor(t *testing.T) {
	// Present, non-empty.
	state := map[string]json.RawMessage{cursorKey: json.RawMessage(`"100"`)}
	if v, ok := readCursor(state); !ok || v != "100" {
		t.Errorf("expected (100,true), got (%q,%v)", v, ok)
	}
	// Absent → empty baseline.
	if _, ok := readCursor(map[string]json.RawMessage{}); ok {
		t.Error("expected ok=false when cursor absent")
	}
	// Present but empty string → treated as absent.
	if _, ok := readCursor(map[string]json.RawMessage{cursorKey: json.RawMessage(`""`)}); ok {
		t.Error("expected ok=false for empty cursor")
	}
}

func TestParseInterval(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultPollInterval},
		{"garbage", DefaultPollInterval},
		{"0s", DefaultPollInterval},
		{"1s", MinPollInterval}, // clamped up
		{"5m", 5 * time.Minute},
		{"90s", 90 * time.Second},
	}
	for _, c := range cases {
		if got := parseInterval(c.in); got != c.want {
			t.Errorf("parseInterval(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestShouldSkipForInterval(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	mk := func(offset time.Duration) map[string]json.RawMessage {
		ts, _ := json.Marshal(now.Add(offset).Format(time.RFC3339Nano))
		return map[string]json.RawMessage{lastPolledKey: ts}
	}

	// Polled 30s ago, interval 5m → skip.
	if !shouldSkipForInterval(mk(-30*time.Second), 5*time.Minute, now) {
		t.Error("expected skip when within interval")
	}
	// Polled 6m ago, interval 5m → poll.
	if shouldSkipForInterval(mk(-6*time.Minute), 5*time.Minute, now) {
		t.Error("expected no skip when interval elapsed")
	}
	// Never polled → poll.
	if shouldSkipForInterval(map[string]json.RawMessage{}, 5*time.Minute, now) {
		t.Error("expected no skip when never polled")
	}
}
