package scheduler

import (
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// withChunkContextHeader 는 임베딩 입력만 조립한다. 저장되는 청크 본문은
// 호출자가 그대로 유지해야 하므로, 헤더가 없을 때 본문을 그대로 돌려주는지도
// 함께 확인한다.
//
// 헤더 내용 자체(라벨·날짜·제목·참여자 조립 규칙)의 골든 테이블은
// internal/chunkctx/chunkctx_test.go 로 옮겨졌다(#270 P0) — 이 파일은
// scheduler 가 그 결과를 청크 본문 앞에 붙이는 조립 부분만 검증한다.
func TestWithChunkContextHeader(t *testing.T) {
	doc := model.Document{
		SourceType: model.SourceSMS,
		Title:      "문자",
		Metadata:   map[string]any{"contact_name": "김영희"},
	}

	got := withChunkContextHeader(doc, "청크 본문")
	want := "[문자] 제목: 문자 · 상대: 김영희\n\n청크 본문"
	if got != want {
		t.Errorf("임베딩 입력 불일치\n got: %q\nwant: %q", got, want)
	}
	if !strings.HasSuffix(got, "\n\n청크 본문") {
		t.Errorf("청크 본문이 원형 그대로 유지돼야 한다: %q", got)
	}

	// 붙일 문맥이 전혀 없으면 본문만 돌려준다(앞머리에 빈 줄이 생기면 안 된다).
	empty := model.Document{SourceType: model.SourceType("")}
	if got := withChunkContextHeader(empty, "청크 본문"); got != "청크 본문" {
		t.Errorf("헤더가 없으면 본문 그대로여야 한다: %q", got)
	}
}

// 재임베딩 모드의 배치 크기가 평상시 백필보다 충분히 커야 한다.
//
// 근거: 청크 8.2만 건을 수집 주기(기본 10분)마다 100건씩 처리하면 827 사이클
// = 약 5.7일이 걸려 재임베딩이 사실상 불가능해진다. 이 상수를 다시 줄이려면
// 그 계산부터 다시 할 것.
func TestReembedBatchSizes_충분히_크다(t *testing.T) {
	if chunkReembedBatchSize <= chunkBackfillBatchSize {
		t.Errorf("청크 재임베딩 배치(%d)가 평상시 백필(%d) 이하다",
			chunkReembedBatchSize, chunkBackfillBatchSize)
	}
	if reembedBatchSize <= backfillBatchSize {
		t.Errorf("문서 재임베딩 배치(%d)가 평상시 백필(%d) 이하다",
			reembedBatchSize, backfillBatchSize)
	}

	// 청크 8.2만 건 기준으로 사이클 수가 200 이하인지(=10분 주기로 하루 반 이내).
	const productionChunkCount = 82_723
	if cycles := productionChunkCount / chunkReembedBatchSize; cycles > 200 {
		t.Errorf("전체 재임베딩에 %d 사이클이 필요하다(배치 %d) — 너무 느리다",
			cycles, chunkReembedBatchSize)
	}
}

// embeddingSelectorVersion 은 재임베딩이 꺼져 있으면 빈 문자열을 돌려줘야
// 한다. 이 성질이 깨지면 플래그를 켜지 않아도 전량 재임베딩이 시작된다.
func TestEmbeddingSelectorVersion(t *testing.T) {
	s := &Scheduler{chunkEmbedVersion: "m:1536:chunk-ctx-v1"}
	if got := s.embeddingSelectorVersion(s.chunkEmbedVersion); got != "" {
		t.Errorf("재임베딩이 꺼졌는데 선별 버전이 비어 있지 않다: %q", got)
	}

	s.reembedStale = true
	if got := s.embeddingSelectorVersion(s.chunkEmbedVersion); got != "m:1536:chunk-ctx-v1" {
		t.Errorf("재임베딩이 켜졌는데 선별 버전이 전달되지 않았다: %q", got)
	}

	// 버전 배선이 없으면(빈 문자열) 켜져 있어도 선별하지 않는다.
	empty := &Scheduler{reembedStale: true}
	if got := empty.embeddingSelectorVersion(empty.chunkEmbedVersion); got != "" {
		t.Errorf("버전 미배선인데 선별 버전이 생겼다: %q", got)
	}
}
