---
paths:
  - "{docs,adr}/**"
  - ".claude/worktrees/*/{docs,adr}/**"
---

# Documentation
- A new page goes in `docs/index.md`, which is also the docs site's navigation
- **Links and anchors are gated** — `internal/docsgate` fails on a relative link to a missing file, an anchor no heading renders, and a `docs/` page the index does not list; `dashboard/src/app/links.test.ts` fails on a printed dashboard address that resolves to Not Found. Renaming a heading renames every anchor to it
- **`docs/` is product documentation**, for people running Crewlet. Process for people with commit rights on this repository is a root-level file beside `CONTRIBUTING.md`
- **Diagrams are Mermaid**, never ASCII box art; directory trees and literal command output stay plain code fences
