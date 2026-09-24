package api

import (
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// --- #267: matched chunk evidence reaches /ask's excerpt ---

// TestBuildBudgetedAskMessages_MidLateEvidenceSurvivesBudget reproduces the
// exact bug #267 fixes: a document whose head has no relevant text, but
// whose Evidence (from a chunk lane fused via mergeRRFMode) points at an
// answer-bearing passage deep inside the body. Before this change,
// buildBudgetedAskMessages always called askPassage — which, given a small
// budget and a question with no lexical overlap with the head, falls back
// to the document's beginning ("head" mode) and the evidence text never
// reached the prompt at all.
func TestBuildBudgetedAskMessages_MidLateEvidenceSurvivesBudget(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	needle := "예약번호는 ZX900 입니다"
	content := strings.Repeat("관련없는 앞부분 문단입니다. ", 200) + needle + strings.Repeat(" 관련없는 뒷부분 문단입니다.", 200)
	r := &model.SearchResult{
		Document:  model.Document{ID: id, Title: "record", Content: content},
		MatchType: "hybrid", // document-lane primary; Content is the FULL body
		Evidence: []model.MatchedEvidence{
			{ChunkID: 42, ChunkIndex: 7, Lane: model.MatchTypeChunkVector, Score: 0.8, Text: needle},
		},
	}
	result := RetrievalResult{Observed: []*model.SearchResult{r}}

	// The question shares NO vocabulary with the document at all — the
	// lexical fallback (askPassage) would have no signal and would keep the
	// document's beginning. Evidence must win regardless.
	messages, manifest := buildBudgetedAskMessages("예약 확인해줘", "예약 확인해줘", result, nil)

	evidenceMsg := messages[len(messages)-2].Content
	if !strings.Contains(evidenceMsg, "ZX900") {
		t.Fatalf("mid-document evidence text missing from prompt: %s", truncateForTest(evidenceMsg))
	}
	if len(manifest.Evidence) != 1 {
		t.Fatalf("manifest.Evidence = %+v, want exactly 1 entry", manifest.Evidence)
	}
	if manifest.Evidence[0].Mode != askEvidenceModeMatchedChunk {
		t.Errorf("Mode = %q, want %q", manifest.Evidence[0].Mode, askEvidenceModeMatchedChunk)
	}
	if len(manifest.Evidence[0].ChunkIDs) != 1 || manifest.Evidence[0].ChunkIDs[0] != 42 {
		t.Errorf("ChunkIDs = %v, want [42]", manifest.Evidence[0].ChunkIDs)
	}
}

// TestBuildBudgetedAskMessages_FollowUpUsesExcerptQueryNotUserQuestion
// verifies the standalone (rewritten) search question — not the user's raw
// pronoun-only follow-up — drives askPassage's lexical fallback window for
// a document that has NO chunk evidence. "그건 언제로 정했지?" carries no
// vocabulary of its own; only the standalone rewrite ("면접 일정 언제로
// 정했지?") can locate the relevant passage.
func TestBuildBudgetedAskMessages_FollowUpUsesExcerptQueryNotUserQuestion(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	content := strings.Repeat("무관한 문단. ", 300) + "면접 일정은 다음주 화요일로 정했다." + strings.Repeat(" 무관한 문단.", 300)
	r := &model.SearchResult{
		Document:  model.Document{ID: id, Title: "record", Content: content},
		MatchType: "hybrid",
	}
	result := RetrievalResult{Observed: []*model.SearchResult{r}}

	pronounOnly := "그건 언제로 정했지?"
	standalone := "면접 일정 언제로 정했지?"

	messages, _ := buildBudgetedAskMessages(pronounOnly, standalone, result, nil)
	evidenceMsg := messages[len(messages)-2].Content
	if !strings.Contains(evidenceMsg, "화요일") {
		t.Fatalf("standalone-question-driven passage missing from prompt: %s", truncateForTest(evidenceMsg))
	}

	// The user's ORIGINAL wording must still be what is shown as the final
	// turn (and what a caller would persist) — the excerpt query is
	// search-only, exactly like searchQuestion elsewhere in the pipeline.
	finalTurn := messages[len(messages)-1].Content
	if finalTurn != pronounOnly {
		t.Errorf("final turn = %q, want the user's original wording %q", finalTurn, pronounOnly)
	}
}

// TestBuildBudgetedAskMessages_ChunkOnlyResultUsesContentDirectly verifies a
// SearchResult that never merged with a document-lane primary
// (model.MatchTypeChunkVector — Content already IS the matched chunk) is
// excerpted from Content directly, tagged askEvidenceModeChunkOnly, and its
// ChunkIDs are recorded in the manifest.
func TestBuildBudgetedAskMessages_ChunkOnlyResultUsesContentDirectly(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	r := &model.SearchResult{
		Document:  model.Document{ID: id, Title: "record", Content: "청크 본문: ZX900"},
		MatchType: model.MatchTypeChunkVector,
		Evidence: []model.MatchedEvidence{
			{ChunkID: 7, ChunkIndex: 2, Lane: model.MatchTypeChunkVector, Score: 0.5, Text: "청크 본문: ZX900"},
		},
	}
	result := RetrievalResult{Observed: []*model.SearchResult{r}}

	messages, manifest := buildBudgetedAskMessages("질문", "질문", result, nil)
	evidenceMsg := messages[len(messages)-2].Content
	if !strings.Contains(evidenceMsg, "ZX900") {
		t.Fatalf("chunk-only content missing from prompt: %s", truncateForTest(evidenceMsg))
	}
	if manifest.Evidence[0].Mode != askEvidenceModeChunkOnly {
		t.Errorf("Mode = %q, want %q", manifest.Evidence[0].Mode, askEvidenceModeChunkOnly)
	}
	if len(manifest.Evidence[0].ChunkIDs) != 1 || manifest.Evidence[0].ChunkIDs[0] != 7 {
		t.Errorf("ChunkIDs = %v, want [7]", manifest.Evidence[0].ChunkIDs)
	}
}

// TestBuildBudgetedAskMessages_UnlocatableEvidenceUsesChunkTextWithMarker
// covers deep-plan #267 finding F3: a chunk's text is not guaranteed to be
// a byte-for-byte substring of its parent document's Content (chunker
// cleanup/heading-prefix/merge). When the evidence text cannot be located,
// the chunk's own text must still reach the prompt — with a marker, never a
// fabricated offset — rather than silently falling back to the document
// head as if no evidence existed.
func TestBuildBudgetedAskMessages_UnlocatableEvidenceUsesChunkTextWithMarker(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	r := &model.SearchResult{
		Document:  model.Document{ID: id, Title: "record", Content: "저장된 원문은 이렇게 시작합니다..."},
		MatchType: "hybrid",
		Evidence: []model.MatchedEvidence{
			// Cleaned/merged chunk text that is NOT a substring of Content.
			{ChunkID: 9, ChunkIndex: 1, Lane: model.MatchTypeChunkFTS, Score: 0.4, Text: "정제된 청크 본문: ZX900 (원문과 바이트 단위로 다름)"},
		},
	}
	result := RetrievalResult{Observed: []*model.SearchResult{r}}

	messages, manifest := buildBudgetedAskMessages("질문", "질문", result, nil)
	evidenceMsg := messages[len(messages)-2].Content
	if !strings.Contains(evidenceMsg, "ZX900") {
		t.Fatalf("unlocatable chunk text missing from prompt: %s", truncateForTest(evidenceMsg))
	}
	if !strings.Contains(evidenceMsg, "위치 미확인") {
		t.Fatalf("missing unlocated-evidence marker: %s", truncateForTest(evidenceMsg))
	}
	if manifest.Evidence[0].Mode != askEvidenceModeMatchedChunk {
		t.Errorf("Mode = %q, want %q", manifest.Evidence[0].Mode, askEvidenceModeMatchedChunk)
	}
}

// TestBuildBudgetedAskMessages_MultipleEvidenceOverlapDedupes verifies
// locateEvidenceSpans merges two overlapping located spans for the same
// document into one window rather than duplicating the shared text. Content
// is deliberately larger than the per-document excerpt budget so the
// assertion cannot pass merely because the whole document fit unwindowed
// (askEvidenceModeFull) — it must actually exercise evidencePassage.
func TestBuildBudgetedAskMessages_MultipleEvidenceOverlapDedupes(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	content := strings.Repeat("무관한 문단입니다. ", 1500) + "겹치는 문단 시작 예약번호 ZX900 겹치는 문단 끝" + strings.Repeat(" 무관한 문단입니다.", 1500)
	if len(content) <= askExcerptBytes {
		t.Fatalf("test setup bug: content (%d bytes) must exceed askExcerptBytes (%d) to exercise windowing", len(content), askExcerptBytes)
	}
	overlapA := "겹치는 문단 시작 예약번호 ZX900"
	overlapB := "예약번호 ZX900 겹치는 문단 끝"
	r := &model.SearchResult{
		Document:  model.Document{ID: id, Title: "record", Content: content},
		MatchType: "hybrid",
		Evidence: []model.MatchedEvidence{
			{ChunkID: 1, ChunkIndex: 0, Lane: model.MatchTypeChunkVector, Score: 0.9, Text: overlapA},
			{ChunkID: 2, ChunkIndex: 1, Lane: model.MatchTypeChunkFTS, Score: 0.6, Text: overlapB},
		},
	}
	result := RetrievalResult{Observed: []*model.SearchResult{r}}

	// Question shares no vocabulary with the document — askPassage's
	// lexical fallback would otherwise retain the document's beginning,
	// which contains neither "ZX900" nor the overlap text at all.
	messages, manifest := buildBudgetedAskMessages("질문", "질문", result, nil)
	evidenceMsg := messages[len(messages)-2].Content
	if strings.Count(evidenceMsg, "ZX900") != 1 {
		t.Fatalf("overlapping evidence duplicated instead of deduped: %s", truncateForTest(evidenceMsg))
	}
	if manifest.Evidence[0].Mode != askEvidenceModeMatchedChunk {
		t.Fatalf("Mode = %q, want %q (evidence path not exercised)", manifest.Evidence[0].Mode, askEvidenceModeMatchedChunk)
	}
	if got := manifest.Evidence[0].ChunkIDs; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("ChunkIDs = %v, want [1 2]", got)
	}
}

func truncateForTest(s string) string {
	if len(s) > 500 {
		return s[:500] + "...(truncated)"
	}
	return s
}
