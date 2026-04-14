---
title: springboot-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/springboot-best-practices/SKILL.md
related:
  - "[[be-springboot-expert]]"
  - "[[lang-kotlin-expert]]"
  - "[[lang-java21-expert]]"
  - "[[guides/springboot]]"
  - "[[skills/java21-best-practices]]"
---

# springboot-best-practices

Spring Boot patterns for enterprise Java: layered architecture, constructor injection, REST API design, exception handling, and test conventions.

## Overview

`springboot-best-practices` enforces the Spring Boot layered architecture (controller → service → repository) and Spring idioms. Constructor injection with `@RequiredArgsConstructor` + `final` fields is mandatory — field injection with `@Autowired` is prohibited (prevents testability). `ResponseEntity<T>` with explicit HTTP status codes on every endpoint.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [be-springboot-expert](../agents/be-springboot-expert.md)

## Core Rules

- **Architecture**: controller (REST) → service (business logic) → repository (data access)
- **Injection**: constructor injection always; `@Autowired` on fields = violation
- **REST**: `@RestController`, `@Validated` for input, `ResponseEntity<T>` for responses
- **Exceptions**: `@ControllerAdvice` for global handling; custom exception hierarchy
- **Testing**: `@SpringBootTest` for integration, `@WebMvcTest` for controller, `@DataJpaTest` for repository
- **Configuration**: `@ConfigurationProperties` for typed config; never `@Value` for complex configs

## Never

- Field injection (`@Autowired` on fields)
- Business logic in controller layer
- `@Transactional` on controller methods

## Relationships

- **Language**: [[skills/java21-best-practices]] for Java 21 language features
- **Kotlin**: [[lang-kotlin-expert]] for Kotlin + Spring Boot patterns
- **Guide**: [[guides/springboot]] for extended reference

## Sources

- `.claude/skills/springboot-best-practices/SKILL.md`
