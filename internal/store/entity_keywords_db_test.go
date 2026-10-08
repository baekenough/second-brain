package store

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// 엔티티 키워드 모드 + 그래프 1-hop 레인의 실DB 검사. TEST_DATABASE_URL 이 없으면
// 건너뛰며, 그 값은 절대 운영 DB 를 가리키면 안 된다(일회용 컨테이너만). 모든
// 데이터는 가상이다. 문서는 sparseDB 의 정리 규칙(source_id 접두)을 그대로
// 쓰고, 엔티티는 이 테스트가 이름 접두(graphDBEntityPrefix)로 직접 지운다.
//
// 격리: 문서를 2032-05 의 전용 시간창에 넣고 모든 질의에 그 창을 건다.

const graphDBEntityPrefix = "zz-lr-"

var (
	graphWinFrom = time.Date(2032, 5, 1, 0, 0, 0, 0, time.UTC)
	graphWinTo   = time.Date(2032, 5, 2, 0, 0, 0, 0, time.UTC)
)

type graphFixture struct {
	pg *Postgres
	// seedDoc 은 시드 엔티티(zz-lr-alice)에 직접 연결된 문서 — 엔티티 레인 적중.
	seedDoc uuid.UUID
	// outEv 는 alice -> acme 관계의 근거 문서(confidence 0.9), inEv 는
	// bobby -> alice 관계의 근거 문서(0.5). 둘 다 그래프 레인 적중, 순서는 out > in.
	outEv, inEv uuid.UUID
	// 아래는 관계의 근거 문서이지만 필터에 걸려야 하는 문서들.
	outsideEv, disposableEv, gmailEv, deletedEv uuid.UUID
	// unrelatedEv 는 시드와 무관한 엔티티 사이 관계의 근거 문서.
	unrelatedEv uuid.UUID
	// plain 은 어떤 엔티티·관계에도 연결되지 않은 문서.
	plain uuid.UUID
}

func newGraphFixture(t *testing.T) graphFixture {
	t.Helper()
	pg := sparseDB(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if _, err := pg.pool.Exec(context.Background(),
			`DELETE FROM entities WHERE normalized_name LIKE $1`, graphDBEntityPrefix+"%"); err != nil {
			t.Errorf("cleanup entities: %v", err)
		}
	})

	in := graphWinFrom.Add(3 * time.Hour)
	f := graphFixture{pg: pg}
	seed := func(src model.SourceType, at time.Time, title, meta string) uuid.UUID {
		return seedSparseDoc(t, pg, src, at, title, "zz 가상 본문 "+title, meta)
	}
	f.seedDoc = seed(model.SourceCalendar, in, "zz 시드 문서", `{}`)
	f.outEv = seed(model.SourceCalendar, in, "zz 근거 나가는 관계", `{}`)
	f.inEv = seed(model.SourceCalendar, in, "zz 근거 들어오는 관계", `{}`)
	f.outsideEv = seed(model.SourceCalendar, graphWinTo.Add(time.Hour), "zz 창 밖", `{}`)
	f.disposableEv = seed(model.SourceCalendar, in, "zz 광고", `{"retention":"disposable"}`)
	f.gmailEv = seed(model.SourceGmail, in, "zz 메일", `{}`)
	f.deletedEv = seed(model.SourceCalendar, in, "zz 삭제됨", `{}`)
	f.unrelatedEv = seed(model.SourceCalendar, in, "zz 무관", `{}`)
	f.plain = seed(model.SourceCalendar, in, "zz 평범", `{}`)
	if _, err := pg.pool.Exec(ctx,
		`UPDATE documents SET status = 'deleted', deleted_at = now() WHERE id = $1`, f.deletedEv); err != nil {
		t.Fatalf("soft-delete fixture doc: %v", err)
	}

	es := NewEntityStore(pg)
	ent := func(name string) int64 {
		id, err := es.UpsertEntity(ctx, graphDBEntityPrefix+name, model.EntityTypeOther)
		if err != nil {
			t.Fatalf("upsert entity %s: %v", name, err)
		}
		return id
	}
	alice, acme, bobby, carol, dave := ent("alice"), ent("acme"), ent("bobby"), ent("carol"), ent("dave")

	if err := es.LinkDocumentEntity(ctx, f.seedDoc, alice); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	rel := func(from, to int64, doc uuid.UUID, conf float64) model.EntityRelation {
		return model.EntityRelation{FromEntityID: from, ToEntityID: to, Type: model.RelationRelatedTo,
			EvidenceDocumentID: doc, Confidence: conf, ObservedAt: now}
	}
	rels := []model.EntityRelation{
		rel(alice, acme, f.outEv, 0.9),
		rel(bobby, alice, f.inEv, 0.5),
		rel(alice, acme, f.outsideEv, 0.9),
		rel(alice, acme, f.disposableEv, 0.9),
		rel(alice, acme, f.gmailEv, 0.9),
		rel(alice, acme, f.deletedEv, 0.9),
		rel(carol, dave, f.unrelatedEv, 1.0),
	}
	if err := NewRelationStore(pg).UpsertEntityRelations(ctx, rels); err != nil {
		t.Fatalf("seed relations: %v", err)
	}
	return f
}

// graphDBQuery 는 격리 창 + 기본 필터를 건 hybrid 질의다. 가중치는 임베딩·희소
// 레인의 기여를 사실상 0 으로 눌러, 점수가 엔티티·그래프 레인 몫인 문서와
// 아닌 문서를 가르게 한다(벡터 레인이 창 안 문서를 전부 돌려주기 때문).
func graphDBQuery(question string, keywords []string, graphWeight float64) model.SearchQuery {
	from, to := graphWinFrom, graphWinTo
	q := model.SearchQuery{
		Query:            question,
		Limit:            50,
		Embedding:        testEmbedding(),
		SourceTypes:      []model.SourceType{model.SourceCalendar},
		ExcludeRetention: []string{model.RetentionDisposable},
		OccurredFrom:     &from,
		OccurredTo:       &to,
		EntityKeywords:   keywords,
		Weights: model.SearchWeights{
			FTSWeight: 1e-9, VecWeight: 1e-9, BigmWeight: 1e-9,
			DisableSummaryVec: true,
			EntityWeight:      1,
		},
	}
	q.Tuning.GraphWeight = graphWeight
	return q
}

// laneScoreThreshold 는 "엔티티/그래프 레인이 점수를 준 문서" 와 "벡터 레인
// 잔여 점수(~1e-11)뿐인 문서" 를 가르는 경계다. 레인 한 개 몫은 1/61 ≈ 0.016.
const laneScoreThreshold = 1e-3

func TestDB_GraphLane_OneHopNeighbours(t *testing.T) {
	f := newGraphFixture(t)
	ds := NewDocumentStore(f.pg)
	ctx := context.Background()

	run := func(q model.SearchQuery) map[uuid.UUID]float64 {
		t.Helper()
		got, err := ds.hybridSearch(ctx, q)
		if err != nil {
			t.Fatalf("hybridSearch: %v", err)
		}
		return resultDocIDs(got)
	}
	hit := func(scores map[uuid.UUID]float64, id uuid.UUID) bool { return scores[id] > laneScoreThreshold }

	// 음성 대조군: 같은 질문을 키워드 없이(현행 엔티티 레인) 던지면 문장형 질문은
	// 엔티티 이름과 맞지 않아 어떤 문서도 레인 점수를 받지 못한다. 이 테스트가
	// 증명하려는 결함(질문 원문 LIKE 의 무력함)이 재현된다는 뜻이다.
	legacy := run(graphDBQuery("zz-lr-alice 와 연결된 문서 알려줘", nil, 0))
	for name, id := range map[string]uuid.UUID{"seedDoc": f.seedDoc, "outEv": f.outEv, "inEv": f.inEv} {
		if hit(legacy, id) {
			t.Fatalf("음성 대조군: 키워드 없는 현행 경로가 %s 를 레인으로 찾았다", name)
		}
	}

	// 정확 일치 키워드 + 그래프 레인.
	exact := run(graphDBQuery("zz-lr-alice 와 연결된 문서 알려줘", []string{"zz-lr-alice"}, 1))
	if !hit(exact, f.seedDoc) {
		t.Error("엔티티 레인(키워드 정확 일치)이 시드 문서를 못 찾았다")
	}
	if !hit(exact, f.outEv) || !hit(exact, f.inEv) {
		t.Errorf("그래프 레인이 from/to 양쪽 이웃 근거 문서를 못 찾았다: out=%v in=%v", exact[f.outEv], exact[f.inEv])
	}
	if exact[f.outEv] <= exact[f.inEv] {
		t.Errorf("SUM(confidence) 순서가 아니다: out(0.9)=%v in(0.5)=%v", exact[f.outEv], exact[f.inEv])
	}
	for name, id := range map[string]uuid.UUID{
		"outsideEv": f.outsideEv, "disposableEv": f.disposableEv, "gmailEv": f.gmailEv,
		"deletedEv": f.deletedEv, "unrelatedEv": f.unrelatedEv, "plain": f.plain,
	} {
		if hit(exact, id) {
			t.Errorf("그래프 레인에 걸러졌어야 할 %s 가 들어왔다", name)
		}
	}

	// 접두 키워드: "zz-lr-ali" 는 zz-lr-alice 의 접두이므로 같은 시드가 잡힌다.
	prefix := run(graphDBQuery("질문", []string{"zz-lr-ali"}, 1))
	if !hit(prefix, f.seedDoc) || !hit(prefix, f.outEv) {
		t.Error("접두 키워드가 시드 엔티티를 못 찾았다")
	}

	// 가중치 0: 엔티티 레인은 그대로, 그래프 레인만 사라진다.
	noGraph := run(graphDBQuery("질문", []string{"zz-lr-alice"}, 0))
	if !hit(noGraph, f.seedDoc) {
		t.Error("GraphWeight=0 에서 엔티티 레인이 사라졌다")
	}
	if hit(noGraph, f.outEv) || hit(noGraph, f.inEv) {
		t.Error("GraphWeight=0 인데 그래프 레인 문서가 점수를 받았다")
	}

	// 시드 키워드가 어떤 엔티티와도 안 맞으면 두 레인 모두 비어 있다.
	none := run(graphDBQuery("질문", []string{"zz-lr-nobody"}, 1))
	for name, id := range map[string]uuid.UUID{"seedDoc": f.seedDoc, "outEv": f.outEv, "inEv": f.inEv} {
		if hit(none, id) {
			t.Errorf("맞는 엔티티가 없는데 %s 가 레인 점수를 받았다", name)
		}
	}
}

// TestDB_GraphLane_LikeMetaCharsAreLiteral 은 키워드의 '_' 가 LIKE 와일드카드로
// 해석되지 않는지 본다("zz-lr-alic_" 가 zz-lr-alice 와 맞으면 안 된다).
func TestDB_GraphLane_LikeMetaCharsAreLiteral(t *testing.T) {
	f := newGraphFixture(t)
	ds := NewDocumentStore(f.pg)
	got, err := ds.hybridSearch(context.Background(), graphDBQuery("질문", []string{"zz-lr-alic_"}, 1))
	if err != nil {
		t.Fatal(err)
	}
	scores := resultDocIDs(got)
	if scores[f.seedDoc] > laneScoreThreshold || scores[f.outEv] > laneScoreThreshold {
		t.Error("'_' 가 와일드카드로 해석돼 엔티티가 맞았다")
	}
}
