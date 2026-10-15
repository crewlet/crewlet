// Package tracing is the engine's OpenTelemetry wiring: one TracerProvider and
// one MeterProvider, built from the environment, installed globally together,
// and flushed on the way out.
//
// # Why the provider is always installed, and only the exporter is optional
//
// A trace id is not only an exporter's concern here. Every event carries
// `trace_id` / `span_id` / `parent_span_id` into `crewlet_events`, which is
// what `GET /events/trace/{id}` reads and what the dashboard's Traces view
// arranges into a tree — all of that works with no collector anywhere. So the
// provider is unconditional and only the OTLP exporter is switched on by
// configuration: ids flow the same whether or not anyone is collecting them,
// and nothing above this package ever branches on whether tracing is "on".
//
// The alternative — no provider unless an endpoint is set — makes every span
// site a place where two behaviours are possible, and makes a deployment with
// no collector a deployment whose stored events have no usable ids. That is
// the state this package was written to end.
//
// # Why the environment configures it and Tier A does not — ADR-0010
//
// The OTLP endpoint, headers, protocol, compression, TLS material and exporter
// switches are the standard OTEL_* variables, "the standard OTel spelling every collector's own
// documentation uses. An operator wiring a collector should not have to
// translate it into a Crewlet-shaped block." The sandbox OTLP receiver reads
// the same variables to find the backend it forwards a coding box's
// telemetry to, and on purpose: a company has one telemetry backend, and the
// whole point of the trace context the receiver stamps into a sandbox is that
// the box's spans nest under the engine's. Two settings would let an operator
// split them across two backends, where the parent/child link resolves on
// neither.
//
// SHARING THE NAMES WAS NOT ENOUGH, and that is why both halves now resolve
// through [github.com/crewlet/crewlet/internal/otlpenv] rather than reading
// the environment themselves: with one set of names they still disagreed
// about the signal endpoint, the protocol and how a header value is decoded.
// TestTheExporterAndTheReceiverResolveOneDestination holds them together, and
// every exporter here is handed the whole resolution explicitly — endpoint,
// headers, compression, TLS and the timeout ([exporterOptions]), the timeout
// bounding the batch processor and the metric reader too ([exportTimeout]) —
// so the SDK's own reading of the same variables is never a third opinion.
// What the SDK still reads for itself is what no resolution here covers: the
// batch processor's queue, batch size and delay, the span limits, and the
// metric exporter's temporality and exemplar filter, each as the SDK documents
// it. A second bound the SDK would read and an explicit setting here would then
// override is refused by name instead ([refuseSuperseded]); a shape it would
// read and then ignore is reported at boot ([reportUnhonoured]).
//
// A second reason, specific to this engine: Tier A is the root of trust and
// resolves with EnvOnly, and it holds to the rule that two keys setting one
// value is a state where they disagree. A `tracing.endpoint:` field beside
// OTEL_EXPORTER_OTLP_ENDPOINT would be exactly that.
//
// # Only W3C trace context propagates
//
// The global propagator is TraceContext alone — no Baggage. This engine never
// writes baggage, so the only baggage a context here could carry is what a
// third party put on a webhook, and a propagator that extracted it would hand
// it to every outbound call that injects through the global propagator — the
// Anthropic SDK does, on every call to the Claude API or the gateway in its
// place: a vendor's key-value pairs forwarded, under this engine's name, to
// whoever it calls next. TraceContext is the half that does a job here — it is what makes
// an inbound `traceparent` join an existing trace.
//
// # Sampling is decided once per trace, at its root
//
// The sampler is ALWAYS parent-based: a span whose parent is sampled is
// sampled, and the operator's OTEL_TRACES_SAMPLER decides only what happens at
// a root. A sampler that re-decided a trace already in flight would split its
// tree at the collector — sampled children under a parent that was never
// exported — and this engine's traces cross processes routinely. So the
// non-parent-based samplers the specification lists are refused at
// [Configure], by name, with the parent-based spelling to use instead. The
// root rule is also kept beside the provider (see [rootSampler]), replaced at
// the same commit, for the trace this engine roots WITHOUT a span — an event
// published outside one — which has no provider to ask.
//
// AND THE DECISION TRAVELS WITH THE TRACE. A context.Context does not survive
// a publish, so the decision rides beside the ids: [TraceOf] writes it into
// the envelope ([events.TraceContext.Unsampled], `trace_unsampled` on the
// wire), [WithRemote] restores it, and a sandbox run's row and the TRACEPARENT
// its box is handed ([Traceparent]) carry it, as does the TRACEPARENT a
// text-mode CLI call is handed for its round ([ProcessEnv]). Before it travelled, everything
// restored read as sampled — the queue's far side, a resumed run, a box — so
// a ratio of 0.1 decided the webhook's span and nothing after the first hop.
//
// # What is deliberately NOT spanned
//
// THE REST API'S OWN ROUTES. `/events`, `/config`, `/secrets` and the dashboard
// shell get no server span, and that is a decision rather than an omission.
// They are an operator surface, not a link in the causal chain a trace exists
// to show — the work they start reaches the trace through the events they
// publish, which already carry one. The webhook edge IS that first link, so it
// is spanned. So is a person's CALL through the operator catalogue — a
// `tool.call` of its own, opened by its one dispatch rather than by a route —
// because a call can make a model call, and the call is what the model call's
// spans, its spend and its audit record have to share.
//
// There is a hazard on that path worth writing down, because the obvious
// middleware placement walks straight into it: a tracing middleware sits
// OUTSIDE [github.com/crewlet/crewlet/internal/api/auth.Guard], and
// `/otlp/{token}/v1/traces` is exempt from the guard by prefix because THE
// TOKEN IN ITS PATH IS THE CREDENTIAL. Any middleware recording `http.route` or
// `http.target` as a span attribute would ship a live credential to the
// telemetry backend. If server spans are ever added, that route has to be
// excluded, or its path redacted before anything reads it.
//
// OUTBOUND HTTP. Every outbound client is built on the one transport
// [github.com/crewlet/crewlet/internal/httpx] holds, and that transport is not
// instrumented. A model round is spanned one layer up (`llm.round`), which is
// where the latency an operator can act on lives, and a third-party app call is
// made by a tool whose `tool.call` span covers it; a span per HTTP request
// beneath those would report the same wait again.
//
// THE ONE EXCEPTION IS THE VENDOR'S OWN, AND IT IS KEPT. The Anthropic Go SDK
// opens a CLIENT span around each call it makes (`anthropic.messages.create`),
// beneath `llm.round`, and sends that span's `traceparent` through the global
// propagator above. That span is the parent the Claude API's own SERVER spans
// name, so an organisation that points its Claude Console telemetry
// destination at the same backend sees the API's spans inside the engine's
// trace; and with the SDK's retries off it is one span per HTTP attempt, so a
// key the pool rotated off shows beside the key that answered. Its switches are
// pinned in code (internal/providers/llm/anthropic) rather than read from the
// SDK's ANTHROPIC_OPEN_TELEMETRY* variables, because tracing is the standard
// OTEL_* environment's alone, and it resolves the global provider on every
// call, so whatever [Configure] installed is what records it.
//
// EVERY MODEL CALL HAS A PARENT, because the vendor's span is only as useful
// as the span above it: a call made where no span was open made the SDK's span
// — and the Claude API's beneath it — the root of a trace nobody else was in,
// while the call's spend record minted a different one through [TraceOf]. So a
// call is made in a span of the engine's own whatever its caller holds:
// `llm.round` in a turn's tool loop, `agent.turn.judge` around the extension
// judge's call, and `llm.auxiliary` around every call on a seat's auxiliary
// chain, opened by the one seam every such call passes (internal/engine's
// auxiliary seam), which files the call's spend record under it. The callers that held no span hold one: a reflection pass restores its
// turn's trace and runs in `learning.reflect` beneath it, a background
// learning pass roots a trace of its own per tick ([StartRoot]), a person's
// call on the operator surface runs in a `tool.call` of its own, a coding
// box's call back over the tool bridge is filed in its run's trace — under the
// box's own span where its request names one in that trace
// ([WithRemoteFrom]) — and a collected coding run is fitted in its run's trace
// too, restored off the run's row before the coordinator collects it, so the
// condensation of a report too long to carry is an `llm.auxiliary` beneath the
// span that launched the run rather than a root per call. Outside a node, `crewlet llm doctor` — the one command
// that calls a model — runs [Configure] as `crewlet run` does and examines each
// entry in a root of its own (`llm.doctor`): left on the provider this
// package's init installs, it sent a sampled `traceparent` on every call while
// exporting nothing, so the Claude API's spans named a parent no backend had.
//
// A PROCESS THE ENGINE STARTS is handed its parent in its ENVIRONMENT —
// TRACEPARENT and TRACESTATE, the propagator's fields upper-cased
// ([ProcessEnv]) — by the same propagator that writes the header, so a coding
// box and a text-mode Claude Code call read the one encoding an outbound
// request carries. Such a process is handed trace context and never this
// package's exporter configuration: a vendor's CLI holding the collector's
// endpoint and headers would hold the company's ingest credential.
package tracing

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/otlpenv"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

var log = logging.Get("tracing")

// Environment variables this package reads beyond the OTLP destinations, which
// are [otlpenv]'s — endpoint, headers, protocol and the exporter switches,
// resolved there for the reason the package doc gives.
const (
	// SamplerVar names the sampler. Only the parent-based ones are accepted
	// — see [rootSamplerFor] for why the others are refused.
	SamplerVar = "OTEL_TRACES_SAMPLER"

	// SamplerArgVar is the head-sampling ratio, 0..1. Set ALONE it means
	// parentbased_traceidratio, which is what it meant before the sampler's
	// name was read at all.
	SamplerArgVar = "OTEL_TRACES_SAMPLER_ARG"

	// ServiceNameVar overrides the reported service, and wins over
	// OTEL_RESOURCE_ATTRIBUTES's service.name as the specification says.
	// An operator running two companies against one collector is the
	// reason it is worth setting at all.
	ServiceNameVar = "OTEL_SERVICE_NAME"

	// ResourceAttributesVar adds resource attributes, and wins over every
	// value this package supplies — see [resourceFor].
	ResourceAttributesVar = "OTEL_RESOURCE_ATTRIBUTES"
)

// The samplers [SamplerVar] may name. All three are parent-based, which is the
// point: see the package doc.
const (
	samplerAlwaysOn  = "parentbased_always_on"
	samplerAlwaysOff = "parentbased_always_off"
	samplerRatio     = "parentbased_traceidratio"
)

// defaultExportTimeout bounds one export to the collector when neither
// OTEL_EXPORTER_OTLP_<SIGNAL>_TIMEOUT nor OTEL_EXPORTER_OTLP_TIMEOUT says
// otherwise ([exportTimeout]).
//
// Tied to something real rather than picked: the specification's default is
// 10s and the SDK's batch processor's 30s, both longer than this engine's whole
// 5s API shutdown grace, so a collector that has stopped answering would hold
// the drain open through the final flush. 5s matches that grace — one export
// attempt fits inside the window the process already promises — and a
// collector slower than that is down, not busy.
const defaultExportTimeout = 5 * time.Second

// exportTimeout is the ONE bound an export of d is held to: the timeout the
// operator wrote, else [defaultExportTimeout].
//
// ONE NUMBER FOR EVERY LAYER AN EXPORT PASSES. The exporter bounds its own
// attempt, the batch span processor bounds the export it hands the exporter,
// and the periodic metric reader bounds the collect-and-export it runs — and
// each read a number of its own. This package once fixed the batch
// processor's at 5s while the SDK read OTEL_EXPORTER_OTLP_TIMEOUT for the
// exporter by itself, so a timeout above 5s did nothing, a timeout below it
// only shortened one layer, and nothing said so: the one setting read two ways
// that [otlpenv] exists to remove. Every layer is now handed this value, and
// the processors' own variables are refused ([refuseSuperseded]).
func exportTimeout(d otlpenv.Destination) time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return defaultExportTimeout
}

// flushGrace bounds the final flush at shutdown. Deliberately a little longer
// than one export attempt so a single in-flight batch can finish, and far
// short of anything a supervisor's SIGKILL timer would notice.
const flushGrace = 8 * time.Second

// rootRule is the sampler that decides a trace this process ROOTS.
//
// Boxed in a struct because the samplers share no concrete type and
// [atomic.Pointer] needs one — an [atomic.Value] would panic on the first
// Store of a different sampler than the one before it.
type rootRule struct{ sampler sdktrace.Sampler }

// root is the current [rootRule]. [Configure] replaces it at its commit,
// together with the TracerProvider whose ParentBased sampler wraps it, so the
// provider and a span-less root never answer the same trace differently.
var root atomic.Pointer[rootRule]

// rootSampler is the sampler that decides a trace this process roots.
//
// The provider's own sampler decides every root that opens a SPAN. This is for
// the root that opens none — an event published outside any span still
// carries a freshly minted trace — so that it is decided by the same rule.
// Every sampler [rootSamplerFor] builds decides on the trace id alone, so one
// id asked twice gets one answer.
func rootSampler() sdktrace.Sampler { return root.Load().sampler }

// providerSampler is the sampler every TracerProvider this package builds
// runs: parent-based, with [continued] around the root rule.
func providerSampler(rule sdktrace.Sampler) sdktrace.Sampler {
	return sdktrace.ParentBased(continued{rule: rule})
}

// continued is the provider's ROOT sampler — what ParentBased asks when the
// context holds no valid parent span — and it keeps one decision ParentBased
// cannot see.
//
// [WithRemote] given a trace id and NO SPAN AT ALL — neither one the event was
// published in nor one it was published beneath — restores the trace with a
// ZERO span id, because no span was ever opened for it: [TraceOf] minted the
// root where none was open, as for the wake a committed record causes through
// the change feed. The SDK keeps the trace id for the span opened beneath it,
// but a span context with no span id is not VALID, so ParentBased treats the
// span as a root and asks the root rule — which would RE-DECIDE a trace that
// is already in flight, the one thing every sampler here exists to refuse.
// Under a ratio that is only usually harmless (the rule is a function of the
// same trace id), and under anything else it is a split tree: a trace decided
// unsampled whose turn exports, or the reverse. So a context that is remote and
// names a trace keeps the decision it was restored with, and only a genuinely
// new trace reaches the rule.
//
// Every trace that names a span — the one its event was published in, or the
// one it was published beneath — is restored to that span, which ParentBased
// counts as a parent and follows on its own.
type continued struct{ rule sdktrace.Sampler }

func (c continued) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	psc := trace.SpanContextFromContext(p.ParentContext)
	if !psc.IsRemote() || !psc.TraceID().IsValid() || psc.TraceID() != p.TraceID {
		return c.rule.ShouldSample(p)
	}
	decision := sdktrace.Drop
	if psc.IsSampled() {
		decision = sdktrace.RecordAndSample
	}
	return sdktrace.SamplingResult{Decision: decision, Tracestate: psc.TraceState()}
}

// Description is the rule's own: what an operator chose is what the log line
// and a test asking which rule is in force should read.
func (c continued) Description() string { return c.rule.Description() }

// propagator is the one global propagator this package installs: W3C trace
// context and nothing else. The package doc says why Baggage is not beside it.
func propagator() propagation.TextMapPropagator { return propagation.TraceContext{} }

// A REAL PROVIDER FROM THE START, installed by this package's own init for
// exactly the reason internal/logging's init installs os.Stderr: the default
// must be the working one, not a state a caller has to remember to leave.
//
// This is not tidiness. OTel's built-in default TracerProvider is a no-op that
// returns a NON-RECORDING span carrying the PARENT's span context straight
// through — so with nothing installed, Start mints no new span id and
// [TraceOf] hands back the id of the span above it. That is precisely the bug
// this whole change removes (a turn publishing its trigger's span id as its
// own), and it would have come back silently in every process that did not
// call [Configure] — the e2e gate caught it doing exactly that.
//
// The default exports nothing and starts no goroutines: a TracerProvider with
// no span processor has nothing to flush and nothing to stop, so it is safe to
// install in a package init and safe never to shut down. [Configure] replaces
// it with the configured one.
func init() {
	otel.SetTextMapPropagator(propagator())
	root.Store(&rootRule{sampler: sdktrace.AlwaysSample()})
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSampler(providerSampler(rootSampler()))))
}

// Shutdown flushes buffered spans and releases the exporter.
//
// It is returned rather than registered, because the ONE process that owns a
// TracerProvider's lifetime is the one that built it — and a global that
// shuts itself down on a signal is the failure `Options.NoSigs` exists to
// prevent in the broker beside it.
type Shutdown func(context.Context) error

// Options are the few things the environment cannot say.
type Options struct {
	// NodeID identifies this node in a fleet. Empty is legal — a single
	// node has nothing to distinguish itself from.
	NodeID string

	// Version is the build's version string, reported as service.version.
	Version string

	// Env reads an environment variable. Nil means os.Getenv; a test passes
	// its own so it never has to mutate the process.
	//
	// It reads every variable this package or [otlpenv] resolves: the OTLP
	// destinations and the sampler. The two RESOURCE variables are the
	// exception — the SDK's own detector reads them, from the process
	// environment, because their parse is the SDK's and a copy of it here
	// would be one more reading of one setting.
	Env func(string) string

	// Recorder is the one place the engine's measurements are written, and
	// the MeterProvider installed here is a READER of it.
	//
	// Passed in rather than built here, because the engine records into it
	// long before and long after this package's provider exists — and two
	// recorders would be the drift this whole arrangement exists to
	// prevent. Nil builds one, so a caller that only wants traces is not
	// made to know about metrics.
	Recorder *metrics.Recorder
}

func (o Options) env(name string) string {
	if o.Env == nil {
		return strings.TrimSpace(os.Getenv(name))
	}
	return strings.TrimSpace(o.Env(name))
}

// destination is where this node's exporter sends one signal: [otlpenv]'s
// answer, and deliberately nothing of this package's own on top of it. The
// sandbox receiver asks the same question for the same signals, and
// TestTheExporterAndTheReceiverResolveOneDestination holds the two answers
// equal.
func (o Options) destination(s otlpenv.Signal) (otlpenv.Destination, error) {
	return otlpenv.Resolve(o.env, s)
}

// Configure builds the TracerProvider, installs it and the W3C propagator
// globally, and returns the flush the process must run before it exits.
//
// It never fails the boot for a telemetry reason it can survive: an
// unreachable collector is not detected here (the exporter connects lazily and
// retries), and an unparseable sampler ratio WARNS and falls back, on the same
// never-fail-on-a-typo rule the log level follows. It DOES return an error for
// a value the operator typed that can never work — an endpoint that is not an
// http(s) URL, a protocol or an exporter this build does not have, a sampler
// that is not parent-based — because silently exporting nowhere, or sampling
// by a rule nobody chose, is how a promise like this stops being true without
// anyone noticing. Each error names the variable to change.
//
// WHEN IT DOES RETURN AN ERROR IT CHANGES NOTHING. Both providers are built
// before either is installed, so a failure leaves the process tracing through
// the provider it already had — this package's init guarantees there is a
// working one — and leaves nothing running behind it. See the commit below.
func Configure(ctx context.Context, opts Options) (Shutdown, error) {
	// The propagator is installed whether or not anything exports: it is
	// what makes an inbound `traceparent` join an existing trace, and what
	// the sandbox receiver's stamped TRACEPARENT is read back with.
	otel.SetTextMapPropagator(propagator())

	// The SDK reports its own failures through a global handler. Left
	// unset it writes to the standard logger, which in this process is the
	// one place a line bypasses the engine's own format.
	//
	// Installed HERE rather than with the providers, and deliberately NOT
	// part of the commit below: it owns nothing and only decides where the
	// SDK's own error lines go, so a Configure that fails half way is
	// better off having installed it than not — the SDK reports some
	// construction-time failures through it, including the ones raised
	// while the providers this function goes on to abandon are being built.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		// NOT WarnContext: this handler is installed GLOBALLY and outlives
		// Configure, so capturing its ctx would pin a request-scoped value
		// into a process-lifetime closure, which is the shape context
		// threading exists to refuse.
		log.Warn("otel_internal_error", "error", err)
	}))

	if err := refuseSuperseded(opts); err != nil {
		return nil, err
	}
	reportUnhonoured(ctx, opts)
	sampler, err := rootSamplerFor(opts)
	if err != nil {
		return nil, err
	}
	traces, err := opts.destination(otlpenv.Traces)
	if err != nil {
		return nil, err
	}
	res, err := resourceFor(ctx, opts)
	if err != nil {
		return nil, err
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(providerSampler(sampler)),
	}

	if traces.Exports() {
		var exporter sdktrace.SpanExporter
		if exporter, err = newExporter(ctx, traces); err != nil {
			return nil, err
		}
		// A BATCH processor, not a simple one: a simple processor exports
		// on the goroutine that ended the span, which puts a collector's
		// latency inside a turn.
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exporter,
			sdktrace.WithExportTimeout(exportTimeout(traces))))
	}

	// BUILT HERE, INSTALLED AT THE COMMIT BELOW, because everything between
	// the two can still fail.
	//
	// A provider installed before the metrics half runs is one this
	// function has already published to every span site in the process by
	// the time it hands back an error — and the caller, holding no
	// [Shutdown], has no way to flush or stop it. The batch processor's
	// goroutine and the OTLP exporter's connection start at CONSTRUCTION
	// rather than at installation, so that is a live exporter belonging to
	// a boot that failed.
	//
	// Installing first and putting the previous provider back on the error
	// path is the other repair, and it is worse: what it would restore is
	// whatever the last Configure's [Shutdown] already terminated, and a
	// terminated sdktrace.TracerProvider hands out NO-OP tracers — which
	// pass the parent's span context straight through, which is exactly the
	// bug this package's own init exists to keep out of the process (see
	// there). Never installing leaves the working provider that is already
	// there and leaves no window in which the global is something
	// mid-teardown.
	tp := sdktrace.NewTracerProvider(tpOpts...)

	// THE METER PROVIDER, on the same terms and for the same reason: it is
	// installed whether or not anything exports, so nothing above this
	// package branches on whether metrics are "on" and the operator record
	// reads the same recorder a collector would.
	//
	// A failure here does NOT fail the boot beyond what the traces half
	// already refuses: the recorder still answers, so what is lost is the
	// export rather than the measurement.
	rec := opts.Recorder
	if rec == nil {
		if rec, err = metrics.New(); err != nil {
			return nil, abandon(ctx, tp, err)
		}
	}
	metricsAt, err := opts.destination(otlpenv.Metrics)
	if err != nil {
		return nil, abandon(ctx, tp, err)
	}
	mp, err := configureMeter(ctx, opts, res, metricsAt, rec)
	if err != nil {
		return nil, abandon(ctx, tp, err)
	}

	// THE COMMIT. Everything that can fail has succeeded, so the globals
	// change together and nothing below here may be fallible: a step added
	// after this line would reopen exactly the window the build-then-install
	// order closes. The root rule moves with the provider whose sampler
	// wraps it, so a span-less root is decided by the rule the provider
	// decides a span's root by.
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	root.Store(&rootRule{sampler: sampler})

	// Logged AFTER the commit, so the lines are a record of what is running
	// rather than an announcement the next statement can still retract.
	// The endpoint is printed as resolved: [otlpenv] refuses one carrying
	// userinfo or a query, the two places a URL holds a credential.
	otlpenv.ReportDropped(ctx, log, otlpenv.ReaderExporter, traces, metricsAt)
	reportTimeouts(ctx, traces, metricsAt)
	log.InfoContext(ctx, "tracing_configured",
		"exporting", traces.Exports(), "declined", traces.Declined,
		"endpoint", traces.Endpoint, "protocol", traces.Protocol,
		"timeout", exportTimeout(traces),
		"service", serviceName(res), "sampler", sampler.Description())

	return func(ctx context.Context) error {
		// BOTH, and the traces error wins when both fail: it is the one
		// with spans in flight, and a joined error here would be two
		// telemetry failures where a caller wants one line.
		metricsErr := shutdownMeter(ctx, mp, metricsAt)
		if err := shutdownTracer(ctx, tp); err != nil {
			return err
		}
		return metricsErr
	}, nil
}

// abandon releases a provider whose Configure did not finish, and hands the
// cause back unchanged.
//
// Nothing is installed until [Configure] commits, so there is no global to put
// back — but a built provider is already RUNNING: the batch processor's
// goroutine and the exporter's connection start when the provider is
// constructed, not when it is installed. Dropping the pointer on an error path
// therefore leaves an exporter alive, on a timer, inside a process that holds
// no handle to flush or stop it.
//
// The cause is returned alone. What an operator has to act on is the value
// they typed; a flush that failed while stopping a provider which recorded
// nothing and which nobody will ever read is not a second thing to fix, and
// joining the two would put two telemetry errors in front of the one that
// matters. It is logged rather than dropped, because a teardown that fails in
// silence is how a leak stops being visible.
func abandon(ctx context.Context, tp *sdktrace.TracerProvider, cause error) error {
	if err := shutdownTracer(ctx, tp); err != nil {
		log.WarnContext(ctx, "trace_provider_abandoned_unflushed", "error", err)
	}
	return cause
}

// shutdownTracer flushes and stops, bounded.
//
// The context it is GIVEN is normally already cancelled — the cancellation is
// what woke the drain — so it takes its own deadline off a detached copy, the
// rule CLAUDE.md states for every teardown: "the failure it is undoing is
// often the cancellation itself, and a cleanup that inherits a dead context
// does nothing at all". Without this, the final flush would return instantly
// and drop every span the drain itself produced.
func shutdownTracer(ctx context.Context, tp *sdktrace.TracerProvider) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushGrace)
	defer cancel()
	if err := tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("flush traces: %w", err)
	}
	return nil
}

// newExporter builds the span exporter for a resolved destination.
//
// The endpoint, the headers, the compression, the TLS material and the
// timeout are passed EXPLICITLY, although the SDK would read the same variables itself: an
// explicit option replaces what the SDK read, so the exporter sends exactly
// what [otlpenv] resolved — which is what the sandbox receiver forwards with —
// rather than the SDK's own reading of the environment, which is the second
// opinion the resolver exists to remove. See [exporterOptions].
func newExporter(ctx context.Context, d otlpenv.Destination) (sdktrace.SpanExporter, error) {
	opts, err := exporterOptions(d)
	if err != nil {
		return nil, err
	}
	// An error names the endpoint's VARIABLE and never its value, which is
	// where a misplaced credential sits — otlpenv's rule for every message
	// about an endpoint.
	switch d.Protocol {
	case otlpenv.GRPC:
		exp, err := otlptracegrpc.New(ctx, opts.traceGRPC()...)
		if err != nil {
			return nil, fmt.Errorf("otlp grpc exporter for %s: %w", d.EndpointVar, err)
		}
		return exp, nil
	case otlpenv.HTTPProtobuf:
		exp, err := otlptracehttp.New(ctx, opts.traceHTTP()...)
		if err != nil {
			return nil, fmt.Errorf("otlp http exporter for %s: %w", d.EndpointVar, err)
		}
		return exp, nil
	}
	// [otlpenv.Resolve] refuses any other protocol, so reaching here is a
	// protocol added to the closed set with no exporter built for it.
	return nil, fmt.Errorf("tracing: no span exporter is built for protocol %q", d.Protocol)
}

// exporterSettings is one resolved destination as the four OTLP exporters'
// options spell it — the one translation, so a span exporter and a metric
// exporter never differ in what they took from the same resolution.
type exporterSettings struct {
	d   otlpenv.Destination
	tls *tls.Config
}

// exporterOptions reads the destination's TLS material once, for whichever
// exporter is built from it.
//
// NIL TLS MEANS THE SDK'S OWN DEFAULT: the scheme decides, and an https
// endpoint is verified against the system's roots. A certificate the
// environment names is therefore never left to the SDK's own reading — that
// reading reports a file it cannot use and exports without it, where
// [otlpenv.Resolve] refused the file by name before anything got this far.
func exporterOptions(d otlpenv.Destination) (exporterSettings, error) {
	cfg, err := d.Config()
	if err != nil {
		return exporterSettings{}, fmt.Errorf("tracing: %w", err)
	}
	return exporterSettings{d: d, tls: cfg}, nil
}

// gzip reports whether the destination compresses.
//
// A gRPC exporter is told only "gzip": its option takes no other value without
// reporting one as invalid, and its default is uncompressed — which is what
// [otlpenv] resolved whenever it did not resolve gzip.
func (e exporterSettings) gzip() bool { return e.d.Compression == otlpenv.Gzip }

func (e exporterSettings) traceGRPC() []otlptracegrpc.Option {
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpointURL(e.d.Endpoint),
		otlptracegrpc.WithHeaders(e.d.Headers),
		otlptracegrpc.WithTimeout(exportTimeout(e.d)),
	}
	if e.gzip() {
		opts = append(opts, otlptracegrpc.WithCompressor(string(otlpenv.Gzip)))
	}
	if e.tls != nil {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(e.tls)))
	}
	return opts
}

func (e exporterSettings) traceHTTP() []otlptracehttp.Option {
	compression := otlptracehttp.NoCompression
	if e.gzip() {
		compression = otlptracehttp.GzipCompression
	}
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(e.d.Endpoint),
		otlptracehttp.WithHeaders(e.d.Headers),
		otlptracehttp.WithCompression(compression),
		otlptracehttp.WithTimeout(exportTimeout(e.d)),
	}
	if e.tls != nil {
		opts = append(opts, otlptracehttp.WithTLSClientConfig(e.tls))
	}
	return opts
}

func (e exporterSettings) metricGRPC() []otlpmetricgrpc.Option {
	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpointURL(e.d.Endpoint),
		otlpmetricgrpc.WithHeaders(e.d.Headers),
		otlpmetricgrpc.WithTimeout(exportTimeout(e.d)),
	}
	if e.gzip() {
		opts = append(opts, otlpmetricgrpc.WithCompressor(string(otlpenv.Gzip)))
	}
	if e.tls != nil {
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(credentials.NewTLS(e.tls)))
	}
	return opts
}

func (e exporterSettings) metricHTTP() []otlpmetrichttp.Option {
	compression := otlpmetrichttp.NoCompression
	if e.gzip() {
		compression = otlpmetrichttp.GzipCompression
	}
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(e.d.Endpoint),
		otlpmetrichttp.WithHeaders(e.d.Headers),
		otlpmetrichttp.WithCompression(compression),
		otlpmetrichttp.WithTimeout(exportTimeout(e.d)),
	}
	if e.tls != nil {
		opts = append(opts, otlpmetrichttp.WithTLSClientConfig(e.tls))
	}
	return opts
}

// serviceName is the service.name the resource settled on, for the log line —
// read back rather than recomputed, because which of three sources won is the
// resource's decision.
func serviceName(res *resource.Resource) string {
	v, _ := res.Set().Value(semconv.ServiceNameKey)
	return v.AsString()
}

// resourceFor builds the resource every span and metric is reported under.
//
// # The operator's attributes win
//
// This package supplies three values — service.name `crewlet`,
// service.version, and service.instance.id from the node id, which is exactly
// what "which copy of this service" means and without which a fleet's spans
// are indistinguishable at the collector. They are FALLBACKS: an operator who
// set OTEL_SERVICE_NAME or wrote any of them into OTEL_RESOURCE_ATTRIBUTES has
// said what the collector should see, so the environment is merged LAST and
// wins. Merged the other way round, as this once was, `crewlet` overwrote the
// service name in OTEL_RESOURCE_ATTRIBUTES — the variable the comment beside
// it promised kept working.
//
// The SDK's own detector reads the environment, with its own precedence
// between the two variables (OTEL_SERVICE_NAME wins, as the specification
// says) and its own parse, so there is no second reading of either here. The
// result carries the telemetry.sdk.* attributes and nothing about the host or
// the process — the SDK's default resource has never detected those, whatever
// this comment once said.
//
// An OTEL_RESOURCE_ATTRIBUTES entry with no `=` is WARNED and the rest kept,
// as the SDK keeps it, on the never-fail-on-a-typo rule. The semconv version
// MUST match the SDK's own (sdk/resource/builtin.go): a resource merged from
// two schema URLs is refused outright, which fails Configure and takes
// tracing out entirely.
func resourceFor(ctx context.Context, opts Options) (*resource.Resource, error) {
	fallbacks := []attribute.KeyValue{semconv.ServiceName("crewlet")}
	if opts.Version != "" {
		fallbacks = append(fallbacks, semconv.ServiceVersion(opts.Version))
	}
	if opts.NodeID != "" {
		fallbacks = append(fallbacks, semconv.ServiceInstanceID(opts.NodeID))
	}
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(fallbacks...),
		// LAST, so what the operator wrote wins over every fallback above.
		resource.WithFromEnv())
	if err != nil {
		if !errors.Is(err, resource.ErrPartialResource) ||
			errors.Is(err, resource.ErrSchemaURLConflict) {
			return nil, fmt.Errorf("tracing: building the OTel resource: %w", err)
		}
		log.WarnContext(ctx, "otel_resource_attributes_partial",
			"var", ResourceAttributesVar, "error", err,
			"detail", "an entry with no '=' is skipped; write each as key=value")
	}
	return res, nil
}

// rootSamplerFor reads the operator's sampler: the rule for a trace this node
// ROOTS. The provider wraps it in ParentBased, so a trace that arrived with a
// decision keeps it.
//
// # Only the parent-based samplers are accepted
//
// The specification's always_on, always_off and traceidratio re-decide at
// every span, and a node that re-decided a trace already in flight would split
// its tree at the collector — an unsampled parent with sampled children, or
// the reverse. The parent-based spelling of each is what an operator asking
// for one of those almost always means, so the refusal names it rather than
// silently substituting it. jaeger_remote and the rest are samplers this build
// does not have.
//
// # A bad ratio does not stop a company
//
// An unparseable or out-of-range ratio WARNS and falls back to sampling
// everything rather than refusing to boot — the same rule `-log-level` follows,
// and for the same reason: a typo in a telemetry knob must never be why a
// company will not start. Falling back to always-on rather than off is
// deliberate: the failure an operator can see is better than the one they
// cannot. A sampler NAME this build does not have is the other kind of value —
// it says something about every trace, and there is no fallback that is not a
// guess about which.
func rootSamplerFor(opts Options) (sdktrace.Sampler, error) {
	name, arg := opts.env(SamplerVar), opts.env(SamplerArgVar)
	switch strings.ToLower(name) {
	case "":
		if arg == "" {
			return sdktrace.AlwaysSample(), nil
		}
		return ratioSampler(arg), nil
	case samplerAlwaysOn:
		ignoredArg(name, arg)
		return sdktrace.AlwaysSample(), nil
	case samplerAlwaysOff:
		ignoredArg(name, arg)
		return sdktrace.NeverSample(), nil
	case samplerRatio:
		if arg == "" {
			// The specification's default ratio for this sampler is 1:
			// every trace, which is what always-on says in fewer words.
			return sdktrace.AlwaysSample(), nil
		}
		return ratioSampler(arg), nil
	case "always_on", "always_off", "traceidratio":
		return nil, fmt.Errorf("%s=%s is not parent-based, and this engine's "+
			"sampler always is: its traces cross processes, and a node that "+
			"re-decided a trace already in flight would split its tree at the "+
			"collector. Set %s=parentbased_%s — a trace this node starts is "+
			"decided by %s, and one it continues keeps its parent's decision",
			SamplerVar, name, SamplerVar, strings.ToLower(name), strings.ToLower(name))
	}
	return nil, fmt.Errorf("%s=%q is not a sampler this build has: set it to "+
		"%s, %s or %s (with %s), or leave it unset to sample every trace",
		SamplerVar, name, samplerAlwaysOn, samplerAlwaysOff, samplerRatio, SamplerArgVar)
}

// ratioSampler reads a ratio, warning and sampling everything on a bad one.
func ratioSampler(raw string) sdktrace.Sampler {
	ratio, err := strconv.ParseFloat(raw, 64)
	switch {
	case err != nil:
		log.Warn("trace_sampler_arg_unparseable", "var", SamplerArgVar,
			"value", raw, "using", "always")
		return sdktrace.AlwaysSample()
	case !(ratio >= 0 && ratio <= 1):
		// Written so NaN fails it: ParseFloat accepts "NaN", every
		// comparison with it is false, and TraceIDRatioBased would turn
		// it into a threshold no conversion defines.
		log.Warn("trace_sampler_arg_out_of_range", "var", SamplerArgVar,
			"value", raw, "range", "0..1", "using", "always")
		return sdktrace.AlwaysSample()
	}
	return sdktrace.TraceIDRatioBased(ratio)
}

// ignoredArg warns that a ratio was given to a sampler that takes none, which
// the specification says is ignored — and an ignored setting an operator
// believes is in force is worth one line.
func ignoredArg(sampler, arg string) {
	if arg != "" {
		log.Warn("trace_sampler_arg_ignored", "var", SamplerArgVar, "value", arg,
			"sampler", sampler, "detail", "only parentbased_traceidratio takes a ratio")
	}
}

// Tracer names one instrumentation scope. Every span in the engine comes from
// here so the collector's scope names match the component names in the logs.
func Tracer(component string) trace.Tracer {
	return otel.Tracer("github.com/crewlet/crewlet/" + component)
}
