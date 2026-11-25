---
paths:
  - "{docs,adr}/**"
  - ".claude/worktrees/*/{docs,adr}/**"
---

# Documentation
- A new page goes in `docs/index.md`
- **Links and anchors are gated** — `internal/docsgate` fails on a relative link to a missing file, an anchor no heading renders, and a `docs/` page the index does not list; `dashboard/src/app/links.test.ts` fails on a printed dashboard address that resolves to Not Found. Renaming a heading renames every anchor to it
- **A page says each thing once**: link to where a subject is explained instead of explaining it again. A reference page lists, a guide shows how, a concept page says why, each briefly; an enumeration is a table or a list. A repeat a test reads stays: grep `_test.go` for the page's path before cutting one (`internal/docsgate/buckets_test.go` and `internal/engine/dutydocs_internal_test.go` read several)
- **Shortening a page keeps every heading's text**: code, `examples/`, absolute docs.crewlet.ai links and `crewlet-operator` link to its anchors, and `internal/docsgate` checks only relative markdown links. Grep the tree for a heading and its slug before renaming or removing it; the operator's links live in its own repository, so an empty grep does not clear them
- **`docs/reference/metrics.md` and `alarms.md` are generated** (`make metrics-doc`, `make alarms-doc`): edit their source, never the page
- **`docs/` is product documentation**, for people running Crewlet. Process for people with commit rights on this repository is a root-level file beside `CONTRIBUTING.md`
- **An ADR lists its gates one per line**: beneath a bare `- **Enforced-by:**`, each gate a bullet indented two spaces (`adr/0000-template.md`), a new one appended at the end, so two changes adding gates merge instead of conflicting on one line. One gate or `nothing` may stay on the field's line; `internal/adr` reads both forms as one value
- **Diagrams are Mermaid**, never ASCII box art; directory trees and literal command output stay plain code fences
