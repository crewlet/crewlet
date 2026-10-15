// Package tracingtest records the spans a test watches, so a case can assert
// on the tree its code opens rather than on the ids it published.
//
// # Importing it is installing it
//
// Its init registers one span processor on the process's TracerProvider — the
// one internal/tracing's own init installs, so the spans recorded are decided
// by the sampler production runs, parent-based with the `continued` root rule,
// and a test reads the tree a node would export. Registered rather than a
// provider of its own: a provider built here would be a second idea of the
// sampler, and a case asserting that a restored, unsampled trace records
// nothing would be asserting it of the copy.
//
// A test binary that swaps the global provider (only internal/tracing's own
// suite does, through Configure) loses the recorder with it, and has no
// business importing this.
//
// # Only what a test watches is kept
//
// Every span the binary opens passes through the processor, and the engine's
// suite opens tens of thousands. Keeping them all would hold every one of them
// for the life of the binary, under the race detector's multiplier, for the
// handful of traces a few cases read. So nothing is kept until a case asks:
// [Watch] keeps a trace it already knows — the usual shape, a case restoring a
// [Remote] parent with tracing.WithRemote — and [WatchNamed] keeps every trace
// a span of one name is OPENED in, for a case whose code under test roots a
// trace the case cannot know in advance. Either keeps the spans of the trace
// that END after it was set, which is why a watch is set before the code runs.
//
// Cases in one binary run in parallel, so a case reads only the trace it
// watched ([Spans], [Only]); a name watched by one case keeps the traces
// another case's spans of that name open too, and a case reading by trace id
// never sees them.
package tracingtest

import (
	"context"
	"crypto/rand"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/events"

	// THE PROVIDER THIS PACKAGE RECORDS ON is the one internal/tracing's
	// init installs, so that init must have run before this one does.
	_ "github.com/crewlet/crewlet/internal/tracing"
)

// kept is the one recorder every importer of this package shares.
var kept = &recorder{
	traces: map[trace.TraceID]bool{},
	names:  map[string]bool{},
	ended:  map[trace.TraceID][]sdktrace.ReadOnlySpan{},
}

func init() {
	tp, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		// internal/tracing's init installs the SDK's provider, so this is
		// a binary whose global was replaced before this init ran — and
		// every case reading spans would read none.
		panic("tracingtest: the process's TracerProvider is not the SDK's; " +
			"something replaced the one internal/tracing installs")
	}
	tp.RegisterSpanProcessor(kept)
}

// Watch keeps every span of the trace named by traceID (32 hex digits) that
// starts from now on. A test sets it before the code it watches runs.
func Watch(t testing.TB, traceID string) {
	t.Helper()
	id, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		t.Fatalf("tracingtest: watch %q: %v", traceID, err)
	}
	kept.mu.Lock()
	defer kept.mu.Unlock()
	kept.traces[id] = true
}

// WatchNamed keeps every trace a span named name is opened in — each of its
// spans that ends after that one opened — for code that roots a trace of its
// own, whose id the test learns only from what the code published.
func WatchNamed(name string) {
	kept.mu.Lock()
	defer kept.mu.Unlock()
	kept.names[name] = true
}

// Remote is the trace of an event published INSIDE a span this process never
// opened — a turn on another node, a coding box's own call, the phase a case
// stands in for — with a fresh trace id and that span's id: what a case
// restores with tracing.WithRemote when the code under test hangs beneath a
// parent the case does not open itself. Sampled, the zero decision.
//
// Not events.NewTrace, which mints a ROOT and so names no span: restored, a
// root has no parent for anything to hang under. And not through it either:
// the ids here are a fixture's, which decides nothing, and NewTrace has one
// caller outside the tests (internal/tracing's TestOnlyTraceOfMintsARoot).
func Remote() events.TraceContext {
	var (
		traceID trace.TraceID
		spanID  trace.SpanID
	)
	for !traceID.IsValid() {
		_, _ = rand.Read(traceID[:])
	}
	for !spanID.IsValid() {
		_, _ = rand.Read(spanID[:])
	}
	return events.TraceContext{TraceID: traceID.String(), SpanID: spanID.String()}
}

// Spans is every kept span of one trace that has ENDED, in the order each
// ended. A span still open is not in it.
func Spans(traceID string) []sdktrace.ReadOnlySpan {
	id, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		return nil
	}
	kept.mu.Lock()
	defer kept.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), kept.ended[id]...)
}

// Only is the one ended span named name in a trace, failing the test when the
// trace holds none or several.
func Only(t testing.TB, traceID, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range Spans(traceID) {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("trace %s holds %d span(s) named %q, want exactly one; it holds %v",
			traceID, len(found), name, Names(traceID))
	}
	return found[0]
}

// Named is every ended span named name in a trace, in the order each ended.
func Named(traceID, name string) []sdktrace.ReadOnlySpan {
	var found []sdktrace.ReadOnlySpan
	for _, s := range Spans(traceID) {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	return found
}

// Names is the name of every ended span of a trace, for a failure message.
func Names(traceID string) []string {
	var names []string
	for _, s := range Spans(traceID) {
		names = append(names, s.Name())
	}
	return names
}

// Attr is a span's attribute of one key as a string ("" when absent), which is
// every shape a case here compares an attribute in.
func Attr(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// recorder is a [sdktrace.SpanProcessor] that keeps the spans of the traces
// it was asked to watch.
type recorder struct {
	mu     sync.Mutex
	traces map[trace.TraceID]bool
	names  map[string]bool
	ended  map[trace.TraceID][]sdktrace.ReadOnlySpan
}

func (r *recorder) OnStart(_ context.Context, s sdktrace.ReadWriteSpan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[s.Name()] {
		r.traces[s.SpanContext().TraceID()] = true
	}
}

func (r *recorder) OnEnd(s sdktrace.ReadOnlySpan) {
	id := s.SpanContext().TraceID()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.traces[id] {
		r.ended[id] = append(r.ended[id], s)
	}
}

func (r *recorder) Shutdown(context.Context) error   { return nil }
func (r *recorder) ForceFlush(context.Context) error { return nil }
