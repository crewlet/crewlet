package tracingtest_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
)

// A WATCHED TRACE IS KEPT AND AN UNWATCHED ONE IS NOT — the bound that lets a
// binary opening tens of thousands of spans import this at all.
//
// Mutations, each red here: keep every trace in OnEnd (the unwatched half);
// drop the trace check's write in Watch (the watched half).
func TestOnlyAWatchedTraceIsKept(t *testing.T) {
	t.Parallel()
	watched, ignored := events.NewTrace(), events.NewTrace()
	tracingtest.Watch(t, watched.TraceID)
	for _, tc := range []events.TraceContext{watched, ignored} {
		_, span := tracing.Start(tracing.WithRemote(context.Background(), tc), "test", "tracingtest.probe")
		span.End()
	}
	got := tracingtest.Only(t, watched.TraceID, "tracingtest.probe")
	if parent := got.Parent().SpanID().String(); parent != watched.SpanID {
		t.Errorf("the kept span's parent is %s, want the restored %s", parent, watched.SpanID)
	}
	if spans := tracingtest.Spans(ignored.TraceID); len(spans) != 0 {
		t.Errorf("an unwatched trace kept %d span(s)", len(spans))
	}
}

// A NAMED SPAN'S TRACE IS KEPT, with the spans opened beneath it — what a case
// whose code roots its own trace reads.
//
// Mutation, red here: stop OnStart adding the trace of a watched name.
func TestANamedSpansTraceIsKept(t *testing.T) {
	t.Parallel()
	tracingtest.WatchNamed("tracingtest.named_root")
	ctx, root := tracing.Start(context.Background(), "test", "tracingtest.named_root")
	_, child := tracing.Start(ctx, "test", "tracingtest.child")
	child.End()
	root.End()
	id := root.SpanContext().TraceID().String()
	got := tracingtest.Only(t, id, "tracingtest.child")
	if got.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("the child's parent is %s, want the named root %s",
			got.Parent().SpanID(), root.SpanContext().SpanID())
	}
	tracingtest.Only(t, id, "tracingtest.named_root")
}
