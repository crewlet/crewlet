package engine

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// seatWriter is a config surface that holds one seat's JSON and records every
// write, which is the only way to see what a teardown recorded.
type seatWriter struct {
	mu    sync.Mutex
	seats map[string]map[string]any
	wrote []string
	fail  error

	// authors is who each seat write was recorded as, and dropped who each
	// company-document write was: where a disconnect records its requester.
	authors []iam.Actor
	dropped []iam.Actor
}

func newSeatWriter(handle string, app map[string]any) *seatWriter {
	return &seatWriter{seats: map[string]map[string]any{
		handle: {
			"name": "SRE Lead", "handle": handle,
			"integrations": map[string]any{"github": app},
		},
	}}
}

func (w *seatWriter) Apply(context.Context, []byte, string, iam.Actor) error { return nil }

func (w *seatWriter) Disconnect(_ context.Context, _ integration.Kind, _ string, by iam.Actor) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropped = append(w.dropped, by)
	return nil, nil
}

func (w *seatWriter) Seat(_ context.Context, handle string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	role, ok := w.seats[handle]
	if !ok {
		return nil, fmt.Errorf("no seat %q", handle)
	}
	return json.Marshal(role)
}

func (w *seatWriter) SetSeat(
	_ context.Context, handle string, body []byte, summary string, by iam.Actor,
) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		return w.fail
	}
	var role map[string]any
	if err := json.Unmarshal(body, &role); err != nil {
		return err
	}
	w.seats[handle] = role
	w.wrote = append(w.wrote, summary)
	w.authors = append(w.authors, by)
	return nil
}

// recorded is who the writes so far were recorded as: every seat write, then
// every company-document write.
func (w *seatWriter) recorded() (seats, documents []iam.Actor) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.authors), slices.Clone(w.dropped)
}

func (w *seatWriter) Reload(context.Context, string, iam.Actor) error { return nil }

// installation reads back what one seat's record now claims.
func (w *seatWriter) installation(handle string) (any, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	role := w.seats[handle]
	integrations, _ := role["integrations"].(map[string]any)
	block, _ := integrations["github"].(map[string]any)
	id, ok := block["installation_id"]
	return id, ok
}

// testAppKey is a real RSA key, because [github.UninstallSeat] mints an app
// JWT from it before it can make a single call.
func testAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// githubTeardownWorld is an engine holding one agent with an installed app,
// pointed at a GitHub that accepts the uninstall.
func githubTeardownWorld(t *testing.T) (*Engine, *seatWriter, *atomic.Int64) {
	t.Helper()
	pemKey := testAppKey(t)
	var uninstalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// THE PATH AS AN ENTERPRISE HOST SERVES IT: a non-github.com
			// URL prefixes every route with /api/v3.
			if r.Method == http.MethodDelete &&
				strings.Contains(r.URL.Path, "/app/installations/") {
				uninstalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
	t.Cleanup(srv.Close)

	doc := fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  github:
    enabled: true
    url: %q
    webhook_secret: "${GH_SIGN}"
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: zulu
    integrations:
      github:
        enabled: false
        app_id: 41
        app_slug: acme-sre-lead
        installation_id: 161055374
        private_key: |
%s
`, srv.URL, indent(pemKey, "          "))
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := &Engine{}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.epoch.current.Store(company)

	writer := newSeatWriter("sre-lead", map[string]any{
		"app_id": 41, "app_slug": "acme-sre-lead",
		"installation_id": 161055374, "private_key": pemKey,
	})
	e.UseConfigWriter(writer)
	return e, writer, &uninstalls
}

func indent(s, with string) string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out = append(out, with+line)
	}
	return strings.Join(out, "\n")
}

// AN UNINSTALLED SEAT STOPS CLAIMING AN INSTALLATION: a disconnect that
// uninstalls each agent's app records it, or the seat reads `satisfied` and
// /query/integrations keeps a github row Connecting forever.
//
// The credentials are deliberately NOT removed: GitHub has no API to delete
// an app registration, so the key stays valid and is the company's only copy.
func TestAGitHubDisconnectStopsTheSeatClaimingAnInstallation(t *testing.T) {
	t.Parallel()
	e, writer, uninstalls := githubTeardownWorld(t)

	pass := &githubPass{engine: e}
	removed, err := pass.Teardown(t.Context(), setup.TeardownInput{RemoveSeats: true})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if got := uninstalls.Load(); got != 1 {
		t.Fatalf("the premise is wrong: %d uninstall(s) reached GitHub", got)
	}
	if got, ok := writer.installation("sre-lead"); !ok || fmt.Sprint(got) != "0" {
		t.Errorf("installation_id = %v (present: %v), want 0: the installation "+
			"this teardown removed at GitHub is still claimed by the record, "+
			"so the seat reads as a finished agent on a card that is gone",
			got, ok)
	}
	// AND NOTHING IS REPORTED REMOVED, so no credential is deleted.
	if len(removed.Accounts) != 0 {
		t.Errorf("removed = %+v, want nothing: the app survives a disconnect "+
			"and its key is the only copy there is", removed.Accounts)
	}
}

// AND A RECORD THAT COULD NOT BE WRITTEN FAILS THE TEARDOWN: swallowed, the
// block drops with every seat claiming a gone installation and nothing ever
// looking again. Held, the surface stays PhaseDisconnecting and the next
// attempt writes the record again.
func TestAGitHubDisconnectThatCannotRecordTheUninstallHolds(t *testing.T) {
	t.Parallel()
	e, writer, _ := githubTeardownWorld(t)
	writer.fail = fmt.Errorf("the config store is unreachable")

	pass := &githubPass{engine: e}
	if _, err := pass.Teardown(t.Context(), setup.TeardownInput{RemoveSeats: true}); err == nil {
		t.Fatal("a teardown that could not record the uninstall reported success, " +
			"so the block drops with the record still claiming an installation")
	}
}

// AN AGENT WITH NO APP IS OFF AT GITHUB, AND THE PASS SAYS NOTHING ABOUT IT
// (D9): a seat is off on every integration until turned on, so no block, or
// one switched off, is not a fault. An agent that IS on and lacks its app is
// the access sync's step for that seat.
func TestTheGitHubPassSaysNothingAboutAnAgentThatIsOff(t *testing.T) {
	t.Parallel()
	doc := `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  github:
    enabled: true
    webhook_secret: "whsec_Y3Jld2xldC10ZXN0LXNpZ25pbmcta2V5LTMyYnl0ZXM="
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: zulu
  - name: Reviewer
    handle: reviewer
    llm: zulu
    integrations:
      github: {enabled: false}
  - name: Jane Founder
    handle: founder
    kind: human
    contact:
      github_login: jane
`
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// A FLEET STORE, which every engine [New] builds has: the pass's access
	// sync reads the seat access ledger kept there.
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.epoch.current.Store(company)

	// THROUGH Run, not the helper: the invariant is the WIRING, which a
	// direct call would pass over.
	pass := &githubPass{engine: e}
	findings, err := pass.Run(t.Context(), setup.PassInput{
		Sink:        newSealingTestSink(),
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range findings {
		for _, off := range []string{"sre-lead", "reviewer", "founder"} {
			if f.Subject == off || slices.Contains(f.Subjects, off) ||
				strings.Contains(f.Detail, off) {
				t.Errorf("the pass reported %s, which is off at GitHub: %+v", off, f)
			}
		}
		if strings.Contains(f.Detail, "no GitHub App of it") {
			t.Errorf("the pass reported agents with no app as a fault: %+v", f)
		}
	}
}
