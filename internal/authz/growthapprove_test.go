package authz_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A GROWTH ACT IS ANSWERED BY AN ADMINISTRATOR IN PERSON, AT A STEP-UP: never
// the seat's lead, a person a list names, an agent, a machine token holding
// admin, or an administrator whose proof is past the step-up window.
//
// Mutation: drop the presence bar, the human-only bar or the step-up recency
// from any of the three rows, or make one ClassNamed or ClassOwnOrLead, and a
// case here is allowed.
func TestAGrowthActIsAnsweredByAnAdministratorInPerson(t *testing.T) {
	t.Parallel()
	admin := iam.GrantAdmin
	stale := person("founder", admin)
	stale.ReauthAt = decidedAt.Add(-time.Hour)
	object := authz.Object{Kind: authz.KindPerson, ID: "agent-1", Owner: "sre", Named: []string{"marketer"}}
	for _, a := range []authz.Action{authz.ActionGrowthSpendApprove, authz.ActionGrowthPublicApprove,
		authz.ActionGrowthAdminApprove} {
		for _, c := range []struct {
			name    string
			p       iam.Principal
			allowed bool
		}{
			{"an administrator who stepped up", person("founder", admin), true},
			{"an administrator past the step-up window", stale, false},
			{"the seat's lead", personLeading("cto"), false},
			{"a person the integration names", personLeading("marketer"), false},
			{"a machine token holding admin", machine("pipeline", admin), false},
			{"an agent seat", seat("sre", admin), false},
		} {
			got := authz.Decide(t.Context(), c.p, a, object, nimbus(), decidedAt)
			if got.Allowed != c.allowed {
				t.Errorf("%s by %s: allowed %v (%s), want %v", a, c.name, got.Allowed, got.Reason, c.allowed)
			}
		}
	}
}
