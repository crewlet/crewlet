package sandbox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// rowCrossingRate is the pace the cases hold a leaf's link to, each way, 160
// KiB a second: about five times the slowest a link is allowed
// (internal/objstore's MiBPace), so a row at its bound takes about thirty-eight
// seconds to cross as a write and its read's reply, base64 a third larger,
// about fifty-one back: past any flat budget of five or thirty seconds, yet far
// inside what a row at its bound is given at the slowest pace allowed.
const rowCrossingRate = 160 << 10

// rowCrossingRoom is how far short of what one key holds a case's row is
// seeded: room for what its write adds (a carry, a stamp, an owed end, each a
// few hundred bytes), so the row stays at its bound and never past it.
const rowCrossingRoom = 8 << 10

// rowCrossingCapture is the begun capture the keep's case stamps the outcome
// of, and the place the take's case reserves.
var rowCrossingCapture = SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Launch: "launch-1",
	State: CaptureBegun, At: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}

// rowCrossingCase is one path that writes a run's row, through a coordinator
// whose node reaches the coordination store across a slow leaf link.
type rowCrossingCase struct {
	name string
	// row is the run as the case seeds it, before padding to its bound.
	row PendingRun
	// write takes the path: "" where the path says its write landed, else
	// how it came out.
	write func(ctx context.Context, c *Coordinator, run PendingRun) string
	// landed reports whether the row read back once write returned holds
	// what the path wrote, and what it holds where it does not.
	landed func(PendingRun) (bool, string)
	// stamps is how many stamp asks the path makes when its change lands at
	// its first ask: one for a path that asks again while the store does not
	// answer ([Coordinator.askStamp]), none for one that changes the row once.
	stamps int32
}

// stampsAsked counts every [PendingStore.StampCapture] ask a case's path
// makes, so a stamp that landed on a later ask is told from one on its first.
type stampsAsked struct {
	PendingStore
	asked atomic.Int32
}

func (s *stampsAsked) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	s.asked.Add(1)
	return s.PendingStore.StampCapture(ctx, turnID, stamp)
}

// A RUN'S ROW AT ITS BOUND, CHANGED ACROSS A LEAF'S LINK, LANDS at its first
// ask, through every path that changes one under a budget of its caller's. A
// node without `data` reaches the coordination store across its leaf link, and
// every change reads and writes the whole row, up to RowBytes. A flat budget (a
// carry's five seconds, a stamp's or an owed end's thirty, a reservation
// sharing the scrub's seven minutes) cuts a change while its bytes still cross, as
// does re-asking the read before its reply arrives; at rowCrossingRate each
// read takes about fifty-one seconds and each write thirty-eight.
//
// Mutation: hold any change to its caller's flat budget instead of RowBudget,
// and its row goes red; read the row without its reply's crossing, and every
// row does, a stamp's and the reservation's second ask refused by their rows.
func TestARunsRowAtItsBoundWrittenAcrossALeafsLinkLands(t *testing.T) {
	t.Parallel()
	// ONE MEMBER AND ONE LEAF FOR EVERY CASE, run one after another: a
	// case's change is the only thing on the link, and the runner builds one
	// reply of a row at its bound at a time, each case under its own turn.
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
			// that took it, with no read's room to keep ([Coordinator.keepCapture]),
			// over the place its take reserved.
			name: "a keep's stamp of what came of its capture", stamps: 1,
			row: PendingRun{TurnID: "t-stamp", AgentHandle: "swe", Status: StatusRunning,
				LaunchID: "launch-1", LaunchSeq: 1, Owner: rigLease.Owner, OwnerEpoch: rigLease.Epoch,
				Capture: &rowCrossingCapture},
			write: func(ctx context.Context, c *Coordinator, run PendingRun) string {
				c.finishCapture(ctx, time.Time{}, run, rowCrossingCapture, CaptureRefused,
					executorkeep.LossStoreRefused, "the session could not be uploaded: the object store refused it")
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
			// THE TAKE'S RESERVATION OF ITS PLACE, held to the room alone the
			// take's context keeps for it beside the take's own budget
			// ([Coordinator.reserveRoom], [Coordinator.takeContext]): the case
			// reserves by the take's deadline less that budget, as though the
			// pack and the scrub had spent theirs, so the reservation lands in
			// the room or nowhere. A check of the room, not a replay of the take
			// ([Coordinator.takeCapture]): inside the take's budget it would
			// share the scrub's seven minutes, which a change of a row at its
			// bound outlasts at the slowest pace a link is allowed.
			name: "a take's reservation of its place", stamps: 1,
			row: PendingRun{TurnID: "t-reserve", AgentHandle: "swe", Status: StatusRunning,
				LaunchID: "launch-1", LaunchSeq: 1, Owner: rigLease.Owner, OwnerEpoch: rigLease.Epoch},
			write: func(ctx context.Context, c *Coordinator, run PendingRun) string {
				take, cancel, _ := c.takeContext(ctx)
				defer cancel()
				deadline, _ := take.Deadline()
				took, err := c.reserveCapture(take, deadline.Add(-SessionTakeBudget), run.TurnID,
					rowCrossingCapture)
				switch {
				case err != nil:
					return err.Error()
				case !took:
					return "the place was not taken"
				}
				return ""
			},
			landed: func(run PendingRun) (bool, string) {
				if run.Capture == nil {
					return false, "no capture"
				}
				return run.Capture.samePlace(rowCrossingCapture) && run.Capture.State == CaptureBegun,
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
			// SEEDED THROUGH THE MEMBER, whose connection crosses no link: the
			// case's time is the path's write, not its setup.
			if _, err := across.Member.CreateSandboxRun(t.Context(), c.row.TurnID, seeded, nil); err != nil {
				t.Fatalf("seed a row of %d bytes: %v", len(seeded), err)
			}
			store := &stampsAsked{PendingStore: NewCoordStore(across.Leaf)}
			rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.Pending = store })
			if why := c.write(t.Context(), rig.coordinator, c.row); why != "" {
				t.Fatalf("a row of %d bytes written across a link carrying %d KiB a second: %s",
					len(seeded), rowCrossingRate>>10, why)
			}
			// READ BACK THROUGH THE MEMBER the moment the path returned: a
			// write its caller gave up on is still crossing, and the row does
			// not hold it yet. Read once (kvtest's Across.Holds).
			held, found := across.Holds(t, c.row.TurnID)
			if !found {
				t.Fatalf("read the row back: the member holds no run %s", c.row.TurnID)
			}
			run, err := decodeRun(held)
			if err != nil {
				t.Fatalf("read the row back: %v", err)
			}
			if ok, holds := c.landed(run); !ok {
				t.Errorf("the path returned before its write of a row of %d bytes landed: the row holds %s",
					len(seeded), holds)
			}
			if asked := store.asked.Load(); asked != c.stamps {
				t.Errorf("the path asked the store for a stamp %d times, want %d: a change that landed "+
					"only once asked again was cut at its first ask", asked, c.stamps)
			}
		})
	}
}
