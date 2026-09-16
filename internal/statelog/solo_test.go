package statelog_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/solo"
)

// barriercluster_test.go stands up THREE-member partitionable clusters
// (jetstreamtest.StartPartitionableCluster(t, 3, …)) — twice — so this package
// cannot share a runner with the rest of the suite.
//
// Like internal/node, it shared one until this declaration existed. See
// `go doc ./internal/solo`.
func TestMain(m *testing.M) {
	os.Exit(solo.Run(m))
}
