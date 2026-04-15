package collector

// Discord collector: the full implementation exists in discord.go.
// Bot response functionality has been removed from the API server.
// Discord collection is available as opt-in in cmd/collector by
// registering NewDiscordCollectorWithAttachments.
//
// To enable: add Discord collector to the collectors slice in
// cmd/collector/main.go with appropriate config (DISCORD_BOT_TOKEN, etc).
