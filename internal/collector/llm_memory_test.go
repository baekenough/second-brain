package collector

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLLMMemoryCollector_Enabled verifies that Enabled() returns true when a
// non-empty dbPath is configured and false when it is empty.
func TestLLMMemoryCollector_Enabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		dbPath  string
		enabled bool
	}{
		{"non-empty path", "/data/llm-memory.sqlite", true},
		{"empty path", "", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewLLMMemoryCollector(tc.dbPath)
			if c.Enabled() != tc.enabled {
				t.Errorf("Enabled()=%v, want %v (dbPath=%q)", c.Enabled(), tc.enabled, tc.dbPath)
			}
		})
	}
}

// TestLLMMemoryCollector_MissingDB_GracefulSkip verifies that Collect returns
// an empty (nil) document slice without error when the configured SQLite file
// does not exist. This prevents "open db" error spam when the volume is not
// mounted (issue #156).
func TestLLMMemoryCollector_MissingDB_GracefulSkip(t *testing.T) {
	t.Parallel()

	// Use a path that is guaranteed not to exist.
	c := NewLLMMemoryCollector("/tmp/second-brain-test-does-not-exist-llm-memory.sqlite")

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Errorf("Collect on missing db returned error %v; want nil (graceful skip)", err)
	}
	if len(docs) != 0 {
		t.Errorf("Collect on missing db returned %d docs; want 0", len(docs))
	}
}

// TestLLMMemoryCollector_MissingDB_LogOnce verifies that the missing-db
// message is emitted only once across multiple Collect calls. Subsequent calls
// must be silent to prevent log spam.
//
// The test does not capture slog output directly (that would require replacing
// the global logger); instead it asserts the internal missingLogged counter
// state — a whitebox check that is acceptable because the test is in the same
// package (package collector).
func TestLLMMemoryCollector_MissingDB_LogOnce(t *testing.T) {
	t.Parallel()

	c := NewLLMMemoryCollector("/tmp/second-brain-test-no-such-file-llm-memory.sqlite")

	// First call: should log and set missingLogged to 1.
	_, _ = c.Collect(context.Background(), time.Time{})
	if got := c.missingLogged.Load(); got != 1 {
		t.Errorf("after first Collect on missing db: missingLogged=%d, want 1", got)
	}

	// Second call: missingLogged must still be 1 (not incremented).
	_, _ = c.Collect(context.Background(), time.Time{})
	if got := c.missingLogged.Load(); got != 1 {
		t.Errorf("after second Collect on missing db: missingLogged=%d, want 1 (no spam)", got)
	}
}

// TestLLMMemoryCollector_Name verifies the collector's display name.
func TestLLMMemoryCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewLLMMemoryCollector("/some/path")
	if c.Name() != "llm-memory" {
		t.Errorf("Name()=%q, want %q", c.Name(), "llm-memory")
	}
}

// TestSQLiteDriverRegistered verifies that the modernc.org/sqlite driver is
// registered and usable via sql.Open("sqlite", ...). This is a regression test
// for the #151 regression where secretary.go (the sole registration point) was
// removed without preserving the blank import, causing "sql: unknown driver
// sqlite" panics in production.
//
// The test creates a real in-process SQLite database, which is safe to run in
// CI because modernc.org/sqlite is a CGO-free pure-Go implementation.
func TestSQLiteDriverRegistered(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "driver_test.sqlite")

	// sql.Open itself does not fail for unknown drivers — the error surfaces on
	// the first database operation (Ping or Query). Use a writable URI here
	// (no mode=ro) so that the driver can create the file.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(\"sqlite\", ...): %v — driver may not be registered", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping() failed: %v — driver may not be registered (regression: #151)", err)
	}

	// Verify a round-trip to confirm the driver is fully functional.
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("CREATE TABLE failed: %v", err)
	}

	// Verify the db file was created by the CGO-free driver.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file not found after Ping: %v", err)
	}
}
