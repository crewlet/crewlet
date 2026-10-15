package tracing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/otlpenv"
)

// envOf builds an Options.Env over a literal map, so no test mutates the
// process environment and none of them can race each other.
func envOf(kv map[string]string) func(string) string {
	return func(name string) string { return kv[name] }
}

// configure installs a provider for one test and tears it down after: see
// [install], which every test that installs a provider goes through.
func configure(t *testing.T, kv map[string]string) {
	t.Helper()
	install(t, Options{Env: envOf(kv), NodeID: "node-a"})
}

// install runs [Configure] for one test, and after it shuts that provider
// down AND PUTS BACK the globals it replaced. It hands back the shutdown for a
// test whose subject is the flush; the cleanup runs it only if the test did
// not.
//
// The shutdown is not enough on its own. TracerProvider.Shutdown is TERMINAL —
// a provider that has been shut down hands out no-op tracers, whose Start
// opens no span and passes the parent's span context straight through — and
// Configure installs its provider as the process global. Left there, every
// later test in the package that opens a span without installing one runs on
// a terminated provider and tests nothing: a span it believes it opened is
// the zero context, or the parent it restored. Two such tests were silently
// vacuous in package order while passing when run alone.
//
// So the tracer provider, the meter provider and the root rule are restored
// together, as Configure's commit installs them. Cleanups run last in, first
// out, so a test that installs twice unwinds to the provider it started on,
// and TestMain holds the package to ending on the provider it began with.
func install(t *testing.T, opts Options) Shutdown {
	t.Helper()
	tracers, meters, rule := otel.GetTracerProvider(), otel.GetMeterProvider(), root.Load()
	shutdown, err := Configure(context.Background(), opts)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	var done atomic.Bool
	once := func(ctx context.Context) error {
		if done.Swap(true) {
			return nil
		}
		return shutdown(ctx)
	}
	t.Cleanup(func() {
		if err := once(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		otel.SetMeterProvider(meters)
		otel.SetTracerProvider(tracers)
		root.Store(rule)
	})
	return once
}

// A TEST THAT CONFIGURES LEAVES A WORKING PROVIDER BEHIND IT, so a later test
// that opens a span without configuring opens a real one.
//
// Mutation: stop restoring the globals in configure's cleanup and the span
// opened after the subtest is the zero context of a terminated provider.
func TestConfigureLeavesAWorkingProviderBehind(t *testing.T) {
	t.Run("configures", func(t *testing.T) { configure(t, nil) })

	_, span := Start(context.Background(), "test", "after")
	defer span.End()
	if sc := span.SpanContext(); !sc.IsValid() || !span.IsRecording() {
		t.Fatalf("a span opened after a configured test is valid=%v recording=%v — "+
			"the provider it left installed is terminated", sc.IsValid(), span.IsRecording())
	}
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
// that is what every publisher did before this package existed — and only a
// trace: no span was open, so there is no span id to name and no parent.
//
// Mutation: mint a span id beside the trace id again and this goes red.
func TestNoSpanStillYieldsARootTrace(t *testing.T) {
	configure(t, nil)

	tc := TraceOf(context.Background())
	if len(tc.TraceID) != 32 {
		t.Fatalf("a span-less context produced %+v", tc)
	}
	if tc.SpanID != "" {
		t.Errorf("a root nobody spanned claims span %q, an id no span ever had", tc.SpanID)
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

// A ROOT IS A ROOT whatever the context it was opened on holds — the span a
// long-lived loop's context carried from whatever armed it included — and it
// claims no parent.
//
// Mutation: make StartRoot plain Start and both halves go red.
func TestStartRootIgnoresTheSpanItsContextHolds(t *testing.T) {
	configure(t, nil)
	stale := events.TraceContext{TraceID: events.NewTrace().TraceID, SpanID: "00f067aa0ba902b7"}
	ctx, root := StartRoot(WithRemote(context.Background(), stale), "test", "root")
	defer root.End()
	got := TraceOf(ctx)
	if got.TraceID == stale.TraceID {
		t.Errorf("the root opened in the trace its context held, %s", stale.TraceID)
	}
	if got.ParentSpanID != "" {
		t.Errorf("the root claimed a parent: %q", got.ParentSpanID)
	}
}

// A CALLER'S OWN SPAN IS FOLLOWED ONLY INSIDE THE TRACE IT WAS HANDED, and
// never its decision: a span in tc's trace becomes the parent, anything else —
// another trace, no carrier — leaves tc's own, and tc's sampling decision
// stands whatever flag the caller sent. Read as what an event published on the
// restored context names: the span it was published beneath.
//
// Mutations: drop the trace comparison (the foreign case), or take the
// carrier's flag (the unsampled case).
func TestWithRemoteFromFollowsACallerOnlyInsideItsOwnTrace(t *testing.T) {
	configure(t, nil)
	const callerSpan, runSpan = "b7ad6b7169203331", "00f067aa0ba902b7"
	run := events.TraceContext{TraceID: events.NewTrace().TraceID, SpanID: runSpan, Unsampled: true}
	foreign := events.NewTrace()
	beneath := func(span string) events.TraceContext {
		return events.TraceContext{TraceID: run.TraceID, ParentSpanID: span, Unsampled: true}
	}
	for _, tc := range []struct {
		name    string
		carrier propagation.TextMapCarrier
		want    events.TraceContext
	}{
		{"no carrier", nil, beneath(runSpan)},
		{"a span in the run's trace", propagation.MapCarrier{
			"traceparent": "00-" + run.TraceID + "-" + callerSpan + "-01"}, beneath(callerSpan)},
		{"a span in another trace", propagation.MapCarrier{
			"traceparent": "00-" + foreign.TraceID + "-" + callerSpan + "-01"}, beneath(runSpan)},
	} {
		if got := TraceOf(WithRemoteFrom(context.Background(), run, tc.carrier)); got != tc.want {
			t.Errorf("%s: restored %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A turn woken by an event off the queue must continue the trigger's trace,
// not start a new one. This is the whole point of carrying ids in the envelope.
//
// AND IT KEEPS THE TRIGGER'S DECISION, both ways. This case used to pin that a
// restored trace is ALWAYS sampled, which was the bug: WithRemote hardcoded the
// sampled flag, so a trace its root had decided not to sample was exported
// from its second hop on. Run under the default sampler, which samples every
// root, so the unsampled half can only pass by honouring the flag it was given.
//
// Mutation: put `TraceFlags: trace.FlagsSampled` back in WithRemote and the
// unsampled half goes red.
func TestARestoredTraceIsContinuedNotRestarted(t *testing.T) {
	configure(t, nil)

	for _, unsampled := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsampled=%v", unsampled), func(t *testing.T) {
			// What a publisher wrote into the envelope on another node.
			origin := events.TraceContext{
				TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736",
				SpanID:    "00f067aa0ba902b7",
				Unsampled: unsampled,
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
			// The upstream sampling decision must be honoured, or half the
			// tree goes missing at the collector — or, the other way, a
			// continuation is exported under a parent nobody exported.
			if sampled := trace.SpanContextFromContext(ctx).IsSampled(); sampled == unsampled {
				t.Errorf("a restored trace decided unsampled=%v continued sampled=%v",
					unsampled, sampled)
			}
			if span.IsRecording() == unsampled {
				t.Errorf("a restored trace decided unsampled=%v recorded=%v",
					unsampled, span.IsRecording())
			}
			if got.Unsampled != unsampled {
				t.Errorf("the turn's events carry unsampled=%v, want the trigger's %v",
					got.Unsampled, unsampled)
			}
		})
	}
}

// The wire feeds this. A wake published outside any span carries no ids at
// all, and garbage or all-zero ids are no better — those must still run.
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
			if got := TraceOf(ctx); len(got.TraceID) != 32 || got.ParentSpanID != "" {
				t.Errorf("could not carry on after a bad remote trace: %+v", got)
			}
		})
	}
}

// A trace id with no span id still names the trace, and joining it beats
// starting an unrelated one — as its root, since it names no span to hang
// under.
func TestATraceWithNoSpanStillJoinsTheTrace(t *testing.T) {
	configure(t, nil)

	origin := events.TraceContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"}
	ctx, span := Start(WithRemote(context.Background(), origin), "test", "op")
	defer span.End()

	got := TraceOf(ctx)
	if got.TraceID != origin.TraceID {
		t.Errorf("trace id = %s, want %s", got.TraceID, origin.TraceID)
	}
	if got.ParentSpanID != "" {
		t.Errorf("the span opened in a trace nobody spanned names parent %q", got.ParentSpanID)
	}
}

// A RESTORED TRACE IS BOUND FOR THE LOGGER WITHOUT A SPAN: a line logged on
// the far side of the queue, before a span opens there, is in the trace and in
// no span of this node's — the span the trace was restored to ended somewhere
// else, and the line was not logged in it. Nor in the span the caller's own
// context held, which the restore replaces.
//
// Mutation: bind the restored span's id (the line claims another node's span),
// or the zero id a root nobody spanned restores to (every such line carried
// span_id 0000000000000000), and a case goes red.
func TestARestoredTraceIsLoggedInNoSpan(t *testing.T) {
	configure(t, nil)
	caller, span := Start(context.Background(), "test", "caller")
	defer span.End()
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	for name, tc := range map[string]events.TraceContext{
		"published in a span":       {TraceID: traceID, SpanID: "00f067aa0ba902b7"},
		"published beneath a span":  {TraceID: traceID, ParentSpanID: "b7ad6b7169203331"},
		"a root nobody spanned":     {TraceID: traceID},
		"an unsampled one, spanned": {TraceID: traceID, SpanID: "00f067aa0ba902b7", Unsampled: true},
	} {
		gotTrace, gotSpan := logging.TraceFromContext(WithRemote(caller, tc))
		if gotTrace != traceID || gotSpan != "" {
			t.Errorf("%s: logging carries (%q, %q), want the trace %s and no span",
				name, gotTrace, gotSpan, traceID)
		}
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
		otlpenv.EndpointVar: "http://localhost:4318",
		otlpenv.ProtocolVar: "http",
	})})
	if err == nil {
		t.Fatal("an unknown protocol was accepted")
	}
	for _, want := range otlpenv.Protocols {
		if !strings.Contains(err.Error(), string(want)) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
}

// The never-fail-on-a-typo rule the log level already follows: a bad sampler
// ratio must not be why a company will not boot.
//
// NaN is in the list because ParseFloat accepts it and every comparison with
// it is false, so a range check written as `ratio < 0 || ratio > 1` waved it
// through to a threshold no conversion defines.
func TestABadSamplerRatioStillBoots(t *testing.T) {
	for _, arg := range []string{"banana", "-1", "2", "1e", "NaN", "+Inf", ""} {
		for _, sampler := range []string{"", samplerRatio} {
			t.Run("sampler="+sampler+",arg="+arg, func(t *testing.T) {
				configure(t, map[string]string{SamplerVar: sampler, SamplerArgVar: arg})
				ctx, span := Start(context.Background(), "test", "op")
				defer span.End()
				if len(TraceOf(ctx).TraceID) != 32 {
					t.Error("tracing did not work after a bad sampler ratio")
				}
				// AND THE FALLBACK IS TO SAMPLE, not to stop: the failure
				// an operator can see beats the one they cannot. Asked of
				// the rule itself, because a NaN ratio that slipped
				// through samples by accident on one CPU and not another.
				if !span.IsRecording() {
					t.Errorf("a bad ratio %q fell back to sampling nothing", arg)
				}
				if got, want := rootSampler().Description(),
					sdktrace.AlwaysSample().Description(); got != want {
					t.Errorf("a bad ratio %q became %s, want %s", arg, got, want)
				}
			})
		}
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

// Shutdown must tolerate an already-cancelled context: the cancellation is
// what woke the drain, and a flush that inherited it would return instantly
// having exported nothing.
func TestShutdownFlushesUnderACancelledContext(t *testing.T) {
	shutdown := install(t, Options{Env: envOf(nil)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown under a cancelled context: %v", err)
	}
}

// A collector that is not reachable must not fail the boot: the exporter
// connects lazily and retries, and refusing to start a company because a
// telemetry backend is down would make tracing a liability.
func TestAnUnreachableCollectorDoesNotFailTheBoot(t *testing.T) {
	// install fails the test if Configure refuses: an unreachable
	// collector must not be why a boot fails.
	shutdown := install(t, Options{Env: envOf(map[string]string{
		// Nothing listens here, and nothing should try until a span ends.
		otlpenv.EndpointVar: "http://127.0.0.1:1",
	})})
	// Shut down here rather than in the cleanup, whose error is a failure:
	// an export to nothing is expected to fail, and that is not the case.
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	ctx, span := Start(context.Background(), "test", "op")
	span.End()
	if len(TraceOf(ctx).TraceID) != 32 {
		t.Error("ids stopped working when the collector was unreachable")
	}
}

// A CONFIGURE THAT FAILS MUST NOT HAVE INSTALLED ANYTHING.
//
// The failure below is the one an operator can actually reach: a protocol
// this build does not speak, written in the METRICS half's own variable. The
// traces half resolves its own destination and never reads that variable, so
// the refusal comes from the metrics half — AFTER the TracerProvider has been
// built, and after it was built with a sampler. Left installed, that provider
// is published to every span site in the process while the caller holds no
// flush for it; its batch processor and exporter run on for the life of a
// boot that failed.
//
// Mutation: install the provider where it is built (`otel.SetTracerProvider(tp)`
// immediately after `sdktrace.NewTracerProvider`) and this goes red on the
// identity check — the global is the half-built provider nothing can flush.
// Store the root rule there too, and it goes red on the sampler check.
func TestAFailedConfigureLeavesTheInstalledProviderAlone(t *testing.T) {
	// Establish the precondition explicitly: a working provider, installed
	// by this test rather than inherited, so the identity check below is
	// against a provider this case knows is live.
	configure(t, nil)
	before, beforeRoot := otel.GetTracerProvider(), rootSampler()

	_, err := Configure(context.Background(), Options{Env: envOf(map[string]string{
		otlpenv.EndpointVar:           "http://127.0.0.1:1",
		otlpenv.Metrics.ProtocolVar(): "otlp",
		SamplerVar:                    samplerAlwaysOff,
	})})
	if err == nil {
		t.Fatal("a protocol this build cannot speak was accepted by the metrics half")
	}
	if !strings.Contains(err.Error(), otlpenv.Metrics.ProtocolVar()) {
		t.Errorf("the error does not name the variable to change: %v", err)
	}
	if got := otel.GetTracerProvider(); got != before {
		t.Error("a failed Configure installed its half-built TracerProvider — " +
			"the caller got no Shutdown, so nothing can ever flush or stop it")
	}
	if got := rootSampler(); got != beforeRoot {
		t.Errorf("a failed Configure installed its root rule %s — a span-less "+
			"root would be decided by a sampler no provider runs", got.Description())
	}

	// And the process is still tracing. A terminated or no-op provider mints
	// no span of its own, which is the state the package doc says every
	// event in the store depends on not being in.
	ctx, span := Start(context.Background(), "test", "op")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Error("tracing stopped working after a failed Configure")
	}
	if len(TraceOf(ctx).SpanID) != 16 {
		t.Errorf("span id = %q, want 16 hex", TraceOf(ctx).SpanID)
	}
}

// WHAT A FAILED CONFIGURE BUILT IS STOPPED, not dropped on the floor. The
// batch processor's goroutine and the exporter's connection start when the
// provider is constructed, so the pointer this frame is about to lose is a
// live exporter.
//
// Mutation: return `cause` from [abandon] without calling shutdownTracer, and
// this goes red — the exporter is never shut down.
func TestAnAbandonedProviderStopsItsExporter(t *testing.T) {
	exp := &stubExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))

	cause := errors.New("the metrics half refused a protocol")
	if err := abandon(context.Background(), tp, cause); !errors.Is(err, cause) {
		t.Errorf("abandon returned %v, want the cause %v — the operator acts on "+
			"what they typed, not on a flush that failed behind it", err, cause)
	}
	if !exp.stopped.Load() {
		t.Error("the exporter was left running by a Configure that failed")
	}
}

// stubExporter is a SpanExporter that records having been shut down. A real
// one would need a collector; what is under test is the teardown, not an
// export.
type stubExporter struct{ stopped atomic.Bool }

func (e *stubExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }

func (e *stubExporter) Shutdown(context.Context) error {
	e.stopped.Store(true)
	return nil
}
