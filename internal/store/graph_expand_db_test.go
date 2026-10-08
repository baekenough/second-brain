package store

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// GraphHubDamping · RRFMissingRank · GraphSupportCounts 의 실DB 검사. 규칙은
// entity_keywords_db_test.go 와 같다: TEST_DATABASE_URL 이 없으면 건너뛰고, 그
// 값은 일회용 DB 만 가리켜야 하며, 데이터는 전부 가상이다. 문서는 sparseDB 의
// source_id 접두로, 엔티티는 graphDBEntityPrefix 로 정리한다.

// entityID 는 픽스처 엔티티의 id 를 돌려준다(UpsertEntity 는 멱등).
func entityID(t *testing.T, pg *Postgres, name string) int64 {
	t.Helper()
	id, err := NewEntityStore(pg).UpsertEntity(context.Background(), graphDBEntityPrefix+name, model.EntityTypeOther)
	if err != nil {
		t.Fatalf("upsert entity %s: %v", name, err)
	}
	return id
}

func seedRelations(t *testing.T, pg *Postgres, rels ...model.EntityRelation) {
	t.Helper()
	if err := NewRelationStore(pg).UpsertEntityRelations(context.Background(), rels); err != nil {
		t.Fatalf("seed relations: %v", err)
	}
}

func relation(from, to int64, doc uuid.UUID, conf float64) model.EntityRelation {
	return model.EntityRelation{FromEntityID: from, ToEntityID: to, Type: model.RelationRelatedTo,
		EvidenceDocumentID: doc, Confidence: conf, ObservedAt: time.Now()}
}

// TestDB_GraphLane_HubDamping 은 허브 감쇠가 "많이 언급된 시드" 에서 나온 관계를
// 눌러 순서를 뒤집는지 본다. 허브(zz-lr-hub)는 창 밖 문서 60건에 언급되고, 희귀
// 시드(zz-lr-rare)는 언급이 없다.
//
//	감쇠 off: hubEv(0.9) > rareEv(0.5)
//	감쇠 on : hubEv 0.9/ln(e+60)≈0.219 < rareEv 0.5/ln(e)=0.5
//
// 감쇠 레인도 필터를 레인 안에 그대로 건다(창 밖·disposable 근거는 제외).
func TestDB_GraphLane_HubDamping(t *testing.T) {
	pg := sparseDB(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if _, err := pg.pool.Exec(context.Background(),
			`DELETE FROM entities WHERE normalized_name LIKE $1`, graphDBEntityPrefix+"%"); err != nil {
			t.Errorf("cleanup entities: %v", err)
		}
	})
	in := graphWinFrom.Add(2 * time.Hour)
	seed := func(at time.Time, title, meta string) uuid.UUID {
		return seedSparseDoc(t, pg, model.SourceCalendar, at, title, "zz 가상 본문 "+title, meta)
	}
	hubEv := seed(in, "zz 허브 근거", `{}`)
	rareEv := seed(in, "zz 희귀 근거", `{}`)
	rareDisposable := seed(in, "zz 희귀 광고", `{"retention":"disposable"}`)
	rareOutside := seed(graphWinTo.Add(time.Hour), "zz 희귀 창 밖", `{}`)

	hub, rare, x, y := entityID(t, pg, "hub"), entityID(t, pg, "rare"), entityID(t, pg, "x"), entityID(t, pg, "y")
	es := NewEntityStore(pg)
	for i := 0; i < 60; i++ {
		filler := seed(graphWinTo.Add(48*time.Hour), "zz 허브 언급", `{}`)
		if err := es.LinkDocumentEntity(ctx, filler, hub); err != nil {
			t.Fatal(err)
		}
	}
	seedRelations(t, pg,
		relation(hub, x, hubEv, 0.9),
		relation(y, rare, rareEv, 0.5), // 시드가 to 쪽
		relation(rare, y, rareDisposable, 1.0),
		relation(rare, y, rareOutside, 1.0),
	)

	ds := NewDocumentStore(pg)
	run := func(damping bool) map[uuid.UUID]float64 {
		t.Helper()
		q := graphDBQuery("질문", []string{"zz-lr-hub", "zz-lr-rare"}, 1)
		q.Tuning.GraphHubDamping = damping
		got, err := ds.hybridSearch(ctx, q)
		if err != nil {
			t.Fatalf("hybridSearch(damping=%v): %v", damping, err)
		}
		return resultDocIDs(got)
	}

	plain := run(false)
	if !(plain[hubEv] > plain[rareEv] && plain[rareEv] > laneScoreThreshold) {
		t.Fatalf("음성 대조군: 감쇠 없이 허브 근거가 앞서야 한다: hub=%v rare=%v", plain[hubEv], plain[rareEv])
	}
	damped := run(true)
	if !(damped[rareEv] > damped[hubEv] && damped[hubEv] > laneScoreThreshold) {
		t.Errorf("감쇠가 허브를 누르지 못했다: hub=%v rare=%v", damped[hubEv], damped[rareEv])
	}
	for name, id := range map[string]uuid.UUID{"disposable": rareDisposable, "outside": rareOutside} {
		if damped[id] > laneScoreThreshold {
			t.Errorf("감쇠 레인에 걸러졌어야 할 %s 근거가 들어왔다", name)
		}
	}
}

// TestDB_RRFMissingRankCutoff 는 cutoff 식이 실제 PostgreSQL 에서 돌고($3 산술),
// 비어 있지 않은 레인에 없는 문서만 w/(k+$3+1) 을 받는지 본다.
func TestDB_RRFMissingRankCutoff(t *testing.T) {
	f := newGraphFixture(t)
	ds := NewDocumentStore(f.pg)
	ctx := context.Background()
	run := func(keywords []string, mode string) map[uuid.UUID]float64 {
		t.Helper()
		q := graphDBQuery("질문", keywords, 1)
		q.Tuning.RRFMissingRank = mode
		got, err := ds.hybridSearch(ctx, q)
		if err != nil {
			t.Fatalf("hybridSearch(%q): %v", mode, err)
		}
		return resultDocIDs(got)
	}
	// Limit 50 → 레인 상한 $3 = 100 → cutoff 기여 1/(60+101).
	cut := 1.0 / 161

	zero := run([]string{"zz-lr-alice"}, model.RRFMissingRankZero)
	cutoff := run([]string{"zz-lr-alice"}, model.RRFMissingRankCutoff)

	// plain 은 엔티티·그래프 레인 어디에도 없다 → 두 레인에서 각각 cutoff.
	if d := cutoff[f.plain] - zero[f.plain]; math.Abs(d-2*cut) > 1e-6 {
		t.Errorf("plain: cutoff 증분 %v, want %v (엔티티+그래프 두 레인)", d, 2*cut)
	}
	// outEv 는 그래프 레인에 있고 엔티티 레인에 없다 → 엔티티 레인 cutoff 하나.
	if d := cutoff[f.outEv] - zero[f.outEv]; math.Abs(d-cut) > 1e-6 {
		t.Errorf("outEv: cutoff 증분 %v, want %v", d, cut)
	}
	// 레인 순위를 가진 문서는 여전히 레인에 없는 문서보다 앞선다.
	if !(cutoff[f.outEv] > cutoff[f.plain] && cutoff[f.seedDoc] > cutoff[f.plain]) {
		t.Errorf("레인 적중이 cutoff 문서보다 앞서지 않는다: out=%v seed=%v plain=%v",
			cutoff[f.outEv], cutoff[f.seedDoc], cutoff[f.plain])
	}

	// 키워드가 아무 엔티티와도 안 맞으면 엔티티·그래프 레인이 비어 cutoff 도 0 이다.
	empty := run([]string{"zz-lr-nobody"}, model.RRFMissingRankCutoff)
	if empty[f.plain] > laneScoreThreshold {
		t.Errorf("빈 레인이 cutoff 기여를 줬다: %v", empty[f.plain])
	}
}

// TestDB_GraphSupportCounts 는 결과 시드 확장의 지지 집계를 본다: 시드 문서
// 엔티티 ∪ 키워드 엔티티, 서로 다른 관계 수, 필터, 후보 밖 문서 제외, 자기 엔티티만으로
// 이어진 관계 제외.
func TestDB_GraphSupportCounts(t *testing.T) {
	f := newGraphFixture(t)
	ds := NewDocumentStore(f.pg)
	ctx := context.Background()
	alice, bobby, carol := entityID(t, f.pg, "alice"), entityID(t, f.pg, "bobby"), entityID(t, f.pg, "carol")
	seedRelations(t, f.pg,
		relation(bobby, alice, f.outEv, 0.4),   // outEv 의 두 번째 관계
		relation(alice, carol, f.seedDoc, 0.8), // 시드 문서 자기 관계
	)

	base := graphDBQuery("질문", nil, 0)
	all := []uuid.UUID{f.seedDoc, f.outEv, f.inEv, f.outsideEv, f.disposableEv, f.gmailEv, f.deletedEv, f.unrelatedEv, f.plain}

	got, err := ds.GraphSupportCounts(ctx, base, []uuid.UUID{f.seedDoc}, all)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]int{f.outEv: 2, f.inEv: 1}
	if len(got) != len(want) || got[f.outEv] != 2 || got[f.inEv] != 1 {
		t.Errorf("시드 문서 지지 = %v, want outEv=2 inEv=1 만 (필터·자기 관계 제외)", named(f, got))
	}

	// 후보에 없는 문서는 지지가 있어도 돌려주지 않는다(재정렬만).
	got, err = ds.GraphSupportCounts(ctx, base, []uuid.UUID{f.seedDoc}, []uuid.UUID{f.seedDoc, f.inEv})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := got[f.outEv]; leaked || got[f.inEv] != 1 {
		t.Errorf("후보 밖 문서가 나왔거나 inEv 가 빠졌다: %v", named(f, got))
	}

	// 키워드 시드는 출처 문서가 없으므로 시드 문서 자기 관계도 지지로 센다.
	kw := base
	kw.EntityKeywords = []string{"zz-lr-alice"}
	got, err = ds.GraphSupportCounts(ctx, kw, nil, all)
	if err != nil {
		t.Fatal(err)
	}
	if got[f.seedDoc] != 1 || got[f.outEv] != 2 || got[f.inEv] != 1 || len(got) != 3 {
		t.Errorf("키워드 시드 지지 = %v, want seedDoc=1 outEv=2 inEv=1", named(f, got))
	}

	// 삭제 포함·필터 없음이면 걸러졌던 근거 문서도 센다(필터가 실제로 일하고 있다는 양성 대조군).
	open := model.SearchQuery{IncludeDeleted: true}
	got, err = ds.GraphSupportCounts(ctx, open, []uuid.UUID{f.seedDoc}, all)
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{"outsideEv": f.outsideEv, "disposableEv": f.disposableEv, "gmailEv": f.gmailEv, "deletedEv": f.deletedEv} {
		if got[id] != 1 {
			t.Errorf("필터 없는 질의에서 %s 지지 = %d, want 1", name, got[id])
		}
	}
	if got[f.unrelatedEv] != 0 || got[f.plain] != 0 {
		t.Error("시드와 무관한 문서가 지지를 받았다")
	}
}

func named(f graphFixture, m map[uuid.UUID]int) map[string]int {
	names := map[uuid.UUID]string{
		f.seedDoc: "seedDoc", f.outEv: "outEv", f.inEv: "inEv", f.outsideEv: "outsideEv",
		f.disposableEv: "disposableEv", f.gmailEv: "gmailEv", f.deletedEv: "deletedEv",
		f.unrelatedEv: "unrelatedEv", f.plain: "plain",
	}
	out := map[string]int{}
	for id, n := range m {
		out[names[id]] = n
	}
	return out
}
