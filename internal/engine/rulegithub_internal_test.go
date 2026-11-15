package engine

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
)

// A rule is offered only repositories under the company's GitHub owners; a
// branch read elsewhere is refused before any request.
//
// Mutation: drop the owner filter or the refusal, and this is red.
func TestARuleIsOfferedOnlyTheCompanysRepositories(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_, _ = w.Write([]byte(`{"token":"tok","expires_at":"2099-01-01T00:00:00Z"}`))
		case r.URL.Path == "/installation/repositories":
			_, _ = w.Write([]byte(`{"total_count":2,"repositories":[` +
				`{"full_name":"Acme/api","default_branch":"main"},{"full_name":"elsewhere/tool"}]}`))
		default:
			t.Errorf("a read outside the company's owners reached GitHub: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	seat := github.SeatApp{Handle: "sre", AppID: 41, InstallationID: 41, Slug: "sre",
		Key: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
	e := &Engine{}
	e.githubPulls.wire(&github.SeatLookup{Opts: github.SeatAppOptions{APIBase: server.URL, WebBase: server.URL,
		Org: "acme", Seats: []github.SeatApp{seat}}}, []string{"acme"})

	repos, err := e.GitHubRepositories(t.Context())
	if err != nil || len(repos) != 1 || repos[0].FullName != "Acme/api" {
		t.Fatalf("the rule is offered %v (%v), want Acme/api alone", repos, err)
	}
	if _, _, err := e.GitHubBranches(t.Context(), "elsewhere", "tool"); !errors.Is(err, github.ErrOutsideCompany) {
		t.Errorf("a branch read outside the company's owners answered %v", err)
	}
	if _, err := e.GitHubCheckNames(t.Context(), "elsewhere", "tool", "main"); !errors.Is(err, github.ErrOutsideCompany) {
		t.Errorf("a check read outside the company's owners answered %v", err)
	}
}

// A COMPANY THAT RUNS NO GITHUB says so, which the form reads as nothing to
// connect rather than as a GitHub that failed.
func TestARuleReadOnACompanyWithoutGitHubSaysSo(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	if _, err := e.GitHubRepositories(t.Context()); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("the repositories without GitHub answered %v", err)
	}
	if _, _, err := e.GitHubBranches(t.Context(), "acme", "api"); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("the branches without GitHub answered %v", err)
	}
	if _, err := e.GitHubCheckNames(t.Context(), "acme", "api", "main"); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("the check names without GitHub answered %v", err)
	}
}
