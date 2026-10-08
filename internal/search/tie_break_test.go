package search

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// 점수가 같은 결과(짧고 비슷한 SMS)는 흔하고, 많은 호출자가 map 을 펼친 순서로 입력을
// 만든다. sortByScore 는 입력 순서와 무관한 전순서여야 한다:
// 점수 내림차순 → 사건 시각 최신순(없으면 뒤) → ID 오름차순.
func tieFixture() (results []*model.SearchResult, want []uuid.UUID) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	newer, older := t0.Add(time.Hour), t0
	mk := func(id byte, score float64, at *time.Time) *model.SearchResult {
		var u uuid.UUID
		u[15] = id
		r := makeSearchResult(u, "t", score)
		r.OccurredAt = at
		return r
	}
	results = []*model.SearchResult{
		mk(9, 0.5, &older), mk(3, 0.5, &newer), mk(7, 0.5, &newer), mk(1, 0.5, nil),
		mk(2, 0.9, nil), mk(5, 0.5, &older), mk(8, 0.1, &newer), mk(4, math.NaN(), &newer),
		mk(6, 0.5, nil),
	}
	// 0.9 → 0.5 중 최신(3, 7: ID 순) → 0.5 중 older(5, 9) → 시각 없음(1, 6) → 0.1 → NaN 마지막.
	for _, id := range []byte{2, 3, 7, 5, 9, 1, 6, 8, 4} {
		var u uuid.UUID
		u[15] = id
		want = append(want, u)
	}
	return results, want
}

func TestSortByScore_TotalOrderIndependentOfInputOrder(t *testing.T) {
	t.Parallel()
	results, want := tieFixture()
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		shuffled := append([]*model.SearchResult(nil), results...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		sortByScore(shuffled)
		if got := idsOf(shuffled); !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle %d: 순서가 입력에 따라 달라졌다\n got: %v\nwant: %v", i, got, want)
		}
	}
}

// mergeRRF 도 같은 입력 집합이면 입력 슬라이스 안의 동점 순서와 무관한 결과를 돌려준다
// (맵을 펼친 뒤 점수 → ID 로 전순서 정렬).
func TestMergeRRF_TieOrderDeterministic(t *testing.T) {
	t.Parallel()
	mk := func(id byte) *model.SearchResult {
		var u uuid.UUID
		u[15] = id
		return makeSearchResult(u, "t", 0.5)
	}
	primary := []*model.SearchResult{mk(4), mk(2)}
	secondary := []*model.SearchResult{mk(3), mk(1)}
	var first []uuid.UUID
	for i := 0; i < 50; i++ {
		got := idsOf(mergeRRFMode(primary, secondary, 10, model.MergeSymmetric))
		if first == nil {
			first = got
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: %v != %v", i, got, first)
		}
	}
	// 같은 순위의 primary/secondary 는 ID 오름차순: (4,3)=rank1 → 3,4 / (2,1)=rank2 → 1,2.
	var want []uuid.UUID
	for _, id := range []byte{3, 4, 1, 2} {
		var u uuid.UUID
		u[15] = id
		want = append(want, u)
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("got %v, want %v", first, want)
	}
}
