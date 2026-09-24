-- Demo corpus for deploy/ptah. Every person, address and number here is
-- fictional. The rows use the metadata keys the collectors write
-- (internal/chunkctx reads the same keys), so cmd/sparsectx builds the same
-- chunk headers it would build in production.
--
-- Idempotent: the demo can be re-run against the same database.

INSERT INTO documents (source_type, source_id, title, content, metadata, collected_at, occurred_at, title_summary, bullet_summary)
VALUES
('gmail', 'demo-mail-001', '3분기 마케팅 예산 검토 요청',
 E'안녕하세요 서연님,\n\n3분기 마케팅 예산 초안을 첨부합니다. 온라인 광고비가 지난 분기보다 18% 늘었고, 오프라인 행사 예산은 박람회 한 건을 줄여서 맞췄습니다.\n\n금요일 오전까지 검토 의견 주시면 월요일 경영회의 자료에 반영하겠습니다.\n\n김민준 드림',
 '{"from": "\"김민준\" <minjun.kim@example.com>", "to": "\"이서연\" <seoyeon.lee@example.com>"}',
 now(), '2026-09-15 09:12:00+09', '3분기 마케팅 예산 초안 검토 요청',
 E'- 온라인 광고비 18% 증가\n- 박람회 한 건 축소\n- 금요일 오전까지 검토 의견 요청'),
('gmail', 'demo-mail-002', 'RE: 3분기 마케팅 예산 검토 요청',
 E'민준님,\n\n예산안 확인했습니다. 온라인 광고비 증가는 동의하지만, 검색 광고 비중을 조금 낮추고 영상 광고를 늘리는 쪽이 좋겠습니다.\n\n박람회 축소는 영업팀과 한 번 더 이야기해 보겠습니다.\n\n이서연',
 '{"from": "\"이서연\" <seoyeon.lee@example.com>", "to": "\"김민준\" <minjun.kim@example.com>"}',
 now(), '2026-09-18 11:40:00+09', '예산안 회신: 영상 광고 확대 제안',
 E'- 온라인 광고비 증가 동의\n- 검색 광고 축소, 영상 광고 확대 제안\n- 박람회 축소는 영업팀과 재논의'),
('gmail', 'demo-mail-003', '제주 워크숍 숙소 예약 확인',
 E'10월 워크숍 숙소 예약이 확정되었습니다.\n\n- 일정: 10월 16일(목) ~ 10월 18일(토)\n- 장소: 제주 서귀포시 중문 리조트\n- 인원: 14명, 객실 7개\n\n항공권은 각자 예약하고 영수증을 제출해 주세요.',
 '{"from": "\"박지훈\" <jihoon.park@example.com>", "to": "team@example.com"}',
 now(), '2026-09-10 16:05:00+09', '10월 제주 워크숍 숙소 예약 확정',
 E'- 10월 16~18일 중문 리조트\n- 14명, 객실 7개\n- 항공권 개별 예약 후 영수증 제출'),
('gmail', 'demo-mail-004', '서버 이전 작업 공지',
 E'이번 주 토요일 새벽 2시부터 4시까지 데이터베이스 서버를 새 장비로 옮깁니다.\n\n작업 중에는 사내 위키와 검색 서비스가 잠시 멈춥니다. 작업이 끝나면 이 메일로 다시 알려드리겠습니다.',
 '{"from": "\"정우진\" <woojin.jung@example.com>", "to": "all@example.com"}',
 now(), '2026-09-17 18:30:00+09', '토요일 새벽 DB 서버 이전 작업',
 E'- 토요일 02:00~04:00\n- 위키와 검색 서비스 일시 중단'),
('sms', 'demo-sms-001', '문자: 최유나',
 E'내일 병원 예약 10시 반으로 바뀌었어. 주차는 지하 2층에 하면 돼.',
 '{"contact_name": "최유나", "direction": "incoming"}',
 now(), '2026-09-19 20:14:00+09', NULL, NULL),
('sms', 'demo-sms-002', '문자: 한도윤',
 E'이번 주말 이사 도와줄 수 있어? 토요일 아침 9시에 트럭 온대. 점심은 내가 살게.',
 '{"contact_name": "한도윤", "direction": "incoming"}',
 now(), '2026-09-16 22:03:00+09', NULL, NULL),
('sms', 'demo-sms-003', '문자: 최유나',
 E'엄마 생신 선물로 안마 의자 어때? 가격 알아보고 나눠서 내자.',
 '{"contact_name": "최유나", "direction": "incoming"}',
 now(), '2026-09-12 19:47:00+09', NULL, NULL),
('call', 'demo-call-001', '통화: 강하은',
 E'강하은: 계약서 초안은 받으셨죠? 3조 위약금 조항이 조금 과한 것 같아서요.\n나: 네, 위약금을 계약 금액의 10%에서 5%로 낮추는 걸 제안드리려고 했습니다.\n강하은: 좋습니다. 법무팀 확인 후 다음 주 화요일까지 수정본 보내드릴게요.',
 '{"contact_name": "강하은", "direction": "outgoing"}',
 now(), '2026-09-18 14:20:00+09', '계약서 위약금 조항 조정 통화',
 E'- 위약금 10%에서 5%로 하향 제안\n- 다음 주 화요일까지 수정본 전달'),
('call', 'demo-call-002', '통화: 윤서준',
 E'윤서준: 다음 달 신제품 출시 일정이 2주 밀릴 것 같아요. 부품 공급이 늦어지고 있어서요.\n나: 그러면 보도자료 배포도 같이 미뤄야겠네요. 새 일정 확정되면 바로 알려주세요.',
 '{"contact_name": "윤서준", "direction": "incoming"}',
 now(), '2026-09-20 10:05:00+09', '신제품 출시 2주 지연 가능성',
 E'- 부품 공급 지연으로 출시 2주 연기 예상\n- 보도자료 배포도 함께 조정'),
('calendar', 'demo-cal-001', '경영회의: 4분기 계획',
 E'4분기 매출 목표와 채용 계획을 확정하는 회의입니다. 각 팀장은 팀별 계획을 10분 안에 발표해 주세요.',
 '{"organizer": "ceo@example.com", "attendees": [{"email": "minjun.kim@example.com", "display_name": "김민준"}, {"email": "seoyeon.lee@example.com", "display_name": "이서연"}], "location": "본사 12층 대회의실"}',
 now(), '2026-09-22 10:00:00+09', '4분기 매출 목표와 채용 계획 확정 회의',
 E'- 팀장별 10분 발표\n- 4분기 매출 목표 확정\n- 채용 계획 확정'),
('calendar', 'demo-cal-002', '치과 정기 검진',
 E'스케일링과 정기 검진. 예약 변경은 하루 전까지 전화로.',
 '{"organizer": "me@example.com", "location": "강남역 스마일 치과"}',
 now(), '2026-09-26 15:30:00+09', NULL, NULL),
('note', 'demo-note-001', '독서 메모: 습관의 설계',
 E'작은 습관은 환경을 바꾸는 것에서 시작한다. 책상 위에 책을 펼쳐 두면 읽을 확률이 올라간다.\n\n반대로 스마트폰은 다른 방에 두기. 방해 요소를 줄이는 것이 의지력보다 강하다.\n\n이번 달 목표: 매일 20쪽 읽기, 주말에는 한 장씩 요약하기.',
 '{}', now(), '2026-09-14 23:10:00+09', '습관 형성: 환경 설계가 의지력보다 강하다',
 E'- 책을 눈에 띄는 곳에 두기\n- 스마트폰은 다른 방에\n- 매일 20쪽 읽기 목표'),
('note', 'demo-note-002', '주간 회고 9월 셋째 주',
 E'잘한 점: 예산안을 기한 안에 마무리했다. 계약서 위약금 조항을 조정했다.\n\n아쉬운 점: 운동을 두 번밖에 못 했다. 신제품 일정 변경을 늦게 공유했다.\n\n다음 주: 출시 일정 확정되면 바로 팀에 공유하기, 아침 운동 세 번.',
 '{}', now(), '2026-09-20 21:45:00+09', '9월 셋째 주 회고',
 E'- 예산안 기한 내 마무리\n- 운동 부족\n- 출시 일정 즉시 공유하기'),
('note', 'demo-note-003', '제주 워크숍 준비물',
 E'노트북 충전기, 발표 자료 USB, 우산, 편한 신발.\n\n둘째 날 오후는 올레길 7코스 걷기. 저녁은 흑돼지 식당 예약 필요.',
 '{}', now(), '2026-09-21 08:30:00+09', NULL, NULL),
('filesystem', 'demo-file-001', 'docs/onboarding/개발환경.md',
 E'# 개발 환경 설정\n\n1. Go 1.26 이상을 설치한다.\n2. docker compose 로 PostgreSQL 을 띄운다.\n3. .env.example 을 복사해 .env 를 만든다.\n\n## 자주 묻는 질문\n\n마이그레이션이 실패하면 pgvector 와 pg_bigm 확장이 설치된 이미지인지 먼저 확인한다.',
 '{}', now(), '2026-08-30 11:00:00+09', '신규 입사자 개발 환경 설정 가이드',
 E'- Go 1.26 이상\n- docker compose 로 PostgreSQL\n- 확장 설치 이미지 확인'),
('filesystem', 'demo-file-002', 'docs/policy/출장비.md',
 E'# 출장비 정산 규정\n\n숙박비는 1박 15만 원까지 실비로 정산한다. 항공권은 이코노미석만 인정한다.\n\n영수증은 출장 종료 후 7일 이내에 제출한다. 기한이 지나면 정산되지 않는다.',
 '{}', now(), '2026-07-01 09:00:00+09', '출장비 정산 규정',
 E'- 숙박 1박 15만 원 한도\n- 이코노미석만 인정\n- 7일 이내 영수증 제출')
ON CONFLICT (source_type, source_id) DO NOTHING;

-- Chunks: one row per paragraph, the shape the chunker produces for short
-- documents. byte_size mirrors what the collector records.
INSERT INTO chunks (document_id, chunk_index, content, byte_size)
SELECT d.id, p.ord - 1, p.part, octet_length(p.part)
FROM documents d
CROSS JOIN LATERAL regexp_split_to_table(d.content, E'\n\n') WITH ORDINALITY AS p(part, ord)
WHERE d.source_id LIKE 'demo-%'
  AND btrim(p.part) <> ''
ON CONFLICT (document_id, chunk_index) DO NOTHING;
