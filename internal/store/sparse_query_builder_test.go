package store

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/sparseq"
)

// #276 희소 질의 키워드가 켜졌을 때의 SQL 빌더 검사(DB 없음). raw 모드
// 바이트 동일성은 sparse_query_snapshot_test.go 가 따로 고정한다.

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

// assertPlaceholdersDense 는 SQL 이 $1..$len(args) 를 빠짐없이 쓰고 그 이상은
// 쓰지 않는지 본다. 참조되지 않는 파라미터가 있으면 PostgreSQL 이 타입을
// 정하지 못해 질의가 실패하고, 범위를 넘는 번호는 인자 개수 오류가 된다.
func assertPlaceholdersDense(t *testing.T, name, sql string, args []interface{}) {
	t.Helper()
	used := map[int]bool{}
	maxN := 0
	for _, m := range placeholderRe.FindAllStringSubmatch(sql, -1) {
		n, _ := strconv.Atoi(m[1])
		used[n] = true
		if n > maxN {
			maxN = n
		}
	}
	if maxN != len(args) {
		t.Errorf("%s: max placeholder $%d != len(args) %d", name, maxN, len(args))
	}
	for i := 1; i <= len(args); i++ {
		if !used[i] {
			t.Errorf("%s: $%d bound but never referenced", name, i)
		}
	}
}

func sparseTermsFixture() map[string]model.SparseTerms {
	full := sparseq.Extract("이번 주 zztermalpha zztermbeta 회의 일정 foo@zzterm.com 010-1234-5678 alpha beta gamma delta")
	return map[string]model.SparseTerms{
		"full":      {TSQuery: sparseq.TSQuery(full.TS), Like: full.Like},
		"one":       {TSQuery: "'zztermone':*", Like: []string{"zztermone"}},
		"like_only": {Like: []string{"zz-1"}},
		// 방어: 호출자가 상한을 넘겨도 저장소는 MaxTerms 에서 자른다.
		"over_cap": {TSQuery: "'a1':*", Like: []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10"}},
	}
}

type builtSQL struct {
	name string
	sql  string
	args []interface{}
}

func buildAllSparse(q model.SearchQuery) []builtSQL {
	entityOn := model.SearchWeights{}.Defaults()
	entityOn.EntityWeight = model.DefaultEntityWeight
	entityOff := model.SearchWeights{}.Defaults()
	var out []builtSQL
	add := func(name, sql string, args []interface{}) { out = append(out, builtSQL{name, sql, args}) }
	sql, args := buildHybridSearchQuery(q, entityOn)
	add("hybrid/entity_on", sql, args)
	sql, args = buildHybridSearchQuery(q, entityOff)
	add("hybrid/entity_off", sql, args)
	sql, args = buildFulltextSearchQuery(q)
	add("fulltext", sql, args)
	sql, args = buildChunkFTSQuery(q, 30)
	add("chunk_fts", sql, args)
	sql, args = buildSparseContextQuery(q, 30, model.ChunkSparseCtxV1Full)
	add("sparse_ctx", sql, args)
	return out
}

// TestSparseQueryBuilders_PlaceholdersAndArgs 는 모든 필터 조합 × 키워드
// 조합에서 플레이스홀더가 빈틈없이 쓰이고, raw 인자 목록이 키워드 인자
// 목록의 앞부분과 정확히 같은지(= 기존 번호가 하나도 밀리지 않았는지) 본다.
func TestSparseQueryBuilders_PlaceholdersAndArgs(t *testing.T) {
	t.Parallel()
	for _, c := range sparseSnapshotCases() {
		raw := buildAllSparse(c.q)
		for _, b := range raw {
			assertPlaceholdersDense(t, c.name+"/raw/"+b.name, b.sql, b.args)
		}
		for tname, terms := range sparseTermsFixture() {
			q := c.q
			q.SparseTerms = terms
			got := buildAllSparse(q)
			for i, b := range got {
				name := c.name + "/" + tname + "/" + b.name
				assertPlaceholdersDense(t, name, b.sql, b.args)
				if len(b.args) < len(raw[i].args) || !reflect.DeepEqual(b.args[:len(raw[i].args)], raw[i].args) {
					t.Errorf("%s: raw 인자가 키워드 인자의 앞부분이 아니다 — 기존 플레이스홀더 번호가 밀렸다", name)
					continue
				}
				extra := b.args[len(raw[i].args):]
				wantExtra := 0
				if terms.TSQuery != "" {
					wantExtra++
				}
				wantExtra += min(len(terms.Like), sparseq.MaxTerms)
				if len(extra) != wantExtra {
					t.Errorf("%s: 키워드 인자 %d개, want %d", name, len(extra), wantExtra)
				}
				// 키워드 값은 파라미터로만 가고 SQL 본문에는 절대 들어가지 않는다.
				for _, l := range terms.Like {
					if strings.Contains(b.sql, l) {
						t.Errorf("%s: 키워드 %q 가 SQL 본문에 들어갔다", name, l)
					}
				}
				if strings.Contains(b.sql, "zzterm") {
					t.Errorf("%s: 키워드가 SQL 본문에 들어갔다", name)
				}
			}
		}
	}
}

// TestSparseQueryBuilders_TermsShape 는 키워드 모드에서 tsquery 가 원문
// plainto_tsquery(AND) 대신 to_tsquery(접두 OR) 파라미터 하나로, LIKE 가
// 키워드별 플레이스홀더 OR 로 바뀌는지 본다. LIKE ANY(배열)는 pg_bigm GIN 이
// 인덱스로 못 쓰므로 나오면 안 된다.
func TestSparseQueryBuilders_TermsShape(t *testing.T) {
	t.Parallel()
	q := sparseSnapshotCases()[0].q
	q.SparseTerms = sparseTermsFixture()["full"]
	nLike := len(q.SparseTerms.Like)
	if nLike != sparseq.MaxTerms {
		t.Fatalf("fixture: want %d like terms, got %d", sparseq.MaxTerms, nLike)
	}

	for _, b := range buildAllSparse(q) {
		tsIdx := -1
		for i, a := range b.args {
			if s, ok := a.(string); ok && s == q.SparseTerms.TSQuery {
				tsIdx = i + 1
			}
		}
		if tsIdx < 0 {
			t.Fatalf("%s: TSQuery 인자가 없다", b.name)
		}
		if !strings.Contains(b.sql, fmt.Sprintf("to_tsquery('simple', $%d)", tsIdx)) {
			t.Errorf("%s: to_tsquery('simple', $%d) 가 없다", b.name, tsIdx)
		}
		if strings.Contains(b.sql, "ANY(ARRAY") || regexp.MustCompile(`LIKE\s+ANY`).MatchString(b.sql) {
			t.Errorf("%s: LIKE ANY 가 생성됐다(F11)", b.name)
		}
		for i := 0; i < nLike; i++ {
			p := fmt.Sprintf("LIKE '%%' || $%d || '%%'", tsIdx+1+i)
			if !strings.Contains(b.sql, p) {
				t.Errorf("%s: 키워드 %d 의 %q 가 없다", b.name, i+1, p)
			}
		}
		switch b.name {
		case "chunk_fts", "sparse_ctx":
			// 청크 레인에서는 원문 AND tsquery 가 완전히 사라진다.
			if strings.Contains(b.sql, "plainto_tsquery") {
				t.Errorf("%s: 키워드 모드인데 plainto_tsquery 가 남았다", b.name)
			}
		case "fulltext":
			if strings.Contains(b.sql, "plainto_tsquery") {
				t.Errorf("%s: 키워드 모드인데 plainto_tsquery 가 남았다", b.name)
			}
			if !strings.Contains(b.sql, fmt.Sprintf("to_tsquery('english', $%d)", tsIdx)) {
				t.Errorf("%s: english tsquery 가 없다", b.name)
			}
		}
	}
}

// TestSparseQueryBuilders_HybridUntouchedLanes 는 chunk_doc 모드에서도
// vec·summvec·entity 레인 SQL 이 raw 와 바이트 단위로 같은지 본다. 특히
// 엔티티 레인은 질문 전체를 이름과 비교하는 방향 문제(F3)가 별도 이슈라
// 이번 변경이 건드리면 안 된다.
func TestSparseQueryBuilders_HybridUntouchedLanes(t *testing.T) {
	t.Parallel()
	entityOn := model.SearchWeights{}.Defaults()
	entityOn.EntityWeight = model.DefaultEntityWeight
	section := func(sql, start, end string) string {
		i := strings.Index(sql, start)
		j := strings.Index(sql[i:], end)
		if i < 0 || j < 0 {
			t.Fatalf("section %q..%q not found", start, end)
		}
		return sql[i : i+j]
	}
	for _, c := range sparseSnapshotCases() {
		rawSQL, _ := buildHybridSearchQuery(c.q, entityOn)
		q := c.q
		q.SparseTerms = sparseTermsFixture()["full"]
		termSQL, _ := buildHybridSearchQuery(q, entityOn)
		for _, lane := range [][2]string{
			{"vec AS (", "bigm AS ("},
			{"summvec AS (", "rrf AS ("},
			{"rrf AS (", "\n\t\tSELECT d.id"},
		} {
			if a, b := section(rawSQL, lane[0], lane[1]), section(termSQL, lane[0], lane[1]); a != b {
				t.Errorf("%s: %q 구간이 키워드 모드에서 바뀌었다\nraw:  %s\nterm: %s", c.name, lane[0], a, b)
			}
		}
		if !strings.Contains(section(termSQL, "entity AS (", "rrf AS ("), "e.normalized_name LIKE '%%' || $") {
			t.Errorf("%s: 엔티티 레인 형태가 바뀌었다", c.name)
		}
		if section(rawSQL, "entity AS (", "rrf AS (") != section(termSQL, "entity AS (", "rrf AS (") {
			t.Errorf("%s: 엔티티 CTE 가 키워드 모드에서 바뀌었다", c.name)
		}
		// fts·bigm 은 실제로 바뀌어야 한다(양성 대조군).
		if section(rawSQL, "WITH fts AS (", "vec AS (") == section(termSQL, "WITH fts AS (", "vec AS (") {
			t.Errorf("%s: fts 레인이 키워드 모드에서 그대로다", c.name)
		}
		if section(rawSQL, "bigm AS (", "summvec AS (") == section(termSQL, "bigm AS (", "summvec AS (") {
			t.Errorf("%s: bigm 레인이 키워드 모드에서 그대로다", c.name)
		}
	}
}
