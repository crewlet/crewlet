package engine

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
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
// connection.
//
// They went into the environment whatever the transport, and an http
// connection reads none, so the remote GitHub server a seat reaches with its
// own `Authorization: Bearer …` was dialled with the template's headers alone
// — for every seat. The stdio neighbour is here to hold the other half: its
// credential stays in the environment, and an http child carries none.
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
    transport: http
    shared: false
    url: "https://mcp.example.com/"
    headers:
      X-Workspace: "acme"
  - name: tracker
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

// provisioningDoc is a company whose agents are each on at four vendors, with
// the integrations migrated onto per-seat access and the provisioning blocks
// as provisioned says. Off, nothing asks a pass to mint: the integrations hold
// no authority to create an agent's account, so the bots, accounts and keys
// are the operator's, made by hand, and Datadog, which refuses an enabled
// block with no provisioning, is switched off.
//
// Each seat's tool names its own sealed names ([envref.SealedName]), which is
// what the engine seals under where it provisions, and what an operator's
// external credential may be handed in under where it does not.
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
// token, and that is what a seat's child waits for. A company that connects
// the same vendors with no authority to create an account made its accounts
// by hand: nothing it runs will ever mint a variable, and a child that read
// one of them as on its way would wait for ever, reported as waiting rather
// than as the missing secret it is.
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
// a seat's child would be spawned with that credential empty again.
//
// The plan is how the engine tells a credential on its way from a secret
// somebody forgot, and only a pass can declare it. So every pass this build
// runs is either a seat minter or named here with why it mints none: a new
// pass that mints and says nothing fails this rather than quietly bringing
// back an empty-token spawn on every first boot.
func TestEveryPassThatMintsASeatsCredentialDeclaresItsPlan(t *testing.T) {
	t.Parallel()
	mintsNoSeatCredential := map[integration.Kind]string{
		// An agent's GitHub App is created by a person's own gesture,
		// which seals its key and writes the pointer to it together, and
		// the token it acts with is issued at use, never sealed by a pass
		// (githubtoken.go), so no plan is waited on.
		integration.KindGitHub: "no pass mints a seat's GitHub credential",
		// Both read an agent's Atlassian account, which the Atlassian
		// pass mints.
		integration.KindJira:       "reads the account the atlassian pass mints",
		integration.KindConfluence: "reads the account the atlassian pass mints",
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

// heldBackCompany is a company that provisions its agents' Atlassian accounts,
// whose per-role servers point at binaries that do not exist — so a start
// that is attempted at all comes back failed, and one that is not leaves
// nothing behind. The CEO's Atlassian credentials are the pass's to mint; the
// CTO's tracker token is a variable nothing mints.
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
    shared: false
    command: "/nonexistent/definitely-not-an-atlassian-server"
  - name: tracker
    shared: false
    command: "/nonexistent/definitely-not-a-tracker-server"
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
// The claim used to spawn such a child with the credential empty, and a
// vendor's server refuses that: every first boot under provisioning logged
// each seat's server failed and retried it on the restart schedule for a
// credential the engine knew it had not minted. Here nothing may reach a
// bridge before the seal, the heartbeat must count the CEO's child as waiting
// — not as failed, which it is not — with the sentence naming what it waits
// for, and the CTO's as failed with the variable to set. Neither arms a
// retry, since nothing changes either until the snapshot does; and the pass
// an apply runs once the CEO's credentials are sealed starts the child.
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

	// THE SEAL, as the apply after it sees it: the snapshot holds the
	// credentials, and the apply asks the seat's runner for a pass.
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
// by the rule a seat's own child is started by.
//
// The box gets the servers role.sandbox.mcp.servers names, with the seat's
// credentials resolved into them. One whose credential resolved to nothing
// used to be rendered anyway, under a warning that it "will not
// authenticate": the agent then spent a round finding that out, or the server
// acted as nobody. The neighbour whose credential resolves is still offered,
// with it.
func TestACodingAgentIsNotOfferedAServerWhoseSeatCredentialIsMissing(t *testing.T) {
	t.Parallel()
	company := heldBackCompany(t)
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
// engine's own child of the same server does: the server's env, its headers
// and its url, beside the seat's credentials.
//
// They were handed to the box verbatim and written into the harness's
// config as `${VAR}` text. OpenCode reads `{env:VAR}`, so a shared key reached
// its server as the literal, and Claude Code expands one from the run env,
// which holds no company secret unless a seat repeats it there.
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
    command: "search-server"
    env:
      SEARCH_KEY: "${SEARCH_KEY}"
  - name: docs
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
