package persistence

import (
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// TestMigrationsSourceLoads is a smoke test for the migration set. It loads the
// embedded migrations exactly as CheckAndUpdate does on service startup — and
// exactly at the point that fails first — before any database is touched.
//
// iofs.New rejects a source with a duplicate version number or a malformed
// filename. That is the class of mistake that otherwise never surfaces in a unit
// test (they use sqlmock, not the real source) and only shows up as a service
// that crash-loops on boot the moment it is deployed.
//
// A duplicate migration version took the sibling api service down in production
// on 2026-07-13. launch was not hit that time only because its numbering happened
// to be unaffected — the same trap applies here. This test costs nothing (no
// database, no service) because the failure is in the file set, not the SQL.
func TestMigrationsSourceLoads(t *testing.T) {
	if _, err := iofs.New(migrations, "migration"); err != nil {
		t.Fatalf("embedded migration source failed to load — most likely a duplicate version number or a malformed filename in internal/persistence/migration/: %v", err)
	}
}
