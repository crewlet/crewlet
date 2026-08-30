package tracing

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
)

// The bridge between OTel's carrier and this engine's.
//
// # Two representations of one fact, and why both stay
//
// OTel carries a span in `context.Context`; Crewlet carries `trace_id` /
// `span_id` / `parent_span_id` in the event envelope, which is what reaches
// `crewlet_events`, `GET /events/trace/{id}` and the dashboard's trace tree.
// Neither can replace the other: a `context.Context` does not survive a
// JetStream publish, and three hex strings do not carry a live span. So this
// file is the ONE place they are converted, in both directions, and no other
// package builds a `TraceContext` by hand.
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
// alongside. [Start] is the only writer.
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

// TraceOf renders the active span as the envelope's three fields, so an event
// published from inside a span carries the ids that span will export under.
//
// # A fresh root when there is no span
//
// Rather than empty strings. Every one of this engine's event publishers used
// to call [events.NewTrace] for exactly this case, and an event with no trace
// id is one the store cannot group and the dashboard cannot place. So the
// no-span answer is the same answer it always was — a freshly minted root —
// and the call sites simply stopped having to know which case they were in.
func TraceOf(ctx context.Context) events.TraceContext {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return events.NewTrace()
	}
	parent, _ := ctx.Value(parentKey{}).(string)
	return events.TraceContext{
		TraceID:      sc.TraceID().String(),
		SpanID:       sc.SpanID().String(),
		ParentSpanID: parent,
	}
}

// WithRemote restores a trace that arrived from somewhere else — an event off
// the queue, a resumed sandbox run, an inbound `traceparent` — so the spans
// opened beneath it become children of the span that caused them rather than
// new roots.
//
// The restored context is marked REMOTE, which is what makes the sampler treat
// the upstream decision as authoritative (see [sampler]): a node that re-decided
// sampling for a trace already in flight is how half a tree goes missing.
//
// An unusable id returns ctx unchanged rather than erroring. This is fed by the
// wire — an old event written before this existed carries nothing, and a
// rolling upgrade guarantees some do — and the honest answer for one of those
// is a new root, not a refusal to run the turn.
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
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
		// Sampled, because the upstream that minted this id is the one
		// that made the decision, and an unsampled parent with sampled
		// children is a broken tree at the collector. This mirrors what
		// sandbox/otel.go already hardcodes into the TRACEPARENT it
		// stamps into a box.
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx = trace.ContextWithSpanContext(ctx, sc)
	return logging.WithTrace(ctx, sc.TraceID().String(), sc.SpanID().String())
}

// Fail marks a span as failed and records the error.
//
// Telemetry never fails the work (the rule at runner/telemetry.go and
// engine/telemetry.go), so this returns nothing and tolerates a nil error —
// the call site stays a single deferred line rather than a branch.
func Fail(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
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
