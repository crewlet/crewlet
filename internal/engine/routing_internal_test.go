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

// A seat identity whose lookup failed is asked again on the next pass, with no
// config change, and is reported as a finding (so the surface is not ready)
// until it resolves; once it does, it is in the live registry at once.
func TestASeatIdentityThatFailedIsRetriedOnTheNextPass(t *testing.T) {
	t.Parallel()
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
			// A real account and credential the instance will not
			// answer for yet.
			http.Error(w, "not grantable yet", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"agent-ceo"}`)
	}))
	t.Cleanup(srv.Close)

	e, company := routingEngine(t, srv.URL)

	// The apply, which leaves this seat unresolved.
	e.refreshParties(t.Context(), company)
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

	// A reconcile pass, with no config change.
	found := e.resolveRouting(t.Context(), integration.KindJira, nil)

	if lookups.Load() <= before {
		t.Error("the pass did not ask again, so a lookup that fails once " +
			"stays failed until somebody edits the configuration")
	}
	if len(found) != 1 || found[0].Subject != "ceo" {
		t.Fatalf("findings = %+v, want one naming the unresolved seat", found)
	}
	// The finding must not classify as ready, or the card reads Connected.
	if phase, actor := found[0].Kind.Verdict(); phase == integration.PhaseReady {
		t.Errorf("an unresolved seat classifies %s/%s, so the card reads "+
			"Connected over an agent that receives nothing", phase, actor)
	}

	// Once the instance answers, the next pass wires the seat into the live
	// registry, with no apply between.
	refuse.Store(false)
	if found := e.resolveRouting(t.Context(), integration.KindJira, nil); len(found) != 0 {
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

// A seat that holds no credential has opted out of the surface and is not a
// finding; reporting it would bury the one seat that is actually broken.
func TestASeatWithNoCredentialIsNotReportedAsUnresolved(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nobody should ask", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	e, company := routingEngine(t, srv.URL)
	// Drop the one credentialled seat's token.
	for role := range company.Config.EachRole() {
		delete(role.MCPEnv, "jira")
	}
	rebuilt, err := NewCompanyWith(company.Config, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.epoch.current.Store(rebuilt)
	e.refreshParties(t.Context(), rebuilt)

	if found := e.resolveRouting(t.Context(), integration.KindJira, nil); len(found) != 0 {
		t.Errorf("findings = %+v, want none: no seat here claims a tracker "+
			"identity, so none of them is missing one", found)
	}
}

// Only Jira, GitLab and GitHub look an account up, so only they can report an
// unresolved seat; any other surface reporting one would be inventing it.
func TestOnlyTheSurfacesThatLookUpAnAccountReportRouting(t *testing.T) {
	t.Parallel()
	e, company := routingEngine(t, "https://jira.example.com")
	e.refreshParties(t.Context(), company)

	for _, kind := range integration.Kinds {
		switch kind {
		case integration.KindJira, integration.KindGitLab, integration.KindGitHub:
			continue
		}
		if found := e.resolveRouting(t.Context(), kind, nil); len(found) != 0 {
			t.Errorf("%s reported %+v; it resolves a seat from the document "+
				"and has no lookup to have failed", kind, found)
		}
	}
}

// routingEngine is a node running one tracker-credentialled seat against the
// instance at url.
func routingEngine(t *testing.T, instance string) (*Engine, *Company) {
	t.Helper()
	e := sealingNode(t)
	// An empty environment, so ${JIRA_ORG_TOKEN} is unset whatever the
	// runner exports.
	e.environ = config.MapSource{}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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
	// Published, because a pass reads the live epoch rather than being
	// handed one.
	e.epoch.current.Store(company)
	return e, company
}

// The adapter every pass reaches the loop through ([passConverger]) appends
// the engine's unresolved seats to the pass's findings. A vendor pass cannot
// see them, and without them a converged pass reads as a healthy surface, so
// the loop would never retry.
func TestTheLoopTickCarriesTheEnginesOwnRoutingFindings(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not grantable yet", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	e, company := routingEngine(t, srv.URL)
	e.refreshParties(t.Context(), company)

	// A converged pass: every finding below is the engine's.
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

// The retry makes the refusal [Engine.startGitLab] makes, through the same
// function, and asks nobody anything: a code host whose signing secret does
// not resolve answers 503 to every delivery, so wiring seats behind it would
// report agents that receive nothing.
func TestTheRetryWillNotWireASurfaceTheStartPathRefuses(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("the retry asked %s about a code host it cannot verify a "+
			"delivery for", r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	// Both through a ${VAR}: config itself refuses an unusable literal.
	for _, bad := range []struct{ name, value string }{
		{"a reference nothing answers", ""},
		{"a value the vendor could not have produced", "not-a-whsec-value"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			t.Parallel()
			// The case's own environment, independent of the runner's.
			env := config.MapSource{}
			if bad.value != "" {
				env["GITLAB_SIGNING_FOR_THIS_CASE"] = bad.value
			}
			e := &Engine{environ: env}
			cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  gitlab:
    enabled: true
    url: ` + srv.URL + `
    signing_secret: "${GITLAB_SIGNING_FOR_THIS_CASE}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      gitlab: {GITLAB_TOKEN: "seat-token"}
`))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			company, err := NewCompanyWith(cfg, e.resolver())
			if err != nil {
				t.Fatalf("company: %v", err)
			}
			e.epoch.current.Store(company)
			e.refreshParties(t.Context(), company)

			if found := e.resolveRouting(t.Context(), integration.KindGitLab, nil); len(found) != 0 {
				t.Errorf("findings = %+v; the surface's own pass reports the "+
					"unusable secret, and one finding per seat here buries it", found)
			}
			if _, ok := e.Registry().ByExternalID("gitlab", "anyone"); ok {
				t.Error("an identity was registered for a surface that can " +
					"verify no delivery")
			}
		})
	}
}

// One cause is one finding: a seat the surface's own pass already reported
// gets none from the engine's wiring, whose second finding would disagree
// with the first about who has to act.
func TestASeatTheSurfacesOwnPassReportedIsNotReportedTwice(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not grantable yet", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	e, company := routingEngine(t, srv.URL)
	e.refreshParties(t.Context(), company)

	if found := e.resolveRouting(t.Context(), integration.KindJira,
		map[string]bool{"ceo": true}); len(found) != 0 {
		t.Fatalf("findings = %+v, want none: the pass already reported ceo", found)
	}
	// A seat the pass said nothing about still gets one.
	if found := e.resolveRouting(t.Context(), integration.KindJira,
		map[string]bool{"someone-else": true}); len(found) != 1 {
		t.Fatalf("findings = %+v, want the unresolved seat reported", found)
	}
}

// The recovery is logged once per change of the resolved count: never would
// leave `jira_has_no_seat_identities` as the log's last word, and every pass
// would repeat it several times a minute.
func TestTheSeatIdentityRecoveryIsLoggedOnTheTransition(t *testing.T) {
	t.Parallel()
	var r rewireLog
	if !r.changed(integration.KindJira, 1) {
		t.Fatal("the first pass that resolved a seat said nothing")
	}
	if r.changed(integration.KindJira, 1) {
		t.Error("an unchanged count logged again, which is the loop narrating " +
			"its own schedule several times a minute")
	}
	if !r.changed(integration.KindJira, 2) {
		t.Error("a seat that newly resolved said nothing")
	}
	// Zero is not news: the finding and the warning already say it.
	if r.changed(integration.KindGitLab, 0) {
		t.Error("a surface that resolved nothing logged a recovery")
	}
	// The surfaces do not share a count.
	if !r.changed(integration.KindGitLab, 1) {
		t.Error("gitlab was silenced by jira's count")
	}
}
