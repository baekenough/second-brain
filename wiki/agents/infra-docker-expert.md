---
title: infra-docker-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/infra-docker-expert.md
related:
  - "[[infra-aws-expert]]"
  - "[[be-go-backend-expert]]"
  - "[[be-fastapi-expert]]"
  - "[[skills/docker-best-practices]]"
  - "[[r010]]"
---

# infra-docker-expert

Docker engineer for optimized Dockerfiles, multi-stage builds, container security hardening, and Docker Compose configurations.

## Overview

`infra-docker-expert` handles all container-related work: Dockerfile authoring, multi-stage build optimization, security hardening, and Docker Compose orchestration. Per R010, it also handles server state changes (restart, environment) and deployment operations — making it the agent for any server-touching infrastructure task.

Memory is user-scoped because Docker patterns (multi-stage builds, non-root user, layer caching) apply broadly.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: user
- **Domain**: devops | **Skill**: docker-best-practices | **Guide**: `guides/docker/`

### Capabilities

1. Optimized Dockerfile design with layer caching strategy
2. Multi-stage builds for minimal production image size
3. Security: non-root users, read-only filesystems, minimal base images
4. Image size optimization and unused layer elimination
5. Docker Compose for local development and multi-service stacks
6. Container orchestration preparation

## Relationships

- **Cloud deployment**: [[infra-aws-expert]] for ECS/EKS container deployment
- **Go services**: [[be-go-backend-expert]] containers to package
- **Python services**: [[be-fastapi-expert]] containers to package
- **Delegation rule**: [[r010]] designates this agent for server deployment and state changes

## Sources

- `.claude/agents/infra-docker-expert.md`
