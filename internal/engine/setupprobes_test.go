package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/integration"
)

// EVERY INTEGRATION THIS BUILD SETS UP HAS A PROBE. Each one has a credential
// a person types into the connect form, so a kind added to integration.Kinds
// with no probe here is a Test button that fails on that form, and the setup
// surface refuses to be built without one.
func TestEveryIntegrationKindHasASetupProbe(t *testing.T) {
	t.Parallel()
	probes := engine.SetupProbes()
	for _, kind := range integration.Kinds {
		if probes[kind] == nil {
			t.Errorf("no setup probe for %s", kind)
		}
	}
	if len(probes) != len(integration.Kinds) {
		t.Errorf("%d probes for %d kinds: a probe for a kind this build does not "+
			"set up is one nothing can reach", len(probes), len(integration.Kinds))
	}
}
