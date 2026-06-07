// Command scout is the second-brain knowledge scout.
//
// second-brain is an LLM-curated private search engine that ingests knowledge
// from many sources and serves curated results to AI agents. This command
// extends that idea outward: once a day it scouts the public frontier
// (arXiv, Hacker News, GitHub, GeekNews) for research and trends, asks Claude
// to judge how much each item is worth *internalizing* into the second brain,
// and files a GitHub issue for every item that clears the relevance gate.
//
// The output is therefore not a generic news digest but a queue of candidate
// knowledge for the second brain to absorb. Each issue carries a relevance
// score, the axis it advances (RAG/search, Go backend, agent/LLM tooling, or
// broad AI), a Korean summary, and a dedup marker so the same item is never
// filed twice.
//
// Required environment:
//
//	ANTHROPIC_API_KEY   — Claude API key (curation)
//	GITHUB_TOKEN        — token with issues:write (issue creation + dedup)
//	GITHUB_REPOSITORY   — "owner/repo" (provided automatically by Actions)
//
// Optional environment:
//
//	ANTHROPIC_MODEL     — curation model (default "claude-sonnet-4-6")
//	ANTHROPIC_BASE_URL  — API base (default "https://api.anthropic.com")
//	SCOUT_MIN_SCORE     — relevance gate 0-100 (default 70)
//	SCOUT_PER_SOURCE    — candidates fetched per source (default 30)
//
// Flags:
//
//	--dry-run  — fetch + curate + log, but create no issues
//
// Exit codes: 0 success, 1 fatal error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// Candidate is a single item discovered from a source, before curation.
type Candidate struct {
	ID        int       `json:"id"`             // batch-local index, used to map Claude's scores back
	Source    string    `json:"source"`         // arxiv | hn | github | geeknews
	Title     string    `json:"title"`          //
	URL       string    `json:"url"`            // canonical link, also the dedup key
	Snippet   string    `json:"snippet"`        // source-provided abstract/description
	Meta      string    `json:"meta,omitempty"` // human context (points, stars, authors…)
	Published time.Time `json:"-"`
}

// Scored is a Candidate after Claude has judged its internalization value.
type Scored struct {
	ID        int    `json:"id"`
	Score     int    `json:"score"`      // 0-100 internalization value
	Axis      string `json:"axis"`       // which second-brain domain it advances
	ReasonKO  string `json:"reason_ko"`  // why it's worth internalizing (or not)
	SummaryKO string `json:"summary_ko"` // Korean summary of the item
	cand      Candidate
}

// config holds resolved runtime settings.
type config struct {
	anthropicKey   string
	anthropicModel string
	anthropicBase  string
	githubToken    string
	repo           string // owner/repo
	minScore       int
	perSource      int
	dryRun         bool
}

const sourceArxiv, sourceHN, sourceGitHub, sourceGeekNews = "arxiv", "hn", "github", "geeknews"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
	if err := run(); err != nil {
		slog.Error("scout failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	loadDotenv() // best-effort, for local dev; Actions injects env directly

	var dryRun bool
	flag.BoolVar(&dryRun, "dry-run", false, "fetch and curate but create no issues")
	flag.Parse()

	cfg, err := loadConfig(dryRun)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hc := &http.Client{Timeout: 30 * time.Second}

	// 1. Collect candidates from every source. A single source failing must
	//    not abort the whole run — the second brain still learns from the rest.
	var candidates []Candidate
	fetchers := []struct {
		name string
		fn   func(context.Context, *http.Client, int) ([]Candidate, error)
	}{
		{sourceArxiv, fetchArxiv},
		{sourceHN, fetchHackerNews},
		{sourceGitHub, fetchGitHubTrending},
		{sourceGeekNews, fetchGeekNews},
	}
	for _, f := range fetchers {
		items, err := f.fn(ctx, hc, cfg.perSource)
		if err != nil {
			slog.Warn("source fetch failed", "source", f.name, "err", err)
			continue
		}
		slog.Info("fetched", "source", f.name, "count", len(items))
		candidates = append(candidates, items...)
	}
	if len(candidates) == 0 {
		slog.Warn("no candidates fetched; nothing to scout")
		return nil
	}

	// 2. Dedup against knowledge already filed, before spending tokens on it.
	seen, err := existingURLs(ctx, hc, cfg)
	if err != nil {
		return fmt.Errorf("load existing issues: %w", err)
	}
	fresh := candidates[:0]
	for _, c := range candidates {
		if seen[normalizeURL(c.URL)] {
			continue
		}
		fresh = append(fresh, c)
	}
	// Re-index IDs after dedup so they map cleanly onto the curation batch.
	for i := range fresh {
		fresh[i].ID = i
	}
	slog.Info("after dedup", "fresh", len(fresh), "already_known", len(candidates)-len(fresh))
	if len(fresh) == 0 {
		slog.Info("everything already internalized; no new issues")
		return nil
	}

	// 3. Curate: ask Claude how much each item is worth internalizing.
	scored, err := curate(ctx, hc, cfg, fresh)
	if err != nil {
		return fmt.Errorf("curate: %w", err)
	}

	// 4. Gate on the relevance threshold. With no count cap, this is the only
	//    line of defence against issue spam — tune SCOUT_MIN_SCORE as needed.
	var passing []Scored
	for _, s := range scored {
		if s.Score >= cfg.minScore {
			passing = append(passing, s)
		}
	}
	sort.SliceStable(passing, func(i, j int) bool { return passing[i].Score > passing[j].Score })
	slog.Info("curation complete", "scored", len(scored), "passing", len(passing), "min_score", cfg.minScore)

	// 5. File one issue per surviving item.
	if err := ensureLabels(ctx, hc, cfg); err != nil {
		slog.Warn("ensure labels failed (continuing)", "err", err)
	}
	created := 0
	for _, s := range passing {
		if cfg.dryRun {
			slog.Info("[dry-run] would file issue",
				"score", s.Score, "axis", s.Axis, "source", s.cand.Source, "title", s.cand.Title)
			continue
		}
		if err := createIssue(ctx, hc, cfg, s); err != nil {
			slog.Warn("create issue failed", "title", s.cand.Title, "err", err)
			continue
		}
		created++
		slog.Info("filed issue", "score", s.Score, "source", s.cand.Source, "title", s.cand.Title)
		time.Sleep(800 * time.Millisecond) // gentle on the issues API
	}

	slog.Info("scout run done", "filed", created, "dry_run", cfg.dryRun)
	return nil
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func loadConfig(dryRun bool) (config, error) {
	cfg := config{
		anthropicKey:   os.Getenv("ANTHROPIC_API_KEY"),
		anthropicModel: envOr("ANTHROPIC_MODEL", "claude-sonnet-4-6"),
		anthropicBase:  strings.TrimRight(envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "/"),
		githubToken:    os.Getenv("GITHUB_TOKEN"),
		repo:           os.Getenv("GITHUB_REPOSITORY"),
		minScore:       envInt("SCOUT_MIN_SCORE", 70),
		perSource:      envInt("SCOUT_PER_SOURCE", 30),
		dryRun:         dryRun,
	}

	var missing []string
	if cfg.anthropicKey == "" {
		missing = append(missing, "ANTHROPIC_API_KEY")
	}
	if !cfg.dryRun && cfg.githubToken == "" {
		missing = append(missing, "GITHUB_TOKEN")
	}
	if cfg.repo == "" {
		missing = append(missing, "GITHUB_REPOSITORY")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Source: arXiv (Atom API)
// ---------------------------------------------------------------------------

func fetchArxiv(ctx context.Context, hc *http.Client, max int) ([]Candidate, error) {
	q := url.Values{}
	// cs.IR information retrieval, cs.CL computation+language, cs.AI, cs.LG —
	// the corners of arXiv that feed the second brain's own RAG/search/agent work.
	q.Set("search_query", "cat:cs.IR OR cat:cs.CL OR cat:cs.AI OR cat:cs.LG")
	q.Set("sortBy", "submittedDate")
	q.Set("sortOrder", "descending")
	q.Set("max_results", strconv.Itoa(max))
	endpoint := "http://export.arxiv.org/api/query?" + q.Encode()

	body, err := httpGet(ctx, hc, endpoint, nil)
	if err != nil {
		return nil, err
	}

	var feed struct {
		Entries []struct {
			Title     string `xml:"title"`
			Summary   string `xml:"summary"`
			ID        string `xml:"id"`
			Published string `xml:"published"`
			Authors   []struct {
				Name string `xml:"name"`
			} `xml:"author"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("parse atom: %w", err)
	}

	out := make([]Candidate, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		pub, _ := time.Parse(time.RFC3339, e.Published)
		authors := make([]string, 0, len(e.Authors))
		for _, a := range e.Authors {
			authors = append(authors, a.Name)
		}
		out = append(out, Candidate{
			Source:    sourceArxiv,
			Title:     clean(e.Title),
			URL:       strings.TrimSpace(e.ID),
			Snippet:   truncate(clean(e.Summary), 1200),
			Meta:      "authors: " + truncate(strings.Join(authors, ", "), 160),
			Published: pub,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Source: Hacker News (Algolia front page)
// ---------------------------------------------------------------------------

func fetchHackerNews(ctx context.Context, hc *http.Client, max int) ([]Candidate, error) {
	endpoint := "https://hn.algolia.com/api/v1/search?tags=front_page&hitsPerPage=" + strconv.Itoa(max)
	body, err := httpGet(ctx, hc, endpoint, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Hits []struct {
			Title     string `json:"title"`
			URL       string `json:"url"`
			ObjectID  string `json:"objectID"`
			Points    int    `json:"points"`
			NumComm   int    `json:"num_comments"`
			CreatedAt string `json:"created_at"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse hn: %w", err)
	}

	out := make([]Candidate, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		link := h.URL
		if link == "" {
			link = "https://news.ycombinator.com/item?id=" + h.ObjectID
		}
		pub, _ := time.Parse(time.RFC3339, h.CreatedAt)
		out = append(out, Candidate{
			Source:    sourceHN,
			Title:     clean(h.Title),
			URL:       link,
			Snippet:   "",
			Meta:      fmt.Sprintf("%d points, %d comments · discussion: https://news.ycombinator.com/item?id=%s", h.Points, h.NumComm, h.ObjectID),
			Published: pub,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Source: GitHub "trending" (Search API — stable proxy for the unofficial page)
// ---------------------------------------------------------------------------

func fetchGitHubTrending(ctx context.Context, hc *http.Client, max int) ([]Candidate, error) {
	since := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	// Bias toward the second brain's interest space; Claude does the fine filtering.
	q := fmt.Sprintf("(LLM OR RAG OR embeddings OR agent OR \"vector search\" OR retrieval) created:>%s", since)
	v := url.Values{}
	v.Set("q", q)
	v.Set("sort", "stars")
	v.Set("order", "desc")
	v.Set("per_page", strconv.Itoa(max))
	endpoint := "https://api.github.com/search/repositories?" + v.Encode()

	headers := map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		headers["Authorization"] = "Bearer " + tok
	}

	body, err := httpGet(ctx, hc, endpoint, headers)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Items []struct {
			FullName    string `json:"full_name"`
			HTMLURL     string `json:"html_url"`
			Description string `json:"description"`
			Stars       int    `json:"stargazers_count"`
			Language    string `json:"language"`
			CreatedAt   string `json:"created_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse github: %w", err)
	}

	out := make([]Candidate, 0, len(resp.Items))
	for _, it := range resp.Items {
		pub, _ := time.Parse(time.RFC3339, it.CreatedAt)
		out = append(out, Candidate{
			Source:    sourceGitHub,
			Title:     it.FullName,
			URL:       it.HTMLURL,
			Snippet:   clean(it.Description),
			Meta:      fmt.Sprintf("%d stars · %s", it.Stars, it.Language),
			Published: pub,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Source: GeekNews (hada.io feedburner — serves Atom, with RSS fallback)
// ---------------------------------------------------------------------------

func fetchGeekNews(ctx context.Context, hc *http.Client, max int) ([]Candidate, error) {
	const endpoint = "https://feeds.feedburner.com/geeknews-feed"
	body, err := httpGet(ctx, hc, endpoint, map[string]string{"User-Agent": "second-brain-scout/1.0"})
	if err != nil {
		return nil, err
	}

	// Primary: Atom (<feed><entry>…) — what feedburner actually serves for GeekNews.
	var atom struct {
		Entries []struct {
			Title string `xml:"title"`
			Links []struct {
				Rel  string `xml:"rel,attr"`
				Href string `xml:"href,attr"`
			} `xml:"link"`
			Content   string `xml:"content"`
			Summary   string `xml:"summary"`
			Published string `xml:"published"`
			Updated   string `xml:"updated"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &atom); err == nil && len(atom.Entries) > 0 {
		out := make([]Candidate, 0, len(atom.Entries))
		for i, e := range atom.Entries {
			if i >= max {
				break
			}
			link := ""
			for _, l := range e.Links {
				if l.Rel == "alternate" || link == "" {
					link = l.Href
				}
			}
			when := e.Published
			if when == "" {
				when = e.Updated
			}
			pub, _ := time.Parse(time.RFC3339, when)
			desc := e.Content
			if desc == "" {
				desc = e.Summary
			}
			out = append(out, Candidate{
				Source:    sourceGeekNews,
				Title:     clean(e.Title),
				URL:       strings.TrimSpace(link),
				Snippet:   truncate(clean(stripTags(desc)), 800),
				Published: pub,
			})
		}
		return out, nil
	}

	// Fallback: RSS 2.0 (<channel><item>…), in case the feed format changes back.
	var rss struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			PubDate     string `xml:"pubDate"`
		} `xml:"channel>item"`
	}
	if err := xml.Unmarshal(body, &rss); err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}

	out := make([]Candidate, 0, len(rss.Items))
	for i, it := range rss.Items {
		if i >= max {
			break
		}
		pub, _ := time.Parse(time.RFC1123Z, it.PubDate)
		out = append(out, Candidate{
			Source:    sourceGeekNews,
			Title:     clean(it.Title),
			URL:       strings.TrimSpace(it.Link),
			Snippet:   truncate(clean(stripTags(it.Description)), 800),
			Published: pub,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Curation via Claude — the heart of the "second brain" framing
// ---------------------------------------------------------------------------

const curationSystem = `너는 second-brain 프로젝트의 "지식 스카우트(knowledge scout)"다.

second-brain은 Go로 만든 LLM 큐레이션 프라이빗 검색 엔진이다:
- PostgreSQL + pgvector(임베딩) + pg_bigm(한국어 2-gram) 위에서 BM25 전문검색과 벡터검색을 RRF로 융합한 하이브리드 검색
- 검색 결과를 LLM이 재랭킹·요약하는 큐레이션 레이어
- 파일시스템/Slack/GitHub/Google Drive에서 문서를 수집·임베딩하는 수집기
- Claude Code 에이전트/스킬 생태계로 운영

second-brain의 핵심 철학은 "외부 지식을 수집·내재화(internalize)해 스스로를 개선하는 두 번째 뇌"다.
너의 임무는 매일 외부 프런티어(arXiv/HN/GitHub/GeekNews)에서 발견된 후보들이, 이 두 번째 뇌에 내재화할 가치가 얼마나 되는지 평가하는 것이다.

각 후보에 대해 0~100 점수를 매겨라. 평가축(axis)은 아래 중 하나:
- "rag-search": RAG·임베딩·하이브리드 검색·리랭킹·HyDE·청킹 개선
- "go-backend": Go 백엔드/인프라/k8s/Postgres/성능
- "agent-tooling": Claude Code·에이전트·MCP·스킬·LLM 오케스트레이션
- "ai-trend": 위에 직접 안 닿지만 알아둘 가치가 있는 넓은 AI 동향

점수 기준:
- 90+ : 지금 바로 설계/구현에 적용 가능, 직접적 업그레이드
- 70~89 : 명확히 관련, 내재화하면 프로젝트에 도움
- 40~69 : 약하게 관련, 참고 수준
- 0~39 : 무관/홍보/재탕

reason_ko에는 "왜 second-brain에 내재화할(또는 하지 않을) 가치가 있는지"를 프로젝트 관점에서 한 문장으로 써라. 일반적 칭찬 금지, 구체적으로.
summary_ko에는 항목 자체의 핵심을 2~3문장 한국어로 요약하라.

출력은 JSON 배열만. 코드펜스·설명·여는말 금지. 스키마:
[{"id": <int>, "score": <int>, "axis": "<rag-search|go-backend|agent-tooling|ai-trend>", "reason_ko": "<...>", "summary_ko": "<...>"}]
모든 후보를 빠짐없이 포함하라.`

// curateBatchSize bounds how many candidates go into a single Claude request.
// One giant request (90+ items) risks both the HTTP client timeout and
// max_tokens truncation of the JSON reply; small sequential batches are slower
// but reliably complete.
const curateBatchSize = 25

func curate(ctx context.Context, hc *http.Client, cfg config, cands []Candidate) ([]Scored, error) {
	out := make([]Scored, 0, len(cands))
	var firstErr error
	for start := 0; start < len(cands); start += curateBatchSize {
		end := start + curateBatchSize
		if end > len(cands) {
			end = len(cands)
		}
		batch, err := curateBatch(ctx, hc, cfg, cands[start:end])
		if err != nil {
			// One failed batch should not discard the others' verdicts.
			slog.Warn("curation batch failed", "from", start, "to", end, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, batch...)
		slog.Info("curation batch done", "from", start, "to", end, "scored", len(batch))
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func curateBatch(ctx context.Context, hc *http.Client, cfg config, cands []Candidate) ([]Scored, error) {
	payload, err := json.Marshal(cands)
	if err != nil {
		return nil, err
	}
	userMsg := "다음은 오늘 발견된 후보 목록(JSON)이다. 각각을 평가하라.\n\n" + string(payload)

	reqBody, _ := json.Marshal(map[string]any{
		"model":      cfg.anthropicModel,
		"max_tokens": 16384,
		"system":     curationSystem,
		"messages": []map[string]any{
			{"role": "user", "content": userMsg},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.anthropicBase+"/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	llmClient := &http.Client{Timeout: 4 * time.Minute}
	resp, err := llmClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("parse anthropic envelope: %w", err)
	}
	var sb strings.Builder
	for _, c := range msg.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	text := stripFences(sb.String())

	var verdicts []Scored
	if err := json.Unmarshal([]byte(text), &verdicts); err != nil {
		return nil, fmt.Errorf("parse curation json: %w (raw: %s)", err, truncate(text, 300))
	}

	// Re-attach the original candidate to each verdict via the batch id.
	byID := make(map[int]Candidate, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}
	out := make([]Scored, 0, len(verdicts))
	for _, v := range verdicts {
		c, ok := byID[v.ID]
		if !ok {
			continue
		}
		v.cand = c
		out = append(out, v)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// GitHub: dedup, labels, issue creation
// ---------------------------------------------------------------------------

const urlMarkerPrefix = "<!-- scout-url: "

// existingURLs lists every scout-labelled issue (open + closed) and extracts
// the dedup marker from each body, so previously internalized knowledge is
// never filed twice.
func existingURLs(ctx context.Context, hc *http.Client, cfg config) (map[string]bool, error) {
	seen := map[string]bool{}
	for page := 1; page <= 10; page++ { // up to 1000 issues of history
		endpoint := fmt.Sprintf("https://api.github.com/repos/%s/issues?labels=scout&state=all&per_page=100&page=%d", cfg.repo, page)
		body, err := httpGet(ctx, hc, endpoint, ghHeaders(cfg))
		if err != nil {
			return nil, err
		}
		var issues []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(body, &issues); err != nil {
			return nil, err
		}
		if len(issues) == 0 {
			break
		}
		for _, is := range issues {
			if u := extractMarker(is.Body); u != "" {
				seen[normalizeURL(u)] = true
			}
		}
		if len(issues) < 100 {
			break
		}
	}
	return seen, nil
}

func extractMarker(body string) string {
	i := strings.Index(body, urlMarkerPrefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(urlMarkerPrefix):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

func ensureLabels(ctx context.Context, hc *http.Client, cfg config) error {
	labels := []struct{ name, color, desc string }{
		{"scout", "5319e7", "second-brain knowledge scout가 자동 생성한 이슈"},
		{"scout:internalize", "0e8a16", "두 번째 뇌에 내재화할 가치가 있다고 판단된 항목"},
		{"scout:arxiv", "1d76db", "출처: arXiv"},
		{"scout:hn", "d93f0b", "출처: Hacker News"},
		{"scout:github", "333333", "출처: GitHub"},
		{"scout:geeknews", "fbca04", "출처: GeekNews"},
		// Axis labels let downstream triage (auto-dev pre-triage) tell product
		// items (go-backend, rag-search) apart from harness knowledge
		// (agent-tooling, ai-trend) instead of blanket-closing everything.
		{"axis:rag-search", "0052cc", "평가축: RAG·임베딩·하이브리드 검색 (제품)"},
		{"axis:go-backend", "006b75", "평가축: Go 백엔드/인프라/Postgres (제품)"},
		{"axis:agent-tooling", "bfd4f2", "평가축: 에이전트/스킬/LLM 오케스트레이션 (하네스)"},
		{"axis:ai-trend", "c5def5", "평가축: 넓은 AI 동향 (참고)"},
	}
	var firstErr error
	for _, l := range labels {
		b, _ := json.Marshal(map[string]string{"name": l.name, "color": l.color, "description": l.desc})
		_, status, err := httpDo(ctx, hc, http.MethodPost,
			fmt.Sprintf("https://api.github.com/repos/%s/labels", cfg.repo), ghHeaders(cfg), b)
		// 201 created, 422 already exists — both fine.
		if err != nil && status != http.StatusUnprocessableEntity && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

var sourceLabel = map[string]string{
	sourceArxiv:    "scout:arxiv",
	sourceHN:       "scout:hn",
	sourceGitHub:   "scout:github",
	sourceGeekNews: "scout:geeknews",
}

// validAxes guards against the model inventing axis values; only known axes
// become labels so the label namespace stays bounded.
var validAxes = map[string]bool{
	"rag-search":    true,
	"go-backend":    true,
	"agent-tooling": true,
	"ai-trend":      true,
}

func createIssue(ctx context.Context, hc *http.Client, cfg config, s Scored) error {
	title := fmt.Sprintf("[%s] %s", s.cand.Source, truncate(s.cand.Title, 140))
	body := renderIssueBody(s)
	labels := []string{"scout", "scout:internalize"}
	if l, ok := sourceLabel[s.cand.Source]; ok {
		labels = append(labels, l)
	}
	if validAxes[s.Axis] {
		labels = append(labels, "axis:"+s.Axis)
	}
	b, _ := json.Marshal(map[string]any{"title": title, "body": body, "labels": labels})
	_, status, err := httpDo(ctx, hc, http.MethodPost,
		fmt.Sprintf("https://api.github.com/repos/%s/issues", cfg.repo), ghHeaders(cfg), b)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("unexpected status %d", status)
	}
	return nil
}

var kst = time.FixedZone("KST", 9*3600)

func renderIssueBody(s Scored) string {
	var b strings.Builder
	fmt.Fprintf(&b, "> **내재화 가치 %d/100** · 분류 `%s` · 출처 `%s`\n\n", s.Score, s.Axis, s.cand.Source)
	fmt.Fprintf(&b, "**왜 두 번째 뇌에 들일 가치가 있나**\n%s\n\n", strings.TrimSpace(s.ReasonKO))
	fmt.Fprintf(&b, "### 요약\n%s\n\n", strings.TrimSpace(s.SummaryKO))
	fmt.Fprintf(&b, "### 원문\n%s\n\n", s.cand.URL)
	if s.cand.Meta != "" {
		fmt.Fprintf(&b, "### 메타\n%s\n\n", s.cand.Meta)
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "- 발견: %s (KST)\n", time.Now().In(kst).Format("2006-01-02 15:04"))
	b.WriteString("- 자동 생성: second-brain knowledge scout (`cmd/scout`)\n\n")
	// Dedup marker — keep on its own line, exact format parsed by extractMarker.
	fmt.Fprintf(&b, "%s%s -->\n", urlMarkerPrefix, s.cand.URL)
	return b.String()
}

// ---------------------------------------------------------------------------
// HTTP + small helpers
// ---------------------------------------------------------------------------

func ghHeaders(cfg config) map[string]string {
	return map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
		"Authorization":        "Bearer " + cfg.githubToken,
		"Content-Type":         "application/json",
	}
}

func httpGet(ctx context.Context, hc *http.Client, endpoint string, headers map[string]string) ([]byte, error) {
	body, status, err := httpDo(ctx, hc, http.MethodGet, endpoint, headers, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("GET %s -> %d: %s", endpoint, status, truncate(string(body), 300))
	}
	return body, nil
}

func httpDo(ctx context.Context, hc *http.Client, method, endpoint string, headers map[string]string, payload []byte) ([]byte, int, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func normalizeURL(u string) string {
	u = strings.TrimSpace(strings.ToLower(u))
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	return u
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// stripTags removes HTML tags (best-effort) so feed HTML bodies become plain
// text snippets. Not a sanitizer — output is only fed to the curation prompt.
func stripTags(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// loadDotenv is a tiny, dependency-free .env loader for local runs. It never
// overrides variables already present in the environment, and silently does
// nothing if no .env file exists (the normal case in CI).
func loadDotenv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

var _ = errors.New // reserved for future typed errors
