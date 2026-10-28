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
