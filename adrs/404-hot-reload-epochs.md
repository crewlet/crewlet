# adr-404 — A config revision is an immutable epoch, and a turn is pinned to one

Status: **Accepted**
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
(adr-401): an ambient pin is one `context.Background()` away from a turn silently
reading live config.

## Apply order, and why it is not alphabetical

A revision touches subsystems that depend on each other, so the order is part of
the contract:

```
secrets → company → tools → learning → sandbox
        → parties → integrations → EPOCH → mailboxes → scheduler
```

Secrets first, because the rotation gesture below turns on it. The company is
built next and touches nothing while it builds, which is what makes stage 2 the
cheapest place to refuse. Everything through `integrations` prepares the new
epoch; the swap at `EPOCH` is the single instant it becomes current; and the two
stages after it read the seat list and the schedules off the company that is now
current, so arming them earlier would act for the outgoing one.

**Nothing is drained, and no seat is paused.** An in-flight turn pins its epoch
once and reads through that, so its tool surface cannot change under it — which
is the whole benefit of publishing rather than mutating, and the reason this
engine needs no quiescing step. What a pin cannot hold is a *capability*: a
shared MCP child the apply restarted leaves a pinned tool dispatching to a
closed client, which surfaces as a tool error the model can read rather than a
name that vanished mid-round.

## Three outcomes, and `degraded` is the one that matters

`ok` / `error` / `degraded`, and only `ok` counts as converged.

`degraded` means the apply failed AFTER a subsystem that cannot be un-applied
was already mutated, leaving the node running something that is neither
revision. It is not a worse `error`; it is a different fact, and conflating them
is what makes a fleet report convergence it does not have. A degraded node needs
a restart, not a retry.

**It is not reachable in this build**, and the ordering is why. The two
subsystems that genuinely cannot be un-applied — the per-role MCP children and
the notification transports — are not on the apply path at all: the children
belong to a seat's lease, and the transports are built once at boot. What the
apply does mutate ahead of its last failure point is bounded and re-doable by
the next successful apply: the resolver snapshot, the reflection workers, and
any shared MCP child whose spec moved. None of that needs a restart to recover,
which is the line between `error` and `degraded`.

So `degraded` is a live constraint on where a new stage may go rather than a
status with nothing behind it: wire either of those two subsystems into the
apply ahead of the swap and it becomes reachable that day.

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
so a no-op check could never be a payload comparison: the payload is identical
and the point is that its `${VAR}` references now resolve differently. The
Python engine reached for a second comparison — a keyed digest over what those
references resolved to — because it HAD a payload short-circuit to defeat.

This engine has no short-circuit to defeat, so it carries no digest either.
`Apply` is straight-line: the reconciler skips on the EPOCH it has applied,
never on content, and the pointer's own KV sequence IS the epoch, which the
store advances on every write. A byte-identical re-activation therefore mints a
new epoch, and always reaches an apply that re-reads the secret store first
(`internal/engine/epoch.go`).

One comparison survives, one layer down and over resolved values rather than a
digest of them: `mcp.Bridge.Reconcile` compares each shared child's spec against
the one it is already running, because a child is a PROCESS and restarting every
one on every apply would tear down working servers to arrive back where they
started. That is safe for a rotation only because the spec's `env`, `headers`
and `url` are resolved at the edge before the comparison — comparing the stored
entry, where `${VAR}` stays verbatim, would silently stop rotation reaching MCP
children at all. Nothing persists a digest of a live credential across applies,
which is the property the Python fingerprint was reaching for and the one that
would have turned the fix into a leak the moment it reached a log line.

## Lag is not divergence

Stated here because it is the rule most likely to be "improved": a node behind
the current epoch is a node mid-rollout, and every successful rollout produces
lag. Shedding on lag alone makes the fastest node the cause of a fleet-wide
outage. The posture function (`internal/configplane`) already encodes this and
its tests pin it; nothing in the apply path may reintroduce a lag-based shed.
