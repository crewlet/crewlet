package github_test

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// The head and an earlier commit of the pull request every case reads.
const (
	factsHead  = "1111111111111111111111111111111111111111"
	factsOlder = "2222222222222222222222222222222222222222"
)

// The paths a merge decision on acme/web#41 reads.
const (
	factsPull     = "/repos/acme/web/pulls/41"
	factsReviews  = factsPull + "/reviews"
	factsBranch   = "/repos/acme/web/branches/main"
	factsRules    = "/repos/acme/web/rules/branches/main"
	factsRuns     = "/repos/acme/web/commits/" + factsHead + "/check-runs"
	factsStatuses = "/repos/acme/web/commits/" + factsHead + "/statuses"
)

// factsAPI stands in for the GitHub API a merge decision reads: each route
// answers what the case gave it, any other path answers 404, and every path
// asked is counted.
type factsAPI struct {
	mu    sync.Mutex
	asked map[string]int
}

func newFactsAPI(t *testing.T, routes map[string]http.HandlerFunc) (*github.Client, *factsAPI) {
	t.Helper()
	api := &factsAPI{asked: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.asked[r.URL.Path]++
		api.mu.Unlock()
		handler, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := github.NewClient(github.ClientOptions{APIBase: server.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return client, api
}

func (a *factsAPI) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked[path]
}

// total is every request asked, on any path.
func (a *factsAPI) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, count := range a.asked {
		n += count
	}
	return n
}

// answer serves body as JSON on every page.
func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// refuse answers status with GitHub's error envelope, and any headers given
// as name, value pairs.
func refuse(status int, headers ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"message":"refused with %d"}`, status)
	}
}

func jsonList(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

// pullJSON is acme/web#41 at factsHead, opened by author, with mergeable as
// GitHub spells it (true, false or null).
func pullJSON(author, mergeable string) string {
	return `{"number":41,"title":"Add a ceiling","state":"open","merged":false,` +
		`"draft":false,"user":{"login":"` + author + `"},` +
		`"head":{"sha":"` + factsHead + `","ref":"ceiling"},"base":{"ref":"main"},` +
		`"mergeable":` + mergeable + `,"mergeable_state":"unknown",` +
		`"labels":[{"name":"security"},{"name":"ui"}],` +
		`"additions":3,"deletions":1,"changed_files":2,"body":"not read"}`
}

func reviewJSON(login, state, commit string) string {
	return fmt.Sprintf(`{"user":{"login":%q},"state":%q,"commit_id":%q}`, login, state, commit)
}

// runJSON is one check run; an empty conclusion is GitHub's null.
func runJSON(id int64, name, status, conclusion string, app int64) string {
	return runOnJSON(id, name, status, conclusion, app, factsHead)
}

// runOnJSON is runJSON on the commit head.
func runOnJSON(id int64, name, status, conclusion string, app int64, head string) string {
	concl := "null"
	if conclusion != "" {
		concl = strconv.Quote(conclusion)
	}
	return fmt.Sprintf(`{"id":%d,"name":%q,"head_sha":%q,"status":%q,"conclusion":%s,"app":{"id":%d}}`,
		id, name, head, status, concl, app)
}

func runsJSON(runs ...string) string {
	return fmt.Sprintf(`{"total_count":%d,"check_runs":%s}`, len(runs), jsonList(runs...))
}

// statusesJSON is a commit's statuses, newest first, from context, state
// pairs, each posted by CI's own account.
func statusesJSON(pairs ...string) string {
	var rows []string
	for i := 0; i+1 < len(pairs); i += 2 {
		rows = append(rows, fmt.Sprintf(`{"context":%q,"state":%q,"creator":{"login":"ci-bot"}}`, pairs[i],
			pairs[i+1]))
	}
	return jsonList(rows...)
}

// The base branch's classic protection: one required check from any source,
// and a branch that requires none.
const (
	protectedMain = `{"name":"main","protected":true,"protection":{"enabled":true,` +
		`"required_status_checks":{"enforcement_level":"non_admins","contexts":["ci"],` +
		`"checks":[{"context":"ci","app_id":null}]}}}`
	unprotectedMain = `{"name":"main","protected":false,"protection":{"enabled":false,` +
		`"required_status_checks":{"enforcement_level":"off","contexts":[],"checks":[]}}}`
)

// pullRoutes is a pull request whose base requires nothing and that nobody
// has reviewed, with over laid on top.
func pullRoutes(over map[string]http.HandlerFunc) map[string]http.HandlerFunc {
	routes := map[string]http.HandlerFunc{
		factsPull:     answer(pullJSON("Writer", "true")),
		factsReviews:  answer(`[]`),
		factsBranch:   answer(unprotectedMain),
		factsRules:    answer(`[]`),
		factsRuns:     answer(runsJSON()),
		factsStatuses: answer(statusesJSON()),
	}
	maps.Copy(routes, over)
	return routes
}

// approvalsFor reads the facts over reviews and returns the approvals.
func approvalsFor(t *testing.T, reviews ...string) []string {
	t.Helper()
	client, _ := newFactsAPI(t, pullRoutes(map[string]http.HandlerFunc{
		factsReviews: answer(jsonList(reviews...)),
	}))
	facts, err := client.PullFacts(t.Context(), "acme", "web", 41, nil, github.Reporters{})
	if err != nil {
		t.Fatalf("PullFacts: %v", err)
	}
	return facts.Approvals
}

// A PULL REQUEST IS READ AS ONE COMMIT: its head, the reviews of that head,
// its base branch's required checks and how each stands on that head, with
// everything a decision words read off the same answer.
func TestPullFactsAreReadAgainstTheHead(t *testing.T) {
	t.Parallel()
	client, api := newFactsAPI(t, pullRoutes(map[string]http.HandlerFunc{
		factsPull:    answer(pullJSON("Writer", "null")),
		factsReviews: answer(jsonList(reviewJSON("Reviewer", "APPROVED", factsHead))),
		factsBranch:  answer(protectedMain),
		factsRuns:    answer(runsJSON(runJSON(7, "ci", "completed", "success", 101))),
	}))
	facts, err := client.PullFacts(t.Context(), "acme", "web", 41, nil, github.Reporters{})
	if err != nil {
		t.Fatalf("PullFacts: %v", err)
	}
	if facts.Ref != (github.PullRef{Owner: "acme", Repo: "web", Number: 41}) ||
		facts.Title != "Add a ceiling" || facts.State != "open" || facts.Merged || facts.Draft {
		t.Errorf("the pull request read as %+v", facts)
	}
	if facts.Author != "writer" {
		t.Errorf("the author is %q, want the folded login", facts.Author)
	}
	if facts.HeadSHA != factsHead || facts.HeadRef != "ceiling" || facts.BaseRef != "main" {
		t.Errorf("head %s (%s) into %s", facts.HeadSHA, facts.HeadRef, facts.BaseRef)
	}
	// NIL WHILE GITHUB COMPUTES, never false: false says it conflicts.
	if facts.Mergeable != nil || facts.MergeableState != "unknown" {
		t.Errorf("mergeable %v (%q), want nil while GitHub computes it", facts.Mergeable,
			facts.MergeableState)
	}
	if !slices.Equal(facts.Labels, []string{"security", "ui"}) {
		t.Errorf("labels %v", facts.Labels)
	}
	if facts.Additions != 3 || facts.Deletions != 1 || facts.ChangedFiles != 2 {
		t.Errorf("size +%d -%d in %d files", facts.Additions, facts.Deletions, facts.ChangedFiles)
	}
	if !slices.Equal(facts.Approvals, []string{"reviewer"}) {
		t.Errorf("approvals %v, want the reviewer's", facts.Approvals)
	}
	wantChecks := []github.CheckResult{{
		RequiredCheck: github.RequiredCheck{Context: "ci"}, State: github.CheckPassed,
	}}
	if !slices.Equal(facts.Required, []github.RequiredCheck{{Context: "ci"}}) ||
		len(facts.Checks) != 1 || facts.Checks[0].RequiredCheck != wantChecks[0].RequiredCheck ||
		facts.Checks[0].State != wantChecks[0].State {
		t.Errorf("required %v, checks %+v, want %+v", facts.Required, facts.Checks, wantChecks)
	}
	// THE HEAD'S checks, read from the head's own commit route.
	if api.count(factsRuns) != 1 {
		t.Errorf("the head's check runs were read %d times", api.count(factsRuns))
	}
}

// AN APPROVAL OF AN EARLIER COMMIT DOES NOT COUNT. It approved code that is
// no longer the head, and a push after it is exactly what nobody reviewed.
func TestAnApprovalOfAnEarlierCommitDoesNotCount(t *testing.T) {
	t.Parallel()
	got := approvalsFor(t,
		reviewJSON("early", "APPROVED", factsOlder),
		reviewJSON("current", "APPROVED", factsHead))
	if !slices.Equal(got, []string{"current"}) {
		t.Errorf("approvals %v, want only the head's", got)
	}
}

// A REVIEWER'S LATEST DECIDING REVIEW IS WHERE THEY STAND: a change request
// or a dismissal after an approval withdraws it, a comment does not, and an
// approval after a change request stands.
func TestALaterChangeRequestOrDismissalWithdrawsAnApproval(t *testing.T) {
	t.Parallel()
	got := approvalsFor(t,
		reviewJSON("withdrew", "APPROVED", factsHead),
		reviewJSON("commented", "APPROVED", factsHead),
		reviewJSON("dismissed", "APPROVED", factsHead),
		reviewJSON("relented", "CHANGES_REQUESTED", factsOlder),
		reviewJSON("withdrew", "CHANGES_REQUESTED", factsHead),
		reviewJSON("commented", "COMMENTED", factsHead),
		reviewJSON("dismissed", "DISMISSED", factsHead),
		reviewJSON("relented", "APPROVED", factsHead),
		reviewJSON("pending", "PENDING", factsHead))
	if !slices.Equal(got, []string{"commented", "relented"}) {
		t.Errorf("approvals %v, want commented and relented", got)
	}
}

// THE AUTHOR'S OWN APPROVAL IS NOBODY ELSE'S JUDGMENT, in whatever case the
// review spells the login.
func TestTheAuthorsOwnApprovalDoesNotCount(t *testing.T) {
	t.Parallel()
	got := approvalsFor(t, reviewJSON("writer", "APPROVED", factsHead))
	if len(got) != 0 {
		t.Errorf("approvals %v, want none: the author opened it as Writer", got)
	}
}

// REVIEWS ARE READ WHOLE OR NOT AT ALL. A list cut at the bound could miss a
// later change request, so one past it is refused; one that ends exactly at
// it is read whole, and its last page counts.
func TestReviewsPastTheBoundAreRefusedNotCut(t *testing.T) {
	t.Parallel()
	pages := func(full int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if r.URL.Query().Get("per_page") != "100" || page > full {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			rows := make([]string, 100)
			for i := range rows {
				rows[i] = reviewJSON(fmt.Sprintf("r%d-%d", page, i), "COMMENTED", factsHead)
			}
			if page == full {
				rows[99] = reviewJSON("last", "APPROVED", factsHead)
			}
			_, _ = w.Write([]byte(jsonList(rows...)))
		}
	}

	client, _ := newFactsAPI(t, pullRoutes(map[string]http.HandlerFunc{factsReviews: pages(11)}))
	if _, err := client.PullFacts(t.Context(), "acme", "web", 41, nil, github.Reporters{}); !errors.Is(err, github.ErrListTooLong) {
		t.Errorf("1100 reviews read as %v, want ErrListTooLong", err)
	}

	client, _ = newFactsAPI(t, pullRoutes(map[string]http.HandlerFunc{factsReviews: pages(10)}))
	facts, err := client.PullFacts(t.Context(), "acme", "web", 41, nil, github.Reporters{})
	if err != nil {
		t.Fatalf("exactly 1000 reviews were refused: %v", err)
	}
	if !slices.Equal(facts.Approvals, []string{"last"}) {
		t.Errorf("approvals %v, want the one on the tenth page", facts.Approvals)
	}
}

// A RENAME TOUCHES TWO PATHS: the change removes the old one as surely as it
// adds the new one.
func TestPullFilesCountBothPathsOfARename(t *testing.T) {
	t.Parallel()
	client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		factsPull + "/files": answer(`[{"filename":"docs/new.md","previous_filename":"docs/old.md",` +
			`"status":"renamed"},{"filename":"main.go","status":"modified"}]`),
	})
	paths, complete, err := client.PullFiles(t.Context(), "acme", "web", 41)
	if err != nil {
		t.Fatalf("PullFiles: %v", err)
	}
	if !complete || !slices.Equal(paths, []string{"docs/old.md", "docs/new.md", "main.go"}) {
		t.Errorf("paths %v (complete %v), want both of the rename's and main.go", paths, complete)
	}
}

// GITHUB LISTS AT MOST 3000 FILES, and a pull request that fills the list is
// said to be incomplete rather than read as touching only those.
func TestPullFilesSaysWhenGitHubsCapIsReached(t *testing.T) {
	t.Parallel()
	client, api := newFactsAPI(t, map[string]http.HandlerFunc{
		factsPull + "/files": func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			rows := make([]string, 100)
			for i := range rows {
				rows[i] = fmt.Sprintf(`{"filename":"f/%s/%d.go"}`, page, i)
			}
			_, _ = w.Write([]byte(jsonList(rows...)))
		},
	})
	paths, complete, err := client.PullFiles(t.Context(), "acme", "web", 41)
	if err != nil {
		t.Fatalf("PullFiles: %v", err)
	}
	if complete || len(paths) != 3000 {
		t.Errorf("%d paths, complete %v; want GitHub's 3000 and incomplete", len(paths), complete)
	}
	if n := api.count(factsPull + "/files"); n != 30 {
		t.Errorf("read %d pages, want GitHub's 30", n)
	}
}
