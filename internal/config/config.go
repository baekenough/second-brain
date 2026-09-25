package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	Port        string
	DatabaseURL string

	// Embedding (optional — vector search disabled when EMBEDDING_API_KEY and
	// CLIPROXY_AUTH_FILE are both empty; FTS remains the graceful fallback).
	//
	// Routing decision (issue #34): embeddings use OpenAI directly via a
	// dedicated sk- key (EMBEDDING_API_KEY).  cliproxy is chat-only — it
	// returns 404 on /v1/embeddings and is therefore NOT suitable for this
	// path.  Setting EMBEDDING_API_KEY disables CLIPROXY_AUTH_FILE for the
	// embedding path (apiKey takes priority).
	//
	// Token resolution order:
	//   1. EMBEDDING_API_KEY non-empty → static Bearer token (OpenAI direct)
	//   2. CLIPROXY_AUTH_FILE non-empty → CliProxyAPI OAuth token (legacy; chat proxies only)
	//   3. Both empty → disabled (FTS-only mode, no embeddings generated)
	//
	// Default EMBEDDING_API_URL: https://api.openai.com/v1
	// Default EMBEDDING_MODEL:   text-embedding-3-small
	// Default EMBEDDING_DIM:     1536 (matches text-embedding-3-small output)
	EmbeddingAPIURL string
	// EmbeddingAPIKey is a dedicated OpenAI API key (EMBEDDING_API_KEY env var).
	// Use a separate key from any chat/LLM key so embedding costs are tracked
	// independently and the key can be rotated without affecting chat traffic.
	EmbeddingAPIKey string
	EmbeddingModel  string
	EmbeddingDim    int // EMBEDDING_DIM — vector dimension; must match the model output. Default 1536.
	// EmbeddingDimensions 는 임베딩 요청에 실어 보내는 OpenAI `dimensions`
	// 파라미터다(EMBEDDING_DIMENSIONS 환경변수, 기본 1536).
	//
	// EmbeddingDim 과 구분할 것: EmbeddingDim 은 "DB pgvector 컬럼이 몇 차원
	// 인가"이고, 이 값은 "API 에게 몇 차원으로 줄여 달라고 요청하는가"다. 둘을
	// 나눠 둔 덕분에 text-embedding-3-large 처럼 원래 3072 차원인 모델을
	// 1536 차원으로 받아서, 컬럼 차원 변경(=전체 인덱스 재생성)과 모델 교체를
	// 분리할 수 있다. 0 이면 요청에 dimensions 필드를 싣지 않는다(모델 기본값).
	//
	// Matryoshka 표현 학습을 지원하는 text-embedding-3 계열에서만 유효하다.
	// 그 외 모델에는 전송하지 않는다(internal/search/embed.go 참고).
	EmbeddingDimensions int
	// EmbeddingReembedEnabled 는 "현재 설정과 다른 버전으로 만들어진 벡터를
	// 다시 임베딩할지" 스위치다(EMBEDDING_REEMBED_ENABLED, 기본 false).
	//
	// 기본을 꺼 두는 이유: 켜는 순간 전체 코퍼스(문서 4.6만 건, 청크 8.2만 건)
	// 를 다시 임베딩하므로 비용이 든다. 모델이나 임베딩 입력 구성을 바꾼 뒤
	// 의도적으로 켜고, 재임베딩이 끝나면 다시 끄는 운영 플래그다.
	EmbeddingReembedEnabled bool
	CliProxyAuthFile        string // CLIPROXY_AUTH_FILE — CliProxyAPI OAuth JSON path (chat proxies only; NOT used for embeddings when EMBEDDING_API_KEY is set)

	// EmbeddingProvider selects the embedding backend (EMBEDDING_PROVIDER env var).
	// Valid values: "openai" (default), "local" (Ollama-compatible).
	EmbeddingProvider string

	// Local embedding (Ollama-compatible) — used when EMBEDDING_PROVIDER=local.
	//
	// LOCAL_EMBEDDING_MODEL:    Ollama model name (default "bge-m3").
	// LOCAL_EMBEDDING_ENDPOINT: Ollama base URL (no default).
	//                           When empty the local embedder is disabled even if
	//                           EMBEDDING_PROVIDER=local (a warning is logged).
	LocalEmbeddingModel    string
	LocalEmbeddingEndpoint string

	// LLM (optional — Discord RAG answer generation; falls back to EmbeddingAPIURL when unset)
	// LLMAPIURL: LLM_API_URL env var; defaults to EmbeddingAPIURL with /embeddings → /chat/completions suffix fix.
	// LLMAPIKey: LLM_API_KEY env var; defaults to EmbeddingAPIKey.
	// LLMAuthFile: LLM_CLIPROXY_AUTH_FILE env var; defaults to CLIPROXY_AUTH_FILE when unset.
	// LLMTimeoutSeconds: LLM_TIMEOUT_SECONDS env var; per-request HTTP timeout for LLM calls.
	//   Default 120 s (generous for local CPU inference). Set higher for slow models
	//   (e.g. gemma3:4b on Mac mini CPU). Setting 0 falls back to the default.
	LLMAPIURL         string
	LLMAPIKey         string
	LLMAuthFile       string // path to CliProxyAPI OAuth JSON for LLM requests
	LLMModel          string
	LLMMaxTokens      int
	LLMTemperature    float64
	LLMTimeoutSeconds int // LLM_TIMEOUT_SECONDS — HTTP client timeout; default 120
	// LLMThinking: LLM_THINKING env var — "disabled" (default) or "enabled".
	// Reasoning models bill reasoning_content against max_tokens; on long
	// reasoning the budget is spent before any visible content is produced
	// and the response comes back empty with finish_reason="length". Both
	// consumers of the client (summarizer, extraction worker) do mechanical
	// structured-output work, so reasoning is off by default. Any value other
	// than "enabled" is treated as "disabled".
	LLMThinking string

	// Slack (optional)
	SlackBotToken string
	SlackTeamID   string

	// Discord (optional)
	DiscordBotToken               string
	DiscordApplicationID          string
	DiscordGuildIDs               []string
	DiscordCollectInterval        time.Duration
	DiscordMentionResponseEnabled bool

	// GitHub (optional)
	GitHubToken string
	GitHubOrg   string

	// Google Drive (optional)
	GDriveCredentialsJSON string

	// Notion (optional)
	NotionToken string

	// Telegram (optional)
	TelegramBotToken string
	TelegramChatIDs  []int64

	// UserEmailAddresses lists the account owner's own email addresses
	// (comma-separated in USER_EMAIL_ADDRESSES). Used by Part A's structural
	// "awaiting my reply" signal (spec §7.1) and by the extraction worker's
	// counterpart resolution for the same kind: gmail carries no `direction`
	// metadata key (unlike sms/call), only a `from` address, so direction
	// must be inferred by comparing `from` against the account's own
	// address(es). A list (not a single string) accommodates aliases on the
	// same Gmail account.
	UserEmailAddresses []string

	// Reranker (optional — cross-encoder post-retrieval reranking disabled when empty)
	RerankURL    string // RERANKER_URL — Jina-compatible /rerank endpoint base URL
	RerankAPIKey string // RERANKER_API_KEY — Bearer token for the reranker API
	RerankModel  string // RERANKER_MODEL — model identifier sent in the request body

	// RerankDefault decides whether internal callers (/ask, MCP search) opt
	// into cross-encoder reranking (model.SearchQuery.UseRerank) unless the
	// caller explicitly overrides it. Default true, matching the
	// SUMMARIZER_BACKFILL_ENABLED convention (unset/anything-but-false/"0" is
	// on). This flag is inert whenever RerankURL is empty: search.HTTPReranker
	// .Enabled() gates on the endpoint, not on this flag, so flipping
	// RerankDefault on without a configured RERANKER_URL is a no-op rather
	// than an error.
	RerankDefault bool // SEARCH_RERANK_DEFAULT, default true

	// RerankTopN bounds how many candidates are sent to the reranker per
	// request (search.NewHTTPReranker's topN argument). Default 10.
	RerankTopN int // RERANKER_TOP_N, default 10

	// SearchTuning 은 검색 실험용 노브다. 전부 기본값이면 현행 동작이며,
	// 그것이 아무 SEARCH_* 노브도 설정하지 않은 배포의 상태다.
	//
	//	SEARCH_RERANK_OVERFETCH      리랭크 시 후보 풀 하한 (기본 0 = 현행)
	//	SEARCH_MERGE_MODE            asymmetric(기본) | symmetric
	//	SEARCH_RERANK_BLEND          replace(기본) | rrf
	//	SEARCH_RERANK_BLEND_WEIGHT   rrf 합산에서 리랭커 항 가중치 (기본 1.0)
	//	SEARCH_RERANK_INPUT          head(기본) | best_chunk
	//	SEARCH_RECENCY_HALFLIFE_DAYS 최신성 감쇠 반감기(일), 기본 0 = 끔
	//	SEARCH_RECENCY_ALPHA         감쇠 최대 강도 (기본 0.3)
	//
	// 파싱은 model.EnvSearchTuning 한 곳에만 있다. search.NewService 도 같은
	// 함수를 부르므로, 이 필드와 실제 검색 동작이 갈라질 수 없다 — 설정
	// 구조체와 실행 경로가 각자 환경변수를 읽던 방식이 기본값 드리프트를
	// 만들어 온 전례가 있다.
	SearchTuning model.SearchTuning

	// OpenSearch (optional — BM25 full-text lane with Korean morphological
	// (nori) tokenization, DISABLED when OPENSEARCH_URL is empty; this is
	// the default and the only state this repo deploys today).
	//
	// Why: the existing Postgres FTS lane is pg_bigm (bigram), which has no
	// notion of a morpheme boundary — "회의" matches inside "사회의"/"기회의".
	// Measured against ubuntu1 production data, pg_bigm returned 4.7x more
	// "회의" hits than an exact morpheme match (865 vs. 184). The excess is
	// substring noise that dilutes the candidate pool before reranking ever
	// sees it. See internal/search/opensearch.go for the full comparison and
	// the fusion strategy (this lane is purely additive, RRF-fused alongside
	// the existing chunk lanes — it never replaces pg_bigm or pgvector).
	//
	// OPENSEARCH_URL: base URL of the OpenSearch node (e.g.
	// http://localhost:9200). Empty disables the lane entirely — no client
	// is constructed and search.Service behaves exactly as it did before
	// this feature existed (see search.NewOpenSearchLane).
	// OPENSEARCH_INDEX: index name. Default "sb-chunks" (the name used by
	// deploy/ubuntu1-stack/opensearch/index-settings.json).
	// OPENSEARCH_TIMEOUT_SECONDS: per-request HTTP timeout. Default 5 — this
	// lane is one signal among several, so a slow/unreachable node should
	// degrade the search fast rather than hold up the whole request.
	OpensearchURL            string
	OpensearchIndex          string
	OpensearchTimeoutSeconds int

	// Alerting (optional — Slack/Discord webhook for eval regression alerts)
	AlertWebhookURL string // ALERT_WEBHOOK_URL — Slack-compatible incoming webhook URL

	// API authentication (optional — disabled when empty, for dev backward compat)
	APIKey string // API_KEY — Bearer token required for /api/v1/* routes

	// Filesystem (optional)
	FilesystemPath        string   // FILESYSTEM_PATH — directory to scan
	FilesystemEnabled     bool     // FILESYSTEM_ENABLED — default false
	FilesystemExcludeDirs []string // FILESYSTEM_EXCLUDE_DIRS — comma-separated dir names to skip (merged with built-in defaults)
	FilesystemExcludeExts []string // FILESYSTEM_EXCLUDE_EXTS — comma-separated file extensions to skip (merged with built-in defaults)

	// Secretary SQLite (optional — disabled when empty)
	SecretaryDBPath string // SECRETARY_DB_PATH — path to secretary.db (e.g. /data/secretary.db)

	// LLM Memory SQLite (optional — disabled when empty)
	LLMMemoryDBPath string // LLM_MEMORY_DB_PATH — path to llm-memory.sqlite (e.g. /data/llm-memory.sqlite)

	// Gmail (optional — disabled when both credential fields are empty)
	// GMAIL_CREDENTIALS_JSON: OAuth2 client credentials JSON string (from Google Cloud Console)
	// GMAIL_TOKEN_JSON: OAuth2 access/refresh token JSON string
	// GMAIL_QUERY: Gmail search query (default: "-in:spam -in:trash")
	// GMAIL_MAX_MESSAGES: per-Collect cap on total message IDs fetched (default: 50000).
	// Set 0 to disable the cap entirely (no limit). Invalid values use the default.
	GmailCredentialsJSON string
	GmailTokenJSON       string
	GmailQuery           string
	GmailMaxMessages     int

	// Calendar (optional — disabled when both credential fields are empty)
	// CALENDAR_CREDENTIALS_JSON: OAuth2 client credentials JSON string
	// CALENDAR_TOKEN_JSON: OAuth2 access/refresh token JSON string
	// CALENDAR_ID: comma-separated calendar identifiers to collect from
	// (e.g. "primary,team@example.com"). A single value behaves exactly as
	// before — this field was extended rather than adding a new env var to
	// keep deploy config changes minimal. Whitespace is trimmed, empty
	// entries and duplicates are dropped (see collector.parseCalendarIDs).
	// The first entry keeps the pre-multi-calendar source_id scheme so
	// existing documents are never re-keyed on upgrade; see
	// internal/collector/calendar.go calendarEventToDocument.
	// CALENDAR_LOOKAHEAD_DAYS: days into the future to collect (default: 90)
	// CALENDAR_LOOKBEHIND_DAYS: days into the past to collect (default: 365)
	CalendarCredentialsJSON string
	CalendarTokenJSON       string
	CalendarID              string
	CalendarLookaheadDays   int
	CalendarLookbehindDays  int

	// SMS + Call Log (optional — disabled when SMSSourceDir is empty)
	// SMS_SOURCE_DIR: directory containing SMS Backup & Restore XML exports
	// (sms-*.xml and calls-*.xml; latest mtime per prefix is used)
	// SMS_MAX_FILE_BYTES: per-file size cap for OOM guard (bytes, int64).
	// Default 1 GiB. Set 0 to disable the cap entirely (no limit).
	SMSSourceDir    string
	SMSMaxFileBytes int64

	// Whisper transcription (optional — disabled when WhisperAPIKey is empty)
	// WHISPER_API_KEY: OpenAI (or compatible) API key
	// WHISPER_API_URL: base URL (default: "https://api.openai.com/v1")
	// WHISPER_AUDIO_DIR: directory containing audio files to transcribe
	// WHISPER_MODEL: model identifier (default: "gpt-4o-transcribe-diarize" —
	// OpenAI's combined transcription + speaker-diarization model, which
	// performs transcription AND diarization in a single API call; see
	// collector.modelSupportsNativeDiarization). This reverses the local-only
	// whisper.cpp default that predated the #100 policy exception below — a
	// local whisper.cpp deployment must now set WHISPER_MODEL=whisper-1 (or
	// similar) explicitly.
	// WHISPER_LANGUAGE: BCP-47 language hint (default: "ko")
	// WHISPER_MAX_FILE_BYTES: per-file size cap (bytes, int64, default: 100 MiB).
	// Set 0 to disable the cap entirely (no limit). Invalid values use the default.
	// This is a LOCAL disk/network-usage guard, distinct from the fixed 25 MiB
	// hard limit OpenAI's hosted API enforces server-side (whisperCloudMaxFileBytes
	// in collector/whisper.go, applied only when the resolved endpoint is
	// non-local; not configurable, since it reflects a third-party constraint,
	// not an operational preference).
	// WHISPER_HTTP_TIMEOUT: per-request HTTP timeout for transcription calls
	// (Go duration string, default: "2h"). Raise for long audio files that exceed
	// the previous hardcoded 10-minute limit. Invalid values use the 2h default.
	// A zero duration (e.g. "0") falls back to the 2h default so a misconfigured
	// value never produces an infinite (zero) timeout.
	// WHISPER_CHUNKING_STRATEGY: value sent as the chunking_strategy multipart
	// field, ONLY when the resolved model supports native diarization (default:
	// "auto"). Sent UNCONDITIONALLY of DIARIZATION_ENABLED for such models — see
	// DiarizationEnabled doc comment below for why. Required by the live
	// gpt-4o-transcribe-diarize API for the response to contain a "segments"
	// array at all: verified against the real API — omitting it yields a
	// text-only response and the model degenerates into repeating text. Set to
	// the empty string to omit the field entirely (escape hatch for future API
	// changes; not recommended for the default native-diarize model).
	// WHISPER_CLOUD_ALLOWED: set "true" to acknowledge that call/recording audio
	// is intentionally sent to a non-local (cloud) transcription endpoint. This
	// is the policy-exception flag for issue #100, which originally mandated
	// call transcription stay on a local whisper.cpp server — a mandate this
	// collector's cloud-by-default configuration deliberately reverses. Setting
	// this to true only changes the log level of the cloud-endpoint guard in
	// WhisperCollector.CollectStream (Info instead of Warn); it never blocks or
	// gates the request — the request is sent regardless either way. Default
	// false.
	WhisperAPIKey           string
	WhisperAPIURL           string
	WhisperAudioDir         string
	WhisperModel            string
	WhisperLanguage         string
	WhisperMaxFileBytes     int64
	WhisperHTTPTimeout      time.Duration
	WhisperChunkingStrategy string
	WhisperCloudAllowed     bool
	// WhisperConcurrency is the number of audio files transcribed in parallel by
	// the whisper collector. Sourced from WHISPER_CONCURRENCY (default 1, clamped
	// to >= 1). Values >1 enable 2-node load balancing across whisper backends
	// (e.g. via a load balancer in front of two whisper containers). Default 1
	// keeps single-node deployments unchanged (sequential behaviour).
	WhisperConcurrency int

	// Speaker diarization (optional — feature-flagged OFF by default).
	//
	// DIARIZATION_ENABLED: set "true" to enable speaker diarization post-processing
	// after each successful Whisper transcription. Default false (no behaviour change).
	//
	// DIARIZATION_API_URL: base URL of the local diarization microservice
	// (e.g. "http://localhost:8765"). Required when DIARIZATION_ENABLED=true.
	// The collector POSTs audio bytes to {DIARIZATION_API_URL}/diarize and
	// receives speaker-segment JSON. When empty, diarization is disabled even
	// if DIARIZATION_ENABLED=true.
	//
	// IMPORTANT: this flag does NOT gate diarization for a native-diarizing
	// model (WhisperModel containing "diarize", e.g. the default
	// gpt-4o-transcribe-diarize) — those models ALWAYS request and use
	// diarized_json output, unconditionally of this flag. Whether call audio
	// leaves the machine is decided entirely by WHISPER_API_URL + WHISPER_MODEL,
	// not by DIARIZATION_ENABLED; gating native diarization on this flag would
	// mean paying the full cloud-transcription cost while silently discarding
	// the speaker labels the model already computed as part of that same
	// request — full privacy cost, zero benefit, and no error to surface it.
	// DiarizationEnabled retains its original meaning ONLY for the legacy local
	// pyannote path (non-native model + DiarizationAPIURL).
	DiarizationEnabled bool   // DIARIZATION_ENABLED — default false
	DiarizationAPIURL  string // DIARIZATION_API_URL — default ""

	// IngestMaxFileBytes is the per-upload file size cap for POST /api/v1/ingest/file
	// and POST /api/v1/ingest/recording.
	// Default 100 MiB. Set INGEST_MAX_FILE_BYTES=0 to disable the cap entirely.
	// Invalid values use the default.
	IngestMaxFileBytes int64

	// IngestRecordingDir is the directory where POST /api/v1/ingest/recording
	// saves uploaded audio files for later transcription by WhisperCollector.
	//
	// Resolution order:
	//   1. INGEST_RECORDING_DIR non-empty → use directly.
	//   2. WHISPER_AUDIO_DIR non-empty    → use WHISPER_AUDIO_DIR/ingest.
	//   3. Both empty                     → recording ingest is disabled.
	//
	// WhisperCollector picks up the saved files on its next scheduled run.
	IngestRecordingDir string

	// IngestMaxBatchMessages is the maximum number of combined SMS + call records
	// accepted in a single POST /api/v1/ingest/messages request.
	// Default 5000. Set INGEST_MAX_BATCH_MESSAGES=0 to use the default.
	// Invalid values use the default.
	IngestMaxBatchMessages int

	// Summarizer
	// SummarizerBackfillEnabled controls whether the SummarizerWorker scans for
	// pre-existing unsummarized documents (WHERE title_summary IS NULL).
	// Default true. Set SUMMARIZER_BACKFILL_ENABLED=false when running a slow
	// local LLM to avoid a flood of LLM calls for the pre-existing backlog.
	SummarizerBackfillEnabled bool // SUMMARIZER_BACKFILL_ENABLED

	// SummarizerBatchSize is the number of documents fetched and processed by
	// SummarizerWorker per tick.
	// SUMMARIZER_BATCH_SIZE: default 50. The previous default of 10 was sized
	// for a local LLM sharing an 8s whole-tick budget; a remote chat-completion
	// API is latency- not CPU-bound, so a larger batch keeps the bounded
	// worker pool (SummarizerConcurrency) saturated for the whole tick
	// interval instead of idling after 10 documents. Invalid values use the default.
	SummarizerBatchSize int // SUMMARIZER_BATCH_SIZE

	// SummarizerInterval controls how often SummarizerWorker polls for
	// unsummarized documents.
	// SUMMARIZER_INTERVAL: Go duration string, default "30s". The previous 5m
	// default assumed a slow local LLM where polling more often was pointless;
	// a remote API can clear a full batch in well under 30s, so a short
	// interval keeps backlog throughput high without busy-looping
	// (ListUnsummarized is a cheap indexed SELECT once caught up).
	// Invalid values use the default.
	SummarizerInterval time.Duration // SUMMARIZER_INTERVAL

	// SummarizerDocTimeout bounds a single document's summarization work
	// (LLM call + optional embed + UpdateSummary write).
	// SUMMARIZER_DOC_TIMEOUT: Go duration string, default "30s". Replaces the
	// old whole-tick ceiling (formerly 8s, tuned for gemma3:4b on local CPU),
	// which silently truncated a multi-document batch once the LLM backend
	// became a remote API. Each document is independently idempotent
	// (UpdateSummary's WHERE title_summary IS NULL guard), so a timed-out
	// document simply stays unsummarized and is retried on a later tick.
	// NOTE: cmd/collector's shutdown drain window is derived from this value —
	// raising it also raises the drain timeout. Invalid values use the default.
	SummarizerDocTimeout time.Duration // SUMMARIZER_DOC_TIMEOUT

	// SummarizerConcurrency is the number of documents SummarizerWorker
	// processes in parallel within a single tick, via a bounded worker pool.
	// SUMMARIZER_CONCURRENCY: default 5. A remote LLM call is latency-bound,
	// not CPU-bound, so running several requests concurrently is the primary
	// throughput lever against a remote backend; kept conservative to avoid
	// tripping a remote provider's rate limit. Clamped to >= 1. Invalid
	// values use the default.
	SummarizerConcurrency int // SUMMARIZER_CONCURRENCY

	// Freshness monitoring (issue #159)
	//
	// SMSFreshnessMaxAge is the maximum time since the most recent active SMS
	// document was created before an alert fires. SMS documents arrive via the
	// Android push app and are NOT tracked in collection_log, so a document-level
	// freshness check is the only reliable way to detect silent push failures.
	//
	// SMS_FRESHNESS_MAX_AGE: Go duration string (default: "24h").
	// Invalid values are ignored and the default is used.
	SMSFreshnessMaxAge time.Duration // SMS_FRESHNESS_MAX_AGE

	// FreshnessCheckInterval controls how often FreshnessChecker.Check() runs in
	// the collector daemon.
	//
	// FRESHNESS_CHECK_INTERVAL: Go duration string (default: "1h").
	// Invalid values are ignored and the default is used.
	FreshnessCheckInterval time.Duration // FRESHNESS_CHECK_INTERVAL

	// RetiredSources is the list of source type strings to exclude from
	// collection_log freshness alerts. Use this for decommissioned collectors
	// whose historical rows remain in collection_log but whose last_success
	// timestamp is permanently frozen (#161).
	//
	// RETIRED_SOURCES: comma-separated source type strings (default: "secretary").
	// Example: RETIRED_SOURCES=secretary,old-source
	RetiredSources []string // RETIRED_SOURCES

	// Scheduler
	CollectInterval time.Duration
	// CollectIntervalPerSource holds per-source overrides for the global
	// CollectInterval. Keys are the collector Name() strings; values are the
	// desired intervals. Populated from COLLECT_INTERVAL_<NAME> env vars where
	// <NAME> is the upper-cased, underscore-normalised collector name
	// (e.g. COLLECT_INTERVAL_WHISPER, COLLECT_INTERVAL_FILESYSTEM).
	// Only positive durations are stored; invalid or zero values are ignored.
	CollectIntervalPerSource map[string]time.Duration

	// DeletionRatioOverride bypasses the 50% deletion-ratio guard when true.
	// Controlled by DELETION_RATIO_OVERRIDE=true. Use only for legitimate large
	// one-off deletions (see scheduler.WithDeletionRatioOverride for trade-offs).
	DeletionRatioOverride bool

	// CollectorInstance is the per-host identifier used to key the
	// collector_state watermark table. Defaults to os.Hostname() (or
	// "default" when that fails) when COLLECTOR_INSTANCE is unset.
	CollectorInstance string

	// PII redaction (issues #163/#165/#167) and phone-number hashing (issue
	// #164) are OFF by default. This is a deliberate policy reversal, not an
	// oversight: second-brain is a single-user personal knowledge base, not a
	// shared or third-party-facing system, and the owner has explicitly asked
	// for both mechanisms disabled — a call transcript reading "[REDACTED]"
	// instead of the actual phone number, or a document searchable only by an
	// opaque hash, is strictly less useful to the person who OWNS that data
	// than the raw text/number would be. The redaction/hashing machinery
	// itself is NOT removed: both flags reactivate the exact prior behaviour
	// (byte-for-byte, per their respective call sites) when set to "true", so
	// this remains available for a future multi-tenant deployment or a
	// compliance requirement without a code revert.
	//
	// Forward-only, by necessity: flipping either flag changes behaviour only
	// for documents ingested AFTER the change. Content stored while
	// PIIRedactionEnabled=false was never redacted — the raw text is exactly
	// what is in the database, and there is nothing to "restore" if the flag
	// is later turned on. Numbers hashed while PIINumberHashingEnabled=true
	// went through smsmap.ShortHash (SHA-256-derived, one-way) before being
	// written to the sidecar/filename/title — that hash cannot be reversed
	// back to the original number, by this codebase or any other, so turning
	// PIINumberHashingEnabled off later does not and cannot recover numbers
	// hashed under the old default. There is no migration in either
	// direction; do not go looking for the old plaintext/original values —
	// they are simply gone for anything ingested before the flag changed.
	//
	// PII_REDACTION_ENABLED: set "true" to redact structured PII (Korean
	// resident-registration numbers, phone numbers, bank-account-shaped digit
	// runs — see smsmap.RedactPII) from whisper call-transcript Content and
	// Title (internal/collector/whisper.go buildDocument). Default false.
	// PIINameRedactionEnabled protects known contacts at ingestion and enables
	// bounded remote API NER before transcript indexing. Forward-only, default false.
	PIINameRedactionEnabled bool
	PIIRedactionEnabled     bool // PII_REDACTION_ENABLED — default false

	// PII_NUMBER_HASHING_ENABLED: set "true" to hash the counterpart phone
	// number (via smsmap.ShortHash) before it is written to:
	//   1. the ingest-recording sidecar (.meta.json "number_hash" field)
	//   2. the uploaded call-recording audio filename ({hash}_{timestamp}.ext)
	//   3. the anonymous-caller Title/Content fallback label ("상대 {hash8}"),
	//      both in smsmap.MapCall and the ingest-recording handler
	//   4. ingest-recording error/warn logs, which are restricted to the
	//      audio file's basename rather than its full (number-bearing) path
	// Default false: all four write the raw number instead, and (additive,
	// not part of the original #164 behaviour) the raw number is also written
	// into the document's Metadata under the "number" key so it is
	// searchable/visible — hashing it away was making it unrecoverable for no
	// benefit to a single-user deployment.
	//
	// Document SourceIDs (dedup/upsert identity, e.g.
	// "call-log:{dateMs}:{numHash}:{durHash}" or "sms:{dateMs}:{addrHash}:{direction}")
	// are NOT affected by this flag in either state — they are always hashed,
	// independent of PII_NUMBER_HASHING_ENABLED. Changing an established
	// SourceID scheme would silently orphan every already-ingested document
	// under a new identity (the exact failure mode #144 was blocked on), so
	// SourceID hashing is out of scope for this flag entirely.
	PIINumberHashingEnabled bool // PII_NUMBER_HASHING_ENABLED — default false

	// CollectorCutover is an optional floor time for IndexAware collectors
	// (SMS, Whisper). When non-zero, the collector will not emit any record
	// whose event time (OccurredAt for SMS/call-log, mtime for Whisper) is
	// before this value — even if the record was never indexed.
	//
	// This prevents re-collecting pre-cutover history that is already covered
	// by the legacy secretary source, while still allowing post-cutover
	// late-arrival recovery via the IndexAware path.
	//
	// Source: COLLECTOR_CUTOVER env var (RFC3339 format).
	// Default: zero time.Time{} = floor DISABLED (no behaviour change).
	CollectorCutover time.Time

	// Ask pipeline (POST /api/v1/ask, ask backend plan Task 5) — all three
	// have sane defaults, so no feature flag: /ask is registered
	// unconditionally, same tier as /api/v1/search.
	//
	// AskTimeoutSeconds bounds the whole request (intent classify + both
	// retrieval calls + LLM synthesis). ASK_TIMEOUT_SECONDS env var,
	// default 60 (mirrors llm.Config.Timeout's own default).
	AskTimeoutSeconds int
	// AskContextTopK is the Limit passed to the 본검색 (main/Observed)
	// search call. ASK_CONTEXT_TOP_K env var, default 8.
	AskContextTopK int
	// AskContextInsightM is the Limit passed to the 인사이트검색 (Inferred)
	// search call — intentionally smaller than AskContextTopK since
	// insight documents are supporting hypotheses, not primary evidence.
	// ASK_CONTEXT_INSIGHT_M env var, default 3.
	AskContextInsightM int

	// LLM observability (OpenTelemetry → self-hosted Langfuse, OTLP/HTTP).
	// All three default to "" (disabled) — internal/telemetry.InitOTel
	// configures a no-op TracerProvider when LangfuseOTLPEndpoint is empty,
	// so an operator who never sets these three env vars sees zero behavior
	// change (see internal/telemetry doc comment).
	//
	// LangfuseOTLPEndpoint is the OTLP *base* URL — InitOTel appends
	// "/v1/traces" itself (verified against a live Langfuse deployment: the
	// bare base path 404s, since Langfuse's own web app owns that route;
	// see internal/telemetry's package doc "Endpoint path contract").
	//
	// For this deployment: "http://100.77.20.12:3300/api/public/otel" (the
	// Tailscale-interface binding of langfuse-web). Do NOT set this to the
	// public https://langfuse.<domain> hostname — that sits behind Cloudflare
	// Access, and a server-to-server OTLP POST with no browser session gets a
	// silent 302-to-login instead of a real response, so every span is
	// dropped with no error anywhere (see InitOTel's doc comment for the
	// full explanation). LANGFUSE_OTLP_ENDPOINT env var.
	LangfuseOTLPEndpoint string
	// LangfusePublicKey / LangfuseSecretKey form the HTTP Basic Auth
	// credentials Langfuse's OTLP receiver expects
	// (Authorization: Basic base64(publicKey:secretKey)).
	// LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY env vars.
	LangfusePublicKey string
	LangfuseSecretKey string

	// Actions & Briefing feature flags (Part C, plan Task 12). Both default
	// false so a fresh deploy exposes neither route until explicitly
	// switched on — the route not existing (404) IS the rollback mechanism,
	// not a branch inside the handler. See cmd/server/main.go wiring.
	//
	// ACTIONS_API_ENABLED: set "true" to register GET /api/v1/actions and
	// POST /api/v1/actions/{identity_key}/status.
	ActionsAPIEnabled bool
	// BRIEFING_ENABLED: set "true" to register GET /api/v1/briefing. Only
	// takes effect when ActionsAPIEnabled is also true (the briefing reads
	// through the actions query path); it is also skipped when the LLM
	// client is not configured, since a briefing without a model has
	// nothing to summarise with. Both conditions are enforced in
	// cmd/server/main.go, not here.
	BriefingEnabled bool
	// BRIEFING_MAX_ACTIONS: caps how many open actions feed a single
	// briefing request. Default 40. Invalid values use the default.
	BriefingMaxActions int

	// BriefingTimeout bounds one GET /api/v1/briefing request (the open-action
	// query plus the LLM call). BRIEFING_TIMEOUT_SECONDS env var, default 120s.
	//
	// It replaces a hardcoded 30s ceiling in internal/api/briefing.go. In
	// production that ceiling was below the actual model latency, so every
	// request hit it and briefing.Generate degraded to the deterministic
	// aggregate: HTTP 200, degraded=true, a single sentence, dropped_count=0.
	// The failure was invisible from the status code alone, which is exactly
	// why the bound must be operator-tunable instead of compiled in.
	//
	// The default deliberately matches LLMTimeoutSeconds' own default (120s):
	// the LLM client's HTTP timeout should be what fails a slow model, not
	// this outer context, so that a timeout is attributed to the model call.
	// Invalid, zero, and negative values use the default.
	BriefingTimeout time.Duration

	// HTTPWriteTimeout is http.Server.WriteTimeout for the API server.
	// HTTP_WRITE_TIMEOUT_SECONDS env var, default 90s.
	//
	// This is a slow-client guard: it bounds how long one connection may hold
	// a response open, so it must stay finite. The default is sized to cover
	// the slowest NON-briefing paths — /ask (AskTimeoutSeconds, default 60s,
	// streamed as SSE) and a cold-start /search whose first query after a
	// container restart has been measured past 30s (issue #195) — with margin.
	// The briefing, which may legitimately run far longer, extends its own
	// write deadline per request instead of forcing this global value up (see
	// internal/api/briefing.go).
	//
	// Invalid, zero, and negative values use the default; there is no
	// "unlimited" setting on purpose.
	HTTPWriteTimeout time.Duration

	// SearchRequestTimeout 은 검색 요청 하나의 검색 서비스 호출(임베딩·DB 레인·
	// 리랭크)에 거는 context 타임아웃이다(#282). SEARCH_REQUEST_TIMEOUT_SECONDS
	// 환경변수, 기본 60초. REST GET/POST /api/v1/search, GraphQL search, MCP
	// search 도구가 쓴다. /api/v1/ask 는 이미 ASK_TIMEOUT_SECONDS 가 검색까지
	// 포함해 요청 전체를 묶고 있어 이 값을 쓰지 않는다.
	//
	// DB 전역이나 커넥션 풀 전체에 statement_timeout 을 걸지 않고 요청 경로에만
	// 거는 이유: 마이그레이션·백필·collector 가 같은 DB 와 풀을 쓰고, 그쪽의
	// 정당하게 긴 문장까지 끊으면 안 된다.
	//
	// 기본 60초의 근거: 이 값은 정상 지연(p50 약 0.5초, 리랭크 포함 수 초)에
	// 맞춘 SLO 가 아니라 폭주 질의를 끊는 상한이다. 재기동 직후 첫 검색은 HNSW
	// 페이지가 캐시에 없어 30초를 넘긴 실측이 있고(#195), HTTPWriteTimeout 기본
	// 90초는 바로 그 콜드 스타트를 살리려고 잡은 값이다. 여기서 30초 이하로 잡으면
	// 배포할 때마다 첫 검색이 504 가 된다. 60초는 /ask 가 같은 검색을 이미 묶고
	// 있는 ASK_TIMEOUT_SECONDS 기본값과 같고, HTTPWriteTimeout(90초)보다 짧아서
	// 연결이 잘리기 전에 504 JSON 을 쓸 여유가 남는다. 워밍업(#195)이 들어가면
	// 운영에서 10초대로 낮추는 것을 권한다.
	//
	// 0·음수·잘못된 값은 기본값으로 대체한다 — 0 은 "즉시 만료"라 모든 검색이
	// 실패하게 되고, 무제한 설정은 의도적으로 두지 않는다.
	SearchRequestTimeout time.Duration

	// SearchMaxConcurrency 는 동시 검색 수 상한 SEARCH_MAX_CONCURRENCY 다
	// (#286 항목 3). 0 은 "자동" — cmd/server 가 DB 풀 크기로 max(1, MaxConns/2)
	// 를 쓴다. 양수는 그 값을 쓰되 MaxConns−1 로 잘린다(비검색 경로에 연결 1개는
	// 반드시 남긴다). 풀 크기를 모르는 여기서는 상한을 자르지 않는다 — 계산은
	// api.SearchConcurrency 한 곳에 있다. 0·음수·잘못된 값은 경고 후 자동이다.
	// 제한을 끄는 설정은 의도적으로 두지 않는다.
	SearchMaxConcurrency int

	// FeedbackEvidenceEnabled gates POST /api/v1/feedback/evidence (Part D).
	// FEEDBACK_EVIDENCE_ENABLED env var, default false.
	//
	// As with ActionsAPIEnabled, "off" means the route is never registered in
	// the router — a 404, not a branch inside the handler — which is the
	// rollback mechanism. The handler in internal/api/feedback_evidence.go
	// re-reads the same variable per request, so both layers must agree on
	// what counts as "on"; envFlag mirrors that handler's truthy set
	// (1/true/yes/on, case-insensitive, surrounding space ignored) rather
	// than the plain == "true" used by older flags.
	FeedbackEvidenceEnabled bool

	// SearchActiveWeightsEnabled makes the promoted row in
	// search_weights_history the default RRF weighting for every search
	// request that does not carry its own (#214).
	// SEARCH_ACTIVE_WEIGHTS_ENABLED env var, default false.
	//
	// Off by default for the same reason ActionsAPIEnabled is: the row is
	// written by a separate process (cmd/tune -promote) after measuring
	// candidates against a holdout set, and a deployment that has not opted in
	// should not start serving a ranking configuration merely because a row
	// appeared in a table. Production currently has no active row at all, so
	// turning this on today is a no-op — which is exactly the state in which a
	// flag should be introduced.
	//
	// It is also the outermost rollback: unset the variable and restart, and
	// the compiled defaults are back whatever the table says. The inner
	// rollback (cmd/tune -rollback) needs no restart, because the row is read
	// once per request — see search.Service.defaultWeights.
	SearchActiveWeightsEnabled bool
}

// Load reads configuration from environment variables and returns a Config.
// Required variables (PORT, DATABASE_URL) fall back to safe defaults for development.
func Load() (*Config, error) {
	interval, err := time.ParseDuration(getenv("COLLECT_INTERVAL", "10m"))
	if err != nil {
		interval = time.Hour
	}

	discordInterval, err := time.ParseDuration(getenv("DISCORD_COLLECT_INTERVAL", "5m"))
	if err != nil {
		discordInterval = 5 * time.Minute
	}

	var discordGuildIDs []string
	if raw := os.Getenv("DISCORD_GUILD_IDS"); raw != "" {
		for _, id := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				discordGuildIDs = append(discordGuildIDs, trimmed)
			}
		}
	}

	var telegramChatIDs []int64
	if raw := os.Getenv("TELEGRAM_CHAT_IDS"); raw != "" {
		for _, id := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
					telegramChatIDs = append(telegramChatIDs, n)
				}
			}
		}
	}

	// LLM config: resolve base URL and API key from dedicated env vars,
	// falling back to the embedding equivalents when not set.
	embeddingAPIURL := getenv("EMBEDDING_API_URL", "https://api.openai.com/v1")
	llmAPIURL := os.Getenv("LLM_API_URL")
	if llmAPIURL == "" {
		// Derive from embedding URL: replace /embeddings suffix with /chat/completions root.
		// Most cliproxy setups expose both under the same base.
		llmAPIURL = strings.TrimSuffix(embeddingAPIURL, "/embeddings")
	}

	embeddingAPIKey := os.Getenv("EMBEDDING_API_KEY")
	llmAPIKey := os.Getenv("LLM_API_KEY")
	if llmAPIKey == "" {
		llmAPIKey = embeddingAPIKey
	}

	// LLM auth file: prefer LLM-specific path, fall back to shared CLIPROXY_AUTH_FILE.
	llmAuthFile := os.Getenv("LLM_CLIPROXY_AUTH_FILE")
	if llmAuthFile == "" {
		llmAuthFile = os.Getenv("CLIPROXY_AUTH_FILE")
	}

	llmMaxTokens := 1500
	if v := os.Getenv("LLM_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			llmMaxTokens = n
		}
	}

	// LLM_TIMEOUT_SECONDS: default 120 s (generous for local CPU inference).
	// Cloud APIs (OpenAI) typically respond well within 60 s; increase when
	// running large local models that take longer to generate tokens.
	llmTimeoutSeconds := 120
	if v := os.Getenv("LLM_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			llmTimeoutSeconds = n
		}
	}

	// LLM_THINKING: "disabled" (default) or "enabled". Anything else falls
	// back to "disabled" with a warning — an unrecognised value must not
	// silently re-enable reasoning and reintroduce empty completions.
	// The literals mirror llm.ThinkingDisabled / llm.ThinkingEnabled; they are
	// repeated rather than imported to keep config free of a dependency on
	// internal/llm.
	llmThinking := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_THINKING")))
	switch llmThinking {
	case "":
		llmThinking = "disabled"
	case "disabled", "enabled":
		// valid
	default:
		slog.Warn("config: LLM_THINKING is invalid; using \"disabled\"",
			"value", llmThinking,
		)
		llmThinking = "disabled"
	}

	// SUMMARIZER_BACKFILL_ENABLED: default true.
	// Set =false to skip the ListUnsummarized scan when running a slow local LLM.
	summarizerBackfill := true
	if v := os.Getenv("SUMMARIZER_BACKFILL_ENABLED"); v == "false" || v == "0" {
		summarizerBackfill = false
	}

	// SEARCH_RERANK_DEFAULT: default true. Set =false to keep /ask and the MCP
	// search tool opted out of cross-encoder reranking by default (individual
	// callers can still override per-request). Mirrors the
	// SUMMARIZER_BACKFILL_ENABLED convention above.
	rerankDefault := true
	if v := os.Getenv("SEARCH_RERANK_DEFAULT"); v == "false" || v == "0" {
		rerankDefault = false
	}

	collectorInstance := os.Getenv("COLLECTOR_INSTANCE")
	if collectorInstance == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			collectorInstance = hn
		} else {
			collectorInstance = "default"
		}
	}

	llmTemperature := 0.3
	if v := os.Getenv("LLM_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			llmTemperature = f
		}
	}

	// EmbeddingDim: default 1536 (text-embedding-3-small).
	// Set EMBEDDING_DIM=384 for multilingual-e5-small-ko or other 384-d models.
	embeddingDim := 1536
	if v := os.Getenv("EMBEDDING_DIM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("config: EMBEDDING_DIM is invalid; using default 1536",
				"value", v,
				"error", err,
			)
		} else {
			embeddingDim = n
		}
	}

	// EmbeddingDimensions: 임베딩 요청의 `dimensions` 파라미터. 기본값은
	// EmbeddingDim 과 같게 둔다 — 모델을 바꾸지 않은 기존 배포에서는
	// text-embedding-3-small 의 native 차원(1536)과 같은 값이 실려 나가므로
	// 벡터가 달라지지 않는다. 0 을 주면 필드 자체를 싣지 않는다.
	embeddingDimensions := embeddingDim
	if v := os.Getenv("EMBEDDING_DIMENSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("config: EMBEDDING_DIMENSIONS is invalid; falling back to EMBEDDING_DIM",
				"value", v,
				"fallback", embeddingDim,
				"error", err,
			)
		} else {
			embeddingDimensions = n
		}
	}
	if embeddingDimensions != 0 && embeddingDimensions != embeddingDim {
		// 요청 차원과 컬럼 차원이 다르면 INSERT 자체가 pgvector 에서 실패한다.
		// 막지는 않되(설정 실수로 프로세스를 죽이지 않는다) 크게 경고한다.
		slog.Warn("config: EMBEDDING_DIMENSIONS differs from EMBEDDING_DIM; stored vectors will not fit the pgvector column",
			"embedding_dimensions", embeddingDimensions,
			"embedding_dim", embeddingDim,
		)
	}

	if os.Getenv("PII_NAME_REDACTION_ENABLED") == "true" && (llmAPIURL == "" || (llmAPIKey == "" && llmAuthFile == "")) {
		return nil, fmt.Errorf("PII_NAME_REDACTION_ENABLED requires a configured approved LLM API")
	}
	return &Config{
		Port:        getenv("PORT", "8080"),
		DatabaseURL: getenv("DATABASE_URL", "postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"),

		EmbeddingAPIURL:     embeddingAPIURL,
		EmbeddingAPIKey:     embeddingAPIKey,
		EmbeddingModel:      getenv("EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingDim:        embeddingDim,
		EmbeddingDimensions: embeddingDimensions,

		EmbeddingReembedEnabled: os.Getenv("EMBEDDING_REEMBED_ENABLED") == "true",

		CliProxyAuthFile: os.Getenv("CLIPROXY_AUTH_FILE"),

		EmbeddingProvider: getenv("EMBEDDING_PROVIDER", "openai"),

		LocalEmbeddingModel:    getenv("LOCAL_EMBEDDING_MODEL", "bge-m3"),
		LocalEmbeddingEndpoint: os.Getenv("LOCAL_EMBEDDING_ENDPOINT"),

		LLMAPIURL:         llmAPIURL,
		LLMAPIKey:         llmAPIKey,
		LLMAuthFile:       llmAuthFile,
		LLMModel:          getenv("LLM_MODEL", "gpt-4o-mini"),
		LLMMaxTokens:      llmMaxTokens,
		LLMTemperature:    llmTemperature,
		LLMTimeoutSeconds: llmTimeoutSeconds,
		LLMThinking:       llmThinking,

		SlackBotToken: os.Getenv("SLACK_BOT_TOKEN"),
		SlackTeamID:   os.Getenv("SLACK_TEAM_ID"),

		DiscordBotToken:               os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordApplicationID:          os.Getenv("DISCORD_APPLICATION_ID"),
		DiscordGuildIDs:               discordGuildIDs,
		DiscordCollectInterval:        discordInterval,
		DiscordMentionResponseEnabled: getenv("DISCORD_MENTION_RESPONSE_ENABLED", "true") == "true",

		GitHubToken: os.Getenv("GITHUB_TOKEN"),
		GitHubOrg:   os.Getenv("GITHUB_ORG"),

		GDriveCredentialsJSON: os.Getenv("GDRIVE_CREDENTIALS_JSON"),

		NotionToken: os.Getenv("NOTION_TOKEN"),

		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatIDs:  telegramChatIDs,

		UserEmailAddresses: splitCSV(os.Getenv("USER_EMAIL_ADDRESSES")),

		RerankURL:     os.Getenv("RERANKER_URL"),
		RerankAPIKey:  os.Getenv("RERANKER_API_KEY"),
		RerankModel:   getenv("RERANKER_MODEL", "jina-reranker-v2-base-multilingual"),
		RerankDefault: rerankDefault,
		RerankTopN:    rerankTopN(),
		SearchTuning:  model.EnvSearchTuning(),

		OpensearchURL:            os.Getenv("OPENSEARCH_URL"),
		OpensearchIndex:          getenv("OPENSEARCH_INDEX", "sb-chunks"),
		OpensearchTimeoutSeconds: opensearchTimeoutSeconds(),

		AlertWebhookURL: os.Getenv("ALERT_WEBHOOK_URL"),

		APIKey: os.Getenv("API_KEY"),

		FilesystemPath:        os.Getenv("FILESYSTEM_PATH"),
		FilesystemEnabled:     os.Getenv("FILESYSTEM_ENABLED") == "true",
		FilesystemExcludeDirs: splitCSV(os.Getenv("FILESYSTEM_EXCLUDE_DIRS")),
		FilesystemExcludeExts: normalizeExts(splitCSV(os.Getenv("FILESYSTEM_EXCLUDE_EXTS"))),

		SecretaryDBPath: os.Getenv("SECRETARY_DB_PATH"),
		LLMMemoryDBPath: os.Getenv("LLM_MEMORY_DB_PATH"),

		GmailCredentialsJSON: os.Getenv("GMAIL_CREDENTIALS_JSON"),
		GmailTokenJSON:       os.Getenv("GMAIL_TOKEN_JSON"),
		GmailQuery:           getenv("GMAIL_QUERY", "-in:spam -in:trash"),
		GmailMaxMessages:     gmailMaxMessages(),

		CalendarCredentialsJSON: os.Getenv("CALENDAR_CREDENTIALS_JSON"),
		CalendarTokenJSON:       os.Getenv("CALENDAR_TOKEN_JSON"),
		CalendarID:              getenv("CALENDAR_ID", "primary"),
		CalendarLookaheadDays:   calendarLookaheadDays(),
		CalendarLookbehindDays:  calendarLookbehindDays(),

		SMSSourceDir:    os.Getenv("SMS_SOURCE_DIR"),
		SMSMaxFileBytes: smsMaxFileBytes(),

		WhisperAPIKey:           os.Getenv("WHISPER_API_KEY"),
		WhisperAPIURL:           getenv("WHISPER_API_URL", "https://api.openai.com/v1"),
		WhisperAudioDir:         os.Getenv("WHISPER_AUDIO_DIR"),
		WhisperModel:            getenv("WHISPER_MODEL", "gpt-4o-transcribe-diarize"),
		WhisperLanguage:         getenv("WHISPER_LANGUAGE", "ko"),
		WhisperMaxFileBytes:     whisperMaxFileBytes(),
		WhisperHTTPTimeout:      whisperHTTPTimeout(),
		WhisperChunkingStrategy: getenv("WHISPER_CHUNKING_STRATEGY", "auto"),
		WhisperCloudAllowed:     os.Getenv("WHISPER_CLOUD_ALLOWED") == "true",
		WhisperConcurrency:      whisperConcurrency(),

		DiarizationEnabled: os.Getenv("DIARIZATION_ENABLED") == "true",
		DiarizationAPIURL:  os.Getenv("DIARIZATION_API_URL"),

		IngestMaxFileBytes: ingestMaxFileBytes(),

		IngestRecordingDir:     resolveIngestRecordingDir(),
		IngestMaxBatchMessages: ingestMaxBatchMessages(),

		SummarizerBackfillEnabled: summarizerBackfill,
		SummarizerBatchSize:       summarizerBatchSize(),
		SummarizerInterval:        summarizerInterval(),
		SummarizerDocTimeout:      summarizerDocTimeout(),
		SummarizerConcurrency:     summarizerConcurrency(),

		SMSFreshnessMaxAge:     smsFreshnessMaxAge(),
		FreshnessCheckInterval: freshnessCheckInterval(),
		RetiredSources:         retiredSources(),

		CollectInterval:          interval,
		CollectIntervalPerSource: collectIntervalPerSource(),
		CollectorInstance:        collectorInstance,
		CollectorCutover:         collectorCutover(),

		// #147 escape hatch: bypasses deletion-ratio guard when set.
		// See Scheduler.WithDeletionRatioOverride for trade-offs.
		DeletionRatioOverride: os.Getenv("DELETION_RATIO_OVERRIDE") == "true",

		// #163/#164/#165/#167 policy reversal: both default false. See the
		// PIIRedactionEnabled / PIINumberHashingEnabled doc comments above.
		PIINameRedactionEnabled: os.Getenv("PII_NAME_REDACTION_ENABLED") == "true",
		PIIRedactionEnabled:     os.Getenv("PII_REDACTION_ENABLED") == "true",
		PIINumberHashingEnabled: os.Getenv("PII_NUMBER_HASHING_ENABLED") == "true",

		AskTimeoutSeconds:  askTimeoutSeconds(),
		AskContextTopK:     askContextTopK(),
		AskContextInsightM: askContextInsightM(),

		// #Langfuse observability — all default "" (disabled); see doc
		// comment on LangfuseOTLPEndpoint above.
		LangfuseOTLPEndpoint: os.Getenv("LANGFUSE_OTLP_ENDPOINT"),
		LangfusePublicKey:    os.Getenv("LANGFUSE_PUBLIC_KEY"),
		LangfuseSecretKey:    os.Getenv("LANGFUSE_SECRET_KEY"),

		// Task 12 feature flags — default false (see doc comments above).
		ActionsAPIEnabled:  os.Getenv("ACTIONS_API_ENABLED") == "true",
		BriefingEnabled:    os.Getenv("BRIEFING_ENABLED") == "true",
		BriefingMaxActions: briefingMaxActions(),
		BriefingTimeout:    BriefingTimeout(),
		HTTPWriteTimeout:   httpWriteTimeout(),

		// #282 검색 요청 타임아웃 — 기본값 근거는 필드 doc comment 참고.
		SearchRequestTimeout: SearchRequestTimeout(),

		// #286 동시 검색 상한 — 0 은 자동(풀 크기 기반), 필드 doc comment 참고.
		SearchMaxConcurrency: searchMaxConcurrency(),

		// Part D feedback collection — default false (see doc comment above).
		FeedbackEvidenceEnabled: envFlag("FEEDBACK_EVIDENCE_ENABLED"),

		// Part D weight serving — default false (see doc comment above).
		SearchActiveWeightsEnabled: envFlag("SEARCH_ACTIVE_WEIGHTS_ENABLED"),
	}, nil
}

// DefaultBriefingTimeout is the fallback for BRIEFING_TIMEOUT_SECONDS.
const DefaultBriefingTimeout = 120 * time.Second

// BriefingTimeout resolves BRIEFING_TIMEOUT_SECONDS from the environment.
//
// Exported, and readable without a *Config, because internal/api's briefing
// handler needs the value but has nowhere to store it: the api.Server fields
// it would live in are owned by another change in flight, and the value is
// read once per briefing request — a low-frequency, cache-protected path — so
// resolving it from the environment costs nothing measurable. Config.Load
// calls this too, so `cfg.BriefingTimeout` and this function can never report
// different numbers.
//
// Invalid, zero, and negative values fall back to DefaultBriefingTimeout: a
// zero timeout would mean "already expired" (context.WithTimeout with a
// non-positive duration cancels immediately), turning a typo into a briefing
// that is permanently degraded.
func BriefingTimeout() time.Duration {
	return timeoutSeconds("BRIEFING_TIMEOUT_SECONDS", DefaultBriefingTimeout)
}

// defaultHTTPWriteTimeout is the fallback for HTTP_WRITE_TIMEOUT_SECONDS.
const defaultHTTPWriteTimeout = 90 * time.Second

// httpWriteTimeout resolves HTTP_WRITE_TIMEOUT_SECONDS from the environment.
// See Config.HTTPWriteTimeout for why this stays finite.
func httpWriteTimeout() time.Duration {
	return timeoutSeconds("HTTP_WRITE_TIMEOUT_SECONDS", defaultHTTPWriteTimeout)
}

// DefaultSearchRequestTimeout 은 SEARCH_REQUEST_TIMEOUT_SECONDS 의 기본값이다.
// 60초로 잡은 근거는 Config.SearchRequestTimeout 의 doc comment 에 있다.
const DefaultSearchRequestTimeout = 60 * time.Second

// SearchRequestTimeout 은 SEARCH_REQUEST_TIMEOUT_SECONDS 를 환경에서 읽는다.
// *Config 없이도 부를 수 있게 공개한 이유는 BriefingTimeout 과 같다 — Load 도
// 이 함수를 불러서 cfg.SearchRequestTimeout 과 값이 어긋날 수 없다.
func SearchRequestTimeout() time.Duration {
	return timeoutSeconds("SEARCH_REQUEST_TIMEOUT_SECONDS", DefaultSearchRequestTimeout)
}

// searchMaxConcurrency 는 SEARCH_MAX_CONCURRENCY 를 읽는다. 비어 있으면 0(자동),
// 양의 정수가 아니면 경고를 남기고 0(자동)이다.
func searchMaxConcurrency() int {
	const key = "SEARCH_MAX_CONCURRENCY"
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: search concurrency is invalid; using automatic value",
			"key", key,
			"value", v,
			"error", err,
		)
		return 0
	}
	return n
}

// timeoutSeconds parses an integer-seconds env var into a duration, logging
// and falling back to def for anything that is not a positive integer.
func timeoutSeconds(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: timeout is invalid; using default",
			"key", key,
			"value", v,
			"default", def.String(),
			"error", err,
		)
		return def
	}
	return time.Duration(n) * time.Second
}

// envFlag reports whether key holds a truthy value. The accepted set matches
// the api package's own per-request gate for FEEDBACK_EVIDENCE_ENABLED, so
// that wiring (this file) and handler (internal/api) cannot disagree about
// whether a feature is on. Anything else — including the empty string and an
// unset variable — is false: a deployment that says nothing about a feature
// does not get it.
func envFlag(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func calendarLookaheadDays() int {
	if v := os.Getenv("CALENDAR_LOOKAHEAD_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 90
}

func calendarLookbehindDays() int {
	if v := os.Getenv("CALENDAR_LOOKBEHIND_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 365
}

// smsMaxFileBytes parses SMS_MAX_FILE_BYTES from the environment.
// Default is 1 GiB (generous for multi-year XML exports).
// Set SMS_MAX_FILE_BYTES=0 to disable the cap entirely (no limit).
// Invalid values are ignored and the default is used.
func smsMaxFileBytes() int64 {
	const defaultCap = 1 << 30 // 1 GiB
	v := os.Getenv("SMS_MAX_FILE_BYTES")
	if v == "" {
		return defaultCap
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		slog.Warn("config: SMS_MAX_FILE_BYTES is invalid; using default 1 GiB",
			"value", v,
			"error", err,
		)
		return defaultCap
	}
	return n // 0 means no limit (caller checks maxFileBytes <= 0)
}

// gmailMaxMessages parses GMAIL_MAX_MESSAGES from the environment.
// Default is 50000 (generous enough to match a large secretary export).
// Set GMAIL_MAX_MESSAGES=0 to disable the cap entirely (no limit).
// Invalid values are ignored and the default is used.
func gmailMaxMessages() int {
	const defaultCap = 50000
	v := os.Getenv("GMAIL_MAX_MESSAGES")
	if v == "" {
		return defaultCap
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		slog.Warn("config: GMAIL_MAX_MESSAGES is invalid; using default 50000",
			"value", v,
			"error", err,
		)
		return defaultCap
	}
	return n // 0 means no limit (caller checks maxMessages <= 0)
}

// whisperMaxFileBytes parses WHISPER_MAX_FILE_BYTES from the environment.
// Default is 100 MiB (covers call recordings in the 28–32 MB range).
// Set WHISPER_MAX_FILE_BYTES=0 to disable the cap entirely (no limit).
// Invalid values are ignored and the default is used.
func whisperMaxFileBytes() int64 {
	const defaultCap = 100 << 20 // 100 MiB
	v := os.Getenv("WHISPER_MAX_FILE_BYTES")
	if v == "" {
		return defaultCap
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		slog.Warn("config: WHISPER_MAX_FILE_BYTES is invalid; using default 100 MiB",
			"value", v,
			"error", err,
		)
		return defaultCap
	}
	return n // 0 means no limit (caller checks maxFileBytes <= 0)
}

// whisperHTTPTimeout parses WHISPER_HTTP_TIMEOUT from the environment.
// Default is 2h (covers long audio files that previously exceeded the old 10m limit).
// Invalid values are ignored and the default is used.
// A zero duration falls back to the default so a misconfigured value never
// produces a zero (= infinite) timeout.
func whisperHTTPTimeout() time.Duration {
	const defaultTimeout = 2 * time.Hour
	v := os.Getenv("WHISPER_HTTP_TIMEOUT")
	if v == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("config: WHISPER_HTTP_TIMEOUT is invalid; using default 2h",
			"value", v,
			"error", err,
		)
		return defaultTimeout
	}
	return d
}

// whisperConcurrency parses WHISPER_CONCURRENCY from the environment.
// Default is 1 (sequential — single-node deployments unchanged).
// The value is clamped to >= 1: zero, negative, or invalid values fall back to 1
// so the collector always has at least one worker.
func whisperConcurrency() int {
	const defaultConcurrency = 1
	v := os.Getenv("WHISPER_CONCURRENCY")
	if v == "" {
		return defaultConcurrency
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		slog.Warn("config: WHISPER_CONCURRENCY is invalid; using default 1",
			"value", v,
			"error", err,
		)
		return defaultConcurrency
	}
	return n
}

// summarizerBatchSize parses SUMMARIZER_BATCH_SIZE from the environment.
// Default is 50 (see SummarizerBatchSize doc comment for rationale).
// Invalid values are ignored and the default is used.
func summarizerBatchSize() int {
	const defaultBatch = 50
	v := os.Getenv("SUMMARIZER_BATCH_SIZE")
	if v == "" {
		return defaultBatch
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: SUMMARIZER_BATCH_SIZE is invalid; using default 50",
			"value", v,
			"error", err,
		)
		return defaultBatch
	}
	return n
}

// summarizerInterval parses SUMMARIZER_INTERVAL from the environment.
// Default is 30s (see SummarizerInterval doc comment for rationale).
// Invalid values are ignored and the default is used.
func summarizerInterval() time.Duration {
	const defaultInterval = 30 * time.Second
	v := os.Getenv("SUMMARIZER_INTERVAL")
	if v == "" {
		return defaultInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("config: SUMMARIZER_INTERVAL is invalid; using default 30s",
			"value", v,
			"error", err,
		)
		return defaultInterval
	}
	return d
}

// summarizerDocTimeout parses SUMMARIZER_DOC_TIMEOUT from the environment.
// Default is 30s (see SummarizerDocTimeout doc comment for rationale).
// Invalid values are ignored and the default is used.
func summarizerDocTimeout() time.Duration {
	const defaultTimeout = 30 * time.Second
	v := os.Getenv("SUMMARIZER_DOC_TIMEOUT")
	if v == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("config: SUMMARIZER_DOC_TIMEOUT is invalid; using default 30s",
			"value", v,
			"error", err,
		)
		return defaultTimeout
	}
	return d
}

// summarizerConcurrency parses SUMMARIZER_CONCURRENCY from the environment.
// Default is 5 (see SummarizerConcurrency doc comment for rationale).
// The value is clamped to >= 1: zero, negative, or invalid values fall back
// to the default so the worker pool always has at least one slot.
func summarizerConcurrency() int {
	const defaultConcurrency = 5
	v := os.Getenv("SUMMARIZER_CONCURRENCY")
	if v == "" {
		return defaultConcurrency
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		slog.Warn("config: SUMMARIZER_CONCURRENCY is invalid; using default 5",
			"value", v,
			"error", err,
		)
		return defaultConcurrency
	}
	return n
}

// ingestMaxFileBytes parses INGEST_MAX_FILE_BYTES from the environment.
// Default is 100 MiB (generous for typical document uploads).
// Set INGEST_MAX_FILE_BYTES=0 to disable the cap entirely (no limit).
// Invalid values are ignored and the default is used.
func ingestMaxFileBytes() int64 {
	const defaultCap = 100 << 20 // 100 MiB
	v := os.Getenv("INGEST_MAX_FILE_BYTES")
	if v == "" {
		return defaultCap
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		slog.Warn("config: INGEST_MAX_FILE_BYTES is invalid; using default 100 MiB",
			"value", v,
			"error", err,
		)
		return defaultCap
	}
	return n // 0 means no limit (caller checks maxFileBytes <= 0)
}

// resolveIngestRecordingDir resolves the directory for POST /api/v1/ingest/recording
// uploads using the three-step resolution order documented on IngestRecordingDir.
func resolveIngestRecordingDir() string {
	if v := os.Getenv("INGEST_RECORDING_DIR"); v != "" {
		return v
	}
	if w := os.Getenv("WHISPER_AUDIO_DIR"); w != "" {
		return filepath.Join(w, "ingest")
	}
	return ""
}

// ingestMaxBatchMessages parses INGEST_MAX_BATCH_MESSAGES from the environment.
// Default is 5000. Set 0 to use the default (not to disable the cap).
// Invalid values use the default.
func ingestMaxBatchMessages() int {
	const defaultCap = 5000
	v := os.Getenv("INGEST_MAX_BATCH_MESSAGES")
	if v == "" {
		return defaultCap
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: INGEST_MAX_BATCH_MESSAGES is invalid; using default 5000",
			"value", v,
			"error", err,
		)
		return defaultCap
	}
	return n
}

// askTimeoutSeconds parses ASK_TIMEOUT_SECONDS from the environment.
// Default is 60 (mirrors llm.Config.Timeout's own default).
// Invalid values are ignored and the default is used.
func askTimeoutSeconds() int {
	const defaultSeconds = 60
	v := os.Getenv("ASK_TIMEOUT_SECONDS")
	if v == "" {
		return defaultSeconds
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: ASK_TIMEOUT_SECONDS is invalid; using default 60",
			"value", v,
			"error", err,
		)
		return defaultSeconds
	}
	return n
}

// opensearchTimeoutSeconds parses OPENSEARCH_TIMEOUT_SECONDS from the
// environment. Default is 5 — the OpenSearch lane is one additive signal
// among several (see internal/search/opensearch.go), so a slow/unreachable
// node should degrade the whole search quickly rather than hold up a request
// that would otherwise have succeeded on the other lanes alone.
// Invalid values are ignored and the default is used.
func opensearchTimeoutSeconds() int {
	const defaultSeconds = 5
	v := os.Getenv("OPENSEARCH_TIMEOUT_SECONDS")
	if v == "" {
		return defaultSeconds
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: OPENSEARCH_TIMEOUT_SECONDS is invalid; using default 5",
			"value", v,
			"error", err,
		)
		return defaultSeconds
	}
	return n
}

// rerankTopN parses RERANKER_TOP_N from the environment: how many candidates
// are sent to the reranker per request (search.NewHTTPReranker's topN
// argument). Default is 10. Invalid values are ignored and the default is
// used.
func rerankTopN() int {
	const defaultTopN = 10
	v := os.Getenv("RERANKER_TOP_N")
	if v == "" {
		return defaultTopN
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: RERANKER_TOP_N is invalid; using default 10",
			"value", v,
			"error", err,
		)
		return defaultTopN
	}
	return n
}

// askContextTopK parses ASK_CONTEXT_TOP_K from the environment: the result
// limit for the 본검색 (Observed) search call. Default is 8.
// Invalid values are ignored and the default is used.
func askContextTopK() int {
	const defaultTopK = 8
	v := os.Getenv("ASK_CONTEXT_TOP_K")
	if v == "" {
		return defaultTopK
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: ASK_CONTEXT_TOP_K is invalid; using default 8",
			"value", v,
			"error", err,
		)
		return defaultTopK
	}
	return n
}

// askContextInsightM parses ASK_CONTEXT_INSIGHT_M from the environment: the
// result limit for the 인사이트검색 (Inferred) search call. Default is 3.
// Invalid values are ignored and the default is used.
func askContextInsightM() int {
	const defaultInsightM = 3
	v := os.Getenv("ASK_CONTEXT_INSIGHT_M")
	if v == "" {
		return defaultInsightM
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: ASK_CONTEXT_INSIGHT_M is invalid; using default 3",
			"value", v,
			"error", err,
		)
		return defaultInsightM
	}
	return n
}

// LoadCollector reads configuration for the collector daemon.
// It excludes server-only fields (PORT, API_KEY).
func LoadCollector() (*Config, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	cfg.Port = ""
	cfg.APIKey = ""
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// splitCSV splits a comma-separated env value into a trimmed, non-empty list.
func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// collectorCutover parses COLLECTOR_CUTOVER (RFC3339) from the environment.
// Returns zero time.Time{} when the variable is unset, empty, or invalid,
// which disables the cutover floor (no behaviour change).
func collectorCutover() time.Time {
	v := os.Getenv("COLLECTOR_CUTOVER")
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		slog.Warn("config: COLLECTOR_CUTOVER is invalid RFC3339; cutover floor disabled",
			"value", v,
			"error", err,
		)
		return time.Time{}
	}
	return t.UTC()
}

// collectIntervalPerSource reads per-source interval overrides from the environment.
//
// For each known collector name N (in its canonical form), it looks for
// COLLECT_INTERVAL_<UPPER_N> where hyphens and spaces are replaced by underscores.
// For example:
//   - COLLECT_INTERVAL_WHISPER    → "whisper"
//   - COLLECT_INTERVAL_FILESYSTEM → "filesystem"
//   - COLLECT_INTERVAL_SMS        → "sms"
//   - COLLECT_INTERVAL_GMAIL      → "gmail"
//
// Only positive, parseable durations are stored; missing or invalid values are
// silently ignored so unrelated collectors fall back to the global COLLECT_INTERVAL.
func collectIntervalPerSource() map[string]time.Duration {
	// Canonical collector names (must match collector.Name() return values).
	knownCollectors := []string{
		"whisper",
		"filesystem",
		"sms",
		"gmail",
		"calendar",
		"slack",
		"discord",
		"github",
		"gdrive",
		"notion",
		"telegram",
		"llm-memory",
	}

	result := make(map[string]time.Duration, len(knownCollectors))
	for _, name := range knownCollectors {
		// Transform "llm-memory" → "LLM_MEMORY" for the env var key.
		envKey := "COLLECT_INTERVAL_" + strings.ToUpper(strings.NewReplacer("-", "_", " ", "_").Replace(name))
		v := os.Getenv(envKey)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			slog.Warn("config: per-source COLLECT_INTERVAL is invalid; using global interval",
				"env", envKey,
				"value", v,
				"error", err,
			)
			continue
		}
		result[name] = d
	}
	return result
}

// smsFreshnessMaxAge parses SMS_FRESHNESS_MAX_AGE from the environment.
// Default is 24h. Invalid values are ignored and the default is used.
func smsFreshnessMaxAge() time.Duration {
	const defaultAge = 24 * time.Hour
	v := os.Getenv("SMS_FRESHNESS_MAX_AGE")
	if v == "" {
		return defaultAge
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("config: SMS_FRESHNESS_MAX_AGE is invalid; using default 24h",
			"value", v,
			"error", err,
		)
		return defaultAge
	}
	return d
}

// freshnessCheckInterval parses FRESHNESS_CHECK_INTERVAL from the environment.
// Default is 1h. Invalid values are ignored and the default is used.
func freshnessCheckInterval() time.Duration {
	const defaultInterval = time.Hour
	v := os.Getenv("FRESHNESS_CHECK_INTERVAL")
	if v == "" {
		return defaultInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("config: FRESHNESS_CHECK_INTERVAL is invalid; using default 1h",
			"value", v,
			"error", err,
		)
		return defaultInterval
	}
	return d
}

// retiredSources returns the list of source type strings to exclude from
// collection_log freshness alerts. Parsed from RETIRED_SOURCES (comma-separated).
// Defaults to ["secretary"] — the decommissioned collector whose historical
// rows remain in collection_log after #101/#151 (#161).
func retiredSources() []string {
	raw := os.Getenv("RETIRED_SOURCES")
	if raw == "" {
		return []string{"secretary"}
	}
	return splitCSV(raw)
}

// briefingMaxActions parses BRIEFING_MAX_ACTIONS from the environment.
// Default is 40. Invalid values are ignored and the default is used.
func briefingMaxActions() int {
	const defaultMax = 40
	v := os.Getenv("BRIEFING_MAX_ACTIONS")
	if v == "" {
		return defaultMax
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: BRIEFING_MAX_ACTIONS is invalid; using default 40",
			"value", v,
			"error", err,
		)
		return defaultMax
	}
	return n
}

// normalizeExts ensures every extension starts with a leading dot and is lowercase.
func normalizeExts(exts []string) []string {
	if len(exts) == 0 {
		return nil
	}
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}
