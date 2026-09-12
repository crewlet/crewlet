package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// gitlabRoutes is a GitLab that records which SERVICE-ACCOUNT ROUTES a run
// used, which is the only thing that distinguishes the two ownership modes.
//
// An instance service account lives at /service_accounts and its tokens at
// /personal_access_tokens; a group's live under the group's own path. The
// group route answers 404 as success for an account it does not own — GitLab
// makes no distinction between "unknown" and "already removed" — so a run
// sent down the wrong one reports every account deleted while they stay live.
type gitlabRoutes struct {
	mu     sync.Mutex
	hits   []string
	fresh  bool
	server *httptest.Server
}

func newGitLabRoutes(t *testing.T) *gitlabRoutes {
	t.Helper()
	f := &gitlabRoutes{}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *gitlabRoutes) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

// used reports whether any recorded call matched a substring.
func (f *gitlabRoutes) used(want string) bool {
	for _, hit := range f.called() {
		if strings.Contains(hit, want) {
			return true
		}
	}
	return false
}

func (f *gitlabRoutes) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
	f.mu.Lock()
	f.hits = append(f.hits, r.Method+" "+path)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && path == "/groups/acme":
		json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "full_path": "acme", "plan": "premium"})
	case r.Method == http.MethodGet && path == "/users":
		f.mu.Lock()
		fresh := f.fresh
		f.mu.Unlock()
		if fresh {
			// NOTHING YET, which is the state a pass creates from.
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		// THE ACCOUNT EXISTS, so a teardown has something to remove and
		// the route it removes it through is observable.
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 502, "username": r.URL.Query().Get("username"), "state": "active"}})
	case r.Method == http.MethodGet && strings.Contains(path, "/personal_access_tokens"):
		json.NewEncoder(w).Encode([]map[string]any{})
	case r.Method == http.MethodGet && path == "/user":
		json.NewEncoder(w).Encode(map[string]any{"id": 1, "username": "admin"})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/hooks"):
		// THE HOOK SWEEP, which runs before the accounts and has nothing to
		// do with either mode. An empty list rather than no body at all: a
		// decode failure here fails the teardown before it reaches the part
		// this case is about.
		json.NewEncoder(w).Encode([]map[string]any{})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/projects/"):
		json.NewEncoder(w).Encode(map[string]any{"id": 11})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/projects"):
		// THE GROUP'S PROJECTS, which a teardown walks in full so a hook on
		// a project the config no longer names is still removed. Empty
		// here: this case is about which OWNER an account is addressed
		// through, and a decode failure would fail the teardown before it
		// reached that.
		json.NewEncoder(w).Encode([]map[string]any{})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/service_accounts"):
		// A FRESH ACCOUNT, which is what a pass does for a seat that has
		// none — and the route it creates through is the other half of
		// what mode decides.
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{
			"id": 503, "username": body["username"]})
	case r.Method == http.MethodPost && strings.Contains(path, "/personal_access_tokens"):
		json.NewEncoder(w).Encode(map[string]any{
			"id": 9001, "name": "crewlet", "token": "glpat-minted-for-the-seat"})
	case r.Method == http.MethodGet && strings.Contains(path, "/members"):
		json.NewEncoder(w).Encode([]map[string]any{})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// gitlabModeCompany is a company whose GitLab accounts are owned as mode says.
func gitlabModeCompany(t *testing.T, url, mode string) *Company {
	t.Helper()
	doc := fmt.Sprintf(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
integrations:
  gitlab:
    enabled: true
    url: %q
    signing_secret: "whsec_Y3Jld2xldC10ZXN0LXNpZ25pbmcta2V5LTMyYnl0ZXM="
    provisioning:
      group: acme
      mode: %s
      admin_token: "glpat-fake-admin-token"
roles:
  - name: SWE
    handle: swe
    llm: zulu
    mcp_env:
      gitlab:
        GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"
`, url, mode)
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := &Engine{}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	return company
}

// sealingTestSink is a keyring that works, which a pass needs before it will
// create anything: [provision.CanMint] gates the account on having somewhere
// to seal the token, so a pass with no sink reports and creates nothing.
type sealingTestSink struct {
	mu   sync.Mutex
	held map[string]string
}

func newSealingTestSink() *sealingTestSink {
	return &sealingTestSink{held: map[string]string{}}
}

func (s *sealingTestSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[name] = value
	return nil
}

func (s *sealingTestSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.held[name]
	return v, ok, nil
}

func (*sealingTestSink) Discard(context.Context) error           { return nil }
func (*sealingTestSink) Forget(context.Context, ...string) error { return nil }
func (*sealingTestSink) Flush(context.Context) error             { return nil }
func (*sealingTestSink) Describe() string                        { return "a test keyring" }
func (*sealingTestSink) NextStep() string                        { return "nothing" }

// noAccountYet answers the seat lookup with "no such user", so a pass has to
// create one and the route it creates through is observable.
func (f *gitlabRoutes) noAccountYet() { f.mu.Lock(); f.fresh = true; f.mu.Unlock() }

// THE ENGINE READS WHERE THE ACCOUNTS ARE OWNED FROM THE DOCUMENT.
//
// `provisioning.mode` was absent from both sets of options the engine builds,
// so every pass and every teardown it ran assumed the GROUP route. Against a
// company provisioned with `-mode instance` that mints through a group which
// does not own the account and is refused — and on the way out it is worse,
// because the group delete answers 404 as success: the teardown reported every
// account removed while they stayed live with every credential they held.
//
// Asserted through the ROUTES, because that is the whole of the difference and
// it is what GitLab sees.
func TestTheEngineSendsGitLabWorkDownTheRouteTheDocumentNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode  string
		want  string
		wrong string
	}{
		{"instance", "/service_accounts", "/groups/7/service_accounts"},
		{"group", "/groups/7/service_accounts", "DELETE /service_accounts"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			f := newGitLabRoutes(t)
			e := &Engine{}
			e.epoch.current.Store(gitlabModeCompany(t, f.server.URL, tc.mode))

			pass := &gitlabPass{engine: e}
			if _, err := pass.Teardown(t.Context(),
				setup.TeardownInput{RemoveSeats: true}); err != nil {
				t.Fatalf("Teardown: %v", err)
			}
			if !f.used("DELETE " + tc.want) {
				t.Errorf("the teardown called %v, none of it %q: an account "+
					"removed through the owner that does not hold it answers "+
					"404, which reads as success while it stays live",
					f.called(), "DELETE "+tc.want)
			}
			if f.used(tc.wrong) {
				t.Errorf("the teardown called %v, which includes the other "+
					"owner's route %q", f.called(), tc.wrong)
			}
		})
	}
}

// AND THE SAME ON THE WAY IN, where a pass CREATES the account.
//
// The two are separate option literals in the engine and each was missing the
// mode independently, so one covered leaves the other free to be deleted with
// nothing noticing. Minting through a group that does not own the account is
// refused by GitLab, which the pass reads as a stale credential and answers by
// minting again.
func TestTheEngineCreatesGitLabAccountsThroughTheOwnerTheDocumentNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode  string
		want  string
		wrong string
	}{
		{"instance", "POST /service_accounts", "POST /groups/7/service_accounts"},
		{"group", "POST /groups/7/service_accounts", "POST /service_accounts"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			f := newGitLabRoutes(t)
			f.noAccountYet()
			e := &Engine{}
			e.epoch.current.Store(gitlabModeCompany(t, f.server.URL, tc.mode))

			pass := &gitlabPass{engine: e}
			if _, err := pass.Run(t.Context(), setup.PassInput{
				Sink: newSealingTestSink(),
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !f.used(tc.want) {
				t.Errorf("the pass called %v, none of it %q", f.called(), tc.want)
			}
			if f.used(tc.wrong) {
				t.Errorf("the pass called %v, which includes the other "+
					"owner's route %q", f.called(), tc.wrong)
			}
		})
	}
}
