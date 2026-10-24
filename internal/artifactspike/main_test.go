package artifactspike

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/solo"
)

// The spikes stand up three-member embedded brokers, so the package runs
// alone (`go doc ./internal/solo` says why).
func TestMain(m *testing.M) { os.Exit(solo.Run(m)) }
