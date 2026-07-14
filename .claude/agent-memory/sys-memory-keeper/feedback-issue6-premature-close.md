---
name: feedback-issue6-premature-close
description: GitHub 이슈 CLOSED != 코드 수정 완료. #6 hostPath 사례 — 이슈 닫혔어도 실제 코드/yaml 확인 필수
metadata:
  type: feedback
---

## 규칙

GitHub 이슈가 CLOSED여도 해당 코드/yaml이 실제로 수정되었는지 반드시 파일로 확인한다.

**Why:** 2026-06-08, Issue #6 "[P0] minikube hostPath 제거"가 CLOSED 상태였으나 `deploy/k8s/collector-deployment.yaml`에 `hostPath: /mnt/drive` 7곳이 그대로 남아 있었음. README도 macOS 경로 참조 유지. Linux 서버 deploy 블로커 사실상 미해결. 이슈 조기 종료(premature close) 사례.

**How to apply:** deploy 착수 전, CLOSED인 인프라/배포 관련 이슈는 반드시 대상 파일(k8s yaml, Dockerfile, docker-compose 등)을 직접 Read해 잔존 여부 확인. "이슈 닫힘 = 완료" 가정 금지.

## 관련 파일

- `deploy/k8s/collector-deployment.yaml` — hostPath /mnt/drive 7곳 (2026-06-08 기준 미수정)
- Issue #6: CLOSED (premature)
- ubuntu24_home_server-ext 배포 blocked
