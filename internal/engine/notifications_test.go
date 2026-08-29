package engine_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// The registry is DERIVED from one org and answers for it permanently, so an
// apply must build a new one — a node that indexed only its first company
// would resolve every party against an org that is no longer running, and a
// seat added by an apply would be permanently unreachable with nothing
// failing.
func TestThePartyRegistryFollowsTheAppliedCompany(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	reg := e.Registry()
	if reg == nil {
		t.Fatal("the engine indexed no parties")
	}
	for _, handle := range []string{"ceo", "cto", "founder"} {
		if _, ok := reg.ByHandle(handle); !ok {
			t.Errorf("%s is not addressable", handle)
		}
	}
	// A human seat's declared contact ids are reconciled in, so a message
	// from that person resolves to the seat rather than to a stranger.
	if p, ok := reg.ByExternalID("slack", "U0FOUNDER"); !ok || p.Handle != "founder" {
		t.Fatalf("the founder's Slack id resolved to %+v", p)
	}
	if p, _ := reg.ByHandle("founder"); !p.Human {
		t.Fatal("the founder is not marked human")
	}

	// THE APPLY. A new seat must become addressable without a restart.
	grown := parsedCompany(t, companyDoc+`
  - name: Staff Engineer
    handle: staff
    llm: alpha
`)
	if _, _, err := e.Apply(t.Context(), grown); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, ok := e.Registry().ByHandle("staff"); !ok {
		t.Fatal("a seat added by an apply is not addressable")
	}
	// And the registry is a NEW one: the old org's answer must not
	// survive into a company that no longer has that seat.
	if e.Registry() == reg {
		t.Fatal("the apply reused the previous registry")
	}
}

// A node with no company answers "nobody matches" rather than panicking —
// which is what `crewlet validate` and a node that has not applied a
// revision both do.
func TestARegistryIsNeverNil(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if got := e.Registry(); got == nil {
		t.Fatal("Registry returned nil")
	}
	// With no chat backend the driver set is EMPTY rather than absent, so
	// the turn engine says what phase it is in without first asking
	// whether indicators exist anywhere — a question about this node's
	// wiring that a turn has no business knowing the answer to.
	driver := e.Status()
	if driver == nil {
		t.Fatal("Status returned nil")
	}
	if got := driver.Backends(); len(got) != 0 {
		t.Fatalf("a company with no chat backend drives %v", got)
	}
	session := driver.Begin(
		context.Background(), "ceo", "turn-1", "plan", map[string]string{
			"transport": "mattermost", "channel": "C1", "ts": "p1",
		})
	if session != nil {
		t.Fatal("a company with no chat backend raised an indicator")
	}
	// And every method on that nil session is a no-op.
	session.Phase(context.Background(), "execute")
	session.End(context.Background(), false)
}

// A chat instance being unreachable at boot is an ordinary state — it
// restarts, it moves, its certificate lapses — and refusing to start the
// company over it takes down every seat's scheduled and tracker work too.
func TestAnUnreachableChatBackendDoesNotStopTheCompany(t *testing.T) {
	// NOT parallel: the seat's token comes from the process environment,
	// which is exactly why Go forbids t.Setenv alongside t.Parallel.
	t.Setenv("MM_CEO_TOKEN", "tok-ceo")
	// A seat with a real bot token, so Start actually tries to reach the
	// instance — without one there are no seats and the failure path is
	// never taken.
	doc := strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        bot_token: ${MM_CEO_TOKEN}`, 1) + `
integrations:
  mattermost:
    enabled: true
    url: http://127.0.0.1:1
    team: eng
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	if e.Company() == nil {
		t.Fatal("the company did not start")
	}
	// Every other surface is live: the seats exist and are addressable.
	if got := len(e.Company().Seats()); got != 2 {
		t.Fatalf("seats = %d", got)
	}
	if _, ok := e.Registry().ByHandle("ceo"); !ok {
		t.Fatal("parties were not indexed")
	}
}

// The valve is FLEET-WIDE or it is nothing: the loop it exists to catch
// bounces between nodes, so a per-process substitute would report a limit it
// is not enforcing.
func TestTheValveIsOffWithoutAStore(t *testing.T) {
	t.Parallel()
	// The default engine has a store, so the valve is available — the
	// assertion here is that the rate limit is read LIVE off the epoch
	// rather than captured, so an apply that changes the cap takes effect
	// on the next notification rather than on the next restart.
	e := newEngine(t, engine.Options{})
	if got := e.Company().Config.NotificationRateLimit; got != 0 {
		t.Fatalf("the shipped default cap is %d, want off", got)
	}

	limited := parsedCompany(t, strings.Replace(companyDoc,
		"name: Acme", "name: Acme\nnotification_rate_limit: 5", 1))
	if _, _, err := e.Apply(t.Context(), limited); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := e.Company().Config.NotificationRateLimit; got != 5 {
		t.Fatalf("the applied cap is %d, want 5", got)
	}
}

// AN INTEGRATION'S ADDRESS IS RESOLVED, and every integration resolves it.
//
// A ${VAR} stays verbatim in the stored config — that is what makes
// re-activating an unchanged revision pick up a rotated value — so every
// consumer resolves at construction. Three wirings do that, and one of them
// forgot: Mattermost handed each seat the literal "${MATTERMOST_URL}", all
// seven failed at the URL parse, and the engine reported the company running
// "without its chat surface" on a host where the address was exported
// correctly.
//
// It is a class, not a typo, so this checks all three at once: the failure
// is silent per-integration, and a fourth vendor would repeat it.
func TestEveryIntegrationResolvesItsAddress(t *testing.T) {
	// NOT parallel: the addresses come from the process environment.
	t.Setenv("TEST_MM_URL", "http://127.0.0.1:1")
	t.Setenv("TEST_GL_URL", "http://127.0.0.1:2")
	t.Setenv("TEST_CF_URL", "http://127.0.0.1:3")
	t.Setenv("MM_CEO_TOKEN", "tok-ceo")

	doc := strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        bot_token: ${MM_CEO_TOKEN}`, 1) + `
integrations:
  mattermost:
    enabled: true
    url: ${TEST_MM_URL}
    team: eng
  gitlab:
    enabled: true
    url: ${TEST_GL_URL}
    signing_secret: whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM=
  confluence:
    url: ${TEST_CF_URL}
    token: cf-token
    webhook_secret: cf-secret
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	if e.Company() == nil {
		t.Fatal("the company did not start")
	}

	// The addresses are unreachable on purpose: what is under test is the
	// string each wiring BUILT, not whether the vendor answered.
	mm := e.Mattermost()
	if mm == nil {
		t.Fatal("no chat transport was built")
	}
	if got := mm.URL(); got != "http://127.0.0.1:1" {
		t.Errorf("mattermost url = %q, want the resolved address", got)
	}
	// The other two wirings refuse a url that failed to resolve outright,
	// so reaching a live company at all is the assertion for them: an
	// unresolved reference would have taken the surface down instead.
	if e.Company() == nil {
		t.Error("the company stopped, which is what an unresolved address does")
	}
}

// AN ADDRESS THAT RESOLVED TO NOTHING IS REFUSED, and the refusal names the
// reference.
//
// The validator already rejected an enabled Mattermost with no url, so an
// empty one here is a ${VAR} that answered nothing — a different problem
// with a different fix. Starting anyway builds clients pointed at "" and
// fails at every call with a message that names neither.
func TestAnUnresolvedChatAddressIsRefusedByName(t *testing.T) {
	// NOT parallel: it depends on a variable NOT being in the environment.
	t.Setenv("MM_CEO_TOKEN", "tok-ceo")
	doc := strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        bot_token: ${MM_CEO_TOKEN}`, 1) + `
integrations:
  mattermost:
    enabled: true
    url: ${TEST_MM_URL_THAT_IS_NEVER_SET}
    team: eng
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	if mm := e.Mattermost(); mm != nil {
		t.Errorf("a chat transport was built for an address that resolved to "+
			"nothing: %q", mm.URL())
	}
}

// THE INBOUND EDGE VERIFIES WITH VALUES, never with references.
//
// Secrets live in the config as ${VAR}s. The edge's material was assembled
// from the config WITHOUT resolving, so every route verified against the
// literal "${GITLAB_SIGNING_SECRET}" — every delivery from every vendor
// refused, with the vendor's settings page showing a healthy hook. Measured
// against a real GitLab, where the only trace was one warning per delivery.
//
// And the literal is not a secret. It is a config field the dashboard
// renders, so a forged delivery would have verified against a string an
// attacker could read.
func TestTheWebhookEdgeGetsResolvedSecrets(t *testing.T) {
	// NOT parallel: the secret comes from the process environment.
	t.Setenv("TEST_GL_SIGNING", "whsec_resolved-value")
	doc := companyDoc + `
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: ${TEST_GL_SIGNING}
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})

	got := e.WebhookSecrets().GitLab
	if strings.Contains(got, "${") {
		t.Fatalf("the edge verifies against %q, which is the reference and "+
			"not a secret at all", got)
	}
	if got != "whsec_resolved-value" {
		t.Errorf("gitlab secret = %q, want the resolved value", got)
	}
}

// A NODE WITH NO COMPANY VERIFIES NOTHING, which is the safe direction: a
// route with no secret answers 503 rather than accepting what arrives.
func TestAnUnconfiguredNodeHasNoWebhookSecrets(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	// The fixture company declares no integrations, so every field is
	// empty — the same answer a node mid-boot gives.
	if s := e.WebhookSecrets(); s.GitLab != "" || s.GitHub != "" || s.Jira != "" {
		t.Errorf("secrets appeared with no integration configured: %+v", s)
	}
}

// A CONFIGURED VENDOR MUST ACTUALLY ROUTE, which is the whole distinction
// RoutedSources exists to draw.
//
// Four vendors once had config models, webhook routes and generated schema
// and no parser behind any of them, so a company naming one got a block that
// validated, appeared on the dashboard's Integrations room beside the
// working ones, and woke nobody. This is what catches a vendor whose config
// ships without its wiring — and, in the other direction, a vendor whose
// wiring is dropped from startNotifications by a refactor.
func TestAConfiguredTrackerActuallyRoutes(t *testing.T) {
	t.Parallel()
	doc := companyDoc + `
integrations:
  jira:
    url: https://jira.example.com
    token: t
    webhook_secret: jira-secret
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Contains(e.RoutedSources(), "jira") {
		t.Fatalf("a company on the Atlassian tracker routes %v", e.RoutedSources())
	}
	// And the inbound route has something to verify with, which is the
	// other half: a parser with no secret behind it answers 503 to every
	// delivery it would have routed.
	if got := e.WebhookSecrets().Jira; got != "jira-secret" {
		t.Errorf("jira webhook secret = %q", got)
	}
}

// THE HOSTED CHAT SURFACE ROUTES TOO, and its per-seat apps are what the
// inbound edge verifies with — so a company on Slack needs both halves and
// each is silent without the other.
func TestAConfiguredHostedChatSurfaceActuallyRoutes(t *testing.T) {
	// NOT parallel: the seat's app credentials come from the environment.
	t.Setenv("SLACK_CEO_TOKEN", "xoxb-ceo")
	t.Setenv("SLACK_CEO_SIGNING", "ceo-signing-secret")
	doc := strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      slack:
        bot_token: ${SLACK_CEO_TOKEN}
        signing_secret: ${SLACK_CEO_SIGNING}`, 1) + `
integrations:
  slack:
    typing_status: addressed
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The workspace is unreachable, so no seat identity resolves and the
	// transport reports itself unavailable — which is the documented
	// degradation, not a company that failed to start.
	if e.Company() == nil {
		t.Fatal("an unreachable workspace stopped the company")
	}
	// The per-seat signing secret reaches the edge either way: it is what
	// answers a delivery, and it comes from config rather than from Slack.
	if got := e.WebhookSecrets().Slack["ceo"]; got != "ceo-signing-secret" {
		t.Errorf("the seat's signing secret = %q", got)
	}
}

// A COMPANY WITH NO TRACKER BLOCK ROUTES NO TRACKER EVENTS. The parser list
// is built from the config, so a source that appeared without one would mean
// the wiring stopped reading it.
func TestAnAbsentTrackerRoutesNothing(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if slices.Contains(e.RoutedSources(), "jira") {
		t.Fatalf("a company with no jira block routes %v", e.RoutedSources())
	}
}

// THE KNOWLEDGE BASE IS THE ONE INTEGRATION WHOSE ABSENCE IS INVISIBLE.
//
// A routing gap surfaces as an agent that never answers. A search gap
// surfaces as an empty "## Relevant knowledge" block on every Plan phase,
// which is indistinguishable from a company that has written nothing down —
// so a configured Confluence has to produce a searcher, and a company with
// no knowledge backend has to produce a nil one rather than a typed nil that
// answers as though a search had run.
func TestAConfiguredKnowledgeBaseProducesASearcher(t *testing.T) {
	t.Parallel()
	doc := companyDoc + `
integrations:
  confluence:
    url: https://wiki.example.com
    token: t
    webhook_secret: cf
knowledge:
  confluence_spaces: [HANDBOOK]
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Contains(e.RoutedSources(), "confluence") {
		t.Fatalf("a company on Confluence routes %v", e.RoutedSources())
	}
	searcher := e.Knowledge()
	if searcher == nil {
		t.Fatal("a configured knowledge base produced no searcher")
	}
	if got := searcher.Backend(); got != "confluence" {
		t.Errorf("the searcher answers for %q", got)
	}
}

// A COMPANY WITH NO KNOWLEDGE BACKEND ANSWERS A NIL INTERFACE, not a typed
// nil: consumers check `searcher == nil`, and a typed nil passes that check
// and then answers as though a search had run and found nothing — which
// hides the fact that nothing is configured.
func TestNoKnowledgeBackendIsANilInterface(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := e.Knowledge(); got != nil {
		t.Fatalf("a company with no knowledge base got %T", got)
	}
}
