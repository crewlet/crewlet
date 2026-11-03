package replicated_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/solo"
)

// Every case here stands up a THREE-member cluster
// (jetstreamtest.StartCluster(…, 3, …)), so this package cannot share a runner
// with the rest of the suite — which is the whole reason it is a package
// apart from internal/objstore/natsobj. See `go doc ./internal/solo`.
func TestMain(m *testing.M) {
	os.Exit(solo.Run(m))
}
