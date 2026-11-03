package seataccess_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"

	_ "github.com/crewlet/crewlet/internal/atlassian/agentaccess"
	_ "github.com/crewlet/crewlet/internal/cloudflare"
	_ "github.com/crewlet/crewlet/internal/datadog"
	_ "github.com/crewlet/crewlet/internal/expo"
	_ "github.com/crewlet/crewlet/internal/github"
	_ "github.com/crewlet/crewlet/internal/gitlab"
	_ "github.com/crewlet/crewlet/internal/mattermost"
	_ "github.com/crewlet/crewlet/internal/slack"
)

// THE DOCUMENT'S RULES AND THE VENDORS AGREE ON WHICH LANES REQUIRE A SCOPE, so
// `crewlet validate`, the access routes and the sync refuse the same agent.
func TestTheDocumentAndTheVendorsAgreeOnARequiredScope(t *testing.T) {
	t.Parallel()
	seen := 0
	for _, v := range seataccess.Registered() {
		for _, spec := range v.Catalog().Lanes {
			seen++
			if got := config.SeatScopeRequired(string(spec.Lane)); got != spec.ScopeRequired {
				t.Errorf("%s: config says scope required = %v, the vendor %v", spec.Lane, got, spec.ScopeRequired)
			}
		}
	}
	if seen != len(config.SeatAccessLanes()) {
		t.Fatalf("checked %d lanes of the %d the document has: a vendor is not registered here",
			seen, len(config.SeatAccessLanes()))
	}
}
