package api

import "github.com/google/uuid"

// askEvidenceLayer records which RetrievalResult layer (ask_retrieval.go)
// one manifest entry came from — the same 원문/정리 vs 추론 split the prompt
// itself renders into two sections. Citation validation (ask_citation.go)
// needs this to flag a citation into the [추론] section separately from a
// citation into [관측된 사실]: both are ALLOWED (the model is explicitly
// permitted to cite a hedged inference), but only the inferred one is a
// signal worth surfacing back to the client (issue #268 §2.2,
// "추론을 사실로 인용" regression detector).
type askEvidenceLayer string

const (
	askEvidenceObserved askEvidenceLayer = "observed"
	askEvidenceInferred askEvidenceLayer = "inferred"
)

// askEvidenceMode records HOW one document's excerpt was produced —
// currently by askPassage (ask_context.go). "full" means the whole document
// fit inside its budget share; "lexical_window" means askPassage's
// term-matching heuristic located and kept a window other than the
// beginning; "head" means no query term matched at all and the beginning of
// the document was kept as the default. #267 adds "matched_chunk" and
// "chunk_only" once retrieval starts preserving which chunk actually
// matched (deep-plan #268 §2.1) — this type is not closed to that
// extension.
type askEvidenceMode string

const (
	askEvidenceModeFull          askEvidenceMode = "full"
	askEvidenceModeLexicalWindow askEvidenceMode = "lexical_window"
	askEvidenceModeHead          askEvidenceMode = "head"
	// askEvidenceModeMatchedChunk (#267): the excerpt was built from a
	// model.SearchResult.Evidence entry — either a budget window located
	// around the matched chunk's text inside the full document body, or,
	// when that text could not be located verbatim (chunker cleanup/merge —
	// model.MatchedEvidence's doc comment), the chunk's own text used
	// directly. Distinct from askEvidenceModeLexicalWindow: that mode's
	// window is a term-overlap HEURISTIC over the whole document with no
	// retrieval-time signal behind it, while this mode's window is centred
	// on evidence retrieval itself already verified as a match.
	askEvidenceModeMatchedChunk askEvidenceMode = "matched_chunk"
	// askEvidenceModeChunkOnly (#267): the SearchResult came from a
	// chunk-lane winner that never merged with a document-lane primary
	// (model.MatchTypeChunkVector/MatchTypeChunkFTS) — Content already IS
	// the matched chunk's text, so the "excerpt" is the full available
	// Content rather than a window into a larger document body.
	askEvidenceModeChunkOnly askEvidenceMode = "chunk_only"
)

// askPromptEvidence is one document actually inserted into the Stage 3
// prompt, in the order it was written. Bytes is the excerpt's own byte
// length (post-clip), not the source document's full size — it exists so a
// future caller can audit how the excerpt budget (ask_context.go's
// askExcerptBytes/perDoc split) was actually spent per document.
// ChunkIDs (#267) lists the model.MatchedEvidence.ChunkID values available
// for this document at excerpt-selection time — i.e. the chunk-lane
// provenance behind Mode askEvidenceModeMatchedChunk/askEvidenceModeChunkOnly.
// It is nil for a document whose excerpt came from askPassage's lexical
// heuristic (Mode full/lexical_window/head): that path has no chunk
// provenance to report.
type askPromptEvidence struct {
	ID       uuid.UUID
	Layer    askEvidenceLayer
	Mode     askEvidenceMode
	Bytes    int
	ChunkIDs []int64
}

// askPromptManifest is the ground truth for citation validation
// (ask_citation.go): the set of document IDs the model actually SAW in this
// turn's prompt, in prompt order.
//
// This is deliberately NOT result.Observed/result.Inferred. A document can
// be selected by retrieval (and therefore appear in the "sources" SSE
// event, spec §5.3) yet still be dropped from the prompt by the excerpt
// budget (ask_context.go's "[입력 예산으로 추가 문서 생략]" branch) — that
// document's ID belongs in Omitted, not Evidence, and a citation of it must
// be treated exactly like a fabricated ID (deep-plan #268 finding F2). Using
// result.Observed as the allow-list here would silently let the model "cite"
// a document it was never shown.
type askPromptManifest struct {
	// Evidence lists every document actually written into the prompt, in
	// the order it was written.
	Evidence []askPromptEvidence
	// Omitted lists documents that were selected by retrieval but dropped
	// by the excerpt budget before ever reaching the prompt.
	Omitted []uuid.UUID
}

// layerOf reports which layer id belongs to, and whether id is present in
// the manifest's Evidence at all. A false ok means id was never shown to the
// model — whether because it was never selected, or because it was selected
// and then Omitted by budget makes no difference to a citation validator:
// both cases mean "the model could not legitimately have this ID".
func (m askPromptManifest) layerOf(id uuid.UUID) (layer askEvidenceLayer, ok bool) {
	for _, e := range m.Evidence {
		if e.ID == id {
			return e.Layer, true
		}
	}
	return "", false
}
