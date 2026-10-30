package github_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// fakeApp is GitHub's app endpoints for one app, answering canned bodies by
// "METHOD /path" and recording every call with the credential it carried.
type fakeApp struct {
	*httptest.Server
	mu      sync.Mutex
	replies map[string]fakeReply
	calls   []string
	auths   []string
	bodies  map[string]map[string]any
}

type fakeReply struct {
	status int
	body   string
}

func newFakeApp(t *testing.T) *fakeApp {
	t.Helper()
	f := &fakeApp{replies: map[string]fakeReply{}, bodies: map[string]map[string]any{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// AN ENTERPRISE BASE IS SERVED UNDER /api/v3, which is what a
		// company block naming this server's URL derives.
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/v3")
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.auths = append(f.auths, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.bodies[key] = body
		reply, ok := f.replies[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if reply.status == 0 {
			reply.status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeApp) on(key string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[key] = fakeReply{status: status, body: body}
}

func (f *fakeApp) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeApp) body(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func (f *fakeApp) client(t *testing.T) *github.AppClient {
	t.Helper()
	_, pem := testKey(t)
	client, err := github.NewAppClient(41, pem, f.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// THE REGISTRATION IS READ AS THE APP, and carries what the app ASKS for,
// which is the half of a raise its owner acts on (D5).
func TestTheRegistrationIsReadAsTheApp(t *testing.T) {
	t.Parallel()
	gh := newFakeApp(t)
	gh.on("GET /app", 200, `{"id":41,"slug":"acme-sre","permissions":{"contents":"read","metadata":"read"},"owner":{"login":"acme","type":"Organization"}}`)
	reg, err := gh.client(t).Registration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reg.Slug != "acme-sre" || reg.Permissions["contents"] != "read" || reg.Owner.Login != "acme" {
		t.Errorf("registration = %+v", reg)
	}

	gone := newFakeApp(t)
	_, err = gone.client(t).Registration(t.Context())
	var apiErr *github.APIError
	if !asAPIError(err, &apiErr) || !apiErr.NotFound() {
		t.Errorf("a deleted app answered %v, want the 404 for the caller to read", err)
	}
}

// OFF AND BACK ON ARE THE INSTALLATION'S SUSPENSION, set and lifted as the
// app, never a person's PAT.
func TestSuspendAndUnsuspendAddressTheInstallation(t *testing.T) {
	t.Parallel()
	gh := newFakeApp(t)
	gh.on("PUT /app/installations/7/suspended", 204, ``)
	gh.on("DELETE /app/installations/7/suspended", 204, ``)
	client := gh.client(t)
	if err := client.Suspend(t.Context(), 7); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := client.Unsuspend(t.Context(), 7); err != nil {
		t.Fatalf("unsuspend: %v", err)
	}
	if got := gh.called(); !slices.Equal(got, []string{
		"PUT /app/installations/7/suspended", "DELETE /app/installations/7/suspended",
	}) {
		t.Errorf("calls = %v", got)
	}

	// A REFUSAL KEEPS GITHUB'S REASON, which is what the step reports.
	refused := newFakeApp(t)
	refused.on("PUT /app/installations/7/suspended", 403, `{"message":"Resource not accessible by integration"}`)
	err := refused.client(t).Suspend(t.Context(), 7)
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Errorf("a refused suspend answered %v", err)
	}
}

// THE REPOSITORY LISTING IS AN INSTALLATION'S READ: a metadata-only token is
// minted for it, used for every page, and revoked straight after.
func TestInstallationRepositoriesWalkWithAMetadataTokenAndRevokeIt(t *testing.T) {
	t.Parallel()
	gh := newFakeApp(t)
	gh.on("POST /app/installations/7/access_tokens", 201,
		`{"token":"ghs_listing","expires_at":"2099-01-01T00:00:00Z"}`)
	page := func(from, n int) string {
		repos := make([]string, 0, n)
		for i := range n {
			repos = append(repos, `{"full_name":"acme/r`+itoa(from+i)+`"}`)
		}
		return `{"total_count":101,"repositories":[` + strings.Join(repos, ",") + `]}`
	}
	gh.on("GET /installation/repositories?per_page=100&page=1", 200, page(0, 100))
	gh.on("GET /installation/repositories?per_page=100&page=2", 200, page(100, 1))
	gh.on("DELETE /installation/token", 204, ``)

	repos, err := gh.client(t).InstallationRepositories(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 101 || repos[0] != "acme/r0" || repos[100] != "acme/r100" {
		t.Fatalf("listed %d repositories: first %q", len(repos), repos[0])
	}
	mint := gh.body("POST /app/installations/7/access_tokens")
	perms, _ := mint["permissions"].(map[string]any)
	if len(perms) != 1 || perms["metadata"] != "read" || mint["repositories"] != nil {
		t.Errorf("the listing token asked for %v", mint)
	}
	gh.mu.Lock()
	auths := slices.Clone(gh.auths)
	gh.mu.Unlock()
	if auths[1] != "ghs_listing" || auths[len(auths)-1] != "ghs_listing" {
		t.Errorf("the listing and the revoke carried %v, want the minted token", auths)
	}
	if last := gh.called(); last[len(last)-1] != "DELETE /installation/token" {
		t.Errorf("the listing token was not revoked: %v", last)
	}
}

// A LISTING THAT FAILS STILL REVOKES ITS TOKEN, and a revoke that fails is
// said rather than swallowed.
func TestAFailedListingStillRevokesAndAFailedRevokeIsSaid(t *testing.T) {
	t.Parallel()
	gh := newFakeApp(t)
	gh.on("POST /app/installations/7/access_tokens", 201, `{"token":"ghs_listing"}`)
	gh.on("GET /installation/repositories?per_page=100&page=1", 502, `{"message":"bad gateway"}`)
	gh.on("DELETE /installation/token", 204, ``)
	if _, err := gh.client(t).InstallationRepositories(t.Context(), 7); err == nil {
		t.Fatal("a failed listing answered no error")
	}
	if last := gh.called(); last[len(last)-1] != "DELETE /installation/token" {
		t.Errorf("a failed listing left its token live: %v", last)
	}

	stuck := newFakeApp(t)
	stuck.on("POST /app/installations/7/access_tokens", 201, `{"token":"ghs_listing"}`)
	stuck.on("GET /installation/repositories?per_page=100&page=1", 200,
		`{"total_count":1,"repositories":[{"full_name":"acme/backend"}]}`)
	repos, err := stuck.client(t).InstallationRepositories(t.Context(), 7)
	if err == nil || !strings.Contains(err.Error(), "could not be revoked") {
		t.Errorf("a failed revoke answered %v", err)
	}
	if !slices.Equal(repos, []string{"acme/backend"}) {
		t.Errorf("the listing was dropped with the revoke: %v", repos)
	}
}

// THE INSTALLATION PAGE is the organization's, where a permission request is
// accepted.
func TestInstallationSettingsURL(t *testing.T) {
	t.Parallel()
	if got := github.InstallationSettingsURL("", "acme", 7); got != "https://github.com/organizations/acme/settings/installations/7" {
		t.Errorf("url = %q", got)
	}
	if got := github.InstallationSettingsURL("https://ghe.example.com/", "", 7); got != "https://ghe.example.com/settings/installations/7" {
		t.Errorf("url = %q", got)
	}
	if got := github.InstallationSettingsURL("", "acme", 0); got != "" {
		t.Errorf("no installation built %q", got)
	}
}

func asAPIError(err error, target **github.APIError) bool { return errors.As(err, target) }

func itoa(n int) string { return strconv.Itoa(n) }
