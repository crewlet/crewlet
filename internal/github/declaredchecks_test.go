package github_test

import (
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// planRefusal is GitHub's answer to branch protection and rulesets on a
// private repository of a free plan.
func planRefusal(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"Upgrade to GitHub Pro or make this repository public to enable this ` +
		`feature.","documentation_url":"https://docs.github.com/rest/repos/rules","status":"403"}`))
}

// A DECLARED CHECK PASSES ONLY AS A CHECK RUN THAT SUCCEEDED ON THE HEAD, by
// an app that is not one of the company's agents: neutral and skipped, which
// GitHub would count, are inconclusive; a run of another commit, a status, an
// agent's run and a run naming no app leave it missing; a re-run supersedes
// the run it repeats; and no status is read for a check only a run can meet.
//
// Mutation: judge a declared check as GitHub judges a required one, or keep a
// run of another commit, and a check CI never passed on the head lets a
// delegate merge.
func TestADeclaredCheckPassesOnlyAsASuccessfulRunOnTheHead(t *testing.T) {
	t.Parallel()
	client, api := newFactsAPI(t, map[string]http.HandlerFunc{
		factsRuns: answer(runsJSON(
			runJSON(1, "test", "completed", "success", 101),
			runJSON(2, "lint", "completed", "neutral", 101),
			runJSON(3, "docs", "completed", "skipped", 101),
			runJSON(4, "e2e", "in_progress", "", 101),
			runJSON(5, "unit", "completed", "failure", 101),
			runOnJSON(6, "stale", "completed", "success", 101, factsOlder),
			runJSON(7, "rerun", "completed", "failure", 101),
			runJSON(8, "rerun", "completed", "success", 101),
			runJSON(9, "agent", "completed", "success", 900),
			runJSON(10, "anonymous", "completed", "success", 0))),
		factsStatuses: answer(statusesJSON("status-only", "success")),
	})
	required := github.WithDeclared(nil, []string{"test", "lint", "docs", "e2e", "unit", "stale", "rerun",
		"agent", "anonymous", "status-only"})
	got, err := client.CommitChecks(t.Context(), "acme", "web", factsHead, required,
		github.Reporters{AppIDs: []int64{900}})
	if err != nil {
		t.Fatalf("CommitChecks: %v", err)
	}
	want := map[string]github.CheckState{
		"test": github.CheckPassed, "lint": github.CheckInconclusive, "docs": github.CheckInconclusive,
		"e2e": github.CheckPending, "unit": github.CheckFailed, "stale": github.CheckMissing,
		"rerun": github.CheckPassed, "agent": github.CheckMissing, "anonymous": github.CheckMissing,
		"status-only": github.CheckMissing,
	}
	if g := statesOf(got); !maps.Equal(g, want) {
		t.Errorf("read %v, want %v", g, want)
	}
	for _, r := range got {
		if !r.Declared {
			t.Errorf("%s lost its declaration", r.Context)
		}
	}
	if n := api.count(factsStatuses); n != 0 {
		t.Errorf("the statuses were read %d times for checks no status can meet", n)
	}
}

// A DECLARED CHECK GITHUB REFUSES THE RUNS OF IS UNREAD, naming Checks: a fact
// about the credential a person changes, never "missing".
func TestADeclaredCheckIsUnreadWhereCheckRunsAreRefused(t *testing.T) {
	t.Parallel()
	got, _ := commitChecks(t, map[string]http.HandlerFunc{factsRuns: refuse(http.StatusForbidden)},
		github.WithDeclared(nil, []string{"test"})...)
	if got[0].State != github.CheckUnread || !slices.Equal(got[0].Unreadable, []string{"Checks"}) {
		t.Errorf("refused check runs read as %+v", got[0])
	}
}

// A NAME BOTH GITHUB AND THE COMPANY REQUIRE IS TWO REQUIREMENTS, each judged
// by its own rule: a neutral run passes GitHub's and not the company's.
func TestADeclaredAndARequiredCheckOfOneNameAreBothJudged(t *testing.T) {
	t.Parallel()
	required := github.WithDeclared([]github.RequiredCheck{{Context: "ci"}}, []string{"ci", "ci"})
	if !slices.Equal(required, []github.RequiredCheck{{Context: "ci"}, {Context: "ci", Declared: true}}) {
		t.Fatalf("required %v, want ci from GitHub and ci declared, once each", required)
	}
	got, _ := commitChecks(t, map[string]http.HandlerFunc{
		factsRuns:     answer(runsJSON(runJSON(1, "ci", "completed", "neutral", 101))),
		factsStatuses: answer(statusesJSON()),
	}, required...)
	if got[0].State != github.CheckPassed || got[1].State != github.CheckInconclusive {
		t.Errorf("GitHub's ci read %q and the declared ci %q, want passed and inconclusive", got[0].State,
			got[1].State)
	}
}

// A PLAN WITHOUT PROTECTION OR RULESETS REQUIRES NOTHING OF GITHUB'S OWN: the
// 403 that names the plan, on the rules or on the branch, reads as none. Any
// other 403 is the credential's, a fact the engine cannot read, and fails it.
//
// Mutation: read the plan's 403 as any other, and a private repository on a
// free plan never reaches its declared checks; read every 403 as the plan's,
// and a credential refused the rules passes a branch they protect.
func TestAPlanWithoutProtectionRequiresNothingOfGitHub(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		routes map[string]http.HandlerFunc
	}{
		{"rules refused for the plan", map[string]http.HandlerFunc{
			factsBranch: answer(unprotectedMain), factsRules: planRefusal}},
		{"branch and rules refused for the plan", map[string]http.HandlerFunc{
			factsBranch: planRefusal, factsRules: planRefusal}},
	} {
		client, _ := newFactsAPI(t, c.routes)
		got, err := client.RequiredChecks(t.Context(), "acme", "web", "main")
		if err != nil || len(got) != 0 {
			t.Errorf("%s: required %v, %v; want none", c.name, got, err)
		}
	}
	for _, c := range []struct {
		name   string
		routes map[string]http.HandlerFunc
	}{
		{"rules refused the credential", map[string]http.HandlerFunc{
			factsBranch: answer(unprotectedMain), factsRules: refuse(http.StatusForbidden)}},
		{"branch refused the credential", map[string]http.HandlerFunc{
			factsBranch: refuse(http.StatusForbidden), factsRules: answer(`[]`)}},
		{"rules rate limited", map[string]http.HandlerFunc{
			factsBranch: answer(unprotectedMain),
			factsRules:  refuse(http.StatusForbidden, "X-RateLimit-Remaining", "0")}},
	} {
		client, _ := newFactsAPI(t, c.routes)
		if got, err := client.RequiredChecks(t.Context(), "acme", "web", "main"); err == nil {
			t.Errorf("%s: read as requiring %v", c.name, got)
		}
	}
}

// A FREE-PLAN PULL REQUEST IS READ WHOLE: GitHub requires nothing, the
// company's declared checks for its base are judged on its head, and its
// reviews, which every plan has, still name who approved that head.
//
// Mutation: drop the declared checks from the facts, and a free-plan pull
// request reads as requiring none.
func TestAFreePlanPullCarriesItsDeclaredChecksAndItsApprovals(t *testing.T) {
	t.Parallel()
	client, _ := newFactsAPI(t, pullRoutes(map[string]http.HandlerFunc{
		factsRules:   planRefusal,
		factsReviews: answer(jsonList(reviewJSON("Reviewer", "APPROVED", factsHead))),
		factsRuns: answer(runsJSON(
			runJSON(1, "test", "completed", "success", 101),
			runJSON(2, "lint", "completed", "failure", 101))),
	}))
	var asked []string
	facts, err := client.PullFacts(t.Context(), "acme", "web", 41, func(base string) []string {
		asked = append(asked, base)
		return []string{"test", "lint"}
	}, github.Reporters{})
	if err != nil {
		t.Fatalf("PullFacts: %v", err)
	}
	if !slices.Equal(asked, []string{"main"}) {
		t.Errorf("the declared checks were asked for %v, want the base branch once", asked)
	}
	want := map[string]github.CheckState{"test": github.CheckPassed, "lint": github.CheckFailed}
	if g := statesOf(facts.Checks); !maps.Equal(g, want) || len(facts.Required) != 2 {
		t.Errorf("required %v read %v, want %v", facts.Required, g, want)
	}
	if !slices.Equal(facts.Approvals, []string{"reviewer"}) {
		t.Errorf("approvals %v, want the reviewer's", facts.Approvals)
	}
}
