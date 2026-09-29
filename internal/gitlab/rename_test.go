package gitlab_test

import (
	"maps"
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

// AN ACCESS LEVEL OVERRIDE FOLLOWS THE SEAT IT NAMES, NOT THE HANDLE IT SPELLS.
//
// `access_levels` lives in the settings, and a rename in the chart cannot
// rewrite it. Looked up by the handle in hand, the override stopped applying
// the moment its seat was renamed — the seat dropped to the default at the
// next pass — and whoever was later given the old handle inherited the grant.
// Followed through the chart it stays with the seat; and a key that is
// ANOTHER seat's live handle is that seat's, however this one used to be
// called.
func TestAnAccessLevelOverrideFollowsTheSeatItNames(t *testing.T) {
	t.Parallel()
	cfg := enabledGitLab()
	cfg.Provisioning.AccessLevels = map[string]config.GitLabAccessLevel{
		"swe": config.GitLabMaintainer,
	}

	// RENAMED FROM `swe`: the override still names it.
	renamed := agentSeat("SWE", map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_SWE}"})
	renamed.DeclaredHandle, renamed.OriginHandle = "platform-swe", "swe"
	if got := gitlabPlanSeat(t, renamed, cfg).AccessLevel; got != string(config.GitLabMaintainer) {
		t.Errorf("the renamed seat's level = %q, want the maintainer override that names it", got)
	}

	// AND A SEAT NOW HOLDING THE HANDLE LIVE takes it from a seat that
	// merely used to answer to it — the chart's own precedence.
	holder := agentSeat("SWE Two", map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_SWE_2}"})
	holder.DeclaredHandle = "swe"
	former := agentSeat("Platform SWE", map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_PSWE}"})
	former.DeclaredHandle, former.OriginHandle = "platform-swe", "platform-swe-0"
	former.FormerHandles = []string{"swe"}
	plan, err := gitlab.PlanFor(provisioningOrg(t, former, holder), cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	levels := map[string]string{}
	for _, s := range plan.Seats {
		levels[s.Handle] = s.AccessLevel
	}
	if want := map[string]string{"swe": "maintainer", "platform-swe": ""}; !maps.Equal(levels, want) {
		t.Errorf("levels by seat = %v, want %v: the live holder of a handle is the seat it names", levels, want)
	}
}
