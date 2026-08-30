# adr-401 — What travels implicitly, and what must be an argument

Status: **Accepted**
Related: `000-go-native-rewrite.md` (contextvars → context.Context is listed there
as a translation; this is the decision that makes it precise), `402`, `404`

## What it replaces

The Python engine carried five separate ambient channels through a turn:
`work_key`, the `TurnPin`, the LLM scope, phase progress, and bound log fields.
Each was a `contextvars.ContextVar`, and each was justified the same way: the
code that needs it sits many frames below the code that knows it, behind
functions with no other reason to carry it.

That justification is real. It is also how a turn ends up with five invisible
inputs, any of which can be missing, stale, or inherited by a goroutine nobody
intended to give it to. Go makes the second half worse, not better: a
`contextvars.Context` is copied into a task at creation, while a Go goroutine
shares whatever `context.Context` it was handed, forever, including after the
turn that created it has finished.

## The rule

**`TurnContext` is an explicit argument. `context.Context` carries a closed,
enumerated set of engine values — two when this was written, three since
[d-508](508-the-tracing-pipeline.md) — and each only because its consumer is a
leaf.**

```go
// TurnContext is everything a turn's own code needs and nothing else.
// Immutable after construction; derive a new one rather than mutating it.
type TurnContext struct {
    TurnID   string
    Seat     org.Seat
    Trigger  TriggerRef
    Pin      *config.Snapshot   // the epoch this turn is pinned to (adr-404)
    Budget   BudgetMeter
    Phases   PhaseRecorder
    LLM      llm.Scope
    Delegation DelegationChain
}
```

Every phase, tool and provider entry point takes
`(ctx context.Context, tc *TurnContext, …)`. Two parameters, in that order,
always — `ctx` for cancellation and deadlines, `tc` for what the turn is.

### The exceptions, and why they earn it

`context.Context` values are for facts a LEAF needs that no intermediate frame
has any business knowing. Two qualify:

| value | consumer | why not an argument |
| --- | --- | --- |
| `workkey.Key` | the completion ledger and conversation ledger writers | They are called from inside store and notification code that has no turn concept at all. Threading it would put a turn-shaped parameter on every write path in `internal/store`. |
| log fields | `logging` | Same shape, one level worse: every function that logs would take a logger. |

Both are **immutable value types**, and both **fail safe when absent**: an
empty work key means "a turn with no ledgerable trigger", which is exactly the
case that skips the duplicate guard; absent log fields mean a line with less
context, never a wrong one. Nothing branches on their presence to decide
correctness.

### Amended by d-508: there is a third, and it is the OTel span

| value | consumer | why not an argument |
| --- | --- | --- |
| the active span | the tracer, and `logging` for the ids | OpenTelemetry has no other carrier — `context.Context` *is* its API. A span on `TurnContext` would be the exact bug the section below forbids: turn state in a struct a tool or a goroutine can capture and outlive the turn with. |

It is admitted on the same three tests: the consumer is a leaf, the value is
immutable, and it fails safe when absent — no span means a **no-op** span, never
wrong behaviour. See [508](508-the-tracing-pipeline.md).

The "log fields" row above was decided here and built there, as `WithTrace` in
`internal/logging` — and as two validated hex fields rather than the open field
bag the phrase suggests, because an open bag is a route for arbitrary text to
reach a terminal.

Nothing else goes in `context.Context`. In particular the config pin does NOT:
a turn reading config through an ambient channel is how a mid-turn reload gets
observed halfway (adr-404).

## Sub-agents inherit by function call, never by sharing

```go
// ForSubagent derives the context an ephemeral sub-agent runs under.
func (tc *TurnContext) ForSubagent(seat org.Seat) (*TurnContext, error)
```

It keeps the pin (a sub-agent must see the same company its parent does), keeps
the budget meter (a sub-agent spends its parent's budget — that is what the
delegation cap is for), **resets** the phase recorder (a sub-agent's phases are
its own), and **extends** the delegation chain, refusing past the cap.

The `context.Context` handed to a sub-agent goroutine is derived with
`context.WithoutCancel` plus an explicit deadline where one applies, never the
parent's raw ctx. A sub-agent outliving its parent's cancellation is a
deliberate choice per call site, not an accident of sharing a pointer.

**A goroutine that captures `tc` and outlives the turn is a bug**, and the one
the linter cannot see. The rule that makes it checkable: `TurnContext` is
passed, never stored in a struct field that outlives a turn. Anything needing
turn state after the turn takes a copy of the values it needs.

## What this costs, honestly

Two parameters on a few hundred functions. That is the price of a turn's inputs
being visible in its signature, and it is worth paying: the Python engine's
hardest bugs in this area — a sub-agent inheriting a parent's phase recorder, a
turn reading a config field that changed underneath it — are both unrepresentable
here rather than merely unlikely.
