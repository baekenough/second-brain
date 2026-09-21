package calendarauto

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testGoogle(t *testing.T, handler http.HandlerFunc) *GoogleWriter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &GoogleWriter{client: server.Client(), baseURL: server.URL, calendarID: "primary", now: func() time.Time { return time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) }}
}
func futureEvent() Event {
	return Event{Summary: "AX interview", Start: "2026-09-22T17:00:00+09:00", End: "2026-09-22T18:00:00+09:00"}
}
func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func TestGoogleRetryConflictVerifiesFingerprint(t *testing.T) {
	for _, kind := range []string{"matching", "wrong fingerprint", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			e := futureEvent()
			gets, posts := 0, 0
			g := testGoogle(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
					if r.URL.Query().Get("sendUpdates") != "none" {
						t.Error("notifications enabled")
					}
					w.WriteHeader(409)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/events") {
					writeJSON(w, map[string]any{"items": []any{}})
					return
				}
				gets++
				if gets == 1 {
					w.WriteHeader(404)
					return
				}
				old := googleEvent{ID: "sb" + eventFingerprint("primary", e)}
				old.ExtendedProperties.Private = map[string]string{"second_brain_fingerprint": eventFingerprint("primary", e)}
				if kind == "wrong fingerprint" {
					old.ExtendedProperties.Private["second_brain_fingerprint"] = "other"
				}
				if kind == "cancelled" {
					old.Status = "cancelled"
				}
				writeJSON(w, old)
			})
			id, created, err := g.EnsureEvent(context.Background(), e)
			if kind == "matching" {
				if err != nil || created || id == "" {
					t.Fatalf("%q %v %v", id, created, err)
				}
			} else if !errors.Is(err, ErrCalendarConflict) {
				t.Fatalf("expected conflict: %v", err)
			}
			if posts != 1 {
				t.Fatalf("posts=%d", posts)
			}
		})
	}
}

func TestGoogleCancelledIDNeverRecreated(t *testing.T) {
	calls := 0
	g := testGoogle(t, func(w http.ResponseWriter, r *http.Request) { calls++; writeJSON(w, googleEvent{Status: "cancelled"}) })
	_, _, err := g.EnsureEvent(context.Background(), futureEvent())
	if !errors.Is(err, ErrCalendarConflict) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestGoogleOverlapPagination(t *testing.T) {
	for _, kind := range []string{"duplicate", "ambiguous", "duplicate then conflict", "two duplicates"} {
		t.Run(kind, func(t *testing.T) {
			e := futureEvent()
			pages := 0
			g := testGoogle(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/events") {
					w.WriteHeader(404)
					return
				}
				if r.Method != "GET" {
					t.Error("must not create event")
					w.WriteHeader(500)
					return
				}
				pages++
				old := googleEvent{ID: "old", Summary: "AX interview", Start: googleDateTime{DateTime: e.Start}}
				if pages == 1 {
					items := []googleEvent{}
					if kind == "duplicate then conflict" || kind == "two duplicates" {
						items = append(items, old)
					}
					writeJSON(w, map[string]any{"items": items, "nextPageToken": "page2"})
					return
				}
				if r.URL.Query().Get("pageToken") != "page2" {
					t.Error("missing pagination token")
				}
				if kind == "ambiguous" || kind == "duplicate then conflict" {
					old.Summary = "Different appointment"
				}
				if kind == "two duplicates" {
					old.ID = "another"
				}
				writeJSON(w, map[string]any{"items": []googleEvent{old}})
			})
			id, created, err := g.EnsureEvent(context.Background(), e)
			if kind == "duplicate" || kind == "duplicate then conflict" || kind == "two duplicates" {
				if id != "old" || created || err != nil {
					t.Fatalf("%q %v %v", id, created, err)
				}
			} else if !errors.Is(err, ErrCalendarConflict) {
				t.Fatalf("expected ambiguous conflict: %v", err)
			}
			expectedPages := 2
			if kind == "duplicate then conflict" || kind == "two duplicates" {
				expectedPages = 1
			}
			if pages != expectedPages {
				t.Errorf("pages=%d want=%d", pages, expectedPages)
			}
		})
	}
}

func TestGoogleErrorsDoNotLeakTokens(t *testing.T) {
	g := testGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("access_token=private-secret"))
	})
	_, _, err := g.EnsureEvent(context.Background(), futureEvent())
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestGoogleFutureCheckBeforeAndAfterLookup(t *testing.T) {
	e := futureEvent()
	start, _ := time.Parse(time.RFC3339, e.Start)
	calls := 0
	g := testGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/events") {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, map[string]any{"items": []any{}})
	})
	g.now = func() time.Time { return start }
	if _, _, err := g.EnsureEvent(context.Background(), e); err == nil || calls != 0 {
		t.Fatalf("past event calls=%d err=%v", calls, err)
	}
	clockCalls := 0
	g.now = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return start.Add(-time.Minute)
		}
		return start
	}
	if _, _, err := g.EnsureEvent(context.Background(), e); err == nil || calls != 2 {
		t.Fatalf("expired during lookup calls=%d err=%v", calls, err)
	}
}

func TestGoogleCreateStableID(t *testing.T) {
	e := futureEvent()
	ids := []string{}
	g := testGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var event googleEvent
			_ = json.NewDecoder(r.Body).Decode(&event)
			ids = append(ids, event.ID)
			if r.URL.Query().Get("sendUpdates") != "none" {
				t.Error("notifications enabled")
			}
			writeJSON(w, event)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeJSON(w, map[string]any{"items": []any{}})
			return
		}
		w.WriteHeader(404)
	})
	for i := 0; i < 2; i++ {
		if _, created, err := g.EnsureEvent(context.Background(), e); err != nil || !created {
			t.Fatalf("created=%v err=%v", created, err)
		}
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("unstable IDs: %v", ids)
	}
}

func TestGoogleInvalidTimesNeverCallAPI(t *testing.T) {
	calls := 0
	g := testGoogle(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	for _, times := range [][2]string{{"2026-09-22T17:00:00", "2026-09-22T18:00:00"}, {"2026-09-22T17:00:00+09:00", "2026-09-22T16:00:00+09:00"}} {
		e := futureEvent()
		e.Start = times[0]
		e.End = times[1]
		if _, _, err := g.EnsureEvent(context.Background(), e); err == nil {
			t.Error("invalid time accepted")
		}
	}
	if calls != 0 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestGoogleConstructorRejectsMissingWriteScope(t *testing.T) {
	dir := t.TempDir()
	credentials := filepath.Join(dir, "client.json")
	token := filepath.Join(dir, "token.json")
	if err := os.WriteFile(credentials, []byte(`{"installed":{"client_id":"id","client_secret":"secret","redirect_uris":["http://localhost"],"auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte(`{"refresh_token":"private-secret","scope":"https://www.googleapis.com/auth/calendar.readonly"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := NewGoogleWriter(context.Background(), credentials, token, "primary")
	if err == nil || !strings.Contains(err.Error(), "no event write scope") || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("unsafe or missing rejection: %v", err)
	}
}
