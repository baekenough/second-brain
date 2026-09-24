package store

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/pgvector/pgvector-go"
)

// updateSparseSnapshot 은 raw 모드 SQL 스냅샷을 다시 쓴다. #276 이전 코드로
// 한 번 생성해 고정한 파일이므로, 이 플래그로 덮어쓰는 것은 "raw 모드 SQL 을
// 의도적으로 바꾼다" 는 결정과 같다 — 리뷰에서 그 diff 를 반드시 설명해야 한다.
var updateSparseSnapshot = flag.Bool("update-sparse-snapshot", false,
	"testdata/sparse_query_raw.golden 을 현재 빌더 출력으로 다시 쓴다")

const sparseSnapshotPath = "testdata/sparse_query_raw.golden"

// sparseSnapshotCases 는 raw 모드 스냅샷이 덮는 필터 조합이다. 필터마다
// 플레이스홀더 번호가 밀리므로, 조합이 빠지면 "번호가 밀린 경우에만 달라지는"
// 회귀를 놓친다.
func sparseSnapshotCases() []struct {
	name string
	q    model.SearchQuery
} {
	from := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	cal := model.SourceCalendar
	emb := make([]float32, 4)
	copy(emb, []float32{0.1, 0.2, 0.3, 0.4})
	base := model.SearchQuery{Query: "이번 주 회의 일정 알려줘", Limit: 10, Embedding: emb}

	withSrc := base
	withSrc.SourceType = &cal
	withExclude := base
	withExclude.ExcludeSourceTypes = []model.SourceType{model.SourceInsight, model.SourceGmail}
	withRetention := base
	withRetention.ExcludeRetention = []string{model.RetentionDisposable}
	withWindow := base
	withWindow.OccurredFrom, withWindow.OccurredTo = &from, &to
	all := base
	all.SourceTypes = []model.SourceType{model.SourceCalendar, model.SourceCall}
	all.ExcludeSourceTypes = []model.SourceType{model.SourceInsight}
	all.ExcludeRetention = []string{model.RetentionDisposable}
	all.OccurredFrom, all.OccurredTo = &from, &to
	deleted := base
	deleted.IncludeDeleted = true
	recentPast := all
	recentPast.Sort = model.SortRecent

	return []struct {
		name string
		q    model.SearchQuery
	}{
		{"base", base},
		{"source", withSrc},
		{"exclude", withExclude},
		{"retention", withRetention},
		{"window", withWindow},
		{"all", all},
		{"include_deleted", deleted},
		{"recent_past_window", recentPast},
	}
}

// renderSnapshotArgs 는 인자를 결정론적 문자열로 만든다. 포인터·벡터 내부
// 주소가 스냅샷에 들어가면 실행마다 달라지므로 값만 남긴다.
func renderSnapshotArgs(args []interface{}) string {
	var b strings.Builder
	for i, a := range args {
		var s string
		switch v := a.(type) {
		case pgvector.Vector:
			s = fmt.Sprintf("vector%v", v.Slice())
		case time.Time:
			s = v.UTC().Format(time.RFC3339Nano)
		default:
			s = fmt.Sprintf("%T %#v", v, v)
		}
		fmt.Fprintf(&b, "$%d = %s\n", i+1, s)
	}
	return b.String()
}

// renderSparseSnapshot 은 네 빌더(문서 hybrid·fulltext, 청크 FTS·fuse_ctx)의
// 출력을 한 파일로 모은다. hybrid 는 엔티티 레인 on/off 를 모두 찍는다 —
// 엔티티 파라미터 위치가 필터 개수에 따라 움직이기 때문이다.
func renderSparseSnapshot() string {
	var b strings.Builder
	emit := func(name, sql string, args []interface{}) {
		fmt.Fprintf(&b, "==== %s ====\n%s\n---- args ----\n%s", name, sql, renderSnapshotArgs(args))
	}
	entityOn := model.SearchWeights{}.Defaults()
	entityOn.SummaryVec = model.DefaultSummaryVecWeight
	entityOn.EntityWeight = model.DefaultEntityWeight
	entityOff := model.SearchWeights{}.Defaults()

	for _, c := range sparseSnapshotCases() {
		sql, args := buildHybridSearchQuery(c.q, entityOn)
		emit("hybrid/entity_on/"+c.name, sql, args)
		sql, args = buildHybridSearchQuery(c.q, entityOff)
		emit("hybrid/entity_off/"+c.name, sql, args)
		sql, args = buildFulltextSearchQuery(c.q)
		emit("fulltext/"+c.name, sql, args)
		sql, args = buildChunkFTSQuery(c.q, 30)
		emit("chunk_fts/"+c.name, sql, args)
		sql, args = buildSparseContextQuery(c.q, 30, model.ChunkSparseCtxV1TP)
		emit("sparse_ctx/"+c.name, sql, args)
	}
	return b.String()
}

// TestSparseQueryRaw_SQLByteIdentical 는 #276 의 핵심 불변식을 고정한다:
// SEARCH_SPARSE_QUERY=raw(기본)일 때 생성되는 SQL 과 인자는 #276 이전과
// 바이트 단위로 같아야 한다. 스냅샷은 #276 코드가 들어가기 전의 빌더로
// 생성했다. SparseTerms 가 비어 있는 질의만 넣으므로, 이 테스트가 깨졌다면
// 노브를 켜지 않은 운영 배포의 검색 SQL 이 바뀐 것이다.
func TestSparseQueryRaw_SQLByteIdentical(t *testing.T) {
	got := renderSparseSnapshot()
	path := filepath.Clean(sparseSnapshotPath)
	if *updateSparseSnapshot {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("스냅샷을 다시 썼다: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("스냅샷 읽기 실패: %v", err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) && i < len(wl); i++ {
			if gl[i] != wl[i] {
				t.Fatalf("raw 모드 SQL 이 #276 이전과 달라졌다 (%d번째 줄)\n got: %q\nwant: %q", i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("raw 모드 SQL 길이가 달라졌다: got %d줄, want %d줄", len(gl), len(wl))
	}
}
