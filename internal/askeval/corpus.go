package askeval

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/baekenough/second-brain/internal/chunker"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// aliasNamespace is a fixed, arbitrary UUID used as uuid.NewSHA1's namespace
// so a fixture's corpus doc alias (e.g. "call-1") maps to the SAME
// uuid.UUID on every run — determinism report.go's baseline diff and every
// fixture's Gold.SupportDocs reference both depend on. This value has no
// meaning beyond "a constant unique to this package" and must never change
// once a fixture file references an alias built against it.
var aliasNamespace = uuid.MustParse("6ad7c1c0-3b8b-4f7c-9c4e-5f0e6a8f5b3d")

// aliasID derives a deterministic uuid.UUID from a fixture-local alias.
func aliasID(alias string) uuid.UUID {
	return uuid.NewSHA1(aliasNamespace, []byte(alias))
}

// embedDim is the fake embedding vector width — small on purpose (this is a
// deterministic bag-of-bigram hash, not a real semantic embedding; see
// hashEmbed) so cosineSim stays cheap across every fixture's corpus.
const embedDim = 48

// corpus is one fixture's isolated document set. It implements every
// interface search.Service needs on BOTH the document-store side
// (search.DocumentSearcher, via Search) and the chunk-lane side
// (search.ChunkSearcher, search.FilteredChunkSearcher, search.ChunkLister),
// so a *search.Service built as
// search.NewService(c, hashedEmbedder{}).WithChunkStore(c) exercises the
// SAME retrieval code path production traffic does — assembleRetrieval
// (ask_retrieval.go) does not know or care that documentSearcher is a fake.
//
// Every chunk this type returns has its Chunk.ID and Chunk.ChunkIndex
// populated from a real chunker.Split pass over the document content (see
// buildCorpus) — issue #267 (matched-chunk evidence propagation) needs both
// fields to be real and stable, and this corpus exists so #267 can be
// measured against this runner without editing it (deep-plan #266 §2.3).
type corpus struct {
	now  time.Time // this fixture's as_of, used for recency-direction and CollectedAt fallback
	docs []model.Document
	byID map[uuid.UUID]*model.Document

	chunksByDoc map[uuid.UUID][]store.Chunk
	allChunks   []store.Chunk

	// semanticFold is this fixture's Fixture.SemanticAliases, compiled once
	// (semanticAliasFold) — the SAME fold function corpus.chunkVector applies
	// to a chunk's text and runOne hands to hashedEmbedder for the query
	// text, so both sides of one cosine-similarity comparison are folded
	// identically. nil-Fixture.SemanticAliases compiles to the identity
	// function (see semanticAliasFold), so every pre-existing fixture's
	// vector-lane behaviour is byte-for-byte unchanged.
	semanticFold func(string) string
}

// buildCorpus converts a Fixture's declared documents (and their derived
// chunks) into a corpus. now is the fixture's parsed AsOf.
func buildCorpus(f Fixture, now time.Time) (*corpus, error) {
	c := &corpus{
		now:          now,
		docs:         make([]model.Document, len(f.Corpus)),
		byID:         make(map[uuid.UUID]*model.Document, len(f.Corpus)),
		chunksByDoc:  make(map[uuid.UUID][]store.Chunk, len(f.Corpus)),
		semanticFold: semanticAliasFold(f.SemanticAliases),
	}
	for i, cd := range f.Corpus {
		id := aliasID(cd.Alias)
		doc := model.Document{
			ID:          id,
			SourceType:  model.SourceType(cd.SourceType),
			SourceID:    cd.Alias,
			Title:       cd.Title,
			Content:     cd.Content,
			Status:      "active",
			CollectedAt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if cd.OccurredAt != "" {
			t, err := time.Parse(time.RFC3339, cd.OccurredAt)
			if err != nil {
				return nil, fmt.Errorf("askeval: corpus doc %q: occurred_at: %w", cd.Alias, err)
			}
			doc.OccurredAt = &t
		}
		c.docs[i] = doc
	}
	// byID is populated AFTER every append into c.docs above — taking &c.docs[i]
	// before the slice is fully built would alias a backing array Go is free
	// to replace on the next append.
	var nextChunkID int64 = 1
	for i := range c.docs {
		doc := &c.docs[i]
		c.byID[doc.ID] = doc
		parts := chunker.Split(doc.Content, chunker.Options{SourceType: string(doc.SourceType)})
		chunks := make([]store.Chunk, 0, len(parts))
		for idx, text := range parts {
			ch := store.Chunk{
				ID:         nextChunkID,
				DocumentID: doc.ID,
				ChunkIndex: idx,
				Content:    text,
				ByteSize:   len(text),
				CreatedAt:  now,
			}
			nextChunkID++
			chunks = append(chunks, ch)
			c.allChunks = append(c.allChunks, ch)
		}
		c.chunksByDoc[doc.ID] = chunks
	}
	return c, nil
}

// resolveAlias returns alias's deterministic document UUID as a string, and
// whether that alias actually names a document in THIS corpus — the
// substitution function llm.go's scriptedCompleter uses for
// "{{doc:<alias>}}" placeholders, and metrics.go uses to translate
// Gold.SupportDocs aliases into the IDs a "sources" SSE event actually
// carries.
func (c *corpus) resolveAlias(alias string) (string, bool) {
	id := aliasID(alias)
	if _, ok := c.byID[id]; !ok {
		return "", false
	}
	return id.String(), true
}

// --- search.DocumentSearcher ---

// Search implements search.DocumentSearcher (assembleRetrieval's Stage 2
// dependency, ask_retrieval.go). It never calls a network or a database —
// every candidate comes from the fixture's own Corpus — and applies the
// SAME filter semantics model.SearchQuery documents production callers can
// rely on: SourceType/SourceTypes ∪ (IncludeSourceTypes), ExcludeSourceTypes,
// the half-open OccurredFrom/OccurredTo window (a document with a nil
// OccurredAt is excluded whenever either bound is set — see
// model.SearchQuery.OccurredFrom's doc comment), and Limit.
func (c *corpus) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	include := toSet(q.IncludeSourceTypes())
	exclude := toSet(q.ExcludeSourceTypes)

	type scored struct {
		doc   *model.Document
		score float64
	}
	var cands []scored
	for i := range c.docs {
		d := &c.docs[i]
		if len(include) > 0 && !include[d.SourceType] {
			continue
		}
		if exclude[d.SourceType] {
			continue
		}
		if !inWindow(d.OccurredAt, q.OccurredFrom, q.OccurredTo) {
			continue
		}
		score := lexicalScore(q.Query, d.Title+"\n"+d.Content)
		if strings.TrimSpace(q.Query) != "" && score <= 0 {
			continue
		}
		cands = append(cands, scored{d, score})
	}

	if q.SortsByRecency() {
		asc := q.RecencyAscending(c.now)
		sort.SliceStable(cands, func(i, j int) bool {
			ti, tj := recencyOf(cands[i].doc), recencyOf(cands[j].doc)
			if asc {
				return ti.Before(tj)
			}
			return ti.After(tj)
		})
	} else {
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	}

	limit := q.Limit
	if limit <= 0 {
		limit = len(cands)
	}
	out := make([]*model.SearchResult, 0, min(limit, len(cands)))
	for _, s := range cands {
		if len(out) >= limit {
			break
		}
		out = append(out, &model.SearchResult{Document: *s.doc, Score: s.score, MatchType: "askeval-lexical"})
	}
	return out, nil
}

// --- search.ChunkSearcher / search.FilteredChunkSearcher / search.ChunkLister ---

func (c *corpus) SearchFTS(_ context.Context, query string, limit int) ([]store.ChunkSearchResult, error) {
	return c.chunkLexical(query, model.SearchQuery{}, limit), nil
}

func (c *corpus) SearchFTSFiltered(_ context.Context, q model.SearchQuery, limit int) ([]store.ChunkSearchResult, error) {
	return c.chunkLexical(q.Query, q, limit), nil
}

func (c *corpus) SearchVector(_ context.Context, vec []float32, limit int) ([]store.ChunkSearchResult, error) {
	return c.chunkVector(vec, model.SearchQuery{}, limit), nil
}

func (c *corpus) SearchVectorFiltered(_ context.Context, q model.SearchQuery, limit int) ([]store.ChunkSearchResult, error) {
	return c.chunkVector(q.Embedding, q, limit), nil
}

func (c *corpus) ListByDocument(_ context.Context, documentID uuid.UUID) ([]store.Chunk, error) {
	return append([]store.Chunk{}, c.chunksByDoc[documentID]...), nil
}

func (c *corpus) chunkLexical(query string, q model.SearchQuery, limit int) []store.ChunkSearchResult {
	include := toSet(q.IncludeSourceTypes())
	exclude := toSet(q.ExcludeSourceTypes)
	var out []store.ChunkSearchResult
	for _, ch := range c.allChunks {
		doc := c.byID[ch.DocumentID]
		if doc == nil {
			continue
		}
		if len(include) > 0 && !include[doc.SourceType] {
			continue
		}
		if exclude[doc.SourceType] {
			continue
		}
		if !inWindow(doc.OccurredAt, q.OccurredFrom, q.OccurredTo) {
			continue
		}
		rank := lexicalScore(query, ch.Content)
		if strings.TrimSpace(query) != "" && rank <= 0 {
			continue
		}
		out = append(out, chunkResultFor(ch, doc, rank, 0))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rank > out[j].Rank })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (c *corpus) chunkVector(vec []float32, q model.SearchQuery, limit int) []store.ChunkSearchResult {
	include := toSet(q.IncludeSourceTypes())
	exclude := toSet(q.ExcludeSourceTypes)
	var out []store.ChunkSearchResult
	for _, ch := range c.allChunks {
		doc := c.byID[ch.DocumentID]
		if doc == nil {
			continue
		}
		if len(include) > 0 && !include[doc.SourceType] {
			continue
		}
		if exclude[doc.SourceType] {
			continue
		}
		if !inWindow(doc.OccurredAt, q.OccurredFrom, q.OccurredTo) {
			continue
		}
		sim := cosineSim(vec, hashEmbed(c.semanticFold(ch.Content)))
		if sim <= 0 {
			continue
		}
		out = append(out, chunkResultFor(ch, doc, 0, sim))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func chunkResultFor(ch store.Chunk, doc *model.Document, rank, score float64) store.ChunkSearchResult {
	return store.ChunkSearchResult{
		Chunk:               ch,
		Rank:                rank,
		Score:               score,
		DocumentTitle:       doc.Title,
		DocumentSource:      string(doc.SourceType),
		DocumentStatus:      doc.Status,
		DocumentOccurredAt:  doc.OccurredAt,
		DocumentCollectedAt: doc.CollectedAt,
		DocumentMetadata:    doc.Metadata,
	}
}

// --- search.EmbeddingEngine ---

// hashedEmbedder is a deterministic stand-in for a real embedding backend:
// Embed/EmbedBatch hash character bigrams of the input text into a fixed
// embedDim-wide bag-of-bigrams vector (hashEmbed) and L2-normalize it, so
// cosine similarity between two texts tracks their literal lexical overlap
// — enough to make the chunk-vector lane (search.go's searchChunksVector)
// actually run end-to-end against synthetic fixtures, without depending on
// a real embedding API this offline runner must never call (issue #266
// scope: CI-safe, no network).
//
// fold is applied to every text before hashing — runOne always constructs
// this with the SAME corpus.semanticFold function corpus.chunkVector uses on
// chunk text, so a fixture's semantic_aliases groups (Fixture.SemanticAliases'
// doc comment) fold identically on both sides of a cosine-similarity
// comparison. A nil fold (e.g. a hashedEmbedder built outside runOne, such as
// in a unit test) means "no folding" — see semanticAliasFold's nil-groups
// case, which is what every pre-existing fixture (no semantic_aliases) gets.
type hashedEmbedder struct {
	fold func(string) string
}

func (hashedEmbedder) Enabled() bool  { return true }
func (hashedEmbedder) Dimension() int { return embedDim }

func (h hashedEmbedder) apply(text string) string {
	if h.fold == nil {
		return text
	}
	return h.fold(text)
}

func (h hashedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return hashEmbed(h.apply(text)), nil
}

func (h hashedEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = hashEmbed(h.apply(t))
	}
	return out, nil
}

func hashEmbed(text string) []float32 {
	vec := make([]float32, embedDim)
	for _, bg := range charBigrams(text) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(bg))
		vec[int(h.Sum32()%uint32(embedDim))]++
	}
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if sumSq == 0 {
		return vec
	}
	norm := float32(1 / math.Sqrt(sumSq))
	for i := range vec {
		vec[i] *= norm
	}
	return vec
}

func cosineSim(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot // both vectors are already L2-normalized, so dot == cosine.
}

// semanticAliasFold compiles a Fixture's SemanticAliases into a text
// transform: every non-canonical member of a group is replaced by that
// group's first ("canonical") member, longest member first so a shorter
// alias sharing a prefix/suffix with a longer one cannot partially consume
// it before the longer replacement runs. groups==nil (the overwhelming
// majority of fixtures, which declare no semantic_aliases) returns the
// identity function — every fixture predating this mechanism embeds exactly
// as before.
//
// This function backs ONLY hashedEmbedder/corpus.chunkVector (the fake
// vector lane) — see Fixture.SemanticAliases' doc comment for why it must
// never reach corpus.Search's lexicalScore or chunkLexical (the fake
// document/FTS lanes): folding a query's synonym into the lexical lane too
// would let a paraphrase fixture pass via the LEXICAL lane, which defeats
// the fixture's purpose of isolating what the vector lane alone recovers.
func semanticAliasFold(groups [][]string) func(string) string {
	if len(groups) == 0 {
		return func(s string) string { return s }
	}
	type replacement struct{ from, to string }
	var reps []replacement
	for _, group := range groups {
		if len(group) < 2 {
			continue // validate() rejects this at Load time; defensive only.
		}
		canonical := group[0]
		for _, member := range group[1:] {
			if member == canonical {
				continue
			}
			reps = append(reps, replacement{from: member, to: canonical})
		}
	}
	sort.Slice(reps, func(i, j int) bool { return len(reps[i].from) > len(reps[j].from) })
	return func(s string) string {
		for _, r := range reps {
			s = strings.ReplaceAll(s, r.from, r.to)
		}
		return s
	}
}

// --- shared lexical helpers ---

func toSet(list []model.SourceType) map[model.SourceType]bool {
	if len(list) == 0 {
		return nil
	}
	out := make(map[model.SourceType]bool, len(list))
	for _, st := range list {
		out[st] = true
	}
	return out
}

// inWindow mirrors model.SearchQuery.OccurredFrom's documented semantics: a
// nil occurredAt is excluded whenever either bound is set; otherwise the
// half-open [from, to) test applies per set bound.
func inWindow(occurredAt, from, to *time.Time) bool {
	if from == nil && to == nil {
		return true
	}
	if occurredAt == nil {
		return false
	}
	if from != nil && occurredAt.Before(*from) {
		return false
	}
	if to != nil && !occurredAt.Before(*to) {
		return false
	}
	return true
}

func recencyOf(d *model.Document) time.Time {
	if d.OccurredAt != nil {
		return *d.OccurredAt
	}
	return d.CollectedAt
}

func normalizeForMatch(s string) string { return strings.ToLower(s) }

// wordTokens splits on anything that is not a letter or digit — the same
// rule ask_context.go's askPassage uses for its own lexical-window scoring,
// reused here so this fake's notion of "a term" matches production's.
func wordTokens(s string) []string {
	return strings.FieldsFunc(normalizeForMatch(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// charBigrams returns every 2-rune shingle of s with whitespace removed
// first — character bigrams, rather than word tokens alone, so short
// Korean queries (which rarely share a segmentable "word" with a matching
// passage the way whitespace-delimited English does) still score a lexical
// match against relevant fixture content.
func charBigrams(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	runes := []rune(b.String())
	if len(runes) == 0 {
		return nil
	}
	if len(runes) == 1 {
		return []string{string(runes)}
	}
	out := make([]string, 0, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		out = append(out, string(runes[i:i+2]))
	}
	return out
}

// lexicalScore is this package's "bigram lexical" fake-corpus scorer (see
// corpus's doc comment): a word-token match counts double a bigram match,
// so a fixture author who wants document A to clearly outrank document B
// for query Q only needs to put Q's actual words in A, not tune a
// similarity threshold.
func lexicalScore(query, text string) float64 {
	if strings.TrimSpace(query) == "" {
		return 0
	}
	text = normalizeForMatch(text)
	var compact strings.Builder
	for _, r := range text {
		if !unicode.IsSpace(r) {
			compact.WriteRune(r)
		}
	}
	compactText := compact.String()

	score := 0.0
	seenWord := map[string]bool{}
	for _, w := range wordTokens(query) {
		if len([]rune(w)) < 2 || seenWord[w] {
			continue
		}
		seenWord[w] = true
		if strings.Contains(text, w) {
			score += 2
		}
	}
	seenBigram := map[string]bool{}
	for _, bg := range charBigrams(query) {
		if seenBigram[bg] {
			continue
		}
		seenBigram[bg] = true
		if strings.Contains(compactText, bg) {
			score++
		}
	}
	return score
}
