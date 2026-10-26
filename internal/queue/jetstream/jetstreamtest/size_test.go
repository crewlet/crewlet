package jetstreamtest

import (
	"testing"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A MEMBER'S CAP IS DECLARED, never read off the runner's disk, and a base
// that declares its own keeps it.
//
// Every member of every cluster this harness starts has a store directory, so
// a member left to derive its cap takes three quarters of the runner's free
// space, and a case reserving gibibytes at three replicas passes or fails on
// how full the machine is (see brokertest). Read through StreamBudget, as a
// node reads it, on every member: each is its own server with its own cap.
//
// Mutation: drop the default in Cluster.start and the first row reads the
// disk's figure; apply it unconditionally and the second row reads sixteen
// gibibytes.
func TestAMembersCapIsDeclared(t *testing.T) {
	t.Parallel()
	const own = int64(8) << 30
	for name, tc := range map[string]struct {
		base js.Config
		want int64
	}{
		"a base that declares none": {js.Config{}, brokertest.StoreMaxBytes},
		"a base that declares one":  {js.Config{StoreMaxBytes: own}, own},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := StartCluster(t, 3, tc.base)
			for i := range c.Servers {
				budget, err := c.Client(t, i).StreamBudget(t.Context())
				if err != nil {
					t.Fatalf("member %d: StreamBudget: %v", i, err)
				}
				if budget.Source != js.BudgetServerStore || budget.Limit != tc.want {
					t.Errorf("member %d's budget = %+v, want its server's own "+
						"declared cap of %d", i, budget, tc.want)
				}
			}
		})
	}
}
