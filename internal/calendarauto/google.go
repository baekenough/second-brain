package calendarauto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleWriter uses a separate write-scoped credential from the read collector.
// Refresh tokens are read from a private read-only mount; access tokens stay in memory.
type GoogleWriter struct {
	client     *http.Client
	baseURL    string
	calendarID string
	now        func() time.Time
}

func NewGoogleWriter(ctx context.Context, credentialsPath, tokenPath, calendarID string) (*GoogleWriter, error) {
	if strings.TrimSpace(calendarID) == "" || strings.Contains(calendarID, ",") {
		return nil, errors.New("calendar write target must be one calendar")
	}
	credentials, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, errors.New("read calendar write client credentials")
	}
	cfg, err := google.ConfigFromJSON(credentials, "https://www.googleapis.com/auth/calendar.events")
	if err != nil {
		return nil, errors.New("parse calendar write client credentials")
	}
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, errors.New("read calendar write token")
	}
	var token struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		TokenType    string  `json:"token_type"`
		ExpiresAt    float64 `json:"expires_at"`
		Scope        string  `json:"scope"`
	}
	if json.Unmarshal(data, &token) != nil || token.RefreshToken == "" {
		return nil, errors.New("invalid calendar write token")
	}
	scopes := " " + token.Scope + " "
	if !strings.Contains(scopes, " https://www.googleapis.com/auth/calendar.events ") && !strings.Contains(scopes, " https://www.googleapis.com/auth/calendar ") {
		return nil, errors.New("calendar token has no event write scope")
	}
	// Enforce trusted Google endpoints rather than accepting token endpoints from files.
	cfg.Endpoint = google.Endpoint
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second})
	client := cfg.Client(ctx, &oauth2.Token{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, TokenType: token.TokenType, Expiry: time.Unix(int64(token.ExpiresAt), 0)})
	client.Timeout = 30 * time.Second
	return &GoogleWriter{client: client, baseURL: "https://www.googleapis.com/calendar/v3", calendarID: calendarID, now: time.Now}, nil
}

type googleDateTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
}
type googleEvent struct {
	ID                 string         `json:"id"`
	Summary            string         `json:"summary"`
	Description        string         `json:"description,omitempty"`
	Location           string         `json:"location,omitempty"`
	Status             string         `json:"status,omitempty"`
	Start              googleDateTime `json:"start"`
	End                googleDateTime `json:"end"`
	ExtendedProperties struct {
		Private map[string]string `json:"private"`
	} `json:"extendedProperties"`
}

func normalizedTitle(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}
func eventFingerprint(calendarID string, e Event) string {
	start, _ := time.Parse(time.RFC3339, e.Start)
	sum := sha256.Sum256([]byte(calendarID + "\n" + start.UTC().Format(time.RFC3339) + "\n" + normalizedTitle(e.Summary)))
	return hex.EncodeToString(sum[:])
}
func (g *GoogleWriter) path() string { return "/calendars/" + url.PathEscape(g.calendarID) + "/events" }

// do never returns upstream response bodies: both Google and OAuth errors can contain secrets.
func (g *GoogleWriter) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, errors.New("encode calendar event")
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, reader)
	if err != nil {
		return 0, errors.New("build calendar request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, errors.New("calendar transport or authorization failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("calendar HTTP %d", resp.StatusCode)
	}
	if out != nil && json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out) != nil {
		return resp.StatusCode, errors.New("decode calendar response")
	}
	return resp.StatusCode, nil
}

func (g *GoogleWriter) EnsureEvent(ctx context.Context, e Event) (string, bool, error) {
	start, err := time.Parse(time.RFC3339, e.Start)
	if err != nil {
		return "", false, errors.New("invalid calendar start")
	}
	end, err := time.Parse(time.RFC3339, e.End)
	if err != nil || !end.After(start) || !start.After(g.now()) || normalizedTitle(e.Summary) == "" {
		return "", false, errors.New("invalid or past calendar event")
	}
	fingerprint := eventFingerprint(g.calendarID, e)
	id := "sb" + fingerprint
	var existing googleEvent
	status, err := g.do(ctx, "GET", g.path()+"/"+id, nil, &existing)
	if err == nil {
		if existing.Status == "cancelled" {
			return "", false, ErrCalendarConflict
		}
		if existing.ExtendedProperties.Private["second_brain_fingerprint"] != fingerprint {
			return "", false, ErrCalendarConflict
		}
		return existing.ID, false, nil
	}
	if status != http.StatusNotFound {
		return "", false, err
	}
	// Scan the entire overlapping window. Any ambiguous overlap is review, never
	// an automatic duplicate or a new competing appointment.
	params := url.Values{"timeMin": {start.Format(time.RFC3339)}, "timeMax": {end.Format(time.RFC3339)}, "singleEvents": {"true"}, "showDeleted": {"false"}, "maxResults": {"2500"}}
	conflict := false
	for {
		var page struct {
			Items         []googleEvent `json:"items"`
			NextPageToken string        `json:"nextPageToken"`
		}
		if _, err = g.do(ctx, "GET", g.path()+"?"+params.Encode(), nil, &page); err != nil {
			return "", false, err
		}
		for _, old := range page.Items {
			if old.Status == "cancelled" {
				continue
			}
			t, parseErr := time.Parse(time.RFC3339, old.Start.DateTime)
			if parseErr == nil && t.Equal(start) && normalizedTitle(old.Summary) == normalizedTitle(e.Summary) {
				return old.ID, false, nil
			}
			conflict = true
		}
		if page.NextPageToken == "" {
			break
		}
		params.Set("pageToken", page.NextPageToken)
	}
	if conflict {
		return "", false, ErrCalendarConflict
	}
	if !start.After(g.now()) {
		return "", false, errors.New("calendar event start has passed")
	}
	event := googleEvent{ID: id, Summary: e.Summary, Description: e.Description, Location: e.Location, Start: googleDateTime{DateTime: e.Start}, End: googleDateTime{DateTime: e.End}}
	event.ExtendedProperties.Private = map[string]string{"second_brain_fingerprint": fingerprint, "second_brain_automation": "jev-v1"}
	var created googleEvent
	status, err = g.do(ctx, "POST", g.path()+"?sendUpdates=none", event, &created)
	if status == http.StatusConflict {
		if _, err = g.do(ctx, "GET", g.path()+"/"+id, nil, &existing); err != nil {
			return "", false, err
		}
		if existing.Status == "cancelled" || existing.ExtendedProperties.Private["second_brain_fingerprint"] != fingerprint {
			return "", false, ErrCalendarConflict
		}
		return existing.ID, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if created.ID != id {
		return "", false, errors.New("calendar returned unexpected event ID")
	}
	return created.ID, true, nil
}
