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
An older UNRELEASED build gets no compatibility (`CLAUDE.md`, "Breaking Changes Are Free Until a Tag Ships Them"), but a rolling upgrade mixes builds, so these forward mechanisms stay: the event envelope evolves additive-only and an unknown type round-trips losslessly; a state-log record this build cannot read is deferred, never dropped, and every record is stamped with the lowest version that reads it (a gate record above that version stops the applier, so its writer waits on `statelog.Readers`); a parked run's unknown keys ride `PendingRun.Extra`; a data node refuses a routed request whose arguments or actor hold a key its build does not declare rather than run it short, and the router leaves every input field at its zero off the wire, so only a request that sets a newer field is refused — a read carrying a newer argument is refused the same way, never answered with the argument ignored, and an answer a caller carries into another request is read whole or refused (`internal/estate`'s wire rule); an apply refuses only a revision that cannot run; and a `coord.ProtocolVersion` bump makes the newer node refuse to claim beside a live lower lease.
