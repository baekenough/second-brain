package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	Port        string
	DatabaseURL string

	// Embedding (optional — vector search disabled when empty)
	// Token resolution order:
	//   1. EmbeddingAPIKey (manual override)
	//   2. CliProxyAuthFile (auto-read OAuth token from CliProxyAPI)
	//   3. No auth (some self-hosted endpoints don't require it)
	EmbeddingAPIURL  string
	EmbeddingAPIKey  string
	EmbeddingModel   string
	CliProxyAuthFile string // path to CliProxyAPI OAuth JSON, e.g. ~/.cli-proxy-api/codex-user@gmail.com-pro.json

	// LLM (optional — Discord RAG answer generation; falls back to EmbeddingAPIURL when unset)
	// LLMAPIURL: LLM_API_URL env var; defaults to EmbeddingAPIURL with /embeddings → /chat/completions suffix fix.
	// LLMAPIKey: LLM_API_KEY env var; defaults to EmbeddingAPIKey.
	// LLMAuthFile: LLM_CLIPROXY_AUTH_FILE env var; defaults to CLIPROXY_AUTH_FILE when unset.
	LLMAPIURL      string
	LLMAPIKey      string
	LLMAuthFile    string // path to CliProxyAPI OAuth JSON for LLM requests
	LLMModel       string
	LLMMaxTokens   int
	LLMTemperature float64

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

	// API authentication (optional — disabled when empty, for dev backward compat)
	APIKey string // API_KEY — Bearer token required for /api/v1/* routes

	// Filesystem (optional)
	FilesystemPath    string // FILESYSTEM_PATH — directory to scan
	FilesystemEnabled bool   // FILESYSTEM_ENABLED — default false

	// Scheduler
	CollectInterval time.Duration
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

	llmTemperature := 0.3
	if v := os.Getenv("LLM_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			llmTemperature = f
		}
	}

	return &Config{
		Port:        getenv("PORT", "8080"),
		DatabaseURL: getenv("DATABASE_URL", "postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"),

		EmbeddingAPIURL:  embeddingAPIURL,
		EmbeddingAPIKey:  embeddingAPIKey,
		EmbeddingModel:   getenv("EMBEDDING_MODEL", "text-embedding-3-small"),
		CliProxyAuthFile: os.Getenv("CLIPROXY_AUTH_FILE"),

		LLMAPIURL:      llmAPIURL,
		LLMAPIKey:      llmAPIKey,
		LLMAuthFile:    llmAuthFile,
		LLMModel:       getenv("LLM_MODEL", "gpt-4o-mini"),
		LLMMaxTokens:   llmMaxTokens,
		LLMTemperature: llmTemperature,

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

		APIKey: os.Getenv("API_KEY"),

		FilesystemPath:    os.Getenv("FILESYSTEM_PATH"),
		FilesystemEnabled: os.Getenv("FILESYSTEM_ENABLED") == "true",

		CollectInterval: interval,
	}, nil
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
