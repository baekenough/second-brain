package graph

import (
	"context"
	"os"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// newTestClient returns a Client pointed at a throwaway Neo4j, or skips.
//
// CI runs without Neo4j, and the projection is a derived store, so these tests
// are opt-in: set NEO4J_TEST_URI (and NEO4J_TEST_PASSWORD) to run them. They
// are destructive — they wipe the target database — so they must never be
// pointed at anything but a scratch instance.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	uri := os.Getenv("NEO4J_TEST_URI")
	if uri == "" {
		t.Skip("NEO4J_TEST_URI not set — skipping Neo4j integration test")
	}
	c, err := New(context.Background(), Config{
		URI: uri,
		// Both spellings are accepted: a mismatch here fails open (falls back to
		// "neo4j") instead of erroring, so the wrong variable name would be
		// invisible until the day the scratch instance uses a different user.
		Username: envOrDefault("NEO4J_TEST_USERNAME", envOrDefault("NEO4J_TEST_USER", "neo4j")),
		Password: os.Getenv("NEO4J_TEST_PASSWORD"),
	})
	if err != nil {
		t.Fatalf("connect to test neo4j: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestEnsureSchemaIntegration pins idempotency (the worker calls EnsureSchema
// on every process start) and the constraint count the MERGE safety argument
// depends on.
func TestEnsureSchemaIntegration(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := c.EnsureSchema(ctx); err != nil {
			t.Fatalf("EnsureSchema call %d: %v", i+1, err)
		}
	}

	out, err := c.Read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, `SHOW CONSTRAINTS YIELD name RETURN count(*) AS n`, nil)
		if err != nil {
			return nil, err
		}
		rec, err := res.Single(ctx)
		if err != nil {
			return nil, err
		}
		n, _ := rec.Get("n")
		return n, nil
	})
	if err != nil {
		t.Fatalf("SHOW CONSTRAINTS: %v", err)
	}
	if got, ok := out.(int64); !ok || got < 3 {
		t.Fatalf("constraint count = %v, want at least 3", out)
	}
}
