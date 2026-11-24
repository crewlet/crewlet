package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// connectRow is a pending store holding one row, which its write of the
// bridge connection marks unless markErr is set; any other call panics on the
// nil store underneath.
type connectRow struct {
	PendingStore
	row     PendingRun
	marked  int
	markErr error
}

func (s *connectRow) Get(context.Context, string) (PendingRun, bool, error) { return s.row, true, nil }

func (s *connectRow) MarkBridgeConnected(_ context.Context, _, launch string) (bool, error) {
	s.marked++
	if s.markErr != nil {
		return false, s.markErr
	}
	if s.row.LaunchID != launch {
		return false, nil
	}
	s.row.Launch.BridgeConnected = true
	return true, nil
}

// A COLLECTION READS A RUN'S BRIDGE CONNECTION ONCE THIS NODE'S WRITE OF IT HAS
// LANDED, right after the harness listed. The write is made off the harness's
// list, so a collection that read the row the launch handed it could fail a
// run whose harness listed; it settles the write held here and reads the row
// again. A read that is not a collection, or a row that holds the mark
// already, settles nothing.
//
// Mutation: skip the settle, or keep the row as read before it, and the run
// that listed reads as never connected.
func TestACollectionReadsTheBridgeConnectionOnceItsWriteHasLanded(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: "t-1", LaunchID: "launch-1"}
	store := &connectRow{row: run}
	settled := 0
	c := &Coordinator{pending: store, connectSettled: func(_ context.Context, turnID string) (bool, error) {
		settled++
		if turnID == "t-1" {
			store.row.Launch.BridgeConnected = true
		}
		return false, nil
	}}
	got, err := c.settledConnect(t.Context(), run, BoxReadCollect)
	if err != nil || !got.Launch.BridgeConnected || got.Handle().BridgeUnconnected {
		t.Errorf("a collection read the run as never connected after its write landed: %+v, %v", got.Launch, err)
	}
	if settled != 1 {
		t.Errorf("the collection settled %d times, want once", settled)
	}
	if got, _ := c.settledConnect(t.Context(), run, BoxReadStop); got.Launch.BridgeConnected || settled != 1 {
		t.Errorf("a stop's read settled the connection (settled %d)", settled)
	}
	connected := run
	connected.Launch.BridgeConnected = true
	if _, _ = c.settledConnect(t.Context(), connected, BoxReadCollect); settled != 1 {
		t.Errorf("a row holding the mark settled again (settled %d)", settled)
	}
	other := PendingRun{TurnID: "t-1", LaunchID: "launch-2"}
	store.row = PendingRun{TurnID: "t-1", LaunchID: "launch-1"}
	store.row.Launch.BridgeConnected = true
	if got, _ := c.settledConnect(t.Context(), other, BoxReadCollect); got.Launch.BridgeConnected {
		t.Error("a relaunch's run took the mark of the job its row names")
	}
	if store.marked != 0 {
		t.Errorf("a settled write was made again by the collection %d times", store.marked)
	}
}

// A CONNECTION WHOSE WRITE NEVER LANDED IS WRITTEN BY THE COLLECTION, OR THE
// COLLECTION FAILS NAMING THE STORE'S ERROR: this node saw the harness connect,
// so the run is never read as one that did not. A settle that could not
// finish fails the read too, which is retried, rather than judge the run.
//
// Mutations, each red: ignore the unrecorded answer; read on past a failed
// write or a settle's error.
func TestAConnectionWhoseWriteNeverLandedIsWrittenByTheCollection(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: "t-1", LaunchID: "launch-1"}
	store := &connectRow{row: run}
	c := &Coordinator{pending: store, connectSettled: func(context.Context, string) (bool, error) { return true, nil }}
	got, err := c.settledConnect(t.Context(), run, BoxReadCollect)
	if err != nil || !got.Launch.BridgeConnected || store.marked != 1 {
		t.Errorf("an unrecorded connection read %+v, %v after %d writes; want it written once", got.Launch, err,
			store.marked)
	}
	store.row, store.markErr = run, errors.New("the store is gone")
	if _, err = c.settledConnect(t.Context(), run, BoxReadCollect); err == nil ||
		!strings.Contains(err.Error(), "the store is gone") {
		t.Errorf("a write that failed again read the run on (%v); want the read failed naming the store's error", err)
	}
	c.connectSettled = func(context.Context, string) (bool, error) { return false, context.Canceled }
	if _, err = c.settledConnect(t.Context(), run, BoxReadCollect); !errors.Is(err, context.Canceled) {
		t.Errorf("a settle cut short read the run on (%v); want the read failed", err)
	}
}

// A COORDINATOR'S COLLECTION SETTLES THE CONNECTION before the box is read,
// here or by its host: a box read for collection asks the node once, and a
// stop's read does not.
//
// Mutation: read the box without [Coordinator.settledConnect], and this goes red.
func TestACoordinatorsCollectionWaitsForTheConnectionsWrite(t *testing.T) {
	t.Parallel()
	var asked []string
	rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
		o.ConnectSettled = func(_ context.Context, turnID string) (bool, error) {
			asked = append(asked, turnID)
			return false, nil
		}
	})
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	if _, _, err := rig.coordinator.boxResult(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
		t.Fatalf("boxResult: %v", err)
	}
	if len(asked) != 1 || asked[0] != "t1" {
		t.Errorf("the collection settled the connection %v, want once for t1", asked)
	}
}
