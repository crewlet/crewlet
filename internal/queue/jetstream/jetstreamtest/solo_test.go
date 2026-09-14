package jetstreamtest

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/solo"
)

// This package IS the multi-member harness, and its own suite exercises it:
// partition_test.go stands up two three-member partitionable clusters to prove
// the partition harness actually partitions.
//
// So it is the one heavy package no import predicate can find — a package
// cannot import itself — which is why internal/solo's roster guard names it
// explicitly rather than deriving the whole set. See `go doc ./internal/solo`.
func TestMain(m *testing.M) {
	os.Exit(solo.Run(m))
}
