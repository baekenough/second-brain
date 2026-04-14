---
title: "Guide: Spring Boot"
type: guide
updated: 2026-04-12
sources:
  - guides/springboot/best-practices.md
related:
  - "[[be-springboot-expert]]"
  - "[[lang-java21-expert]]"
  - "[[lang-kotlin-expert]]"
  - "[[skills/springboot-best-practices]]"
---

# Guide: Spring Boot

Reference documentation for Spring Boot 3.5.x — dependency injection, REST APIs, Spring Data, Security, virtual threads, and GraalVM native images.

## Overview

The Spring Boot guide provides reference documentation for `be-springboot-expert` and the `springboot-best-practices` skill. It covers Spring Boot 3.5.x running on Java 21, with particular focus on modern patterns: virtual threads for high-concurrency, Micrometer observability, and GraalVM native compilation.

## Key Topics

- **Application Architecture**: `@SpringBootApplication`, component scanning, bean lifecycle
- **Dependency Injection**: `@Service`, `@Repository`, `@Controller`, `@Configuration`, constructor injection
- **REST APIs**: `@RestController`, `@RequestMapping`, `@ControllerAdvice` for global error handling
- **Spring Data JPA**: Repository pattern, JPQL, native queries, N+1 prevention with `JOIN FETCH`
- **Spring Security**: Authentication/authorization, JWT integration, method security
- **Virtual Threads**: `spring.threads.virtual.enabled=true`, carrier thread pinning scenarios
- **GraalVM Native**: AOT compilation, reflection hints, native image build configuration
- **Micrometer**: Metrics, tracing, Actuator endpoint configuration

## Relationships

- **Agent**: [[be-springboot-expert]] primary consumer
- **Java 21**: [[lang-java21-expert]] for language-layer patterns
- **Skill**: [[skills/springboot-best-practices]] implements patterns

## Sources

- `guides/springboot/best-practices.md`
