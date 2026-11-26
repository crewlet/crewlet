---
paths:
  - "internal/{statelog,estate,coord,events,queue,engine,worklet,doclet,chatlet,gitlet,usage,search,artifactdata}/**"
  - "internal/*domain/**"
  - "internal/store/schema/**"
  - "internal/sandbox/pending.go"
  - ".claude/worktrees/*/internal/{statelog,estate,coord,events,queue,engine,worklet,doclet,chatlet,gitlet,usage,search,artifactdata}/**"
  - ".claude/worktrees/*/internal/*domain/**"
  - ".claude/worktrees/*/internal/store/schema/**"
  - ".claude/worktrees/*/internal/sandbox/pending.go"
---

# Forward Mechanisms: This Build Is a Good Older Peer to the Next One
An older UNRELEASED build gets no compatibility (`CLAUDE.md`, "Breaking Changes Are Free Until a Tag Ships Them"), but a rolling upgrade mixes builds, so these forward mechanisms stay:
- The event envelope evolves additive-only; an unknown type round-trips losslessly.
- A state-log record this build cannot read is deferred, never dropped. Every record is stamped with the lowest version that reads it; a gate record stamped above the version a node reads stops that node's applier, so its writer first waits on `statelog.Readers`.
- A parked run's unknown keys ride `PendingRun.Extra`.
- A run's standing this build cannot read (an older build wrote the record without its headers) is judged on its record, read whole, never skipped.
- `internal/estate`'s wire rule: a data node refuses a routed request whose arguments or actor hold a key its build does not declare, rather than run it short. The router leaves every input field at its zero off the wire, so only a request that sets a newer field is refused. A read carrying a newer argument is refused the same way, never answered with the argument ignored. An answer a caller carries into another request is read whole or refused.
- An apply refuses only a revision that cannot run.
- A `coord.ProtocolVersion` bump makes the newer node refuse to claim beside a live lower lease.
- Every node records the capture stamp version it asks again whole (`coord.CaptureStampRecord`) durably, before it claims presence, and a take keeps media apart only once every live node and every recorded one says it.
- A run's capture stamp holding keys this build cannot ask its record again with (`SessionCapture.Extra`) is never asked short nor refused unasked: it is settled off the turn's row on the executor log, landed where the row names it, refused only where the row does not and its object is past the log's record window, otherwise left for the build that wrote it.
