# ADR-0010 — Tracing is configured by the standard OTel environment, not by Tier A

- **Status:** accepted
- **Authority:** `internal/tracing`
- **Enforced-by:** `internal/tracing.TestTheExporterAndTheReceiverResolveOneDestination`, which holds the engine's span exporter and the sandbox receiver to one traces destination, `internal/tracing.TestTheResolverReadsTheEnvironmentAsTheSDKDoes`, which holds that destination to what the OTel SDK itself sends, `internal/tracing.TestTheSamplingDecisionCrossesTheQueue`, which holds a trace continued on a node whose sampler says otherwise to the decision its root made, and `internal/providers/llm/anthropic.TestTheVendorSwitchesTurnNothingOff` and `internal/providers/llm/anthropic.TestTheSpanCarriesNoContentWhateverTheEnvironmentSays`, which hold the Anthropic SDK's own spans to this configuration rather than to its `ANTHROPIC_OPEN_TELEMETRY*` variables, and `internal/providers/llm/cliagent.TestAClaudeCodeCallIsHandedTraceContextAndNoTelemetryDestination`, which holds a vendor CLI on the engine host to receiving the round's trace context and none of the engine's telemetry configuration, and `internal/sandbox.TestTheRunnersSwitchesTravelOnlyBesideAnEndpoint` with `internal/sandbox/codingagent.TestClaudeCodeSwitchesItsTracesOnAndNothingElse` and `TestClaudeCodeIsToldToExportNoContent`, which hold a vendor CLI in a box to exporting its traces — only its traces, with no content — through the receiver and nowhere else, and `internal/sandbox.TestAnImagesPerSignalVariablesDecideNothing` with `TestTheOperatorsTelemetryEnvironmentWins`, which hold what a box's exporter resolves to the receiver whatever per-signal variable its image carries, and to the operator's collector where `role.sandbox.env` names one, and `internal/sandbox/codingagent.TestClaudeCodeIsHandedTheRunsTelemetryAboveItsSettingsFiles`, which holds what the launch decided over a settings file the box carries, and `cmd/crewlet.TestDoctorIsTracedFromTheOTelEnvironment` with `TestDoctorExaminesEachEntryInARootSpanOfItsOwn`, which hold the one command outside a node that calls a model to this configuration and to a span over every call it makes
- **Measured:** the provider is installed unconditionally and only the exporter is switched on, so `trace_id` reaches `crewlet_events` — an indexed column, and what `GET /events/trace/{id}` answers on — with no collector anywhere
- **Cost-when-tried:** OTel's own built-in default provider is a no-op that passes the parent's span context through, which silently reinstates the bug this package removed. `internal/tracing`'s `init` installs a working one for that reason. And two readers of one set of variable names disagreed three ways while a test asserting the names passed — see "How it is held"
- **Tag-status:** unreleased

## The decision

The OTLP endpoints, headers, protocols, compression, TLS material and exporter
switches, the service name
and resource attributes, and the sampler and its ratio are the `OTEL_*`
environment variables every collector's own documentation uses. There is no
`tracing:` block in the Tier A file, and there will not be one.

Two packages send telemetry by those variables, which is why this is a record
rather than a package doc: the engine's own exporters in `internal/tracing`
(traces and metrics), and the sandbox OTLP forwarder in `internal/sandbox`,
which stamps a trace context into a coding box so the box's spans nest under the
turn that started them, and forwards the traces the box exports — traces alone,
the one signal any runner switches on in a box, so a box's token is worth spans
in its run's trace and nothing else. **Both resolve the
OTLP variables through one resolver, `internal/otlpenv`**, and neither reads
them itself — the engine hands its exporters everything the resolver read, so
the SDK's own reading of the same variables never decides anything, and the
forwarder sends what a box exported the way those exporters send: the resolved
protocol (a gRPC `Export` call to a gRPC collector), compression and TLS.

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

**A vendor CLI the engine starts on its own host is handed trace context, never
a telemetry destination.** A text-mode `claude-code` call is given the W3C trace
context of the round it belongs to (`TRACEPARENT`, `TRACESTATE`, rendered by
`internal/tracing`'s one encoding) and the profile's two propagation switches,
and nothing of the `OTEL_*` environment — the child's environment is an
allowlist that carries none of it. So the CLI forwards the round's context to
the Claude API, whose spans nest under the round, and exports nothing itself:
handing it the exporter's endpoint and headers would give a vendor's CLI the
company's collector credential, and its own spans and log events would be a
second telemetry configuration, with content switches of its own, beside this
one.

**A vendor CLI in a box exports its traces through the receiver, switched on by
its runner.** A box is where the engine's own tracer cannot reach, so there the
CLI's spans are the only record of what the run did between its launch and its
end, and they are worth exporting — but to the receiver, which holds the
collector's credential outside the box, and nowhere else. Whether an agent
exports at all is declared by its runner (`internal/sandbox/codingagent`), in
that CLI's own vocabulary, and added only beside an endpoint the receiver
minted: an agent switched on with nowhere to send exports to its own default
address, and an endpoint minted for an agent that never exports is a token
nothing needs. Claude Code is switched to its traces alone — its metrics would
count the run's tokens a second time beside the engine's own, and its log
events are its content channel — with every content switch declared off and a
parent-based sampler, so the box follows the decision `TRACEPARENT` carries.
The box is handed the receiver as the BASE endpoint, and each signal's own
endpoint, headers, protocol and timeout variable is written empty, which every
exporter reads as unset: an exporter reads a signal's own variable first, so
one an image baked in would otherwise decide where the box's spans went. The
same values are handed to Claude Code as flag settings too, because it reads
the `env` of a settings file over its environment, and a user settings file in
the box would otherwise decide. A MANAGED settings file is the one layer above
those, and nothing the engine sets reaches it: an image that ships one, or a
direct box's host, decides the CLI's telemetry itself. The
operator's `role.sandbox.env` wins over all of it, as over every variable the
engine sets in a box.

**A command that calls a model is traced as a node is.** `crewlet llm doctor`
sends real calls through the engine's own providers, so it configures
`internal/tracing` from this environment as `crewlet run` does, flushes it as
it exits, and makes each entry's calls inside a root span of its own. A
command left on the provider `internal/tracing`'s init installs exports
nothing and still sends a sampled `traceparent` on every call, so the Claude
API's spans of its calls named a parent no backend ever received.

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
point and requires equal destinations — endpoint, headers, protocol, compression,
TLS material, whether the signal was switched off — and equal refusals; the
conformance tests build the SDK's exporter from the environment alone and
require it to send where, with what, compressed as and trusting what the
resolver said.

The forwarder once honoured the shared destination only as far as an address:
it re-sent a box's body over OTLP/HTTP whatever protocol was resolved, without
the box's Content-Encoding and with none of the certificate variables, so a
deployment on gRPC, on gzip from inside its boxes, or behind a private
certificate authority received the engine's spans and refused every box's. So
it decodes what it receives and writes the message itself, and
`internal/sandbox` holds it to a gRPC collector, an https one behind a private
authority with a client certificate, and both compressions. Each half's wire behaviour is held to its own
resolution beside them, so "resolved alike" and "sent alike" are the same claim.
