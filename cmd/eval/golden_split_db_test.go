package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 실DB 검사: 날짜 분리가 없는 질문에서 goldenPairsFromJudgments(ExportEvalJudgmentRows)
// 가 ExportEvalPairs 와 같은 쌍을 만들고, 여러 날 판정된 상대 기간 질문만 나뉘는지 본다.
// TEST_DATABASE_URL 이 없으면 건너뛰며, 그 값은 일회용 DB 만 가리켜야 한다. 데이터는
// 전부 가상이고 문구 접두(splitDBPrefix)로 정리한다. 골든 표를 TRUNCATE 하는 store
// 패키지 테스트와 같은 DB 를 쓰므로 패키지를 동시에 돌리지 않는다(go test -p 1).

const splitDBPrefix = "zz-split-db-"

func splitDB(t *testing.T) (*store.GoldenStore, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database golden split test")
	}
	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cleanup := func() {
		_, _ = conn.Exec(context.Background(), `DELETE FROM golden_queries WHERE text LIKE $1`, splitDBPrefix+"%")
		_, _ = conn.Exec(context.Background(), `DELETE FROM documents WHERE source_id LIKE $1`, splitDBPrefix+"%")
	}
	cleanup()
	t.Cleanup(func() { cleanup(); _ = conn.Close(context.Background()) })
	return store.NewGoldenStore(pg), conn
}

func TestDB_GoldenSplit_MatchesExportEvalPairs(t *testing.T) {
	gs, conn := splitDB(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	doc := func() uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO documents (id, source_type, source_id, title, content, collected_at)
			VALUES ($1, 'filesystem', $2, 'zz', 'zz', now())`, id, splitDBPrefix+id.String())
		return id
	}
	query := func(text, source string, asked time.Time) uuid.UUID {
		var id uuid.UUID
		if err := conn.QueryRow(ctx, `INSERT INTO golden_queries (text, source, status, asked_at)
			VALUES ($1, $2, 'done', $3) RETURNING id`, splitDBPrefix+text, source, asked).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	judge := func(q, d uuid.UUID, judgment, judgeName string, at time.Time) {
		exec(`INSERT INTO golden_judgments (query_id, document_id, judgment, judge, judged_at)
			VALUES ($1, $2, $3, $4, $5)`, q, d, judgment, judgeName, at)
	}

	asked := time.Date(2031, 3, 2, 1, 0, 0, 0, time.UTC)
	day1 := time.Date(2031, 3, 2, 3, 0, 0, 0, time.UTC) // 2031-03-02 12:00 KST
	day2 := time.Date(2031, 3, 4, 3, 0, 0, 0, time.UTC) // 2031-03-04 12:00 KST
	same := time.Date(2031, 3, 2, 3, 0, 0, 0, time.UTC) // day1 과 같은 시각: 정렬 동점

	// 1) 기간 없는 질문, 여러 날 판정 → 묶임.
	plain := query("거래처 연락처", "manual", asked)
	judge(plain, doc(), "relevant", "user", day1)
	judge(plain, doc(), "relevant", "user", day2)
	judge(plain, doc(), "noise", "user", day2)
	// 2) 상대 기간 질문, 하루에 판정 → 묶임.
	oneDay := query("내일 일정 뭐야", "ask_history", asked)
	judge(oneDay, doc(), "relevant", "user", same)
	judge(oneDay, doc(), "irrelevant", "user", same.Add(time.Minute))
	// 3) 오답만 있는 질문(동점 시각) + llm 판정(제외돼야 함).
	negOnly := query("가상 메모", "seed", asked)
	judge(negOnly, doc(), "irrelevant", "user", same)
	judge(negOnly, doc(), "relevant", "llm", day2)

	filter := func(pairs []store.EvalPair) []store.EvalPair {
		var out []store.EvalPair
		for _, p := range pairs {
			if strings.HasPrefix(p.Query, splitDBPrefix) {
				p.ID = 0 // 전역 순번은 이 테스트 밖 데이터에 따라 달라진다
				out = append(out, p)
			}
		}
		return out
	}
	load := func() ([]store.EvalPair, []store.EvalPair) {
		t.Helper()
		want, err := gs.ExportEvalPairs(ctx, "user")
		if err != nil {
			t.Fatal(err)
		}
		rows, err := gs.ExportEvalJudgmentRows(ctx, "user")
		if err != nil {
			t.Fatal(err)
		}
		return filter(want), filter(goldenPairsFromJudgments(rows, windowAnchorJudgedAt))
	}

	if !hasPeriodExpression(splitDBPrefix+"내일 일정 뭐야", day1) || hasPeriodExpression(splitDBPrefix+"거래처 연락처", day1) {
		t.Fatal("fixture: 기간 표현 판정이 가정과 다르다")
	}

	want, got := load()
	if len(want) != 3 {
		t.Fatalf("ExportEvalPairs 기준 쌍 %d개, want 3", len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		// pgx 가 돌려준 시각의 위치 정보(Local/UTC)만 다를 수 있다.
		if !w.CreatedAt.Equal(g.CreatedAt) || !w.AskedAt.Equal(g.AskedAt) {
			t.Errorf("[%d] 시각이 다르다: want created=%v asked=%v, got created=%v asked=%v", i, w.CreatedAt, w.AskedAt, g.CreatedAt, g.AskedAt)
		}
		w.CreatedAt, g.CreatedAt, w.AskedAt, g.AskedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		if !reflect.DeepEqual(w, g) {
			t.Errorf("[%d] 쌍이 ExportEvalPairs 와 다르다\n want=%+v\n  got=%+v", i, w, g)
		}
	}
	if labelFingerprint(want) != labelFingerprint(got) {
		t.Error("분리가 없는데 label_hash 가 달라졌다")
	}

	// 4) 상대 기간 질문에 다른 날 판정을 더하면 그 질문만 둘로 나뉜다.
	judge(oneDay, doc(), "relevant", "user", day2)
	want, got = load()
	if len(want) != 3 || len(got) != 4 {
		t.Fatalf("export %d / split %d, want 3 / 4", len(want), len(got))
	}
	var split []store.EvalPair
	for _, p := range got {
		if p.Query == splitDBPrefix+"내일 일정 뭐야" {
			split = append(split, p)
		}
	}
	if len(split) != 2 || split[0].GoldenQueryID != oneDay.String()+"@2031-03-04" ||
		split[1].GoldenQueryID != oneDay.String()+"@2031-03-02" ||
		len(split[0].RelevantDocIDs) != 1 || len(split[1].RelevantDocIDs) != 1 || len(split[1].IrrelevantDocIDs) != 1 {
		t.Errorf("분리 결과가 예상과 다르다: %+v", split)
	}
}
