package usage_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/solo"
)

// departed_test.go stands up a THREE-member partitionable JetStream cluster
// (jetstreamtest.StartPartitionableCluster(t, 3, …)) — the one shape in which a
// node can leave while the fleet keeps a quorum — so this package cannot share
// a runner with the rest of the suite. See `go doc ./internal/solo`.
func TestMain(m *testing.M) {
	os.Exit(solo.Run(m))
}
