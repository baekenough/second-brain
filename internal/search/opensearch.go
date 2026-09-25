package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// OpenSearchClient queries an OpenSearch index of document chunks analyzed
// with a Korean morphological (nori) tokenizer, giving this lane a precision
// property the Postgres pg_bigm (bigram) FTS lane does not have: bigram has
// no notion of a morpheme boundary, so "회의" matches inside "사회의" and
// "기회의". Measured against ubuntu1 production data:
//
//	query "회의": pg_bigm 865 hits, nori exact-morpheme-match only 184 of them
//	query "계약": pg_bigm 1,356 hits, nori exact-morpheme-match only 368
//	query "예약": pg_bigm 1,273 hits, nori exact-morpheme-match only 497
//
// The excess is substring noise that dilutes the candidate pool before a
// reranker ever sees it. This client does not replace pg_bigm — it is fused
// into the existing RRF pipeline as one more additive signal, exactly like
// the chunk vector/FTS lanes in search.go (see WithOpenSearch).
//
// The index this client queries (default name "sb-chunks") is built and
// maintained OUTSIDE this codebase for now — see
// deploy/ubuntu1-stack/opensearch/ for the index settings and the bulk
// script that populates it from the `chunks`/`documents` tables. This client
// only reads it.
type OpenSearchClient struct {
	baseURL    string
	index      string
	httpClient *http.Client
}

// NewOpenSearchClient returns a client for the given OpenSearch base URL and
// index. baseURL == "" produces a disabled client (Enabled() == false); it is
// still safe to call Search on it, which returns (nil, nil).
//
// timeout <= 0 falls back to 5s. index == "" falls back to "sb-chunks" (the
// name used by deploy/ubuntu1-stack/opensearch/index-settings.json).
func NewOpenSearchClient(baseURL, index string, timeout time.Duration) *OpenSearchClient {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if index == "" {
		index = "sb-chunks"
	}
	return &OpenSearchClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		index:      index,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// Enabled reports whether an OpenSearch endpoint is configured. A nil
// receiver is treated as disabled so that a zero-value *OpenSearchClient
// (e.g. an unwired field) never panics.
func (c *OpenSearchClient) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// osHit is the subset of an OpenSearch hit this client decodes. It mirrors
// the sb-chunks mapping in deploy/ubuntu1-stack/opensearch/index-settings.json.
// Kind and ChunkIndex are decoded but currently unused by model.SearchResult;
// they stay in the struct so a future caller does not have to touch the
// wire-format decode again.
type osHit struct {
	Score  float64 `json:"_score"`
	Source struct {
		ChunkID    int64   `json:"chunk_id"`
		DocumentID string  `json:"document_id"`
		SourceType string  `json:"source_type"`
		Kind       string  `json:"kind"`
		ChunkIndex int     `json:"chunk_index"`
		OccurredAt *string `json:"occurred_at"`
		Title      string  `json:"title"`
		Content    string  `json:"content"`
	} `json:"_source"`
}

type osSearchResponse struct {
	Hits struct {
		Hits []osHit `json:"hits"`
	} `json:"hits"`
}

// OpenSearch 응답 본문 상한(#286 항목 2).
//
// 오류 본문은 64KB 까지만 읽는다. 필요한 것은 error.type 하나뿐이고, 그 필드는
// 본문 앞쪽에 오지 않을 수도 있지만 잘려서 해석에 실패하면 "unknown" 으로
// 떨어질 뿐이라 안전하다. 성공 본문은 limit×3 개 청크라 정상이면 수백 KB 이므로
// 16MB 는 고장 난(또는 가로챈) 응답이 메모리를 끝없이 쓰지 못하게 하는 상한이다.
const (
	opensearchMaxErrorBodyBytes = 64 << 10
	opensearchMaxResponseBytes  = 16 << 20

	// opensearchMaxErrorDrainBytes 는 오류 본문 64KB 를 읽고 남은 부분을 버리며
	// 읽는 상한이다. net/http 는 본문을 끝까지 읽고 닫아야 keep-alive 연결을
	// 다시 쓴다. 다만 끝없이 읽으면 고장 난 서버의 거대한 본문에 시간을 쓰므로
	// 상한을 두고, 넘으면 그 연결만 버린다(다음 요청은 새 연결).
	opensearchMaxErrorDrainBytes = 1 << 20
)

// opensearchErrorTypeRe 는 오류 문자열에 넣어도 되는 error.type 모양이다.
// OpenSearch 의 예외 이름(query_shard_exception, parse_exception 등)은 소문자와
// 밑줄뿐이다. 이 모양이 아니면 따옴표·개행 같은 주입 문자나 질의 조각일 수
// 있으므로 쓰지 않는다.
var opensearchErrorTypeRe = regexp.MustCompile(`^[a-z_]{1,64}$`)

// opensearchStatusError 는 비-200 응답을 오류로 바꾼다.
//
// 본문을 그대로 오류에 담지 않는 이유: query_shard_exception·parse_exception
// 의 reason·root_cause 에는 질의 조각이 들어가고, 이 오류는 search.Service 가
// slog.Warn 으로 남긴다. 질의는 개인정보가 섞일 수 있는 사용자 입력이므로
// 상태 코드와 정제된 error.type 만 남긴다. 운영 진단에는 이 둘로 충분하고,
// 자세한 사유는 OpenSearch 자체 로그에 있다.
func opensearchStatusError(resp *http.Response) error {
	typ := "unknown"
	b, err := io.ReadAll(io.LimitReader(resp.Body, opensearchMaxErrorBodyBytes))
	// 남은 본문을 상한 안에서 버려 연결 재사용을 지킨다. 내용은 쓰지 않는다.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, opensearchMaxErrorDrainBytes))
	if err == nil {
		var parsed struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(b, &parsed) == nil && len(parsed.Error) > 0 {
			var obj struct {
				Type string `json:"type"`
			}
			// error 가 문자열인 옛 형식은 해석에 실패해 unknown 이 된다.
			if json.Unmarshal(parsed.Error, &obj) == nil && opensearchErrorTypeRe.MatchString(obj.Type) {
				typ = obj.Type
			}
		}
	}
	return fmt.Errorf("opensearch: status %d (error.type=%s)", resp.StatusCode, typ)
}

// Search runs a BM25 multi_match query (content + title, title weighted 2x)
// against the OpenSearch index, applying the SAME include/exclude source-type
// and occurred_at-window semantics every other lane honours — see
// buildRequest. Results are chunk-level hits aggregated to the
// highest-scoring hit per document, sorted by descending score, and
// truncated to limit: the same shape searchChunksFTS/searchChunksVector
// return in search.go, ready to fuse via mergeRRF.
//
// A nil/disabled client, an empty query, or limit <= 0 (normalised to the
// package default) are handled without a request. Every other failure —
// network error, non-2xx response, malformed JSON, an unparsable
// document_id — is returned to the caller. search.Service.Search treats an
// error here as a soft failure: log a warning and drop this lane's
// contribution, never fail the overall request. See the OpenSearch
// integration block in Search() for that behaviour; this method never
// swallows the error itself, so a caller that wants a different policy can
// still see the real error.
func (c *OpenSearchClient) Search(ctx context.Context, q model.SearchQuery, limit int) ([]*model.SearchResult, error) {
	if !c.Enabled() || strings.TrimSpace(q.Query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}

	// Over-fetch for per-document dedup, mirroring the *3 factor
	// searchChunksVector/searchChunksFTS use for the same reason: several
	// chunks of the same document may each independently match the query.
	body, err := json.Marshal(buildOpenSearchRequest(q, limit*3))
	if err != nil {
		return nil, fmt.Errorf("opensearch: marshal request: %w", err)
	}

	reqURL := c.baseURL + "/" + c.index + "/_search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("opensearch: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opensearch: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, opensearchStatusError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, opensearchMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("opensearch: read response: %w", err)
	}
	if len(b) > opensearchMaxResponseBytes {
		return nil, fmt.Errorf("opensearch: response exceeds %d bytes", opensearchMaxResponseBytes)
	}

	var parsed osSearchResponse
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, fmt.Errorf("opensearch: unmarshal response: %w", err)
	}

	type entry struct {
		result *model.SearchResult
		score  float64
	}
	seen := make(map[uuid.UUID]entry, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		docID, perr := uuid.Parse(h.Source.DocumentID)
		if perr != nil {
			continue // malformed row; skip it rather than fail the whole lane
		}
		if prev, ok := seen[docID]; ok && prev.score >= h.Score {
			continue
		}
		seen[docID] = entry{result: opensearchHitToSearchResult(docID, h), score: h.Score}
	}

	out := make([]*model.SearchResult, 0, len(seen))
	for _, e := range seen {
		out = append(out, e.result)
	}
	sortByScore(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// buildOpenSearchRequest translates q's filters into the OpenSearch query
// DSL. It intentionally mirrors applySourceTypeFilters' conflict rule
// (EXCLUDE WINS — a source type named by both filters is dropped, via
// must_not, regardless of what filter/terms includes) and
// SearchQuery.OccurredFrom/OccurredTo's half-open window
// [OccurredFrom, OccurredTo).
//
// Because this client can express both filters directly in the query, the
// window and source-type constraints are enforced SERVER-SIDE, unlike the
// ChunkSearcher lanes (whose interface takes only a query/vector and a
// limit). That is why the OpenSearch lane in search.go needs neither
// chunkLanesEnabled's fail-closed skip nor verifyWindow's post-hoc
// membership check: what OpenSearch returns already satisfies the
// predicate, and an unreachable node returns nothing at all (handled by the
// caller as a soft failure) — the same fail-closed outcome, reached a
// different way.
func buildOpenSearchRequest(q model.SearchQuery, size int) map[string]any {
	must := []map[string]any{
		{
			"multi_match": map[string]any{
				"query":  q.Query,
				"fields": []string{"content", "title^2"},
			},
		},
	}

	var filter []map[string]any
	if q.OccurredFrom != nil || q.OccurredTo != nil {
		rng := map[string]any{}
		if q.OccurredFrom != nil {
			rng["gte"] = q.OccurredFrom.UTC().Format(time.RFC3339)
		}
		if q.OccurredTo != nil {
			rng["lt"] = q.OccurredTo.UTC().Format(time.RFC3339) // half-open, matches OccurredTo's documented semantics
		}
		filter = append(filter, map[string]any{
			"range": map[string]any{"occurred_at": rng},
		})
	}
	if include := q.IncludeSourceTypes(); len(include) > 0 {
		values := make([]string, len(include))
		for i, st := range include {
			values[i] = string(st)
		}
		filter = append(filter, map[string]any{
			"terms": map[string]any{"source_type": values},
		})
	}

	var mustNot []map[string]any
	if len(q.ExcludeSourceTypes) > 0 {
		values := make([]string, len(q.ExcludeSourceTypes))
		for i, st := range q.ExcludeSourceTypes {
			values[i] = string(st)
		}
		mustNot = append(mustNot, map[string]any{
			"terms": map[string]any{"source_type": values},
		})
	}

	boolQuery := map[string]any{"must": must}
	if len(filter) > 0 {
		boolQuery["filter"] = filter
	}
	if len(mustNot) > 0 {
		boolQuery["must_not"] = mustNot
	}

	return map[string]any{
		"size":  size,
		"query": map[string]any{"bool": boolQuery},
	}
}

// opensearchHitToSearchResult converts a decoded hit into a model.SearchResult.
//
// OccurredAt is parsed when present; an unparsable value is treated as absent
// rather than failing the hit — one malformed timestamp should not cost the
// whole document a place in the result set. CollectedAt is left at its zero
// value: the sb-chunks index does not carry it (see index-settings.json), and
// recencyKey (internal/search/sort_recent.go) already treats a result with
// neither OccurredAt nor a non-zero CollectedAt as unplaceable rather than as
// the zero instant, so this degrades a Sort="recent" query exactly the way an
// OccurredAt-less chunk-lane row always has — it sorts last, never first.
func opensearchHitToSearchResult(docID uuid.UUID, h osHit) *model.SearchResult {
	var occurred *time.Time
	if h.Source.OccurredAt != nil {
		if t, err := time.Parse(time.RFC3339, *h.Source.OccurredAt); err == nil {
			occurred = &t
		}
	}
	return &model.SearchResult{
		Document: model.Document{
			ID:         docID,
			SourceType: model.SourceType(h.Source.SourceType),
			Title:      h.Source.Title,
			Content:    h.Source.Content,
			OccurredAt: occurred,
		},
		Score:     h.Score,
		MatchType: "opensearch-bm25",
	}
}
