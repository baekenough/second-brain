---
name: no-changelog-in-readme
description: README/ARCHITECTURE 등 핵심 문서에 버전별 변경 사항 섹션 금지
type: feedback
---

Do not include version history sections (e.g., "What's New in vX.Y.Z", "Recent Changes", "Changelog", "vX.Y Highlights") in core project documentation files.

**Why:** User-explicit requirement. CHANGELOG and release notes are separate artifacts. Mixing them into README creates per-release manual maintenance burden and stale section risk. Core docs should describe current state only.

**How to apply:**
- Prohibited sections in `README.md`, `README.en.md`, `ARCHITECTURE.md`, `ARCHITECTURE.en.md`, and similar core project docs:
  - "What's New", "Recent Changes", "Changelog", "Release History" sections
  - "vX.Y Highlights" or any version-specific emphasis sections
- Instead, describe only current features and architecture state
- If release history is needed, use dedicated files/channels: `CHANGELOG.md`, `RELEASE_NOTES.md`, GitHub Releases
- Rely on git tags and commit log as soft authority for version history
- Verified: 2026-04-13 [confidence: high, permanent]
