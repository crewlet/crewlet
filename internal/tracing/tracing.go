// Package tracing is the engine's OpenTelemetry wiring: one TracerProvider,
// built from the environment, installed globally, and flushed on the way out.
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
// # Why the environment configures it and Tier A does not
//
// The same call `internal/sandbox/otel.go` already made and wrote down: the
// OTLP endpoint and headers are "the standard OTel spelling every collector's
// own documentation uses. An operator wiring a collector should not have to
// translate it into a Crewlet-shaped block." That receiver reads
// OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_HEADERS to find the real
// backend, and this exporter reads THE SAME PAIR on purpose: a company has one
// telemetry backend, and the whole point of the trace context the receiver
// stamps into a sandbox is that the box's spans nest under the engine's. Two
// settings would let an operator split them across two backends, where the
// parent/child link resolves on neither.
//
// A second reason, specific to this engine: Tier A is the root of trust and
// resolves with EnvOnly, and d-001 retired `debug:` on the rule that two keys
// setting one value is a state where they disagree. A `tracing.endpoint:`
// field beside OTEL_EXPORTER_OTLP_ENDPOINT would be exactly that.
package tracing

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
)

var log = logging.Get("tracing")

// Environment variables this package reads. The OTLP pair is shared with
// [sandbox.OtelReceiver] by design — see the package doc — and is named there
// so the two cannot drift apart.
const (
	// EndpointVar is the OTLP base endpoint. Unset means spans are created
	// and not exported, which is the ordinary single-node configuration.
	EndpointVar = sandbox.OtelUpstreamEndpointVar

	// HeadersVar carries the backend's ingest credential, in the standard
	// `k=v,k2=v2` form.
	HeadersVar = sandbox.OtelUpstreamHeadersVar

	// ProtocolVar selects the OTLP transport. The spec's default is
	// http/protobuf. This was DOCUMENTED as read long before anything read
	// it — see decisions/508.
	ProtocolVar = "OTEL_EXPORTER_OTLP_PROTOCOL"

	// ServiceNameVar overrides the reported service. Defaults to "crewlet";
	// an operator running two companies against one collector is the reason
	// it is settable at all.
	ServiceNameVar = "OTEL_SERVICE_NAME"

	// SamplerArgVar is the head-sampling ratio, 0..1. The sampler itself is
	// always parent-based — see [sampler].
	SamplerArgVar = "OTEL_TRACES_SAMPLER_ARG"

	// TracesEndpointVar is the SIGNAL-SPECIFIC endpoint, used verbatim when
	// set. The spec gives the two variables different meanings and this is
	// the half that already names a path — see [tracesEndpoint].
	TracesEndpointVar = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
)

// tracesEndpoint resolves the URL spans are POSTed to.
//
// # The two variables mean different things, and conflating them doubles the path
//
// The OTLP spec is explicit: OTEL_EXPORTER_OTLP_ENDPOINT is a BASE and the
// exporter appends the signal path to it, while
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is the full URL and is used as given.
// The SDK's WithEndpointURL implements the second meaning only — handed a
// path-less base it targets "/" rather than "/v1/traces" — so the base has to
// be completed here.
//
// This is the same rule [sandbox.OtelReceiver] already follows when it
// forwards (it appends "/v1/"+signal to the trimmed upstream), which is what
// keeps one setting correct for both halves. It is also why the deployment
// guide's example had to change: it told operators to set the BASE variable
// to a URL ending in /v1/traces, which the receiver turned into
// /v1/traces/v1/traces at the collector.
func tracesEndpoint(opts Options) string {
	if signal := opts.env(TracesEndpointVar); signal != "" {
		return signal
	}
	base := strings.TrimRight(opts.env(EndpointVar), "/")
	if base == "" {
		return ""
	}
	return base + "/v1/traces"
}

// The protocols this build can speak, and the closed set a typo is checked
// against so an unknown value names the two that work.
const (
	ProtocolHTTP = "http/protobuf"
	ProtocolGRPC = "grpc"
)

// Protocols is the closed set, shared with the validator so an operator's
// typo is told what it could have been.
var Protocols = []string{ProtocolHTTP, ProtocolGRPC}

// exportTimeout bounds one batch's delivery to the collector.
//
// Tied to something real rather than picked: the SDK's own default is 30s,
// which is longer than this engine's whole 5s API shutdown grace, so a
// collector that has stopped answering would hold the drain open through the
// final flush. 5s matches that grace — one export attempt fits inside the
// window the process already promises — and a collector slower than that is
// down, not busy.
const exportTimeout = 5 * time.Second

// flushGrace bounds the final flush at shutdown. Deliberately a little longer
// than one export attempt so a single in-flight batch can finish, and far
// short of anything a supervisor's SIGKILL timer would notice.
const flushGrace = 8 * time.Second

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
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample()))))
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
	Env func(string) string
}

func (o Options) env(name string) string {
	if o.Env == nil {
		return strings.TrimSpace(os.Getenv(name))
	}
	return strings.TrimSpace(o.Env(name))
}

// Configure builds the TracerProvider, installs it and the W3C propagator
// globally, and returns the flush the process must run before it exits.
//
// It never fails the boot for a telemetry reason it can survive: an
// unreachable collector is not detected here (the exporter connects lazily and
// retries), and an unparseable sampler ratio WARNS and falls back, on the same
// never-fail-on-a-typo rule the log level follows. It DOES return an error for
// a malformed endpoint, because that is a value the operator typed that can
// never work, and silently exporting nowhere is how a promise like this stops
// being true without anyone noticing.
func Configure(ctx context.Context, opts Options) (Shutdown, error) {
	// The propagator is installed whether or not anything exports: it is
	// what makes an inbound `traceparent` join an existing trace, and what
	// the sandbox receiver's stamped TRACEPARENT is read back with.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	res, err := resourceFor(opts)
	if err != nil {
		return nil, err
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler(opts)),
	}

	endpoint := tracesEndpoint(opts)
	var exporter sdktrace.SpanExporter
	if endpoint != "" {
		if exporter, err = newExporter(ctx, opts, endpoint); err != nil {
			return nil, err
		}
		// A BATCH processor, not a simple one: a simple processor exports
		// on the goroutine that ended the span, which puts a collector's
		// latency inside a turn.
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exporter,
			sdktrace.WithExportTimeout(exportTimeout)))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)

	// The SDK reports its own failures through a global handler. Left
	// unset it writes to the standard logger, which in this process is the
	// one place a line bypasses the engine's own format.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		// NOT WarnContext: this handler is installed GLOBALLY and outlives
		// Configure, so capturing its ctx would pin a request-scoped value
		// into a process-lifetime closure — the shape d-401 exists to refuse.
		log.Warn("otel_internal_error", "error", err)
	}))

	log.InfoContext(ctx, "tracing_configured",
		"exporting", endpoint != "", "endpoint", endpoint,
		"protocol", protocol(opts), "service", serviceName(opts))

	return func(ctx context.Context) error { return shutdown(ctx, tp) }, nil
}

// shutdown flushes and stops, bounded.
//
// The context it is GIVEN is normally already cancelled — the cancellation is
// what woke the drain — so it takes its own deadline off a detached copy, the
// rule CLAUDE.md states for every teardown: "the failure it is undoing is
// often the cancellation itself, and a cleanup that inherits a dead context
// does nothing at all". Without this, the final flush would return instantly
// and drop every span the drain itself produced.
func shutdown(ctx context.Context, tp *sdktrace.TracerProvider) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushGrace)
	defer cancel()
	if err := tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("flush traces: %w", err)
	}
	return nil
}

func newExporter(ctx context.Context, opts Options, endpoint string) (sdktrace.SpanExporter, error) {
	headers := sandbox.ParseOtelHeaders(opts.env(HeadersVar))

	switch p := protocol(opts); p {
	case ProtocolGRPC:
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpointURL(endpoint),
			otlptracegrpc.WithHeaders(headers))
		if err != nil {
			return nil, fmt.Errorf("otlp grpc exporter for %s=%q: %w", EndpointVar, endpoint, err)
		}
		return exp, nil
	case ProtocolHTTP:
		exp, err := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(endpoint),
			otlptracehttp.WithHeaders(headers))
		if err != nil {
			return nil, fmt.Errorf("otlp http exporter for %s=%q: %w", EndpointVar, endpoint, err)
		}
		return exp, nil
	default:
		return nil, fmt.Errorf("%s=%q is not a protocol this build speaks; use one of %s",
			ProtocolVar, p, strings.Join(Protocols, ", "))
	}
}

// protocol resolves the transport, defaulting to the spec's own default.
func protocol(opts Options) string {
	if p := opts.env(ProtocolVar); p != "" {
		return p
	}
	return ProtocolHTTP
}

func serviceName(opts Options) string {
	if n := opts.env(ServiceNameVar); n != "" {
		return n
	}
	return "crewlet"
}

func resourceFor(opts Options) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(serviceName(opts))}
	if opts.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(opts.Version))
	}
	if opts.NodeID != "" {
		// service.instance.id is the semconv spelling of "which copy of
		// this service", which is exactly what a node id is. A fleet's
		// spans are otherwise indistinguishable at the collector.
		attrs = append(attrs, semconv.ServiceInstanceID(opts.NodeID))
	}
	// Merged onto the SDK's default resource so the process, host and SDK
	// attributes an operator expects are present without restating them,
	// and so OTEL_RESOURCE_ATTRIBUTES keeps working.
	//
	// The semconv version MUST match the one resource.Default() was built
	// with (sdk/resource/builtin.go). Merge refuses two different schema
	// URLs outright, so a mismatch is not a subtle attribute difference —
	// it fails Configure and takes tracing out entirely.
	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...))
	if err != nil {
		return nil, fmt.Errorf("tracing: building the OTel resource: %w", err)
	}
	return res, nil
}

// sampler is ALWAYS parent-based, and the ratio only decides what happens at a
// root.
//
// A trace that starts at a webhook and continues through a queue must not be
// half-sampled: an unsampled parent with sampled children is a broken tree at
// the collector, and this engine's traces cross processes routinely. So a
// remote decision is always honoured and the ratio governs the roots this node
// mints.
//
// An unparseable or out-of-range ratio WARNS and falls back to always-on,
// rather than refusing to boot — the same rule `-log-level` follows, and for
// the same reason: a typo in a telemetry knob must never be why a company will
// not start. Falling back to always-on rather than off is deliberate: the
// failure an operator can see is better than the one they cannot.
func sampler(opts Options) sdktrace.Sampler {
	raw := opts.env(SamplerArgVar)
	if raw == "" {
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	switch {
	case err != nil:
		log.Warn("trace_sampler_arg_unparseable", "var", SamplerArgVar,
			"value", raw, "using", "always")
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case ratio < 0 || ratio > 1:
		log.Warn("trace_sampler_arg_out_of_range", "var", SamplerArgVar,
			"value", raw, "range", "0..1", "using", "always")
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

// Tracer names one instrumentation scope. Every span in the engine comes from
// here so the collector's scope names match the component names in the logs.
func Tracer(component string) trace.Tracer {
	return otel.Tracer("github.com/crewlet/crewlet/" + component)
}
