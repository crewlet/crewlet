package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	_ "github.com/crewlet/crewlet/internal/aws"         // registers a lane that takes grants
	_ "github.com/crewlet/crewlet/internal/googlecloud" // registers a lane that takes grants
	_ "github.com/crewlet/crewlet/internal/scaleway"    // registers a lane that takes grants
	"github.com/crewlet/crewlet/internal/seataccess"
)

// THE TOOL SPEAKS EVERY GRANT LANE'S OWN WORDS: its description names each
// lane's refusal and what a grant there names, so an agent refused at any of
// them knows to ask, and in what terms. Mutation: write one vendor's words
// into the description and the others go unnamed.
func TestRequestAccessNamesEveryGrantLanesRefusalAndPermission(t *testing.T) {
	t.Parallel()
	entry, ok := workRegistry(t, grantDeps(newFakeTracker(), newGrantDesk())).Lookup(builtin.RequestAccessTool)
	if !ok {
		t.Fatal("request_access is not registered")
	}
	desc := entry.Tool.Description()
	params := entry.Tool.Parameters()["properties"].(map[string]any)
	permission := params["permission"].(map[string]any)["description"].(string)
	for _, lane := range seataccess.GrantLanes() {
		terms := seataccess.GrantTermsOf(lane)
		if !strings.Contains(desc, terms.Refusal) {
			t.Errorf("the description names no %s refusal (%s)", lane, terms.Refusal)
		}
		if !strings.Contains(permission, terms.Permission) {
			t.Errorf("`permission` does not say what a %s grant names (%s)", lane, terms.Permission)
		}
	}
	if len(seataccess.GrantLanes()) < 4 {
		t.Fatalf("grant lanes %v, want Cloudflare, Scaleway, AWS and Google Cloud", seataccess.GrantLanes())
	}
}

// A GRANT NAMING NO ENTRY IS ASKED WHERE ITS LANE APPLIES IT: a Scaleway set
// with no projects is the agent's own projects, never every project the
// company's agents reach.
func TestAnUnscopedRequestIsAskedInItsLanesReach(t *testing.T) {
	t.Parallel()
	desk := newGrantDesk()
	desk.want = seataccess.Desired{Enabled: true, Level: "read", Scope: []string{"web"}}
	desk.opts = seataccess.Options{Lane: seataccess.LaneScaleway,
		Levels: []seataccess.Level{{ID: "read"}, {ID: "operate"}, {ID: "administer"}},
		Grants: []seataccess.GrantEntry{{ID: "KubernetesFullAccess", Name: "KubernetesFullAccess",
			ScopeKind: "project", Write: true}},
		GrantScope: []string{"web", "data"}}
	trk := newFakeTracker()
	got := requestOf(t, trk, desk, map[string]any{"integration": "scaleway", "permission": "KubernetesFullAccess"})
	if got.Failed {
		t.Fatalf("the request failed: %s", got.Output)
	}
	q := trk.patched[0].Comment.Decision.Question
	if !strings.Contains(q, "its own projects") || strings.Contains(q, "agents reach") {
		t.Errorf("question %q, want the agent's own projects", q)
	}
}
