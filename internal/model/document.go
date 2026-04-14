package model

import (
	"time"

	"github.com/google/uuid"
)

// SourceType identifies the origin of a document.
type SourceType string

const (
	SourceSlack      SourceType = "slack"
	SourceGitHub     SourceType = "github"
	SourceGDrive     SourceType = "gdrive"
	SourceNotion     SourceType = "notion"
	SourceFilesystem SourceType = "filesystem"
	SourceDiscord    SourceType = "discord"
)

// Document represents a piece of content collected from an external source.
type Document struct {
	ID          uuid.UUID      `json:"id"`
	SourceType  SourceType     `json:"source_type"`
	SourceID    string         `json:"source_id"`
	Title       string         `json:"title"`
	Content     string         `json:"content"`
	Metadata    map[string]any `json:"metadata"`
	Embedding   []float32      `json:"embedding,omitempty"`  // nil when not embedded
	Status      string         `json:"status"`               // "active", "deleted", "moved"
	DeletedAt   *time.Time     `json:"deleted_at,omitempty"` // nil for active documents
	CollectedAt time.Time      `json:"collected_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// SearchResult wraps a Document with relevance scoring metadata.
type SearchResult struct {
	Document
	Score     float64 `json:"score"`
	MatchType string  `json:"match_type"` // "fulltext", "vector", or "hybrid"
}

// SearchQuery describes a search request.
type SearchQuery struct {
	Query              string
	SourceType         *SourceType  // nil means all sources
	ExcludeSourceTypes []SourceType // source types to exclude from results
	Limit              int
	Embedding          []float32 // populated by search service when available
	IncludeDeleted     bool      // when true, search includes deleted/moved docs
	Sort               string    // "relevance" (default, score DESC) | "recent" (collected_at DESC)
}
