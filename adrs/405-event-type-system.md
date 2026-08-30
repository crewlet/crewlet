# adr-405 — The event type system

Status: **Accepted**
Related: `103-payload-pointer-invariant.md`, `401`

## Settled

`internal/events` implements this decision and `internal/events/types`
carries the 60 payloads. The choices, each with the reason that outlives the
code:

- **Explicit registration**, `events.Register[TaskAssigned]()`, never a
  reflection scan over a types package. The set of types a build understands is
  greppable, and a typo surfaces as an unknown type at decode rather than as a
  silently missing subclass. Duplicate registration panics.
- **Additive-only evolution.** New fields get defaults; nothing is removed or
  repurposed. A fleet mid-upgrade has both versions on the wire, so this is a
  liveness property rather than politeness.
- **An unknown type never fails.** It decodes into the envelope with its
  unknown fields preserved in `Extra` and re-encodes losslessly. A rolling
  upgrade publishes types the older half has never heard of; dropping or
  erroring on those makes every upgrade an outage.
- **Flat JSON**, envelope and typed body in one object, because every consumer
  — dashboards, the event store's promoted filter columns, an operator reading
  a log — treats an event's own fields as first-class and a nested body puts
  every one of them behind an extra hop.
- **Trace context is passed at construction**, not read from an ambient span,
  matching adr-401: callers that create events already hold the context that
  knows the span.
- **Derived fields come from narrow optional interfaces** — `Summarizer`,
  `ActorSummarizer`, `Actorer`, `Roler`, `AgentIdentified` — with the ENVELOPE
  owning the resolution order. A payload states the one fact it knows. Handing
  each payload the whole event would let sixty of them re-derive the actor
  chain, and the moment two disagree the same turn reads differently on two
  surfaces.
- **`Event.Data` is always the pointer form**, enforced by the `PayloadPtr`
  constraint at compile time. See adr-103, which is where this was actually
  learned: construction and decoding disagreeing about a payload's Go type made
  `DataAs[*T]` answer differently on the publishing node, invisibly.

## What is not settled here

Nothing. Event work is per-subsystem: a subsystem that emits a new event
registers it, and `RegisteredTypes()` plus the test asserting the registry
matches the declared set is what keeps that honest.
