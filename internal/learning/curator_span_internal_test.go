package learning

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
)

// A BACKGROUND PASS ROOTS A TRACE OF ITS OWN, and runs inside it — whatever
// span the loop's context happened to hold. Nothing caused the pass but its
// clock, so nesting it under that span would file a company-wide sweep under an
// unrelated piece of work; and running it under no span made each of its model
// calls the root of a trace of its own and its announcement traceless.
//
// Mutations, each red here: open the span with tracing.Start (the root half —
// the tick lands in the stale trace); run the pass on the loop's context (the
// pass half).
func TestABackgroundPassRootsATraceOfItsOwn(t *testing.T) {
	t.Parallel()
	stale := tracingtest.Remote()
	tracingtest.WatchNamed(PassSpanPrefix + "skill_curator")
	b := NewBackground(BackgroundOptions{})

	var under trace.SpanContext
	b.run(tracing.WithRemote(t.Context(), stale), "skill_curator", func(ctx context.Context) {
		under = trace.SpanContextFromContext(ctx)
	})

	if !under.IsValid() {
		t.Fatal("the pass ran under no span")
	}
	if under.TraceID().String() == stale.TraceID {
		t.Fatalf("the pass ran in the trace its loop's context held, %s", stale.TraceID)
	}
	span := tracingtest.Only(t, under.TraceID().String(), PassSpanPrefix+"skill_curator")
	if span.Parent().IsValid() {
		t.Errorf("the pass's span has parent %s, want a root", span.Parent().SpanID())
	}
	if span.SpanContext().SpanID() != under.SpanID() {
		t.Errorf("the pass ran under span %s, want its own %s", under.SpanID(), span.SpanContext().SpanID())
	}
}
