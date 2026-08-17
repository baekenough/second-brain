// Package intent implements Stage 1 of the /ask pipeline: classifying a
// user's natural-language question into a Kind that Stage 2 uses to shape
// document retrieval (entity-weighted search, recency sort, exact-token
// matching, or a plain general search).
//
// Classification never fails fatally: LLMClassifier.Classify always
// degrades to KindGeneral on any LLM error or malformed response so the
// /ask pipeline can proceed with a best-effort search rather than aborting
// the whole request over a classification hiccup.
package intent

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/llm"
)

// Kind describes how Stage 2 should shape the retrieval query for a
// classified question.
type Kind string

const (
	// KindGeneral is the default/fallback classification: no special
	// retrieval shaping is applied.
	KindGeneral Kind = "general"
	// KindEntity indicates the question centers on a named entity
	// (person, place, org); Stage 2 boosts the entity RRF lane.
	KindEntity Kind = "entity"
	// KindTemporal indicates the question references a specific time
	// window; Stage 2 approximates this via recency sort only — there is
	// no date-range filter on model.SearchQuery yet.
	KindTemporal Kind = "temporal"
	// KindExactToken indicates the question contains a phone-number-shaped
	// token; Stage 2 nudges the bigram-similarity lane.
	KindExactToken Kind = "exact_token"
)

// Params is the result of Stage 1 classification.
type Params struct {
	RawQuery     string
	Kind         Kind
	EntityName   string
	OccurredFrom *time.Time
	OccurredTo   *time.Time
	Confidence   float64
}

// Classifier classifies a raw user question into Params. The error return
// exists for interface generality (future implementations, tests); the
// only shipped implementation, LLMClassifier, never returns a non-nil
// error — see LLMClassifier.Classify.
type Classifier interface {
	Classify(ctx context.Context, question string) (Params, error)
}

// LLMClassifier runs a deterministic regex pre-pass first (phone numbers,
// Korean date phrases); anything that doesn't match falls back to a single
// LLM call that distinguishes "general" from "entity" questions.
type LLMClassifier struct {
	llm llm.Completer
	now func() time.Time // injected for deterministic date-phrase tests; nil means time.Now
}

// NewLLMClassifier returns an LLMClassifier backed by c for the LLM
// fallback path.
func NewLLMClassifier(c llm.Completer) *LLMClassifier {
	return &LLMClassifier{llm: c}
}

var (
	phoneRe     = regexp.MustCompile(`\d{2,3}-?\d{3,4}-?\d{4}`)
	yearMonthRe = regexp.MustCompile(`(\d{4})년\s*(\d{1,2})월`)
	lastMonthRe = regexp.MustCompile(`지난달`)
	thisWeekRe  = regexp.MustCompile(`이번\s?주`)
	todayRe     = regexp.MustCompile(`오늘`)
	yesterdayRe = regexp.MustCompile(`어제`)
)

const classifySystemPrompt = `You classify a user's Korean question for a personal-knowledge search assistant.
Respond with ONLY a JSON object, no markdown fences, no extra text:
{"kind":"general"|"entity","entity_name":"<name or empty string>","confidence":0.0-1.0}
Use "entity" only when the question centers on a specific named person, place, or organization.
Otherwise use "general".`

// Classify runs the deterministic pre-pass first, falling back to an LLM
// call when nothing matches. It never returns a non-nil error: every
// failure path (LLM error, malformed response) degrades to KindGeneral.
func (c *LLMClassifier) Classify(ctx context.Context, question string) (Params, error) {
	now := c.nowFunc()

	if phoneRe.MatchString(question) {
		return Params{RawQuery: question, Kind: KindExactToken, Confidence: 1.0}, nil
	}

	if m := yearMonthRe.FindStringSubmatch(question); m != nil {
		year, errY := strconv.Atoi(m[1])
		month, errM := strconv.Atoi(m[2])
		if errY == nil && errM == nil && month >= 1 && month <= 12 {
			from, to := monthRange(year, month, now.Location())
			return Params{RawQuery: question, Kind: KindTemporal, OccurredFrom: &from, OccurredTo: &to, Confidence: 1.0}, nil
		}
	}

	if lastMonthRe.MatchString(question) {
		lm := now.AddDate(0, -1, 0)
		from, to := monthRange(lm.Year(), int(lm.Month()), now.Location())
		return Params{RawQuery: question, Kind: KindTemporal, OccurredFrom: &from, OccurredTo: &to, Confidence: 1.0}, nil
	}

	if thisWeekRe.MatchString(question) {
		from, to := weekRange(now)
		return Params{RawQuery: question, Kind: KindTemporal, OccurredFrom: &from, OccurredTo: &to, Confidence: 1.0}, nil
	}

	if todayRe.MatchString(question) {
		from, to := dayRange(now)
		return Params{RawQuery: question, Kind: KindTemporal, OccurredFrom: &from, OccurredTo: &to, Confidence: 1.0}, nil
	}

	if yesterdayRe.MatchString(question) {
		y := now.AddDate(0, 0, -1)
		from, to := dayRange(y)
		return Params{RawQuery: question, Kind: KindTemporal, OccurredFrom: &from, OccurredTo: &to, Confidence: 1.0}, nil
	}

	return c.classifyWithLLM(ctx, question)
}

func (c *LLMClassifier) nowFunc() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// classifyWithLLM asks the LLM to distinguish "entity" from "general".
// Any failure (call error, nil client, malformed JSON) degrades to
// KindGeneral — Classify's contract never surfaces an error to the caller.
func (c *LLMClassifier) classifyWithLLM(ctx context.Context, question string) (Params, error) {
	degraded := Params{RawQuery: question, Kind: KindGeneral, Confidence: 0}

	if c.llm == nil {
		return degraded, nil
	}

	raw, err := c.llm.CompleteWithMessages(ctx, classifySystemPrompt, []llm.Message{
		{Role: "user", Content: question},
	})
	if err != nil {
		return degraded, nil
	}

	var parsed struct {
		Kind       string  `json:"kind"`
		EntityName string  `json:"entity_name"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &parsed); err != nil {
		return degraded, nil
	}

	kind := KindGeneral
	if parsed.Kind == "entity" {
		kind = KindEntity
	}

	return Params{
		RawQuery:   question,
		Kind:       kind,
		EntityName: parsed.EntityName,
		Confidence: parsed.Confidence,
	}, nil
}

// extractJSON returns the substring between the first '{' and the last
// '}' in s, tolerating LLM responses wrapped in markdown code fences or
// surrounded by extra prose. Returns s unchanged if no braces are found.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start == -1 || end == -1 || end < start {
		return s
	}
	return s[start : end+1]
}

// monthRange returns [00:00:00 on day 1, 23:59:59 on the last day] of the
// given year/month in loc. The last-day computation uses the standard Go
// idiom: day 0 of the following month is the last day of the given month.
func monthRange(year, month int, loc *time.Location) (time.Time, time.Time) {
	from := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc)
	to := time.Date(year, time.Month(month)+1, 0, 23, 59, 59, 0, loc)
	return from, to
}

// dayRange returns [00:00:00, 23:59:59] for the calendar day containing t.
func dayRange(t time.Time) (time.Time, time.Time) {
	from := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	to := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location())
	return from, to
}

// weekRange returns [Monday 00:00:00, Sunday 23:59:59] of the ISO week
// containing t.
func weekRange(t time.Time) (time.Time, time.Time) {
	// time.Weekday: Sunday = 0 ... Saturday = 6. Convert to days since Monday.
	daysSinceMonday := (int(t.Weekday()) + 6) % 7
	monday := t.AddDate(0, 0, -daysSinceMonday)
	from, _ := dayRange(monday)
	sunday := monday.AddDate(0, 0, 6)
	_, to := dayRange(sunday)
	return from, to
}
