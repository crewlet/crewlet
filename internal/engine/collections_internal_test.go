package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A NODE READS AS MANY OF ITS RUNS' BOXES AT ONCE AS IT RUNS TURNS: its
// coordinator is handed node.max_concurrent as the node's gate resolves it,
// the default where the key is absent. A completion ended at its turn's
// deadline, a stop's read and a loss's pass no gate, so this bound is theirs.
//
// Mutation: hand the coordinator no Collections, or max_concurrent unresolved,
// and a case goes red.
func TestANodeReadsAsManyOfItsRunsBoxesAtOnceAsItRunsTurns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		configured, want int
	}{
		{0, node.DefaultMaxConcurrent},
		{7, 7},
	} {
		b := testBootstrap(t)
		b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
		b.Node.MaxConcurrent = tc.configured
		e, err := New(t.Context(), Options{Bootstrap: &b})
		if err != nil {
			t.Fatalf("New (max_concurrent %d): %v", tc.configured, err)
		}
		t.Cleanup(func() { e.Stop(context.Background()) })
		if got := e.coordinatorOptions(nil, nil).Collections; got != tc.want {
			t.Errorf("max_concurrent %d: the coordinator reads %d of its runs' boxes at once, want %d, "+
				"the turns the node's gate admits", tc.configured, got, tc.want)
		}
	}
}
