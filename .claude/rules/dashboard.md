---
paths:
  - "{dashboard,static}/**"
  - "internal/clientsource/**"
  - "internal/e2e/replay.mjs"
  - ".claude/worktrees/*/{dashboard,static}/**"
  - ".claude/worktrees/*/internal/clientsource/**"
  - ".claude/worktrees/*/internal/e2e/replay.mjs"
---

# The Dashboard
```
dashboard/            # The dashboard's SOURCE: React 19 + TypeScript, built by Vite.
                      #   src/contract/ is the ONE home of every declaration an engine
                      #   test reads (clientsource), and its own suites are Vitest,
                      #   beside the modules they cover (`make dashboard-test`).
                      #   `make dashboard` builds it into static/dashboard and THAT IS
                      #   COMMITTED, so `go build ./...` needs no Node; CI rebuilds and
                      #   diffs it (`make dashboard-check`)
static/               # Embedded assets; static/dashboard/ is that build output,
                      #   compiled in via embed. NEVER hand-edited. Beside it,
                      #   static/protocol/protocol.js is the wire protocol alone,
                      #   which internal/e2e/replay.mjs runs under `node`: built and
                      #   committed with the dashboard, and never embedded
```
