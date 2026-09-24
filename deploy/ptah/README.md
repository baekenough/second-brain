# Ptah로 임베딩 모델 교체하기

[English](README.en.md)

이 디렉터리는 second-brain의 벡터를 새 모델로 옮기는 절차다(여기서는
OpenAI `text-embedding-3-small` 1536차원 → 로컬 `bge-m3` 1024차원). 테이블을
비우지 않고, 한 인덱스에 두 모델의 벡터를 섞지도 않는다.
[Ptah](https://ptah.run)가 새 벡터를 기존 벡터 옆에 따로 만들고, 앱은 그
벡터가 완성되고 검증된 뒤에만 넘어간다.

## 왜 필요한가

지금 모델을 바꾸는 방법은 세 가지이고, 각각 빈틈이 있다.

- `EMBEDDING_REEMBED_ENABLED=true`는 제자리에서 다시 임베딩한다. 끝날
  때까지 HNSW 인덱스 하나에 두 모델의 벡터가 섞인다. 마이그레이션 037이
  드러내려던 바로 그 상태다.
- 마이그레이션 011·015는 테이블이 비어 있을 때만 벡터 차원을 바꾼다.
  데이터가 있으면 건너뛰고, 안내하는 `docs/embedding-dimension.md`는
  존재하지 않는다.
- `TODO.md`의 전체 재임베딩은 `collected_at`을 되돌리고 스케줄러를 기다린다.
  언제 끝났는지 알려주는 것이 없다.

## 무엇이 바뀌나

Ptah는 벡터 계열마다 세대(generation) 하나를, Ptah가 만든 별도 테이블에
둔다. 기존 테이블에는 컬럼이 추가되지 않는다. 대신 원본 테이블 두 곳에
트리거와 outbox 테이블이 붙어서, 세대를 만드는 동안 들어온 쓰기를 모두
따라잡는다(catch-up).

| 계열 | 원본 | 임베딩하는 텍스트 (Go 코드와 같은 레시피) | Ptah 테이블 |
| --- | --- | --- | --- |
| 문서 (`vec` 레인) | `documents` | `title + "\n\n" + content` (doc-v1) | `document_vectors` |
| 요약 (`summvec` 레인) | `documents` | `title_summary + "\n\n" + bullet_summary` | `document_summary_vectors` |
| 청크 (청크 검색, /ask) | `chunk_sparse_context` | `context_version = 'v1-full'`인 `sparse_text` | `chunk_context_vectors` |

청크 행이 핵심이다. chunk-ctx-v1 임베딩 입력은 상위 문서의 날짜·제목·
참여자로 Go에서 만드는데, `cmd/sparsectx --recipe=v1-full`이 정확히 같은
문자열을 이미 `chunk_sparse_context.sparse_text`에 저장하고 있다. 그래서
Ptah는 그 테이블을 읽고, 헤더 로직을 다시 구현하지 않는다. 두 구성이 같다는
것은 `TestChunkEmbeddingInputIsTheV1FullSparseText`가 지킨다.

앱 쪽에서 `VECTOR_SOURCE=ptah`는 다음을 한다.

- 검색은 활성 세대를 Ptah의 포인터(`ptah_embedding_pointer`)에서 읽고,
  30초마다 다시 확인한다. 그래서 컷오버나 롤백이 배포 없이 검색에 반영된다.
- 세대가 없거나 차원이 `EMBEDDING_DIM`과 다른 레인은 pgvector 안에서
  실패하는 대신 꺼진다.
- 청크 레인은 희소(sparse) 레인과 같은 fingerprint 규칙을 적용한다. 제목
  변경이나 마스킹 전에 쓰인 문맥 행은 `sparsectx --sweep`이 다시 쓸 때까지
  쓰이지 않는다.
- 쓰기 경로(스케줄러, 요약기, ingest, 노트)는 임베딩을 멈춘다. 세대는
  `ptah inference catchup`이 최신으로 유지한다.

기본값인 `VECTOR_SOURCE=app`에서는 조회·쓰기 경로가 모두 지금 그대로다.

## 데모 실행

```bash
deploy/ptah/demo/run.sh
```

Docker(compose 포함), Go, curl, python3가 필요하고, 띄운 것은 끝나면 모두
지운다. 이 저장소의 PostgreSQL 이미지를 빌드하고 `migrations/`를 적용한 뒤,
작은 한국어 말뭉치(등장인물은 모두 가상)를 넣고 `cmd/sparsectx`를 돌린다.
그다음:

1. 배포된 `ghcr.io/stokaro/ptah:0.8.0`으로 세 세대를 만든다.
2. 만드는 동안 쓰기가 들어온다 — 제목 변경, 청크가 딸린 새 메일, 요약 추가,
   삭제 — 그리고 `sparsectx --sweep`을 돌린다.
3. catch-up, HNSW 인덱스 생성, 검증.
4. 컷오버. 승인은 보여준 계획(plan)의 digest에 묶인다.
5. `VECTOR_SOURCE=ptah`로 서버를 띄우고 `/api/v1/search`에 한국어로 묻는다.

## 운영 환경 전환

```bash
DB=postgres://...    # 운영 DATABASE_URL. 셸 기록에 남기지 않는다.

for f in documents summaries chunks; do
  ptah inference prepare  --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  ptah inference backfill --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
done
```

세 파일의 `model.endpoint`를 Ollama 주소로 바꾼다. endpoint는 세대 식별자에
들어가지 않으므로 같은 파일을 어느 환경에서나 쓴다. `backfill`은 멈추면
체크포인트부터 이어간다.

그동안 앱은 자기 컬럼으로 그대로 돌아간다. 준비가 되면:

```bash
go run ./cmd/sparsectx --recipe=v1-full --dry-run=false --sweep
for f in documents summaries chunks; do
  ptah inference catchup --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  ptah inference index   --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  ptah inference verify  --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  ptah inference cutover --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  # 계획 digest가 출력된다. 바로 그 계획을 승인한다:
  ptah inference cutover --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3 \
    --approve <digest> --approver "<이름>"
done
```

그다음 `VECTOR_SOURCE=ptah`, `EMBEDDING_PROVIDER=local`,
`LOCAL_EMBEDDING_MODEL=bge-m3`, `EMBEDDING_DIM=1024`로 배포한다. 이후에는
작업 두 개가 벡터를 최신으로 유지한다. 예를 들어 collector 옆의 CronJob으로
`sparsectx --recipe=v1-full --dry-run=false --sweep`과 세 계열의
`ptah inference catchup`을 돌린다. 새 청크는 두 작업이 모두 돈 뒤에 벡터를
갖는다.

이전 모델로 돌아가려면 `VECTOR_SOURCE=app`과 이전 임베딩 설정으로 되돌린다.
앱의 컬럼은 건드리지 않았고, 그사이 들어온 문서와 청크는 스케줄러의 백필이
임베딩한다. 그사이 쓰인 요약에는 요약 벡터가 생기지 않는다. 요약기는 요약을
쓸 때만 임베딩하기 때문이다. 그다음 모델 교체(bge-m3 → 다른 모델)는 새
컬럼을 쓰는 새 명세이고, 그때는 `cutover --stabilize-for`로 준 기간 안에서
`ptah inference rollback`을 쓸 수 있다.

## 한계

- Ollama는 `OLLAMA_CONTEXT_LENGTH`를 따로 주지 않으면 2048토큰 문맥으로
  동작하고, 한국어는 bge-m3 토큰당 약 4.8바이트로 측정됐다. 그래서 명세는
  입력을 8000바이트로 자른다. 둘은 함께 올린다.
- Ptah는 pgvector가 있는 PostgreSQL만 다룬다. second-brain이 쓰는 그것이다.
- 명세는 `schema: public`을 명시한다. Ptah 0.8.0에서는 이것이 없으면 같은
  원본 테이블 위의 두 번째 세대가 `prepare`에서 거부된다.
