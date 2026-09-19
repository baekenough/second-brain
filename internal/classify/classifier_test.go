package classify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/jev"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// decideRetention: the asymmetric core of the whole package.
// ---------------------------------------------------------------------------

func TestDecideRetention_PersonSegment_Keep(t *testing.T) {
	for segment := range personSegments {
		retention, needsReview := decideRetention(segment, 0.99, 0, Gate{}, 0.9)
		if retention != model.RetentionKeep || needsReview {
			t.Errorf("segment %q: decideRetention() = (%q, %v), want (keep, false)", segment, retention, needsReview)
		}
	}
}

// TestDecideRetention_PersonSegment_BulkSenderCaps is the first of the two
// load-bearing asymmetric cases: a person-conversation segment is capped to
// low+needs_review, NEVER disposable, when the gate says the sender is bulk.
func TestDecideRetention_PersonSegment_BulkSenderCaps(t *testing.T) {
	retention, needsReview := decideRetention("personal_comm", 0.99, 0, Gate{BulkSender: true}, 0.9)
	if retention != model.RetentionLow || !needsReview {
		t.Errorf("decideRetention() = (%q, %v), want (low, true)", retention, needsReview)
	}
}

func TestDecideRetention_DisposableCandidate_HighConfidence_Disposable(t *testing.T) {
	for segment := range disposableCandidateSegments {
		retention, needsReview := decideRetention(segment, 0.95, 0, Gate{}, 0.9)
		if retention != model.RetentionDisposable || needsReview {
			t.Errorf("segment %q: decideRetention() = (%q, %v), want (disposable, false)", segment, retention, needsReview)
		}
	}
}

// TestDecideRetention_DisposableCandidate_PersonSignalFloors is the second
// load-bearing asymmetric case: a disposable-candidate segment is floored to
// low+needs_review, NEVER disposable, when the gate found a person signal.
func TestDecideRetention_DisposableCandidate_PersonSignalFloors(t *testing.T) {
	retention, needsReview := decideRetention("ad", 0.95, 0, Gate{PersonSignal: true}, 0.9)
	if retention != model.RetentionLow || !needsReview {
		t.Errorf("decideRetention() = (%q, %v), want (low, true)", retention, needsReview)
	}
}

func TestDecideRetention_DisposableCandidate_LowConfidence_FallsToScore(t *testing.T) {
	// Below threshold: does not qualify for disposable at all, regardless of
	// PersonSignal, and falls through to the raw score bucket.
	retention, needsReview := decideRetention("ad", 0.5, 2, Gate{}, 0.9)
	if retention != model.RetentionKeep || needsReview {
		t.Errorf("decideRetention() = (%q, %v), want (keep, false) — low confidence must fall to the score bucket", retention, needsReview)
	}
	retention, needsReview = decideRetention("ad", 0.5, 1, Gate{}, 0.9)
	if retention != model.RetentionLow || needsReview {
		t.Errorf("decideRetention() = (%q, %v), want (low, false)", retention, needsReview)
	}
}

func TestDecideRetention_OtherSegment_ScoreBucket(t *testing.T) {
	if got, _ := decideRetention("notification", 0.3, 2, Gate{}, 0.9); got != model.RetentionKeep {
		t.Errorf("score=2: decideRetention() = %q, want keep", got)
	}
	if got, _ := decideRetention("notification", 0.3, 1, Gate{}, 0.9); got != model.RetentionLow {
		t.Errorf("score=1: decideRetention() = %q, want low", got)
	}
	if got, _ := decideRetention("notification", 0.3, 0, Gate{}, 0.9); got != model.RetentionLow {
		t.Errorf("score=0: decideRetention() = %q, want low", got)
	}
}

// TestDecideRetention_FourAsymmetricCombinations pins the exact 2x2 the spec
// calls out: {person, bulk} x {disposable-candidate, person-signal}.
func TestDecideRetention_FourAsymmetricCombinations(t *testing.T) {
	cases := []struct {
		name            string
		segment         string
		segmentP        float64
		gate            Gate
		wantRetention   string
		wantNeedsReview bool
	}{
		{"person_no_bulk", "work_comm", 0.9, Gate{}, model.RetentionKeep, false},
		{"person_with_bulk", "work_comm", 0.9, Gate{BulkSender: true}, model.RetentionLow, true},
		{"disposable_no_person", "telemarketing", 0.95, Gate{}, model.RetentionDisposable, false},
		{"disposable_with_person", "telemarketing", 0.95, Gate{PersonSignal: true}, model.RetentionLow, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			retention, needsReview := decideRetention(tc.segment, tc.segmentP, 0, tc.gate, 0.9)
			if retention != tc.wantRetention || needsReview != tc.wantNeedsReview {
				t.Errorf("decideRetention(%q, gate=%+v) = (%q, %v), want (%q, %v)",
					tc.segment, tc.gate, retention, needsReview, tc.wantRetention, tc.wantNeedsReview)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// classifier="user" protection.
//
// The write-side guard lives in internal/store (MergeClassificationMetadata's
// SQL WHERE clause — see classification_sql_test.go); this package's
// contribution to the guarantee is that ClassifyDeterministic/ClassifyWithJev
// never inspect or special-case an existing "user" tag themselves, so there
// is exactly one place (the SQL guard) that can get this wrong, not two
// disagreeing checks.
// ---------------------------------------------------------------------------

func TestResult_Metadata_NeverEmitsUserClassifier(t *testing.T) {
	r := &Result{Segment: "ad", Retention: model.RetentionDisposable, Classifier: "rule", ClassifierP: 1.0}
	meta := r.Metadata(time.Unix(0, 0))
	if meta["classifier"] == "user" {
		t.Fatal("Result.Metadata() must never itself write classifier=\"user\" — that value is reserved for golden-set labeling, never produced by this package")
	}
}

// ---------------------------------------------------------------------------
// ClassifyDeterministic
// ---------------------------------------------------------------------------

func TestClassifyDeterministic_WrapsGateDecision(t *testing.T) {
	c := &Classifier{}
	gate := Gate{Decided: &Tag{Segment: "short_call", Retention: model.RetentionLow}, BulkSender: true}
	result := c.ClassifyDeterministic(gate)

	if result.Segment != "short_call" || result.Retention != model.RetentionLow {
		t.Errorf("result = %+v, want segment=short_call retention=low", result)
	}
	if result.Classifier != "rule" || result.ClassifierP != 1.0 {
		t.Errorf("result.Classifier/ClassifierP = %q/%v, want rule/1.0", result.Classifier, result.ClassifierP)
	}
	if result.NeedsReview {
		t.Error("result.NeedsReview = true, want false for a deterministic rule decision")
	}
	if result.Gate.BulkSender != true {
		t.Error("result.Gate must be the gate passed in")
	}
}

func TestClassifyDeterministic_PanicsWithoutDecision(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("ClassifyDeterministic() did not panic for a Gate with Decided == nil")
		}
	}()
	(&Classifier{}).ClassifyDeterministic(Gate{})
}

// ---------------------------------------------------------------------------
// ClassifyWithJev (httptest-backed end-to-end through the Jev client).
// ---------------------------------------------------------------------------

func newTestJevServer(t *testing.T, segment string, segmentP float64, score float64, inputTokens int) *jev.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"answers": map[string]any{
				"segment":   map[string]any{"choice": segment, "confidence": segmentP},
				"retention": map[string]any{"score": score},
			},
			"usage": map[string]any{"input_tokens": inputTokens},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return jev.NewForTest(srv.URL, "test-key", srv.Client())
}

func TestClassifyWithJev_SMS_PersonalSegment_Keep(t *testing.T) {
	c := &Classifier{Jev: newTestJevServer(t, "personal_comm", 0.8, 2, 55)}
	doc := &model.Document{SourceType: model.SourceSMS, Content: "저녁에 뭐해"}

	result, tokens, err := c.ClassifyWithJev(context.Background(), doc, Gate{})
	if err != nil {
		t.Fatalf("ClassifyWithJev() error = %v", err)
	}
	if result.Segment != "personal_comm" || result.Retention != model.RetentionKeep {
		t.Errorf("result = %+v, want personal_comm/keep", result)
	}
	if result.Classifier != "jev-latest" {
		t.Errorf("result.Classifier = %q, want jev-latest", result.Classifier)
	}
	if tokens != 55 {
		t.Errorf("tokens = %d, want 55", tokens)
	}
}

func TestClassifyWithJev_Mail_UsesTitleAndDomainInState(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotState, _ = body["state"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{
				"segment":   map[string]any{"choice": "newsletter", "confidence": 0.6},
				"retention": map[string]any{"score": 0},
			},
			"usage": map[string]any{"input_tokens": 10},
		})
	}))
	t.Cleanup(srv.Close)
	c := &Classifier{Jev: jev.NewForTest(srv.URL, "test-key", srv.Client())}

	doc := &model.Document{
		SourceType: model.SourceGmail,
		Title:      "이번주 특가 안내",
		Content:    "다양한 상품을 만나보세요",
		Metadata:   map[string]any{"from": "sale@shop.example.com"},
	}
	result, _, err := c.ClassifyWithJev(context.Background(), doc, Gate{})
	if err != nil {
		t.Fatalf("ClassifyWithJev() error = %v", err)
	}
	if result.Segment != "newsletter" || result.Retention != model.RetentionLow {
		t.Errorf("result = %+v, want newsletter/low", result)
	}
	for _, want := range []string{"이번주 특가 안내", "shop.example.com", "다양한 상품을"} {
		if !strings.Contains(gotState, want) {
			t.Errorf("state %q does not contain %q", gotState, want)
		}
	}
}

func TestClassifyWithJev_NotEnabled_ReturnsError(t *testing.T) {
	c := &Classifier{Jev: jev.New("", nil)}
	_, _, err := c.ClassifyWithJev(context.Background(), &model.Document{SourceType: model.SourceSMS}, Gate{})
	if err == nil {
		t.Fatal("ClassifyWithJev() error = nil, want error when Jev is not configured")
	}
}

func TestClassifyWithJev_UnsupportedSourceType_ReturnsError(t *testing.T) {
	c := &Classifier{Jev: newTestJevServer(t, "x", 1, 0, 0)}
	doc := &model.Document{SourceType: model.SourceSlack, Content: "n/a"}
	_, _, err := c.ClassifyWithJev(context.Background(), doc, Gate{})
	if err == nil {
		t.Fatal("ClassifyWithJev() error = nil, want error for an unsupported source type")
	}
}
