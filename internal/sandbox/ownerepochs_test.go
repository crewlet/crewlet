package sandbox

import (
	"testing"

	"github.com/google/uuid"

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
			AgentHandle: "h", CodingAgent: "claude-code"}, row.fence); err != nil {
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
// floored: failing the read over it stopped every node starting.
func TestOwnerEpochsSkipsARecordItCannotDecode(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := NewCoordStore(fleet)
	swe := uuid.New()
	if _, err := store.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentID: swe.String(),
		AgentHandle: "h", CodingAgent: "claude-code"}, Fence{Owner: "n1", Epoch: 4}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if _, err := fleet.CreateSandboxRun(t.Context(), "t-garbled", []byte("{not json")); err != nil {
		t.Fatalf("CreateSandboxRun: %v", err)
	}
	got, err := store.OwnerEpochs(t.Context())
	if err != nil || len(got) != 1 || got[swe] != 4 {
		t.Fatalf("OwnerEpochs = %v, %v; want swe at 4 with the garbled record skipped", got, err)
	}
}
