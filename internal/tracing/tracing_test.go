package tracing

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// envOf builds an Options.Env over a literal map, so no test mutates the
// process environment and none of them can race each other.
func envOf(kv map[string]string) func(string) string {
	return func(name string) string { return kv[name] }
}

// configure installs a provider for one test and tears it down after.
//
// TracerProvider.Shutdown is TERMINAL — a provider that has been shut down
// silently drops every later span — so a test that forgot this would not fail,
// it would quietly stop testing anything.
func configure(t *testing.T, kv map[string]string) {
	t.Helper()
	shutdown, err := Configure(context.Background(), Options{Env: envOf(kv), NodeID: "node-a"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
}

// The load-bearing property of the whole design: with NO collector configured,
// spans are still real and their ids still flow. Everything downstream — the
// event envelope, crewlet_events, the dashboard's trace tree — depends on this
// being true in the ordinary single-node deployment that exports nothing.
func TestIdsFlowWithNoCollectorConfigured(t *testing.T) {
	configure(t, nil)

	ctx, span := Start(context.Background(), "test", "an.operation")
	defer span.End()

	tc := TraceOf(ctx)
	if len(tc.TraceID) != 32 {
		t.Errorf("trace id = %q, want 32 hex", tc.TraceID)
	}
	if len(tc.SpanID) != 16 {
		t.Errorf("span id = %q, want 16 hex", tc.SpanID)
	}
	if !span.SpanContext().IsValid() {
		t.Error("the span context is not valid — nothing downstream can use it")
	}
}

// An event published outside any span must still carry a usable trace, because
// that is what every publisher did before this package existed.
func TestNoSpanStillYieldsARootTrace(t *testing.T) {
	configure(t, nil)

	tc := TraceOf(context.Background())
	if len(tc.TraceID) != 32 || len(tc.SpanID) != 16 {
		t.Fatalf("a span-less context produced %+v", tc)
	}
	if tc.ParentSpanID != "" {
		t.Errorf("a root claimed a parent: %q", tc.ParentSpanID)
	}
}

// The bug this replaces: every downstream hop used to copy its PARENT's span
// id into its own SpanID, or leave SpanID empty. An event's span id must be
// the span that emitted it, and its parent the span above.
func TestANestedSpanReportsItselfAndItsParent(t *testing.T) {
	configure(t, nil)

	outerCtx, outer := Start(context.Background(), "test", "outer")
	defer outer.End()
	innerCtx, inner := Start(outerCtx, "test", "inner")
	defer inner.End()

	out, in := TraceOf(outerCtx), TraceOf(innerCtx)

	if in.TraceID != out.TraceID {
		t.Errorf("the child left the trace: %s vs %s", in.TraceID, out.TraceID)
	}
	if in.SpanID == out.SpanID {
		t.Error("the child reported its parent's span id as its own — the exact bug this replaces")
	}
	if in.ParentSpanID != out.SpanID {
		t.Errorf("parent_span_id = %q, want the outer span %q", in.ParentSpanID, out.SpanID)
	}
	if out.ParentSpanID != "" {
		t.Errorf("the root claimed a parent: %q", out.ParentSpanID)
	}
}

// A turn woken by an event off the queue must continue the trigger's trace,
// not start a new one. This is the whole point of carrying ids in the envelope.
func TestARestoredTraceIsContinuedNotRestarted(t *testing.T) {
	configure(t, nil)

	// What a publisher wrote into the envelope on another node.
	origin := events.TraceContext{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:  "00f067aa0ba902b7",
	}

	ctx := WithRemote(context.Background(), origin)
	ctx, span := Start(ctx, "test", "agent.turn")
	defer span.End()

	got := TraceOf(ctx)
	if got.TraceID != origin.TraceID {
		t.Errorf("trace id = %s, want the trigger's %s", got.TraceID, origin.TraceID)
	}
	if got.ParentSpanID != origin.SpanID {
		t.Errorf("parent = %s, want the trigger's span %s", got.ParentSpanID, origin.SpanID)
	}
	if got.SpanID == origin.SpanID {
		t.Error("the turn reused the trigger's span id instead of minting its own")
	}
	// The upstream sampling decision must be honoured, or half the tree
	// goes missing at the collector.
	if !trace.SpanContextFromContext(ctx).IsSampled() {
		t.Error("a restored remote trace was not treated as sampled")
	}
}

// The wire feeds this. An event written by an older build carries no ids at
// all, and a rolling upgrade guarantees some do — those must still run.
func TestAnUnusableRemoteTraceIsNotFatal(t *testing.T) {
	configure(t, nil)

	for _, tc := range []struct {
		name  string
		trace events.TraceContext
	}{
		{"empty", events.TraceContext{}},
		{"garbage trace", events.TraceContext{TraceID: "nope", SpanID: "00f067aa0ba902b7"}},
		{"all-zero trace", events.TraceContext{TraceID: strings.Repeat("0", 32)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := context.Background()
			ctx := WithRemote(base, tc.trace)
			// It must be safe to keep working: a fresh root, not a panic
			// and not a poisoned context.
			if got := TraceOf(ctx); len(got.TraceID) != 32 {
				t.Errorf("could not carry on after a bad remote trace: %+v", got)
			}
		})
	}
}

// A trace id with no span id still names the trace, and joining it beats
// starting an unrelated one.
func TestATraceWithNoSpanStillJoinsTheTrace(t *testing.T) {
	configure(t, nil)

	origin := events.TraceContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"}
	ctx, span := Start(WithRemote(context.Background(), origin), "test", "op")
	defer span.End()

	if got := TraceOf(ctx); got.TraceID != origin.TraceID {
		t.Errorf("trace id = %s, want %s", got.TraceID, origin.TraceID)
	}
}

// Starting a span must bind its ids for the logger too — one call doing both
// is what stops a span existing whose ids never reached a log line.
func TestStartingASpanBindsItForLogging(t *testing.T) {
	configure(t, nil)

	ctx, span := Start(context.Background(), "test", "an.operation")
	defer span.End()

	traceID, spanID := logging.TraceFromContext(ctx)
	sc := span.SpanContext()
	if traceID != sc.TraceID().String() || spanID != sc.SpanID().String() {
		t.Errorf("logging carries (%s, %s), span is (%s, %s)",
			traceID, spanID, sc.TraceID(), sc.SpanID())
	}
}

// A protocol this build cannot speak is refused, and the message names the
// ones that work — an operator who typed `http` or `otlp` gets told, rather
// than exporting nowhere in silence.
func TestAnUnknownProtocolIsRefusedByName(t *testing.T) {
	_, err := Configure(context.Background(), Options{Env: envOf(map[string]string{
		EndpointVar: "http://localhost:4318",
		ProtocolVar: "http",
	})})
	if err == nil {
		t.Fatal("an unknown protocol was accepted")
	}
	for _, want := range Protocols {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
}

// The never-fail-on-a-typo rule the log level already follows: a bad sampler
// ratio must not be why a company will not boot.
func TestABadSamplerRatioStillBoots(t *testing.T) {
	for _, arg := range []string{"banana", "-1", "2", "1e", ""} {
		t.Run("arg="+arg, func(t *testing.T) {
			configure(t, map[string]string{SamplerArgVar: arg})
			ctx, span := Start(context.Background(), "test", "op")
			defer span.End()
			if len(TraceOf(ctx).TraceID) != 32 {
				t.Error("tracing did not work after a bad sampler ratio")
			}
		})
	}
}

// A ratio of 0 is a REAL setting — "never sample" — and must be distinguishable
// from unset rather than silently reading as the default.
func TestASamplerRatioOfZeroIsRespected(t *testing.T) {
	configure(t, map[string]string{SamplerArgVar: "0"})

	ctx, span := Start(context.Background(), "test", "op")
	defer span.End()

	// Ids still exist (they must, for the event store); the span is simply
	// not recorded for export.
	if len(TraceOf(ctx).TraceID) != 32 {
		t.Error("a zero ratio destroyed the ids the event store needs")
	}
	if span.IsRecording() {
		t.Error("a ratio of 0 still recorded a root span")
	}
}

// THE ENGINE AND THE SANDBOX RECEIVER MUST READ ONE SETTING. The receiver
// stamps a TRACEPARENT into a box whose parent is the engine's own span; if
// the two halves exported to different backends, that parent/child link would
// resolve on neither.
func TestTheExporterSharesTheReceiversEnvironment(t *testing.T) {
	if EndpointVar != sandbox.OtelUpstreamEndpointVar {
		t.Errorf("endpoint var %q has drifted from the receiver's %q",
			EndpointVar, sandbox.OtelUpstreamEndpointVar)
	}
	if HeadersVar != sandbox.OtelUpstreamHeadersVar {
		t.Errorf("headers var %q has drifted from the receiver's %q",
			HeadersVar, sandbox.OtelUpstreamHeadersVar)
	}
}

// Shutdown must tolerate an already-cancelled context: the cancellation is
// what woke the drain, and a flush that inherited it would return instantly
// having exported nothing.
func TestShutdownFlushesUnderACancelledContext(t *testing.T) {
	shutdown, err := Configure(context.Background(), Options{Env: envOf(nil)})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown under a cancelled context: %v", err)
	}
}
