package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// rowCrossingRate is the pace the cases below hold a leaf's link to, towards
// its member: 160 KiB a second, about five times the slowest a link is
// allowed to be (internal/objstore's MiBPace), so a run's row at its bound
// takes about thirty-eight seconds to cross — past every flat budget a write
// of a row was held to, five seconds and thirty, and far inside what a row at
// its bound is given at the slowest pace allowed.
const rowCrossingRate = 160 << 10

// rowCrossingRoom is how far short of what one key holds a case's row is
// written: the room the write a case makes adds to it — a carry, a stamp, an
// owed end, each a few hundred bytes — so the row it writes is at its bound
// still, and never past it.
const rowCrossingRoom = 8 << 10

// rowCrossingCase is one path that writes a run's row, through a coordinator
// whose node reaches the coordination store across a slow leaf link.
type rowCrossingCase struct {
	name string
	// row is the run as the case seeds it, before it is padded to its
	// bound.
	row PendingRun
	// write takes the path, and says how it came out where the path answers
	// at all: "" for a write the path says landed.
	write func(ctx context.Context, c *Coordinator, run PendingRun) string
	// landed reports whether the row read back once write returned holds
	// what the path wrote, and what it holds where it does not.
	landed func(PendingRun) (bool, string)
}

// A RUN'S ROW AT ITS BOUND, WRITTEN ACROSS A LEAF'S LINK, LANDS — through every
// path that writes one under a budget of its caller's.
//
// A node without the `data` role reaches the coordination store across its
// leaf link, and a run's row weighs up to what one key holds (RowBytes, six
// megabytes), written whole by every write of it. Each path below held its
// write to a flat budget of its caller's — a resumed turn's carry to five
// seconds, a capture's stamp to thirty for every attempt together, a turn's
// owed end to a teardown's thirty — which a row of megabytes cannot cross on a
// link slower than about 1.2 MiB a second at five, or 200 KiB a second at
// thirty. Cut while its bytes were still crossing, the write read as a store
// that did not answer: the carry went unrecorded, the stamp was asked again
// sending the whole row from its first byte until its budget ended, and the
// end was refused (crewlet/crewlet#1076). Here the link carries 160 KiB a
// second towards the member, so each row takes about thirty-eight seconds to
// cross, and each path's write lands, and its caller hears that it did.
//
// Mutation: hold any of these writes to its caller's flat budget again,
// without the crossing of a row at its bound (RowBudget), and its row goes
// red.
func TestARunsRowAtItsBoundWrittenAcrossALeafsLinkLands(t *testing.T) {
	t.Parallel()
	for _, c := range []rowCrossingCase{
		{
			name: "a resumed turn's carry",
			row: PendingRun{TurnID: "t-carry", AgentHandle: "swe", Status: StatusResumed,
				ClaimedFrom: StatusRunning, LaunchID: "launch-1", LaunchSeq: 1,
				Owner: rigLease.Owner, OwnerEpoch: rigLease.Epoch},
			write: func(ctx context.Context, c *Coordinator, run PendingRun) string {
				c.markResumeBegun(ctx, run, ResumeCarriesOn)
				return ""
			},
			landed: func(run PendingRun) (bool, string) {
				return run.Launch.ResumeCarry == ResumeCarriesOn, "the carry " + string(run.Launch.ResumeCarry)
			},
		},
		{
			name: "a capture's stamp",
			row: PendingRun{TurnID: "t-stamp", AgentHandle: "swe", Status: StatusRunning,
				LaunchID: "launch-1", LaunchSeq: 1, Owner: rigLease.Owner, OwnerEpoch: rigLease.Epoch},
			write: func(ctx context.Context, c *Coordinator, run PendingRun) string {
				stamp := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Launch: run.LaunchID,
					State: CaptureBegun, At: time.Now().UTC()}
				if !c.settleStamp(ctx, time.Time{}, run.TurnID, stamp, c.pending.StampCapture) {
					return "the stamp was not taken"
				}
				return ""
			},
			landed: func(run PendingRun) (bool, string) {
				return run.Capture != nil && run.Capture.State == CaptureBegun, "no begun capture"
			},
		},
		{
			name: "an owed end",
			row: PendingRun{TurnID: "t-end", AgentHandle: "swe", Status: StatusRunning,
				LaunchID: "launch-1", LaunchSeq: 1, Owner: rigLease.Owner, OwnerEpoch: rigLease.Epoch},
			write: func(ctx context.Context, c *Coordinator, run PendingRun) string {
				owed, err := c.OweEnd(ctx, run.TurnID, executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted})
				switch {
				case err != nil:
					return err.Error()
				case !owed:
					return "the end was not owed"
				}
				return ""
			},
			landed: func(run PendingRun) (bool, string) {
				return len(run.OwedEnd) > 0, "no owed end"
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			across := kvtest.SlowLeaf(t, rowCrossingRate)
			raw, err := encodeRun(c.row)
			if err != nil {
				t.Fatalf("encode the run: %v", err)
			}
			seeded := kvtest.AtTheBound(t, raw, rowCrossingRoom)
			// SEEDED THROUGH THE MEMBER, whose own connection crosses no
			// link: the case's time is the path's write, not its setup.
			if _, err := across.Member.CreateSandboxRun(t.Context(), c.row.TurnID, seeded); err != nil {
				t.Fatalf("seed a row of %d bytes: %v", len(seeded), err)
			}
			rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.Pending = NewCoordStore(across.Leaf) })
			if why := c.write(t.Context(), rig.coordinator, c.row); why != "" {
				t.Fatalf("a row of %d bytes written across a link carrying %d KiB a second: %s",
					len(seeded), rowCrossingRate>>10, why)
			}
			// READ BACK THROUGH THE MEMBER the moment the path returned: a
			// write its caller gave up on is still crossing then, and the
			// row does not hold it yet.
			run, found, err := NewCoordStore(across.Member).Get(t.Context(), c.row.TurnID)
			if err != nil || !found {
				t.Fatalf("read the row back: found %t, %v", found, err)
			}
			if ok, holds := c.landed(run); !ok {
				t.Errorf("the path returned before its write of a row of %d bytes landed: the row holds %s",
					len(seeded), holds)
			}
		})
	}
}
