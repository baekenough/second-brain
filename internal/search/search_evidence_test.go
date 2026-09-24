package search

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// --- #267: chunk-lane Evidence survives RRF fusion ---

// TestMergeRRFMode_OverlapKeepsPrimaryContentGainsEvidence reproduces the
// exact bug #267 fixes: a document's answer lives in a mid/late chunk that
// only the chunk-vector lane matched, while the document-lane primary (the
// document store's own hybrid fusion) also returns the same document —
// ranked on unrelated signals — with its FULL body as Content. Before this
// change, mergeRRFMode's overlap branch discarded the secondary's chunk
// entirely (`e.score += rrf; continue`); the matched passage never reached
// /ask's excerpt selection.
func TestMergeRRFMode_OverlapKeepsPrimaryContentGainsEvidence(t *testing.T) {
	t.Parallel()

	docID := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	fullBody := "문서 앞부분에는 답이 없다. ... 문서 중반부에 예약번호 ZX900 이 적혀 있다."

	primary := []*model.SearchResult{{
		Document:  model.Document{ID: docID, Title: "doc", Content: fullBody},
		Score:     0.9,
		MatchType: "hybrid",
	}}
	secondary := []*model.SearchResult{chunkVecToSearchResult(makeChunkResult(docID, 3, 0.7, "doc"))}

	got := mergeRRFMode(primary, secondary, 10, model.MergeAsymmetric)
	if len(got) != 1 {
		t.Fatalf("want 1 merged result, got %d", len(got))
	}
	merged := got[0]

	if merged.Content != fullBody {
		t.Fatalf("Content = %q, want primary's full body unchanged (never replaced by chunk text)", merged.Content)
	}
	if merged.MatchType != "hybrid" {
		t.Errorf("MatchType = %q, want primary's own (%q) preserved", merged.MatchType, "hybrid")
	}
	if len(merged.Evidence) != 1 {
		t.Fatalf("Evidence = %+v, want exactly 1 entry carried over from the chunk-vector secondary", merged.Evidence)
	}
	if merged.Evidence[0].ChunkID != secondary[0].Evidence[0].ChunkID {
		t.Errorf("Evidence[0].ChunkID = %d, want %d", merged.Evidence[0].ChunkID, secondary[0].Evidence[0].ChunkID)
	}
	if merged.Evidence[0].Lane != model.MatchTypeChunkVector {
		t.Errorf("Evidence[0].Lane = %q, want %q", merged.Evidence[0].Lane, model.MatchTypeChunkVector)
	}
}

// TestMergeRRFMode_NoOverlapChunkOnlyResultKeepsOwnEvidence verifies a
// chunk-lane hit admitted as a brand-new (non-overlapping) result keeps its
// own single-entry Evidence — this is the "chunk_only" case /ask's
// isChunkOnlyResult relies on: Content already IS the matched chunk's text.
func TestMergeRRFMode_NoOverlapChunkOnlyResultKeepsOwnEvidence(t *testing.T) {
	t.Parallel()

	docID := uuid.MustParse("00000000-0000-0000-0000-0000000000bb")
	secondary := []*model.SearchResult{chunkVecToSearchResult(makeChunkResult(docID, 0, 0.6, "doc"))}

	got := mergeRRFMode(nil, secondary, 10, model.MergeAsymmetric)
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	if len(got[0].Evidence) != 1 {
		t.Fatalf("Evidence = %+v, want the chunk-only result's own single entry", got[0].Evidence)
	}
	if got[0].Content != got[0].Evidence[0].Text {
		t.Errorf("Content = %q, want equal to Evidence[0].Text = %q (chunk-only result)", got[0].Content, got[0].Evidence[0].Text)
	}
}

// TestMergeRRFMode_EvidenceCapPerDocument verifies appendEvidenceCapped
// truncates to maxEvidencePerResult, keeping the highest-scoring entries —
// exercised directly since production rarely accumulates more than one
// overlap per mergeRRFMode call (see appendEvidenceCapped's doc comment).
func TestMergeRRFMode_EvidenceCapPerDocument(t *testing.T) {
	t.Parallel()

	existing := []model.MatchedEvidence{
		{ChunkID: 1, Score: 0.5},
		{ChunkID: 2, Score: 0.9},
	}
	added := []model.MatchedEvidence{
		{ChunkID: 3, Score: 0.7},
		{ChunkID: 4, Score: 0.95},
	}

	got := appendEvidenceCapped(existing, added)
	if len(got) != maxEvidencePerResult {
		t.Fatalf("len(got) = %d, want %d", len(got), maxEvidencePerResult)
	}
	wantIDs := map[int64]bool{2: true, 4: true, 3: true} // top 3 by score: 4(.95) 2(.9) 3(.7)
	for _, e := range got {
		if !wantIDs[e.ChunkID] {
			t.Errorf("unexpected ChunkID %d survived the cap: %+v", e.ChunkID, got)
		}
	}
	if got[0].ChunkID != 4 || got[0].Score != 0.95 {
		t.Errorf("got[0] = %+v, want highest-scoring entry (ChunkID=4, Score=0.95) first", got[0])
	}
}

// TestMergeRRFMode_DoesNotMutateCallerSlices guards the clone-before-append
// discipline appendEvidenceCapped documents: mutating a merged result's
// Evidence must never corrupt a DIFFERENT SearchResult that happens to share
// the same backing array via an earlier shallow copy (mergeRRFMode's own
// primary-population loop, and applyRerank/applyLowRetentionPenalty
// elsewhere in this package, both do `cp := *r`, which copies the Evidence
// slice HEADER but not its backing array).
func TestMergeRRFMode_DoesNotMutateCallerSlices(t *testing.T) {
	t.Parallel()

	docA := uuid.MustParse("00000000-0000-0000-0000-0000000000cc")
	docB := uuid.MustParse("00000000-0000-0000-0000-0000000000dd")

	// Two distinct SearchResult pointers deliberately share one backing
	// array for their Evidence slices — the exact shape a careless
	// `append` on one merged entry could corrupt.
	sharedBacking := make([]model.MatchedEvidence, 2, 4)
	sharedBacking[0] = model.MatchedEvidence{ChunkID: 10, Score: 0.5}
	sharedBacking[1] = model.MatchedEvidence{ChunkID: 11, Score: 0.5}

	primary := []*model.SearchResult{
		{Document: model.Document{ID: docA, Content: "a"}, MatchType: "hybrid", Evidence: sharedBacking[:1]},
		{Document: model.Document{ID: docB, Content: "b"}, MatchType: "hybrid", Evidence: sharedBacking[1:2]},
	}
	secondary := []*model.SearchResult{
		chunkVecToSearchResult(makeChunkResult(docA, 0, 0.6, "a")),
	}

	_ = mergeRRFMode(primary, secondary, 10, model.MergeAsymmetric)

	// docB's caller-owned Evidence entry must be untouched: its ChunkID
	// must still read 11, not have been silently overwritten by docA's
	// append reusing spare capacity in the shared backing array.
	if sharedBacking[1].ChunkID != 11 {
		t.Fatalf("caller's Evidence backing array was mutated: sharedBacking[1] = %+v, want ChunkID=11", sharedBacking[1])
	}
}

// TestMatchedEvidence_JSONOmitEmpty verifies SearchResult's JSON shape is
// byte-for-byte unchanged when Evidence is empty — the additive-field
// compatibility guarantee deep-plan #268's risk table calls out: HTTP/MCP
// consumers of model.SearchResult must not see a new key appear.
func TestMatchedEvidence_JSONOmitEmpty(t *testing.T) {
	t.Parallel()

	r := model.SearchResult{
		Document:  model.Document{ID: uuid.MustParse("00000000-0000-0000-0000-0000000000ee"), Content: "x"},
		Score:     1,
		MatchType: "hybrid",
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["evidence"]; ok {
		t.Fatalf("evidence key present with empty Evidence: %s", out)
	}

	r.Evidence = []model.MatchedEvidence{{ChunkID: 1, ChunkIndex: 0, Lane: model.MatchTypeChunkVector, Score: 0.5, Text: "should never serialise"}}
	out, err = json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal with evidence: %v", err)
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal with evidence: %v", err)
	}
	ev, ok := raw["evidence"].([]any)
	if !ok || len(ev) != 1 {
		t.Fatalf("evidence = %v, want 1-element array", raw["evidence"])
	}
	entry, _ := ev[0].(map[string]any)
	if _, ok := entry["Text"]; ok {
		t.Fatalf("MatchedEvidence.Text leaked into JSON: %s", out)
	}
	if _, ok := entry["chunk_id"]; !ok {
		t.Fatalf("chunk_id missing from serialised evidence: %s", out)
	}
}

// --- rerank input invariance (deep-plan #267 completion gate) ---

// TestBuildRerankDocs_EvidenceDoesNotChangeInput verifies that populating
// Evidence never changes buildRerankDocs' output — Evidence must never
// become a silent second writer of the reranker's input, in either
// RerankInput mode. This guards the golden-eval ndcg baseline: rerank input
// is what buildRerankDocs has always produced from Title/Content (or, in
// best_chunk mode, an isChunkResult document's own Content) — see
// tuning_knobs.go, which #267 does not touch.
func TestBuildRerankDocs_EvidenceDoesNotChangeInput(t *testing.T) {
	t.Parallel()

	docID := uuid.MustParse("00000000-0000-0000-0000-0000000000ff")
	withoutEvidence := &model.SearchResult{
		Document:  model.Document{ID: docID, Title: "제목", Content: "본문"},
		MatchType: model.MatchTypeChunkVector,
	}
	withEvidence := &model.SearchResult{
		Document:  model.Document{ID: docID, Title: "제목", Content: "본문"},
		MatchType: model.MatchTypeChunkVector,
		Evidence:  []model.MatchedEvidence{{ChunkID: 1, Lane: model.MatchTypeChunkVector, Text: "본문"}},
	}

	svc := &Service{}
	ctx := context.Background()

	for _, tune := range []model.SearchTuning{
		{RerankInput: model.RerankInputHead},
		{RerankInput: model.RerankInputBestChunk},
	} {
		got1 := svc.buildRerankDocs(ctx, "query", []*model.SearchResult{withoutEvidence}, tune)
		got2 := svc.buildRerankDocs(ctx, "query", []*model.SearchResult{withEvidence}, tune)
		if got1[0] != got2[0] {
			t.Fatalf("RerankInput=%q: buildRerankDocs output changed by Evidence:\n  without: %q\n  with:    %q", tune.RerankInput, got1[0], got2[0])
		}
	}
}
