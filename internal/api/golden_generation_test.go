package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

type corpusGoldenStub struct {
	*stubGoldenSet
	documents             []store.GoldenGenerationDocument
	readCalls, writeCalls int
	saved                 []store.GoldenGeneratedQuery
	readErr, writeErr     error
}

func (s *corpusGoldenStub) CandidateGenerationDocuments(_ context.Context, limit int) ([]store.GoldenGenerationDocument, error) {
	s.readCalls++
	return s.documents, s.readErr
}
func (s *corpusGoldenStub) NewGeneratedQueries(_ context.Context, q []store.GoldenGeneratedQuery) (int, int, error) {
	s.writeCalls++
	s.saved = q
	return len(q), len(q), s.writeErr
}
func generationFixture() (*Server, *corpusGoldenStub, *fakeAskLLM) {
	id := uuid.New()
	content := "오로라 프로젝트 회의에서 검색 품질 개선과 배포 일정을 논의했습니다."
	g := &corpusGoldenStub{stubGoldenSet: &stubGoldenSet{}, documents: []store.GoldenGenerationDocument{{ID: id, Source: model.SourceGmail, Title: "오로라 회의", Content: content}}}
	body, _ := json.Marshal([]goldenGeneratedCandidate{{DocumentID: id.String(), Question: "오로라 프로젝트 회의에서 논의한 검색 개선 내용은 무엇인가요?", Evidence: content}})
	l := &fakeAskLLM{enabled: true, completeResp: string(body)}
	s := newGoldenTestServer(g, nil)
	s.llmClient = l
	return s, g, l
}
func TestGoldenGenerationExhaustionUsesGroundedCorpus(t *testing.T) {
	s, g, l := generationFixture()
	r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if g.generateCalls != 1 || g.readCalls != 1 || g.writeCalls != 1 || l.completeCalls != 1 || len(g.saved) != 1 {
		t.Fatal("fallback missing")
	}
	if g.saved[0].DocumentID != g.documents[0].ID || g.upsertJudgments != nil {
		t.Fatal("provenance missing or labels written")
	}
	if !strings.Contains(l.gotSystemPrompt, "untrusted") || !strings.Contains(l.gotSystemPrompt, "not 오늘") {
		t.Fatal("missing source/time boundary")
	}
}
func TestGoldenGenerationOnlyAfterExhaustion(t *testing.T) {
	for _, counts := range [][2]int{{0, 1}, {1, 1}} {
		s, g, l := generationFixture()
		g.generateCreated = counts[0]
		g.generateTotalOpen = counts[1]
		r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
		if r.Code != 200 || g.readCalls != 0 || l.completeCalls != 0 || g.writeCalls != 0 {
			t.Fatal("fallback despite existing queue")
		}
	}
}
func TestGoldenGenerationUnavailableLLMAndFailures(t *testing.T) {
	for _, scenario := range []string{"nil", "disabled", "provider", "corpus", "write"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, l := generationFixture()
			switch scenario {
			case "nil":
				s.llmClient = nil
			case "disabled":
				l.enabled = false
			case "provider":
				l.completeErr = errors.New("secret provider body")
			case "corpus":
				g.readErr = errors.New("private source")
			case "write":
				g.writeErr = errors.New("private SQL")
			}
			r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
			if r.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", r.Code, r.Body)
			}
			if strings.Contains(r.Body.String(), "private") || strings.Contains(r.Body.String(), "secret") {
				t.Fatal("leaked upstream error")
			}
		})
	}
}
func TestGoldenGenerationRejectsUngroundedOutput(t *testing.T) {
	for _, scenario := range []string{"unknown_id", "invented_evidence", "generic", "unknown_field", "trailing", "null", "too_many"} {
		t.Run(scenario, func(t *testing.T) {
			s, g, l := generationFixture()
			var items []goldenGeneratedCandidate
			_ = json.Unmarshal([]byte(l.completeResp), &items)
			switch scenario {
			case "unknown_id":
				items[0].DocumentID = uuid.NewString()
			case "invented_evidence":
				items[0].Evidence = "문서에는 존재하지 않는 가짜 근거입니다"
			case "generic":
				items[0].Question = "최근 내역 알려주세요 자세하게"
			case "too_many":
				for len(items) <= goldenGenerationMaxQuestions {
					items = append(items, items[0])
				}
			}
			body, _ := json.Marshal(items)
			l.completeResp = string(body)
			switch scenario {
			case "unknown_field":
				l.completeResp = strings.Replace(l.completeResp, `"question":`, `"judgment":"relevant","question":`, 1)
			case "trailing":
				l.completeResp += " []"
			case "null":
				l.completeResp = "null"
			}
			r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
			if r.Code != 503 || g.writeCalls != 0 {
				t.Fatalf("invalid output wrote: %d %s", r.Code, r.Body)
			}
		})
	}
}
func TestGoldenGenerationDeduplicatesOutput(t *testing.T) {
	s, g, l := generationFixture()
	var items []goldenGeneratedCandidate
	_ = json.Unmarshal([]byte(l.completeResp), &items)
	items = append(items, items[0])
	body, _ := json.Marshal(items)
	l.completeResp = string(body)
	r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
	if r.Code != 200 || len(g.saved) != 1 {
		t.Fatalf("duplicates retained: %d %+v", r.Code, g.saved)
	}
}
func TestGoldenGenerationEmptyCorpusOrResultIsValid(t *testing.T) {
	for _, emptyCorpus := range []bool{true, false} {
		s, g, l := generationFixture()
		if emptyCorpus {
			g.documents = nil
		} else {
			l.completeResp = "[]"
		}
		r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
		if r.Code != 200 || g.writeCalls != 0 || !strings.Contains(r.Body.String(), `"created":0`) {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
	}
}
func TestGoldenGenerationReadAndFeedbackNeverGenerate(t *testing.T) {
	s, g, l := generationFixture()
	r := doGoldenRequest(s, http.MethodGet, "/api/v1/golden/next", nil)
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	body := []byte(`{"query_text":"없는 문서 질문","source":"document","judge":"user","judgments":[]}`)
	r = doGoldenRequest(s, http.MethodPost, "/api/v1/golden/feedback", body)
	if r.Code != 404 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body)
	}
	if g.generateCalls != 0 || g.readCalls != 0 || g.writeCalls != 0 || l.completeCalls != 0 {
		t.Fatal("read/feedback generated")
	}
}

func TestGoldenGenerationKeepsValidSiblings(t *testing.T) {
	s, g, l := generationFixture()
	var valid []goldenGeneratedCandidate
	_ = json.Unmarshal([]byte(l.completeResp), &valid)
	invalidID := valid[0]
	invalidID.DocumentID = "not-a-uuid"
	unknown := valid[0]
	unknown.DocumentID = uuid.NewString()
	invalidEvidence := valid[0]
	invalidEvidence.Evidence = "근거로 제공된 문서에는 존재하지 않는 내용"
	generic := valid[0]
	generic.Question = "최근 내역 알려주세요 자세하게"
	items := []goldenGeneratedCandidate{invalidID, unknown, invalidEvidence, generic, valid[0]}
	body, _ := json.Marshal(items)
	l.completeResp = string(body)
	r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
	if r.Code != 200 || len(g.saved) != 1 || g.saved[0].DocumentID != g.documents[0].ID {
		t.Fatalf("valid sibling lost: %d %+v", r.Code, g.saved)
	}
}

type corpusGoldenHistoryStub struct {
	*corpusGoldenStub
	existing     []string
	historyLimit int
	historyErr   error
}

func (s *corpusGoldenHistoryStub) ExistingGenerationQuestions(_ context.Context, limit int) ([]string, error) {
	s.historyLimit = limit
	return s.existing, s.historyErr
}

func TestGoldenGenerationIncludesExistingQuestions(t *testing.T) {
	s, g, l := generationFixture()
	h := &corpusGoldenHistoryStub{corpusGoldenStub: g, existing: []string{"오로라 프로젝트 배포 일정은 어떻게 결정되었나요?"}}
	s.golden = h
	r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
	if r.Code != 200 || h.historyLimit != 100 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	var input struct {
		Documents         []goldenGenerationInput `json:"documents"`
		ExistingQuestions []string                `json:"existing_questions"`
	}
	if err := json.Unmarshal([]byte(l.gotMessages[0].Content), &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Documents) != 1 || len(input.ExistingQuestions) != 1 || input.ExistingQuestions[0] != h.existing[0] {
		t.Fatalf("missing exclusion context: %+v", input)
	}
	if !strings.Contains(l.gotSystemPrompt, "do not repeat or merely paraphrase") {
		t.Fatal("history not explained")
	}
}

func TestGoldenGenerationHistoryFailureDoesNotCallLLM(t *testing.T) {
	s, g, l := generationFixture()
	s.golden = &corpusGoldenHistoryStub{corpusGoldenStub: g, historyErr: errors.New("secret SQL")}
	r := doGoldenRequest(s, http.MethodPost, "/api/v1/golden/queries/generate", nil)
	if r.Code != 503 || l.completeCalls != 0 || strings.Contains(r.Body.String(), "secret") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}
