package evalcompare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// Slices is the versioned, query-text-free slice-label file described in
// issue #269: golden query IDs (golden_queries.id, never the question text
// itself) mapped to human-reviewed tags such as "person", "time",
// "exact_token", "pronoun". It is designed to live outside this repository,
// next to the dumps it labels — LoadSlices only reads whatever path the
// caller points at, and nothing in this package writes one.
type Slices struct {
	Schema  int                 `json:"schema"`
	Version string              `json:"version"`
	Tags    map[string][]string `json:"tags"`

	sha256 string
}

// SHA256 is the hex digest of the raw file bytes LoadSlices read. It lets a
// Report cite exactly which slice-label version produced a given tag
// grouping without embedding the file's content.
func (s *Slices) SHA256() string { return s.sha256 }

// LoadSlices reads and validates a slice-label file. schema must be 1 (the
// only version this package understands); a future incompatible schema
// change should bump this and add explicit migration, not silently
// misinterpret an old file's tags.
func LoadSlices(path string) (*Slices, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("evalcompare: read slices file %s: %w", path, err)
	}
	var s Slices
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("evalcompare: parse slices file %s: %w", path, err)
	}
	if s.Schema != 1 {
		return nil, fmt.Errorf("evalcompare: slices file %s has schema %d, want 1", path, s.Schema)
	}
	sum := sha256.Sum256(raw)
	s.sha256 = hex.EncodeToString(sum[:])
	return &s, nil
}
