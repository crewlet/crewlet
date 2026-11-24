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

// A HARNESS'S LIST IS ANSWERED ONLY ONCE ITS JOB'S ROW SAYS IT CONNECTED. The
// bridge answers the list when the session's hook returns (connected.go there),
// so the ledger's record of the connection returns only once the store took
// it: a call or the harness's end, which follow the list, then never reach a
// collection that reads the row without the mark, and fails a run that listed.
//
// Mutation: record the connection off the hook's path (a goroutine), and the
// hook returns while the store still holds the write.
func TestAHarnessesListIsAnsweredOnlyOnceItsConnectionIsRecorded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &heldConnection{entered: make(chan struct{}), release: make(chan struct{})}
		ledger := newBridgeLedger(store, nil)
		ledger.bind(sandbox.OpenedJob{LaunchID: "launch-1", Round: "1"})
		returned := make(chan struct{})
		go func() {
			ledger.connected(t.Context(), "t-connected")
			close(returned)
		}()
		synctest.Wait()
		select {
		case <-store.entered:
		default:
			t.Fatal("the harness's list was told of without the connection's write being made")
		}
		select {
		case <-returned:
			t.Error("the hook returned, so the list was answered, while the store still held the write")
		default:
		}
		close(store.release)
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Fatal("the hook never returned once the store took the write")
		}
	})
}
