# RAG Chunking Strategy

Reference guide for the adaptive chunking system introduced in issue #60.

---

## 1. The 5 Intrinsic Metrics (Chen et al. 2024)

The Adaptive Chunking paper defines five model-agnostic metrics for evaluating
chunk quality without a labeled question set. This project uses them as design
goals for the deterministic rules in `internal/chunker/adaptive.go`.

| Metric | Abbreviation | Definition | How we approximate it |
|--------|-------------|------------|-----------------------|
| Retrieval Coherence | RC | Each chunk answers at least one coherent sub-question | Smaller chunks for dense sources (chat, memory) |
| Block Integrity | BI | Chunks respect logical block boundaries (sections, paragraphs) | `HeadingAware=true` for structured sources |
| Inter-Chunk Coherence | ICC | Adjacent chunks stay topically linked | Overlap bytes bridge chunk boundaries |
| Document-Chunk Coherence | DCC | Every chunk stays faithful to the document's overall topic | Heading prefix prepended to every chunk in a section |
| Size Compliance | SC | Chunk byte-size stays within [TargetSize, MaxSize] | Per-source-type TargetSize/MaxSize constants |

---

## 2. Source-Type → Strategy Mapping

Implemented in `chunker.SelectOptions` (`internal/chunker/adaptive.go`).

| Source Type | HeadingAware | TargetSize | MaxSize | Overlap | Rationale |
|-------------|:------------:|:----------:|:-------:|:-------:|-----------|
| `filesystem` | true* | 2000 | 4000 | 100 | Markdown/text files frequently have headings (BI) |
| `notion` | true* | 2000 | 4000 | 100 | Notion pages use heading hierarchy (BI) |
| `github` | true* | 2000 | 4000 | 100 | READMEs, issues, PRs contain Markdown headings |
| `gdrive` | true* | 2000 | 4000 | 100 | Docs use h1/h2 structure |
| `slack` | false | 900 | 1500 | 80 | Short turns; no headings; fine-grained RC matters |
| `discord` | false | 900 | 1500 | 80 | Same as Slack |
| `telegram` | false | 900 | 1500 | 80 | Same as Slack |
| `secretary` | false | 1200 | 2500 | 100 | Dense prose logs; no headings expected |
| `llm-memory` | false | 1200 | 2500 | 100 | Behavioral memory entries; prose without structure |
| *(unknown)* | true | 2000 | 4000 | 100 | Conservative fallback; preserves pre-#60 behaviour |

\* **Content-shape override**: if the content contains no heading markers and no
`\n\n` paragraph breaks, `HeadingAware` is downgraded to `false` even for
long-form source types (BI heuristic — no structure to detect).

---

## 3. Future Extension: LLM-Scored Selection

The current rules are fast and deterministic but cannot adapt to atypical
documents (e.g., a Slack channel used as a knowledge base, or a filesystem
file that is actually a chat log).

The paper's **LLM Regex Splitter** and **Split-then-Merge** approaches provide
a scored upgrade path:

```
// Future: replace or supplement contentIsStructured() with an LLM probe.
//
// Step 1 — Sample: extract the first 512 tokens of the document.
// Step 2 — Score: ask the LLM to rate BI and RC on a 1–5 scale.
// Step 3 — Select:
//   BI >= 4 → HeadingAware=true
//   RC < 3  → reduce TargetSize by 30%
//   SC fail → increase MaxSize or reduce TargetSize to bring chunks in range.
```

Extension point in code: `chunker.contentIsStructured()` and the `default`
branch of `chunker.SelectOptions()` are the two places to plug in LLM scoring.

The Adaptive Chunking paper reference:
> Chen, Y. et al. (2024). "Evaluating Chunking Strategies for Retrieval."
> Metrics: RC, BI, ICC, DCC, SC.

---

## 4. Key Files

| File | Role |
|------|------|
| `internal/chunker/chunker.go` | Core split logic: `Split()`, `Options` |
| `internal/chunker/adaptive.go` | `SelectOptions()` — per-source strategy selector |
| `internal/chunker/adaptive_test.go` | Table-driven tests + regression guard |
| `internal/scheduler/scheduler.go` | `persistChunks()` — wires `SelectOptions` into ingestion |
| `internal/model/document.go` | `SourceType` constants |
