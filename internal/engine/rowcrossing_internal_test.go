package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// rowCrossingRate is the pace the cases below hold a leaf's link to, towards
// its member: 160 KiB a second, about five times the slowest a link is
// allowed to be (internal/objstore's MiBPace), so a run's row at its bound
// takes about thirty-eight seconds to cross — past the five seconds each write
// below was held to, and far inside what a row at its bound is given at the
// slowest pace allowed.
const rowCrossingRate = 160 << 10

// rowCrossingRoom is how far short of what one key holds a case's row is
// seeded: the room the write a case makes adds to it, a few dozen bytes, so
// the row it writes is at its bound still, and never past it.
const rowCrossingRoom = 8 << 10

// THE ENGINE'S WRITES OF A RUN'S ROW AT ITS BOUND, ACROSS A LEAF'S LINK, LAND.
//
// A node without the `data` role reaches the coordination store across its
// leaf link, and a run's row weighs up to what one key holds (sandbox.RowBytes,
// six megabytes), written whole by every write of it. The engine wrote two
// facts onto it under five seconds of their own — which session a job's box
// started, and that a job's harness has asked for notes — which a row of
// megabytes cannot cross on a link slower than about 1.2 MiB a second: cut
// while its bytes were still crossing, each read as a store that did not
// answer (crewlet/crewlet#1076). Here the link carries 160 KiB a second
// towards the member, so each row takes about thirty-eight seconds to cross,
// and each write lands before the call that made it returns.
//
// Mutation: hold either write to its five seconds again, without the crossing
// of a row at its bound (sandbox.RowBudget), and its row goes red.
func TestTheEnginesWritesOfARunsRowAtItsBoundAcrossALeafsLinkLand(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		write  func(ctx context.Context, store sandbox.PendingStore, turnID string)
		landed func(sandbox.PendingRun) bool
	}{
		{
			name: "the session a job's box started",
			write: func(ctx context.Context, store sandbox.PendingStore, turnID string) {
				newBridgeLedger(store, nil).recordStart(ctx, turnID, "launch-1", sandbox.SessionStartContinued)
			},
			landed: func(run sandbox.PendingRun) bool {
				return run.Launch.SessionStart == sandbox.SessionStartContinued
			},
		},
		{
			name: "a job's first pull of its notes",
			write: func(ctx context.Context, store sandbox.PendingStore, turnID string) {
				inbox := newRunSteers(steerSeals{}, store, nil).inbox(
					runIdentity{turn: steer.Turn{TurnID: turnID, AgentHandle: "swe"}}, 1)
				inbox.recordPulled(ctx, "launch-1")
			},
			landed: func(run sandbox.PendingRun) bool { return run.Steer.Pulled == "launch-1" },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			across := kvtest.SlowLeaf(t, rowCrossingRate)
			const turnID = "t-crossing"
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
			// hold it yet.
			run, found, err := sandbox.NewCoordStore(across.Member).Get(t.Context(), turnID)
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
