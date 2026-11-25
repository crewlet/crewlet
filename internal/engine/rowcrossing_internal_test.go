package engine

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// rowCrossing is the cases' store's crossing
// ([sandbox.PendingStore.RowCrossing]): an hour, far past either write's own
// five seconds, so a budget that leaves it out is unmistakable.
const rowCrossing = time.Hour

// crossingMarks is a pending store whose connection takes rowCrossing to carry
// a run's row at its bound, recording when each write was handed over and its
// context's deadline, on the bubble clock. It answers every write as taken;
// any other call panics on the nil store underneath, since these writes
// call nothing else.
type crossingMarks struct {
	sandbox.PendingStore

	mu    sync.Mutex
	given map[string]handedMark
	done  chan string
}

// handedMark is one write as [crossingMarks] saw it.
type handedMark struct {
	at, deadline time.Time
	set          bool
}

func (s *crossingMarks) RowCrossing() time.Duration { return rowCrossing }

func (s *crossingMarks) note(ctx context.Context, write string) {
	deadline, set := ctx.Deadline()
	s.mu.Lock()
	s.given[write] = handedMark{at: time.Now(), deadline: deadline, set: set}
	s.mu.Unlock()
	s.done <- write
}

func (s *crossingMarks) MarkSessionStart(ctx context.Context, _, _ string, _ sandbox.SessionStart) (bool, error) {
	s.note(ctx, "MarkSessionStart")
	return true, nil
}

func (s *crossingMarks) MarkBridgeConnected(ctx context.Context, _, _ string) (bool, error) {
	s.note(ctx, "MarkBridgeConnected")
	return true, nil
}

func (s *crossingMarks) MarkPulled(ctx context.Context, _, _ string) (bool, error) {
	s.note(ctx, "MarkPulled")
	return true, nil
}

// The session-start, bridge-connect and pull-mark writes of a run's row are given their five
// seconds AND the time the store's connection takes to carry a row at its
// bound, read and written (sandbox.RowBudget): a non-`data` node reaches the
// store across its leaf link, and a row of up to six megabytes
// (sandbox.RowBytes) cannot cross a slow link in five seconds alone.
//
// The crossing time itself is the store's to say, held to a real link by
// internal/sandbox's TestARunsRowAtItsBoundWrittenAcrossALeafsLinkLands; this
// case reads only the engine's budget.
//
// Mutation: hold either write to its five seconds again, without the crossing
// (sandbox.RowBudget), and its row goes red.
func TestTheEnginesWritesOfARunsRowAreGivenItsCrossing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		mark   string
		reason time.Duration
		write  func(ctx context.Context, store sandbox.PendingStore)
	}{
		{
			name: "the session a job's box started", mark: "MarkSessionStart", reason: sessionStartRecordBudget,
			write: func(ctx context.Context, store sandbox.PendingStore) {
				newBridgeLedger(store, nil).recordStart(ctx, "t-session-start", "launch-1",
					sandbox.SessionStartContinued)
			},
		},
		{
			name: "a job's harness connecting its bridge", mark: "MarkBridgeConnected", reason: sessionStartRecordBudget,
			write: func(ctx context.Context, store sandbox.PendingStore) {
				newBridgeLedger(store, nil).recordConnected(ctx, "t-bridge-connected", "launch-1")
			},
		},
		{
			name: "a job's first pull of its notes", mark: "MarkPulled", reason: steerPublishBudget,
			write: func(ctx context.Context, store sandbox.PendingStore) {
				inbox := newRunSteers(steerSeals{}, store, nil).inbox(
					runIdentity{turn: steer.Turn{TurnID: "t-pull-mark", AgentHandle: "swe"}}, 1, "")
				inbox.recordPulled(ctx, "launch-1")
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := &crossingMarks{given: map[string]handedMark{}, done: make(chan string, 1)}
				c.write(t.Context(), store)
				if got := <-store.done; got != c.mark {
					t.Fatalf("the write made %s, want %s", got, c.mark)
				}
				store.mu.Lock()
				w := store.given[c.mark]
				store.mu.Unlock()
				if want := c.reason + rowCrossing; !w.set || w.deadline.Sub(w.at) != want {
					t.Errorf("%s was given %v (deadline set: %v), want its %v and the crossing of a row "+
						"at its bound, %v: %v", c.mark, w.deadline.Sub(w.at), w.set, c.reason, rowCrossing, want)
				}
			})
		})
	}
}
