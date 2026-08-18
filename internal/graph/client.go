// Package graph owns every interaction with the Neo4j derived projection.
//
// PostgreSQL is the source of truth; Neo4j is a projection that can be thrown
// away and rebuilt at any time (see the Part B plan, "Global Constraints").
// Two invariants shape this package:
//
//  1. The projection worker is the ONLY writer. Read paths go through
//     Client.Read, which opens a read-mode session — a Cypher write clause
//     reaching a read session is rejected by the server, so "the read API
//     cannot mutate the graph" is enforced by the transport, not by review.
//  2. Relationship types and node labels cannot be parameterised in Cypher.
//     Every literal that ends up concatenated into a query string comes from
//     a code-level whitelist (reltypes.go), never from a database value.
package graph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

const (
	// defaultGraphTimeout bounds connection acquisition and the initial
	// connectivity probe.
	defaultGraphTimeout = 10 * time.Second

	// defaultMaxPoolSize is deliberately small: one serial projection worker
	// plus a handful of concurrent read requests.
	defaultMaxPoolSize = 10

	// defaultTxTimeout is the server-side transaction timeout applied to every
	// managed transaction. A read query that cannot finish in 5s on a graph of
	// this size is a bug, and letting it run only holds a connection hostage.
	defaultTxTimeout = 5 * time.Second
)

// Config holds the connection settings for the Neo4j projection store.
type Config struct {
	URI         string
	Username    string
	Password    string
	MaxPoolSize int
	Timeout     time.Duration
}

// Client owns one driver for the whole process. Sessions are short-lived and
// never shared between goroutines (a neo4j session is not safe for concurrent
// use); the driver itself is.
type Client struct {
	driver  neo4j.DriverWithContext
	timeout time.Duration
}

// withDefaults fills in the optional fields. Exported behaviour depends on it
// only through New, but it is unit-tested directly because these fallbacks are
// what keep a half-configured deployment bounded.
func withDefaults(cfg Config) Config {
	if cfg.Username == "" {
		cfg.Username = "neo4j"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultGraphTimeout
	}
	if cfg.MaxPoolSize <= 0 {
		cfg.MaxPoolSize = defaultMaxPoolSize
	}
	return cfg
}

// New validates cfg, constructs the driver and verifies connectivity once.
// Verification happens here on purpose: callers (collector worker, server
// wiring) treat a failure as "graph disabled for this process" and must learn
// about it at startup rather than on the first tick.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.URI == "" {
		return nil, errors.New("graph: URI must not be empty")
	}
	if cfg.Password == "" {
		return nil, errors.New("graph: password must not be empty")
	}
	cfg = withDefaults(cfg)

	d, err := neo4j.NewDriverWithContext(
		cfg.URI,
		neo4j.BasicAuth(cfg.Username, cfg.Password, ""),
		func(c *neo4j.Config) {
			c.MaxConnectionPoolSize = cfg.MaxPoolSize
			c.ConnectionAcquisitionTimeout = cfg.Timeout
		},
	)
	if err != nil {
		return nil, fmt.Errorf("graph: driver: %w", err)
	}

	vctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := d.VerifyConnectivity(vctx); err != nil {
		// Close the driver we just built — returning an error while leaving a
		// live connection pool behind would leak goroutines for the lifetime
		// of the process.
		_ = d.Close(ctx)
		return nil, fmt.Errorf("graph: connect: %w", err)
	}
	return &Client{driver: d, timeout: cfg.Timeout}, nil
}

// Close releases the driver's connection pool.
func (c *Client) Close(ctx context.Context) error {
	if c == nil || c.driver == nil {
		return nil
	}
	if err := c.driver.Close(ctx); err != nil {
		return fmt.Errorf("graph: close: %w", err)
	}
	return nil
}

// Read runs work inside a managed READ transaction. Read never opens a
// write-mode session: that is one of the defence lines behind "the read API
// cannot change the graph".
func (c *Client) Read(ctx context.Context, work func(tx neo4j.ManagedTransaction) (any, error)) (any, error) {
	session := c.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() { _ = session.Close(ctx) }()

	out, err := session.ExecuteRead(ctx, work, neo4j.WithTxTimeout(defaultTxTimeout))
	if err != nil {
		return nil, fmt.Errorf("graph: read tx: %w", err)
	}
	return out, nil
}

// Write runs work inside a managed WRITE transaction. Only the projection
// worker (and schema bootstrap) may call this.
func (c *Client) Write(ctx context.Context, work func(tx neo4j.ManagedTransaction) (any, error)) (any, error) {
	session := c.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = session.Close(ctx) }()

	out, err := session.ExecuteWrite(ctx, work, neo4j.WithTxTimeout(defaultTxTimeout))
	if err != nil {
		return nil, fmt.Errorf("graph: write tx: %w", err)
	}
	return out, nil
}
