// Package setup implements an interactive setup wizard for the collector binary.
// Files in this package that are not guarded by a build tag are always compiled,
// regardless of whether the wizard TUI (charm.land/huh/v2) is linked in.
package setup

// EnvVar describes a single environment variable the wizard can configure.
type EnvVar struct {
	// Key is the variable name written to .env (e.g. "SLACK_BOT_TOKEN").
	Key string

	// Label is a human-readable prompt label shown in the wizard.
	Label string

	// Description is a short explanation shown below the prompt.
	Description string

	// Secret marks the value as sensitive; the wizard masks input.
	Secret bool

	// Multiline allows multi-line paste (used for JSON credentials).
	Multiline bool

	// DefaultValue is pre-populated when the user leaves the field blank.
	DefaultValue string

	// Hardcoded means the value is always written as DefaultValue and the
	// user is never prompted.
	Hardcoded bool
}

// Channel describes a collector data source and the env vars it requires.
type Channel struct {
	// ID is the canonical short name (e.g. "slack", "filesystem").
	ID string

	// Label is the display name shown in the channel picker.
	Label string

	// InfoOnly channels print an informational message instead of prompting.
	InfoOnly bool

	// InfoMessage is printed when InfoOnly is true.
	InfoMessage string

	// Vars is the ordered list of environment variables the wizard writes.
	Vars []EnvVar
}

// Registry is the canonical ordered list of collector channels.
// discord is info-only — it points users to the README §Discord section.
var Registry = []Channel{
	{
		ID:    "filesystem",
		Label: "Filesystem (local folder)",
		Vars: []EnvVar{
			{
				Key:          "FILESYSTEM_PATH",
				Label:        "Filesystem path",
				Description:  "Absolute path to the folder to collect from (e.g. /Users/you/Google Drive/My Drive).",
				DefaultValue: "",
			},
			{
				Key:          "FILESYSTEM_ENABLED",
				Label:        "Filesystem enabled",
				Description:  "Always set to true when filesystem path is configured.",
				DefaultValue: "true",
				Hardcoded:    true,
			},
		},
	},
	{
		ID:    "slack",
		Label: "Slack",
		Vars: []EnvVar{
			{
				Key:         "SLACK_BOT_TOKEN",
				Label:       "Slack bot token",
				Description: "OAuth bot token starting with xoxb-.",
				Secret:      true,
			},
			{
				Key:         "SLACK_TEAM_ID",
				Label:       "Slack team ID",
				Description: "Workspace ID (T...). Found at api.slack.com/apps → Basic Information.",
			},
		},
	},
	{
		ID:    "github",
		Label: "GitHub",
		Vars: []EnvVar{
			{
				Key:         "GITHUB_TOKEN",
				Label:       "GitHub personal access token",
				Description: "Fine-grained or classic PAT with repo read scope.",
				Secret:      true,
			},
			{
				Key:         "GITHUB_ORG",
				Label:       "GitHub organization",
				Description: "GitHub organisation or user name whose repositories to collect.",
			},
		},
	},
	{
		ID:    "gdrive",
		Label: "Google Drive (service account)",
		Vars: []EnvVar{
			{
				Key:         "GDRIVE_CREDENTIALS_JSON",
				Label:       "Google service account JSON",
				Description: "Paste the full contents of your service_account.json (will be stored on a single line).",
				Secret:      true,
				Multiline:   true,
			},
		},
	},
	{
		ID:    "notion",
		Label: "Notion",
		Vars: []EnvVar{
			{
				Key:         "NOTION_TOKEN",
				Label:       "Notion integration token",
				Description: "Internal integration secret from notion.so/my-integrations.",
				Secret:      true,
			},
		},
	},
	{
		ID:    "telegram",
		Label: "Telegram",
		Vars: []EnvVar{
			{
				Key:         "TELEGRAM_BOT_TOKEN",
				Label:       "Telegram bot token",
				Description: "Token from @BotFather.",
				Secret:      true,
			},
			{
				Key:         "TELEGRAM_CHAT_IDS",
				Label:       "Telegram chat IDs",
				Description: "Comma-separated list of chat IDs to collect from (e.g. -1001234567890).",
			},
		},
	},
	{
		ID:       "discord",
		Label:    "Discord",
		InfoOnly: true,
		InfoMessage: "Discord collector requires a bot token and guild IDs configured via the API server. " +
			"See README §Discord for setup instructions. No .env keys are written by this wizard.",
	},
}
