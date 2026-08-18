package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/go-chi/chi/v5"
)

// Bounds for GET /api/v1/actions. They mirror store.clampActionLimit; the store
// clamps as well, because a bound that lives only in the handler protects
// nothing the moment a second caller appears.
const (
	actionsDefaultLimit = 50
	actionsMaxLimit     = 200
)

// ActionLister is the read side of the actions feature.
type ActionLister interface {
	ListOpenActions(ctx context.Context, f store.ActionFilter) ([]store.ActionListItem, error)
}

// ActionStateSetter is the user-driven state transition. It returns found=false
// (with a nil error) when identityKey does not exist, so the handler can answer
// 404 instead of leaking a foreign-key violation as a 500.
type ActionStateSetter interface {
	SetActionState(ctx context.Context, identityKey string, state model.ActionState, note string) (bool, error)
}

// actionResponseItem is the wire shape of one action.
//
// It deliberately carries no document content: the response gives a
// document_id, and the body is fetched from the existing document detail route.
// That keeps the amount of personal data sitting in any proxy or cache along
// this path down to a one-line summary (privacy convention, plan Task 8).
//
// detected_by and confidence are exposed verbatim rather than hidden behind a
// "verified" boolean — the UI shows them so a user can discount an action the
// LLM invented (spec §7.4).
type actionResponseItem struct {
	IdentityKey     string     `json:"identity_key"`
	DocumentID      string     `json:"document_id"`
	ThreadKey       string     `json:"thread_key"`
	Kind            string     `json:"kind"`
	Summary         string     `json:"summary"`
	CounterpartName string     `json:"counterpart_name"`
	DueAt           *time.Time `json:"due_at"`
	DetectedBy      string     `json:"detected_by"`
	Confidence      float64    `json:"confidence"`
	ObservedAt      time.Time  `json:"observed_at"`
	State           string     `json:"state"`
}

type listActionsResponse struct {
	Actions []actionResponseItem `json:"actions"`
	Count   int                  `json:"count"`
	// Truncated is true when the page came back exactly full, i.e. there may be
	// more rows behind it. Without this flag a client cannot distinguish "50
	// open actions" from "at least 50 open actions".
	Truncated bool `json:"truncated"`
}

// WithActions wires the actions read and write dependencies. Both routes are
// registered only when the corresponding dependency is non-nil, so leaving the
// feature flag off means the routes do not exist (404) rather than existing and
// erroring (500).
func (s *Server) WithActions(lister ActionLister, setter ActionStateSetter) *Server {
	s.actionLister = lister
	s.actionSetter = setter
	return s
}

// parseActionFilter converts query parameters into a store.ActionFilter.
//
// The returned message is a fixed string chosen from this function; it never
// interpolates the submitted value, because a counterpart filter can contain a
// real person's name and error bodies are logged by intermediaries.
func parseActionFilter(r *http.Request) (store.ActionFilter, string) {
	q := r.URL.Query()
	f := store.ActionFilter{}

	for _, raw := range q["kind"] {
		k := model.ActionKind(raw)
		if !model.IsValidActionKind(k) {
			return f, "invalid kind filter"
		}
		f.Kinds = append(f.Kinds, k)
	}

	f.Counterpart = q.Get("counterpart")

	if v := q.Get("due_before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, "invalid due_before filter (want RFC3339)"
		}
		f.DueBefore = &t
	}

	if v := q.Get("min_confidence"); v != "" {
		c, err := strconv.ParseFloat(v, 64)
		if err != nil || c < 0 || c > 1 {
			return f, "invalid min_confidence filter (want 0..1)"
		}
		f.MinConfidence = c
	}

	switch v := q.Get("sort"); v {
	case "":
		f.Sort = "due"
	case "due", "confidence":
		f.Sort = v
	default:
		return f, "invalid sort filter"
	}

	f.Limit = actionsDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > actionsMaxLimit {
			return f, "invalid limit filter"
		}
		f.Limit = n
	}

	f.IncludeArchived = q.Get("include_archived") == "true"

	return f, ""
}

// listActionsHandler handles GET /api/v1/actions.
func (s *Server) listActionsHandler(w http.ResponseWriter, r *http.Request) {
	f, msg := parseActionFilter(r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	items, err := s.actionLister.ListOpenActions(r.Context(), f)
	if err != nil {
		// The error is logged, not returned: a pgx error can quote row values.
		slog.Error("actions: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	out := make([]actionResponseItem, 0, len(items))
	for _, it := range items {
		out = append(out, actionResponseItem{
			IdentityKey:     it.IdentityKey,
			DocumentID:      it.DocumentID.String(),
			ThreadKey:       it.ThreadKey,
			Kind:            string(it.Kind),
			Summary:         it.Summary,
			CounterpartName: it.CounterpartName,
			DueAt:           it.DueAt,
			DetectedBy:      string(it.DetectedBy),
			Confidence:      it.Confidence,
			ObservedAt:      it.ObservedAt,
			State:           string(it.State),
		})
	}

	writeJSON(w, http.StatusOK, listActionsResponse{
		Actions:   out,
		Count:     len(out),
		Truncated: len(out) == f.Limit,
	})
}

// actionIdentityKeyRE matches the shape produced by action.BuildIdentityKey
// ("action:" + 16 hex chars). The handler validates the SHAPE only: the value
// is a deterministic hash that action_status rows already point at, so
// recomputing or normalising it would break the link between an action and the
// user's earlier decision about it (spec §5.5).
var actionIdentityKeyRE = regexp.MustCompile(`^action:[0-9a-f]{16}$`)

// actionNoteMaxLen bounds the free-text note. It is user-visible data, never
// logged, and exists only to be shown back to the same user.
const actionNoteMaxLen = 500

type setActionStateRequest struct {
	State string `json:"state"`
	Note  string `json:"note"`
}

type setActionStateResponse struct {
	IdentityKey string `json:"identity_key"`
	State       string `json:"state"`
}

// setActionStateHandler handles POST /api/v1/actions/{identity_key}/status.
//
// The endpoint is idempotent: the same request replayed produces the same
// status code, the same body, and the same database row (see
// ActionQueryStore.SetActionState).
func (s *Server) setActionStateHandler(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "identity_key")
	if !actionIdentityKeyRE.MatchString(key) {
		writeError(w, http.StatusBadRequest, "invalid action key")
		return
	}

	var req setActionStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// The decoder error can quote the request body, which may contain a
		// note; only the fixed message goes out and nothing is logged.
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	state := model.ActionState(req.State)
	switch state {
	case model.StateOpen, model.StateDone, model.StateIgnored:
	default:
		writeError(w, http.StatusBadRequest, "invalid state")
		return
	}

	// Truncate by runes, not bytes: notes are Korean in practice, and cutting a
	// multi-byte rune in half would hand PostgreSQL invalid UTF-8.
	note := req.Note
	if runes := []rune(note); len(runes) > actionNoteMaxLen {
		note = string(runes[:actionNoteMaxLen])
	}

	found, err := s.actionSetter.SetActionState(r.Context(), key, state, note)
	if err != nil {
		slog.Error("actions: set state failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}

	writeJSON(w, http.StatusOK, setActionStateResponse{IdentityKey: key, State: string(state)})
}
