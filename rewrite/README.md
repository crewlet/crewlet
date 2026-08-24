# The rewrite record

How Crewlet's Go engine came to be shaped the way it is. Two kinds of file:

- **`decisions/`** — a call that was made, why, and what the alternative cost.
  A decision is written when a reader would otherwise be tempted to "fix" the
  code back to the obvious shape.
- **`questions/`** — something found during the port that did not settle into a
  decision: a vendor behaving differently from its documentation, an SDK
  flattening a distinction the engine needs, a bug in the implementation being
  replaced.

This is not product documentation. It is the reasoning behind the engine, for
people changing it; what an operator needs is under [`docs/`](../docs).

## Two conventions

**Decisions are numbered by the phase of [`REWRITE_PLAN.md`](../REWRITE_PLAN.md)
they belong to** — `1xx` the queue, `2xx` coordination, `3xx` the watchdog,
`4xx` the engine and config, `5xx` the API and dashboard, `6xx` sandbox and
MCP, `7xx` notifications and the vendors, `9xx` release tooling. Numbers are
unique across the whole tree, not per phase: two files claiming one number
means every reference to it is ambiguous, and nothing warns you.

**`Spec:` names the Python implementation the decision was ported from.** Those
paths — `src/crewlet/…`, `tests/test_…` — do not exist any more; the Go tree
replaced them, and git history is where they live now. They are provenance,
recording what a decision was measured against, and are deliberately not
rewritten to Go paths: a decision that *departed* from its spec is exactly the
kind this record exists to keep. `Applies to:` is the live path.
