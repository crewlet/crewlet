# ADR-0010 — Tracing is configured by the standard OTel environment, not by Tier A

- **Status:** accepted
- **Authority:** `internal/tracing`
- **Enforced-by:** `internal/tracing.TestTheExporterAndTheReceiverResolveOneDestination`, which holds the engine's exporters and the sandbox receiver to one destination per signal, `internal/tracing.TestTheResolverReadsTheEnvironmentAsTheSDKDoes`, which holds that destination to what the OTel SDK itself sends, `internal/tracing.TestTheSamplingDecisionCrossesTheQueue`, which holds a trace continued on a node whose sampler says otherwise to the decision its root made, and `internal/providers/llm/anthropic.TestTheVendorSwitchesTurnNothingOff` and `internal/providers/llm/anthropic.TestTheSpanCarriesNoContentWhateverTheEnvironmentSays`, which hold the Anthropic SDK's own spans to this configuration rather than to its `ANTHROPIC_OPEN_TELEMETRY*` variables
- **Measured:** the provider is installed unconditionally and only the exporter is switched on, so `trace_id` reaches `crewlet_events` — an indexed column, and what `GET /events/trace/{id}` answers on — with no collector anywhere
- **Cost-when-tried:** OTel's own built-in default provider is a no-op that passes the parent's span context through, which silently reinstates the bug this package removed. `internal/tracing`'s `init` installs a working one for that reason. And two readers of one set of variable names disagreed three ways while a test asserting the names passed — see "How it is held"
- **Tag-status:** unreleased

## The decision

The OTLP endpoints, headers, protocols and exporter switches, the service name
and resource attributes, and the sampler and its ratio are the `OTEL_*`
environment variables every collector's own documentation uses. There is no
`tracing:` block in the Tier A file, and there will not be one.

Two packages send telemetry by those variables, which is why this is a record
rather than a package doc: the engine's own exporters in `internal/tracing`
(traces and metrics), and the sandbox OTLP forwarder in `internal/sandbox`,
which stamps a trace context into a coding box so the box's spans nest under the
turn that started them, and forwards what the box exports. **Both resolve the
OTLP variables through one resolver, `internal/otlpenv`**, and neither reads
them itself.

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

**The environment chooses within the engine's invariants, not around them.**
The sampler is always parent-based, so `OTEL_TRACES_SAMPLER` decides a trace's
root and a non-parent-based sampler is refused rather than honoured: these
traces cross processes, and a node re-deciding a trace in flight splits its
tree. For the same reason the decision crosses every hop the trace does — the
event envelope, a suspended run's record, a box's `TRACEPARENT` — so whatever
`OTEL_TRACES_SAMPLER` says on the node that continues a trace, it continues it
as decided. The propagator is W3C trace context alone and `OTEL_PROPAGATORS` is not
read: the engine writes no baggage, so the only baggage a context could carry
is a third party's off a webhook.

**A vendor library's own switches are pinned in code, never read.** The
Anthropic Go SDK spans every call it makes and sends that span's
`traceparent`, which is what the Claude API's own spans nest under, and it reads
four variables of its own on every call — `ANTHROPIC_OPEN_TELEMETRY`,
`ANTHROPIC_OPEN_TELEMETRY_PROPAGATION`,
`ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE` and
`ANTHROPIC_OPEN_TELEMETRY_TRACES_MAX_CONTENT_BYTES` — to switch the span or the
header off or to put prompts and answers on the span. Honoured, they would be a
second configuration of tracing beside this one, and the third would ship a
seat's conversation to a collector. So `internal/providers/llm/anthropic` passes
the SDK's options in code — spans on, trace context sent, metadata only — and
the variables are documented as ignored, as the OpenAI SDK's are. The SDK
records through the global provider `internal/tracing` installs, resolved on
every call, so it follows this configuration and no other.

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

## How it is held

This record first said the standard variables were the shared interface, that
each reader would read them directly, and that a test of agreement was worth
building "the day a third reader appears". That was wrong before a third reader
arrived. With two readers and one set of names, the forwarder ignored
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` — a deployment that set only that variable
exported the engine's spans and silently dropped every box's — ignored the
protocol, and neither half percent-decoded a header value, so the documented
`authorization=Bearer%20…` was refused by the backend from both. The only test
asserted the two variable NAMES were equal, which they were.

So the variables are read in one place, `internal/otlpenv`, which mirrors the
OTel Go SDK's own reading of them, and two tests hold the arrangement: the
agreement test resolves a table of environments through each half's own entry
point and requires equal destinations — endpoint, headers, protocol, whether the
signal was switched off — and equal refusals; the conformance test builds the
SDK's exporter from the environment alone and requires it to send where, and
with what, the resolver said. Each half's wire behaviour is held to its own
resolution beside them, so "resolved alike" and "sent alike" are the same claim.
