package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// goldenJudgmentsCap 는 internal/api 의 goldenJudgmentsMax(요청당 판정 수 상한,
// #286)와 같은 값이다. store 는 api 를 import 할 수 없어 숫자를 둔다 — 상한을
// 바꾸면 이 값도 함께 바꿔 상한 크기의 요청이 실DB 에서 끝까지 도는지 다시 본다.
const goldenJudgmentsCap = 100

// TestGoldenStore_UpsertJudgments_AtRequestCap 은 요청 하나가 허용하는 최대
// 판정 수(100)를 실제 PostgreSQL 에서 한 트랜잭션으로 저장할 수 있는지 보고,
// 소요 시간을 남긴다(상한 결정의 근거). user 판정이라 보존 태그 반영(문서
// metadata 갱신)까지 판정마다 도는 가장 비싼 경로다.
func TestGoldenStore_UpsertJudgments_AtRequestCap(t *testing.T) {
	pg := goldenTestDB(t)
	gs := NewGoldenStore(pg)
	queryID := seedGoldenQuery(t, pg, "zz-dummy-golden-cap-query", "seed", "open")

	inputs := make([]GoldenJudgmentInput, goldenJudgmentsCap)
	for i := range inputs {
		inputs[i] = GoldenJudgmentInput{
			DocumentID: seedGoldenDocument(t, pg, nil),
			Judgment:   "relevant",
			Rank:       i + 1,
			Judge:      "user",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	saved, applied, err := gs.UpsertJudgments(ctx, queryID, inputs, false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("UpsertJudgments(%d): %v", goldenJudgmentsCap, err)
	}
	if saved != goldenJudgmentsCap {
		t.Errorf("saved = %d, want %d", saved, goldenJudgmentsCap)
	}
	t.Logf("UpsertJudgments: %d judgments (feedback applied %d) in %v", saved, applied, elapsed)
}

// TestGoldenStore_UpsertJudgments_ForeignKeyRollsBackAll 은 api 가 400 으로
// 바꾸는 두 오류의 실제 모양과 트랜잭션 롤백을 고정한다(#286 deep-verify).
//
//   - 없는 document_id 는 PgError 23503 이 %w 로 감싸져 올라온다(api 의
//     isForeignKeyViolation 이 errors.As 로 찾는 모양).
//   - 앞선 정상 판정까지 한 트랜잭션으로 롤백되어 행이 남지 않는다.
//   - INT4 를 넘는 rank 는 오류(500)가 된다 — api 가 rank 범위를 검사하는 이유.
func TestGoldenStore_UpsertJudgments_ForeignKeyRollsBackAll(t *testing.T) {
	pg := goldenTestDB(t)
	gs := NewGoldenStore(pg)
	queryID := seedGoldenQuery(t, pg, "zz-dummy-golden-fk-query", "seed", "open")
	ctx := context.Background()

	count := func() int {
		var n int
		if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM golden_judgments WHERE query_id = $1`, queryID).Scan(&n); err != nil {
			t.Fatalf("count judgments: %v", err)
		}
		return n
	}

	_, _, err := gs.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: seedGoldenDocument(t, pg, nil), Judgment: "relevant", Rank: 1, Judge: "user"},
		{DocumentID: uuid.New(), Judgment: "relevant", Rank: 2, Judge: "user"},
	}, false)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("missing document: err = %v, want a wrapped PgError 23503", err)
	}
	if n := count(); n != 0 {
		t.Errorf("judgment rows after FK failure = %d, want 0 (whole transaction rolled back)", n)
	}

	_, _, err = gs.UpsertJudgments(ctx, queryID, []GoldenJudgmentInput{
		{DocumentID: seedGoldenDocument(t, pg, nil), Judgment: "relevant", Rank: 1 << 31, Judge: "user"},
	}, false)
	// pgx 가 서버에 보내기 전에 인코딩 단계에서 거부한다(22003 이 아니라 클라이언트
	// 오류). 어느 쪽이든 FK 가 아닌 오류라 api 에서는 500 이 된다.
	if err == nil || isFKErr(err) {
		t.Errorf("rank beyond INT4: err = %v, want a non-FK error", err)
	}
	if n := count(); n != 0 {
		t.Errorf("judgment rows after rank failure = %d, want 0", n)
	}
}

func isFKErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
