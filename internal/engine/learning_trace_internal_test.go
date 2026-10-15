package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/tracing"
)

// A BACKGROUND PASS'S ANNOUNCEMENT IS FILED UNDER THE PASS. The loop opens a
// root span per tick and hands it down; the announcement used to carry no trace
// at all, so it sat in no trace while each of the pass's model calls sat in a
// root of its own.
//
// Mutation, red here: publish under an empty trace again.
func TestABackgroundAnnouncementIsFiledUnderItsPass(t *testing.T) {
	t.Parallel()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var (
		mu  sync.Mutex
		got []*events.Event
	)
	q.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	})
	e := &Engine{backends: &Backends{Queue: q}}

	ctx, span := tracing.StartRoot(t.Context(), "learning", "learning.probe")
	defer span.End()
	pass := tracing.TraceOf(ctx)
	e.publishLearning(ctx, "dev",
		types.CompactionRequested{AgentHandle: "dev", RawCount: 3, Threshold: 2})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("published %d events, want the one announcement", len(got))
	}
	if tr := got[0].Trace(); tr != pass {
		t.Errorf("the announcement is filed under %+v, want the pass's %+v", tr, pass)
	}
}
