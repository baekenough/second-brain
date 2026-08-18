package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// schemaStatements are the constraints and indexes the projection depends on.
//
// The three uniqueness constraints are the safety net under every MERGE: a
// MERGE on a property with no uniqueness constraint can create duplicate nodes
// under concurrency. They also provide the backing index, which is why no
// separate index on pgId is declared.
//
// Neo4j Community does not support relationship uniqueness/existence
// constraints (Enterprise only), so relationship de-duplication is guaranteed
// structurally instead: the projection worker is the single serial writer and
// every relationship MERGE keys on pgId (= entity_relations.id).
var schemaStatements = []string{
	`CREATE CONSTRAINT entity_pg_id IF NOT EXISTS FOR (e:Entity) REQUIRE e.pgId IS UNIQUE`,
	`CREATE CONSTRAINT document_pg_id IF NOT EXISTS FOR (d:Document) REQUIRE d.pgId IS UNIQUE`,
	`CREATE CONSTRAINT projection_state_id IF NOT EXISTS FOR (p:ProjectionState) REQUIRE p.id IS UNIQUE`,
	`CREATE INDEX entity_normalized_name IF NOT EXISTS FOR (e:Entity) ON (e.normalizedName)`,
}

// EnsureSchema applies the DDL above. Every statement is IF NOT EXISTS, so
// repeated calls are safe — the worker calls it once per process on its first
// successful tick.
//
// One transaction per statement is required, not stylistic: Neo4j refuses to
// mix schema and data operations inside a single transaction, and batching
// several schema commands together is also rejected on some versions.
func (c *Client) EnsureSchema(ctx context.Context) error {
	for _, stmt := range schemaStatements {
		if _, err := c.Write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			res, err := tx.Run(ctx, stmt, nil)
			if err != nil {
				return nil, err
			}
			// Consume, not just Run: server-side failures for a statement that
			// streams no rows are only surfaced when the result is drained.
			return res.Consume(ctx)
		}); err != nil {
			return fmt.Errorf("graph: ensure schema: %w", err)
		}
	}
	return nil
}
