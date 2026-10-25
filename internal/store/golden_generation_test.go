package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func seedGenerationDocument(t *testing.T, pg *Postgres, source, metadata string, occurred time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pg.pool.Exec(context.Background(), `INSERT INTO documents(id,source_type,source_id,title,content,metadata,occurred_at,collected_at)
        VALUES($1,$2,$3,'Specific fixture title','A useful source with enough content for a grounded test question.',$4::jsonb,$5,now())`,
		id, source, goldenTestSentinel+id.String(), metadata, occurred)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGoldenGenerationHistoryBeyondSeenPool(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()
	old := "The older unseen question must remain reachable"
	insertAskSessionAt(t, pg, old, time.Now().Add(-24*time.Hour))
	for i := 0; i < 205; i++ {
		text := fmt.Sprintf("Recently completed distinct query %03d", i)
		insertAskSession(t, pg, text)
		seedGoldenQuery(t, pg, text, "ask_history", "done")
	}
	created, _, err := s.GenerateQueries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if created != len(goldenSeedQueries)+1 {
		t.Fatalf("created=%d want=%d", created, len(goldenSeedQueries)+1)
	}
	var status string
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM golden_queries WHERE text=$1`, old).Scan(&status); err != nil || status != "open" {
		t.Fatalf("older unseen question not generated: %s %v", status, err)
	}
}

func TestGoldenGenerationReplenishesCompletedSetPreservingJudgments(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()
	for i := 0; i < 15; i++ {
		insertAskSession(t, pg, fmt.Sprintf("Distinct existing history question %02d", i))
	}
	// 이력 질문 15건 + 시드 질문 전부. 시드 목록 길이가 바뀌어도(수집기 제거로
	// 줄었다) 이 테스트가 숫자를 따로 들고 있지 않도록 길이에서 구한다.
	total := 15 + len(goldenSeedQueries)
	created, _, err := s.GenerateQueries(ctx)
	if err != nil || created != total {
		t.Fatalf("initial generation=%d err=%v", created, err)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE golden_queries SET status='done'`); err != nil {
		t.Fatal(err)
	}
	var queryID uuid.UUID
	var askedAt time.Time
	if err := pg.pool.QueryRow(ctx, `SELECT id,asked_at FROM golden_queries ORDER BY id LIMIT 1`).Scan(&queryID, &askedAt); err != nil {
		t.Fatal(err)
	}
	doc := seedGenerationDocument(t, pg, "sms", `{"retention":"keep"}`, time.Now().Add(-time.Hour))
	if _, err := pg.pool.Exec(ctx, `INSERT INTO golden_judgments(query_id,document_id,judgment,judge) VALUES($1,$2,'relevant','user')`, queryID, doc); err != nil {
		t.Fatal(err)
	}
	created, open, err := s.GenerateQueries(ctx)
	if err != nil || created != 0 || open != 0 {
		t.Fatalf("exhaustion=%d/%d %v", created, open, err)
	}
	inputs := []GoldenGeneratedQuery{{Text: "What did the recent message confirm?", DocumentID: doc}}
	created, open, err = s.NewGeneratedQueries(ctx, inputs)
	if err != nil || created != 1 || open != 1 {
		t.Fatalf("replenish=%d/%d %v", created, open, err)
	}
	created, open, err = s.NewGeneratedQueries(ctx, inputs)
	if err != nil || created != 0 || open != 1 {
		t.Fatalf("repeat=%d/%d %v", created, open, err)
	}
	var done, judgments int
	var retainedAskedAt time.Time
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM golden_queries WHERE status='done'`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM golden_judgments WHERE query_id=$1 AND judge='user' AND judgment='relevant'`, queryID).Scan(&judgments); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(ctx, `SELECT asked_at FROM golden_queries WHERE id=$1`, queryID).Scan(&retainedAskedAt); err != nil {
		t.Fatal(err)
	}
	if done != total || judgments != 1 || !retainedAskedAt.Equal(askedAt) {
		t.Fatalf("existing progress changed: done=%d labels=%d", done, judgments)
	}
	var automatic int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM golden_judgments j JOIN golden_queries q ON q.id=j.query_id WHERE q.source='document'`).Scan(&automatic); err != nil {
		t.Fatal(err)
	}
	if automatic != 0 {
		t.Fatal("generation assigned automatic ground truth")
	}
	docs, err := s.CandidateGenerationDocuments(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.ID == doc {
			t.Fatal("used source returned again")
		}
	}
	// Deleting evidence preserves the human query and its completed history.
	if _, err := pg.pool.Exec(ctx, `DELETE FROM documents WHERE id=$1`, doc); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM golden_queries WHERE source='document' AND source_document_id IS NULL`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("source deletion lost query: %d %v", remaining, err)
	}
}

func TestGoldenGenerationCandidateEligibilityAndSourceBalance(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 15; i++ {
		seedGenerationDocument(t, pg, "sms", `{"retention":"keep"}`, now.Add(-time.Duration(i+1)*time.Minute))
	}
	seedGenerationDocument(t, pg, "note", `{"retention":"keep"}`, now.Add(-time.Hour))
	seedGenerationDocument(t, pg, "call", `{"retention":"keep","transcription":"done"}`, now.Add(-time.Hour))
	invalid := []uuid.UUID{
		seedGenerationDocument(t, pg, "sms", "{}", now.Add(-time.Hour)),
		seedGenerationDocument(t, pg, "note", `{"retention":"low"}`, now.Add(-time.Hour)),
		seedGenerationDocument(t, pg, "sms", `{"retention":"disposable"}`, now.Add(-time.Hour)),
		seedGenerationDocument(t, pg, "call", `{"retention":"keep","transcription":"pending"}`, now.Add(-time.Hour)),
		seedGenerationDocument(t, pg, "call", `{"retention":"keep","transcription":"none"}`, now.Add(-time.Hour)),
		seedGenerationDocument(t, pg, "sms", `{"retention":"keep"}`, now.Add(-15*24*time.Hour)),
		seedGenerationDocument(t, pg, "note", `{"retention":"keep"}`, now.Add(time.Hour)),
		// 제거된 소스: 테이블에 남아 있는 행도 생성 후보가 아니다.
		seedGenerationDocument(t, pg, "gmail", `{"retention":"keep"}`, now.Add(-time.Hour)),
		seedGenerationDocument(t, pg, "calendar", `{"retention":"keep"}`, now.Add(-time.Hour)),
		seedGenerationDocument(t, pg, "filesystem", `{"retention":"keep"}`, now.Add(-time.Hour)),
	}
	deleted := seedGenerationDocument(t, pg, "sms", `{"retention":"keep"}`, now.Add(-time.Hour))
	short := seedGenerationDocument(t, pg, "sms", `{"retention":"keep"}`, now.Add(-time.Hour))
	missingDate := seedGenerationDocument(t, pg, "note", `{"retention":"keep"}`, now.Add(-time.Hour))
	for _, statement := range []struct {
		q  string
		id uuid.UUID
	}{
		{`UPDATE documents SET status='deleted' WHERE id=$1`, deleted},
		{`UPDATE documents SET content='ok' WHERE id=$1`, short},
		{`UPDATE documents SET occurred_at=NULL WHERE id=$1`, missingDate},
	} {
		if _, err := pg.pool.Exec(ctx, statement.q, statement.id); err != nil {
			t.Fatal(err)
		}
	}
	invalid = append(invalid, deleted, short, missingDate)
	docs, err := s.CandidateGenerationDocuments(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]bool{}
	for _, d := range docs {
		sources[string(d.Source)] = true
	}
	if len(docs) != 3 || len(sources) != 3 {
		t.Fatalf("unbalanced sources: %+v", sources)
	}
	docs, err = s.CandidateGenerationDocuments(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		for _, id := range invalid {
			if d.ID == id {
				t.Fatalf("ineligible candidate %s", id)
			}
		}
	}
	var inputs []GoldenGeneratedQuery
	for i, id := range invalid {
		inputs = append(inputs, GoldenGeneratedQuery{Text: fmt.Sprintf("Ineligible source question number %d", i), DocumentID: id})
	}
	created, _, err := s.NewGeneratedQueries(ctx, inputs)
	if err != nil || created != 0 {
		t.Fatalf("ineligible source persisted: %d %v", created, err)
	}
}

func TestGoldenGenerationConcurrentNormalizedDedup(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()
	doc1 := seedGenerationDocument(t, pg, "sms", `{"retention":"keep"}`, time.Now().Add(-time.Hour))
	doc2 := seedGenerationDocument(t, pg, "note", `{"retention":"keep"}`, time.Now().Add(-time.Hour))
	inputs := []GoldenGeneratedQuery{{Text: "A novel grounded question?", DocumentID: doc1}, {Text: " a NOVEL grounded question? ", DocumentID: doc2}}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	errors := make(chan error, 2)
	for _, input := range inputs {
		wg.Add(1)
		go func(q GoldenGeneratedQuery) {
			defer wg.Done()
			n, _, err := s.NewGeneratedQueries(ctx, []GoldenGeneratedQuery{q})
			results <- n
			errors <- err
		}(input)
	}
	wg.Wait()
	close(results)
	close(errors)
	total := 0
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for n := range results {
		total += n
	}
	if total != 1 {
		t.Fatalf("near duplicates inserted concurrently: %d", total)
	}
	// A duplicate text must not consume the losing source document.
	docs, err := s.CandidateGenerationDocuments(ctx, 10)
	if err != nil || len(docs) != 1 {
		t.Fatalf("duplicate consumed source: %d %v", len(docs), err)
	}
	// Reuse of the same source with a different text must not create a second row.
	var used uuid.UUID
	if err := pg.pool.QueryRow(ctx, `SELECT source_document_id FROM golden_queries WHERE source='document'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	n, _, err := s.NewGeneratedQueries(ctx, []GoldenGeneratedQuery{{Text: "An entirely different question from same source", DocumentID: used}})
	if err != nil || n != 0 {
		t.Fatalf("source reused: %d %v", n, err)
	}
}

func TestGoldenGenerationExistingQuestionsBoundedAcrossStatuses(t *testing.T) {
	pg := goldenTestDB(t)
	s := NewGoldenStore(pg)
	ctx := context.Background()
	for i := 0; i < 110; i++ {
		status := []string{"done", "skipped", "open"}[i%3]
		seedGoldenQuery(t, pg, fmt.Sprintf("Existing exclusion question %03d", i), "manual", status)
	}
	questions, err := s.ExistingGenerationQuestions(ctx, 1000)
	if err != nil || len(questions) != 100 {
		t.Fatalf("bounded exclusion list: %d %v", len(questions), err)
	}
	if questions[0] != "Existing exclusion question 109" || questions[99] != "Existing exclusion question 010" {
		t.Fatalf("unexpected exclusion ordering: %s .. %s", questions[0], questions[99])
	}
	questions, err = s.ExistingGenerationQuestions(ctx, 0)
	if err != nil || len(questions) != 0 {
		t.Fatalf("zero limit: %d %v", len(questions), err)
	}
}
