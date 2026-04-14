package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
)

// GitHubCollector collects issues, PRs, and READMEs from an organisation's
// repositories using the GitHub REST API.
type GitHubCollector struct {
	token  string
	org    string
	client *http.Client
}

// NewGitHubCollector returns a GitHubCollector. When token is empty the
// collector is disabled.
func NewGitHubCollector(token, org string) *GitHubCollector {
	return &GitHubCollector{
		token:  token,
		org:    org,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *GitHubCollector) Name() string             { return "github" }
func (c *GitHubCollector) Source() model.SourceType { return model.SourceGitHub }
func (c *GitHubCollector) Enabled() bool            { return c.token != "" }

// Collect fetches issues and pull requests updated after since.
func (c *GitHubCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	repos, err := c.listRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("github list repos: %w", err)
	}

	var docs []model.Document
	for _, repo := range repos {
		issues, err := c.listIssues(ctx, repo, since)
		if err != nil {
			slog.Warn("github: failed to fetch issues", "repo", repo, "error", err)
			continue
		}
		docs = append(docs, issues...)
	}

	slog.Info("github: collected documents", "count", len(docs))
	return docs, nil
}

// --- GitHub API helpers ---

func (c *GitHubCollector) listRepos(ctx context.Context) ([]string, error) {
	type repo struct {
		FullName string `json:"full_name"`
		Archived bool   `json:"archived"`
	}

	var all []string
	page := 1
	for {
		var repos []repo
		path := fmt.Sprintf("/orgs/%s/repos?per_page=100&page=%d&type=all", c.org, page)
		if err := c.get(ctx, path, &repos); err != nil {
			return nil, err
		}
		if len(repos) == 0 {
			break
		}
		for _, r := range repos {
			if !r.Archived {
				all = append(all, r.FullName)
			}
		}
		page++
	}
	return all, nil
}

func (c *GitHubCollector) listIssues(ctx context.Context, repo string, since time.Time) ([]model.Document, error) {
	type issue struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		HTMLURL   string `json:"html_url"`
		UpdatedAt string `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		PullRequest *struct{} `json:"pull_request"` // non-nil means it is a PR
	}

	var docs []model.Document
	page := 1
	for {
		var issues []issue
		path := fmt.Sprintf(
			"/repos/%s/issues?state=all&per_page=100&page=%d&since=%s",
			repo, page, since.UTC().Format(time.RFC3339),
		)
		if err := c.get(ctx, path, &issues); err != nil {
			return nil, err
		}
		if len(issues) == 0 {
			break
		}

		for _, i := range issues {
			kind := "issue"
			if i.PullRequest != nil {
				kind = "pull_request"
			}
			docs = append(docs, model.Document{
				ID:         uuid.New(),
				SourceType: model.SourceGitHub,
				SourceID:   fmt.Sprintf("%s#%d", repo, i.Number),
				Title:      fmt.Sprintf("[%s] %s", repo, i.Title),
				Content:    i.Body,
				Metadata: map[string]any{
					"repo":    repo,
					"number":  i.Number,
					"state":   i.State,
					"kind":    kind,
					"url":     i.HTMLURL,
					"author":  i.User.Login,
				},
				CollectedAt: time.Now().UTC(),
			})
		}
		page++
	}
	return docs, nil
}

func (c *GitHubCollector) get(ctx context.Context, path string, dest interface{}) error {
	u := "https://api.github.com" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("github API %s: status %d: %s", path, res.StatusCode, body)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dest)
}
