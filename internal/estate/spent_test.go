package estate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// countedAppender is a broker that commits every barrier at once, counting
// them across every node that appends through it — the one log a fleet's
// data nodes all append their barriers to.
type countedAppender struct{ n *atomic.Int64 }

func (a countedAppender) Append(ctx context.Context, subject, msgID string, expect *uint64,
	body []byte) (uint64, bool, error) {
	a.n.Add(1)
	return committedAppender{}.Append(ctx, subject, msgID, expect, body)
}

func (countedAppender) LastSeq(context.Context, string) (uint64, bool, error) {
	return 0, false, nil
}

// A READ EVERY DATA NODE REFUSES `behind` APPENDS ONE BARRIER AND WAITS ONE
// READ BUDGET, however many nodes it asks, every copy a real statelog reader
// that never reaches the end of the one log they append to: the first node
// appends and waits its budget; each later one is handed the read index and
// the position waited out, so it appends nothing and refuses at once. Lagging
// copies (asked only as the last resort) and the asker's own copy, first or
// lagging last, pay the same. COUNTED, NOT TIMED: barriers appended and waits
// begun with time left; a wall-clock bound also charged the walk's signed
// round trips, which a loaded race run exceeds.
//
// Mutation: hand the nodes asked after the first a whole read budget of their
// own for the position it waited out, and every node's wait is counted.
func TestAReadEveryDataNodeRefusesBehindPaysForOneBarrierAndOneWait(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lagging      bool
		local        bool
		localLagging bool
	}{
		"every copy answering":                   {},
		"every copy lagging its logs":            {lagging: true},
		"the asker's own copy among them":        {local: true},
		"the asker's own copy lagging, its last": {local: true, localLagging: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var appends, waits atomic.Int64
			f := newFleet(t, "data-a", "data-b", "data-c")
			for name, n := range f.nodes {
				n.set(func(n *fakeNode) {
					n.levelRead = laggingReadOn(t, countedAppender{n: &appends}, &waits)
					n.notReady = tc.lagging || (tc.localLagging && name == "data-a")
				})
			}
			r := f.client
			if tc.local {
				r = f.router(t, "data-a", f.nodes["data-a"])
			}

			_, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
			var refusal *statelog.Refused
			if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBehind {
				t.Fatalf("a read every node refuses = %v, want the refusal %q", err, statelog.RefuseBehind)
			}
			for name, n := range f.nodes {
				if !n.askedFor("tasks") {
					t.Errorf("%s was never asked: the walk ended before it reached every node", name)
				}
			}
			if got := appends.Load(); got != 1 {
				t.Errorf("the walk appended %d barriers to the log for one read, want one", got)
			}
			if got := waits.Load(); got != 1 {
				t.Errorf("the walk waited out %d read budgets for one read, want one", got)
			}
		})
	}
}

// A FLOOR EVERY DATA NODE IS BEHIND IS WAITED FOR ONCE. The asking node's own
// write is the floor its read carries, and no data node has applied it: the
// first one asked waits its whole read budget for it and answers `behind`,
// and every node asked after it only checks the floor. Counted, not timed, as
// the read's own waits are ([TestAReadEveryDataNodeRefusesBehindPaysForOneBarrierAndOneWait]).
//
// Mutation: hand the nodes asked after the first a whole read budget of their
// own for the floor it waited out, and every node's wait is counted.
func TestAFloorEveryDataNodeIsBehindIsWaitedForOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a", "data-b", "data-c")
	for _, n := range f.nodes {
		n.set(func(n *fakeNode) { n.behind = true })
	}
	f.client.Observe(statelog.Position{Stream: trackerStream, Generation: 1, Seq: 5})

	_, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) {
		t.Fatalf("a read no node has the floor for = %v, want it unserved", err)
	}
	waits := 0
	for name, n := range f.nodes {
		n.mu.Lock()
		floors := len(n.floors)
		waits += n.waits
		n.mu.Unlock()
		if floors == 0 {
			t.Errorf("%s was never asked for the floor", name)
		}
	}
	if waits != 1 {
		t.Errorf("the walk waited out %d read budgets for one floor, want one", waits)
	}
}
