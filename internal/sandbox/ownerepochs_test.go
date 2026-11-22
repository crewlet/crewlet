package sandbox

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// EVERY EPOCH A SEAT'S COUNTER MUST STAY ABOVE: the highest each seat's runs
// carry, whatever their status, and nothing for a row naming no seat or
// stamped by no lease.
func TestOwnerEpochsIsTheHighestEachSeatsRunsCarry(t *testing.T) {
	t.Parallel()
	store := NewCoordStore(memory.NewFleet())
	swe, qa := uuid.New(), uuid.New()
	for _, row := range []struct {
		turn, agent string
		fence       Fence
	}{
		{"t1", swe.String(), Fence{Owner: "n1", Epoch: 2}},
		{"t2", swe.String(), Fence{Owner: "n1", Epoch: 5}},
		{"t3", qa.String(), Fence{Owner: "n1", Epoch: 1}},
		{"t4", qa.String(), Fence{}},
		{"t5", "not-a-seat", Fence{Owner: "n1", Epoch: 9}},
	} {
		if _, err := store.BeginLaunch(t.Context(), PendingRun{TurnID: row.turn, AgentID: row.agent,
			AgentHandle: "h", Harness: "claude-code"}, row.fence); err != nil {
			t.Fatalf("BeginLaunch(%s): %v", row.turn, err)
		}
	}
	got, err := store.OwnerEpochs(t.Context())
	if err != nil {
		t.Fatalf("OwnerEpochs: %v", err)
	}
	if len(got) != 2 || got[swe] != 5 || got[qa] != 1 {
		t.Fatalf("OwnerEpochs = %v, want swe at 5 and qa at 1", got)
	}
}

// ONE RECORD THAT DOES NOT DECODE IS SKIPPED, and every other seat is still
// floored: failing the read over it would stop every node starting.
func TestOwnerEpochsSkipsARecordItCannotDecode(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := NewCoordStore(fleet)
	swe := uuid.New()
	if _, err := store.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentID: swe.String(),
		AgentHandle: "h", Harness: "claude-code"}, Fence{Owner: "n1", Epoch: 4}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if _, err := fleet.CreateSandboxRun(t.Context(), "t-garbled", []byte("{not json"), nil); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	got, err := store.OwnerEpochs(t.Context())
	if err != nil || len(got) != 1 || got[swe] != 4 {
		t.Fatalf("OwnerEpochs = %v, %v; want swe at 4 with the garbled record skipped", got, err)
	}
}

// EVERY WRITE OF A RUN STATES ITS STANDING: the launch's create, and every
// change after it, a lease claimed and a status moved included. A write
// stating none leaves the record judged on its value, read whole, so a
// change that forgot it costs every later floor and seat listing a read.
//
// Mutation: drop the standing from writeFound's change, and the claim's
// write leaves the record stating none.
func TestEveryWriteOfARunStatesItsStanding(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := NewCoordStore(fleet)
	seat := uuid.New()
	stands := func(want coord.RunStanding, after string) {
		t.Helper()
		stood, err := fleet.SandboxRunStandings(ctx)
		if err != nil || len(stood) != 1 || stood[0].Standing == nil || *stood[0].Standing != want {
			t.Fatalf("after %s the run stands as %+v (%v), want epoch %d", after, stood, err, want.OwnerEpoch)
		}
	}
	if _, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentID: seat.String(), AgentHandle: "swe",
		Harness: "claude-code"}, Fence{Owner: "n1", Epoch: 3}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	stands(coord.StandingOf(seat, "swe", 3), "the launch")
	if won, err := store.ClaimOwnership(ctx, "t1", "n2", 7); err != nil || !won {
		t.Fatalf("ClaimOwnership = (%v, %v), want it won", won, err)
	}
	stands(coord.StandingOf(seat, "swe", 7), "the claim")
	if err := store.SetStatus(ctx, "t1", StatusRunning, Fence{Owner: "n2", Epoch: 7}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	stands(coord.StandingOf(seat, "swe", 7), "a status moved")
}
