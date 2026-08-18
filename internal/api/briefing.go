package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/baekenough/second-brain/internal/briefing"
	"github.com/baekenough/second-brain/internal/store"
)

// briefingTimeout bounds one briefing request. On expiry briefing.Generate
// degrades to the deterministic aggregate rather than failing the request: a
// slow model must not turn into an error page over data the server already has.
const briefingTimeout = 30 * time.Second

type briefingResponse struct {
	Sentences []briefing.Sentence `json:"sentences"`
	Degraded  bool                `json:"degraded"`
	// DroppedCount is how many model sentences failed evidence validation. It is
	// deliberately part of the public response: it lets the user see that the
	// server checks the model's claims instead of forwarding them.
	DroppedCount int       `json:"dropped_count"`
	ActionCount  int       `json:"action_count"`
	Cached       bool      `json:"cached"`
	GeneratedAt  time.Time `json:"generated_at"`
}

// WithBriefing enables GET /api/v1/briefing. The route is registered only when
// an action lister and an LLM client are also present, so the feature flag being
// off means the route does not exist (404).
//
// maxActions bounds how many open actions feed one briefing; <= 0 falls back to
// the package default.
func (s *Server) WithBriefing(cache *briefing.Cache, maxActions int) *Server {
	s.briefingCache = cache
	if maxActions <= 0 {
		maxActions = defaultBriefingMaxActions
	}
	s.briefingMaxActions = maxActions
	// The model name participates in the cache key so that swapping models does
	// not serve the previous model's sentences. llm.Completer does not expose
	// the model, and the flag-wiring signature is fixed, so it is read from the
	// same environment variable the LLM client is configured from. An empty
	// value is harmless — it just makes the model a constant in the key.
	s.briefingModel = os.Getenv("LLM_MODEL")
	return s
}

const defaultBriefingMaxActions = 40

// briefingHandler handles GET /api/v1/briefing.
//
// The open action set is re-read on every request: it IS the cache key, so
// there is no way to know whether a cached briefing is still valid without it.
// The LLM call is what the cache protects, not the query.
func (s *Server) briefingHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), briefingTimeout)
	defer cancel()

	items, err := s.actionLister.ListOpenActions(ctx, store.ActionFilter{
		Limit: s.briefingMaxActions,
		Sort:  "due",
	})
	if err != nil {
		slog.Error("briefing: listing open actions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if len(items) == 0 {
		writeJSON(w, http.StatusOK, briefingResponse{
			Sentences:   []briefing.Sentence{},
			ActionCount: 0,
			GeneratedAt: time.Now().UTC(),
		})
		return
	}

	in := briefing.Input{
		Actions: make([]briefing.InputAction, 0, len(items)),
		Model:   s.briefingModel,
	}
	for _, it := range items {
		in.Actions = append(in.Actions, briefing.InputAction{
			IdentityKey:     it.IdentityKey,
			Kind:            string(it.Kind),
			Summary:         it.Summary,
			CounterpartName: it.CounterpartName,
			DocumentID:      it.DocumentID.String(),
			DetectedBy:      string(it.DetectedBy),
			DueAt:           it.DueAt,
			Confidence:      it.Confidence,
		})
		// "Latest document time" is the newest observation among the actions
		// themselves. Querying documents separately would also invalidate the
		// cache for documents that produced no action — which is precisely when
		// the briefing has no reason to change.
		if it.ObservedAt.After(in.LatestDocumentAt) {
			in.LatestDocumentAt = it.ObservedAt
		}
	}

	key := in.CacheKey()
	if cached, ok := s.briefingCache.Get(key); ok {
		writeJSON(w, http.StatusOK, briefingResponse{
			Sentences:    cached.Sentences,
			Degraded:     cached.Degraded,
			DroppedCount: cached.DroppedCount,
			ActionCount:  len(items),
			Cached:       true,
			GeneratedAt:  cached.GeneratedAt,
		})
		return
	}

	started := time.Now()
	// Generate never returns an error for an LLM failure; it degrades.
	result, _ := briefing.Generate(ctx, s.llmClient, in)
	s.briefingCache.Put(key, result)

	// Only content-free fields are logged: counts, a duration, the cache key
	// hash (computed from identity keys alone), and a truncated key prefix. No
	// summary, name, sentence, prompt, or raw model output.
	slog.Info("briefing generated",
		append(briefing.SafeLogFields(in, result),
			// The cache key is a hash of identity keys and timestamps only, so
			// it carries no personal data (see Input.CacheKey).
			"cache_key", key,
			"duration_ms", time.Since(started).Milliseconds())...)

	writeJSON(w, http.StatusOK, briefingResponse{
		Sentences:    result.Sentences,
		Degraded:     result.Degraded,
		DroppedCount: result.DroppedCount,
		ActionCount:  len(items),
		GeneratedAt:  result.GeneratedAt,
	})
}
