---
title: ubuntu1-deploy
type: skill
updated: 2026-09-20
sources:
  - .claude/skills/ubuntu1-deploy/SKILL.md
related:
  - "[[infra-docker-expert]]"
  - "[[mgr-gitnerd]]"
  - "[[skills/docker-best-practices]]"
  - "[[skills/action-validator]]"
  - "[[r001]]"
  - "[[r004]]"
---

# ubuntu1-deploy

Deploy or redeploy the second-brain stack to the `ubuntu1` host: build images on the server, roll out via Compose, and verify the running image actually changed.

## Overview

`ubuntu1-deploy` enforces a checklist-driven deployment procedure for the second-brain server/collector/mcp/web images. It exists because `ubuntu1:~/second-brain-app` is an rsync copy of the local repo, not a git clone — committing locally does not update the server, and Compose has no `build:` section, so images must be built directly on `ubuntu1`. The skill codifies six ordered steps (Go source sync, web source sync, image build, optional pre-migration backup, rollout, verification) plus a pre-flight checklist and rollback procedure, so that a deploy is never declared done on the strength of a passing health check alone.

## Key Details

- **Scope**: package | **User-invocable**: true
- Trigger phrases: "ubuntu1에 배포해줘", "재빌드해줘", "롤아웃해줘"
- Migrations auto-run on server boot with no tracking table — SQL must be idempotent

## Workflow (6 steps)

1. Sync Go source (`rsync -az`, excludes `.git`)
2. Sync web source (separate command, requires `-R` for relative paths)
3. Build images on `ubuntu1` (`docker build --target <svc>` per service — Compose has no `build:` section)
4. Pre-deploy backup (only when the deploy includes a data-mutating migration; `pg_dump` of affected tables)
5. Roll out (`docker compose --env-file .env.local -f docker-compose.ubuntu1.yml up -d`)
6. Verify — image ID diff between running and built images takes priority over any HTTP/health check, since a health check passes even when `up -d` silently restarted the old image

## Incidents It Guards Against

- **Stale-image false "done"**: building with a tag Compose doesn't reference (e.g. `second-brain-ubuntu1-server:latest`) lets `up -d` restart the old container while health checks and endpoints still return 200 — reported as deployed when two fixes were actually missing. Guard: image-ID diff is verification step 1, before any endpoint check.
- **rsync path flattening**: syncing Go and web files in one non-`-R` command flattens `web/Dockerfile` onto the repo-root Dockerfile and scatters `web/src` into root `src/`, breaking multi-stage builds (`target stage "server" could not be found`). Guard: Go/web rsyncs are separate commands; web always uses `-R`; a post-sync `grep -c '^FROM .* AS (server|collector|mcp)' Dockerfile == 3` check catches the overwrite immediately.

## Relationships

- **Infra operations**: [[infra-docker-expert]] for Docker/Compose expertise this skill's steps rely on
- **Git delegation**: [[mgr-gitnerd]] — repo commits stay separate from the rsync-based deploy path
- **Related practices**: [[skills/docker-best-practices]] for multi-stage build patterns, [[skills/action-validator]] for pre-action validation gate philosophy
- **Safety rules**: [[r001]] (destructive-operation approval), [[r004]] (error classification/rollback discipline)

## Sources

- `.claude/skills/ubuntu1-deploy/SKILL.md`
- [`deploy/ubuntu1-stack/README.md`](../../deploy/ubuntu1-stack/README.md)
- [`Dockerfile`](../../Dockerfile)
