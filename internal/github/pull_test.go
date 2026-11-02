package github_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// A PULL REQUEST'S ADDRESS IS READ ONLY ON ITS OWN HOST: its page or any tab
// of it names the owner, the repository and the number, and an issue, a
// repository, another host or a malformed number names nothing.
func TestAPullRequestAddressIsReadOnlyOnItsHost(t *testing.T) {
	t.Parallel()
	want := github.PullRef{Owner: "acme", Repo: "web", Number: 41}
	for _, raw := range []string{
		"https://github.com/acme/web/pull/41",
		"https://github.com/acme/web/pull/41/files",
		"https://GitHub.com/acme/web/pull/41?diff=split#discussion",
	} {
		if got, ok := github.ParsePullURL("https://github.com", raw); !ok || got != want {
			t.Errorf("%s read as %+v (%v), want %+v", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"https://github.com/acme/web/issues/41", "https://github.com/acme/web",
		"https://github.example.com/acme/web/pull/41", "http://github.com/acme/web/pull/41",
		"https://github.com/acme/web/pull/x", "https://github.com/acme/web/pull/0",
	} {
		if got, ok := github.ParsePullURL("https://github.com", raw); ok {
			t.Errorf("%s read as %+v, want no pull request", raw, got)
		}
	}
	if got := want.Canonical("https://github.com"); got != "https://github.com/acme/web/pull/41" {
		t.Errorf("the canonical address is %s", got)
	}
}

// A PULL REQUEST IS READ FROM ITS OWN ROUTE, its preview's fields decoded.
func TestAPullRequestIsReadFromItsRoute(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/web/pulls/41" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"number":41,"title":"Add a ceiling","state":"closed",` +
			`"merged":true,"draft":false,"additions":3,"deletions":1,"body":"secret plan"}`))
	}))
	t.Cleanup(srv.Close)
	client, err := github.NewClient(github.ClientOptions{APIBase: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := client.PullRequest(t.Context(), "acme", "web", 41)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if pr.Number != 41 || pr.Title != "Add a ceiling" || !pr.Merged || pr.Additions != 3 {
		t.Errorf("the pull request read as %+v", pr)
	}
}
