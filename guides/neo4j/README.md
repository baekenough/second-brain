# Neo4j Guide

Reference documentation for Neo4j graph database usage in this project: Cypher authoring, graph data modeling, indexing, Go-driver integration, and safe execution of LLM-generated Cypher.

## Source

Based on [Neo4j official documentation](https://neo4j.com/docs/) (Cypher Manual, Go Manual, Operations Manual) and the [neo4j-go-driver](https://pkg.go.dev/github.com/neo4j/neo4j-go-driver/v5/neo4j) package documentation.

## Categories

| Priority | Category | Impact |
|----------|----------|--------|
| 1 | Safe Execution of LLM-Generated Cypher | CRITICAL |
| 2 | Query Optimization (PROFILE/EXPLAIN) | CRITICAL |
| 3 | Data Modeling (labels, relationships) | CRITICAL |
| 4 | Indexes and Constraints | CRITICAL |
| 5 | Go Driver (sessions, transactions) | CRITICAL |
| 6 | PostgreSQL-as-Source-of-Truth Sync | HIGH |
| 7 | Deployment (docker-compose, ubuntu1) | MEDIUM |

## Project Context

This project introduces Neo4j as a **knowledge-graph layer derived from PostgreSQL**, not as an independent system of record:

- PostgreSQL remains the source of truth; Neo4j is a rebuildable projection
- Backend is Go — all driver-level guidance targets `neo4j-go-driver` v5
- Deployment is docker-compose (not Kubernetes); Neo4j is planned for host ubuntu1
- The project plans a natural-language-to-Cypher feature via remote LLM APIs only (no local inference/ollama) — see the Safe Execution section in `neo4j-best-practices` for the mandatory read-only + parameterized + `EXPLAIN`-gated + destructive-clause-blocked execution pipeline

## Cypher Safety Quick Reference

| Control | Mechanism |
|---------|-----------|
| Read-only enforcement | `reader`-role session / `neo4j.AccessModeRead` |
| Injection prevention | Mandatory `$param` parameterization, never string interpolation |
| Pre-execution validation | `EXPLAIN <query>` before real execution |
| Destructive-clause blocking | Static reject-list: `DETACH DELETE`, `DELETE`, `REMOVE`, `SET`, `CREATE`, `MERGE`, `LOAD CSV`, `apoc.*` write procedures |
| Runaway query protection | Bounded variable-length paths, default `LIMIT`, server-side query timeout |

## Relationship to Other DB Agents

| Agent | Scope | When to Use |
|-------|-------|------------|
| db-postgres-expert | Pure PostgreSQL | Source-of-truth schema, sync query design |
| db-neo4j-expert | Neo4j graph layer | Cypher, graph modeling, Go-driver integration, LLM-Cypher safety |

## Usage

This guide is referenced by:
- **Agent**: db-neo4j-expert
- **Skill**: neo4j-best-practices

## External Resources

- [Cypher Manual](https://neo4j.com/docs/cypher-manual/current/)
- [Cypher Query Tuning](https://neo4j.com/docs/cypher-manual/current/planning-and-tuning/)
- [Neo4j Go Driver Manual](https://neo4j.com/docs/go-manual/current/)
- [neo4j-go-driver Go package docs](https://pkg.go.dev/github.com/neo4j/neo4j-go-driver/v5/neo4j)
- [Index Configuration](https://neo4j.com/docs/operations-manual/current/performance/index-configuration/)
- [Constraints](https://neo4j.com/docs/cypher-manual/current/constraints/)
- [Graph Data Modeling Guide](https://neo4j.com/docs/getting-started/data-modeling/guide-data-modeling/)
- [Access Control / Roles](https://neo4j.com/docs/operations-manual/current/authentication-authorization/access-control/)
