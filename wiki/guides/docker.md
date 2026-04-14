---
title: "Guide: Docker"
type: guide
updated: 2026-04-12
sources:
  - guides/docker/compose-best-practices.md
related:
  - "[[infra-docker-expert]]"
  - "[[infra-aws-expert]]"
  - "[[skills/docker-best-practices]]"
---

# Guide: Docker

Reference documentation for Docker containerization — Dockerfile optimization, multi-stage builds, security hardening, and Docker Compose patterns.

## Overview

The Docker guide provides reference documentation for `infra-docker-expert` and the `docker-best-practices` skill. It covers official Docker documentation patterns for production container images and compose-based orchestration.

## Key Topics

- **Dockerfile Optimization**: Layer caching strategy, instruction ordering, `.dockerignore`
- **Multi-stage Builds**: Builder pattern for minimal production images, stage naming
- **Base Images**: Alpine vs distroless vs ubuntu — trade-offs for size vs compatibility
- **Security Hardening**: Non-root user, read-only filesystem, capability dropping, image scanning
- **Docker Compose**: Service dependencies, health checks, network segmentation, volume management
- **Image Size**: Layer squashing, unused package removal, COPY specificity

## Relationships

- **Agent**: [[infra-docker-expert]] primary consumer
- **Skill**: [[skills/docker-best-practices]] implements patterns
- **Cloud deployment**: [[infra-aws-expert]] for ECS/EKS deployment of built images

## Sources

- `guides/docker/compose-best-practices.md`
