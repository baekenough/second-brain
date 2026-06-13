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

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
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
	}
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
