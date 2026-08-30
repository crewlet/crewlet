# Architecture decision records

Why the engine is shaped the way it is.

## An ADR records a decision; it does not impose one

Every record here is a snapshot: **what was decided, when, and on the evidence
available at the time.** That is the whole of its authority. An ADR is not a
rule, not a policy, and not a promise that the choice was right — it is the
argument, written down while it was still fresh, so that whoever revisits it
argues with the reasoning rather than guessing at it.

So nothing here binds. A record that no longer matches the system it describes
is not a constraint to work around, it is a record to **supersede**. What an
ADR buys you is not obedience — it is the cost of the alternative, already
paid and already written down, so a change is made knowing what it gives up
instead of rediscovering it in production.

Several of these are backed by measurement rather than argument, because the
vendor behaviour they turn on is not what its documentation says. Those numbers
are the most perishable content in the tree: a broker version moves and the
measurement is stale. Re-measure before relying on one, and supersede the
record when it no longer holds.

**This is not product documentation.** It is for people changing the engine.
What an operator needs — how to configure, deploy and integrate it — is under
[`docs/`](../docs), published to docs.crewlet.ai.

## Revisiting one

This is the normal lifecycle, not a failure of the original. To revisit:

1. **Write a new ADR** at the next free number in the subsystem's range. Do not
   edit the old one into agreement with the new one — the superseded reasoning
   is the point, and rewriting history leaves the next reader wondering why the
   code ever looked like that.
2. **Say what changed**, not just what is now true. An ADR that reverses another
   is most useful when it names which premise stopped holding: a measurement
   that moved, a dependency that gained a feature, a failure mode that never
   materialised.
3. **Mark the old one** `Superseded by adr-NNN` (or `Amended by adr-NNN` when
   only part of it is retired — see [adr-002](002-turso-sql-dialect.md), whose
   §2 is still load-bearing while the rest is retired).
4. **Leave the number alone.** Numbers are never reused and never renumbered.

A decision that turns out to be wrong, and is superseded a week later, has done
its job. One that nobody can find the reasoning for has not.

## Status

| Status | Means |
|---|---|
| `Accepted` | The decision stands and describes the current system. |
| `Amended by adr-NNN` | Partly retired. The record says which clauses survive. |
| `Superseded by adr-NNN` | Fully replaced. Kept for the reasoning, not the conclusion. |

A record may also carry `Applies to:` naming the live code path it governs,
`Method:` where the conclusion rests on measurement, and `Implementation:`
where the decision landed across several files.

## The numbers are identifiers, not an order

Decisions are numbered by subsystem, and the number is a stable id: code
comments cite `adr-201` and `adrs/602`, so a number is never reused and never
renumbered. The ranges are a filing convention only —

| Range | Subsystem |
|---|---|
| `0xx` | Cross-cutting rules and the SQL dialect |
| `1xx` | The event queue |
| `2xx` | Coordination and the fleet's shared state |
| `3xx` | The watchdog |
| `4xx` | The engine core: context, config, epochs, events, the turn |
| `5xx` | The API, the dashboard and observability |
| `6xx` | The code sandbox and MCP |
| `7xx` | Notifications and the vendors |
| `9xx` | Release tooling |

Two files claiming one number would make every reference to it ambiguous.
`internal/version` asserts that every `adr-NNN` cited anywhere in the tree
resolves to exactly one file here, so a rename or a removal that orphans a
citation fails the build rather than going quiet.

## Writing one

Write an ADR when a change makes a choice a future reader would plausibly
reverse: a failure polarity, an ordering nothing enforces, a shape that looks
redundant until you know what it prevents. Take the next free number in the
subsystem's range, and cite it from the code it governs — a record nothing
points at is a record nobody will find, and the citation is a pointer to the
context, never an instruction to leave the code alone.

Everything else belongs in a package doc, where `go doc` will surface it beside
the code. See the `Comments explain WHY` rule in [`CLAUDE.md`](../CLAUDE.md).
