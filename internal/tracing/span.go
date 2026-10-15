package tracing

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/textcut"
)

// The bridge between OTel's carrier and this engine's.
//
// # Two representations of one fact, and why both stay
//
// OTel carries a span in `context.Context`; Crewlet carries `trace_id` /
// `span_id` / `parent_span_id` — and the sampling decision, `trace_unsampled`
// — in the event envelope, which is what reaches `crewlet_events`,
// `GET /events/trace/{id}` and the dashboard's trace tree. Neither can replace
// the other: a `context.Context` does not survive a JetStream publish, and
// three hex strings do not carry a live span. So this file is the ONE place
// they are converted, in both directions. Copying a trace from one event, or
// one run row, to the next is the envelope's own business —
// [events.Event.Trace] and [events.TraceContext.Beneath] — so a field added to
// the trace is carried by every hop or by none.
//
// # Why a span is allowed in context.Context at all
//
// Almost nothing in this engine travels ambiently: a turn's inputs are
// arguments, because a goroutine SHARES whatever context it captured, forever,
// including after the turn that created it has finished. A live span is
// admitted anyway, on the three tests anything ambient here has to pass — it
// is IMMUTABLE, its consumer is a LEAF (the tracer), and when it is absent it
// degrades to a no-op span rather than to wrong behaviour.
//
// There is also no alternative. OTel has no other carrier, and putting a live
// span on `TurnContext` would be exactly the "a goroutine captures turn state
// and outlives the turn" bug that the argument-passing rule exists to prevent.

// parentKey carries the id of the span that is the CURRENT span's parent.
//
// OTel's SpanContext deliberately does not expose a parent — the collector
// reconstructs the tree from the span records themselves. Crewlet's event
// envelope has a `parent_span_id` column that predates any of this and that
// the dashboard's tree reads directly, so the parent has to be remembered
// alongside. [Start] writes it for a span it opens and [WithRemote] for a
// span it restores, and nothing else writes it.
type parentKey struct{}

// Start opens a span and makes its ids available to everything below it — the
// tracer, and every log line that passes a context.
//
// One call does both because two calls is one call that gets forgotten: the
// engine's log lines and its spans are read together during an incident, and a
// span whose ids never reached the logs is exactly the correlation this whole
// change exists to provide.
//
// The returned context carries the span, its parent id, and the logging
// binding. Callers MUST end the span; the ordinary shape is:
//
//	ctx, span := tracing.Start(ctx, "engine", "agent.turn")
//	defer span.End()
func Start(ctx context.Context, component, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	parent := trace.SpanContextFromContext(ctx).SpanID()

	ctx, span := Tracer(component).Start(ctx, name, trace.WithAttributes(attrs...))

	sc := span.SpanContext()
	if parent.IsValid() {
		ctx = context.WithValue(ctx, parentKey{}, parent.String())
	} else {
		// A root span has no parent, and leaving a stale one on the
		// context would make every root claim its caller's span.
		ctx = context.WithValue(ctx, parentKey{}, "")
	}
	if sc.IsValid() {
		ctx = logging.WithTrace(ctx, sc.TraceID().String(), sc.SpanID().String())
	}
	return ctx, span
}

// StartRoot is [Start] for work NOTHING CAUSED but a clock — a background pass
// — which must root a trace of its own whatever ctx holds: a span left on a
// long-lived loop's context by whatever armed it would otherwise make every
// tick of a company-wide sweep a child of one unrelated piece of work.
func StartRoot(ctx context.Context, component, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Start(trace.ContextWithSpanContext(ctx, trace.SpanContext{}), component, name, attrs...)
}

// TraceOf renders the active span as the envelope's fields, so an event
// published from inside a span carries the ids that span will export under —
// and its SAMPLING DECISION, so whatever restores the trace on the far side of
// the queue continues it as decided rather than re-deciding it.
//
// # A fresh root when there is no span
//
// Rather than empty strings. Every one of this engine's event publishers used
// to call [events.NewTrace] for exactly this case, and an event with no trace
// id is one the store cannot group and the dashboard cannot place. So the
// no-span answer is the same answer it always was — a freshly minted root —
// and the call sites simply stopped having to know which case they were in.
//
// A minted root is DECIDED like any other root: by the operator's root rule
// ([rootSampler]), asked about the minted trace id. Every rule it can hold
// decides on the trace id alone, so the provider deciding a span-rooted trace
// and this deciding a span-less one cannot disagree about the same id, and a
// ratio of 0.1 governs the traces a schedule's span roots and the ones an
// event published outside any span roots alike.
//
// # A restored trace with no span keeps its trace
//
// [WithRemote] given a trace id and no span id — an event published beneath a
// span by code that opens none, such as a chat wake — restores the trace with
// no parent span. That context holds no VALID span, and minting there would
// file the event under a trace nobody else is in, so it reports the restored
// trace and decision back unchanged instead.
func TraceOf(ctx context.Context) events.TraceContext {
	sc := trace.SpanContextFromContext(ctx)
	parent, _ := ctx.Value(parentKey{}).(string)
	switch {
	case sc.IsValid():
		return events.TraceContext{
			TraceID:      sc.TraceID().String(),
			SpanID:       sc.SpanID().String(),
			ParentSpanID: parent,
			Unsampled:    !sc.IsSampled(),
		}
	case sc.IsRemote() && sc.TraceID().IsValid():
		return events.TraceContext{
			TraceID:      sc.TraceID().String(),
			ParentSpanID: parent,
			Unsampled:    !sc.IsSampled(),
		}
	}
	tc := events.NewTrace()
	tc.Unsampled = !sampledRoot(ctx, tc.TraceID)
	return tc
}

// sampledRoot asks the root rule about a trace this process roots without a
// span. The id is one [events.NewTrace] just minted, so it always parses; a
// parse failure would be a minting bug, and sampling it is the failure an
// operator can see.
func sampledRoot(ctx context.Context, hex string) bool {
	id, err := trace.TraceIDFromHex(hex)
	if err != nil {
		return true
	}
	return rootSampler().ShouldSample(sdktrace.SamplingParameters{
		ParentContext: ctx, TraceID: id,
	}).Decision == sdktrace.RecordAndSample
}

// WithRemote restores a trace that arrived from somewhere else — an event off
// the queue, a resumed sandbox run, an inbound `traceparent` — so the spans
// opened beneath it become children of the span that caused them rather than
// new roots.
//
// # The decision it arrived with is the one it keeps
//
// The restored context is marked REMOTE and carries the trace's own sampling
// decision ([events.TraceContext.Unsampled]), and the provider's sampler is
// parent-based, so a span opened beneath it is sampled exactly when the trace
// it continues was: a node that re-decided a trace already in flight is how a
// tree ends up with children under a parent nobody exported, or with half its
// spans missing. A trace id with NO span id is a context the SDK does not treat
// as a parent at all — see [continued], the root sampler that keeps its
// decision anyway.
//
// It is also the decision a process this engine starts is handed: the
// TRACEPARENT a coding box receives is this context rendered by the propagator
// ([Traceparent]), so the box's flag follows the turn rather than claiming
// sampled for every run.
//
// # Its parent is the trace's own
//
// The context's parent id is set to tc.ParentSpanID, so [TraceOf] on the
// restored context reports tc back unchanged. Left alone, it kept whatever the
// caller's context held before — a stale parent from a span that has nothing
// to do with this trace.
//
// An unusable id returns ctx unchanged rather than erroring. This is fed by the
// wire, and some wakes carry no trace — one published outside any span, such
// as an A2A ask or reply or an answer to a sandbox question — and the honest
// answer for one of those is a new root, not a refusal to run the turn.
func WithRemote(ctx context.Context, tc events.TraceContext) context.Context {
	traceID, err := trace.TraceIDFromHex(tc.TraceID)
	if err != nil {
		return ctx
	}
	spanID, err := trace.SpanIDFromHex(tc.SpanID)
	if err != nil {
		// A trace id with no span id still names the trace, and joining it
		// with no parent beats starting an unrelated one. The dashboard's
		// tree already tolerates a root with no parent span.
		spanID = trace.SpanID{}
	}
	flags := trace.FlagsSampled
	if tc.Unsampled {
		flags = 0
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: flags,
		Remote:     true,
	})
	parent := ""
	if id, err := trace.SpanIDFromHex(tc.ParentSpanID); err == nil {
		parent = id.String()
	}
	ctx = trace.ContextWithSpanContext(ctx, sc)
	ctx = context.WithValue(ctx, parentKey{}, parent)
	return logging.WithTrace(ctx, sc.TraceID().String(), sc.SpanID().String())
}

// WithRemoteFrom is [WithRemote] for a caller that hands back trace context of
// its own — an HTTP request from a process the engine started inside the trace
// tc names: the span carrier names becomes the parent instead of tc's, but ONLY
// WHEN IT IS IN tc's OWN TRACE.
//
// The process is the engine's (a coding box it launched under tc) and so is
// the trace, so the deeper parent it names — the span of its own that made the
// call — is a better one than the span that launched it, and the engine's
// spans beneath nest under the step that caused them. A traceparent naming any
// OTHER trace is ignored rather than followed: the caller holds a credential
// for one run, and honouring an arbitrary trace would let it file the engine's
// spans, and every event published beneath them, into somebody else's.
//
// THE DECISION STAYS tc's. The process continued the engine's decision when it
// started, and a flag it sends back is at best that decision again and at
// worst its own sampler re-deciding a trace already in flight, which is the
// one thing every sampler here refuses.
func WithRemoteFrom(ctx context.Context, tc events.TraceContext, carrier propagation.TextMapCarrier) context.Context {
	own, err := trace.TraceIDFromHex(tc.TraceID)
	if carrier != nil && err == nil {
		inbound := trace.SpanContextFromContext(propagator().Extract(context.Background(), carrier))
		if inbound.IsValid() && inbound.TraceID() == own {
			tc.SpanID, tc.ParentSpanID = inbound.SpanID().String(), ""
		}
	}
	return WithRemote(ctx, tc)
}

// Traceparent renders tc as the W3C `traceparent` value a process this engine
// starts reads its parent from — "" when tc names no span a child could hang
// under. Only tc decides what is rendered: whatever span ctx holds is replaced
// by tc's before the propagator reads it.
//
// THROUGH THE PROPAGATOR, from the context [WithRemote] restores, rather than
// formatted by hand: the flag field is the trace's own decision, and a second
// encoding of the header beside the one every inbound request is read with is
// a second place for that decision to be spelled wrong. It was: the sandbox
// wrote `-01` for every run. It is [ProcessEnv] over that context, so a box
// and a CLI the engine starts read one encoding.
func Traceparent(ctx context.Context, tc events.TraceContext) string {
	// THE CALLER'S SPAN IS CLEARED FIRST. WithRemote leaves ctx as it was
	// for a tc it cannot use, and the propagator would then render the
	// caller's own span — handing a box the launching frame's parent in
	// place of the run's, or a parent where the run has none.
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	return ProcessEnv(WithRemote(ctx, tc))[TraceparentVar]
}

// The variables a process this engine starts reads W3C trace context from:
// each field the trace-context propagator writes, upper-cased, which is the
// OpenTelemetry convention for carrying a propagator's fields in a process's
// ENVIRONMENT rather than in headers — and what Claude Code reads when it
// runs non-interactively.
const (
	TraceparentVar = "TRACEPARENT"
	TracestateVar  = "TRACESTATE"
)

// ProcessEnv renders the span ctx holds as the environment a process this
// engine starts reads its parent from: [TraceparentVar], and [TracestateVar]
// where the trace carries vendor state. Nil when ctx holds no valid span —
// a process handed no parent roots a trace of its own, which is honest, where
// a parent invented for it is not.
//
// FROM THE SPAN, decision included: the flag field is the span's own sampling
// decision, so a process started beneath an unsampled round is told `-00`,
// and anything it calls that honours the flag records nothing of a trace the
// engine is not exporting either.
//
// THE PROPAGATOR'S OWN ENCODING, through a carrier that names each field as an
// environment variable, so the header an outbound call carries and the
// variable a child process reads are spelled by one implementation.
func ProcessEnv(ctx context.Context) map[string]string {
	carrier := envCarrier{}
	propagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	return carrier
}

// ProcessEnvNames is every variable [ProcessEnv] can set, sorted — the names a
// layer of a child's environment that the engine owns must not carry.
func ProcessEnvNames() []string {
	names := make([]string, 0, len(propagator().Fields()))
	for _, field := range propagator().Fields() {
		names = append(names, envName(field))
	}
	slices.Sort(names)
	return names
}

// envCarrier is a [propagation.TextMapCarrier] over environment variables:
// a field is stored under its environment name ([envName]).
type envCarrier map[string]string

func (c envCarrier) Get(key string) string { return c[envName(key)] }
func (c envCarrier) Set(key, value string) { c[envName(key)] = value }
func (c envCarrier) Keys() []string        { return slices.Collect(maps.Keys(c)) }

// envName spells a propagator field as an environment variable: upper-cased,
// with every character a portable variable name cannot hold made an
// underscore — `traceparent` is TRACEPARENT.
func envName(field string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, field)
}

// Fail marks a span as failed and records the error.
//
// Telemetry never fails the work (the rule at runner/telemetry.go and
// engine/telemetry.go), so this returns nothing and tolerates a nil error —
// the call site stays a single deferred line rather than a branch.
//
// # The message is bounded, at the bound an event's diagnostic already has
//
// The text is recorded twice — the exception event's message and the span's
// status — and its length is set by something outside the engine: a provider
// chain that exhausted carries every attempt's body, and a decode failure
// carries the document it could not decode. Unbounded, one such span is not
// merely large: a collector refuses an export request past its size limit
// WHOLE, so it takes the batch's other spans with it, and the exporter's retry
// sends the same refused batch again. [events.MaxDiagnosticBytes] is the bound
// the same error text already has on an event, two orders of magnitude past
// any message written to be read, and the cut is marked so a reader cannot
// take the head for the whole.
//
// The exception event is written here rather than through span.RecordError,
// which takes the message from err.Error() with no way to bound it; the type
// is named the way RecordError names it, so a backend grouping failures by
// exception.type sees no difference.
func Fail(span trace.Span, err error) {
	if err == nil || !span.IsRecording() {
		return
	}
	message := textcut.Ellipsis(err.Error(), events.MaxDiagnosticBytes)
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
		semconv.ExceptionType(errorType(err)),
		semconv.ExceptionMessage(message)))
	span.SetStatus(codes.Error, message)
}

// errorType names an error's type as the SDK's RecordError does: the package
// path and the name, or the type's own spelling for an unnamed one.
func errorType(err error) string {
	t := reflect.TypeOf(err)
	if t.PkgPath() == "" && t.Name() == "" {
		return t.String()
	}
	return fmt.Sprintf("%s.%s", t.PkgPath(), t.Name())
}

// Active reports whether ctx carries a usable span context.
//
// It exists for one shape: a caller that wants the ACTIVE span's ids when
// there is one, and a known fallback rather than a fresh root when there is
// not. [TraceOf] mints a root for the no-span case, which is right for a
// publisher that would otherwise have no trace at all and wrong for one that
// already belongs to a turn — an event published from a detached goroutine
// would leave its turn's trace and start a second one nobody looks at.
func Active(ctx context.Context) bool {
	return trace.SpanContextFromContext(ctx).IsValid()
}
