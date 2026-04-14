package config

import (
	"os"
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

	// Slack (optional)
	SlackBotToken string
	SlackTeamID   string

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

	return &Config{
		Port:        getenv("PORT", "9200"),
		DatabaseURL: getenv("DATABASE_URL", "postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"),

		EmbeddingAPIURL:  getenv("EMBEDDING_API_URL", "https://api.openai.com/v1"),
		EmbeddingAPIKey:  os.Getenv("EMBEDDING_API_KEY"),
		EmbeddingModel:   getenv("EMBEDDING_MODEL", "text-embedding-3-small"),
		CliProxyAuthFile: os.Getenv("CLIPROXY_AUTH_FILE"),

		SlackBotToken: os.Getenv("SLACK_BOT_TOKEN"),
		SlackTeamID:   os.Getenv("SLACK_TEAM_ID"),

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

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
