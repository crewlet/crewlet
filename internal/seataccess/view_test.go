package seataccess_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// githubOffer is GitHub's permission catalogue as the access route offers it.
func githubOffer() seataccess.Options {
	return seataccess.Options{Lane: seataccess.LaneGitHub, Scoped: true,
		ScopeNames: map[string]string{"101": "acme/api"},
		Permissions: []seataccess.PermissionSpec{{Key: "contents", Name: "Contents"},
			{Key: "pull_requests", Name: "Pull requests"},
			{Key: "workflows", Name: "Workflows", WriteOnly: true}}}
}

// THE GRANT, WHAT THE TOKENS CARRY AND WHAT THE APP HOLDS ARE THREE FACTS, and
// what the app holds past the grant is said as unused rather than hidden: the
// gap that decides an agent's missing permission is in Crewlet's grant. The
// two clocks are the vendor read (every pass) and the last writing sync.
// Mutation: compare the ceiling with what the tokens carry, or drop it from the
// view, and Unused is wrong or empty.
func TestALaneViewSaysWhatTheGrantLeavesUnused(t *testing.T) {
	t.Parallel()
	checked := time.Date(2026, 11, 10, 11, 48, 0, 0, time.UTC)
	applied := checked.Add(-10 * time.Minute)
	grant := seataccess.Permissions{"contents": "write", "pull_requests": "write"}
	answer := seataccess.Answer{Kind: integration.KindGitHub,
		Options: []seataccess.Options{githubOffer()},
		Seats: []seataccess.SeatAnswer{{Handle: "sre", Name: "Site Reliability Engineer",
			Lanes: map[seataccess.Lane]seataccess.LaneAnswer{seataccess.LaneGitHub: {
				Desired: seataccess.Desired{Enabled: true, Permissions: grant, Scope: []string{"101"}},
				Observed: seataccess.Observed{Exists: true, Enabled: true, Account: "acme-sre[bot]",
					Permissions: grant,
					Ceiling: seataccess.Permissions{"contents": "write", "pull_requests": "write",
						"workflows": "write"}},
				ObservedAt: checked,
				Sync:       &seataccess.SyncResult{At: applied, Outcome: seataccess.OutcomeOK},
				Tool:       seataccess.ToolLive, Plan: []seataccess.Step{}}}}}}

	view, ok := seataccess.ViewOf(answer, "sre", seataccess.LaneGitHub)
	if !ok {
		t.Fatal("the agent's lane was not found")
	}
	want := seataccess.LaneView{Kind: integration.KindGitHub, Lane: seataccess.LaneGitHub,
		Integration: "GitHub", Handle: "sre", Name: "Site Reliability Engineer",
		Status: seataccess.StatusInSync, StatusLabel: "In sync",
		Grant:    "Contents Read and write, Pull requests Read and write",
		Holds:    "Contents Read and write, Pull requests Read and write",
		AppHolds: "Contents Read and write, Pull requests Read and write, Workflows Read and write",
		Unused:   "Workflows Read and write", Scope: "acme/api", Account: "acme-sre[bot]",
		Tool: seataccess.ToolLive, CheckedAt: checked, AppliedAt: applied, Outcome: "Applied",
		Plan: []seataccess.Step{}}
	if view.Grant != want.Grant || view.Holds != want.Holds || view.AppHolds != want.AppHolds ||
		view.Unused != want.Unused || view.Scope != want.Scope || view.StatusLabel != want.StatusLabel ||
		view.Status != want.Status || !view.CheckedAt.Equal(checked) || !view.AppliedAt.Equal(applied) ||
		view.Outcome != want.Outcome || view.Name != want.Name || view.Account != want.Account ||
		view.Integration != want.Integration {
		t.Fatalf("view = %+v\nwant %+v", view, want)
	}
}

// A REFUSED SYNC IS SAID IN THE VENDOR'S WORDS WITH ITS REMEDY, a lane granted
// by level is named by the vendor's level, and an account not made yet holds
// nothing. Mutation: drop the remedy or name the level by its id and this fails.
func TestALaneViewWordsALevelAndARefusal(t *testing.T) {
	t.Parallel()
	offer := seataccess.Options{Lane: seataccess.LaneDatadog,
		Levels: []seataccess.Level{{ID: "r1", Name: "Datadog Standard Role"}}}
	answer := seataccess.Answer{Kind: integration.KindDatadog,
		Options: []seataccess.Options{offer},
		Seats: []seataccess.SeatAnswer{{Handle: "qa", Name: "QA Engineer",
			Lanes: map[seataccess.Lane]seataccess.LaneAnswer{seataccess.LaneDatadog: {
				Desired: seataccess.Desired{Enabled: true, Level: "r1"},
				Sync: &seataccess.SyncResult{Outcome: seataccess.OutcomeFailed,
					Error: &seataccess.Failure{Said: "the key lacks user_access_manage",
						Remedy: "Grant it to the integration's key"}},
				Plan: []seataccess.Step{{Op: seataccess.OpCreate, Text: "Create the account"}}}}}}}

	view, ok := seataccess.ViewOf(answer, "qa", seataccess.LaneDatadog)
	if !ok {
		t.Fatal("the agent's lane was not found")
	}
	if view.Grant != "Datadog Standard Role" || view.Holds != "No account yet" ||
		view.Outcome != "Refused" ||
		view.Refusal != "the key lacks user_access_manage (Grant it to the integration's key)" ||
		view.Status != seataccess.StatusRefused || len(view.Plan) != 1 || view.AppHolds != "" {
		t.Fatalf("view = %+v", view)
	}
}

// AN AGENT THE ANSWER DOES NOT LIST, OR ONE THAT HAS LEFT, HAS NO LANE TO VIEW.
func TestALaneViewNamesOnlyAnAgentTheAnswerLists(t *testing.T) {
	t.Parallel()
	answer := seataccess.Answer{Kind: integration.KindGitHub,
		Seats: []seataccess.SeatAnswer{{Handle: "gone", Departed: true,
			Lanes: map[seataccess.Lane]seataccess.LaneAnswer{}}}}
	for _, handle := range []string{"gone", "nobody"} {
		if _, ok := seataccess.ViewOf(answer, handle, seataccess.LaneGitHub); ok {
			t.Errorf("%s has a lane view", handle)
		}
	}
}
