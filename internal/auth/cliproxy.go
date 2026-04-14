// Package auth provides shared token sources for HTTP-based API clients
// (embeddings, LLM chat completions, etc.) that authenticate with
// CliProxyAPI-issued OAuth tokens or static API keys.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// TokenSource abstracts the means of obtaining a Bearer token.
// Implementations may return a cached token or read fresh from disk.
type TokenSource interface {
	Token() (string, error)
}

// StaticToken returns the same string every call.
type StaticToken string

// Token implements TokenSource.
func (s StaticToken) Token() (string, error) {
	if s == "" {
		return "", fmt.Errorf("auth: static token is empty")
	}
	return string(s), nil
}

// CliProxyFile reads a CliProxyAPI OAuth JSON file on demand and caches the
// access_token for a short TTL (default 5 minutes). When the cache expires the
// file is re-read, which allows the credential file to be rotated by an
// external refresh daemon without restarting the application.
type CliProxyFile struct {
	path     string
	cacheTTL time.Duration

	mu        sync.Mutex
	cached    string
	expiresAt time.Time
}

// NewCliProxyFile returns a CliProxyFile that reads tokens from path.
func NewCliProxyFile(path string) *CliProxyFile {
	return &CliProxyFile{path: path, cacheTTL: 5 * time.Minute}
}

// Token implements TokenSource. It returns the cached access_token when it is
// still valid, otherwise re-reads the file from disk.
func (c *CliProxyFile) Token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != "" && time.Now().Before(c.expiresAt) {
		return c.cached, nil
	}

	data, err := os.ReadFile(c.path)
	if err != nil {
		return "", fmt.Errorf("auth: cliproxy file: %w", err)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("auth: cliproxy parse: %w", err)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("auth: cliproxy access_token empty in %s", c.path)
	}

	c.cached = payload.AccessToken
	c.expiresAt = time.Now().Add(c.cacheTTL)
	return c.cached, nil
}

// Resolve constructs the appropriate TokenSource based on available inputs.
//
// Precedence:
//  1. staticKey non-empty → StaticToken
//  2. cliProxyPath non-empty → CliProxyFile (OAuth token auto-refreshed from disk)
//  3. both empty → nil (caller sends no Authorization header)
func Resolve(staticKey, cliProxyPath string) TokenSource {
	if staticKey != "" {
		return StaticToken(staticKey)
	}
	if cliProxyPath != "" {
		return NewCliProxyFile(cliProxyPath)
	}
	return nil
}
