package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
)

// A SEAT IDENTITY THAT FAILED TO RESOLVE IS ASKED AGAIN, WITH NO CONFIG
// CHANGE — AND SAYS SO UNTIL IT DOES.
//
// A tracker webhook names people by account, and nothing in the org model says
// which account a seat holds, so the engine asks with the seat's own
// credential. A lookup that FAILS leaves the seat unresolved rather than
// failing the boot, on the reasoning that the instance may be briefly down and
// the next apply retries.
//
// Nothing schedules an apply. Measured: a seat's token was created and sealed,
// the tracker checked it one second later, the account was not grantable yet
// and answered 403 — and the surface sat at zero seat identities with every
// card reading Connected. Two minutes later it had still not asked again.
//
// Both halves are the same fix. The unresolved seat is a FINDING, so the
// surface stops reporting itself ready and the reconcile loop keeps visiting
// it on the engine's own cadence; and the visit RE-RESOLVES before it reports,
// so the seat that answers is registered into the live registry and starts
// receiving work immediately.
func TestASeatIdentityThatFailedIsRetriedOnTheNextPass(t *testing.T) {
	var refuse atomic.Bool
	refuse.Store(true)

	var lookups atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/myself" {
			http.Error(w, "no such route", http.StatusNotFound)
			return
		}
		lookups.Add(1)
		if refuse.Load() {
			// EXACTLY THE FIELD'S SHAPE: the account exists and the
			// credential is real, and the instance is not ready to say
			// who it is yet.
			http.Error(w, "not grantable yet", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"agent-ceo"}`)
	}))
	t.Cleanup(srv.Close)

	e, company := routingEngine(t, srv.URL)

	// THE APPLY, which resolves what it can and leaves this seat behind.
	e.refreshParties(company)
	if _, err := e.startJira(t.Context(), company, company.Config.Integrations.Jira); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := e.Registry().ByExternalID(jira.Backend, "agent-ceo"); ok {
		t.Fatal("precondition: the seat resolved while the instance was refusing")
	}
	before := lookups.Load()
	if before == 0 {
		t.Fatal("precondition: the apply never asked the instance at all")
	}

	// A PASS, which is what the reconcile loop does on its own cadence and
	// what nothing did before: no config changed, nobody pressed anything.
	found := e.resolveRouting(t.Context(), integration.KindJira)

	if lookups.Load() <= before {
		t.Error("the pass did not ask again, so a lookup that fails once " +
			"stays failed until somebody edits the configuration")
	}
	if len(found) != 1 || found[0].Subject != "ceo" {
		t.Fatalf("findings = %+v, want one naming the unresolved seat", found)
	}
	// THE CARD STOPS SAYING READY. An empty finding list is what the loop
	// reads as a healthy surface and holds for a full settled interval.
	if phase, actor := found[0].Kind.Verdict(); phase == integration.PhaseReady {
		t.Errorf("an unresolved seat classifies %s/%s, so the card reads "+
			"Connected over an agent that receives nothing", phase, actor)
	}

	// AND WHEN THE INSTANCE COMES GOOD, the next pass wires it — into the
	// LIVE registry, with no apply between.
	refuse.Store(false)
	if found := e.resolveRouting(t.Context(), integration.KindJira); len(found) != 0 {
		t.Errorf("findings = %+v after the instance answered, want none", found)
	}
	party, ok := e.Registry().ByExternalID(jira.Backend, "agent-ceo")
	if !ok {
		t.Fatal("the seat never reached the live registry, so every issue " +
			"naming it still falls through to the project lead")
	}
	if party.Handle != "ceo" {
		t.Errorf("agent-ceo resolved to %q, want ceo", party.Handle)
	}
}

// A SEAT THAT HOLDS NO CREDENTIAL IS NOT A FINDING.
//
// It has opted out of the surface, which is a choice rather than a fault.
// Reporting it would put every human seat in the company on the card and bury
// the one seat that is actually broken.
func TestASeatWithNoCredentialIsNotReportedAsUnresolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nobody should ask", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	e, company := routingEngine(t, srv.URL)
	// Drop the one credentialled seat's token, leaving a company whose
	// seats simply do not use the tracker.
	for role := range company.Config.EachRole() {
		delete(role.MCPEnv, "jira")
	}
	rebuilt, err := NewCompanyWith(company.Config, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.epoch.current.Store(rebuilt)
	e.refreshParties(rebuilt)

	if found := e.resolveRouting(t.Context(), integration.KindJira); len(found) != 0 {
		t.Errorf("findings = %+v, want none: no seat here claims a tracker "+
			"identity, so none of them is missing one", found)
	}
}

// A SURFACE THAT READS ITS SEATS OUT OF THE DOCUMENT REPORTS NOTHING HERE.
//
// Only the three that ask a third-party app who a credential belongs to can
// have a lookup fail. Slack and Mattermost learn an identity at connect,
// Confluence and Datadog route by the handle itself, and Atlassian ingests
// nothing at all — so a finding for any of them would be an invention.
func TestOnlyTheSurfacesThatLookUpAnAccountReportRouting(t *testing.T) {
	t.Parallel()
	e, company := routingEngine(t, "https://jira.example.com")
	e.refreshParties(company)

	for _, kind := range integration.Kinds {
		switch kind {
		case integration.KindJira, integration.KindGitLab, integration.KindGitHub:
			continue
		}
		if found := e.resolveRouting(t.Context(), kind); len(found) != 0 {
			t.Errorf("%s reported %+v; it resolves a seat from the document "+
				"and has no lookup to have failed", kind, found)
		}
	}
}

// routingEngine is a node running one tracker-credentialled seat against the
// instance at url.
func routingEngine(t *testing.T, instance string) (*Engine, *Company) {
	t.Helper()
	e := &Engine{}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
integrations:
  jira:
    url: ` + instance + `
    token: "${JIRA_ORG_TOKEN}"
    webhook_secret: "whsec_Y3Jld2xldC10ZXN0LWppcmEtd2ViaG9vay1rZXktMzI="
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      jira: {JIRA_API_TOKEN: "seat-token"}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	// PUBLISHED, because a pass reads the LIVE epoch rather than being
	// handed one: it runs on a timer, long after whoever applied has gone.
	e.epoch.current.Store(company)
	return e, company
}

// THE LOOP'S OWN TICK CARRIES THEM, which is the only reason any of the above
// is ever retried.
//
// A vendor pass cannot see this: a seat's account is read with that seat's own
// credential by the ENGINE, and the pass asks the third-party app about the
// surface. So the adapter every pass reaches the loop through appends what
// this node could not resolve to what the pass found — and if it did not, the
// surface would report itself ready over a seat receiving nothing, the loop
// would hold that answer for a settled interval, and nothing would ask again.
func TestTheLoopTickCarriesTheEnginesOwnRoutingFindings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not grantable yet", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	e, company := routingEngine(t, srv.URL)
	e.refreshParties(company)

	// A pass whose own world is entirely converged: every finding below
	// belongs to the engine.
	c := &passConverger{engine: e, pass: &convergedPass{kind: integration.KindJira}}
	found, err := c.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(found) != 1 || found[0].Subject != "ceo" {
		t.Fatalf("findings = %+v, want the unresolved seat: a converged "+
			"vendor pass answers nothing, and an empty list is what the loop "+
			"reads as a healthy surface", found)
	}
}
