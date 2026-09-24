package store

import (
	"fmt"
	"strings"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/sparseq"
)

// sparseSQL 은 model.SparseTerms 를 바인딩한 플레이스홀더 묶음이다(#276).
// 모든 조각은 플레이스홀더 번호만 SQL 에 넣고 값은 절대 넣지 않는다 —
// tsquery 문자열도 LIKE 키워드도 파라미터로만 간다.
type sparseSQL struct {
	// ts 는 tsquery 문자열의 플레이스홀더("$7")다. 비어 있으면 이 질의에는
	// tsquery 로 쓸 렉심이 없다는 뜻이고 tsquery 조건은 false 가 된다.
	ts string
	// like 는 LIKE 키워드별 플레이스홀더다. 최대 sparseq.MaxTerms 개.
	like []string
}

// appendSparseTerms 는 키워드를 args 끝에 붙이고 플레이스홀더를 돌려준다.
// 반드시 다른 모든 인자를 붙인 뒤 마지막에 호출한다 — 그래야 키워드가 없을
// 때(raw 모드)와 있을 때 기존 플레이스홀더 번호가 하나도 움직이지 않고,
// 엔티티 CTE 같은 기존 조각이 바이트 단위로 그대로 남는다.
//
// LIKE 키워드는 이스케이프하지 않는다. sparseq 의 Like 키워드에는 '%' 와
// '\' 가 들어갈 수 없고(분리 문자), '_' 만 한 글자 와일드카드로 남는다 —
// 질문 원문을 그대로 바인딩하는 raw 경로와 같은 의미다.
func appendSparseTerms(args []interface{}, t model.SparseTerms) ([]interface{}, sparseSQL) {
	var s sparseSQL
	if t.TSQuery != "" {
		args = append(args, t.TSQuery)
		s.ts = fmt.Sprintf("$%d", len(args))
	}
	for i, term := range t.Like {
		if i == sparseq.MaxTerms {
			break
		}
		args = append(args, term)
		s.like = append(s.like, fmt.Sprintf("$%d", len(args)))
	}
	return args, s
}

// tsMatch 는 "col @@ to_tsquery('cfg', $n)" 이다. 렉심이 없으면 "false".
func (s sparseSQL) tsMatch(col, cfg string) string {
	if s.ts == "" {
		return "false"
	}
	return fmt.Sprintf("%s @@ to_tsquery('%s', %s)", col, cfg, s.ts)
}

// tsRank 는 "ts_rank(col, to_tsquery('cfg', $n))" 이다. 렉심이 없으면 "0".
// OR 로 묶인 tsquery 에서 ts_rank 는 더 많은 렉심이 맞은 행에 더 높은 점수를
// 주므로 별도의 일치 개수 항은 두지 않는다.
func (s sparseSQL) tsRank(col, cfg string) string {
	if s.ts == "" {
		return "0"
	}
	return fmt.Sprintf("ts_rank(%s, to_tsquery('%s', %s))", col, cfg, s.ts)
}

// termMatch 는 키워드 p 하나가 cols 중 어디에든 부분 문자열로 들어 있는지를
// 본다. contact 가 true 이면 통화 문서의 contact_name 조건도 붙인다(raw
// bigm 레인의 contact_name strpos 조건을 키워드별로 펼친 것).
func termMatch(p string, contact bool, cols ...string) string {
	parts := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf("%s LIKE '%%' || %s || '%%'", c, p))
	}
	if contact {
		parts = append(parts, fmt.Sprintf("(source_type = 'call' AND strpos(lower(metadata->>'contact_name'), lower(%s)) > 0)", p))
	}
	return strings.Join(parts, " OR ")
}

// likeAny 는 키워드 하나라도 맞으면 참인 조건이다. 키워드마다 플레이스홀더를
// 따로 두고 OR 로 펼친다 — pg_bigm 의 GIN 인덱스는 LIKE ANY(array) 를
// 인덱스 스캔으로 쓰지 못하지만, 명시적 OR 는 플래너가 BitmapOr 로 인덱스를
// 합칠 수 있다. 키워드가 없으면 "false".
func (s sparseSQL) likeAny(contact bool, cols ...string) string {
	if len(s.like) == 0 {
		return "false"
	}
	parts := make([]string, 0, len(s.like))
	for _, p := range s.like {
		parts = append(parts, termMatch(p, contact, cols...))
	}
	return "(" + strings.Join(parts, "\n\t\t\t    OR ") + ")"
}

// likeCount 는 cols 에 맞은 키워드 개수(정수)다. CASE 로 감싸는 이유:
// title·contact_name 이 NULL 이면 LIKE 결과가 NULL 이 되고, NULL::int 를 더한
// 합은 NULL 이 되어 DESC 정렬에서 맨 앞으로 올라간다.
func (s sparseSQL) likeCount(contact bool, cols ...string) string {
	if len(s.like) == 0 {
		return "0"
	}
	parts := make([]string, 0, len(s.like))
	for _, p := range s.like {
		parts = append(parts, fmt.Sprintf("CASE WHEN %s THEN 1 ELSE 0 END", termMatch(p, contact, cols...)))
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

// chunkSparseRank 는 청크 레인의 bigm 쪽 점수다. raw 경로의 상수 0.01 을
// "맞은 키워드 비율 × 0.01, 질문 전체가 맞으면 0.01 추가" 로 바꾼다. 최대값이
// 0.02 라 bigm 만 맞은 청크는 여전히 진짜 FTS 히트(보통 0.01~0.5) 아래에
// 머문다 — raw 경로 주석의 근거(#146)를 그대로 따른다.
func (s sparseSQL) chunkSparseRank(col string) string {
	return fmt.Sprintf("(0.01 * %s / %d + CASE WHEN %s LIKE '%%' || $1 || '%%' THEN 0.01 ELSE 0 END)",
		s.likeCount(false, col), max(len(s.like), 1), col)
}

// chunkLaneExprs 는 청크 희소 레인 SQL 에서 질의 형태에 따라 바뀌는 네
// 조각이다. raw 모드(키워드 없음)의 값은 #276 이전 SQL 과 글자 하나까지 같다
// — testdata/sparse_query_raw.golden 이 이를 고정한다.
type chunkLaneExprs struct {
	rankTS, rankBigm   string // GREATEST(rankTS, rankBigm) AS rank
	matchTS, matchLike string // WHERE (matchTS OR matchLike)
}

// chunkSparseExprs 는 tsvCol/textCol 에 대한 조각을 만든다. sp 가 nil 이면
// raw 모드다.
func chunkSparseExprs(sp *sparseSQL, tsvCol, textCol string) chunkLaneExprs {
	if sp == nil {
		return chunkLaneExprs{
			rankTS:    "ts_rank(" + tsvCol + ", plainto_tsquery('simple', $1))",
			rankBigm:  "CASE WHEN " + textCol + " LIKE '%%' || $1 || '%%' THEN 0.01 ELSE 0 END",
			matchTS:   tsvCol + " @@ plainto_tsquery('simple', $1)",
			matchLike: textCol + " LIKE '%%' || $1 || '%%'",
		}
	}
	return chunkLaneExprs{
		rankTS:    sp.tsRank(tsvCol, "simple"),
		rankBigm:  sp.chunkSparseRank(textCol),
		matchTS:   sp.tsMatch(tsvCol, "simple"),
		matchLike: sp.likeAny(false, textCol),
	}
}

// bindChunkSparse 는 청크 빌더의 공통 앞부분이다: 키워드가 있으면 args 끝에
// 붙이고 플레이스홀더를, 없으면 nil 을 돌려준다.
func bindChunkSparse(args []interface{}, t model.SparseTerms) ([]interface{}, *sparseSQL) {
	if !t.Active() {
		return args, nil
	}
	args, sp := appendSparseTerms(args, t)
	return args, &sp
}

// docSparseRank 는 fulltext 경로(임베딩 없음)의 bigm 쪽 점수로,
// chunkSparseRank 와 같은 척도를 content·title·contact_name 에 적용한다.
func (s sparseSQL) docSparseRank() string {
	return fmt.Sprintf("(0.01 * %s / %d + CASE WHEN content LIKE '%%' || $1 || '%%' OR title LIKE '%%' || $1 || '%%' THEN 0.01 ELSE 0 END)",
		s.likeCount(true, "content", "title"), max(len(s.like), 1))
}
