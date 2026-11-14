package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// rowCrossingRate is the pace the cases below hold a leaf's link to, each way:
// 160 KiB a second, about five times the slowest a link is allowed to be
// (internal/objstore's MiBPace), so a run's row at its bound takes about
// thirty-eight seconds to cross towards the member as a write, and its
// leader's reply to a read of it about fifty-one back — each past the five
// seconds each change below was held to, the read past the thirty seconds a
// read was ever asked for, and far inside what a row at its bound is given at
// the slowest pace allowed.
const rowCrossingRate = 160 << 10

// rowCrossingRoom is how far short of what one key holds a case's row is
// seeded: the room the write a case makes adds to it, a few dozen bytes, so
// the row it writes is at its bound still, and never past it.
const rowCrossingRoom = 8 << 10

// THE ENGINE'S WRITES OF A RUN'S ROW AT ITS BOUND, ACROSS A LEAF'S LINK, LAND.
//
// A node without the `data` role reaches the coordination store across its
// leaf link, and a run's row weighs up to what one key holds (sandbox.RowBytes,
// six megabytes), read whole and written whole by every change of it. The
// engine wrote two facts onto it under five seconds of their own — which
// session a job's box started, and that a job's harness has asked for notes —
// which a row of megabytes cannot cross on a link slower than about 1.2 MiB a
// second: cut while its bytes were still crossing, each read as a store that
// did not answer (crewlet/crewlet#1076); and the read each change begins with
// was asked for no longer than thirty seconds, which a reply of eight
// mebibytes cannot cross on a link slower than about 280 KiB a second
// (crewlet/crewlet#1149). Here the link carries 160 KiB a second each way, so
// each row's read takes about fifty-one seconds and its write thirty-eight,
// and each change lands before the call that made it returns.
//
// Mutation: hold either change to its five seconds again, without the
// crossing of a change of a row at its bound (sandbox.RowBudget), or read the
// row without its reply's arrival, and its row goes red.
func TestTheEnginesWritesOfARunsRowAtItsBoundAcrossALeafsLinkLand(t *testing.T) {
	t.Parallel()
	// ONE MEMBER AND ONE LEAF FOR EVERY CASE, which run one after another: a
	// case's write is the only thing on the link, and the runner builds one
	// reply of a row at its bound at a time — each case its own row, under a
	// turn of its own.
	across := kvtest.SlowLeaf(t, rowCrossingRate)
	for _, c := range []struct {
		name   string
		turnID string
		write  func(ctx context.Context, store sandbox.PendingStore, turnID string)
		landed func(sandbox.PendingRun) bool
	}{
		{
			name: "the session a job's box started", turnID: "t-session-start",
			write: func(ctx context.Context, store sandbox.PendingStore, turnID string) {
				newBridgeLedger(store, nil).recordStart(ctx, turnID, "launch-1", sandbox.SessionStartContinued)
			},
			landed: func(run sandbox.PendingRun) bool {
				return run.Launch.SessionStart == sandbox.SessionStartContinued
			},
		},
		{
			name: "a job's first pull of its notes", turnID: "t-pull-mark",
			write: func(ctx context.Context, store sandbox.PendingStore, turnID string) {
				inbox := newRunSteers(steerSeals{}, store, nil).inbox(
					runIdentity{turn: steer.Turn{TurnID: turnID, AgentHandle: "swe"}}, 1)
				inbox.recordPulled(ctx, "launch-1")
			},
			landed: func(run sandbox.PendingRun) bool { return run.Steer.Pulled == "launch-1" },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			turnID := c.turnID
			raw, err := json.Marshal(sandbox.PendingRun{TurnID: turnID, AgentHandle: "swe",
				Status: sandbox.StatusRunning, LaunchID: "launch-1", LaunchSeq: 1})
			if err != nil {
				t.Fatalf("encode the run: %v", err)
			}
			seeded := kvtest.AtTheBound(t, raw, rowCrossingRoom)
			// SEEDED THROUGH THE MEMBER, whose own connection crosses no
			// link: the case's time is the write's, not its setup's.
			if _, err := across.Member.CreateSandboxRun(t.Context(), turnID, seeded); err != nil {
				t.Fatalf("seed a row of %d bytes: %v", len(seeded), err)
			}
			c.write(t.Context(), sandbox.NewCoordStore(across.Leaf), turnID)
			// READ BACK THROUGH THE MEMBER the moment the write returned: a
			// write cut short is still crossing then, and the row does not
			// hold it yet. Asked for as long as a leader read of a row at its
			// bound is ever asked on a member's own connection — the lookup
			// ceiling and the reply's handling — since its reply carries the
			// row whole: a reply that long in the making is the runner's
			// load, never what the case asks.
			read, cancel := context.WithTimeout(t.Context(),
				jsprovision.LookupBudget+jsapi.Handling(coord.MaxValueBytes))
			defer cancel()
			run, found, err := sandbox.NewCoordStore(across.Member).Get(read, turnID)
			if err != nil || !found {
				t.Fatalf("read the row back: found %t, %v", found, err)
			}
			if !c.landed(run) {
				t.Errorf("the write of a row of %d bytes across a link carrying %d KiB a second "+
					"returned before it landed", len(seeded), rowCrossingRate>>10)
			}
		})
	}
}
