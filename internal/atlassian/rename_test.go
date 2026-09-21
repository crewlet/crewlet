package atlassian_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// A RENAMED SEAT KEEPS THE ATLASSIAN ACCOUNT IT IS ALREADY WORKING AS.
//
// Atlassian is where a marker built on a moving value has already been shown
// to fail once: the description was the natural place for one, Atlassian
// accepts it and stores nothing, and four passes made four accounts for one
// agent before a listing showed it. A marker carrying the LIVE handle fails
// in exactly the same way for exactly the same reason — the next pass
// recognises none of this seat's accounts — except that the trigger is a
// rename rather than every single run.
//
// THE DISPLAY NAME IS BOTH LABEL AND KEY here, so the two halves are asserted
// apart: the label follows the role as the chart names it today, and the
// marker does not move at all.
func TestARenamedSeatKeepsItsAccountMarker(t *testing.T) {
	t.Parallel()
	before := atlassianSeat("SRE Lead")
	after := atlassianSeat("Head of Reliability")
	after.DeclaredHandle, after.OriginHandle = "head-reliability", "sre-lead"

	was := atlassianPlanSeat(t, before)
	is := atlassianPlanSeat(t, after)

	if was.Handle == is.Handle {
		t.Fatalf("the fixture did not rename the seat: both plans say %q", was.Handle)
	}
	if is.Handle != "head-reliability" {
		t.Errorf("plan handle = %q, want the seat as the chart addresses it today", is.Handle)
	}

	wasName := atlassian.AccountName(was.Role, was.Origin)
	isName := atlassian.AccountName(is.Role, is.Origin)

	// THE MARKER, which is how the pass finds the account it made.
	if got, want := atlassian.OriginFrom(isName), atlassian.OriginFrom(wasName); got != want {
		t.Errorf("account marker after the rename = %q, want the one the "+
			"account already carries, %q: the pass recognises none of this "+
			"seat's accounts and makes another", got, want)
	}
	// AND THE LABEL, which is the half that SHOULD follow the chart: an
	// account whose marker is frozen and whose label is frozen too is a row
	// in somebody's user list naming a role the company no longer has.
	if isName == wasName {
		t.Errorf("account name = %q for both roles, want the label to follow "+
			"the role the chart names today", isName)
	}
}

func atlassianSeat(name string) *org.Role {
	return &org.Role{Name: name, MCPEnv: map[string]map[string]string{
		"jira": {
			"JIRA_API_TOKEN": "${ATLASSIAN_TOKEN_SEAT}",
			"JIRA_USERNAME":  "${ATLASSIAN_EMAIL_SEAT}",
		},
	}}
}

// atlassianPlanSeat is the one seat atlassian.PlanFor makes of a role.
func atlassianPlanSeat(t *testing.T, role *org.Role) provision.Seat {
	t.Helper()
	plan, err := atlassian.PlanFor(&org.Organization{
		Name: "Nimbus", Roles: []*org.Role{role},
	})
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 {
		t.Fatalf("planned %d seats, want 1: %+v", len(plan.Seats), plan)
	}
	return plan.Seats[0]
}
