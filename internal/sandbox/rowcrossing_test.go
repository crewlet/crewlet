package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// rowCrossingRate is the pace the cases below hold a leaf's link to, each way:
// 160 KiB a second, about five times the slowest a link is allowed to be
// (internal/objstore's MiBPace), so a run's row at its bound takes about
// thirty-eight seconds to cross towards the member as a write, and its
// leader's reply to a read of it, base64-encoded a third larger, about
// fifty-one seconds back — each past every flat budget a change of a row was
// held to, five seconds and thirty, and the read past the thirty seconds a
// read was ever asked for, and far inside what a row at its bound is given at
// the slowest pace allowed.
const rowCrossingRate = 160 << 10

// rowCrossingRoom is how far short of what one key holds a case's row is
// written: the room the write a case makes adds to it — a carry, a stamp, an
// owed end, each a few hundred bytes — so the row it writes is at its bound
// still, and never past it.
const rowCrossingRoom = 8 << 10

// rowCrossingCapture is the capture the keep's case stamps what came of: the
// place its take reserved on the row, begun.
var rowCrossingCapture = SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Launch: "launch-1",
	State: CaptureBegun, At: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}

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

// A RUN'S ROW AT ITS BOUND, CHANGED ACROSS A LEAF'S LINK, LANDS — through every
// path that changes one under a budget of its caller's.
//
// A node without the `data` role reaches the coordination store across its
// leaf link, and a run's row weighs up to what one key holds (RowBytes, six
// megabytes), read whole and written whole by every change of it. Each path
// below held its change to a flat budget of its caller's — a resumed turn's
// carry to five seconds, a capture's stamp to thirty for every attempt
// together, a turn's owed end to a teardown's thirty — which a row of
// megabytes cannot cross on a link slower than about 1.2 MiB a second at
// five, or 200 KiB a second at thirty. Cut while its bytes were still
// crossing, the write read as a store that did not answer: the carry went
// unrecorded, the stamp was asked again sending the whole row from its first
// byte until its budget ended, and the end was refused (crewlet/crewlet#1076).
// And the read every change begins with was asked again at two seconds and at
// four while its reply was still on its way, each re-ask another whole reply
// on the link, and never for longer than thirty seconds, which a reply of
// eight mebibytes cannot cross on a link slower than about 280 KiB a second
// (crewlet/crewlet#1149). Here the link carries 160 KiB a second each way, so
// each row's read takes about fifty-one seconds and its write thirty-eight,
// and each path's change lands, and its caller hears that it did.
//
// Mutation: hold any of these changes to its caller's flat budget again,
// without the crossing of a change of a row at its bound (RowBudget), or read
// the row without its reply's arrival, and its row goes red.
func TestARunsRowAtItsBoundWrittenAcrossALeafsLinkLands(t *testing.T) {
	t.Parallel()
	// ONE MEMBER AND ONE LEAF FOR EVERY CASE, which run one after another: a
	// case's change is the only thing on the link, and the runner builds one
	// reply of a row at its bound at a time — each case its own row, under a
	// turn of its own.
	across := kvtest.SlowLeaf(t, rowCrossingRate)
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
			// THE KEEP'S STAMP OF WHAT CAME OF ITS CAPTURE, made off the read
			// that took it, with no read's room to keep ([Coordinator.keepCapture]):
			// over the place its take reserved, begun.
			name: "a keep's stamp of what came of its capture",
			row: PendingRun{TurnID: "t-stamp", AgentHandle: "swe", Status: StatusRunning,
				LaunchID: "launch-1", LaunchSeq: 1, Owner: rigLease.Owner, OwnerEpoch: rigLease.Epoch,
				Capture: &rowCrossingCapture},
			write: func(ctx context.Context, c *Coordinator, run PendingRun) string {
				c.finishCapture(ctx, time.Time{}, run, rowCrossingCapture, CaptureRefused,
					"the session could not be uploaded: the object store refused it")
				return ""
			},
			landed: func(run PendingRun) (bool, string) {
				if run.Capture == nil {
					return false, "no capture"
				}
				return run.Capture.samePlace(rowCrossingCapture) && run.Capture.State == CaptureRefused,
					"the capture " + string(run.Capture.State)
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
			// row does not hold it yet. Asked for as long as a leader read of
			// a row at its bound is ever asked on a member's own connection
			// — the lookup ceiling and the reply's handling — since its reply
			// carries the row whole: a reply that long in the making is the
			// runner's load, never what the case asks.
			read, cancel := context.WithTimeout(t.Context(),
				jsprovision.LookupBudget+jsapi.Handling(coord.MaxValueBytes))
			defer cancel()
			run, found, err := NewCoordStore(across.Member).Get(read, c.row.TurnID)
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
