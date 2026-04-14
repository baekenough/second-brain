---
name: feedback-docker-minikube
description: 모든 서비스는 Docker+minikube로 배포해야 함 — 네이티브 바이너리 실행 금지 (2026-04-12 명시)
type: feedback
---

모든 서비스는 Docker 이미지로 빌드하고 minikube(Kubernetes)로 배포해야 한다. 네이티브 바이너리 직접 실행은 일회성 디버깅 목적에만 허용된다.

**Why:** 사용자가 2026-04-12 세션에서 명시적으로 지정. 로컬 바이너리(`go run`, `/tmp/second-brain-server` 등)는 환경 재현성이 없고 프로덕션과 괴리가 생긴다. minikube가 정식 배포 경로의 정답지(canonical target)임.

**How to apply:**
- 새 서비스 구현 시 반드시 `Dockerfile` 작성 포함
- 로컬 개발 환경: `docker-compose.yml` 사용
- 스테이징/프로덕션 배포: minikube + Kubernetes 매니페스트 (`k8s/` 디렉토리)
- 이미지 태그 규칙: `second-brain:dev` (로컬 빌드), `second-brain:v{major}.{minor}.{patch}` (릴리즈)
- 포트 노출은 Kubernetes Service/Ingress로 처리 (포트 :9200 유지)
- "서버 실행" 요청 시 `nohup` 또는 바이너리 직접 실행 제안 금지 — docker/kubectl 명령어로 응답
- 일회성 디버깅(`go build && ./server`)은 허용하되, 그 상태를 "배포"로 표현 금지
