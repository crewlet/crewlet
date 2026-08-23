# d-404 — A config revision is an immutable epoch, and a turn is pinned to one

Status: decided
Related: `401` (the pin lives in TurnContext, never in context.Context), `402`
(which epoch a resumed turn gets), `docs/concepts/control-plane.md`

## What dies

The Python engine applied a new revision by MUTATING the live objects in place
and keeping their identity: `clear()` then `update()`, field assignment on the
org, provider swaps under a lock. Identity was preserved so that anything
holding a reference kept working.

That is precisely the problem. Anything holding a reference kept working — and
kept reading, mid-turn, values from two different revisions. A turn that read
the budget cap before the swap and the model chain after it ran under a company
that never existed. Nothing raised; the turn simply behaved like neither
revision.

## The rule

**A config revision is an immutable `*config.Snapshot`. Applying one publishes a
NEW snapshot; nothing is ever mutated in place. A turn pins one snapshot at
start and reads only that snapshot until it ends.**

```go
// Live holds the current epoch. Readers take a snapshot; writers publish one.
type Live struct{ cur atomic.Pointer[Snapshot] }

func (l *Live) Pin() *Snapshot          { return l.cur.Load() }
func (l *Live) Publish(s *Snapshot)     { l.cur.Store(s) }
```

`Pin()` at the top of a turn, into `TurnContext.Pin`, and every read for the
rest of that turn goes through it. A revision published mid-turn is simply not
observed by that turn — which is the guarantee, not a limitation. The next turn
gets it.

The pin is an explicit field rather than a `context.Context` value on purpose
(d-401): an ambient pin is one `context.Background()` away from a turn silently
reading live config.

## Apply order, and why it is not alphabetical

A revision touches subsystems that depend on each other, so the order is part of
the contract:

```
org → budgets → turn engine → providers → scalars → restart-required
```

Org first because everything downstream is keyed by seat. Budgets before the
turn engine because a turn must never start under a cap that is about to
change. Providers after the turn engine because a provider swap is the most
failure-prone step and the cheapest to roll back at that point. Restart-required
last, because reaching it is what makes a failure unrecoverable — see below.

**A seat is drained before its tools are mutated.** Not paused, drained: an
in-flight turn holding an MCP child that is about to be replaced would have its
tool calls fail mid-round, and a turn's tool surface must not change under it.

## Three outcomes, and `degraded` is the one that matters

`ok` / `error` / `degraded`, and only `ok` counts as converged.

`degraded` means the apply failed AFTER a restart-required subsystem was already
mutated — so rollback could not restore the previous state, and this node is now
running something that is neither revision. It is not a worse `error`; it is a
different fact, and conflating them is what makes a fleet report convergence it
does not have. A degraded node needs a restart, not a retry.

This is why restart-required subsystems are applied LAST: every step before them
can be rolled back cleanly, so the window in which `degraded` is reachable is as
small as the ordering can make it.

## Rollback is re-publishing, not un-applying

Because snapshots are immutable, rolling back is `Publish(previous)` — the
previous snapshot is still intact and still correct, since nothing mutated it.
Un-applying a mutation, which is what the Python engine had to attempt, is a
second code path exercised only on the failure it exists to handle. This
version has no second path.

The exception is the restart-required subsystems, which is the whole content of
`degraded`.

## What re-activating an unchanged revision must still do

Re-activating the same revision is the documented credential-rotation gesture,
so the no-op check cannot be a payload comparison: the payload is identical and
the point is that its `${VAR}` references now resolve differently. The apply
path compares the payload AND the resolution fingerprint (`config_resolution`),
and a change in either is a real apply. Preserved verbatim from the Python
engine, which learned it the hard way.

## Lag is not divergence

Stated here because it is the rule most likely to be "improved": a node behind
the current epoch is a node mid-rollout, and every successful rollout produces
lag. Shedding on lag alone makes the fastest node the cause of a fleet-wide
outage. The posture function (`internal/configplane`) already encodes this and
its tests pin it; nothing in the apply path may reintroduce a lag-based shed.
