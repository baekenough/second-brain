package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// All real-database tests use production migrations, never a partial schema or
// extension stub. TEST_DATABASE_URL must name a disposable test database.
// Missing configuration skips integration tests; broken configuration fails.
func TestMain(m *testing.M) {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		pg, err := NewPostgres(ctx, dsn)
		if err == nil {
			err = pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536)
			pg.Close()
		}
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bootstrap store test database: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// Match the production migration dimension in every vector lane.
func testEmbedding() []float32 {
	embedding := make([]float32, 1536)
	copy(embedding, []float32{0.1, 0.2, 0.3})
	return embedding
}
