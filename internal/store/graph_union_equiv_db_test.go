package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// 그래프 레인 관계 선택을 "IN (SELECT ...) OR IN (SELECT ...)" 에서 from/to 등치
// 조인 UNION ALL + 관계 id 중복 제거(graphRelCTE)로 바꾼 것이 결과 집합과 순서를
// 바꾸지 않는다는 실DB 검사. 기준은 바로 아래에 글자 그대로 옮겨 둔 예전 빌더다
// (79e0bf7 의 buildGraphCTEs / buildDampedGraphCTEs). 양 끝점이 모두 시드인 관계와
// 자기 루프 관계가 두 번 세지면 순서가 달라지도록 픽스처를 짰다.

// legacyGraphCTEs 는 79e0bf7 의 그래프 레인 빌더 사본이다. 동등성 기준으로만 쓴다.
func legacyGraphCTEs(kwParam, prefixParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter string, hubDamping bool) string {
	if hubDamping {
		return legacyDampedGraphCTEs(kwParam, prefixParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
	}
	return fmt.Sprintf(`,
		graph_seed AS (
			SELECT e.id
			FROM entities e
			WHERE %s
		),
		graph AS (
			SELECT er.evidence_document_id AS id,
			       row_number() OVER (ORDER BY SUM(er.confidence) DESC, d.occurred_at DESC NULLS LAST, er.evidence_document_id ASC) AS rank
			FROM entity_relations er
			JOIN documents d ON d.id = er.evidence_document_id
			WHERE (er.from_entity_id IN (SELECT id FROM graph_seed)
			    OR er.to_entity_id   IN (SELECT id FROM graph_seed))
			%s
			%s
			%s
			%s
			%s
			GROUP BY er.evidence_document_id, d.occurred_at
			ORDER BY rank
			LIMIT $3
		)`, legacyEntityKeywordMatch("e", kwParam, prefixParam),
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
}

// legacyDampedGraphCTEs 는 79e0bf7 의 buildDampedGraphCTEs 사본이다.
func legacyDampedGraphCTEs(kwParam, prefixParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter string) string {
	return fmt.Sprintf(`,
		graph_seed AS (
			SELECT e.id,
			       1.0::float8 / ln(exp(1.0::float8) + m.n) AS w
			FROM entities e
			CROSS JOIN LATERAL (
				SELECT count(*) AS n
				FROM document_entities de
				WHERE de.entity_id = e.id
			) m
			WHERE %s
		),
		graph AS (
			SELECT er.evidence_document_id AS id,
			       row_number() OVER (ORDER BY SUM(er.confidence::float8 * GREATEST(gf.w, gt.w)) DESC, d.occurred_at DESC NULLS LAST, er.evidence_document_id ASC) AS rank
			FROM entity_relations er
			JOIN documents d ON d.id = er.evidence_document_id
			LEFT JOIN graph_seed gf ON gf.id = er.from_entity_id
			LEFT JOIN graph_seed gt ON gt.id = er.to_entity_id
			WHERE (er.from_entity_id IN (SELECT id FROM graph_seed)
			    OR er.to_entity_id   IN (SELECT id FROM graph_seed))
			%s
			%s
			%s
			%s
			%s
			GROUP BY er.evidence_document_id, d.occurred_at
			ORDER BY rank
			LIMIT $3
		)`, legacyEntityKeywordMatch("e", kwParam, prefixParam),
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
}

// legacyEntityKeywordMatch 는 상한 없는 예전 시드 매칭(79e0bf7)이다. 시드가
// MaxEntityKeywordSeeds 개 미만이면 entityKeywordSeeds 와 같은 집합을 고른다.
func legacyEntityKeywordMatch(alias, kwParam, prefixParam string) string {
	return fmt.Sprintf("(%[1]s.normalized_name = ANY(%[2]s::text[]) OR %[1]s.normalized_name LIKE ANY(%[3]s::text[]))",
		alias, kwParam, prefixParam)
}

type laneRow struct {
	ID   uuid.UUID
	Rank int64
}

// runGraphLaneOnly 는 그래프 CTE 조각만으로 "SELECT id, rank FROM graph" 를 돌린다.
// filtered 이면 $4..$7 에 소스(calendar)·retention(disposable)·창 필터를 건다.
func runGraphLaneOnly(t *testing.T, pg *Postgres, build func(kw, prefix, status, source, exclude, retention, occurred string, damping bool) string,
	keywords []string, damping, filtered bool) []laneRow {
	t.Helper()
	kws := NormalizeEntityKeywords(keywords)
	args := []interface{}{kws, entityKeywordPrefixes(kws), 50}
	status, source, retention, occurred := "", "", "", ""
	if filtered {
		status = "AND d.status = 'active'"
		source = "AND d.source_type = ANY($4)"
		retention = "AND COALESCE(d.metadata->>'retention', '') <> ALL($5)"
		occurred = "AND d.occurred_at >= $6\n\t\t\tAND d.occurred_at < $7"
		args = append(args, []string{string(model.SourceCalendar)}, []string{model.RetentionDisposable}, graphWinFrom, graphWinTo)
	}
	ctes := build("$1", "$2", status, source, "", retention, occurred, damping)
	sql := "WITH " + strings.TrimLeft(ctes, ",\n\t ") + "\nSELECT id, rank FROM graph ORDER BY rank"
	rows, err := pg.pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("graph lane (damping=%v filtered=%v): %v", damping, filtered, err)
	}
	defer rows.Close()
	var out []laneRow
	for rows.Next() {
		var r laneRow
		if err := rows.Scan(&r.ID, &r.Rank); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDB_GraphLane_IndexedUnionMatchesLegacy(t *testing.T) {
	f := newGraphFixture(t)
	alice, acme := entityID(t, f.pg, "alice"), entityID(t, f.pg, "acme")
	selfEv := seedSparseDoc(t, f.pg, model.SourceCalendar, graphWinFrom.Add(time.Hour), "zz 자기 루프", "zz 가상 본문 자기 루프", `{}`)
	seedRelations(t, f.pg,
		relation(alice, alice, selfEv, 0.6),                     // 자기 루프: 두 가지(from·to) 모두에서 나온다
		relation(acme, entityID(t, f.pg, "bobby"), f.inEv, 0.3), // inEv 의 두 번째 관계(이번엔 from 쪽이 시드)
	)
	// outEv 는 alice -> acme (양 끝 모두 시드, 0.9). 두 번 세면 1.8 로, selfEv 는 1.2 로
	// 올라가 순서가 outEv, selfEv, inEv 로 바뀐다.
	keywords := []string{"zz-lr-alice", "zz-lr-acme"}

	for _, damping := range []bool{false, true} {
		for _, filtered := range []bool{true, false} {
			name := fmt.Sprintf("damping=%v filtered=%v", damping, filtered)
			legacy := runGraphLaneOnly(t, f.pg, legacyGraphCTEs, keywords, damping, filtered)
			got := runGraphLaneOnly(t, f.pg, buildGraphCTEs, keywords, damping, filtered)
			if len(legacy) == 0 {
				t.Fatalf("%s: 기준 레인이 비었다 — 비교가 공허하다", name)
			}
			if !reflect.DeepEqual(got, legacy) {
				t.Errorf("%s: 결과가 예전 형태와 다르다\n got=%v\nwant=%v", name, got, legacy)
			}
		}
	}

	// 동등성만으로는 "둘 다 틀림" 을 못 잡으므로 기대 순서를 직접 고정한다.
	//   감쇠 off: outEv 0.9 > inEv 0.5+0.3 > selfEv 0.6
	//   감쇠 on : alice 언급 1건(w≈0.761), acme 0건(w=1)
	//             outEv 0.9 > inEv 0.5*0.761+0.3 ≈ 0.68 > selfEv 0.6*0.761 ≈ 0.457
	want := []uuid.UUID{f.outEv, f.inEv, selfEv}
	for _, damping := range []bool{false, true} {
		got := runGraphLaneOnly(t, f.pg, buildGraphCTEs, keywords, damping, true)
		ids := make([]uuid.UUID, len(got))
		for i, r := range got {
			ids[i] = r.ID
			if r.Rank != int64(i+1) {
				t.Errorf("damping=%v: rank %d at %d", damping, r.Rank, i)
			}
		}
		if !reflect.DeepEqual(ids, want) {
			t.Errorf("damping=%v: 순서 = %v, want outEv, inEv, selfEv (%v)", damping, ids, want)
		}
	}
}
