package jetstreamtest

import (
	"testing"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A CLUSTERED MEMBER STATES NO ROOM FOR AN UPDATE, and does for a create.
//
// A create is placed, so a member's own room is what the cluster weighs. An
// update is checked by the metadata leader against that server's reservations,
// which no other member can read, so a member that stated its own room there
// would refuse raises the broker grants. Only an account's limit is shared by
// every server, and an embedded cluster's account states none.
func TestAClusteredMemberStatesNoRoomForAnUpdate(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	q := c.Client(t, 0)
	room, err := q.GrowthBudget(t.Context())
	if err != nil {
		t.Fatalf("GrowthBudget: %v", err)
	}
	if room.Limit >= 0 || room.Source != js.BudgetUnstated {
		t.Errorf("a clustered member's growth budget = %+v, want unstated", room)
	}
	placed, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	if placed.Limit < 0 || placed.Source != js.BudgetServerStore {
		t.Errorf("a clustered member's create budget = %+v, want its own room", placed)
	}
}
