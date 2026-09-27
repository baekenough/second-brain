#!/usr/bin/env bash
# PostgreSQL chunks → OpenSearch(sb-chunks) 색인
#
# 기존 검색 경로에는 영향이 없다. 같은 청크를 nori 형태소 분석으로 색인해
# pg_bigm 방식과 결과를 나란히 비교하기 위한 실험 인덱스다.
set -uo pipefail
OUT=/tmp/sb_bulk.ndjson
echo "[1/3] NDJSON 생성"
docker exec second-brain-postgres psql -U brain -d second_brain -t -A -o /tmp/sb_bulk_raw.ndjson -c "
SELECT json_build_object('index', json_build_object('_index','sb-chunks','_id',c.id))::text
       || chr(10) ||
       json_build_object(
         'chunk_id',    c.id,
         'document_id', d.id,
         'source_type', d.source_type,
         'kind',        d.metadata->>'kind',
         'chunk_index', c.chunk_index,
         'occurred_at', d.occurred_at,
         'title',       d.title,
         'content',     c.content
       )::text
FROM chunks c JOIN documents d ON d.id = c.document_id
ORDER BY c.id;
"
docker cp second-brain-postgres:/tmp/sb_bulk_raw.ndjson "$OUT"
docker exec second-brain-postgres rm -f /tmp/sb_bulk_raw.ndjson
LINES=$(wc -l < "$OUT")
echo "  생성: $LINES 줄 ($(du -h "$OUT" | cut -f1))"

echo "[2/3] 분할 (배치당 10000 줄 = 5000 문서)"
rm -f /tmp/sb_bulk_part_*
split -l 10000 "$OUT" /tmp/sb_bulk_part_
PARTS=$(ls /tmp/sb_bulk_part_* | wc -l)
echo "  $PARTS 개 배치"

echo "[3/3] bulk 색인"
i=0
for f in /tmp/sb_bulk_part_*; do
  i=$((i+1))
  # bulk API 는 마지막 줄이 개행으로 끝나야 한다
  printf '\n' >> "$f"
  R=$(curl -s -X POST "http://localhost:9200/_bulk" \
        -H "Content-Type: application/x-ndjson" --data-binary "@$f")
  ERR=$(echo "$R" | python3 -c "import sys,json;d=json.load(sys.stdin);print(1 if d.get('errors') else 0)" 2>/dev/null || echo "?")
  echo "  [$i/$PARTS] errors=$ERR"
  if [ "$ERR" = "1" ]; then
    echo "$R" | python3 -c "
import sys,json
d=json.load(sys.stdin)
for it in d.get('items',[])[:3]:
    v=list(it.values())[0]
    if v.get('error'): print('    err:', str(v['error'])[:160])
" 2>/dev/null
  fi
done
rm -f /tmp/sb_bulk_part_* "$OUT"

echo "[검증]"
curl -s -X POST "http://localhost:9200/sb-chunks/_refresh" >/dev/null
curl -s "http://localhost:9200/sb-chunks/_count" | head -c 200; echo
curl -s "http://localhost:9200/_cat/indices/sb-chunks?v&h=index,docs.count,store.size"
