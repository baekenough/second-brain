package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// QueryPlan is the answer to "what must be fetched to answer this question".
// Both the deterministic path and the LLM path produce this one type — see
// LLMPlanner's doc comment for why that matters more than which path ran.
//
// The zero value is a valid unconstrained plan except for Limit, which callers
// must treat as "use your own default" when zero (assembleRetrieval does).
type QueryPlan struct {
	// OccurredFrom / OccurredTo are the half-open event-time window
	// [From, To) on documents.occurred_at. Both nil means no time constraint.
	// The half-open convention is the same one dayRange/weekRange/monthRange
	// return and the same one internal/store binds to
	// `occurred_at >= $from AND occurred_at < $to`; an inclusive upper bound
	// would drop the period's final second and leave gaps between consecutive
	// windows.
	OccurredFrom *time.Time
	OccurredTo   *time.Time

	// SourceTypes is the include set. Empty means "all sources", NOT "no
	// sources". It never contains model.SourceInsight — see stripInsight.
	SourceTypes []model.SourceType

	// Limit is the candidate cap for the observed lane.
	Limit int

	// Reason is a one-line, user-facing rationale ("내일(8/20) 캘린더만 조회").
	//
	// PRIVACY (design spec §9): on the LLM path this string is written by the
	// model from the user's question and can therefore contain names, phone
	// numbers, or any other personal detail the question carried. It is
	// returned to the asking user and to nobody else: never log it, never use
	// it as a metric label, never put it in an error. Log len(Reason) if you
	// need a signal at all (same rule as llm.TruncatedError, issue #194).
	Reason string

	// Origin is OriginDeterministic, OriginLLM, or OriginFallback. It exists
	// so a wrong plan can be diagnosed at all: a fallback spike means the LLM
	// path is down, an "llm" plan with an empty window means the prompt is
	// wrong, and a "deterministic" plan with a wrong window means the regex or
	// the timezone is wrong. Those are three different repairs and without
	// this field they look identical from the outside.
	Origin string
}

// Origin values for QueryPlan.Origin.
const (
	// OriginDeterministic: a regex matched and no LLM call was made.
	OriginDeterministic = "deterministic"
	// OriginLLM: the model produced a schema-valid plan.
	OriginLLM = "llm"
	// OriginFallback: planning failed and the plan is unconstrained. This is
	// the pre-planner behaviour, not a new failure mode (spec §6.1).
	OriginFallback = "fallback"
)

// Planner turns one question into exactly one QueryPlan.
//
// There is no error return, by design: every failure inside a planner is
// absorbed into an unconstrained fallback plan (spec §6), because a /ask
// request that cannot be planned is still answerable — just not narrowed.
// Callers that need to know planning failed read Origin.
type Planner interface {
	Plan(ctx context.Context, question string) QueryPlan
}

// LLMPlanner implements Planner with a deterministic regex pre-pass plus a
// single LLM call for everything the pre-pass misses.
//
// The regexes are a CACHE for common phrasings, not a second set of
// semantics. That distinction is the whole architecture: the original defect
// was that "오늘" produced a date window and "내일" — absent from the regex
// list — produced no window at all, so a missing vocabulary entry silently
// became "search everything, ranked by meaning", and a three-month-old call
// transcript outranked tomorrow's calendar entry. Because both paths now
// return a QueryPlan, a regex miss costs one LLM round-trip instead of
// deleting the filter.
//
// REQUIRED DEPLOYMENT CONDITION — LLM_THINKING=disabled (spec §4.2).
// The remote model bills reasoning_content against the same max_tokens budget
// as the visible answer. With reasoning enabled, the ~750-token budget this
// prompt is sized for is consumed before a single content byte is emitted and
// the client returns *llm.TruncatedError (finish_reason="length", content
// length 0). This planner treats that as a failure and falls back — correctly,
// but on EVERY call, which turns the feature off while leaving no user-visible
// error at all. Raising max_tokens instead costs ~17x the tokens
// (measured; see internal/llm's thinking documentation).
type LLMPlanner struct {
	llm   llm.Completer
	now   func() time.Time // injected for deterministic tests; nil means time.Now
	limit int

	// deterministicDisabled is set only by tests (export_test.go), so the
	// golden set can be driven through the LLM path with the same table the
	// deterministic path is held to (spec §4.3/§8).
	deterministicDisabled bool
}

// NewLLMPlanner returns a planner backed by c, stamping every plan with
// defaultLimit. c may be nil or disabled: planning then always falls back.
func NewLLMPlanner(c llm.Completer, defaultLimit int) *LLMPlanner {
	return &LLMPlanner{llm: c, limit: defaultLimit}
}

var (
	// Matched in the order listed in Plan. Order is load-bearing where one
	// pattern is a prefix of another: `이번 주말` must be tested before
	// `이번 주`, and `모레` before `내일` so that "내일모레" resolves to the day
	// after tomorrow rather than to tomorrow.
	weekendRe   = regexp.MustCompile(`이번\s*주말`)
	nextWeekRe  = regexp.MustCompile(`다음\s*주`)
	tomorrowRe  = regexp.MustCompile(`내일`)
	dayAfterRe  = regexp.MustCompile(`모레`)
	calendarKwR = regexp.MustCompile(`일정|스케줄|캘린더|약속`)
)

// planSourceTypes is the enumeration the LLM is allowed to choose from and the
// validator for what it returns. One list serves both so the prompt and the
// check cannot drift apart.
//
// model.SourceInsight is deliberately absent: insight documents are
// model-derived inference, they are retrieved by a separate fixed lane, and a
// plan naming them would empty the observed lane — which askHandler reports as
// finish_reason="no_evidence" regardless of how much insight material came
// back. See stripInsight.
var planSourceTypes = []model.SourceType{
	model.SourceCalendar,
	model.SourceGmail,
	model.SourceSMS,
	model.SourceCallLog,
	model.SourceCallTranscript,
	model.SourceNote,
	model.SourceUpload,
	model.SourceSecretary,
	model.SourceLLMMemory,
	model.SourceFilesystem,
	model.SourceSlack,
	model.SourceGitHub,
	model.SourceGDrive,
	model.SourceNotion,
	model.SourceDiscord,
	model.SourceTelegram,
}

// Plan implements Planner. It runs the deterministic pre-pass, then the LLM
// path, and absorbs every failure into an unconstrained fallback plan.
func (p *LLMPlanner) Plan(ctx context.Context, question string) QueryPlan {
	// Normalise to KST before any calendar arithmetic: the server container
	// runs Etc/UTC, so for the first nine hours of every Korean day the raw
	// clock names the PREVIOUS day (and, at a month edge, the previous month).
	now := p.nowFunc().In(timeutil.KST())

	if !p.deterministicDisabled {
		if plan, ok := p.deterministicPlan(question, now); ok {
			return plan
		}
	}
	return p.llmPlan(ctx, question, now)
}

func (p *LLMPlanner) nowFunc() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// deterministicPlan resolves the nine phrasings of spec §4.1 without an LLM
// call. ok is false when nothing matched.
func (p *LLMPlanner) deterministicPlan(question string, now time.Time) (QueryPlan, bool) {
	var (
		from, to time.Time
		label    string
	)

	switch {
	case yearMonthRe.MatchString(question):
		m := yearMonthRe.FindStringSubmatch(question)
		year, errY := strconv.Atoi(m[1])
		month, errM := strconv.Atoi(m[2])
		if errY != nil || errM != nil || month < 1 || month > 12 {
			return QueryPlan{}, false
		}
		from, to = monthRange(year, month)
		label = fmt.Sprintf("%d년 %d월", year, month)
	case lastMonthRe.MatchString(question):
		lm := now.AddDate(0, -1, 0)
		from, to = monthRange(lm.Year(), int(lm.Month()))
		label = "지난달"
	case weekendRe.MatchString(question):
		// This week's Saturday and Sunday: Monday+5 up to Monday+7.
		weekFrom, _ := weekRange(now)
		from, _ = dayRange(weekFrom.AddDate(0, 0, 5))
		_, to = dayRange(weekFrom.AddDate(0, 0, 6))
		label = "이번 주말"
	case nextWeekRe.MatchString(question):
		_, thisWeekEnd := weekRange(now)
		from, to = weekRange(thisWeekEnd)
		label = "다음 주"
	case thisWeekRe.MatchString(question):
		from, to = weekRange(now)
		label = "이번 주"
	case dayAfterRe.MatchString(question):
		from, to = dayRange(now.AddDate(0, 0, 2))
		label = "모레"
	case tomorrowRe.MatchString(question):
		from, to = dayRange(now.AddDate(0, 0, 1))
		label = "내일"
	case todayRe.MatchString(question):
		from, to = dayRange(now)
		label = "오늘"
	case yesterdayRe.MatchString(question):
		from, to = dayRange(now.AddDate(0, 0, -1))
		label = "어제"
	default:
		return QueryPlan{}, false
	}

	sources := p.deterministicSources(question, from, now)
	return QueryPlan{
		OccurredFrom: &from,
		OccurredTo:   &to,
		SourceTypes:  sources,
		Limit:        p.limit,
		Reason:       planReason(label, from, to, sources),
		Origin:       OriginDeterministic,
	}, true
}

// deterministicSources decides the include set for a date-matched question.
//
// It narrows in only two situations, both of which are safe to state without a
// model, because narrowing is destructive: an over-narrow include set turns a
// good answer into zero results, and the pre-planner behaviour (no filter) is
// always a valid, merely-wider plan. Everything more nuanced is left to the
// LLM path.
//
//  1. The question names a calendar concept outright (일정/스케줄/캘린더/약속).
//  2. The window lies entirely in the future. Nothing that has not happened yet
//     can be observed in a message, a call, or a mail thread; the only source
//     that records future events is the calendar. This is what makes
//     "이번 주말에 뭐 하지" — which names no source at all — a calendar question.
func (p *LLMPlanner) deterministicSources(question string, from, now time.Time) []model.SourceType {
	if calendarKwR.MatchString(question) {
		return []model.SourceType{model.SourceCalendar}
	}
	tomorrowStart, _ := dayRange(now.AddDate(0, 0, 1))
	if !from.Before(tomorrowStart) {
		return []model.SourceType{model.SourceCalendar}
	}
	return nil
}

// planSystemPrompt is the LLM path's system prompt. %s placeholders are, in
// order: the current time in KST, today's KST date, and the allowed
// source_type enumeration.
//
// The current time is not decoration — the model cannot resolve "내일" without
// it, and it must be stated in KST because the process clock is UTC.
const planSystemPrompt = `You plan document retrieval for a Korean personal-knowledge assistant.
Current time: %s (KST). Today's date is %s in KST.

Respond with ONLY a JSON object, no markdown fences, no prose:
{"occurred_from":"YYYY-MM-DD","occurred_to":"YYYY-MM-DD","source_types":["..."],"reason":"<한국어 한 줄>"}

Rules:
1. occurred_from/occurred_to describe a HALF-OPEN date window [from, to) in KST: occurred_to is the day AFTER the last day you want. One single day D is {"occurred_from":"D","occurred_to":"D+1"}.
2. If the question contains NO explicit time expression, set BOTH to "". Vague words such as "최근", "요즘", "예전에" are NOT explicit: do not invent a window for them. An invented window hides documents the user asked for.
3. source_types may only contain values from this list: %s. Use it only when the question clearly names a kind of record. Leave it [] when unsure — an empty list means "search everything", and a wrong narrow list returns nothing at all.
4. Questions about the future (tomorrow, this weekend, next week) can only be answered by "calendar" documents; nothing that has not happened yet appears in messages, calls or mail.
5. reason: one short Korean sentence stating what will be searched, e.g. "내일(8/20) 캘린더만 조회".`

// planResponse is the LLM path's wire schema. Field names are the contract
// stated in planSystemPrompt.
type planResponse struct {
	OccurredFrom string   `json:"occurred_from"`
	OccurredTo   string   `json:"occurred_to"`
	SourceTypes  []string `json:"source_types"`
	Reason       string   `json:"reason"`
}

// llmPlan makes at most one LLM call (spec §2.3) and never retries: a
// truncated response fails identically against the same budget and would bill
// a full generation per attempt.
func (p *LLMPlanner) llmPlan(ctx context.Context, question string, now time.Time) QueryPlan {
	if p.llm == nil || !p.llm.Enabled() {
		return p.fallbackPlan()
	}

	names := make([]string, 0, len(planSourceTypes))
	for _, st := range planSourceTypes {
		names = append(names, string(st))
	}
	system := fmt.Sprintf(planSystemPrompt,
		now.Format("2006-01-02 15:04"),
		now.Format("2006-01-02"),
		strings.Join(names, ", "),
	)

	raw, err := p.llm.CompleteWithMessages(ctx, system, []llm.Message{{Role: "user", Content: question}})
	if err != nil {
		// Includes *llm.TruncatedError, which is what an accidentally-enabled
		// reasoning mode produces on every single call.
		return p.fallbackPlan()
	}

	var parsed planResponse
	if err := json.Unmarshal([]byte(extractJSON(raw)), &parsed); err != nil {
		return p.fallbackPlan()
	}

	from, to, ok := parseWindow(parsed.OccurredFrom, parsed.OccurredTo)
	if !ok {
		return p.fallbackPlan()
	}

	sources, ok := parseSourceTypes(parsed.SourceTypes)
	if !ok {
		return p.fallbackPlan()
	}
	sources, widened := stripInsight(sources)

	reason := strings.TrimSpace(parsed.Reason)
	if reason == "" {
		reason = planReasonFromWindow(from, to, sources)
	}
	if widened {
		// Disclosed widening only (spec §6.2): the user is told the narrow
		// request could not be honoured, instead of quietly receiving the
		// whole corpus under a Reason that claims otherwise.
		reason = "추론(insight) 전용 조회는 지원하지 않아 전체 소스로 조회"
	}

	return QueryPlan{
		OccurredFrom: from,
		OccurredTo:   to,
		SourceTypes:  sources,
		Limit:        p.limit,
		Reason:       reason,
		Origin:       OriginLLM,
	}
}

// fallbackPlan is spec §6.1: no window, no source filter, the caller's default
// limit. Identical to the behaviour before this feature existed.
func (p *LLMPlanner) fallbackPlan() QueryPlan {
	return QueryPlan{
		Limit:  p.limit,
		Reason: "시간·소스 제약 없이 전체 검색",
		Origin: OriginFallback,
	}
}

// parseWindow converts two "YYYY-MM-DD" KST dates into a half-open window.
// ok is false for a malformed date or a non-positive interval; the caller
// falls back rather than searching a window that can never match.
func parseWindow(fromStr, toStr string) (from, to *time.Time, ok bool) {
	loc := timeutil.KST()
	parse := func(s string) (*time.Time, bool) {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, true
		}
		t, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			return nil, false
		}
		return &t, true
	}

	from, ok = parse(fromStr)
	if !ok {
		return nil, nil, false
	}
	to, ok = parse(toStr)
	if !ok {
		return nil, nil, false
	}
	if from != nil && to != nil && !to.After(*from) {
		return nil, nil, false
	}
	return from, to, true
}

// parseSourceTypes validates the model's source list against planSourceTypes.
// A single unrecognised value fails the whole plan (spec §6): the model has
// demonstrably misunderstood the enumeration, and guessing which of the
// remaining entries it meant is how a plan quietly becomes wrong instead of
// visibly absent. model.SourceInsight is accepted here and removed by
// stripInsight, because it is a real source type, just not a plannable one.
func parseSourceTypes(names []string) ([]model.SourceType, bool) {
	if len(names) == 0 {
		return nil, true
	}
	allowed := make(map[model.SourceType]struct{}, len(planSourceTypes)+1)
	for _, st := range planSourceTypes {
		allowed[st] = struct{}{}
	}
	allowed[model.SourceInsight] = struct{}{}

	out := make([]model.SourceType, 0, len(names))
	seen := make(map[model.SourceType]struct{}, len(names))
	for _, n := range names {
		st := model.SourceType(strings.TrimSpace(n))
		if _, okType := allowed[st]; !okType {
			return nil, false
		}
		if _, dup := seen[st]; dup {
			continue
		}
		seen[st] = struct{}{}
		out = append(out, st)
	}
	return out, true
}

// stripInsight removes model.SourceInsight from an include set. widened
// reports that the set was non-empty and became empty — i.e. the plan asked
// for insight and nothing else, and dropping it turns the query into a
// whole-corpus search that the caller must disclose rather than perform
// silently.
func stripInsight(sources []model.SourceType) (kept []model.SourceType, widened bool) {
	if len(sources) == 0 {
		return nil, false
	}
	kept = make([]model.SourceType, 0, len(sources))
	for _, st := range sources {
		if st == model.SourceInsight {
			continue
		}
		kept = append(kept, st)
	}
	if len(kept) == 0 {
		return nil, true
	}
	return kept, false
}

// sourceLabels renders source types in the Korean the user's question used.
// Unlisted types fall through to their raw identifier — a slightly technical
// Reason is better than a plan that cannot describe itself.
var sourceLabels = map[model.SourceType]string{
	model.SourceCalendar:       "캘린더",
	model.SourceGmail:          "메일",
	model.SourceSMS:            "문자",
	model.SourceCallLog:        "통화 기록",
	model.SourceCallTranscript: "통화 전사",
	model.SourceNote:           "노트",
	model.SourceUpload:         "업로드 파일",
}

// planReason renders "내일(8/20) 캘린더만 조회".
func planReason(label string, from, to time.Time, sources []model.SourceType) string {
	return fmt.Sprintf("%s(%s) %s", label, formatSpan(from, to), sourcePhrase(sources))
}

// planReasonFromWindow is planReason for a plan with no phrase label — used
// when the LLM returns a valid plan but an empty reason.
func planReasonFromWindow(from, to *time.Time, sources []model.SourceType) string {
	if from == nil && to == nil {
		return fmt.Sprintf("기간 제한 없이 %s", sourcePhrase(sources))
	}
	switch {
	case from != nil && to != nil:
		return fmt.Sprintf("%s %s", formatSpan(*from, *to), sourcePhrase(sources))
	case from != nil:
		return fmt.Sprintf("%s 이후 %s", formatDay(*from), sourcePhrase(sources))
	default:
		return fmt.Sprintf("%s 이전 %s", formatDay(*to), sourcePhrase(sources))
	}
}

// formatSpan renders a half-open window the way a person reads it: a single
// day as "8/20", a longer window as its INCLUSIVE last day, "8/22~8/23". The
// stored bound stays exclusive; only the rendering closes it, because
// "8/22~8/24" for a two-day weekend reads as three days.
func formatSpan(from, to time.Time) string {
	last := to.AddDate(0, 0, -1)
	if !last.After(from) {
		return formatDay(from)
	}
	return fmt.Sprintf("%s~%s", formatDay(from), formatDay(last))
}

func formatDay(t time.Time) string {
	kst := t.In(timeutil.KST())
	return fmt.Sprintf("%d/%d", int(kst.Month()), kst.Day())
}

func sourcePhrase(sources []model.SourceType) string {
	if len(sources) == 0 {
		return "전체 소스 조회"
	}
	labels := make([]string, 0, len(sources))
	for _, st := range sources {
		if label, ok := sourceLabels[st]; ok {
			labels = append(labels, label)
			continue
		}
		labels = append(labels, string(st))
	}
	return strings.Join(labels, "·") + "만 조회"
}
