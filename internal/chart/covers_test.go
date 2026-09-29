package chart_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// ROWS BEHIND THEIR LOG CANNOT VOUCH FOR A SEAT'S ABSENCE.
//
// A caller judging a seat gone from what these rows derive — the mailbox
// sweep, which retires the seat's mail — must be refused while the record that
// hired the seat may be exactly the one not applied here, and answered once it
// has been.
func TestRowsBehindTheLogDoNotCoverIt(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reader().Covers(t.Context(), end+1); !errors.Is(err, chart.ErrNotCurrent) {
		t.Fatalf("rows against a log one record ahead answered %v, want ErrNotCurrent", err)
	}
	// THE CONTROL: against the log they have applied, they cover it.
	if err := r.reader().Covers(t.Context(), end); err != nil {
		t.Fatalf("current rows were refused: %v", err)
	}
}
