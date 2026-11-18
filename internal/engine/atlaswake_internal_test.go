package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
)

// A SEAT THAT TAKES NO WORK IS NOT WOKEN ABOUT ITS ATLAS ENTRY (ADR-0143): the
// duty asks before it claims the day's wake, and a paused seat, a seat parked
// on a spent budget and a handle the chart does not hold all answer no.
//
// Mutation: drop the pause or the budget reading in seatTakesWork and its case
// answers yes, spending the seat's one wake of the day on mail it cannot read.
func TestASeatThatCannotStartATurnTakesNoAtlasWake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		handle        string
		paused, spent bool
		want          bool
	}{
		{name: "an agent seat with room", handle: "lead", want: true},
		{name: "a paused seat", handle: "lead", paused: true},
		{name: "a seat whose budget window is spent", handle: "lead", spent: true},
		{name: "a handle the chart does not hold", handle: "nobody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newParkRig(t, "100")
			if tc.spent {
				r.spend(t, 100)
			}
			pauses := map[uuid.UUID]coord.SeatPause{}
			if tc.paused {
				pauses[seatOf("lead")] = coord.SeatPause{Seat: seatOf("lead"), By: "jane", At: parkedAt}
			}
			r.e.adoptSeatPauses(t.Context(), pauses)
			got, err := r.e.seatTakesWork(t.Context(), tc.handle)
			if err != nil || got != tc.want {
				t.Fatalf("takes work = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

// A SEAT'S HOLDER IS THE STORE'S WORD, whatever this node's clock says: a seat
// held on a store whose clock runs ninety seconds behind this node's, its
// lease's deadline already past here, takes its atlas wake, and a seat no node
// holds takes none (the coord package doc, "A lease a read hands out is the
// store's word").
//
// Mutation: judge the lease on this node's clock in seatTakesWork, and the
// held seat takes no wake.
func TestASeatHeldOnTheStoresWordTakesItsAtlasWake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		held, want bool
	}{
		{name: "a seat held on a store whose clock runs behind", held: true, want: true},
		{name: "a seat no node holds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newParkRig(t, "100")
			store := &coordmem.Backend{Clock: func() time.Time { return time.Now().Add(-90 * time.Second) }}
			r.e.backends.Coord = store
			if tc.held {
				lease, _, err := store.TryAcquire(t.Context(), coord.SeatResource(seatOf("lead")),
					coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute})
				if err != nil || lease == nil {
					t.Fatalf("hold the Lead's seat: %v", err)
				}
			}
			r.e.adoptSeatPauses(t.Context(), nil)
			if got, err := r.e.seatTakesWork(t.Context(), "lead"); err != nil || got != tc.want {
				t.Fatalf("takes work = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

// WHAT CANNOT BE READ IS NOT "TAKES WORK": a node that has not read the
// pauses answers an error, and the duty leaves the seat for its next pass.
func TestASeatNobodyCanAskAboutIsUnknown(t *testing.T) {
	t.Parallel()
	r := newParkRig(t, "100")
	if got, err := r.e.seatTakesWork(t.Context(), "lead"); !errors.Is(err, errPausesUnread) || got {
		t.Fatalf("before the pauses are read, takes work = %v (%v), want unknown", got, err)
	}
}
