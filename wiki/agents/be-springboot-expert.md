---
title: be-springboot-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/be-springboot-expert.md
related:
  - "[[lang-java21-expert]]"
  - "[[lang-kotlin-expert]]"
  - "[[db-postgres-expert]]"
  - "[[infra-docker-expert]]"
  - "[[skills/springboot-best-practices]]"
---

# be-springboot-expert

Enterprise Spring Boot 3.5.x developer for Java 21 and Kotlin applications with virtual threads, GraalVM native images, and Micrometer observability.

## Overview

`be-springboot-expert` targets the modern Spring Boot ecosystem — Spring Boot 3.5.x running on Java 21. It leverages Java 21's virtual threads (JEP 444) for high-concurrency without reactive complexity, GraalVM native compilation for startup optimization, and annotation-driven enterprise patterns (`@Service`, `@Repository`, `@RestController`, `@ControllerAdvice`).

The agent applies `springboot-best-practices` skill and references `guides/springboot/` for framework specifics.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Tools**: Read, Write, Edit, Grep, Glob, Bash
- **Skill**: springboot-best-practices | **Guide**: `guides/springboot/`

### Key Capabilities

- Spring Boot 3.5.x application architecture and DI
- Virtual Threads (JEP 444) for scalable synchronous concurrency
- RESTful APIs, Spring Data JPA, transaction management
- Spring Security patterns (authentication, authorization)
- GraalVM native image configuration for optimized startup
- Micrometer + Actuator for observability

## Relationships

- **Java language**: [[lang-java21-expert]] for Java 21 features
- **Kotlin alternative**: [[lang-kotlin-expert]] for Kotlin + Spring
- **Database**: [[db-postgres-expert]] for JPA/Hibernate PostgreSQL integration
- **Deployment**: [[infra-docker-expert]] for Spring Boot container packaging

## Sources

- `.claude/agents/be-springboot-expert.md`
