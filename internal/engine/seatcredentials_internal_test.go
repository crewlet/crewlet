package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
)

// The rule a seat's child is started by when its own credentials do not
// resolve (seatcredentials.go): what tells a credential on its way from one
// nobody will supply, what tells a wait from a wait that never ends, and what
// starts the child once the credential is there.

// mixedDoc is a company that provisions its agents' Atlassian accounts, whose
// per-role servers point at binaries that do not exist, and whose seats' blocks
// mix what the pass mints with what nothing does: the CEO's site address is a
// variable nobody sets, and the CTO names one token under two keys. Its
// servers' own env passes a host setting through by name that no host here
// sets, the guide's own example of a reference that may resolve to nothing.
const mixedDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-spec-key"]
integrations:
  atlassian:
    org_id: "${ATLASSIAN_ORG_ID}"
    api_key: "${ATLASSIAN_ORG_KEY}"
mcp_servers:
  - name: atlassian
    shared: false
    command: "/nonexistent/definitely-not-an-atlassian-server"
    env:
      NPM_CONFIG_REGISTRY: "${HOST_SETTING_NOBODY_SETS}"
  - name: notes
    shared: true
    command: "/nonexistent/definitely-not-a-notes-server"
    env:
      NPM_CONFIG_REGISTRY: "${HOST_SETTING_NOBODY_SETS}"
  - name: tracker
    shared: false
    command: "/nonexistent/definitely-not-a-tracker-server"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${CEO_ATLASSIAN_EMAIL}"
        JIRA_API_TOKEN: "${CEO_ATLASSIAN_TOKEN}"
        JIRA_URL: "${CEO_JIRA_SITE}"
  - name: CTO
    handle: cto
    llm: zulu
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${CTO_ATLASSIAN_EMAIL}"
        JIRA_API_TOKEN: "${CTO_ATLASSIAN_TOKEN}"
      tracker:
        SEAT_TOKEN: "${CTO_TRACKER_TOKEN}"
        OTHER_TOKEN: "${CTO_TRACKER_TOKEN}"
`

// missingFor is what of one seat's block for a server resolves to nothing
// under env, judged as a pass judges it before reading any integration.
func missingFor(t *testing.T, c *Company, handle, server string, env *config.Resolver) *seatCredentialMissing {
	t.Helper()
	seat := c.Org.AgentSeatByHandle(handle)
	return seatCredentialsMissing(handle, server, mcp.InstanceName(server, seat.Name),
		seat.MCPEnv[server], env, (&Engine{}).mintedFor(c, handle))
}

// A BLOCK ONE OF WHOSE VARIABLES A PASS MINTS AND ANOTHER NOTHING DOES IS A
// FAILURE, and its sentence promises no start.
//
// The pass mints the CEO's Atlassian address and token, and nothing mints the
// site address beside them: waiting for the pass would wait for ever, since
// the pass never mints the other, behind a screen that says nothing is wrong.
func TestABlockMixingAMintedAndAnUnsetVariableIsAFailureNotAWait(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	missing := missingFor(t, c, "ceo", "atlassian", config.NewResolver(config.MapSource{}))
	if missing == nil {
		t.Fatal("nothing reported missing")
	}
	if missing.waiting() {
		t.Errorf("a block with ${CEO_JIRA_SITE} unset reads as waiting: %s", missing.Error())
	}
	sentence := missing.Error()
	if strings.Contains(sentence, "starts once") {
		t.Errorf("a failure's sentence promises a start: %s", sentence)
	}
	for _, said := range []string{"${CEO_JIRA_SITE} is not set", "nothing this engine runs mints it",
		"waiting for the atlassian integration to mint ${CEO_ATLASSIAN_EMAIL} and ${CEO_ATLASSIAN_TOKEN}"} {
		if !strings.Contains(sentence, said) {
			t.Errorf("the sentence %q does not say %q", sentence, said)
		}
	}
}

// A VARIABLE TWO KEYS POINT AT IS NAMED ONCE, because a sentence telling an
// operator to set it twice reads as two different things to set.
func TestOneVariableTwoKeysPointAtIsNamedOnce(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	missing := missingFor(t, c, "cto", "tracker", config.NewResolver(config.MapSource{}))
	if missing == nil {
		t.Fatal("nothing reported missing")
	}
	if n := strings.Count(missing.Error(), "${CTO_TRACKER_TOKEN}"); n != 1 {
		t.Errorf("the sentence names ${CTO_TRACKER_TOKEN} %d times: %s", n, missing.Error())
	}
}

// A VARIABLE SET TO NOTHING IS TOLD FROM ONE SET NOWHERE, because the remedies
// differ: "export it" sends an operator to do what they already did.
func TestAVariableSetToNothingIsToldFromOneSetNowhere(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	empty := missingFor(t, c, "cto", "tracker",
		config.NewResolver(config.MapSource{"CTO_TRACKER_TOKEN": ""}))
	unset := missingFor(t, c, "cto", "tracker", config.NewResolver(config.MapSource{}))
	if empty == nil || unset == nil {
		t.Fatalf("missing = %v, %v: want both reported", empty, unset)
	}
	if got := empty.Error(); !strings.Contains(got, "is set to an empty value") ||
		strings.Contains(got, "is not set") {
		t.Errorf("a variable exported empty reads %q, want it called empty and not unset", got)
	}
	if got := unset.Error(); !strings.Contains(got, "is not set") ||
		strings.Contains(got, "empty value") {
		t.Errorf("a variable set nowhere reads %q, want it called unset and not empty", got)
	}
	if empty.waiting() || unset.waiting() {
		t.Error("a variable nothing mints reads as waiting")
	}
}

// THE RULE STOPS AT THE SEAT'S OWN BLOCK: a host setting a server's own env
// passes through by name, unset on most hosts, holds back neither a seat's
// child nor a shared server.
//
// The guide passes NPM_CONFIG_REGISTRY that way and npm reads it empty as
// unset; a gate that reached the template's env or a shared server would
// refuse those servers on every host that does not set it.
func TestAnUnsetHostSettingInAServersOwnEnvHoldsNothingBack(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	env := config.NewResolver(config.MapSource{
		"CEO_ATLASSIAN_EMAIL": "agent-ceo@example.com", "CEO_ATLASSIAN_TOKEN": "ceo-minted",
		"CEO_JIRA_SITE": "https://acme.example.com",
	})
	specs, outcomes := seatSpecs(c, c.Org.AgentSeatByHandle("ceo"), env, nil)
	if len(specs) != 1 || len(outcomes) != 0 {
		t.Errorf("per-role: %d spec(s), outcomes %+v: an unset host setting in the "+
			"template held the seat's child back", len(specs), outcomes)
	}
	if len(specs) == 1 {
		if got, present := specs[0].Env["NPM_CONFIG_REGISTRY"]; !present || got != "" {
			t.Errorf("the host setting reached the child as %q (present %t), want it "+
				"passed on empty", got, present)
		}
	}
	for _, server := range c.Config.MCPServers {
		if server.Name != "notes" {
			continue
		}
		if _, err := serverSpec(server, server.Name, env, nil); err != nil {
			t.Errorf("shared: an unset host setting refused the server: %v", err)
		}
	}
}

// statedAs is the fleet's record of an integration after one pass that found
// findings, as the reconcile loop writes it.
func statedAs(kind integration.Kind, findings ...integration.Finding) integration.State {
	state, _ := integration.Observe(integration.State{Kind: kind}, kind, findings, nil,
		time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	return state
}

// WHETHER A PASS WILL MINT A SEAT'S CREDENTIAL IS READ OFF THE PASS'S OWN LAST
// CONCLUSION, by the vocabulary it reported in.
//
// A child held back for a credential the pass cannot mint until somebody acts
// waited for ever, reported as nothing being wrong, while the integration's own
// card said the opposite. Each case is a record a real pass writes.
func TestAMintIsStoppedOnlyWhereTheIntegrationsLastPassSaysAPersonMustAct(t *testing.T) {
	t.Parallel()
	kind := integration.KindAtlassian
	disconnecting := statedAs(kind)
	disconnecting.Disconnecting = true
	for name, tc := range map[string]struct {
		state   integration.State
		stopped bool
		says    string
	}{
		"no pass has run, which is every first boot": {state: integration.State{Kind: kind}},
		"the pass found everything fine":             {state: statedAs(kind)},
		"its own credential does not resolve": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingCredentialMissing,
				Detail: "the Atlassian organization id and its API key did not both resolve"}),
			stopped: true, says: "organization id and its API key",
		},
		"an administrator has an app to approve": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingApprovalRequired,
				Subject: "ServiceSettings.EnableBotAccountCreation", Detail: "bot creation is off"}),
			stopped: true, says: "bot creation is off",
		},
		"this seat's account was switched off by somebody else": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingIdentityFailed,
				Subject: "ceo", Detail: "ceo's bot is deactivated"}),
			stopped: true, says: "ceo's bot is deactivated",
		},
		"another seat's account failed": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingIdentityFailed,
				Subject: "cto", Detail: "cto's account could not be made"}),
		},
		"this seat's grant is landing": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingGrantPending,
				Subject: "ceo"}),
		},
		"a webhook cannot be registered, after the accounts were made": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingIngressBlocked,
				Detail: "no public base URL"}),
		},
		"a credential for something else is missing while this seat's account is being made": {
			state: statedAs(kind,
				integration.Finding{Kind: integration.FindingCredentialMissing,
					Subject: "integrations.datadog.webhook_token", Detail: "no webhook token"},
				integration.Finding{Kind: integration.FindingIdentityMissing, Subject: "ceo"}),
		},
		"one finding about this seat is the engine's and another a person's": {
			state: statedAs(kind,
				integration.Finding{Kind: integration.FindingIdentityMissing, Subject: "ceo"},
				integration.Finding{Kind: integration.FindingIdentityFailed, Subject: "ceo",
					Detail: "the account was refused"}),
			stopped: true, says: "the account was refused",
		},
		"the integration is being disconnected": {
			state: disconnecting, stopped: true, says: "being disconnected",
		},
	} {
		why, stopped := mintStopped(tc.state, "ceo")
		if stopped != tc.stopped {
			t.Errorf("%s: stopped = %t (%q), want %t", name, stopped, why, tc.stopped)
			continue
		}
		if tc.stopped && !strings.Contains(why, tc.says) {
			t.Errorf("%s: the reason %q does not say %q", name, why, tc.says)
		}
	}
}

// A CHILD WAITING FOR A PASS THAT CANNOT MINT IS REPORTED FAILED, from the beat
// after the integration says so, and waiting again once it no longer does.
//
// The claim's pass runs before the integration's first, so all it can say is
// waiting; only the heartbeat sees the integration's later report. Read once at
// the claim, a company whose Atlassian organization key nobody set showed every
// seat's server as waiting for credentials for ever, in a neutral tone, beside
// an integration card reporting the failure.
func TestAWaitingChildIsReportedFailedWhileThePassThatMintsItWaitsOnAPerson(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), backends: &Backends{Fleet: fleet}}
	company := heldBackCompany(t)
	e.epoch.current.Store(company)
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), ceo, true)

	atlassianRow := func() coord.MCPServerStatus {
		t.Helper()
		for _, row := range e.mcpStatus(t.Context()) {
			if row.Server == "atlassian" {
				return row
			}
		}
		t.Fatal("no atlassian row")
		return coord.MCPServerStatus{}
	}
	if row := atlassianRow(); row.Waiting != 1 || row.Failed != 0 {
		t.Fatalf("before any pass, atlassian row = %+v: want the CEO's child waiting", row)
	}

	store, err := e.IntegrationStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	save := func(state integration.State) {
		t.Helper()
		if err := store.SaveIntegration(t.Context(), state); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	save(statedAs(integration.KindAtlassian, integration.Finding{
		Kind:   integration.FindingCredentialMissing,
		Detail: "the Atlassian organization id and its API key did not both resolve",
	}))
	row := atlassianRow()
	if row.Failed != 1 || row.Waiting != 0 {
		t.Fatalf("with the pass refused its own credential, atlassian row = %+v: want "+
			"the CEO's child failed, its wait having no end", row)
	}
	for _, said := range []string{"the atlassian integration will not mint", "until a person acts",
		"organization id"} {
		if !strings.Contains(row.Error, said) {
			t.Errorf("the failure %q does not say %q", row.Error, said)
		}
	}

	save(statedAs(integration.KindAtlassian, integration.Finding{
		Kind: integration.FindingIdentityMissing, Subject: "ceo",
	}))
	if row := atlassianRow(); row.Waiting != 1 || row.Failed != 0 {
		t.Errorf("with the pass making the CEO's account, atlassian row = %+v: want "+
			"waiting again", row)
	}
}

// A SEAL THAT REFRESHES THIS NODE'S SNAPSHOT STARTS A CHILD WAITING FOR IT,
// WITH NO APPLY.
//
// The re-activation a seal asks for is a write the node may not get — here it
// has no config surface at all, as on a worker-only node — and a child held
// back arms no retry. On the node holding the value, the child stayed
// unstarted, its status saying the integration had yet to mint what the
// snapshot resolved.
func TestASealThatRefreshesThisNodeStartsTheChildItHeldBackWithNoApply(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1",
		Keys:     map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")},
	})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}),
		backends: &Backends{Fleet: coordmem.NewFleet()}, cipher: cipher}
	if e.configWriterOrNil() != nil {
		t.Fatal("precondition: this node must have no config surface, so no apply follows")
	}
	company := heldBackCompany(t)
	e.epoch.current.Store(company)
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), ceo, true)
	var missing *seatCredentialMissing
	if outcomes := seatOutcomes(e, "ceo"); len(outcomes) != 1 || !errors.As(outcomes[0].err, &missing) {
		t.Fatalf("before the seal, outcomes = %+v: want the child held back", outcomes)
	}

	sink, err := e.SetupSink(testParty)
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	for name, value := range map[string]string{
		"CEO_ATLASSIAN_EMAIL": "agent-ceo@example.com", "CEO_ATLASSIAN_TOKEN": "ceo-minted",
	} {
		if err := sink.Record(t.Context(), name, value); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	pollUntil(t, 10*time.Second, "the held-back child's start to be attempted", func() bool {
		outcomes := seatOutcomes(e, "ceo")
		return len(outcomes) == 1 && outcomes[0].err != nil && !errors.As(outcomes[0].err, &missing)
	})
}

// lookupGate answers nothing, and holds the FIRST lookup made through it until
// released: a pass reading it is then mid-decision, its snapshot read.
type lookupGate struct {
	once     sync.Once
	entered  chan struct{}
	released chan struct{}
}

func (g *lookupGate) Lookup(string) (string, bool) {
	g.once.Do(func() {
		close(g.entered)
		<-g.released
	})
	return "", false
}

// THE SEAL'S APPLY LANDING WHILE THE CLAIM'S PASS IS DECIDING "WAITING" IS
// FOLDED INTO THE CLAIM.
//
// The claim's pass has read a snapshot without the credential; the apply that
// installs the sealed one asks the seat's runner for a pass while that pass is
// still deciding. A claim that dropped the ask would end with the child held
// back, no retry armed and nothing left to start it until some later apply.
func TestASealDuringTheClaimsWaitingDecisionStartsTheChild(t *testing.T) {
	t.Parallel()
	gate := &lookupGate{entered: make(chan struct{}), released: make(chan struct{})}
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), environ: gate}
	company := heldBackCompany(t)
	e.epoch.current.Store(company)
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })

	// THE CLAIM, as claimSeatChildren leaves it: the caller is the runner,
	// with a pass asked for.
	ceo.mu.Lock()
	ceo.running, ceo.again = true, true
	ceo.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); e.runSeatPasses(t.Context(), ceo) }()

	<-gate.entered
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{
		"CEO_ATLASSIAN_EMAIL": "agent-ceo@example.com", "CEO_ATLASSIAN_TOKEN": "ceo-minted",
	})
	e.secretsMu.Unlock()
	if ceo.ask(true) {
		t.Fatal("the apply became the runner while the claim held it")
	}
	close(gate.released)
	<-done

	outcomes := seatOutcomes(e, "ceo")
	var missing *seatCredentialMissing
	if len(outcomes) != 1 || outcomes[0].err == nil || errors.As(outcomes[0].err, &missing) {
		t.Fatalf("outcomes after the claim = %+v: want the child's start attempted once the "+
			"apply that sealed its credential landed mid-pass", outcomes)
	}
}

// TWO SEATS WAITING ON ONE SERVER REPORT THE LOWER SEAT ID'S WAIT, whatever
// order their outcomes were filed in, so a beat that re-sends the same facts
// sends the same bytes.
func TestTheReportedWaitIsTheFirstBySeatID(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(c)
	always := func() bool { return true }
	ceo := heldSeatChildren(t, e, "ceo", always)
	cto := heldSeatChildren(t, e, "cto", always)
	// Only what the pass mints left missing on atlassian, for both.
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"CEO_JIRA_SITE": "https://acme.example.com"})
	e.secretsMu.Unlock()
	e.convergeSeat(t.Context(), ceo, true)
	e.convergeSeat(t.Context(), cto, true)
	ceoID, _ := c.Org.AgentIDFor(c.Org.AgentSeatByHandle("ceo"))
	ctoID, _ := c.Org.AgentIDFor(c.Org.AgentSeatByHandle("cto"))
	first, firstHandle := ceoID, "CEO"
	if ctoID.String() < ceoID.String() {
		first, firstHandle = ctoID, "CTO"
	}
	for _, row := range e.mcpStatus(t.Context()) {
		if row.Server != "atlassian" {
			continue
		}
		if row.Waiting != 2 || row.WaitSeat != first ||
			!strings.Contains(row.Wait, "${"+firstHandle+"_ATLASSIAN_TOKEN}") {
			t.Errorf("atlassian row = %+v: want 2 waiting, reported as the first seat id %s's", row, first)
		}
		return
	}
	t.Fatal("no atlassian row")
}

// engineLogs is every line the process's logger writes while a test runs, as
// decoded JSON records.
type engineLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *engineLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// records is every line written so far whose message is msg, about seat.
// About the seat because the logger is the process's: a line another case's
// seat writes on a goroutine of its own lands here too.
func (l *engineLogs) records(msg, seat string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg && rec["seat"] == seat {
			out = append(out, rec)
		}
	}
	return out
}

// captureEngineLogs adds a JSON destination to the process's logger for the
// rest of the test. A PROCESS-WIDE CHANGE, so only a test that does not run in
// parallel may make it.
func captureEngineLogs(t *testing.T) *engineLogs {
	t.Helper()
	logs := &engineLogs{}
	debug := slog.LevelDebug
	logging.SetFile(logging.FileSink{Writer: logs, Format: logging.FormatJSON, Level: &debug})
	t.Cleanup(func() { logging.SetFile(logging.FileSink{}) })
	return logs
}

// WAITING IS WRITTEN AT INFO AND A CREDENTIAL NOTHING MINTS AT ERROR, and each
// line names only what applies to it.
//
// The level is the half of "this is not a failure" an operator watching the
// log sees: the claim of every first boot under provisioning used to write an
// error per seat and a retry line per attempt.
func TestWaitingIsLoggedAtInfoAndAMissingCredentialAtError(t *testing.T) {
	logs := captureEngineLogs(t)
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(heldBackCompany(t))
	for _, handle := range []string{"ceo", "cto"} {
		e.convergeSeat(t.Context(), heldSeatChildren(t, e, handle, func() bool { return true }), true)
	}
	waiting := logs.records("mcp_server_awaiting_credential", "ceo")
	if len(waiting) != 1 || waiting[0]["level"] != "INFO" {
		t.Errorf("mcp_server_awaiting_credential = %v: want one line for the CEO, at INFO", waiting)
	}
	missing := logs.records("mcp_server_credential_missing", "cto")
	if len(missing) != 1 || missing[0]["level"] != "ERROR" {
		t.Fatalf("mcp_server_credential_missing = %v: want one line for the CTO, at ERROR", missing)
	}
	if others := logs.records("mcp_server_credential_missing", "ceo"); len(others) != 0 {
		t.Errorf("the CEO's wait was also written as an error: %v", others)
	}
	if _, said := missing[0]["awaiting"]; said {
		t.Errorf("a line about a variable nothing mints names what it awaits: %v", missing[0])
	}
}

// A CHILD WHOSE CREDENTIAL STOPS RESOLVING IS STOPPED, AND SAYS SO.
//
// A sealed token deleted — by a disconnect that removed the seats' accounts,
// or by hand — leaves a running child acting on the value it started with.
// Stopping it is the rule; it was logged as the seat no longer declaring the
// server, which sent the reader to a configuration that still declares it.
func TestAChildWhoseCredentialStopsResolvingIsRetiredSayingSo(t *testing.T) {
	logs := captureEngineLogs(t)
	company, _ := helperCompany(t, false, nil)
	company.Org.AgentSeatByHandle("ceo").MCPEnv["tracker"]["SEAT_TOKEN"] = "${CEO_TRACKER_TOKEN}"
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(company)
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"CEO_TRACKER_TOKEN": "ceo-token"})
	e.secretsMu.Unlock()
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), ceo, true)
	if h := seatChild(ceo); !h.Serving {
		t.Fatalf("with its credential, the CEO's child is %+v: want it serving", h)
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{})
	e.secretsMu.Unlock()
	e.convergeSeat(t.Context(), ceo, true)
	if servers := ceo.bridge.Servers(); len(servers) != 0 {
		t.Errorf("the child whose credential went is still supervised: %v", servers)
	}
	retired := logs.records("mcp_server_retired", "ceo")
	if len(retired) != 1 {
		t.Fatalf("mcp_server_retired = %v: want one line", retired)
	}
	detail, _ := retired[0]["detail"].(string)
	if !strings.Contains(detail, "no longer resolves") || !strings.Contains(detail, "${CEO_TRACKER_TOKEN}") ||
		strings.Contains(detail, "no longer declares") {
		t.Errorf("the child was retired as %q: want the credential that went named, not "+
			"a server the seat still declares", detail)
	}
}

// A VENDOR'S PLAN IS READ IN THIS PACKAGE ONLY THROUGH A PASS'S seatPlan, or by
// a teardown.
//
// The plan is what a seat's child waits for ([Engine.mintedFor]), and a pass
// that minted by a plan of its own — another gate, a filter — would mint what
// no child waits for, or leave a child waiting for what it never mints, with
// every case of the plan's own still green. The Atlassian sealing clock read
// the vendor's plan directly and called a hand-made account one this engine
// provisions. A teardown reads the plan because what it removes is what may
// have been made, whether or not the company still asks for more.
func TestAVendorPlanIsReadOnlyThroughSeatPlan(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	read := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			allowed := fn.Recv != nil && (fn.Name.Name == "seatPlan" || fn.Name.Name == "Teardown")
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "PlanFor" {
					return true
				}
				if _, pkg := sel.X.(*ast.Ident); pkg && !allowed {
					t.Errorf("%s: %s reads a vendor's plan itself; take it from the "+
						"pass's seatPlan", fset.Position(call.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
	if read < 50 {
		t.Fatalf("read %d source files: the walk did not find this package", read)
	}
}

// THE ATLASSIAN SEALING CLOCK COVERS ONLY THE SEATS THIS ENGINE PROVISIONS, the
// ones the pass's seat plan names.
//
// A company with no `integrations.atlassian` block makes no account, so a
// refusal minutes after a person sealed a token is the failure it looks like —
// not Atlassian still applying a grant this engine never made.
func TestTheAtlassianSealingClockCoversOnlySeatsThisEngineProvisions(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1",
		Keys:     map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")},
	})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}, cipher: cipher}
	if sealed := e.atlassianCredentialSealed(specCompany(t, provisioningDoc(false))); sealed != nil {
		t.Error("a company that provisions no Atlassian account was given a sealing clock, " +
			"so a refusal of a hand-made account's token reads as a grant still landing")
	}
	if sealed := e.atlassianCredentialSealed(specCompany(t, provisioningDoc(true))); sealed == nil {
		t.Error("a company that provisions its Atlassian accounts has no sealing clock")
	}
}

// A CODING BOX LAUNCHED WITHOUT A VARIABLE SAYS WHETHER A PASS IS MINTING IT,
// by the rule a seat's own child is judged by.
//
// The box goes ahead either way — a coding run is a tool call a turn made — but
// its warning told the operator to export the variable or store it, for a token
// the GitLab pass was about to mint into that very name: the same variable the
// seat's code-host server points at, which the guide tells a seat to share.
func TestABoxLaunchedWithoutAVariableSaysWhetherAPassIsMintingIt(t *testing.T) {
	logs := captureEngineLogs(t)
	c := specCompany(t, provisioningDoc(true))
	seat := c.Org.AgentSeatByHandle("swe")
	e := &Engine{}

	e.sandboxEnv(t.Context(), c, seat, &org.RoleSandbox{
		Env: map[string]string{"GITLAB_TOKEN": "${SWE_GITLAB_TOKEN}"}}, nil)
	minted := logs.records("sandbox_env_unresolved", "swe")
	if len(minted) != 1 || minted[0]["level"] != "INFO" ||
		!strings.Contains(minted[0]["detail"].(string), "waiting for the gitlab integration to mint ${SWE_GITLAB_TOKEN}") {
		t.Errorf("sandbox_env_unresolved for a minted variable = %v: want one line at INFO "+
			"naming the integration minting it", minted)
	}

	e.sandboxEnv(t.Context(), c, seat, &org.RoleSandbox{
		Env: map[string]string{"OTHER_TOKEN": "${SWE_NOBODY_SETS}"}}, nil)
	all := logs.records("sandbox_env_unresolved", "swe")
	if len(all) != 2 || all[1]["level"] != "WARN" ||
		!strings.Contains(all[1]["detail"].(string), "${SWE_NOBODY_SETS} is not set") {
		t.Errorf("sandbox_env_unresolved for a variable nothing mints = %v: want a second "+
			"line, a warning naming the variable and the remedy", all)
	}

	e.boxServers(t.Context(), c, seat, &org.RoleSandbox{
		MCP: org.RoleSandboxMCP{Servers: []string{"gitlab"}}})
	withheld := logs.records("sandbox_mcp_server_withheld", "swe")
	if len(withheld) != 1 || withheld[0]["level"] != "INFO" ||
		!strings.Contains(withheld[0]["detail"].(string), "a coding run launched once the seal") {
		t.Errorf("sandbox_mcp_server_withheld = %v: want one line at INFO saying which run "+
			"is given the server", withheld)
	}
}
