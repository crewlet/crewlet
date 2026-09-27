package kv

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/solo"
)

// walk_cluster_test.go stands up a THREE-member partitionable JetStream
// cluster (jetstreamtest.StartPartitionableCluster(t, 3, …)), so this package
// cannot share a runner with the rest of the suite.
//
// The case lives HERE rather than beside the harness's other users, because
// what it certifies is this package's unexported walk: it has to hand the
// certification a key index naming a key the cut member never received, and
// only package kv can reach that seam. Driven through the exported listings
// instead, the same defect would need a pass that lost a key AND a replica
// behind on that very key at once — two races, which is a case that proves
// nothing on the run where they do not meet. See `go doc ./internal/solo` for
// why the declaration is an import.
func TestMain(m *testing.M) {
	os.Exit(solo.Run(m))
}
