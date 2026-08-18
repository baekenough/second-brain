// Package api wires together the HTTP router, middleware, and handlers.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/baekenough/second-brain/internal/briefing"
	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// Ask pipeline defaults (spec §5.2/§6, plan Task 5). Used whenever
// WithAskConfig is not called — NewServer always constructs a working ask
// pipeline, mirroring how /api/v1/search needs no optional builder.
const (
	defaultAskTimeout  = 60 * time.Second
	defaultAskTopK     = 8
	defaultAskInsightM = 3
)

// DocumentStore is the subset of store.DocumentStore used by the API.
type DocumentStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*model.Document, error)
	ListBySource(ctx context.Context, src model.SourceType, limit, offset int) ([]*model.Document, error)
	ListRecent(ctx context.Context, includeSrc model.SourceType, excludeSrcs []model.SourceType, limit, offset int) ([]*model.Document, error)
	CountBySource(ctx context.Context) (map[string]int, error)
	QueryBaselineStats(ctx context.Context) (*store.BaselineStats, error)
}

// Server holds the dependencies needed by all handlers.
type Server struct {
	docs           DocumentStore
	search         *search.Service
	feedback       FeedbackRecorder
	eval           EvalExporter
	llmClient      llm.Completer
	filesystemPath string // root directory for filesystem source documents
	apiKey         string // Bearer token for /api/v1/* routes; empty means disabled

	// reindexState is optional. When non-nil, the /api/v1/reindex route is
	// registered. Set via WithReindexState before calling Handler().
	reindexState ReindexStateRecorder

	// evalMetrics is optional. When non-nil, the GET /api/v1/eval/metrics route
	// is registered. Set via WithEvalMetrics before calling Handler().
	evalMetrics EvalMetricsLister

	// reindexCheck is optional. When non-nil, the GET /api/v1/reindex/check route
	// is registered. Set via WithReindexCheck before calling Handler().
	reindexCheck ReindexRecommender

	// reindexAlertWebhookURL is optional. When non-empty and a reindex is
	// recommended (ShouldReindex=true), a notification is sent to this URL via
	// the same channel used for eval regression alerts (#142).
	// Set via WithReindexAlertWebhook before calling Handler().
	reindexAlertWebhookURL string

	// ingestUpserter, ingestChunks, ingestEmbedder, and ingestMaxFileBytes are
	// optional. When ingestUpserter is non-nil the POST /api/v1/ingest/file
	// route is registered. Set via WithIngestFile before calling Handler().
	ingestUpserter     IngestFileUpserter
	ingestChunks       IngestFileChunkWriter
	ingestEmbedder     IngestFileEmbedder
	ingestMaxFileBytes int64

	// messagesUpserter, messagesChunks, messagesEmbedder, messagesMaxBatch, and
	// messagesCutover are optional. When messagesUpserter is non-nil the
	// POST /api/v1/ingest/messages route is registered.
	// Set via WithIngestMessages before calling Handler().
	messagesUpserter IngestMessagesUpserter
	messagesChunks   IngestFileChunkWriter
	messagesEmbedder IngestFileEmbedder
	messagesMaxBatch int
	messagesCutover  time.Time

	// recordingUpserter, recordingDir, recordingMaxFileBytes, and
	// recordingCutover are optional. When recordingUpserter is non-nil and
	// recordingDir is non-empty the POST /api/v1/ingest/recording route is
	// registered. Set via WithIngestRecording before calling Handler().
	recordingUpserter     IngestRecordingUpserter
	recordingDir          string
	recordingMaxFileBytes int64
	recordingCutover      time.Time

	// collectStatus is optional. When non-nil, the GET /api/v1/collect/status
	// route is registered. Set via WithCollectStatus before calling Handler().
	collectStatus CollectionStatusProvider

	// graph is optional. When non-nil, the four read-only GET
	// /api/v1/graph/* routes are registered. Set via WithGraph before calling
	// Handler(). nil (the default) means the graph feature does not exist in
	// the router at all — which is the rollback path for Part B.
	graph GraphReader

	// notesUpserter, notesChunks, and notesEmbedder are optional. When
	// notesUpserter is non-nil, POST /api/v1/notes,
	// POST /api/v1/notes/{id}/retry-enrichment, and DELETE /api/v1/notes/{id}
	// are registered. Set via WithNotes before calling Handler().
	notesUpserter NotesUpserter
	notesChunks   IngestFileChunkWriter
	notesEmbedder IngestFileEmbedder

	// intentClassifier, askTimeout, askTopK, and askInsightM back the
	// POST /api/v1/ask pipeline (ask backend plan Task 3-5). Unlike the
	// optional With* dependencies above, these are always non-nil/non-zero
	// — set to defaults inside NewServer — because /ask depends only on
	// s.search and s.llmClient, both already unconditional NewServer
	// params (same tier as /api/v1/search, not /api/v1/notes). Override
	// tuning knobs via WithAskConfig.
	intentClassifier intent.Classifier
	askTimeout       time.Duration
	askTopK          int
	askInsightM      int

	// askSessions is optional. When non-nil, POST /api/v1/ask persists each
	// turn and rewrites follow-up questions using prior turns (multi-turn
	// conversations, ask_history.go), and GET /api/v1/ask/conversations +
	// GET /api/v1/ask/conversations/{id} are registered. Set via
	// WithAskSessions before calling Handler(). nil (the default) keeps
	// /api/v1/ask fully functional but single-turn only — see
	// resolveConversation.
	askSessions AskConversationStore

	// now is the clock buildAskSystemPrompt uses to render "current date" in
	// the Stage 3 system prompt (date-context fix: askSystemPrompt used to
	// be a compile-time const with no way to know "today", so the model
	// could not resolve "내일"/"어제" and answered that it didn't know what
	// day it was). nil means time.Now — mirrors intent.LLMClassifier.now's
	// injected-clock convention for deterministic tests.
	now func() time.Time

	// piiNumberHashingEnabled mirrors cfg.PIINumberHashingEnabled (issue #164
	// policy reversal). Zero value (false) is the new default and is used by
	// both ingestMessagesHandler (smsmap.MapSMS/MapCall) and
	// ingestRecordingHandler (sidecar/filename/anonymous-title logic): the raw
	// phone number is written instead of a hash, and (additively) into
	// Metadata["number"] so it stays searchable. Set via
	// WithPIINumberHashing before calling Handler().
	piiNumberHashingEnabled bool

	// actionLister and actionSetter are optional. When actionLister is non-nil
	// the GET /api/v1/actions route is registered; when actionSetter is non-nil
	// the POST /api/v1/actions/{identity_key}/status route is. Set via
	// WithActions before calling Handler(). nil (the default) means the routes
	// do not exist in the router at all — that missing route, answering 404, is
	// exactly what the ACTIONS_API_ENABLED rollback switch buys.
	actionLister ActionLister
	actionSetter ActionStateSetter

	// briefingCache, briefingMaxActions, and briefingModel are optional. When
	// briefingCache is non-nil AND both actionLister and llmClient are present,
	// the GET /api/v1/briefing route is registered. Set via WithBriefing before
	// calling Handler(). The briefing depends on the actions read path, so
	// turning the actions feature off turns the briefing off with it.
	briefingCache      *briefing.Cache
	briefingMaxActions int
	briefingModel      string

	// handlerOnce ensures buildHandler is called exactly once per Server so
	// that the graphql-go schema (and its package-level type objects) are
	// constructed a single time regardless of how many goroutines call Handler.
	handlerOnce   sync.Once
	cachedHandler http.Handler
}

// NewServer creates a Server with the provided dependencies.
func NewServer(
	docs DocumentStore,
	svc *search.Service,
	feedback FeedbackRecorder,
	eval EvalExporter,
	llmClient llm.Completer,
	filesystemPath string,
	apiKey string,
) *Server {
	return &Server{
		docs:           docs,
		search:         svc,
		feedback:       feedback,
		eval:           eval,
		llmClient:      llmClient,
		filesystemPath: filesystemPath,
		apiKey:         apiKey,

		// intentClassifier degrades safely even when llmClient.Enabled()
		// is false (intent.LLMClassifier's LLM fallback path collapses to
		// KindGeneral on any error — spec §4.4), so it is always
		// constructed here rather than gated behind a nil check.
		intentClassifier: intent.NewLLMClassifier(llmClient),
		askTimeout:       defaultAskTimeout,
		askTopK:          defaultAskTopK,
		askInsightM:      defaultAskInsightM,
	}
}

// WithAskConfig sets the ask-pipeline tuning knobs: the overall
// request timeout and the Stage 2 result-count caps (본검색/인사이트검색
// limits). Optional — a Server that never calls this still has a working
// /api/v1/ask route using the package defaults (60s / 8 / 3). Values <= 0
// are ignored (defaults are kept) so a misconfigured env var cannot zero
// out the pipeline.
func (s *Server) WithAskConfig(timeout time.Duration, topK, insightM int) *Server {
	if timeout > 0 {
		s.askTimeout = timeout
	}
	if topK > 0 {
		s.askTopK = topK
	}
	if insightM > 0 {
		s.askInsightM = insightM
	}
	return s
}

// nowFunc returns s.now() if a clock was injected (tests), otherwise the
// real time.Now. Used by askHandler to render buildAskSystemPrompt's
// "current date" line.
func (s *Server) nowFunc() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// WithPIINumberHashing sets the phone-number hashing policy (issue #164;
// cfg.PIINumberHashingEnabled) shared by the ingest-messages and
// ingest-recording handlers. Default false (zero value): the raw number is
// used instead of smsmap.ShortHash / a hashed label, and is additionally
// written into the document Metadata under "number" so it is
// searchable/visible. Pass true to restore the original #164 behaviour
// byte-for-byte. Must be called before the first call to Handler() (though
// the value is read per-request, not at wiring time, so ordering relative to
// the other With* builders does not matter).
func (s *Server) WithPIINumberHashing(enabled bool) *Server {
	s.piiNumberHashingEnabled = enabled
	return s
}

// Handler returns the root http.Handler for the application. It is safe to
// call from multiple goroutines: the underlying router is built exactly once
// via sync.Once and the result is reused for every subsequent call.
func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() {
		s.cachedHandler = s.buildHandler()
	})
	return s.cachedHandler
}

// buildHandler constructs the chi router with all middleware and routes.
// It must only be called once (enforced by Handler via sync.Once).
func (s *Server) buildHandler() http.Handler {
	r := chi.NewRouter()

	// Global middleware
	//
	// middleware.RealIP (chi v5.3.0+) is deprecated: it unconditionally trusts
	// True-Client-IP/X-Real-IP/X-Forwarded-For and mutates r.RemoteAddr, which
	// is spoofable by any client that can reach this service directly
	// (GHSA-3fxj-6jh8-hvhx, GHSA-rjr7-jggh-pgcp / govulncheck GO-2026-5774,
	// GO-2026-5775). Verified this mutation is currently a no-op in this
	// service: r.RemoteAddr is not read anywhere downstream — requestLogger
	// below logs method/path/status/bytes only (no IP), requireAPIKey does a
	// constant-time Bearer-token compare with no IP component, and there is no
	// rate limiter. Kept for now since removing it changes no observable
	// behavior; if RemoteAddr is ever wired into logging, auth, or rate
	// limiting, switch to middleware.ClientIPFromXFFTrustedProxies (with the
	// Cloudflare tunnel's proxy CIDR/header allowlisted) instead of RealIP.
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(recoverer)

	// Health — public, no auth (k8s liveness/readiness probe).
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// API routes — protected by Bearer token when API_KEY is set.
	r.Group(func(r chi.Router) {
		r.Use(requireAPIKey(s.apiKey))

		r.Post("/api/v1/search", s.searchHandler)
		r.Get("/api/v1/search", s.searchGetHandler)

		r.Get("/api/v1/documents", s.listDocumentsHandler)
		r.Get("/api/v1/documents/recent", s.recentDocumentsHandler)
		r.Get("/api/v1/documents/{id}", s.getDocumentHandler)
		r.Get("/api/v1/documents/{id}/raw", s.getDocumentRawHandler)

		r.Get("/api/v1/sources", s.listSourcesHandler)

		r.Get("/api/v1/stats", s.statsHandler)
		r.Get("/api/v1/stats/baseline", s.baselineStatsHandler)

		r.Post("/api/v1/feedback", s.feedbackHandler)

		r.Post("/api/v1/ask", s.askHandler)
		if s.askSessions != nil {
			r.Get("/api/v1/ask/conversations", s.askConversationsHandler)
			r.Get("/api/v1/ask/conversations/{id}", s.askConversationDetailHandler)
		}

		r.Handle("/api/v1/graphql", s.graphqlHandler())

		r.Get("/api/v1/eval/export", s.evalExportHandler)

		if s.evalMetrics != nil {
			r.Get("/api/v1/eval/metrics", s.evalMetricsHandler)
		}
		if s.reindexCheck != nil {
			r.Get("/api/v1/reindex/check", s.reindexCheckHandler)
		}
		if s.reindexState != nil {
			r.Post("/api/v1/reindex", s.reindexHandler)
		}
		if s.collectStatus != nil {
			r.Get("/api/v1/collect/status", s.collectStatusHandler)
		}
		if s.ingestUpserter != nil {
			r.Post("/api/v1/ingest/file", s.ingestFileHandler)
		}
		if s.messagesUpserter != nil {
			r.Post("/api/v1/ingest/messages", s.ingestMessagesHandler)
		}
		if s.recordingUpserter != nil && s.recordingDir != "" {
			r.Post("/api/v1/ingest/recording", s.ingestRecordingHandler)
		}
		if s.graph != nil {
			// GET only — no write route exists, which is the simplest
			// guarantee that this API cannot change the projection.
			r.Get("/api/v1/graph/entry", s.graphEntryHandler)
			r.Get("/api/v1/graph/expand", s.graphExpandHandler)
			r.Get("/api/v1/graph/evidence", s.graphEvidenceHandler)
			r.Get("/api/v1/graph/entities", s.graphEntitiesHandler)
		}
		if s.actionLister != nil {
			r.Get("/api/v1/actions", s.listActionsHandler)
		}
		if s.actionSetter != nil {
			r.Post("/api/v1/actions/{identity_key}/status", s.setActionStateHandler)
		}
		if s.briefingCache != nil && s.actionLister != nil && s.llmClient != nil {
			r.Get("/api/v1/briefing", s.briefingHandler)
		}
		if s.notesUpserter != nil {
			r.Post("/api/v1/notes", s.createNoteHandler)
			r.Post("/api/v1/notes/{id}/retry-enrichment", s.retryEnrichmentHandler)
			r.Delete("/api/v1/notes/{id}", s.deleteNoteHandler)
		}
	})

	return r
}

// --- shared response helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("api: failed to encode JSON response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- middleware implementations ---

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
		)
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				slog.Error("http: panic recovered",
					"panic", rv,
					"stack", string(debug.Stack()),
				)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
