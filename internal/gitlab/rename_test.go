package gitlab_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// A RENAMED SEAT KEEPS THE SERVICE ACCOUNT IT IS ALREADY WORKING AS.
//
// GitLab stores no field of this engine's own, so the username is the whole
// of how a later pass recognises what it made. Built from the LIVE handle it
// moved on every rename: the next pass created a SECOND service account, the
// first went on authenticating with a token this engine had sealed, and
// `-decommission` deleted the first outright — a delete, here, not a disable.
//
// A COMPARISON rather than a literal, so it cannot pass by both halves
// changing together.
func TestARenamedSeatKeepsItsServiceAccount(t *testing.T) {
	t.Parallel()
	cfg := enabledGitLab()

	before := agentSeat("SWE", map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_SWE}"})
	after := agentSeat("SWE", map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_SWE}"})
	after.DeclaredHandle, after.OriginHandle = "platform-swe", "swe"

	was := gitlabPlanSeat(t, before, cfg)
	is := gitlabPlanSeat(t, after, cfg)

	if was.Handle == is.Handle {
		t.Fatalf("the fixture did not rename the seat: both plans say %q", was.Handle)
	}
	if is.Handle != "platform-swe" {
		t.Errorf("plan handle = %q, want the seat as the chart addresses it today", is.Handle)
	}
	if got, want := gitlab.Username(cfg.Provisioning, is.Origin),
		gitlab.Username(cfg.Provisioning, was.Origin); got != want {
		t.Errorf("username after the rename = %q, want the account it already "+
			"has, %q: a second is created and -decommission deletes the first",
			got, want)
	}
	// AND THE NAME ITS TOKENS CARRY, which the retire step matches on: a
	// name that moved would leave every earlier token live and unowned.
	if got, want := gitlab.TokenName(is.Origin), gitlab.TokenName(was.Origin); got != want {
		t.Errorf("token name after the rename = %q, want %q", got, want)
	}
}

// gitlabPlanSeat is the one seat gitlab.PlanFor makes of a role.
func gitlabPlanSeat(t *testing.T, role *org.Role, cfg *config.GitLab) provision.Seat {
	t.Helper()
	plan, err := gitlab.PlanFor(provisioningOrg(t, role), cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 {
		t.Fatalf("planned %d seats, want 1: %+v", len(plan.Seats), plan)
	}
	return plan.Seats[0]
}
