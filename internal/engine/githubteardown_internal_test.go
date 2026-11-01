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
	// company-document write was — the two a disconnect records its
	// requester on.
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

func (w *seatWriter) Apply(_ context.Context, _ []byte, _ string, by iam.Actor) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropped = append(w.dropped, by)
	return nil
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
			// THE PATH AS AN ENTERPRISE HOST SERVES IT. A URL that is not
			// github.com takes /api/v3 in front of every route, so matching
			// the bare path here would answer 404 to the one call this
			// fixture exists for.
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

// AN UNINSTALLED SEAT STOPS CLAIMING AN INSTALLATION.
//
// A GitHub disconnect uninstalls each agent's app, which is what actually
// revokes its access, and it used to record nothing — so every reader went on
// believing the installation was there. Measured on a live disconnect: the
// seat still reported `satisfied: true` with its app slug while every other
// disconnected surface said it was waiting to be connected, and
// /query/integrations kept a github row alive on the strength of it, which
// the dashboard renders as Connecting with a `routes nowhere` badge,
// permanently.
//
// The credentials are deliberately NOT removed — GitHub offers no API to
// delete an app registration, so the key stays valid for an app that still
// exists and deleting the company's only copy is a move nothing can undo.
// The installation is the one fact this teardown actually changed at GitHub.
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

// AND A RECORD THAT COULD NOT BE WRITTEN FAILS THE TEARDOWN.
//
// The block is dropped only when the vendor step succeeds, so swallowing this
// leaves every seat claiming an installation that no longer exists with
// nothing that will ever look again. Holding instead keeps the surface in
// PhaseDisconnecting, and the next attempt uninstalls nothing (already gone)
// and writes the record again.
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
// (D9).
//
// A new agent seat is off on every integration until somebody turns it on, so
// an agent carrying no `integrations.github` block, or one switched off, is
// not a fault: reporting each as acting as nobody would read, on a company of
// many agents and one GitHub agent, as a problem with every other agent. What
// an agent that IS on still needs (a person creating its app) is the access
// sync's step for that seat.
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

	// THROUGH Run, not through the helper: what was missing was the WIRING,
	// so a case that called the helper directly would pass over a pass that
	// never calls it.
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
