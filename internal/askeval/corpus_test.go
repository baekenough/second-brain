package askeval

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// TestSemanticAliasFold_ReplacesNonCanonicalMembers pins semanticAliasFold's
// text-transform contract directly (Fixture.SemanticAliases' doc comment):
// every non-canonical member of a group folds to that group's first
// (canonical) member; text with no matching member, and a nil/empty groups
// slice, are both left byte-for-byte unchanged.
func TestSemanticAliasFold_ReplacesNonCanonicalMembers(t *testing.T) {
	fold := semanticAliasFold([][]string{{"마감일", "완료 기준일"}})

	got := fold("최종 완료 기준일은 8월 15일로 정해졌다.")
	want := "최종 마감일은 8월 15일로 정해졌다."
	if got != want {
		t.Errorf("fold(doc text) = %q, want %q", got, want)
	}

	// The canonical member itself, and unrelated text, pass through unchanged.
	if got := fold("마감일이 언제야?"); got != "마감일이 언제야?" {
		t.Errorf("fold(canonical-only text) = %q, want unchanged", got)
	}
	if got := fold("전혀 관련 없는 문장"); got != "전혀 관련 없는 문장" {
		t.Errorf("fold(unrelated text) = %q, want unchanged", got)
	}

	if identity := semanticAliasFold(nil); identity("아무 텍스트") != "아무 텍스트" {
		t.Errorf("semanticAliasFold(nil) must be the identity function")
	}
	if identity := semanticAliasFold([][]string{}); identity("아무 텍스트") != "아무 텍스트" {
		t.Errorf("semanticAliasFold([][]string{}) must be the identity function")
	}
}

// TestSemanticAliasFold_LongestMemberFirst guards the "longest member first"
// ordering semanticAliasFold's doc comment promises: a short alias sharing a
// prefix with a longer one in the SAME group must not partially consume the
// longer one's match before the longer replacement gets a chance to run.
func TestSemanticAliasFold_LongestMemberFirst(t *testing.T) {
	fold := semanticAliasFold([][]string{{"총액", "총 계약 금액", "총 계약"}})
	got := fold("이번 총 계약 금액은 크다")
	want := "이번 총액은 크다"
	if got != want {
		t.Errorf("fold = %q, want %q (longest alias %q must win over the shorter %q it contains)", got, want, "총 계약 금액", "총 계약")
	}
}

// TestSemanticAliasFold_AffectsOnlyVectorLane is this package's proof of the
// scoping contract in Fixture.SemanticAliases' doc comment: a query
// paraphrasing a document's actual wording is found by the FAKE VECTOR LANE
// once folded, but the fake LEXICAL lanes (corpus.Search's document lane,
// corpus.SearchFTS/SearchFTSFiltered's chunk lane) never see a folded text
// at all — a synonym must never leak into them, or a fixture built to show
// "the vector lane alone recovers this" would pass on both lanes and stop
// discriminating anything (see docs/ask-evaluation-protocol.md's
// "semantic_aliases" section).
//
// "마감일" (the paraphrase) and "완료 기준일" (the document's literal wording)
// share ZERO character bigrams by construction — {마감,감일} vs
// {완료,료기,기준,준일} — so any lexical/vector hit below is attributable
// entirely to folding, not to incidental overlap.
func TestSemanticAliasFold_AffectsOnlyVectorLane(t *testing.T) {
	f := Fixture{
		ID: "unit-alias-scope", Category: "single_turn",
		AsOf: "2026-06-10T09:00:00+09:00",
		Corpus: []CorpusDoc{{
			Alias: "doc-1", SourceType: "call", Title: "테스트 통화",
			// Deliberately minimal: every character bigram here — {완료,료기,
			// 기준,준일} plus the title's own bigrams — is disjoint from
			// "마감일"'s bigrams {마감,감일}, so any hit below is
			// attributable entirely to folding (see this test's doc
			// comment). A longer, more natural sentence risks an
			// unrelated character (e.g. a second "일") accidentally
			// creating a shared bigram and silently invalidating that
			// assumption.
			Content: "완료 기준일",
		}},
		Question: "완료 기준일 확인",
		SemanticAliases: [][]string{
			{"마감일", "완료 기준일"},
		},
		Gold: Gold{Answerable: false},
	}
	asOf, err := time.Parse(time.RFC3339, f.AsOf)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := buildCorpus(f, asOf)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// --- Lexical document lane: must NOT find the paraphrase. ---
	docResults, err := cp.Search(ctx, model.SearchQuery{Query: "마감일"})
	if err != nil {
		t.Fatal(err)
	}
	if len(docResults) != 0 {
		t.Errorf("document lane (lexical) matched paraphrase %q — semantic_aliases must not leak into corpus.Search: %+v", "마감일", docResults)
	}

	// --- Lexical chunk (FTS) lane: must NOT find the paraphrase either. ---
	chunkResults, err := cp.SearchFTS(ctx, "마감일", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunkResults) != 0 {
		t.Errorf("chunk FTS lane (lexical) matched paraphrase %q — semantic_aliases must not leak into chunkLexical: %+v", "마감일", chunkResults)
	}

	// --- Fake vector lane: MUST find it once the query is folded exactly
	// the way hashedEmbedder (runner.go's runOne) folds it. ---
	embedder := hashedEmbedder{fold: cp.semanticFold}
	queryVec, err := embedder.Embed(ctx, "마감일")
	if err != nil {
		t.Fatal(err)
	}
	vecResults, err := cp.SearchVectorFiltered(ctx, model.SearchQuery{Embedding: queryVec}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecResults) == 0 {
		t.Fatalf("vector lane did not match paraphrase %q even after folding — semantic_aliases had no effect", "마감일")
	}

	// --- Control: an otherwise-identical corpus WITHOUT semantic_aliases
	// (cp2.semanticFold is the identity function — see semanticAliasFold's
	// nil-groups case) must NOT match the same paraphrase query, on either
	// side of the comparison unfolded. This isolates the hit above as
	// coming from folding specifically, not from some other incidental
	// bigram overlap this fake embedder might produce.
	fNoAlias := f
	fNoAlias.SemanticAliases = nil
	cp2, err := buildCorpus(fNoAlias, asOf)
	if err != nil {
		t.Fatal(err)
	}
	controlEmbedder := hashedEmbedder{fold: cp2.semanticFold}
	controlVec, err := controlEmbedder.Embed(ctx, "마감일")
	if err != nil {
		t.Fatal(err)
	}
	controlResults, err := cp2.SearchVectorFiltered(ctx, model.SearchQuery{Embedding: controlVec}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(controlResults) != 0 {
		t.Errorf("vector lane matched the paraphrase query on a corpus WITHOUT semantic_aliases — the bigram-disjointness assumption this test relies on no longer holds: %+v", controlResults)
	}
}
