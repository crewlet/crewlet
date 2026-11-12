package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/github"
)

// freePlanHead is the head of acme/web#42 and the commit a deploy names.
var freePlanHead = strings.Repeat("ab", 20)

// freePlanGitHub is acme/web as GitHub answers it for a private repository on
// a free plan: main is unprotected and the rules are refused for the plan; the
// head's check runs are test by the repository's CI (app 101) and lint by the
// dev seat's own app (900); and somebody other than its author approved the
// head of #42.
func freePlanGitHub(t *testing.T) *github.Client {
	t.Helper()
	run := func(id int, name string, app int) string {
		return fmt.Sprintf(`{"id":%d,"name":%q,"head_sha":%q,"status":"completed","conclusion":"success",`+
			`"app":{"id":%d}}`, id, name, freePlanHead, app)
	}
	routes := map[string]string{
		"/repos/acme/web/pulls/42": `{"number":42,"title":"Retry the upload","state":"open","draft":false,` +
			`"user":{"login":"writer"},"head":{"sha":"` + freePlanHead + `","ref":"retry"},` +
			`"base":{"ref":"main"},"mergeable":true,"mergeable_state":"clean","labels":[]}`,
		"/repos/acme/web/pulls/42/reviews": `[{"user":{"login":"reviewer"},"state":"APPROVED",` +
			`"commit_id":"` + freePlanHead + `"}]`,
		"/repos/acme/web/pulls/42/files": `[{"filename":"web/src/upload.ts"}]`,
		"/repos/acme/web/branches/main": `{"name":"main","protected":false,"protection":{"enabled":false,` +
			`"required_status_checks":{"enforcement_level":"off","contexts":[],"checks":[]}}}`,
		"/repos/acme/web/commits/" + freePlanHead + "/check-runs": `{"total_count":2,"check_runs":[` +
			run(1, "test", 101) + "," + run(2, "lint", 900) + `]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/acme/web/rules/branches/main" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Upgrade to GitHub Pro or make this repository public to ` +
				`enable this feature.","status":"403"}`))
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	c, err := github.NewClient(github.ClientOptions{APIBase: server.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A free-plan repository's merge and deploy are judged on the checks the rule
// declares for its branch, the repository matched whatever its case: test,
// which the repository's CI reported, passes; lint, which only an agent's own
// app reported, is missing; and the head's approval still reads. A rule
// declaring nothing for the branch leaves nothing required.
//
// Mutation: hand GitHub none of the rule's checks, or count the agent's own
// run, and a merge CI never passed reads as allowed.
func TestAFreePlanReleaseIsJudgedOnItsDeclaredChecks(t *testing.T) {
	t.Parallel()
	c := freePlanGitHub(t)
	roster := github.SeatAppOptions{Seats: []github.SeatApp{{Handle: "dev", AppID: 900, Slug: "dev-agent"}}}
	ref := builtin.PullRef{Owner: "acme", Repo: "web", Number: 42}
	declared := []delegated.RequiredChecks{{Repo: "Acme/Web", Branch: "main", Checks: []string{"test", "lint"}}}
	want := []builtin.CheckFact{{Name: "lint", Result: builtin.CheckMissing, Declared: true},
		{Name: "test", Result: builtin.CheckPassed, Declared: true}}

	f, err := pullFacts(t.Context(), c, roster, ref, declared)
	if err != nil {
		t.Fatalf("a free-plan pull request could not be read: %v", err)
	}
	if !slices.Equal(f.Required, want) || !f.RequiredRead {
		t.Errorf("the merge's checks read %+v, want %+v", f.Required, want)
	}
	if !slices.Equal(f.Approvers, []string{"reviewer"}) {
		t.Errorf("approvers %v, want the reviewer of the head", f.Approvers)
	}

	got, read, err := commitChecks(t.Context(), c, roster, builtin.PullRef{Owner: "acme", Repo: "web"}, "main",
		freePlanHead, declared)
	if err != nil || !read || !slices.Equal(got, want) {
		t.Errorf("the deploy's checks read %+v (%v, %v), want %+v", got, read, err, want)
	}

	none, err := pullFacts(t.Context(), c, roster, ref, []delegated.RequiredChecks{{Repo: "acme/web",
		Branch: "release", Checks: []string{"test"}}})
	if err != nil || len(none.Required) != 0 {
		t.Errorf("checks declared for another branch read %+v, %v; want none required", none.Required, err)
	}
}
