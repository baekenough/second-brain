// Package note contains the shared logic for persisting a user- or
// agent-authored note into the second-brain knowledge base. It is used by
// both the MCP add_note tool (cmd/mcp/main.go) and the POST /api/v1/notes
// REST handler (internal/api/notes.go), so a change here affects both paths
// identically — behaviour must not silently diverge between them.
package note

import "errors"

// MaxContentBytes is the upper bound for note content (10 MiB). Unchanged
// from the pre-extraction MCP add_note limit.
const MaxContentBytes = 10 * 1024 * 1024

// MaxTitleBytes is the upper bound for note title (1 KiB). Unchanged from
// the pre-extraction MCP add_note limit.
const MaxTitleBytes = 1024

// MaxEmbedChunks is the upper bound for the number of chunks embedded in a
// single Save call. Notes that produce more chunks are stored and indexed
// via FTS, but embedding is skipped to avoid unbounded API cost and
// long-running DB transactions.
const MaxEmbedChunks = 2000

// ErrEmbedSkipped is returned by embedChunks when the chunk count exceeds
// MaxEmbedChunks. It is a sentinel that Save treats as a deliberate skip
// (non-fatal, non-success) rather than an actual embedding failure. Callers
// MUST NOT surface this as a user-facing error.
var ErrEmbedSkipped = errors.New("embedding skipped: chunk count exceeds limit")
