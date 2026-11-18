package kv

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/solo"
)

// walk_cluster_test.go stands up a THREE-member partitionable JetStream
// cluster, so this package cannot share a runner with the rest of the suite.
// The case lives here because it certifies this package's unexported walk,
// handing it a key index naming a key the cut member never received, a seam
// only package kv reaches; through the exported listings it would need two
// races to meet. `go doc ./internal/solo` has why the declaration is an import.
func TestMain(m *testing.M) {
	os.Exit(solo.Run(m))
}
