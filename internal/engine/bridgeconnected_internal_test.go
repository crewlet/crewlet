package engine

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// heldConnection is a pending store whose MarkBridgeConnected is held until
// the case releases it, saying when it was entered. Any other call panics on
// the nil store underneath: recording the connection calls nothing else.
type heldConnection struct {
	sandbox.PendingStore

	entered chan struct{}
	release chan struct{}
}

func (s *heldConnection) RowCrossing() time.Duration { return 0 }

func (s *heldConnection) MarkBridgeConnected(context.Context, string, string) (bool, error) {
	close(s.entered)
	<-s.release
	return true, nil
}

// A HARNESS'S LIST IS NEVER HELD FOR ITS CONNECTION'S ROW WRITE, AND A
// COLLECTION WAITS FOR THAT WRITE. The bridge answers the list when the
// session's hook returns (connected.go there): the hook registers the write
// under way and returns while the store still holds it, so the harness's first
// request never waits on a row write (one to three seconds in a loaded fleet,
// past Grok Build's two-second handshake wait), and this node's collection
// ([connectMarks.settle]) returns only once the store took it, so a run that
// listed is never read as one that did not.
//
// Mutations, each red: write on the hook's path (no goroutine); register the
// write after it starts, or never; let settle return before the write ends.
func TestAHarnessesListIsNotHeldForItsConnectionAndACollectionWaitsForIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &heldConnection{entered: make(chan struct{}), release: make(chan struct{})}
		marks := newConnectMarks()
		ledger := newBridgeLedger(store, nil)
		ledger.marks = marks
		ledger.bind(sandbox.OpenedJob{LaunchID: "launch-1", Round: "1"})
		ledger.connected(t.Context(), "t-connected")
		synctest.Wait()
		select {
		case <-store.entered:
		default:
			t.Fatal("the connection's write was never made")
		}
		settled := make(chan struct{})
		go func() {
			marks.settle(t.Context(), "t-connected")
			close(settled)
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
		case <-settled:
		default:
			t.Fatal("a collection waited on after the store took the write")
		}
		// NOTHING UNDER WAY for another run: its collection waits for nothing.
		marks.settle(t.Context(), "t-other")
	})
}
