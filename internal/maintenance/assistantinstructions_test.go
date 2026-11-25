package maintenance_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/maintenance"
)

// A SEAT THAT LEFT LOSES ITS ASK CREWLET INSTRUCTIONS AT ITS HOME BOARD'S
// GRACE, by a job of their own, and a seat the company has keeps its own.
// Mutation: drop the job, or judge it at another grace, and this is red.
func TestASeatThatLeftLosesItsInstructionsWithItsHomeBoard(t *testing.T) {
	t.Parallel()
	fleet, roster, clock := coordmem.NewFleet(), &roster{}, &clock{at: base}
	jobs := maintenance.InstructionsJobs(fleet, roster.humanSeats)
	if len(jobs) != 1 || jobs[0].Name != "assistant_instructions" || jobs[0].Scope != maintenance.Fleet ||
		jobs[0].Horizon == nil || jobs[0].Horizon() != boardGrace {
		t.Fatalf("jobs = %+v, want one assistant_instructions fleet job at %v", jobs, boardGrace)
	}
	w := newWorker(t, maintenance.Options{Now: clock.now, Jobs: jobs})
	ctx := context.Background()
	for _, handle := range []string{"ceo", "former"} {
		if _, ok, err := fleet.SaveSeatInstructions(ctx, handle, json.RawMessage(`{"length":"brief"}`),
			base, 0); err != nil || !ok {
			t.Fatalf("SaveSeatInstructions(%s) = (%v, %v)", handle, ok, err)
		}
	}
	roster.set("ceo")
	// The first sweep sees the seat gone; one a grace later forgets it.
	for _, at := range []time.Time{base, base.Add(boardGrace)} {
		clock.set(at)
		if _, err := w.Tick(ctx); err != nil {
			t.Fatalf("tick at %v: %v", at, err)
		}
	}
	if _, _, found, _ := fleet.SeatInstructions(ctx, "former"); found {
		t.Error("a seat absent for the grace kept its instructions")
	}
	if _, _, found, _ := fleet.SeatInstructions(ctx, "ceo"); !found {
		t.Error("a seat the company has lost its instructions")
	}
	if got := maintenance.InstructionsJobs(nil, roster.humanSeats); len(got) != 0 {
		t.Fatalf("no store built %d jobs", len(got))
	}
}
