package sandbox

import (
	"context"
	"testing"
)

// connectRow is a pending store holding one row, which its write of the
// bridge connection (the case's settle) marks; any other call panics on the
// nil store underneath.
type connectRow struct {
	PendingStore
	row PendingRun
}

func (s *connectRow) Get(context.Context, string) (PendingRun, bool, error) { return s.row, true, nil }

// A COLLECTION READS A RUN'S BRIDGE CONNECTION ONCE THIS NODE'S WRITE OF IT HAS
// LANDED. The write is made off the harness's list, so a collection that read
// the row the launch handed it could fail a run whose harness listed; it waits
// for the write under way here and reads the row again. A read that is not a
// collection, or a row that holds the mark already, waits for nothing.
//
// Mutation: skip the wait, or keep the row as read before it, and the run
// that listed reads as never connected.
func TestACollectionReadsTheBridgeConnectionOnceItsWriteHasLanded(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: "t-1", LaunchID: "launch-1"}
	store := &connectRow{row: run}
	waited := 0
	c := &Coordinator{pending: store, connectSettled: func(_ context.Context, turnID string) {
		waited++
		if turnID == "t-1" {
			store.row.Launch.BridgeConnected = true
		}
	}}
	if got := c.settledConnect(t.Context(), run, BoxReadCollect); !got.Launch.BridgeConnected || got.Handle().BridgeUnconnected {
		t.Errorf("a collection read the run as never connected after its write landed: %+v", got.Launch)
	}
	if waited != 1 {
		t.Errorf("the collection waited %d times, want once", waited)
	}
	if got := c.settledConnect(t.Context(), run, BoxReadStop); got.Launch.BridgeConnected || waited != 1 {
		t.Errorf("a stop's read waited for the connection's write (waited %d)", waited)
	}
	connected := run
	connected.Launch.BridgeConnected = true
	if c.settledConnect(t.Context(), connected, BoxReadCollect); waited != 1 {
		t.Errorf("a row holding the mark waited again (waited %d)", waited)
	}
	other := PendingRun{TurnID: "t-1", LaunchID: "launch-2"}
	store.row = PendingRun{TurnID: "t-1", LaunchID: "launch-1"}
	store.row.Launch.BridgeConnected = true
	if got := c.settledConnect(t.Context(), other, BoxReadCollect); got.Launch.BridgeConnected {
		t.Error("a relaunch's run took the mark of the job its row names")
	}
}

// A COORDINATOR'S COLLECTION WAITS FOR THE CONNECTION'S WRITE before it hands
// the runner its job's handle: a box read for collection asks the node for the
// write under way, once, and a stop's read does not.
//
// Mutation: read the box without [Coordinator.settledConnect], and this goes red.
func TestACoordinatorsCollectionWaitsForTheConnectionsWrite(t *testing.T) {
	t.Parallel()
	var asked []string
	rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
		o.ConnectSettled = func(_ context.Context, turnID string) { asked = append(asked, turnID) }
	})
	rig.launch("t1")
	rig.runner.Finish(Result{Success: true, Text: "done"})
	if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
		t.Fatalf("readBox: %v", err)
	}
	if len(asked) != 1 || asked[0] != "t1" {
		t.Errorf("the collection asked for the connection's write %v, want once for t1", asked)
	}
}
