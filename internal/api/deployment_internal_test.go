package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY DEPLOYMENT WRITE ASKS FOR A RECENT PROOF, AND NO READ DOES.
//
// The deployment's own controls — a budget reset, a backup, the retention and
// capacity gestures — change what the engine does for everybody on it, so each
// asks for a proof inside `api.auth.session.step_up`; the two reads beside them
// (the maintenance status, the reanchor confirmation value) change nothing and
// ask for none. Walked over the routes this surface actually mounts, so a
// route added later is decided by the same rule without anybody adding it
// here. Internal because the route list is the mount's own and the walk must
// not stand the deployment up to read it.
func TestEveryDeploymentWriteAsksForARecentProofAndNoReadDoes(t *testing.T) {
	t.Parallel()
	var patterns []string
	(&App{}).deploymentRoutes(func(pattern string, _ http.HandlerFunc) {
		patterns = append(patterns, pattern)
	})
	if len(patterns) < 2 {
		t.Fatalf("the deployment surface mounted %v, so this walk certifies "+
			"nothing", patterns)
	}
	reads := 0
	for _, pattern := range patterns {
		policy := deploymentPolicy(pattern)
		grant, _ := authz.GrantOf(policy.Action)
		recency, _ := authz.RecencyOf(policy.Action)
		if grant != iam.GrantFleetOperate {
			t.Errorf("%s is decided on %s, want the deployment's own grant",
				pattern, grant)
		}
		read := strings.HasPrefix(pattern, http.MethodGet+" ")
		if read {
			reads++
		}
		switch {
		case read && recency.Demands():
			t.Errorf("the read %s asks for a %s proof", pattern, recency)
		case !read && recency != iam.RecencyStepUp:
			t.Errorf("the write %s asks for %q, want a proof inside step_up",
				pattern, recency)
		}
	}
	// THE CONTROL: both arms were reached, or the walk proves one of them.
	if reads == 0 || reads == len(patterns) {
		t.Errorf("the deployment surface mounted %d reads of %d routes, so "+
			"one arm of this walk certified nothing", reads, len(patterns))
	}
}
