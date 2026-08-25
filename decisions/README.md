# Design decisions

Why the engine is shaped the way it is.

A decision is written down when a reader would otherwise be tempted to "fix"
the code back to the obvious shape — and would break something in the process.
Each one records the call that was made, the reasoning behind it, and what the
alternative cost. Several are backed by measurement rather than argument,
because the vendor behaviour they turn on is not what its documentation says.

This is **not product documentation**. It is for people changing the engine.
What an operator needs — how to configure, deploy and integrate it — is under
[`docs/`](../docs), published to docs.crewlet.ai.

## The numbers are identifiers, not an order

Decisions are numbered by subsystem, and the number is a stable id: code
comments cite `d-201` and `decisions/602`, so a number is never reused and
never renumbered. The ranges are a filing convention only —

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

Two files claiming one number would make every reference to it ambiguous, and
nothing warns you.

## What a decision looks like

A heading, a status, and — where it applies to a specific package — an
`Applies to:` line naming the live path. Then the reasoning.

Many of these compare the current design against the engine's **previous
Python implementation**. That is deliberate and it is the most load-bearing
content in the record: an approach that has already been tried and has already
failed in production is the strongest argument a design has, and the code it
protects looks arbitrary without it. Those passages stay whatever else changes.

## Adding one

Write a decision when a change makes a choice a future reader would plausibly
reverse: a failure polarity, an ordering nothing enforces, a shape that looks
redundant until you know what it prevents. Take the next free number in the
subsystem's range, and cite it from the code it governs — a decision nothing
points at is a decision nobody will find.

Everything else belongs in a package doc, where `go doc` will surface it beside
the code. See the `Comments explain WHY` rule in
[`CLAUDE.md`](../CLAUDE.md).
