package github_test

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// commitChecks reads required on factsHead through routes.
func commitChecks(
	t *testing.T, routes map[string]http.HandlerFunc, required ...github.RequiredCheck,
) ([]github.CheckResult, *factsAPI) {
	t.Helper()
	client, api := newFactsAPI(t, routes)
	got, err := client.CommitChecks(t.Context(), "acme", "web", factsHead, required, github.Reporters{})
	if err != nil {
		t.Fatalf("CommitChecks: %v", err)
	}
	return got, api
}

// statesOf is each result's context and state, for a comparison.
func statesOf(results []github.CheckResult) map[string]github.CheckState {
	out := map[string]github.CheckState{}
	for _, r := range results {
		out[r.Context] = r.State
	}
	return out
}

func anySource(contexts ...string) []github.RequiredCheck {
	out := make([]github.RequiredCheck, 0, len(contexts))
	for _, c := range contexts {
		out = append(out, github.RequiredCheck{Context: c})
	}
	return out
}

// A CONTEXT REPORTED ONLY AS A COMMIT STATUS PASSES. Plenty of CI posts a
// status rather than a check run, and GitHub counts either.
func TestAContextReportedOnlyByACommitStatusPasses(t *testing.T) {
	t.Parallel()
	got, _ := commitChecks(t, map[string]http.HandlerFunc{
		factsRuns:     answer(runsJSON()),
		factsStatuses: answer(statusesJSON("deploy/preview", "success")),
	}, anySource("deploy/preview")...)
	if s := statesOf(got)["deploy/preview"]; s != github.CheckPassed {
		t.Errorf("a successful status read as %q", s)
	}
}

// EACH WAY A REQUIRED CHECK CAN FALL SHORT IS SAID AS ITSELF: a run that
// finished badly or a status that errored is failed, one still running is
// pending, and a name nothing reported is missing. Success, neutral and
// skipped are GitHub's three passes.
func TestAFailedPendingOrMissingRequiredContextIsSaid(t *testing.T) {
	t.Parallel()
	required := anySource("unit", "lint", "docs", "fmt", "e2e", "sign", "scan", "license")
	got, _ := commitChecks(t, map[string]http.HandlerFunc{
		factsRuns: answer(runsJSON(
			runJSON(1, "unit", "completed", "failure", 101),
			runJSON(2, "lint", "in_progress", "", 101),
			runJSON(3, "docs", "completed", "skipped", 101),
			runJSON(4, "fmt", "completed", "neutral", 101),
			runJSON(5, "e2e", "completed", "timed_out", 101))),
		factsStatuses: answer(statusesJSON("sign", "error", "scan", "pending")),
	}, required...)
	want := map[string]github.CheckState{
		"unit": github.CheckFailed, "lint": github.CheckPending, "docs": github.CheckPassed,
		"fmt": github.CheckPassed, "e2e": github.CheckFailed, "sign": github.CheckFailed,
		"scan": github.CheckPending, "license": github.CheckMissing,
	}
	for context, state := range want {
		if g := statesOf(got)[context]; g != state {
			t.Errorf("%s read as %q, want %q", context, g, state)
		}
	}
	for i, r := range got {
		if r.RequiredCheck != required[i] {
			t.Errorf("result %d is %s: results are not in the order required", i, r.Context)
		}
	}
}

// EVERY SOURCE REPORTING A CONTEXT MUST PASS, as GitHub requires of a check
// and a status of one name: a newer success from another app does not hide a
// failure, and a status's failure is not hidden by a check run's pass. A
// re-run from the SAME app does supersede the run it repeats.
func TestEverySourceReportingAContextMustPass(t *testing.T) {
	t.Parallel()
	got, _ := commitChecks(t, map[string]http.HandlerFunc{
		factsRuns: answer(runsJSON(
			runJSON(10, "ci", "completed", "failure", 101),
			runJSON(11, "ci", "completed", "success", 202),
			runJSON(20, "build", "completed", "success", 101),
			runJSON(30, "test", "completed", "failure", 101),
			runJSON(31, "test", "completed", "success", 101))),
		factsStatuses: answer(statusesJSON("build", "failure")),
	}, anySource("ci", "build", "test")...)
	want := map[string]github.CheckState{
		"ci": github.CheckFailed, "build": github.CheckFailed, "test": github.CheckPassed,
	}
	if g := statesOf(got); !maps.Equal(g, want) {
		t.Errorf("read %v, want %v", g, want)
	}
}

// A CHECK BOUND TO AN APP COUNTS ONLY THAT APP'S RUNS. An agent's own app can
// hold Checks: Read and write and post a successful run of any name; matched
// by name alone it would pass a check the repository's CI never ran. A status
// names no app the API reports, so none is read for a bound check.
func TestAnAppBoundCheckCountsOnlyThatAppsRuns(t *testing.T) {
	t.Parallel()
	got, api := commitChecks(t, map[string]http.HandlerFunc{
		factsRuns: answer(runsJSON(
			runJSON(40, "ci", "completed", "success", 666),
			runJSON(41, "lint", "completed", "success", 101))),
		factsStatuses: answer(statusesJSON("ci", "success")),
	}, github.RequiredCheck{Context: "ci", AppID: 101}, github.RequiredCheck{Context: "lint", AppID: 101})
	want := map[string]github.CheckState{"ci": github.CheckMissing, "lint": github.CheckPassed}
	if g := statesOf(got); !maps.Equal(g, want) {
		t.Errorf("read %v, want %v", g, want)
	}
	if n := api.count(factsStatuses); n != 0 {
		t.Errorf("the statuses were read %d times for checks no status can meet", n)
	}
}

// A SOURCE GITHUB REFUSES THE CREDENTIAL IS UNREAD, NOT MISSING. Missing is a
// fact about the commit; this is a fact about the credential, and the result
// names the permission. What could be read still fails a check, and a check
// the refused source cannot report is unaffected. A rate limit is no such
// fact: it fails the read, for a retry to answer.
func TestASourceGitHubRefusesIsUnreadNotMissing(t *testing.T) {
	t.Parallel()
	got, _ := commitChecks(t, map[string]http.HandlerFunc{
		factsRuns: answer(runsJSON(
			runJSON(1, "ci", "completed", "success", 101),
			runJSON(2, "lint", "completed", "failure", 101),
			runJSON(3, "build", "completed", "success", 101))),
		factsStatuses: refuse(http.StatusForbidden),
	}, github.RequiredCheck{Context: "ci"}, github.RequiredCheck{Context: "lint"},
		github.RequiredCheck{Context: "build", AppID: 101})
	want := map[string]github.CheckState{
		"ci": github.CheckUnread, "lint": github.CheckFailed, "build": github.CheckPassed,
	}
	if g := statesOf(got); !maps.Equal(g, want) {
		t.Errorf("read %v, want %v", g, want)
	}
	if !slices.Equal(got[0].Unreadable, []string{"Commit statuses"}) || len(got[2].Unreadable) != 0 {
		t.Errorf("unreadable %v and %v, want Commit statuses for the any-source check alone",
			got[0].Unreadable, got[2].Unreadable)
	}

	runsRefused, _ := commitChecks(t, map[string]http.HandlerFunc{
		factsRuns:     refuse(http.StatusForbidden),
		factsStatuses: answer(statusesJSON("ci", "success")),
	}, anySource("ci")...)
	if runsRefused[0].State != github.CheckUnread ||
		!slices.Equal(runsRefused[0].Unreadable, []string{"Checks"}) {
		t.Errorf("refused check runs read as %+v", runsRefused[0])
	}

	client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		factsRuns:     answer(runsJSON()),
		factsStatuses: refuse(http.StatusForbidden, "X-RateLimit-Remaining", "0"),
	})
	if _, err := client.CommitChecks(t.Context(), "acme", "web", factsHead, anySource("ci"), github.Reporters{}); err == nil {
		t.Error("a rate-limited read of the statuses answered as if read")
	}
}

// A COMMIT IS NAMED BY ITS FULL ID, never a branch whose tip moves between
// the read and the act, and a commit nothing is required of costs no read.
func TestCommitChecksNamesOneCommit(t *testing.T) {
	t.Parallel()
	client, api := newFactsAPI(t, map[string]http.HandlerFunc{})
	for _, sha := range []string{"main", factsHead[:12], factsHead + "0", "g" + factsHead[1:], ""} {
		if _, err := client.CommitChecks(t.Context(), "acme", "web", sha, anySource("ci"), github.Reporters{}); err == nil {
			t.Errorf("%q was read as a commit", sha)
		}
	}
	if n := api.total(); n != 0 {
		t.Errorf("%d requests for names that are not a commit's full id", n)
	}
	got, err := client.CommitChecks(t.Context(), "acme", "web", factsHead, nil, github.Reporters{})
	if err != nil || len(got) != 0 {
		t.Errorf("nothing required read as %v, %v", got, err)
	}
	if n := api.total(); n != 0 {
		t.Errorf("%d reads for a commit nothing is required of", n)
	}
}

// REQUIRED IS THE UNION OF PROTECTION AND RULESETS, as GitHub enforces it:
// either alone answers "none" for a branch the other protects. A context
// bound to an app in one and to any source in the other stays two
// requirements, and a rule of another type is not a check.
func TestRequiredChecksAreTheUnionOfProtectionAndRulesets(t *testing.T) {
	t.Parallel()
	client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		factsBranch: answer(`{"name":"main","protected":true,"protection":{"enabled":true,` +
			`"required_status_checks":{"enforcement_level":"everyone",` +
			`"contexts":["ci","lint","legacy"],` +
			`"checks":[{"context":"ci","app_id":null},{"context":"lint","app_id":101}]}}}`),
		factsRules: answer(`[{"type":"pull_request","parameters":{"required_approving_review_count":1}},` +
			`{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true,` +
			`"required_status_checks":[{"context":"e2e"},{"context":"ci","integration_id":101},` +
			`{"context":"lint","integration_id":101}]}}]`),
	})
	got, err := client.RequiredChecks(t.Context(), "acme", "web", "main")
	if err != nil {
		t.Fatalf("RequiredChecks: %v", err)
	}
	want := []github.RequiredCheck{
		{Context: "ci"}, {Context: "ci", AppID: 101}, {Context: "e2e"},
		{Context: "legacy"}, {Context: "lint", AppID: 101},
	}
	if !slices.Equal(got, want) {
		t.Errorf("required %v, want %v", got, want)
	}
}

// A SERVER WITHOUT RULESETS REQUIRES WHAT ITS PROTECTION DOES. The rules
// route answers 404 on a GitHub Enterprise Server from before rulesets, and
// the branch read has already shown the repository is visible, so the 404 is
// "none". Any other refusal of the rules is not, and protection that is
// switched off requires nothing.
func TestABranchWithoutRulesRequiresItsProtectionAlone(t *testing.T) {
	t.Parallel()
	client, api := newFactsAPI(t, map[string]http.HandlerFunc{factsBranch: answer(protectedMain)})
	got, err := client.RequiredChecks(t.Context(), "acme", "web", "main")
	if err != nil {
		t.Fatalf("a rules 404 failed the read: %v", err)
	}
	if !slices.Equal(got, []github.RequiredCheck{{Context: "ci"}}) || api.count(factsRules) != 1 {
		t.Errorf("required %v after %d rules reads, want ci alone", got, api.count(factsRules))
	}

	client, _ = newFactsAPI(t, map[string]http.HandlerFunc{
		factsBranch: answer(protectedMain), factsRules: refuse(http.StatusInternalServerError),
	})
	if _, err := client.RequiredChecks(t.Context(), "acme", "web", "main"); err == nil {
		t.Error("a 500 from the rules read as no rules")
	}

	client, _ = newFactsAPI(t, map[string]http.HandlerFunc{
		factsBranch: answer(`{"protected":true,"protection":{"enabled":true,"required_status_checks":` +
			`{"enforcement_level":"off","contexts":["ci"],"checks":[]}}}`),
		factsRules: answer(`[]`),
	})
	if got, err := client.RequiredChecks(t.Context(), "acme", "web", "main"); err != nil || len(got) != 0 {
		t.Errorf("protection with its checks off required %v, %v", got, err)
	}
}

// A PROTECTED BRANCH WHOSE PROTECTION GITHUB DID NOT SHOW IS NOT READ AS
// UNPROTECTED, which would pass every check it requires.
func TestAProtectedBranchWithoutItsProtectionIsNotUnprotected(t *testing.T) {
	t.Parallel()
	client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		factsBranch: answer(`{"name":"main","protected":true}`), factsRules: answer(`[]`),
	})
	if got, err := client.RequiredChecks(t.Context(), "acme", "web", "main"); err == nil {
		t.Errorf("an unshown protection required %v", got)
	}
}

// A STATE OFF THE WIRE IS A VALUE, and only the six are valid.
func TestCheckStatesAreClosed(t *testing.T) {
	t.Parallel()
	for _, s := range []github.CheckState{
		github.CheckPassed, github.CheckFailed, github.CheckPending, github.CheckMissing, github.CheckUnread,
		github.CheckInconclusive,
	} {
		if !s.Valid() {
			t.Errorf("%q is not valid", s)
		}
	}
	for _, s := range []github.CheckState{"", "success", "Passed"} {
		if s.Valid() {
			t.Errorf("%q is valid", s)
		}
	}
}

// A BRANCH'S TIP IS A WHOLE COMMIT ID, lowercased, or an error: a deploy asks
// whether its commit is on the branch by comparing it with this tip, and a
// tip read as empty would compare against nothing.
//
// Mutation: answer the tip without checking it is a whole SHA, and a branch
// GitHub answered with no commit reads as one.
func TestABranchTipIsAWholeCommitOrAnError(t *testing.T) {
	t.Parallel()
	tip := strings.Repeat("AB", 20)
	client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		factsBranch: answer(`{"name":"main","commit":{"sha":"` + tip + `"},"protected":false}`)})
	got, err := client.BranchTip(t.Context(), "acme", "web", "main")
	if err != nil || got != strings.ToLower(tip) {
		t.Fatalf("BranchTip = %q, %v; want the tip lowercased", got, err)
	}
	empty, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		factsBranch: answer(`{"name":"main","protected":false}`)})
	if got, err := empty.BranchTip(t.Context(), "acme", "web", "main"); err == nil {
		t.Fatalf("a branch answered with no commit read as tip %q", got)
	}
}

// A CHECK AN AGENT'S OWN APP REPORTED COUNTS FOR NOTHING: a passing check run
// by one of the company's agents' apps and a passing status its bot posted
// leave their requirements missing, whatever the login's case, while the same
// reports by anybody else pass them.
//
// Mutation: count an untrusted app's run or bot's status, and an agent whose
// app may write checks reports its own change as passing.
func TestACheckAnAgentReportedCountsForNothing(t *testing.T) {
	t.Parallel()
	routes := map[string]http.HandlerFunc{
		factsRuns:     answer(runsJSON(runJSON(1, "ci", "completed", "success", 900))),
		factsStatuses: answer(`[{"context":"lint","state":"success","creator":{"login":"Dev-Agent[bot]"}}]`),
	}
	agents := github.Reporters{AppIDs: []int64{900}, Logins: []string{"dev-agent[bot]"}}
	for _, c := range []struct {
		name      string
		untrusted github.Reporters
		want      github.CheckState
	}{
		{"reported by the company's agent", agents, github.CheckMissing},
		{"reported by anybody else", github.Reporters{}, github.CheckPassed},
	} {
		client, _ := newFactsAPI(t, routes)
		got, err := client.CommitChecks(t.Context(), "acme", "web", factsHead, anySource("ci", "lint"), c.untrusted)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, r := range got {
			if r.State != c.want {
				t.Errorf("%s: %s reads %s, want %s", c.name, r.Context, r.State, c.want)
			}
		}
	}
}
