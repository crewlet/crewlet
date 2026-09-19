# ADR-0010 — Tracing is configured by the standard OTel environment, not by Tier A

- **Status:** accepted
- **Authority:** `internal/tracing`
- **Enforced-by:** nothing
- **Measured:** the provider is installed unconditionally and only the exporter is switched on, so `trace_id` reaches `crewlet_events` — an indexed column, and what `GET /events/trace/{id}` answers on — with no collector anywhere
- **Cost-when-tried:** OTel's own built-in default provider is a no-op that passes the parent's span context through, which silently reinstates the bug this package removed. `internal/tracing`'s `init` installs a working one for that reason.
- **Tag-status:** unreleased

## The decision

The OTLP endpoint, headers, protocol, service name and sampling ratio are the
`OTEL_*` environment variables every collector's own documentation uses. There
is no `tracing:` block in the Tier A file, and there will not be one.

Two packages read those variables, which is why this is a record rather than a
package doc: the engine's own exporter in `internal/tracing`, and the sandbox
OTLP forwarder in `internal/sandbox`, which stamps a trace context into a
coding box so the box's spans nest under the turn that started them.

**The tracer is always running; only the exporter is optional.** A trace id is
not merely an exporter's concern here — it is an indexed column in the event
store and what the dashboard's trace view arranges into a tree. All of that
works with no collector anywhere, so ids flow whether or not anything is
collecting them, and no part of the engine branches on whether tracing is "on".

**Spans carry timing; events carry content.** Prompts, responses, tool
arguments and results and token counts are already on the phase events in the
store. A span adds the one thing no event records — how long it took — so span
attributes stay to small enums and counts rather than shipping whole prompts to
a tracing backend.

## Why the obvious alternative is wrong

The obvious alternative is a `tracing:` block in Tier A, consistent with every
other thing an operator configures. It is wrong twice.

An operator wiring a collector should be able to paste the vendor's own
snippet. A Crewlet-shaped block means translating it, and the translation is
where a wrong endpoint comes from.

More decisively, the engine's exporter and the sandbox forwarder share these
settings **deliberately**. Two settings would let those halves point at
different backends — and the link the forwarder exists to create, a box's spans
nesting under the turn that started them, then resolves on neither.

## Why Enforced-by is `nothing`

A gate would have to assert that no Tier A field configures an exporter, which
is an absence in a config struct — checkable, but the check passes identically
on a tree where somebody removed the assertion. The stronger formulation is
that both readers resolve the endpoint through one helper, and they do not:
each reads the environment directly, because the standard variables are the
shared interface. What would catch a divergence is a test that the two agree on
one endpoint given one environment, and that is worth building the day a third
reader appears.
