package engine

import (
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// specCompany parses a company document into the epoch a seat's specs are
// built from.
func specCompany(t *testing.T, doc string) *Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return company
}

// A SEAT'S CREDENTIALS REACH A PER-ROLE SERVER THROUGH WHAT ITS TRANSPORT
// AUTHENTICATES WITH: the environment of a stdio child, the headers of an http
// connection, which reads no environment (an `Authorization: Bearer …` there
// would be dropped, dialling as the template alone for every seat). The stdio
// neighbour holds the other half: its credential stays in the environment,
// and the http child carries none.
func TestASeatsCredentialsReachAnHTTPServerAsHeaders(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: github
    tools: {default: allow}
    transport: http
    shared: false
    url: "https://mcp.example.com/"
    headers:
      X-Workspace: "acme"
  - name: tracker
    tools: {default: allow}
    shared: false
    command: "tracker-server"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      github:
        Authorization: "Bearer ${CEO_GITHUB_TOKEN}"
      tracker:
        SEAT_TOKEN: "${CEO_TRACKER_TOKEN}"
`)
	env := config.NewResolver(config.MapSource{
		"CEO_GITHUB_TOKEN": "ghp-ceo", "CEO_TRACKER_TOKEN": "tracker-ceo",
	})
	specs, outcomes := seatSpecs(company, company.Org.AgentSeatByHandle("ceo"), env, nil)
	if len(outcomes) != 0 || len(specs) != 2 {
		t.Fatalf("specs = %+v, outcomes = %+v: want both servers launchable", specs, outcomes)
	}
	byName := map[string]int{}
	for i, spec := range specs {
		byName[spec.Server()] = i
	}

	github := specs[byName["github"]]
	wantHeaders := map[string]string{"X-Workspace": "acme", "Authorization": "Bearer ghp-ceo"}
	if !maps.Equal(github.Headers, wantHeaders) {
		t.Errorf("the http child's headers are %v, want the template's with the seat's "+
			"credential over them, %v", github.Headers, wantHeaders)
	}
	if len(github.Env) != 0 {
		t.Errorf("the http child carries an environment it never reads: %v", github.Env)
	}

	tracker := specs[byName["tracker"]]
	if got := tracker.Env["SEAT_TOKEN"]; got != "tracker-ceo" {
		t.Errorf("the stdio child's SEAT_TOKEN is %q, want the seat's credential", got)
	}
	if len(tracker.Headers) != 0 {
		t.Errorf("the stdio child carries headers no pipe sends: %v", tracker.Headers)
	}
}

// provisioningDoc is a company whose agents are each on at four vendors under
// per-seat access, with the provisioning blocks iff provisioned. Off, no
// integration may create an agent's account, so the accounts are made by hand
// and Datadog (which refuses an enabled block without provisioning) is off.
//
// Each seat's tool names its own sealed names ([envref.SealedName]): what the
// engine seals under where it provisions, and what an operator hands an
// external credential in under where it does not.
func provisioningDoc(provisioned bool) string {
	atlassian, gitlab, mattermost := "", "", ""
	datadogEnabled := "false"
	if provisioned {
		datadogEnabled = "true"
		atlassian = `
  atlassian:
    org_id: "${ATLASSIAN_ORG_ID}"
    api_key: "${ATLASSIAN_ORG_KEY}"`
		gitlab = `
    provisioning:
      group: acme
      admin_token: "${GITLAB_ADMIN_TOKEN}"`
		mattermost = `
    provisioning:
      admin_token: "${MATTERMOST_ADMIN_TOKEN}"`
	}
	return `
name: Acme
seat_access_migrated: [atlassian, datadog, gitlab, mattermost]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:` + atlassian + `
  jira:
    url: https://acme.atlassian.net
    token: "${JIRA_ORG_TOKEN}"
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: "whsec_Y3Jld2xldC10ZXN0LXNpZ25pbmcta2V5LTMyYnl0ZXM="` + gitlab + `
  mattermost:
    enabled: true
    url: https://chat.example.com
    team: acme` + mattermost + `
  datadog:
    enabled: ` + datadogEnabled + `
    webhook_token: EXAMPLEDATADOGTOKEN0000000
    route_to: ops
    provisioning:
      site: datadoghq.com
      api_key: "${DD_API_KEY}"
      app_key: "${DD_APP_KEY}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      jira: {enabled: true, project_role: "10101"}
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${ATLASSIAN_EMAIL_CEO}"
        JIRA_API_TOKEN: "${ATLASSIAN_TOKEN_CEO}"
  - name: SWE
    handle: swe
    llm: zulu
    integrations:
      gitlab: {enabled: true, access_level: 30}
    mcp_env:
      gitlab:
        GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"
  - name: PM
    handle: pm
    llm: zulu
    integrations:
      mattermost:
        enabled: true
        roles: [system_user]
        bot_token: "${MATTERMOST_BOT_TOKEN_PM}"
  - name: Ops
    handle: ops
    llm: zulu
    integrations:
      datadog: {enabled: true, role: "dd-role-ops"}
    mcp_env:
      datadog:
        DD_APP_KEY: "${DATADOG_APP_KEY_OPS}"
`
}

// WHAT A PASS WILL MINT IS READ FROM THE PLAN IT MINTS BY, and from a company
// that gives it the authority to.
//
// The access sync seals each seat that is on under the seat's own names
// ([Engine.seatAccessPlan]), Atlassian's assigned address as well as the
// token, and a seat's child waits for those. Without the authority to create
// accounts nothing mints a variable, and a child reading one as on its way
// would wait forever, reported as waiting instead of as a missing secret.
func TestWhatAPassMintsIsReadFromItsOwnPlan(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	provisioned := specCompany(t, provisioningDoc(true))
	want := map[string]map[string]integration.Kind{
		"ceo": {
			"ATLASSIAN_EMAIL_CEO": integration.KindAtlassian,
			"ATLASSIAN_TOKEN_CEO": integration.KindAtlassian,
		},
		"swe": {"GITLAB_TOKEN_SWE": integration.KindGitLab},
		"pm":  {"MATTERMOST_BOT_TOKEN_PM": integration.KindMattermost},
		"ops": {"DATADOG_APP_KEY_OPS": integration.KindDatadog},
	}
	for handle, minted := range want {
		if got := e.mintedFor(provisioned, handle); !maps.Equal(got, minted) {
			t.Errorf("under provisioning, %s's minted variables are %v, want %v", handle, got, minted)
		}
	}

	byHand := specCompany(t, provisioningDoc(false))
	for handle := range want {
		if got := e.mintedFor(byHand, handle); len(got) != 0 {
			t.Errorf("with every account made by hand, %s's %v read as minted by a pass "+
				"that will never run", handle, got)
		}
	}
}

// EVERY PASS THAT MINTS A SEAT'S CREDENTIAL DECLARES THE PLAN IT MINTS BY, or
// a seat's child would be spawned with that credential empty.
//
// The plan is how the engine tells a credential on its way from a forgotten
// secret, and only a pass can declare it. So every pass is a seatMinter or
// named here with why it mints none, and a new minting pass that declares
// nothing fails this.
func TestEveryPassThatMintsASeatsCredentialDeclaresItsPlan(t *testing.T) {
	t.Parallel()
	mintsNoSeatCredential := map[integration.Kind]string{
		// An agent's GitHub App is created by a person's gesture, and its
		// token is issued at use (githubtoken.go), never sealed by a pass.
		integration.KindGitHub: "no pass mints a seat's GitHub credential",
		// Issued at use by impersonating its service account.
		integration.KindGoogleCloud: "no pass mints a seat's Google Cloud credential",
		// Both read the agent's Atlassian account.
		integration.KindJira:       "reads the account the atlassian pass mints",
		integration.KindConfluence: "reads the account the atlassian pass mints",
		// Issued at use through the broker.
		integration.KindAWS: "no pass mints a seat's AWS credential",
		// Signed at use.
		integration.KindSSH: "no pass mints a seat's SSH credential",
		// The system user's tokens stay in the engine, which makes every call (ADR-0170).
		integration.KindMetaAds: "no seat holds a Meta Ads token",
		// The team's keys stay in the engine, which makes every call (ADR-0170).
		integration.KindAppStoreConnect: "no seat holds an App Store Connect key",
		// The API users' keys stay in the engine, which makes every call (ADR-0170).
		integration.KindAppleAds:        "no seat holds an Apple Ads key",
		integration.KindGoogleAnalytics: "no pass mints a seat's Google Analytics credential: its tokens are minted at use",
		integration.KindAdMob:           "no seat holds an AdMob credential: every call goes as the company's grant",
		integration.KindAdSense:         "no seat holds an AdSense credential: every call goes as the company's grant",
		integration.KindGoogleAds:       "no pass mints a seat's Google Ads credential: its tokens are minted at use",
		integration.KindSearchConsole:   "no pass mints a seat's Search Console credential: its tokens are minted at use",
		integration.KindGooglePlay:      "no pass mints a seat's Google Play credential: its tokens are minted at use",
	}
	e := &Engine{}
	for _, pass := range e.setupPasses() {
		_, mints := pass.(seatMinter)
		why, listed := mintsNoSeatCredential[pass.Kind()]
		switch {
		case mints && listed:
			t.Errorf("%s declares a seat plan and is listed as minting none (%s)", pass.Kind(), why)
		case !mints && !listed:
			t.Errorf("%s declares no seat plan and is not listed as minting none: if it "+
				"mints a seat's credential, a seat's child is spawned without it", pass.Kind())
		}
	}
}

// heldBackCompany provisions its agents' Atlassian accounts, and its per-role
// servers name missing binaries, so an attempted start comes back failed and
// an unattempted one leaves nothing. The CEO's Atlassian credentials are the
// pass's to mint; the CTO's tracker token is a variable nothing mints.
func heldBackCompany(t *testing.T) *Company {
	t.Helper()
	return specCompany(t, `
name: Acme
seat_access_migrated: [atlassian]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  atlassian:
    org_id: "${ATLASSIAN_ORG_ID}"
    api_key: "${ATLASSIAN_ORG_KEY}"
  jira:
    url: https://acme.atlassian.net
    token: "${JIRA_ORG_TOKEN}"
mcp_servers:
  - name: atlassian
    tools: {default: allow}
    shared: false
    command: "/nonexistent/definitely-not-an-atlassian-server"
    access: {seats: [ceo]}
  - name: tracker
    tools: {default: allow}
    shared: false
    command: "/nonexistent/definitely-not-a-tracker-server"
    access: {seats: [cto]}
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      jira: {enabled: true, project_role: "10101"}
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${ATLASSIAN_EMAIL_CEO}"
        JIRA_API_TOKEN: "${ATLASSIAN_TOKEN_CEO}"
  - name: CTO
    handle: cto
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "${CTO_TRACKER_TOKEN}"
`)
}

// A SEAT'S CHILD WHOSE CREDENTIAL IS NOT THERE IS NOT STARTED: held back as
// waiting where a provisioning pass will mint it, failed where nothing will,
// retried by neither, and started by the first pass after the snapshot holds
// it.
//
// A vendor's server refuses an empty credential, so nothing may reach a bridge
// before the seal. The status counts the CEO's child as waiting (not failed),
// naming what it waits for, and the CTO's as failed, naming the variable to
// set. Neither arms a retry: nothing changes until the snapshot does.
func TestASeatChildWithoutItsCredentialIsHeldBackNotStarted(t *testing.T) {
	t.Parallel()
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	company := heldBackCompany(t)
	e.epoch.current.Store(company)
	always := func() bool { return true }
	ceo := heldSeatChildren(t, e, "ceo", always)
	cto := heldSeatChildren(t, e, "cto", always)

	e.convergeSeat(t.Context(), ceo, true)
	e.convergeSeat(t.Context(), cto, true)
	for _, s := range []*seatChildren{ceo, cto} {
		if servers := s.bridge.Servers(); len(servers) != 0 {
			t.Errorf("%s: a child reached the bridge with its credential missing: %v", s.handle, servers)
		}
		s.mu.Lock()
		armed := s.retry != nil
		s.mu.Unlock()
		if armed {
			t.Errorf("%s: a child held back for its credential armed a retry, which can "+
				"start nothing until the snapshot changes", s.handle)
		}
	}

	ceoID, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("ceo"))
	ctoID, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("cto"))
	rows := map[string]coord.MCPServerStatus{}
	for _, row := range e.mcpStatus(t.Context()) {
		rows[row.Server] = row
	}
	atlassianRow := rows["atlassian"]
	if atlassianRow.Waiting != 1 || atlassianRow.Failed != 0 || atlassianRow.Started != 0 ||
		atlassianRow.WaitSeat != ceoID || atlassianRow.Error != "" {
		t.Errorf("atlassian row = %+v: want the CEO's child waiting, and not failed", atlassianRow)
	}
	for _, said := range []string{"${ATLASSIAN_EMAIL_CEO}", "${ATLASSIAN_TOKEN_CEO}",
		"atlassian integration", "starts once the seal reaches this node"} {
		if !strings.Contains(atlassianRow.Wait, said) {
			t.Errorf("the waiting child's sentence %q does not say %q", atlassianRow.Wait, said)
		}
	}
	trackerRow := rows["tracker"]
	if trackerRow.Failed != 1 || trackerRow.Waiting != 0 || trackerRow.ErrorSeat != ctoID {
		t.Errorf("tracker row = %+v: want the CTO's child failed, its credential being "+
			"one nothing mints", trackerRow)
	}
	for _, said := range []string{"${CTO_TRACKER_TOKEN} is not set", "nothing this engine runs mints it",
		"crewlet secrets set", "restart"} {
		if !strings.Contains(trackerRow.Error, said) {
			t.Errorf("the unset credential's sentence %q does not say %q", trackerRow.Error, said)
		}
	}

	// The seal, as the next apply sees it: the snapshot holds the credentials
	// and the apply asks the seat's runner for a pass.
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{
		"ATLASSIAN_EMAIL_CEO": "agent-ceo@example.com", "ATLASSIAN_TOKEN_CEO": "ceo-minted",
	})
	e.secretsMu.Unlock()
	e.convergeSeat(t.Context(), ceo, true)
	outcomes := seatOutcomes(e, "ceo")
	var missing *credentialMissing
	if len(outcomes) != 1 || outcomes[0].err == nil || errors.As(outcomes[0].err, &missing) {
		t.Fatalf("outcomes once the credentials are sealed = %+v: want the child's start "+
			"attempted (and failed, its binary being absent)", outcomes)
	}
}

// A HARNESS IS NOT OFFERED A SERVER WHOSE SEAT CREDENTIALS ARE MISSING,
// by the rule a seat's own child is started by: offered, the agent would spend
// a round finding out, or the server would act as nobody. The neighbour whose
// credential resolves is still offered, with it.
func TestAHarnessIsNotOfferedAServerWhoseSeatCredentialIsMissing(t *testing.T) {
	t.Parallel()
	company := heldBackCompany(t)
	// Granted to every agent, so the CEO's box may hold it.
	configured(t, company, "tracker").Access = nil
	seat := company.Org.AgentSeatByHandle("ceo")
	seat.MCPEnv["tracker"] = map[string]string{"SEAT_TOKEN": "${CEO_TRACKER_TOKEN}"}
	env := config.NewResolver(config.MapSource{"CEO_TRACKER_TOKEN": "tracker-ceo"})
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"atlassian", "tracker"}}}

	got, withheld := sandboxMCP(env, company, seat, gate, &seatMint{kinds: (&Engine{}).mintedFor(company, "ceo")})
	if _, offered := got["atlassian"]; offered {
		t.Error("the box was offered the atlassian server with the seat's credentials missing")
	}
	if len(withheld) != 1 || withheld[0].server != "atlassian" || !withheld[0].waiting() {
		t.Errorf("withheld = %+v: want the atlassian server, waiting for the pass that mints "+
			"its credentials, handed back for the launch to say", withheld)
	}
	tracker, offered := got["tracker"]
	if !offered {
		t.Fatalf("the box was not offered the tracker, whose credential resolves: %v", got)
	}
	if tracker.Env["SEAT_TOKEN"] != "tracker-ceo" {
		t.Errorf("the tracker was offered with SEAT_TOKEN %q, want the seat's credential",
			tracker.Env["SEAT_TOKEN"])
	}
}

// A BOX'S TOOL SERVER READS ITS OWN REFERENCES THROUGH THE ENGINE, as the
// engine's own child of the same server does: its env, headers and url, beside
// the seat's credentials. Left as `${VAR}` text, OpenCode (which reads
// `{env:VAR}`) sends the literal, and Claude Code expands it from the run env,
// which holds no company secret.
func TestABoxsToolServerReadsItsOwnReferencesThroughTheEngine(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: search
    tools: {default: allow}
    command: "search-server"
    env:
      SEARCH_KEY: "${SEARCH_KEY}"
  - name: docs
    tools: {default: allow}
    transport: http
    shared: false
    url: "https://${DOCS_HOST}/mcp"
    headers:
      X-Workspace: "${DOCS_WORKSPACE}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      docs:
        Authorization: "Bearer ${CEO_DOCS_TOKEN}"
`)
	seat := company.Org.AgentSeatByHandle("ceo")
	env := config.NewResolver(config.MapSource{
		"SEARCH_KEY": "search-secret", "DOCS_HOST": "docs.example.com",
		"DOCS_WORKSPACE": "acme", "CEO_DOCS_TOKEN": "docs-ceo",
	})
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"search", "docs"}}}

	got, withheld := sandboxMCP(env, company, seat, gate, nil)
	if len(withheld) != 0 {
		t.Fatalf("withheld = %+v: every reference resolves", withheld)
	}
	if key := got["search"].Env["SEARCH_KEY"]; key != "search-secret" {
		t.Errorf("the box's search server is given SEARCH_KEY %q, want the stored value", key)
	}
	docs := got["docs"]
	if docs.URL != "https://docs.example.com/mcp" {
		t.Errorf("the box's docs server is dialled at %q, want its url resolved", docs.URL)
	}
	want := map[string]string{"X-Workspace": "acme", "Authorization": "Bearer docs-ceo"}
	if !maps.Equal(docs.Headers, want) {
		t.Errorf("the box's docs server sends %v, want %v", docs.Headers, want)
	}
}

// A BOX'S OWN SERVER CARRIES WHAT THE COMPANY SAYS IT IS FOR, which the run's
// environment brief lists beside its name: the box reaches it past the bridge,
// whose endpoint says a bridged server's description as its MCP instructions,
// so nothing else tells the model which of the box's servers to look in. One
// whose entry says nothing carries nothing.
//
// Mutation: leave Description out of sandboxMCP's server, and this goes red.
func TestABoxsOwnServerCarriesWhatTheCompanySaysItIsFor(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: handbook
    description: Searches the engineering handbook and the runbooks
    tools: {default: allow}
    command: "handbook-server"
  - name: tracker
    tools: {default: allow}
    transport: http
    url: "https://tracker.example.com/mcp"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`)
	seat := company.Org.AgentSeatByHandle("ceo")
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"handbook", "tracker"}}}

	got, withheld := sandboxMCP(config.NewResolver(nil), company, seat, gate, nil)
	if len(withheld) != 0 || len(got) != 2 {
		t.Fatalf("the box was given %+v and %+v withheld: want both servers, neither reading a value",
			got, withheld)
	}
	if said := got["handbook"].Description; said != "Searches the engineering handbook and the runbooks" {
		t.Errorf("the box's handbook server says it is for %q, want its entry's description", said)
	}
	if said := got["tracker"].Description; said != "" {
		t.Errorf("the box's tracker server says it is for %q, and its entry says nothing", said)
	}
}

// A SERVER THE BRIDGE SERVES CARRIES NO WORDS FOR THE RUN'S BRIEF, though the
// company says what it is for: the bridge's endpoint for it says that as the
// server's MCP instructions, which the harness hands its model, so a
// description on its entry would have the brief say it a second time.
//
// Mutation: give each of bridgeServers' servers what its tools carry of it
// (their ServerDescription, which its endpoint says) as its description, and
// this goes red.
func TestAServerTheBridgeServesCarriesNoWordsForTheBrief(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if err := reg.Register(waitedTool{name: "create_issue"}, tools.Origin("github")); err != nil {
		t.Fatalf("Register(create_issue): %v", err)
	}
	reg.DescribeServers(map[string]string{"github": "The company's code host."})
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{"create_issue"}).
		ForTurn(&turnctx.Turn{RunID: "run-1", Seat: seat})
	described := false
	for _, entry := range surface.Universe().Entries() {
		if server, fromMCP := entry.FromMCP(); fromMCP && server == "github" && entry.ServerDescription != "" {
			described = true
		}
	}
	if !described {
		t.Fatal("github's tools carry nothing of what it is for, so the case holds nothing")
	}

	servers := bridgeServers("https://engine.example.com/mcp/job-token", &mcpbridge.Session{Surface: surface},
		time.Minute)
	if _, served := servers["github"]; !served {
		t.Fatal("the bridge does not serve github under its own name, so the case holds nothing")
	}
	for name, server := range servers {
		if server.Description != "" {
			t.Errorf("the bridge's %s server carries %q for the run's brief, which its endpoint "+
				"says already", name, server.Description)
		}
	}
}
