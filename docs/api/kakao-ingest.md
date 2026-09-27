# 카카오톡 수집 API

`POST /api/v1/ingest/kakao`는 기존 모바일 API와 같은 Bearer API 키로 인증합니다. 모바일은 Go backend 전용 터널에 직접 연결합니다. 본문은 `{"messages":[...]}`이며 한 요청은 최대 300건, JSON 전체는 최대 24 MiB입니다.

| 필드 | 형식과 제한 |
| --- | --- |
| `message_id` | 메시지의 안정적인 ID. 영문·숫자로 시작하며 영문·숫자·`:_-`만 허용, 최대 128바이트 |
| `room_id`, `sender_id` | 동일한 ID 규칙. 방·화자를 구분하며 표시명이 같다는 이유로 병합하지 않음 |
| `room_name`, `sender_name` | 표시명, 각각 UTF-8 최대 1,024바이트 |
| `room_type` | `direct`, `group`, `open`, `unknown` |
| `is_self` | `true`, `false`, `null`. 알 수 없으면 `null` |
| `friend_status` | `friend`, `not_friend`, `unknown` |
| `friend_evidence` | `user_confirmed`, `unknown` |
| `identity_confidence` | `confirmed`, `provisional` |
| `body` | 비어 있지 않은 본문, UTF-8 최대 64 KiB, NUL 불가 |
| `date_ms` | Unix 밀리초. 2000-01-01 이후, 서버 시각에서 최대 7일 앞까지 |
| `capture_source` | `notification`, `text_import` |

친구 여부가 `friend` 또는 `not_friend`이면 `friend_evidence=user_confirmed`가 필요합니다. 친구 여부가 `unknown`이면 근거도 `unknown`이어야 합니다. 서버는 방 유형·이름·본문에서 친구 관계나 본인 여부를 추론하지 않습니다. 알림으로 관찰한 신원은 `provisional`로 저장하고 사용자 확인 뒤 같은 `message_id`로 메타데이터를 갱신할 수 있습니다.

메시지는 `source_type=kakao`, `source_id=kakao:<message_id>` 문서 하나로 저장합니다. 제목에 방과 화자를 표시하고 본문 앞에 방·화자·발생 시각을 붙입니다. 모든 입력 분류 필드는 메타데이터에 보존합니다. 방·화자 ID를 별도로 남겨 같은 표시명의 다른 방이나 사람을 혼동하지 않습니다. 과거 대화 가져오기는 SMS 수집 시작일과 무관하며 날짜 하한만 적용합니다.

문서와 청크를 하나의 트랜잭션으로 저장합니다. 같은 ID를 다시 보내면 중복 문서를 만들지 않으며 본문이 바뀌면 청크를 교체합니다. 친구·본인 확인처럼 메타데이터만 바뀌면 청크는 그대로 두고 메타데이터를 갱신합니다. 임베딩은 요청 안에서 만들지 않고 기존 백필이 처리합니다. 사용자·정책 삭제 문서는 재전송으로 복원하지 않습니다.

성공 응답은 HTTP 201이며 형식은 다음과 같습니다.

```json
{"accepted":2,"skipped":1,"rejected_ids":["invalid-record-id"],"errors":["message[3]: invalid_room_type"]}
```

- `accepted`: 저장 또는 멱등 갱신에 성공한 원소 수.
- `skipped`: 이미 삭제된 문서여서 저장하지 않은 원소 수.
- `rejected_ids`: 재시도해도 바뀌지 않는 입력 오류로 거부한 메시지 ID.
- `errors`: 원소 인덱스와 고정 사유 코드. 이름·본문·DB 오류 원문을 포함하지 않음.

201에서는 `accepted + skipped + rejected_ids.length`가 요청 원소 수와 같습니다. `rejected_ids`에는 같은 ID를 두 번 넣지 않습니다. ID가 없거나 규칙에 맞지 않거나 배치 안에서 중복되면 저장 전에 전체 요청을 400으로 거절합니다. ID가 정상인 원소의 필드 타입·길이·열거값 오류는 해당 ID만 거부합니다. 요청 크기·건수 초과는 413입니다.

DB 연결 오류·처리 시간 초과·동시 수집 게이트 대기는 `503`과 `Retry-After: 30`으로 응답합니다. 이 경우 부분 성공을 ACK하지 않고 배치 전체를 다시 보냅니다. 앞서 저장된 원소는 재전송 때 멱등 처리되므로 청크 오류 후 데이터가 빠지지 않습니다.

`GET /api/v1/documents/recent?kind=kakao`는 활성 카카오톡 문서의 최근 목록과 실제 전체 건수 `total`을 돌려줍니다. 웹 검색 필터와 출처 배지도 `kakao`를 지원합니다.

기존 `PII_NAME_REDACTION_ENABLED=true` 설정에서는 명시된 방·화자 표시 필드를 가립니다. 자유 본문은 두 글자 이상 화자 이름에 기존 known-contact redactor를 적용하고 구조화된 전화번호 등도 기존 규칙으로 가립니다. 방 이름이나 한 글자 표시명으로 본문 전체를 치환하지 않습니다. ID는 바꾸지 않습니다. `pii_name_redacted`는 처리 여부이며 완전한 익명화를 보장하지 않습니다. 기본값인 false에서는 원문을 보존합니다.
