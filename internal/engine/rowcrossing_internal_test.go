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

// rowCrossing is the crossing the cases below give their store's connection
// ([sandbox.PendingStore.RowCrossing]): an hour, far past either write's own
// five seconds, so a budget that leaves it out is told apart from one that
// counts it by any margin a reader can see.
const rowCrossing = time.Hour

// crossingMarks is a pending store whose connection takes rowCrossing to carry
// a run's row at its bound, and which records, for each write the engine makes
// on a run's row off the path that asked for it, the instant it was handed the
// write and its context's deadline, on the case's bubble clock. It answers
// every write as taken. Any other call reaches the nil store underneath and
// panics, which is the case's mistake: these two writes call nothing else.
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

func (s *crossingMarks) MarkPulled(ctx context.Context, _, _ string) (bool, error) {
	s.note(ctx, "MarkPulled")
	return true, nil
}

// THE ENGINE'S WRITES OF A RUN'S ROW ARE GIVEN THE ROW'S CROSSING.
//
// A node without the `data` role reaches the coordination store across its
// leaf link, and a run's row weighs up to what one key holds (sandbox.RowBytes,
// six megabytes), read whole and written whole by every change of it. The
// engine wrote two facts onto it under five seconds of their own — which
// session a job's box started, and that a job's harness has asked for notes —
// which a row of megabytes cannot cross on a link slower than about 1.2 MiB a
// second: cut while its bytes were still crossing, each read as a store that
// did not answer (crewlet/crewlet#1076). Each is now given its five seconds
// AND the time the store's connection takes to carry a row at its bound, read
// and written (sandbox.RowBudget): the deadline each write is handed is
// exactly the two, on the case's clock.
//
// How long a row takes to cross is the store's to say, and the store's own
// suite holds it to a real link: internal/sandbox's
// TestARunsRowAtItsBoundWrittenAcrossALeafsLinkLands writes a row at its bound
// across a leaf's link held to 160 KiB a second, its read's reply included
// (crewlet/crewlet#1149). This case held these two writes to that same link,
// which spent three minutes of CI per run carrying bytes the store's suite
// already carries; the engine's part is the budget, and that is what it reads.
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
			name: "a job's first pull of its notes", mark: "MarkPulled", reason: steerPublishBudget,
			write: func(ctx context.Context, store sandbox.PendingStore) {
				inbox := newRunSteers(steerSeals{}, store, nil).inbox(
					runIdentity{turn: steer.Turn{TurnID: "t-pull-mark", AgentHandle: "swe"}}, 1)
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
