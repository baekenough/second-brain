package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
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
	EmbeddingAPIURL  string
	// EmbeddingAPIKey is a dedicated OpenAI API key (EMBEDDING_API_KEY env var).
	// Use a separate key from any chat/LLM key so embedding costs are tracked
	// independently and the key can be rotated without affecting chat traffic.
	EmbeddingAPIKey  string
	EmbeddingModel   string
	EmbeddingDim     int    // EMBEDDING_DIM — vector dimension; must match the model output. Default 1536.
	CliProxyAuthFile string // CLIPROXY_AUTH_FILE — CliProxyAPI OAuth JSON path (chat proxies only; NOT used for embeddings when EMBEDDING_API_KEY is set)

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
	LLMAPIURL          string
	LLMAPIKey          string
	LLMAuthFile        string // path to CliProxyAPI OAuth JSON for LLM requests
	LLMModel           string
	LLMMaxTokens       int
	LLMTemperature     float64
	LLMTimeoutSeconds  int    // LLM_TIMEOUT_SECONDS — HTTP client timeout; default 120

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

	// Reranker (optional — cross-encoder post-retrieval reranking disabled when empty)
	RerankURL    string // RERANKER_URL — Jina-compatible /rerank endpoint base URL
	RerankAPIKey string // RERANKER_API_KEY — Bearer token for the reranker API
	RerankModel  string // RERANKER_MODEL — model identifier sent in the request body

	// Alerting (optional — Slack/Discord webhook for eval regression alerts)
	AlertWebhookURL string // ALERT_WEBHOOK_URL — Slack-compatible incoming webhook URL

	// API authentication (optional — disabled when empty, for dev backward compat)
	APIKey string // API_KEY — Bearer token required for /api/v1/* routes

	// Filesystem (optional)
	FilesystemPath         string   // FILESYSTEM_PATH — directory to scan
	FilesystemEnabled      bool     // FILESYSTEM_ENABLED — default false
	FilesystemExcludeDirs  []string // FILESYSTEM_EXCLUDE_DIRS — comma-separated dir names to skip (merged with built-in defaults)
	FilesystemExcludeExts  []string // FILESYSTEM_EXCLUDE_EXTS — comma-separated file extensions to skip (merged with built-in defaults)

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
	// CALENDAR_ID: calendar identifier (default: "primary")
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
	SMSSourceDir     string
	SMSMaxFileBytes  int64

	// Whisper transcription (optional — disabled when WhisperAPIKey is empty)
	// WHISPER_API_KEY: OpenAI (or compatible) API key
	// WHISPER_API_URL: base URL (default: "https://api.openai.com/v1")
	// WHISPER_AUDIO_DIR: directory containing audio files to transcribe
	// WHISPER_MODEL: model identifier (default: "whisper-1")
	// WHISPER_LANGUAGE: BCP-47 language hint (default: "ko")
	// WHISPER_MAX_FILE_BYTES: per-file size cap (bytes, int64, default: 100 MiB).
	// Set 0 to disable the cap entirely (no limit). Invalid values use the default.
	WhisperAPIKey       string
	WhisperAPIURL       string
	WhisperAudioDir     string
	WhisperModel        string
	WhisperLanguage     string
	WhisperMaxFileBytes int64

	// Summarizer
	// SummarizerBackfillEnabled controls whether the SummarizerWorker scans for
	// pre-existing unsummarized documents (WHERE title_summary IS NULL).
	// Default true. Set SUMMARIZER_BACKFILL_ENABLED=false when running a slow
	// local LLM to avoid a flood of LLM calls for the pre-existing backlog.
	SummarizerBackfillEnabled bool // SUMMARIZER_BACKFILL_ENABLED

	// Scheduler
	CollectInterval time.Duration

	// CollectorInstance is the per-host identifier used to key the
	// collector_state watermark table. Defaults to os.Hostname() (or
	// "default" when that fails) when COLLECTOR_INSTANCE is unset.
	CollectorInstance string
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

	// SUMMARIZER_BACKFILL_ENABLED: default true.
	// Set =false to skip the ListUnsummarized scan when running a slow local LLM.
	summarizerBackfill := true
	if v := os.Getenv("SUMMARIZER_BACKFILL_ENABLED"); v == "false" || v == "0" {
		summarizerBackfill = false
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

	return &Config{
		Port:        getenv("PORT", "8080"),
		DatabaseURL: getenv("DATABASE_URL", "postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"),

		EmbeddingAPIURL:  embeddingAPIURL,
		EmbeddingAPIKey:  embeddingAPIKey,
		EmbeddingModel:   getenv("EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingDim:     embeddingDim,
		CliProxyAuthFile: os.Getenv("CLIPROXY_AUTH_FILE"),

		EmbeddingProvider: getenv("EMBEDDING_PROVIDER", "openai"),

		LocalEmbeddingModel:    getenv("LOCAL_EMBEDDING_MODEL", "bge-m3"),
		LocalEmbeddingEndpoint: os.Getenv("LOCAL_EMBEDDING_ENDPOINT"),

		LLMAPIURL:          llmAPIURL,
		LLMAPIKey:          llmAPIKey,
		LLMAuthFile:        llmAuthFile,
		LLMModel:           getenv("LLM_MODEL", "gpt-4o-mini"),
		LLMMaxTokens:       llmMaxTokens,
		LLMTemperature:     llmTemperature,
		LLMTimeoutSeconds:  llmTimeoutSeconds,

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

		RerankURL:    os.Getenv("RERANKER_URL"),
		RerankAPIKey: os.Getenv("RERANKER_API_KEY"),
		RerankModel:  getenv("RERANKER_MODEL", "jina-reranker-v2-base-multilingual"),

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

		WhisperAPIKey:       os.Getenv("WHISPER_API_KEY"),
		WhisperAPIURL:       getenv("WHISPER_API_URL", "https://api.openai.com/v1"),
		WhisperAudioDir:     os.Getenv("WHISPER_AUDIO_DIR"),
		WhisperModel:        getenv("WHISPER_MODEL", "whisper-1"),
		WhisperLanguage:     getenv("WHISPER_LANGUAGE", "ko"),
		WhisperMaxFileBytes: whisperMaxFileBytes(),

		SummarizerBackfillEnabled: summarizerBackfill,

		CollectInterval:   interval,
		CollectorInstance: collectorInstance,
	}, nil
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
