---
name: migration-sql-type-check
description: SQL 마이그레이션 FK column 타입은 참조 테이블 PK 타입과 반드시 일치 확인
type: feedback
---

FK column 타입은 참조 테이블 PK 타입과 반드시 사전에 확인하고 일치시킬 것.

**Why:** 2026-04-14 second-brain 세션에서 `005_feedback.sql`이 `document_id BIGINT REFERENCES documents(id)`로 선언됐는데 `documents.id`는 UUID. v0.1.11/v0.1.12 태그 CI는 green이었지만 서버 배포 시 SQLSTATE 42804 FK 타입 불일치 에러로 CrashLoopBackOff 발생. 기존 구버전 pod가 살아있어 서비스는 유지됐으나 신규 기능 전면 차단. v0.1.13 hotfix로 `document_id UUID`로 수정.

**How to apply:**
1. 새 마이그레이션 SQL 추가 시 참조 테이블의 PK 타입을 먼저 확인 (UUID vs BIGINT vs INT)
2. FK column 타입을 정확히 일치시킴
3. Go store 구조체 필드 타입도 맞춤 (`*string` for UUID, `*int64` for BIGINT)
4. CI에 pgvector/pg16 통합 테스트 job 추가 권장 (second-brain #42 tracked)
