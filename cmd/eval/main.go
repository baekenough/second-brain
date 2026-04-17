// Command eval runs the nightly evaluation pipeline.
// It builds eval pairs from positive feedback, runs the search service against
// each pair, computes NDCG@5, NDCG@10, and MRR@10 metrics, persists them to
// the eval_metrics table, and compares against the previous baseline.
//
// Exit codes:
//
//	0 — success (metrics within acceptable bounds, or no baseline exists yet)
//	1 — regression detected (any metric dropped more than 5% relative to baseline)
//	2 — reindex recommended (--check-reindex flag set and thresholds breached)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/sync/errgroup"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
)

// errRegression is returned by run() when a metric regression is detected.
// main() maps this to os.Exit(1) so that deferred cleanup runs normally.
var errRegression = errors.New("regression detected")

// errReindexRecommended is returned by run() when a reindex is recommended.
// main() maps this to os.Exit(2).
var errReindexRecommended = errors.New("reindex recommended")

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		switch {
		case errors.Is(err, errRegression):
			// Regression already logged inside run(); exit with code 1.
			os.Exit(1)
		case errors.Is(err, errReindexRecommended):
			// Reindex recommendation already logged inside run(); exit with code 2.
			os.Exit(2)
		default:
			slog.Error("eval failed", "error", err)
			os.Exit(1)
		}
	}
}

// evalOutput is the JSON report written to stdout.
type evalOutput struct {
	Current    metricsSnapshot              `json:"current"`
	Baseline   *metricsSnapshot             `json:"baseline"`
	Regression bool                         `json:"regression"`
	Deltas     map[string]float64           `json:"deltas,omitempty"`
	Reindex    *search.ReindexRecommendation `json:"reindex,omitempty"` // populated when --check-reindex is set
}

type metricsSnapshot struct {
	NDCG5  float64 `json:"ndcg5"`
	NDCG10 float64 `json:"ndcg10"`
	MRR10  float64 `json:"mrr10"`
	Pairs  int     `json:"pairs"`
}

func run() error {
	// Parse flags before doing any work so that --help works cleanly.
	checkReindex := flag.Bool("check-reindex", false,
		"evaluate reindex thresholds after computing eval metrics and include "+
			"the recommendation in the JSON output (exit code 2 when reindex is recommended)")
	flag.Parse()

	// Load .env file if present (ignore error — env vars may be set directly).
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Database ---
	pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()

	migrationsDir := migrationsPath()
	if err := pg.RunMigrations(ctx, migrationsDir); err != nil {
		return err
	}

	// --- Stores ---
	docStore := store.NewDocumentStore(pg)
	chunkStore := store.NewChunkStore(pg)
	evalStore := store.NewEvalStore(pg)
	metricsStore := store.NewEvalMetricsStore(pg)

	// --- Embedding client ---
	embedClient := search.NewEmbedClient(
		cfg.EmbeddingAPIURL,
		cfg.EmbeddingAPIKey,
		cfg.CliProxyAuthFile,
		cfg.EmbeddingModel,
	)

	// --- Reranker (optional) ---
	reranker := search.NewHTTPReranker(cfg.RerankURL, cfg.RerankAPIKey, cfg.RerankModel, 0)

	// --- Search service ---
	searchSvc := search.NewService(docStore, embedClient).
		WithChunkStore(chunkStore).
		WithReranker(reranker)

	// --- Build eval pairs ---
	pairs, err := evalStore.BuildFromFeedback(ctx)
	if err != nil {
		return fmt.Errorf("build eval pairs: %w", err)
	}
	if len(pairs) == 0 {
		slog.Warn("eval: no eval pairs found — skipping evaluation")
		return nil
	}
	slog.Info("eval: pairs loaded", "count", len(pairs))

	// Load baseline BEFORE running the current eval so we compare against the
	// previous run, not the one we are about to save.
	baseline, err := metricsStore.Latest(ctx)
	if err != nil {
		return fmt.Errorf("load baseline metrics: %w", err)
	}

	// --- Run search for each pair (bounded parallel) ---
	type evalResult struct {
		docIDs   []string
		relevant map[string]bool
	}

	resultsCh := make(chan evalResult, len(pairs))

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(10) // max 10 concurrent searches

	for _, pair := range pairs {
		pair := pair // capture loop variable
		g.Go(func() error {
			q := model.SearchQuery{
				Query: pair.Query,
				Limit: 10, // evaluate top-10
			}

			searchResults, err := searchSvc.Search(gCtx, q)
			if err != nil {
				// Non-fatal: log and skip the pair rather than aborting the whole run.
				slog.Warn("eval: search failed for pair", "query", pair.Query, "error", err)
				return nil
			}

			docIDs := make([]string, 0, len(searchResults))
			for _, r := range searchResults {
				docIDs = append(docIDs, r.ID.String())
			}

			relevant := make(map[string]bool, len(pair.RelevantDocIDs))
			for _, id := range pair.RelevantDocIDs {
				relevant[id] = true
			}

			resultsCh <- evalResult{docIDs: docIDs, relevant: relevant}
			return nil
		})
	}

	// Close the channel once all goroutines finish.
	go func() {
		_ = g.Wait()
		close(resultsCh)
	}()

	// Collect results from the channel.
	results := make([][]string, 0, len(pairs))
	relevantSets := make([]map[string]bool, 0, len(pairs))
	for r := range resultsCh {
		results = append(results, r.docIDs)
		relevantSets = append(relevantSets, r.relevant)
	}

	if len(results) == 0 {
		slog.Warn("eval: all searches failed — no metrics to compute")
		return nil
	}

	// --- Compute aggregate metrics ---
	metrics := search.Aggregate(results, relevantSets)
	slog.Info("eval: metrics computed",
		"ndcg5", metrics.NDCG5,
		"ndcg10", metrics.NDCG10,
		"mrr10", metrics.MRR10,
		"pairs", metrics.Pairs,
	)

	// --- Persist current run ---
	if err := metricsStore.Save(ctx, store.EvalMetricsRecord{
		NDCG5:  metrics.NDCG5,
		NDCG10: metrics.NDCG10,
		MRR10:  metrics.MRR10,
		Pairs:  metrics.Pairs,
	}); err != nil {
		return fmt.Errorf("save eval metrics: %w", err)
	}

	// --- Build output ---
	current := metricsSnapshot{
		NDCG5:  metrics.NDCG5,
		NDCG10: metrics.NDCG10,
		MRR10:  metrics.MRR10,
		Pairs:  metrics.Pairs,
	}

	out := evalOutput{Current: current}

	if baseline != nil {
		base := metricsSnapshot{
			NDCG5:  baseline.NDCG5,
			NDCG10: baseline.NDCG10,
			MRR10:  baseline.MRR10,
			Pairs:  baseline.Pairs,
		}
		out.Baseline = &base
		out.Deltas, out.Regression = computeDeltas(current, base)
	}

	// --- Optional: reindex threshold check ---
	if *checkReindex {
		stateStore := store.NewReindexStateStore(pg)
		checker := search.NewReindexChecker(
			search.DefaultReindexConfig(),
			metricsStore,
			docStore,
			stateStore,
		)

		var rec search.ReindexRecommendation
		var recErr error
		if baseline != nil && out.Baseline != nil {
			// Use CheckWithBaseline to also evaluate eval regression.
			rec, recErr = checker.CheckWithBaseline(ctx,
				current.NDCG5, out.Baseline.NDCG5,
				current.NDCG10, out.Baseline.NDCG10,
				current.MRR10, out.Baseline.MRR10,
			)
		} else {
			rec, recErr = checker.Check(ctx)
		}
		if recErr != nil {
			slog.Warn("reindex check failed", "error", recErr)
		} else {
			out.Reindex = &rec
		}
	}

	// --- Write JSON report to stdout ---
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}

	// --- Webhook alert (non-blocking) ---
	// Send an alert when a regression is detected and a webhook URL is configured.
	if out.Regression && cfg.AlertWebhookURL != "" {
		sendWebhookAlert(cfg.AlertWebhookURL, out)
	}

	// --- Determine exit condition ---
	// Check reindex recommendation and regression AFTER all output is written and
	// all deferred cleanup (pg.Close, cancel) can run via normal return paths.
	if out.Regression {
		slog.Error("eval: regression detected", "deltas", out.Deltas)
		return errRegression
	}

	if out.Reindex != nil && out.Reindex.ShouldReindex {
		slog.Warn("eval: reindex recommended", "reasons", out.Reindex.Reasons)
		return errReindexRecommended
	}

	slog.Info("eval: completed successfully")
	return nil
}

// webhookPayload is a Slack-compatible incoming webhook message.
type webhookPayload struct {
	Text string `json:"text"`
}

// sendWebhookAlert POSTs a Slack-compatible alert to webhookURL.
// It is non-blocking: failures are logged but do not affect the eval exit code.
func sendWebhookAlert(webhookURL string, out evalOutput) {
	text := fmt.Sprintf(
		":warning: *Eval regression detected*\n"+
			"NDCG@5: %.4f (Δ %.4f) | NDCG@10: %.4f (Δ %.4f) | MRR@10: %.4f (Δ %.4f)",
		out.Current.NDCG5, out.Deltas["ndcg5"],
		out.Current.NDCG10, out.Deltas["ndcg10"],
		out.Current.MRR10, out.Deltas["mrr10"],
	)

	payload := webhookPayload{Text: text}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("eval: webhook: failed to marshal payload", "error", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("eval: webhook: failed to send alert", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		slog.Warn("eval: webhook: alert returned non-2xx status", "status", resp.StatusCode)
	}
}

// regressionThreshold is the maximum allowed relative metric drop (5%).
const regressionThreshold = 0.05

// computeDeltas returns per-metric deltas (current - baseline) and whether any
// metric regressed by more than regressionThreshold relative to the baseline.
// A metric that was 0 in the baseline is skipped (no valid denominator).
func computeDeltas(current, baseline metricsSnapshot) (map[string]float64, bool) {
	deltas := map[string]float64{
		"ndcg5":  current.NDCG5 - baseline.NDCG5,
		"ndcg10": current.NDCG10 - baseline.NDCG10,
		"mrr10":  current.MRR10 - baseline.MRR10,
	}

	regression := false
	type pair struct {
		cur, base float64
	}
	checks := []pair{
		{current.NDCG5, baseline.NDCG5},
		{current.NDCG10, baseline.NDCG10},
		{current.MRR10, baseline.MRR10},
	}
	for _, p := range checks {
		if p.base == 0 {
			continue
		}
		relativeDrop := (p.base - p.cur) / p.base
		if relativeDrop >= regressionThreshold {
			regression = true
			break
		}
	}

	return deltas, regression
}

// migrationsPath returns the path to the migrations directory.
// Resolution order:
//  1. MIGRATIONS_DIR env var (useful in Docker/k8s where -trimpath strips source paths)
//  2. runtime.Caller(0) relative path (works for go run / local dev builds)
//  3. "migrations" — CWD-relative fallback (used when WORKDIR=/app and migrations/ is there)
func migrationsPath() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "migrations"
	}
	// When built with -trimpath, filename is a module-relative path
	// (e.g. github.com/baekenough/second-brain/cmd/eval/main.go) which is not
	// a real filesystem path. Detect this and fall back to CWD-relative path.
	if !filepath.IsAbs(filename) {
		return "migrations"
	}
	// filename is cmd/eval/main.go; walk up two levels to reach project root.
	root := filepath.Join(filepath.Dir(filename), "..", "..")
	return filepath.Join(root, "migrations")
}
