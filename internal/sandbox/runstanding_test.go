package sandbox_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// standingRuns is a store of four runs: swe's and qa's, each launched and so
// stating its standing, and one of each an older build wrote, stating none.
type standingRuns struct {
	runs    *countedRuns
	store   *sandbox.CoordStore
	swe, qa uuid.UUID
}

func newStandingRuns(t *testing.T) standingRuns {
	t.Helper()
	ctx := t.Context()
	s := standingRuns{runs: &countedRuns{runStore: memory.NewFleet()}, swe: uuid.New(), qa: uuid.New()}
	s.store = sandbox.NewCoordStore(s.runs)
	for _, r := range []struct {
		turn, handle string
		seat         uuid.UUID
		epoch        int64
	}{{"t1", "swe", s.swe, 2}, {"t2", "qa", s.qa, 5}} {
		if _, err := s.store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: r.turn, AgentID: r.seat.String(),
			AgentHandle: r.handle, Harness: "claude-code"}, sandbox.Fence{Owner: "n1", Epoch: r.epoch}); err != nil {
			t.Fatalf("BeginLaunch %s: %v", r.turn, err)
		}
	}
	for _, r := range []struct {
		turn, handle string
		seat         uuid.UUID
		epoch        int64
	}{{"t3", "swe", s.swe, 9}, {"t4", "qa", s.qa, 7}} {
		older := fmt.Sprintf(`{"turn_id":%q,"agent_handle":%q,"agent_id":%q,"status":"running",`+
			`"launch_id":"an-older-job","launch_seq":1,"owner":"n0","owner_epoch":%d}`,
			r.turn, r.handle, r.seat, r.epoch)
		if made, err := s.runs.CreateSandboxRun(ctx, r.turn, []byte(older), nil); err != nil || !made {
			t.Fatalf("create an older build's row %s = (%v, %v)", r.turn, made, err)
		}
	}
	s.runs.listings.Store(0)
	s.runs.reads.Store(0)
	return s
}

// THE EPOCH FLOOR READS NO STATED RECORD WHOLE. Every node's boot floors each
// seat's counter above its runs' epochs; read off the records, a node across a
// leaf's link moved every record at boot. Off the standing it reads none, but
// a record an older build wrote, which states none: read whole and floored,
// since skipped it would let a seat's next holder mint below a parked run's
// lease while builds are mixed.
//
// Mutation: floor off the listing of records (CoordStore.list), and the
// records are listed; skip a run stating none, and swe floors at 2, qa at 5.
func TestTheEpochFloorReadsNoStatedRecordWhole(t *testing.T) {
	t.Parallel()
	s := newStandingRuns(t)
	floors, err := s.store.OwnerEpochs(t.Context())
	if err != nil {
		t.Fatalf("OwnerEpochs: %v", err)
	}
	if len(floors) != 2 || floors[s.swe] != 9 || floors[s.qa] != 7 {
		t.Errorf("OwnerEpochs = %v, want swe at 9 and qa at 7, the older build's runs", floors)
	}
	if listings, reads := s.runs.listings.Load(), s.runs.reads.Load(); listings != 0 || reads != 2 {
		t.Errorf("the floor listed the records %d times and read %d whole, want none listed and two "+
			"read, the runs stating no standing", listings, reads)
	}
}

// A SEAT'S LISTING READS ONLY ITS OWN RECORDS WHOLE: the busy check, recovery
// and a retirement ask after one seat, and selecting on the standing reads
// that seat's stated records and every one stating none, never another seat's
// stated one. A run stating none is held to the seat on its value, so another
// seat's older run never joins the listing while builds are mixed.
//
// Mutation: select through the listing of records (CoordStore.list), and
// every record is listed for one seat's answer; keep a run stating none
// without matching its seat, and qa's older run is swe's.
func TestASeatsListingReadsOnlyItsOwnRecordsWhole(t *testing.T) {
	t.Parallel()
	for name, list := range map[string]func(context.Context, standingRuns) ([]sandbox.PendingRun, error){
		"by handle": func(ctx context.Context, s standingRuns) ([]sandbox.PendingRun, error) {
			return s.store.ListActiveForSeat(ctx, "swe")
		},
		"by id": func(ctx context.Context, s standingRuns) ([]sandbox.PendingRun, error) {
			return s.store.ListActiveForSeatID(ctx, s.swe)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStandingRuns(t)
			runs, err := list(t.Context(), s)
			if err != nil {
				t.Fatalf("list swe's runs: %v", err)
			}
			if got := turnsOf(runs); !slices.Equal(got, []string{"t1", "t3"}) {
				t.Fatalf("swe's runs = %v, want t1 and t3", got)
			}
			if listings, reads := s.runs.listings.Load(), s.runs.reads.Load(); listings != 0 || reads != 3 {
				t.Errorf("swe's listing listed the records %d times and read %d whole, want none listed "+
					"and three read, swe's two and qa's stating none", listings, reads)
			}
		})
	}
}

// turnsOf is the turn ids of runs, sorted.
func turnsOf(runs []sandbox.PendingRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.TurnID)
	}
	slices.Sort(out)
	return out
}
