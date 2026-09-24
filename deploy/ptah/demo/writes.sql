-- What the application does while Ptah builds the generations: a title
-- changes, a mail arrives with its chunks, a summary lands, a document is
-- deleted. run.sh applies this after the backfill snapshot, and catch-up has
-- to account for every one of them.
UPDATE documents SET title = '서버 이전 작업 일정 변경: 일요일 새벽', content = replace(content, '토요일', '일요일'), updated_at = now() WHERE source_id = 'demo-mail-004';
INSERT INTO documents (source_type, source_id, title, content, metadata, collected_at, occurred_at)
VALUES ('gmail', 'demo-mail-005', '신입 개발자 면접 일정 안내',
        E'다음 주 수요일 오후 2시에 신입 개발자 최종 면접이 있습니다.\n\n면접관은 정우진, 이서연 두 분입니다. 이력서는 공유 폴더에 올려 두었습니다.',
        '{"from": "\"한지민\" <jimin.han@example.com>", "to": "\"정우진\" <woojin.jung@example.com>"}',
        now(), '2026-09-23 09:00:00+09')
ON CONFLICT (source_type, source_id) DO NOTHING;
INSERT INTO chunks (document_id, chunk_index, content, byte_size)
SELECT d.id, p.ord - 1, p.part, octet_length(p.part)
FROM documents d CROSS JOIN LATERAL regexp_split_to_table(d.content, E'\n\n') WITH ORDINALITY AS p(part, ord)
WHERE d.source_id = 'demo-mail-005'
ON CONFLICT (document_id, chunk_index) DO NOTHING;
UPDATE documents SET title_summary = '제주 워크숍 준비물과 둘째 날 일정', bullet_summary = E'- 충전기, USB, 우산, 편한 신발\n- 둘째 날 올레길 7코스\n- 흑돼지 식당 예약' WHERE source_id = 'demo-note-003';
DELETE FROM documents WHERE source_id = 'demo-sms-002';
