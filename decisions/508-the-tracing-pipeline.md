# 508 — The tracing pipeline, and the ids that were wrong before it

Status: **decided, implemented**

Applies to: `internal/tracing`, `internal/logging`, `internal/engine`,
`internal/agent/runner`, `internal/agent/toolloop`, `internal/tools`,
`internal/api/webhooks`, `internal/schedule`

Relates to: [001 — logging is slog](001-logging-is-slog.md),
[401 — what travels implicitly](401-context-threading.md),
[402 — suspend and resume](402-suspend-resume.md),
[405 — the event type system](405-event-type-system.md),
[506 — the observability pipeline](506-the-observability-pipeline.md)

## What was actually there

The same shape [506](506-the-observability-pipeline.md) found, one layer up.

`events.Event` had `trace_id`, `span_id` and `parent_span_id`. `crewlet_events`
stored all three as indexed columns. `GET /events/trace/{trace_id}` answered on
them. The dashboard's Traces view arranged them into a tree. `docs/` told
operators that "the engine configures a `BatchSpanProcessor` with an
`OTLPSpanExporter` at startup" when `OTEL_EXPORTER_OTLP_ENDPOINT` was set, and
the stack table listed OpenTelemetry for tracing.

There was no OpenTelemetry in the tree.

```
$ grep -rn 'go.opentelemetry.io' --include='*.go' . | wc -l
0
```

`events.NewTrace` minted a 32-hex trace id and a 16-hex span id from
`crypto/rand`, and said so in its own doc comment: *"SHAPED rather than merely
random: a 32-hex trace id and a 16-hex span id are what a real tracer accepts
**when one is wired**"*. Eight publishers called it. Nothing ever wired a tracer.

## The three ways the ids were wrong

Worth stating separately, because none of them needed a tracer to be a bug —
they were wrong against the dashboard that was already reading them.

1. **A turn published its trigger's span id as its own.** `describeTurn` copied
   `{TraceID: ev.TraceID, SpanID: ev.SpanID, ParentSpanID: ev.ParentSpanID}`
   verbatim off the trigger. So every event a turn emitted claimed a span that
   had already ended, and the tree — which keys on `span_id`/`parent_span_id` —
   collapsed the whole turn onto the wake that started it. Inheriting the
   *trace* was right; inheriting the *span* was not.

2. **A resumed turn published no span id at all.** `describeResume` set
   `ParentSpanID` and left `SpanID` empty, so the second half of every resumed
   turn was unplaceable in the tree and indistinguishable from every other
   resumed turn.

3. **The sandbox OTLP receiver was dead from the engine side.** `TurnRef` has
   carried `TraceID` and `SpanID` since that receiver was written, and its only
   construction site set neither. So `PendingRun.TraceID` was always `""`, and
   `RunEnv` returns `nil` for an empty trace on purpose — a token scoped to an
   empty trace authenticates every run's export as every other run's. No coding
   run has ever exported in-box telemetry, through an endpoint the engine goes
   to considerable length to mint, sign and expire.

## The decision

**One `TracerProvider`, installed unconditionally, configured from the standard
`OTEL_*` environment. Spans at five boundaries. The event envelope stays the
carrier.**

### The provider is unconditional; only the exporter is optional

A trace id here is not only an exporter's concern. It is a column in the event
store, a REST route and a dashboard view, all of which work with no collector
anywhere. Making the provider conditional would leave a deployment that exports
nothing holding events whose ids are unusable, and would make every span site a
place where two behaviours are possible.

**The package's own `init` installs a working provider**, the way
`internal/logging`'s init installs `os.Stderr`. This is not tidiness, and it is
the one thing here that was found by a test rather than by reasoning: OTel's
built-in default provider is a no-op that returns a **non-recording span
carrying the parent's span context straight through**. With nothing installed,
`Start` mints no new span id and `TraceOf` returns the id of the span above —
which is bug (1) above, reappearing in every process that had not called
`Configure`. That is every test in the tree, and any embedding of the engine.
The default exports nothing and starts no goroutines, so it is safe to install
at init and safe never to shut down.

### The environment configures it, not Tier A

`internal/sandbox/otel.go` had already made this call and written down why: the
OTLP pair is *"the standard OTel spelling every collector's own documentation
uses. An operator wiring a collector should not have to translate it into a
Crewlet-shaped block."*

The exporter reads **the same two variables that receiver reads**, deliberately.
The receiver stamps a `TRACEPARENT` into a sandbox whose parent is the engine's
own span; if the two halves exported to different backends, that parent/child
link would resolve on neither. A test asserts the constants cannot drift apart.

[d-001](001-logging-is-slog.md) supplies the other half of the argument. It
retired `debug:` on the rule that two keys setting one value is a state where
they disagree, and a `tracing.endpoint:` field beside
`OTEL_EXPORTER_OTLP_ENDPOINT` is exactly that.

**The two endpoint variables mean different things**, and conflating them was a
real defect: `OTEL_EXPORTER_OTLP_ENDPOINT` is a base the exporter appends the
signal path to, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is the full URL. The SDK's
`WithEndpointURL` implements only the second — handed a path-less base it
targets `/`. The deployment guide had been telling operators to put
`/v1/traces` on the *base*, which the sandbox forwarder then appends `/v1/`+signal
to, producing `/v1/traces/v1/traces` at the collector.

`OTEL_EXPORTER_OTLP_PROTOCOL` is now actually read. It was documented as read
and propagated and never was.

### Five span names, and thin attributes

`webhook.receive` · `agent.turn` (and `agent.turn.resume`) ·
`agent.turn.<phase>` · `llm.round` · `tool.call`, plus `schedule.fire`.

**Spans must not duplicate what events already record.** [506](506-the-observability-pipeline.md)
put prompts and responses verbatim, every tool call's arguments and result,
token counts and the decision onto `agent_phase_completed`. Copying that onto a
span would ship whole prompts to a collector that is not the event store. The
one thing no event records is **duration**, per phase and per round — so spans
carry timing plus small enum attributes, and nothing else.

Three placements were not obvious:

- **One span per phase wraps the whole extension loop**, not each
  `toolloop.Run` inside it. An extended Execute is one phase that ran longer;
  a span per invocation would report it as two and split its rounds. Its
  attributes are read off the phase totals, never off the last invocation.
- **The tool span lives in `tools.Surface`, not `toolloop`.** Surface is the
  only frame that knows the tool's MCP origin, the guard refusal and the acting
  seat — toolloop sees a name and a phase. It covers all three outcomes, and
  two of them (unknown/inactive, guard refusal) never reach `invoke`. Those are
  the calls a reader is most often hunting for. It is also the only in-band
  record of a *suspending* call, which returns no ledger row at all.
- **Only the LLM round is spanned, not the provider stack beneath it.** A
  phase runs through the fallback chain, each member's backend and the
  credential pool's rotation; instrumenting all four on a three-member chain
  over a four-key pool yields up to twelve nested spans per round and tells a
  reader nothing they can act on.

### A suspend ends its span; a resume opens a new one

A suspend is a **return** ([402](402-suspend-resume.md)), not an error. The
process may exit, the seat may move node, and days may pass. A live span cannot
survive any of that, so the honest representation is two spans in one trace:
the suspending span ends, and the resume reconstructs it as a **remote** parent
from the ids on the run's own row and opens a new span beneath it. The wait
becomes the gap it actually is.

A run written by a build before those ids were stored carries none, and that
resumes under a fresh root rather than refusing to resume — a rolling upgrade
guarantees some of those exist.

### Sampling is parent-based, always

The ratio governs only the traces a node **roots**. A remote decision is always
honoured, because these traces cross processes routinely and an unsampled
parent with sampled children is a broken tree at the collector. A bad ratio
warns and falls back to always-on rather than refusing to boot — the same rule
`-log-level` follows, and falling back to *on* because the failure an operator
can see beats the one they cannot.

## What this changes in d-401, and why it is admitted

[d-401](401-context-threading.md) says `context.Context` carries exactly two
engine values and that "nothing else goes in `context.Context`". A span is now
the third.

It is admitted on the same test the other two passed, and the record's own
table is the test: the consumer is a **leaf** (the tracer), the value is
**immutable**, and it **fails safe when absent** — no span means a no-op span,
never wrong behaviour. There is also no alternative: OTel has no other carrier,
and a live span on `TurnContext` would be precisely the "a goroutine captures
turn state and outlives the turn" bug d-401 exists to forbid.

The other half of d-401 — "log fields", consumer `logging` — was decided and
never built, and is built here. It is **two validated hex fields, not an open
bag**. An open bag is a path for arbitrary caller-supplied text to reach a
terminal, and `TestNothingReachesTheTerminalRaw` guards the attribute path, not
that one; an ESC repaints a terminal and a newline forges a whole log line. A
trace id genuinely arrives from outside — a remote `traceparent` on the webhook
edge — so ids that are not lowercase hex of the right length are dropped.

The carrier is defined in `internal/logging` rather than read from the OTel span
in the handler, which would be the idiomatic OTel route. `go list -deps
./internal/logging` returns only itself and 78 packages import it; that one
import would put `go.opentelemetry.io/otel/trace` into every one of their
dependency graphs to render two hex strings.

Injection is in `lazy.Handle`, so all three formats carry the ids and no format
learns about tracing. **`Enabled` is untouched** — d-001's constraint stands,
and a level that varied by whether a trace happened to be bound would filter
different lines depending on which spelling a call site used. Wrapping inside
`install` instead would give console, text and json the same concrete type and
fail `TestEveryDeclaredFormatInstallsItsOwnHandler`, which exists precisely so
a format cannot silently render as another.

## What this does NOT change in d-405

[d-405](405-event-type-system.md) requires that trace context is **passed at
construction, not read from an ambient span**. That stands. `events.New(payload,
trace)` still takes its trace as an argument; what changed is only where the
caller gets the argument from — `tracing.TraceOf(ctx)` rather than
`events.NewTrace()`. `TraceOf` reports the active span's ids, and mints a fresh
root when there is no span, which is exactly what those eight call sites did
before. So no event ever gets an empty trace, and no event's trace depends on
which frame happened to build it.

The docs claimed the opposite — that `trace_id` is "auto-populated from the
active OTel span at creation time" — and that prose is now corrected rather
than implemented.

## The gate

`TestATurnsEventsJoinTheTriggersTrace` in `internal/e2e`: a real company, woken
by an event carrying a known trace, whose stored events must carry that trace
id, must **not** carry the trigger's span id as their own, and must include one
hanging off the trigger's span.

It is in `internal/e2e` for the reason 506 gives — every component's tests stop
at a seam and substitute what is on the other side, so "does anything actually
connect these" is the one question none of them asks. It is what caught the
no-op default provider, and it fails when either half of the wiring is removed.
