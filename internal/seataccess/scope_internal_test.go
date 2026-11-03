package seataccess

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// THE REGISTRY REFUSES A LANE THAT REQUIRES A SCOPE IT DOES NOT HAVE: no agent
// could ever be turned on there.
func TestRegisterRefusesARequiredScopeOnAnUnscopedLane(t *testing.T) {
	t.Parallel()
	defer func() {
		if said, _ := recover().(string); !strings.Contains(said, "requires a scope it does not have") {
			t.Errorf("Register said %q", said)
		}
	}()
	Register(defaulter{cat: Catalog{Kind: integration.KindDatadog, Credentials: []string{"APP_KEY"},
		Lanes: []LaneSpec{{Lane: LaneDatadog, ScopeRequired: true}}}})
}
