package store

import (
	"context"
	"fmt"

	"github.com/baekenough/second-brain/internal/model"
)

// TranscribedSourceIDSet returns the set of source_ids that the whisper pipeline
// has already transcribed for the given source type, as recorded in the
// transcription_ledger table.
//
// The ledger is the durable counterpart to the active document index: a
// source_id is present here as soon as it has been successfully transcribed,
// regardless of whether the resulting document was stored or rejected as a
// duplicate (ErrDuplicateTranscript). The whisper collector unions this set with
// the active document index to decide which immutable audio files must NOT be
// re-transcribed, eliminating the infinite re-transcription loop.
//
// The returned map is keyed by source_id and is safe for O(1) membership tests.
// On no rows an empty (non-nil) set is returned.
func (s *DocumentStore) TranscribedSourceIDSet(ctx context.Context, sourceType model.SourceType) (map[string]struct{}, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT source_id FROM transcription_ledger
		WHERE source_type = $1`,
		sourceType,
	)
	if err != nil {
		return nil, fmt.Errorf("transcribed source ID set for %s: %w", sourceType, err)
	}
	defer rows.Close()

	set := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		set[id] = struct{}{}
	}
	return set, rows.Err()
}

// RecordTranscribed records that each source_id in sourceIDs has been
// transcribed for the given source type. Existing rows are left untouched
// (ON CONFLICT DO NOTHING), so re-recording an already-ledgered source_id is a
// no-op and the original transcribed_at timestamp is preserved.
//
// The insert is a single round-trip: source_ids are passed as an array and
// expanded with unnest, avoiding one query per id. An empty slice is a no-op.
func (s *DocumentStore) RecordTranscribed(ctx context.Context, sourceType model.SourceType, sourceIDs []string) error {
	if len(sourceIDs) == 0 {
		return nil
	}

	_, err := s.pg.pool.Exec(ctx, `
		INSERT INTO transcription_ledger (source_type, source_id)
		SELECT $1, unnest($2::text[])
		ON CONFLICT (source_type, source_id) DO NOTHING`,
		sourceType, sourceIDs,
	)
	if err != nil {
		return fmt.Errorf("record transcribed for %s (%d ids): %w", sourceType, len(sourceIDs), err)
	}
	return nil
}
