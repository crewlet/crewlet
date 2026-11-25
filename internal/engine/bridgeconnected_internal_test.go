package engine

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// heldConnection is a pending store whose first MarkBridgeConnected is held
// until the case releases it, saying when it was entered, and whose first
// failFirst tries answer the store gone. Any other call panics on the nil
// store underneath: recording the connection calls nothing else.
type heldConnection struct {
	sandbox.PendingStore

	entered   chan struct{}
	release   chan struct{}
	failFirst int

	mu    sync.Mutex
	tries int
}

func (s *heldConnection) RowCrossing() time.Duration { return 0 }

func (s *heldConnection) MarkBridgeConnected(context.Context, string, string) (bool, error) {
	s.mu.Lock()
	s.tries++
	try := s.tries
	s.mu.Unlock()
	if try == 1 && s.entered != nil {
		close(s.entered)
		<-s.release
	}
	if try <= s.failFirst {
		return false, errors.New("the store is gone")
	}
	return true, nil
}

func (s *heldConnection) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tries
}

// connectedLedger is a ledger over store whose harness listed its bridge, its
// marks in marks, its seat "ceo".
func connectedLedger(t *testing.T, store sandbox.PendingStore, marks *connectMarks) {
	t.Helper()
	ledger := newBridgeLedger(store, nil)
	ledger.marks, ledger.seat = marks, "ceo"
	ledger.log = slog.New(slog.DiscardHandler)
	ledger.bind(sandbox.OpenedJob{LaunchID: "launch-1", Round: "1"})
	ledger.connected(t.Context(), "t-connected")
}

// A HARNESS'S LIST IS NEVER HELD FOR ITS CONNECTION'S ROW WRITE, AND A
// COLLECTION RIGHT AFTER THE LIST STILL SEES THE CONNECTION. The bridge answers
// the list when the session's hook returns (connected.go there): the hook
// returns while the store still holds the write, so the harness's first
// request never waits on a row write (one to three seconds in a loaded fleet,
// past Grok Build's two-second handshake wait), and this node's collection
// ([connectMarks.settle]) returns only once the store took it, on the write's
// completion, so a run that listed is never read as one that did not.
//
// Mutations, each red: write on the hook's path; register the write after it
// starts, or never; let settle return before the write ends.
func TestAHarnessesListIsNotHeldForItsConnectionAndACollectionWaitsForIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &heldConnection{entered: make(chan struct{}), release: make(chan struct{})}
		marks := newConnectMarks()
		connectedLedger(t, store, marks)
		synctest.Wait()
		select {
		case <-store.entered:
		default:
			t.Fatal("the connection's write was never made, or the list waited on it")
		}
		settled := make(chan bool)
		go func() {
			unrecorded, _ := marks.settle(t.Context(), "t-connected")
			settled <- unrecorded
		}()
		synctest.Wait()
		select {
		case <-settled:
			t.Error("a collection read past the connection's write while the store still held it")
		default:
		}
		close(store.release)
		synctest.Wait()
		select {
		case unrecorded := <-settled:
			if unrecorded {
				t.Error("a write the store took was told to the collection as unrecorded")
			}
		default:
			t.Fatal("a collection waited on after the store took the write")
		}
		// NOTHING HELD for another run: its collection waits for nothing.
		if unrecorded, err := marks.settle(t.Context(), "t-other"); unrecorded || err != nil {
			t.Errorf("another run's settle = %v, %v; want nothing held", unrecorded, err)
		}
	})
}

// A CONNECTION WHOSE WRITE FAILS IS TRIED AGAIN, AND ONE THAT NEVER LANDS IS
// TOLD TO THE COLLECTION, never read as a harness that did not connect: the
// write is tried after each of appendRetryWaits, and settle then answers that
// this node saw the harness connect with nothing recorded, which the
// collection writes itself (sandbox's settledConnect). A settle whose context
// ends first answers its error, never "settled".
//
// Mutations, each red: try the write once; drop the mark once its tries fail;
// answer a settle cut short as settled.
func TestAConnectionWhoseWriteFailsIsToldToTheCollection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &heldConnection{failFirst: 1 << 30}
		marks := newConnectMarks()
		connectedLedger(t, store, marks)
		short, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := marks.settle(short, "t-connected"); err == nil {
			t.Error("a settle cut short before the tries ended answered settled")
		}
		unrecorded, err := marks.settle(t.Context(), "t-connected")
		if err != nil || !unrecorded {
			t.Errorf("settle after every try failed = %v, %v; want the connection unrecorded", unrecorded, err)
		}
		if got := store.attempts(); got != appendAttempts {
			t.Errorf("the write was tried %d times, want %d", got, appendAttempts)
		}
		if unrecorded, _ := marks.settle(t.Context(), "t-connected"); unrecorded {
			t.Error("a second settle was told again what the first one wrote")
		}
	})
}

// A SEAT LEAVING THIS NODE WAITS FOR ITS RUNS' CONNECTION WRITES and tries a
// failed one again before its lease is given back: the next holder collects
// the run and knows nothing of this node's marks. Another seat's runs are
// neither waited for nor forgotten.
//
// Mutations, each red: release without waiting; skip the failed one's try;
// forget another seat's marks.
func TestASeatLeavingWaitsForItsRunsConnectionWrites(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &heldConnection{entered: make(chan struct{}), release: make(chan struct{}),
			failFirst: appendAttempts}
		marks := newConnectMarks()
		connectedLedger(t, store, marks)
		other := &heldConnection{failFirst: 1 << 30}
		marks.record(t.Context(), "t-other", "cto", func(ctx context.Context) error {
			_, err := other.MarkBridgeConnected(ctx, "t-other", "launch-9")
			return err
		})
		released := make(chan struct{})
		go func() {
			marks.release(t.Context(), "ceo", slog.New(slog.DiscardHandler))
			close(released)
		}()
		synctest.Wait()
		select {
		case <-released:
			t.Fatal("the seat left while its run's connection write was under way")
		default:
		}
		close(store.release)
		synctest.Wait()
		<-released
		if got := store.attempts(); got != appendAttempts+1 {
			t.Errorf("the write was tried %d times, want its %d tries and the release's", got, appendAttempts)
		}
		if unrecorded, err := marks.settle(t.Context(), "t-connected"); unrecorded || err != nil {
			t.Errorf("the released run's mark stayed: %v, %v", unrecorded, err)
		}
		if unrecorded, err := marks.settle(t.Context(), "t-other"); !unrecorded || err != nil {
			t.Errorf("another seat's run, its write failed, settles %v, %v; want it still held", unrecorded, err)
		}
	})
}
