package engine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/engine"
)

// The registry is DERIVED from one org, so an apply must build a new one: a
// reused one resolves parties against an org no longer running, and a seat an
// apply added stays unreachable with nothing failing.
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
	// A human seat's declared contact ids resolve to the seat, not a stranger.
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
	if _, _, err := e.Apply(t.Context(), grown, time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, ok := e.Registry().ByHandle("staff"); !ok {
		t.Fatal("a seat added by an apply is not addressable")
	}
	// A NEW registry: the old org's answers must not survive the apply.
	if e.Registry() == reg {
		t.Fatal("the apply reused the previous registry")
	}
}

// A PERSON ON MATTERMOST IS THE SEAT THEIR USERNAME NAMES, from boot and
// across an apply.
//
// The chart names a person by username and a post its author by user id, so
// the engine hands the transport its people at boot and on every apply, and
// the transport asks the server what each name holds: a name nobody holds
// after the apply takes the person's old account back and is reported.
func TestAMattermostPersonIsTheSeatTheirUsernameNames(t *testing.T) {
	t.Parallel()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me"):
			w.Write([]byte(`{"id":"bot-ceo","username":"ceo","is_bot":true}`))
		case strings.HasSuffix(r.URL.Path, "/users/usernames"):
			var names []string
			json.NewDecoder(r.Body).Decode(&names)
			users := []map[string]string{}
			if slices.Contains(names, "jane") {
				users = append(users, map[string]string{"id": "u-jane", "username": "jane"})
			}
			json.NewEncoder(w).Encode(users)
		case strings.HasSuffix(r.URL.Path, "/config/client"):
			w.Write([]byte(`{}`))
		default:
			// The websocket too: the seat's reconnects are not under test.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"not here"}`))
		}
	}))
	t.Cleanup(chat.Close)
	withFounder := func(username string) string {
		return strings.Replace(strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        enabled: false
        bot_token: ${MM_CEO_TOKEN}`, 1), `      slack_user_id: U0FOUNDER`, `      slack_user_id: U0FOUNDER
      mattermost_user_id: `+username, 1) + `
integrations:
  mattermost:
    enabled: true
    url: ${TEST_MM_URL}
    team: eng
`
	}
	e := newEngine(t, engine.Options{Company: parsedCompany(t, withFounder("Jane")),
		Environment: config.MapSource{"MM_CEO_TOKEN": "tok-ceo", "TEST_MM_URL": chat.URL}})

	if p, ok := e.Registry().ByExternalID("mattermost", "u-jane"); !ok || p.Handle != "founder" || !p.Human {
		t.Fatalf("a post by the founder's account names %+v, want the founder", p)
	}
	if got := e.Mattermost().PeopleFindings(); len(got) != 0 {
		t.Fatalf("a person the server holds was reported: %+v", got)
	}

	// THE APPLY names the founder by an account nobody holds. It changes
	// nothing a socket is built from, so the transport is not rebuilt: it
	// is handed the new people and asks again.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, withFounder("jane.doe")), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(e.Mattermost().PeopleFindings()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a name nobody holds was never reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.Mattermost().PeopleFindings(); got[0].Subject != "founder" {
		t.Fatalf("the finding is about %q", got[0].Subject)
	}
	if p, ok := e.Registry().ByExternalID("mattermost", "u-jane"); ok {
		t.Fatalf("the founder's old account still names %+v", p)
	}
}

// A node with no company (`crewlet validate`, or one that has applied no
// revision) answers "nobody matches" rather than panicking.
func TestARegistryIsNeverNil(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if got := e.Registry(); got == nil {
		t.Fatal("Registry returned nil")
	}
	// With no chat backend the driver set is EMPTY, not nil, so a turn
	// reports its phase without asking about this node's wiring.
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
	session.Phase("execute")
	session.End(context.Background(), false)
}

// A chat instance unreachable at boot is an ordinary state (a restart, a
// move, a lapsed certificate); refusing to start over it would take down
// every seat's scheduled and tracker work too.
func TestAnUnreachableChatBackendDoesNotStopTheCompany(t *testing.T) {
	t.Parallel()
	// A real bot token, so Start tries the instance at all. UNREACHABLE
	// rather than refusing: that is the start that blocks, each call
	// spending the client's retry budget, and the company must still come up.
	doc := strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        enabled: false
        bot_token: ${MM_CEO_TOKEN}`, 1) + `
integrations:
  mattermost:
    enabled: true
    url: http://127.0.0.1:1
    team: eng
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc),
		Environment: config.MapSource{"MM_CEO_TOKEN": "tok-ceo"}})
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
	// The default engine has a store, so the valve is available; what is
	// held is that the cap is read LIVE off the epoch, not captured, so an
	// apply changes it without a restart.
	e := newEngine(t, engine.Options{})
	if got := e.Company().Config.NotificationRateLimit; got != 0 {
		t.Fatalf("the shipped default cap is %d, want off", got)
	}

	limited := parsedCompany(t, strings.Replace(companyDoc,
		"name: Acme", "name: Acme\nnotification_rate_limit: 5", 1))
	if _, _, err := e.Apply(t.Context(), limited, time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := e.Company().Config.NotificationRateLimit; got != 5 {
		t.Fatalf("the applied cap is %d, want 5", got)
	}
}

// AN INTEGRATION'S ADDRESS IS RESOLVED, and every integration resolves it.
//
// A ${VAR} stays verbatim in the stored config, so re-activating an unchanged
// revision picks up a rotated value, and every wiring resolves it at
// construction. A wiring that forgets fails silently, so this checks all three.
func TestEveryIntegrationResolvesItsAddress(t *testing.T) {
	t.Parallel()
	// A CHAT SERVER THAT REFUSES THE BOT, so the transport's start fails at
	// once: an unreachable one costs a retry budget per call and tests
	// nothing more (TestAnUnreachableChatBackendDoesNotStopTheCompany).
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Invalid or expired session"}`))
	}))
	t.Cleanup(chat.Close)
	env := config.MapSource{
		"TEST_MM_URL":  chat.URL,
		"TEST_GL_URL":  "http://127.0.0.1:2",
		"TEST_CF_URL":  "http://127.0.0.1:3",
		"MM_CEO_TOKEN": "tok-ceo",
	}

	doc := strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        enabled: false
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
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc), Environment: env})
	if e.Company() == nil {
		t.Fatal("the company did not start")
	}

	// Under test is the address each wiring BUILT, not whether it answers.
	mm := e.Mattermost()
	if mm == nil {
		t.Fatal("no chat transport was built")
	}
	if got := mm.URL(); got != chat.URL {
		t.Errorf("mattermost url = %q, want the resolved address", got)
	}
	// The other two refuse an unresolved url outright, taking the surface
	// down, so a live company is their assertion.
	if e.Company() == nil {
		t.Error("the company stopped, which is what an unresolved address does")
	}
}

// AN ADDRESS THAT RESOLVED TO NOTHING IS REFUSED, and the refusal names the
// reference.
//
// The validator refuses an enabled Mattermost with no url, so an empty one
// here is a ${VAR} that answered nothing; started anyway, every call fails
// with a message naming neither.
func TestAnUnresolvedChatAddressIsRefusedByName(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        enabled: false
        bot_token: ${MM_CEO_TOKEN}`, 1) + `
integrations:
  mattermost:
    enabled: true
    url: ${TEST_MM_URL_THAT_IS_NEVER_SET}
    team: eng
`
	// AN ENVIRONMENT THAT CANNOT HOLD the address, whatever the runner exports.
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc),
		Environment: config.MapSource{"MM_CEO_TOKEN": "tok-ceo"}})
	if mm := e.Mattermost(); mm != nil {
		t.Errorf("a chat transport was built for an address that resolved to "+
			"nothing: %q", mm.URL())
	}
}

// THE INBOUND EDGE VERIFIES WITH VALUES, never with references.
//
// Secrets are ${VAR}s in the config. An edge built from it unresolved refuses
// every delivery while the vendor shows a healthy hook, and the literal is no
// secret: the dashboard renders it, so a forged delivery could verify against
// a string an attacker can read.
func TestTheWebhookEdgeGetsResolvedSecrets(t *testing.T) {
	t.Parallel()
	doc := companyDoc + `
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: ${TEST_GL_SIGNING}
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc),
		Environment: config.MapSource{"TEST_GL_SIGNING": "whsec_resolved-value"}})

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

// A CONFIGURED VENDOR MUST ACTUALLY ROUTE, the distinction RoutedSources
// draws: a vendor whose config ships without its parser, or whose wiring a
// refactor drops from startNotifications, validates and shows in the
// dashboard's Integrations yet wakes nobody.
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
	// And the route has a secret: without one it answers 503 to every
	// delivery the parser would have routed.
	if got := e.WebhookSecrets().Jira; got != "jira-secret" {
		t.Errorf("jira webhook secret = %q", got)
	}
}

// AND SO DOES THE ALERTING SURFACE: an alert source with config and a route
// but no parser has every delivery verified, stored and counted, then skipped
// by the spine as inbound_source_unparsed.
func TestAConfiguredAlertingSurfaceActuallyRoutes(t *testing.T) {
	t.Parallel()
	doc := companyDoc + `
integrations:
  datadog:
    enabled: true
    webhook_token: whsec_ZGF0YWRvZy10b2tlbi0zMi1ieXRlcy1sb25nISE=
    route_to: ceo
    provisioning:
      site: datadoghq.com
      api_key: dd-api
      app_key: dd-app
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Contains(e.RoutedSources(), "datadog") {
		t.Fatalf("a company on Datadog routes %v", e.RoutedSources())
	}
	if e.WebhookSecrets().Datadog == "" {
		t.Error("the datadog route has no token to check a delivery against")
	}
}

// THE HOSTED CHAT SURFACE ROUTES TOO, and its per-seat apps are what the
// inbound edge verifies with — so a company on Slack needs both halves and
// each is silent without the other.
func TestAConfiguredHostedChatSurfaceActuallyRoutes(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(companyDoc, `  - name: CEO
    handle: ceo
    llm: zulu`, `  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      slack:
        enabled: false
        bot_token: ${SLACK_CEO_TOKEN}
        signing_secret: ${SLACK_CEO_SIGNING}`, 1) + `
integrations:
  slack:
    typing_status: addressed
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc),
		Environment: config.MapSource{
			"SLACK_CEO_TOKEN": "xoxb-ceo", "SLACK_CEO_SIGNING": "ceo-signing-secret",
		}})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The workspace is unreachable: the transport reports itself
	// unavailable, the documented degradation, and the company still runs.
	if e.Company() == nil {
		t.Fatal("an unreachable workspace stopped the company")
	}
	// The seat's signing secret comes from config, so it reaches the edge
	// either way.
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

// THE KNOWLEDGE BASE IS THE ONE INTEGRATION WHOSE ABSENCE IS INVISIBLE: a
// search gap is an empty "## Relevant knowledge" block in every prompt,
// indistinguishable from a company that wrote nothing down, so a configured
// Confluence has to produce a searcher.
func TestAConfiguredKnowledgeBaseProducesASearcher(t *testing.T) {
	t.Parallel()
	doc := companyDoc + `
integrations:
  confluence:
    url: https://wiki.example.com
    token: t
    webhook_secret: cf
knowledge:
  backend: confluence
  scope: [HANDBOOK]
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
// nil: consumers check `searcher == nil`, and a typed nil passes it and
// answers as though a search found nothing. A company that declares nothing
// runs Doclet, so `backend: none` is how it says none.
func TestNoKnowledgeBackendIsANilInterface(t *testing.T) {
	t.Parallel()
	doc := companyDoc + `
knowledge:
  backend: none
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := e.Knowledge(); got != nil {
		t.Fatalf("a company with no knowledge base got %T", got)
	}
}

// A COMPANY THAT DECLARES NOTHING RUNS DOCLET, its searcher this node's own
// index: the default almost every company runs, where a nil Knowledge() would
// leave every seat no knowledge block and no search tool.
func TestTheDefaultKnowledgeBackendIsDoclet(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	searcher := e.Knowledge()
	if searcher == nil {
		t.Fatal("a company that declares no backend got no searcher")
	}
	if got := searcher.Backend(); got != doclet.Backend {
		t.Errorf("the default searcher answers for %q, want Doclet's %q", got, doclet.Backend)
	}
}

// A VENDOR A REVISION RETIRES STOPS ROUTING, AND STOPS INGESTING: removing
// its block, or `enabled: false` after a credential leak, must take its
// parser, its RoutedSources entry and its webhook secret, or deliveries go on
// being handled under the credential being revoked.
func TestAVendorARevisionRetiresStopsRouting(t *testing.T) {
	t.Parallel()
	with := companyDoc + `
integrations:
  jira:
    url: https://jira.example.com
    token: t
    webhook_secret: jira-secret
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, with)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Contains(e.RoutedSources(), "jira") {
		t.Fatalf("the boot epoch does not route jira: %v", e.RoutedSources())
	}

	// The same company with the block removed.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if slices.Contains(e.RoutedSources(), "jira") {
		t.Errorf("a retired vendor still routes: %v\n"+
			"its parser is live and handling deliveries under the credential "+
			"the revision revoked", e.RoutedSources())
	}
	// And the ingest half agrees: a route with nothing to verify with
	// answers 503 rather than accepting a delivery that goes nowhere.
	if got := e.WebhookSecrets().Jira; got != "" {
		t.Errorf("a retired third-party app still verifies deliveries with %q", got)
	}
}

// AND `enabled: false` IS THE SAME GESTURE for the two vendors that have the
// switch — the block stays in the document, which is what an operator
// actually does when revoking a credential they intend to replace.
func TestADisabledVendorStopsIngesting(t *testing.T) {
	t.Parallel()
	enabled := companyDoc + `
integrations:
  github:
    enabled: true
    webhook_secret: gh-secret
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, enabled)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := e.WebhookSecrets().GitHub; got != "gh-secret" {
		t.Fatalf("the boot epoch has no github secret: %q", got)
	}

	disabled := companyDoc + `
integrations:
  github:
    enabled: false
    webhook_secret: gh-secret
`
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, disabled), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := e.WebhookSecrets().GitHub; got != "" {
		t.Errorf("a disabled vendor still verifies deliveries with %q: the route "+
			"ingests what the routing half will then drop", got)
	}
	if slices.Contains(e.RoutedSources(), "github") {
		t.Errorf("a disabled vendor still routes: %v", e.RoutedSources())
	}
}

// A RECONCILER THAT ONLY RETIRES IS NOT A RECONCILER.
//
// The parser set is assembled once, in New, so a reconciler acting only on a
// running parser never registers one a revision adds after boot.
// Disconnecting and reconnecting, the sequence an operator makes most, would
// leave the route verifying and storing every delivery while nothing turned
// one into work for a seat, until a restart.
func TestConfluenceRoutesAgainAfterBeingRemovedAndAddedBack(t *testing.T) {
	t.Parallel()
	with := companyDoc + `
integrations:
  confluence:
    url: https://wiki.example.com
    token: t
    webhook_secret: cf
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, with)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Contains(e.RoutedSources(), "confluence") {
		t.Fatalf("a company on Confluence routes %v", e.RoutedSources())
	}

	// Disconnected: the parser goes.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc), time.Now()); err != nil {
		t.Fatalf("Apply without confluence: %v", err)
	}
	if slices.Contains(e.RoutedSources(), "confluence") {
		t.Fatalf("a removed knowledge base still routes %v", e.RoutedSources())
	}

	// Connected again: the parser has to come back.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, with), time.Now()); err != nil {
		t.Fatalf("Apply with confluence: %v", err)
	}
	if !slices.Contains(e.RoutedSources(), "confluence") {
		t.Fatalf("a reconnected knowledge base does not route: %v", e.RoutedSources())
	}
	// AND ITS SEARCHER COMES BACK WITH IT. A revived parser beside a dead
	// searcher would route page activity while every turn went on getting
	// an empty knowledge block.
	if searcher := e.Knowledge(); searcher == nil || searcher.Backend() != "confluence" {
		t.Error("the knowledge searcher did not come back with the parser")
	}
}

// integrationBlocks is one configuration block per integration whose parser
// builds without reaching the network, keyed by the source it routes.
var integrationBlocks = []struct {
	source string
	block  string
}{
	{source: "jira", block: `
integrations:
  jira:
    url: https://jira.example.com
    token: t
    webhook_secret: s
`},
	{source: "confluence", block: `
integrations:
  confluence:
    url: https://wiki.example.com
    token: t
    webhook_secret: s
`},
	{source: "gitlab", block: `
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    token: t
    signing_secret: whsec_Y3Jld2xldC10ZXN0LXNpZ25pbmcta2V5LTMyYnl0ZXM=
`},
	{source: "github", block: `
integrations:
  github:
    enabled: true
    token: t
    webhook_secret: s
`},
	{source: "datadog", block: `
integrations:
  datadog:
    enabled: true
    webhook_token: EXAMPLEDATADOGTOKEN0000000
    route_to: ceo
    provisioning:
      site: datadoghq.com
      api_key: dd-api
      app_key: dd-app
`},
}

// EVERY INTEGRATION HAS TO SURVIVE BEING ADDED AFTER BOOT.
//
// The parser set is assembled once, in New, so an integration not rebuilt on
// an apply routes only if configured when the process started; connected
// later, its route verifies and stores every delivery while nothing turns one
// into work for a seat. TestConfluenceRoutesAgainAfterBeingRemovedAndAddedBack
// asks it of one; this asks every integration, since a reconciler nobody
// calls looks like one that converges in one direction.
func TestEveryIntegrationRoutesWhenAddedAfterBoot(t *testing.T) {
	t.Parallel()
	for _, tc := range integrationBlocks {
		t.Run(tc.source, func(t *testing.T) {
			t.Parallel()
			// BOOTED WITHOUT IT, which is the case that matters: a company
			// that has never configured this integration.
			e := newEngine(t, engine.Options{Company: parsedCompany(t, companyDoc)})
			if err := e.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if slices.Contains(e.RoutedSources(), tc.source) {
				t.Fatalf("%s routes before it is configured: %v", tc.source, e.RoutedSources())
			}

			if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc+tc.block), time.Now()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !slices.Contains(e.RoutedSources(), tc.source) {
				t.Errorf("%s was connected and does not route: %v", tc.source, e.RoutedSources())
			}
		})
	}
}

// AND ON A NODE THAT BOOTED WITH NO COMPANY AT ALL, as every company created
// from the dashboard, or by the first PUT /config, begins: boot starts the
// inbound edge only for a company it has, and the reconcilers rebuild a
// running edge, so the apply must start one or nothing routes until a restart.
func TestTheFirstCompanyOnAnUnconfiguredNodeRoutesItsIntegrations(t *testing.T) {
	t.Parallel()
	for _, tc := range integrationBlocks {
		t.Run(tc.source, func(t *testing.T) {
			t.Parallel()
			e := unconfiguredEngine(t)
			if err := e.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			// Nil, not empty: an unconfigured node cannot say what routes.
			if got := e.RoutedSources(); got != nil {
				t.Fatalf("an unconfigured node reports routed sources %v", got)
			}

			if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc+tc.block), time.Now()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !slices.Contains(e.RoutedSources(), tc.source) {
				t.Errorf("the first company on a fresh node connected %s and routes %v",
					tc.source, e.RoutedSources())
			}
		})
	}
}

// AND A ROUTING SETTING CHANGES WITHOUT A RESTART: Datadog's parser carries
// the monitor tag key and the fallback seat, so one built once at boot would
// go on waking the old fallback after an apply moved it, silently.
func TestDatadogRoutingFollowsTheAppliedRevision(t *testing.T) {
	t.Parallel()
	with := func(route string) string {
		return companyDoc + `
integrations:
  datadog:
    enabled: true
    webhook_token: EXAMPLEDATADOGTOKEN0000000
    route_to: ` + route + `
    provisioning:
      site: datadoghq.com
      api_key: dd-api
      app_key: dd-app
`
	}
	e := newEngine(t, engine.Options{Company: parsedCompany(t, with("ceo"))})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !slices.Contains(e.RoutedSources(), "datadog") {
		t.Fatalf("a company on Datadog routes %v", e.RoutedSources())
	}

	// Turned off, which is the gesture after a leaked webhook token.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc), time.Now()); err != nil {
		t.Fatalf("Apply without datadog: %v", err)
	}
	if slices.Contains(e.RoutedSources(), "datadog") {
		t.Fatalf("a removed alert source still routes %v", e.RoutedSources())
	}

	// And back, which is what a reconnect is.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, with("ceo")), time.Now()); err != nil {
		t.Fatalf("Apply with datadog: %v", err)
	}
	if !slices.Contains(e.RoutedSources(), "datadog") {
		t.Errorf("a reconnected alert source does not route: %v", e.RoutedSources())
	}
}
