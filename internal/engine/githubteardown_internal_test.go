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
}

func newSeatWriter(handle string, app map[string]any) *seatWriter {
	return &seatWriter{seats: map[string]map[string]any{
		handle: {
			"name": "SRE Lead", "handle": handle,
			"integrations": map[string]any{"github": app},
		},
	}}
}

func (w *seatWriter) Apply(context.Context, []byte, string, string) error { return nil }

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
	_ context.Context, handle string, body []byte, summary, _ string,
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
	return nil
}

func (w *seatWriter) Reload(context.Context, string, string) error { return nil }

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
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
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

// THE PASS REPORTS THE AGENTS THAT HAVE NO APP AT ALL.
//
// The engine builds its seat list from the seats carrying an
// `integrations.github` block, because that block is where an app's id, slug
// and key are recorded — so a seat that has never had one is absent from the
// pass's input and github's "no app" arm cannot be reached for it. Measured
// on a live connect: `phase: ready`, `findings: []`, one agent, no app, and
// the seat's own row three screens away saying it acts as nobody.
//
// APPROVAL_REQUIRED, because the engine can never create an app: it is a form
// POST from a page carrying a person's own GitHub session.
func TestTheGitHubPassReportsAgentsWithNoAppOfTheirOwn(t *testing.T) {
	t.Parallel()
	pemKey := testAppKey(t)
	doc := fmt.Sprintf(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
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
      github:
        app_id: 42
        app_slug: acme-reviewer
        installation_id: 7
        private_key: |
%s
  - name: Jane Founder
    handle: founder
    kind: human
    contact:
      github_login: jane
`, indent(pemKey, "          "))
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
	var got *integration.Finding
	for i, f := range findings {
		// MATCHED ON THE STABLE HALF. "of their own" is the plural form
		// and this company has one such seat, which now names the agent
		// rather than counting it — so the clause the two share is what
		// picks the finding out.
		if f.Kind == integration.FindingApprovalRequired &&
			strings.Contains(f.Detail, "no GitHub App of it") {
			got = &findings[i]
		}
	}
	if got == nil {
		t.Fatalf("findings = %v: an agent with no app of its own was reported "+
			"as nothing, so the card reads Connected over an agent that can "+
			"do nothing", findings)
	}
	if !strings.Contains(got.Detail, "sre-lead") {
		t.Errorf("the finding does not name the agent:\n%s", got.Detail)
	}
	// NOT THE SEAT THAT HAS ONE, and not the person: a human seat has its
	// own GitHub account, so creating an app for one would be a second
	// identity for somebody who already has one.
	for _, wrong := range []string{"reviewer", "founder"} {
		if strings.Contains(got.Detail, wrong) ||
			slices.Contains(got.Subjects, wrong) {
			t.Errorf("the finding names %s:\n%s / %v", wrong, got.Detail, got.Subjects)
		}
	}
}
