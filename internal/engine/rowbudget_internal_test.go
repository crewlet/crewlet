package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// crossingRows is a pending store whose connection takes crossing to carry a
// run's row at its bound (sandbox.PendingStore.RowCrossing), and which records
// each write of a row a case watches — the instant it was handed it and its
// context's deadline, on the case's clock. Every other call is the store
// underneath's.
type crossingRows struct {
	sandbox.PendingStore
	crossing time.Duration

	mu    sync.Mutex
	given []rowWrite
}

// rowWrite is one write as [crossingRows] saw it.
type rowWrite struct {
	write        string
	at, deadline time.Time
	set          bool
}

func (s *crossingRows) RowCrossing() time.Duration { return s.crossing }

func (s *crossingRows) note(ctx context.Context, write string) {
	deadline, set := ctx.Deadline()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.given = append(s.given, rowWrite{write: write, at: time.Now(), deadline: deadline, set: set})
}

func (s *crossingRows) writes() []rowWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rowWrite(nil), s.given...)
}

func (s *crossingRows) MarkSessionStart(ctx context.Context, turnID, launch string,
	start sandbox.SessionStart) (bool, error) {
	s.note(ctx, "MarkSessionStart")
	return s.PendingStore.MarkSessionStart(ctx, turnID, launch, start)
}

func (s *crossingRows) MarkBridgeConnected(ctx context.Context, turnID, launch string) (bool, error) {
	s.note(ctx, "MarkBridgeConnected")
	return s.PendingStore.MarkBridgeConnected(ctx, turnID, launch)
}

func (s *crossingRows) MarkPulled(ctx context.Context, turnID, launch string) (bool, error) {
	s.note(ctx, "MarkPulled")
	return s.PendingStore.MarkPulled(ctx, turnID, launch)
}

func (s *crossingRows) SettleTake(ctx context.Context, turnID string, settle sandbox.NoteSettle) (
	[]sandbox.RunNote, error) {
	s.note(ctx, "SettleTake")
	return s.PendingStore.SettleTake(ctx, turnID, settle)
}

// The engine's own-budget writes of a run's row (the session start, the bridge
// connect, the pull mark, each settle attempt) get their reason's budget plus the time a row at
// its bound takes to cross (sandbox.RowBudget): a row is written whole, up to
// six megabytes, at a slow link's pace, and the store keeps a caller's
// deadline as it is. Exact, in a bubble.
//
// Mutation: hold any of them to its reason alone, and its row reads the
// crossing short.
func TestTheEnginesWritesOfARunsRowAreGivenTheRowsCrossingBesideTheirReason(t *testing.T) {
	t.Parallel()
	const crossing = 7 * time.Minute
	for _, c := range []struct {
		name  string
		write string
		want  time.Duration
		path  func(ctx context.Context, store sandbox.PendingStore)
	}{
		{
			name: "the session a job's box started", write: "MarkSessionStart",
			want: sessionStartRecordBudget + crossing,
			path: func(ctx context.Context, store sandbox.PendingStore) {
				newBridgeLedger(store, nil).recordStart(ctx, "t1", "launch-1", sandbox.SessionStartContinued)
			},
		},
		{
			name: "a job's harness connecting its bridge", write: "MarkBridgeConnected",
			want: sessionStartRecordBudget + crossing,
			path: func(ctx context.Context, store sandbox.PendingStore) {
				newBridgeLedger(store, nil).recordConnected(ctx, "t1", "launch-1")
			},
		},
		{
			name: "a job's first pull of its notes", write: "MarkPulled",
			want: steerPublishBudget + crossing,
			path: func(ctx context.Context, store sandbox.PendingStore) {
				newRunSteers(steerSeals{}, store, nil).inbox(
					runIdentity{turn: steer.Turn{TurnID: "t1", AgentHandle: "swe"}}, 1, "").recordPulled(ctx, "launch-1")
			},
		},
		{
			name: "an attempt at a settle of a take", write: "SettleTake",
			want: steerSettleAttempt + crossing,
			path: func(ctx context.Context, store sandbox.PendingStore) {
				_, _ = newRunSteers(steerSeals{}, store, nil).settleOnce(ctx, "t1",
					sandbox.NoteSettle{Take: "take-1"})
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := &crossingRows{PendingStore: sandbox.NewCoordStore(memory.NewFleet()), crossing: crossing}
				c.path(t.Context(), store)
				got := store.writes()
				if len(got) != 1 || got[0].write != c.write {
					names := make([]string, 0, len(got))
					for _, w := range got {
						names = append(names, w.write)
					}
					t.Fatalf("the path wrote the row %d times (%s), want one %s", len(got),
						strings.Join(names, ", "), c.write)
				}
				if given := got[0].deadline.Sub(got[0].at); !got[0].set || given != c.want {
					t.Errorf("%s was given %v (set %t), want %v: its reason and the crossing of a row at "+
						"its bound, %v", c.write, given, got[0].set, c.want, crossing)
				}
			})
		})
	}
}
