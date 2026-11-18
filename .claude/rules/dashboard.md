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
dashboard/            # src/contract/ is the ONE home of every declaration an
                      #   engine test reads (clientsource). Its Vitest suites sit
                      #   beside the modules they cover (`make dashboard-test`).
                      #   The committed build means `go build ./...` needs no
                      #   Node; CI rebuilds and diffs all three output trees
                      #   (`make dashboard-check`)
static/               # static.go embeds dashboard/ and artifacts/.
                      #   protocol/protocol.js is the wire protocol alone, run
                      #   under `node` by internal/e2e/replay.mjs, never embedded
```
