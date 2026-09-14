package node_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/solo"
)

// fleet_test.go stands up a THREE-member JetStream cluster
// (jetstreamtest.StartCluster(t, 3, …)), so this package cannot share a runner
// with the rest of the suite.
//
// It did share one until this declaration existed, and passed — on the
// harness's four-attempt port-race retry rather than on having room. See
// `go doc ./internal/solo` for what that retry's own doc measured, and for why
// the declaration is an import rather than a build tag or a name filter.
func TestMain(m *testing.M) {
	os.Exit(solo.Run(m))
}
