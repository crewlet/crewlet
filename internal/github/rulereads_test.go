package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// ruleGitHub is two installations over overlapping repositories, one past the
// branch bound, and check runs from CI, an agent's app and no app.
func ruleGitHub(t *testing.T) string {
	t.Helper()
	repos := map[string]string{
		"Bearer tok-41": `[{"full_name":"acme/api","default_branch":"main"},{"full_name":"acme/web","default_branch":"trunk"}]`,
		"Bearer tok-42": `[{"full_name":"Acme/API","default_branch":"main"},{"full_name":"acme/billing","default_branch":"main","archived":true}]`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token := r.Header.Get("Authorization")
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		switch path := r.URL.Path; {
		case strings.HasSuffix(path, "/access_tokens"):
			id := strings.Split(path, "/")[3]
			if id == "43" {
				http.Error(w, `{"message":"suspended"}`, http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprintf(w, `{"token":"tok-%s","expires_at":"2099-01-01T00:00:00Z"}`, id)
		case path == "/installation/repositories":
			list := repos[token]
			_, _ = fmt.Fprintf(w, `{"total_count":%d,"repositories":%s}`, strings.Count(list, "full_name"), list)
		case path == "/repos/acme/billing/branches" && token == "Bearer tok-41":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case path == "/repos/acme/billing/branches":
			_, _ = w.Write([]byte(`[{"name":"main"},{"name":"release"}]`))
		case path == "/repos/acme/big/branches":
			// 301 BRANCHES, one past what a listing reads.
			var rows []map[string]string
			for i := (page-1)*perPage + 1; i <= min(page*perPage, 301); i++ {
				rows = append(rows, map[string]string{"name": fmt.Sprintf("b%03d", i)})
			}
			if rows == nil {
				rows = []map[string]string{}
			}
			_ = json.NewEncoder(w).Encode(rows)
		case path == "/repos/acme/api/commits/main/check-runs":
			_, _ = w.Write([]byte(`{"check_runs":[` +
				`{"id":1,"name":"test","app":{"id":9}},{"id":2,"name":"lint","app":{"id":9}},` +
				`{"id":3,"name":"test","app":{"id":10}},{"id":4,"name":"looks fine","app":{"id":41}},` +
				`{"id":5,"name":"no app","app":null}]}`))
		case path == "/repos/acme/web/commits/trunk/check-runs":
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func ruleLookup(t *testing.T, base string, seats ...github.SeatApp) *github.SeatLookup {
	t.Helper()
	return &github.SeatLookup{Opts: github.SeatAppOptions{APIBase: base, WebBase: base, Org: "acme", Seats: seats}}
}

// A rule offers every repository an agent's app covers, once whatever its case,
// sorted, skipping a seat that cannot mint.
func TestARulesRepositoriesAreEveryAgentsInstallationsOnce(t *testing.T) {
	t.Parallel()
	base := ruleGitHub(t)
	lookup := ruleLookup(t, base, installedSeat(t, "suspended", 43), installedSeat(t, "sre", 41),
		installedSeat(t, "release", 42))
	repos, err := lookup.Repositories(t.Context())
	if err != nil {
		t.Fatalf("the agents' apps listed nothing: %v", err)
	}
	var names []string
	for _, r := range repos {
		names = append(names, fmt.Sprintf("%s@%s/%v", r.FullName, r.DefaultBranch, r.Archived))
	}
	want := []string{"acme/api@main/false", "acme/billing@main/true", "acme/web@trunk/false"}
	if !slices.Equal(names, want) {
		t.Errorf("the repositories read %v, want %v", names, want)
	}
}

// NO INSTALLED APP IS NOT AN EMPTY LIST: the form says why it lists nothing.
func TestARulesRepositoriesWithNoInstalledAppSaySo(t *testing.T) {
	t.Parallel()
	lookup := ruleLookup(t, ruleGitHub(t), github.SeatApp{Handle: "fresh"})
	if repos, err := lookup.Repositories(t.Context()); !errors.Is(err, github.ErrNoSeatCanRead) {
		t.Fatalf("a company with no installed app answered %v, %v", repos, err)
	}
}

// A repository's branches are read through the first app that covers it, and a
// listing past the bound says there are more.
func TestARulesBranchesAreReadThroughAnAppThatCoversTheRepository(t *testing.T) {
	t.Parallel()
	lookup := ruleLookup(t, ruleGitHub(t), installedSeat(t, "sre", 41), installedSeat(t, "release", 42))
	names, more, err := lookup.Branches(t.Context(), "acme", "billing")
	if err != nil || more || !slices.Equal(names, []string{"main", "release"}) {
		t.Fatalf("acme/billing's branches read %v (more %v, %v), want main and release", names, more, err)
	}
	names, more, err = lookup.Branches(t.Context(), "acme", "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 300 || !more {
		t.Errorf("a repository of 301 branches read %d, more %v; want 300 and more", len(names), more)
	}
}

// The check names on a branch's newest commit leave out runs an agent's own app
// or no app reported: neither passes a required check.
func TestARulesCheckNamesLeaveOutWhatNeverPassesOne(t *testing.T) {
	t.Parallel()
	lookup := ruleLookup(t, ruleGitHub(t), installedSeat(t, "sre", 41))
	names, err := lookup.CheckNames(t.Context(), "acme", "api", "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lint", "test"}; !slices.Equal(names, want) {
		t.Errorf("the check names read %v, want %v", names, want)
	}
}

// AN APP THAT MAY NOT READ CHECK RUNS is said as GitHub refused it, so the
// form can name the permission rather than show an empty list.
func TestARulesCheckNamesRefusedSayWhy(t *testing.T) {
	t.Parallel()
	lookup := ruleLookup(t, ruleGitHub(t), installedSeat(t, "sre", 41))
	names, err := lookup.CheckNames(t.Context(), "acme", "web", "trunk")
	var apiErr *github.APIError
	if !errors.Is(err, github.ErrNoSeatCanRead) || !errors.As(err, &apiErr) || !apiErr.Forbidden() {
		t.Fatalf("a refused read answered %v, %v; want no seat can read, forbidden", names, err)
	}
}
