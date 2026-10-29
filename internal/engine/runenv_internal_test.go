package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/tools"
)

// A coding run's own environment, held to the rule a tool server's values are
// (toolcredentials.go): every reference resolves before a run_sandbox box is
// started, or its key is declared optional and left out.

// runEnvDoc is a company whose engineer runs code in a fake remote cell, with
// a code-host token nothing has sealed and a test database it may go without.
const runEnvDoc = `
name: Acme
providers:
  sandbox:
    fake: true
    setup:
      - name: npm
        files:
          .npmrc: "${NPMRC_BODY}"
        env:
          NPM_TOKEN: "${NPM_TOKEN}"
        optional_keys: [.npmrc, NPM_TOKEN]
  llm:
    zulu:
      model: claude-sonnet-5
      auth: {mode: api-key, api_key: "sk-ant-fake-run-env-key"}
roles:
  - name: Engineer
    handle: swe
    llm: zulu
    sandbox:
      enabled: true
      env:
        GITLAB_TOKEN: "${SWE_GITLAB_TOKEN}"
        DATABASE_URL: "${SWE_TEST_DATABASE}"
      optional_keys: [DATABASE_URL]
`

// A RUN_SANDBOX LAUNCH WHOSE ENVIRONMENT DOES NOT RESOLVE IS REFUSED, BEFORE A
// BOX EXISTS, AND STARTS ONCE IT DOES — without what it declared it may go
// without.
//
// It used to start with GITLAB_TOKEN empty: the coding agent spent a box, an
// install and its model's tokens, then could not clone or push, and the line
// that said why sat at info or warn beside a run that went ahead. Refused, the
// call is one round of the turn — the tool classes it unavailable — and its
// sentence names the variable, its remedies, and where the key could be
// declared optional instead.
func TestARunSandboxLaunchIsRefusedUntilItsEnvironmentResolves(t *testing.T) {
	t.Parallel()
	c := specCompany(t, runEnvDoc)
	e, jobs := codeWorkRig(t, c)
	seat := c.Org.AgentSeatByHandle("swe")

	_, err := (&launcher{engine: e}).Launch(t.Context(),
		&turnctx.Turn{RunID: "t1", Seat: seat}, builtin.SandboxRequest{Brief: "fix the failing test"})
	if err == nil {
		t.Fatal("a run whose code-host token is unset was launched")
	}
	for _, said := range []string{"no box was started", "${SWE_GITLAB_TOKEN} is not set",
		"list GITLAB_TOKEN under role.sandbox.optional_keys"} {
		if !strings.Contains(err.Error(), said) {
			t.Errorf("the refusal %q does not say %q", err, said)
		}
	}
	if strings.Contains(err.Error(), "SWE_TEST_DATABASE") || strings.Contains(err.Error(), "NPM") {
		t.Errorf("the refusal names what the run declared it may go without: %v", err)
	}
	if strings.Contains(err.Error(), "provision command") {
		t.Errorf("a run's setting is offered an integration's provision command, which "+
			"seals a seat's bot or account and nothing else: %v", err)
	}
	if started := jobs[codingagent.ClaudeCodeName].Started(); len(started) != 0 {
		t.Errorf("%d jobs started for a refused launch", len(started))
	}
	if _, found, getErr := e.sandbox.Load().pending.Get(t.Context(), "t1"); getErr != nil || found {
		t.Errorf("a run row exists for a refused launch (found %v, err %v)", found, getErr)
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"SWE_GITLAB_TOKEN": "glpat-swe"})
	e.secretsMu.Unlock()
	if _, err := (&launcher{engine: e}).Launch(t.Context(),
		&turnctx.Turn{RunID: "t2", Seat: seat}, builtin.SandboxRequest{Brief: "fix the failing test"}); err != nil {
		t.Fatalf("the run was refused once its token was sealed: %v", err)
	}
	started := jobs[codingagent.ClaudeCodeName].Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want the one launch", len(started))
	}
	env := started[0].Env
	if env["GITLAB_TOKEN"] != "glpat-swe" {
		t.Errorf("the run's GITLAB_TOKEN is %q, want the sealed token", env["GITLAB_TOKEN"])
	}
	for _, key := range []string{"DATABASE_URL", "NPM_TOKEN"} {
		if value, present := env[key]; present {
			t.Errorf("the optional %s reached the run as %q, want it left out", key, value)
		}
	}
}

// A RUN WAITING FOR A CREDENTIAL A PASS IS MINTING IS REFUSED SAYING WHEN IT
// CAN RUN, and logged at info, as a seat's child waiting for one is.
func TestARunWaitingForAMintedCredentialIsRefusedSayingWhenItCanRun(t *testing.T) {
	logs := captureEngineLogs(t)
	c := specCompany(t, provisioningDoc(true))
	seat := c.Org.AgentSeatByHandle("swe")
	e := &Engine{}

	run := e.runEnvironment(t.Context(), c, seat, &org.RoleSandbox{
		Env: map[string]string{"GITLAB_TOKEN": "${SWE_GITLAB_TOKEN}"}}, nil)
	if run.gap == nil || !run.gap.waiting() {
		t.Fatalf("gap = %+v: want a run waiting for the gitlab integration", run.gap)
	}
	refusal := run.gap.runRefusal()
	for _, said := range []string{"waiting for the gitlab integration to mint ${SWE_GITLAB_TOKEN}",
		"try again then"} {
		if !strings.Contains(refusal, said) {
			t.Errorf("the refusal %q does not say %q", refusal, said)
		}
	}
	run.logGap(t.Context(), seat.Handle(), "no box is started")
	if lines := logs.records("sandbox_env_unresolved", "swe"); len(lines) != 1 ||
		lines[0]["level"] != "INFO" {
		t.Errorf("sandbox_env_unresolved = %v: want one line at INFO", lines)
	}

	nobody := e.runEnvironment(t.Context(), c, seat, &org.RoleSandbox{
		Env: map[string]string{"OTHER_TOKEN": "${SWE_NOBODY_SETS}"}}, nil)
	nobody.logGap(t.Context(), seat.Handle(), "no box is started")
	if lines := logs.records("sandbox_env_unresolved", "swe"); len(lines) != 2 ||
		lines[1]["level"] != "WARN" ||
		!strings.Contains(lines[1]["detail"].(string), "${SWE_NOBODY_SETS} is not set") {
		t.Errorf("sandbox_env_unresolved = %v: want a second line, a warning naming the "+
			"variable", lines)
	}

	// THE PASS THAT MINTS IT WAITING ON A PERSON is no wait at all: the run
	// is told what the integration needs, and not to try again until then.
	blocked := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}}
	store, err := blocked.IntegrationStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := store.SaveIntegration(t.Context(), statedAs(integration.KindGitLab,
		integration.Finding{Kind: integration.FindingCredentialMissing,
			Detail: "the GitLab admin token did not resolve"})); err != nil {
		t.Fatalf("save: %v", err)
	}
	stopped := blocked.runEnvironment(t.Context(), c, seat, &org.RoleSandbox{
		Env: map[string]string{"GITLAB_TOKEN": "${SWE_GITLAB_TOKEN}"}}, nil)
	if stopped.gap == nil || stopped.gap.waiting() {
		t.Fatalf("gap = %+v: want a run the blocked gitlab pass will not mint for", stopped.gap)
	}
	refusal = stopped.gap.runRefusal()
	for _, said := range []string{"the gitlab integration will not mint", "until a person acts",
		"admin token"} {
		if !strings.Contains(refusal, said) {
			t.Errorf("the refusal %q does not say %q", refusal, said)
		}
	}
	if strings.Contains(refusal, "try again then") {
		t.Errorf("a run no pass will mint for is told to try again: %q", refusal)
	}

	// DECLARED OPTIONAL, IT IS STILL WAITED FOR: the declaration is for a
	// value nothing will supply, and started without its code-host identity
	// the run would spend its box cloning nothing.
	declared := e.runEnvironment(t.Context(), c, seat, &org.RoleSandbox{
		Env:          map[string]string{"GITLAB_TOKEN": "${SWE_GITLAB_TOKEN}"},
		OptionalKeys: []string{"GITLAB_TOKEN"}}, nil)
	if declared.gap == nil || !declared.gap.waiting() {
		t.Errorf("gap = %+v: a key a pass mints, declared optional, must still wait for it",
			declared.gap)
	}
}

// OPTIONAL KEYS WRITTEN IN THE DOCUMENT REACH THE RUN, on a provider-wide
// step and on a seat's own: each crosses a conversion — the config into the
// org chart, the org chart or the config into the box's steps — and a field
// one of them dropped would leave every declared key required and every
// run_sandbox refused, on a document that reads as though it said otherwise.
// And a seat step's gap names the seat's step as where to declare it, not a
// provider-wide one that does not hold it.
func TestOptionalKeysWrittenInTheDocumentReachTheRun(t *testing.T) {
	t.Parallel()
	c := specCompany(t, `
name: Acme
providers:
  sandbox:
    fake: true
    setup:
      - name: npm
        files:
          .npmrc: "${NPMRC_BODY}"
        env:
          NPM_TOKEN: "${NPM_TOKEN}"
        optional_keys: [.npmrc, NPM_TOKEN]
  llm:
    zulu:
      model: claude-sonnet-5
      auth: {mode: api-key, api_key: "sk-ant-fake-run-env-key"}
roles:
  - name: Engineer
    handle: swe
    llm: zulu
    sandbox:
      enabled: true
      setup:
        - name: certs
          files:
            ca.pem: "${CA_PEM}"
          env:
            PIP_INDEX_URL: "${PIP_INDEX}"
          optional_keys: [ca.pem, PIP_INDEX_URL]
`)
	e := &Engine{}
	seat := c.Org.AgentSeatByHandle("swe")
	defaults := setupSteps(c.Config.Providers.Sandbox.Setup)
	run := e.runEnvironment(t.Context(), c, seat, seat.Sandbox, defaults)
	if run.gap != nil {
		t.Fatalf("a run whose every unresolved value the document declares optional has a "+
			"gap: %s", run.gap.reason())
	}

	undeclared := *seat.Sandbox
	undeclared.Setup = []org.SandboxSetupStep{seat.Sandbox.Setup[0]}
	undeclared.Setup[0].OptionalKeys = []string{"PIP_INDEX_URL"}
	run = e.runEnvironment(t.Context(), c, seat, &undeclared, defaults)
	if want := `ca.pem under the optional_keys of role.sandbox.setup step "certs"`; run.gap == nil ||
		!strings.Contains(run.gap.reason(), want) {
		t.Errorf("gap = %v: want it to say %q", run.gap, want)
	}
}

// A RUN STARTED WITHOUT THE SEAT'S EMAIL SAYS SO.
//
// The engine's identity facts never refuse a run, and a seat's `email:` naming
// a variable nobody set is left out of CREWLET_AGENT_EMAIL. Left out with
// nothing said, a setup step's `git config user.email "$CREWLET_AGENT_EMAIL"`
// committed as nobody and no line said why.
func TestARunStartedWithoutTheSeatsEmailSaysSo(t *testing.T) {
	logs := captureEngineLogs(t)
	c := specCompany(t, runEnvDoc)
	e, jobs := codeWorkRig(t, c)
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"SWE_GITLAB_TOKEN": "glpat-swe"})
	e.secretsMu.Unlock()
	seat := c.Org.AgentSeatByHandle("swe")
	seat.Email = "${SWE_EMAIL_NOBODY_SETS}"

	if _, err := (&launcher{engine: e}).Launch(t.Context(),
		&turnctx.Turn{RunID: "t1", Seat: seat}, builtin.SandboxRequest{Brief: "fix the failing test"}); err != nil {
		t.Fatalf("a run whose only missing value is the seat's email was refused: %v", err)
	}
	started := jobs[codingagent.ClaudeCodeName].Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want the one launch", len(started))
	}
	if value, present := started[0].Env["CREWLET_AGENT_EMAIL"]; present {
		t.Errorf("the unresolved email reached the run as %q, want it left out", value)
	}
	lines := logs.records("sandbox_env_unresolved", "swe")
	if len(lines) != 1 || lines[0]["level"] != "WARN" ||
		!strings.Contains(lines[0]["detail"].(string), "${SWE_EMAIL_NOBODY_SETS} is not set") ||
		!strings.Contains(lines[0]["detail"].(string), "role.email") {
		t.Errorf("sandbox_env_unresolved = %v: want one warning naming the seat's email "+
			"and its variable", lines)
	}
}

// A RUN'S OPTIONAL KEYS AND FILES ARE LEFT OUT WHOLE, BY THE DECLARATION OF
// THE LAYER WHOSE VALUE WON — and the engine's own identity facts are never
// a gap.
//
// The seat's block writes the step's NPM_TOKEN over again: the seat's value
// is the run's, so the step's declaration no longer covers the key and its
// unset reference is the run's gap — named with the seat block as where to
// declare it. A file the step lists is not written; one it does not is the
// gap, named by its step and path.
func TestARunsOptionalValuesAreLeftOutByTheLayerThatWroteThem(t *testing.T) {
	t.Parallel()
	c := specCompany(t, runEnvDoc)
	e := &Engine{}
	seat := c.Org.AgentSeatByHandle("swe")
	seat.Email = "${SWE_EMAIL_NOBODY_SETS}"
	defaults := []sandbox.SetupStep{{
		Name: "npm", Env: map[string]string{"NPM_TOKEN": "${NPM_TOKEN}"},
		Files:        map[string]string{".npmrc": "${NPMRC_BODY}", "certs/ca.pem": "${CA_PEM}"},
		OptionalKeys: []string{"NPM_TOKEN", ".npmrc"},
	}}
	gate := &org.RoleSandbox{Env: map[string]string{"NPM_TOKEN": "${SWE_NPM_TOKEN}"}}

	run := e.runEnvironment(t.Context(), c, seat, gate, defaults)
	if run.gap == nil {
		t.Fatal("no gap: the seat's own unset NPM_TOKEN and the step's unset ca.pem")
	}
	reason := run.gap.reason()
	for _, said := range []string{"${SWE_NPM_TOKEN}", "${CA_PEM}",
		"list NPM_TOKEN under role.sandbox.optional_keys",
		`certs/ca.pem under the optional_keys of providers.sandbox.setup step "npm"`} {
		if !strings.Contains(reason, said) {
			t.Errorf("the gap %q does not say %q", reason, said)
		}
	}
	if strings.Contains(reason, "SWE_EMAIL") {
		t.Errorf("the engine's own identity fact is reported as the run's gap: %s", reason)
	}
	if _, written := run.setup[0].Files[".npmrc"]; written {
		t.Error("an optional file whose reference does not resolve was written")
	}
	if _, set := run.env["CREWLET_AGENT_EMAIL"]; set {
		t.Errorf("an identity fact that does not resolve reached the run: %v", run.env)
	}
	if run.env["CREWLET_AGENT_HANDLE"] != "swe" {
		t.Errorf("the run lost the identity fact that does resolve: %v", run.env)
	}
}

// AN AGENT-MODE EXECUTOR STARTS WITHOUT WHAT ITS ENVIRONMENT LACKS, where a
// run_sandbox call is refused.
//
// The executor is not a tool call a turn chose but the turn itself: refused,
// the phase fails and the turn with it, so every message the seat could answer
// through its bridge would fail for a token a pass may mint in a minute.
func TestAnAgentModeExecutorStartsWithoutWhatItsEnvironmentLacks(t *testing.T) {
	logs := captureEngineLogs(t)
	c, seat := splitLoginCompany(t, "api", "codex")
	seat.Sandbox = &org.RoleSandbox{Env: map[string]string{"GITHUB_TOKEN": "${SWE_GITHUB_TOKEN}"}}
	seat.Email = "${SWE_EMAIL_NOBODY_SETS}"
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	launcher := &agentLauncher{
		engine: e, turn: &turnctx.Turn{RunID: "t1", Seat: seat}, seat: seat,
		codingAgent: "claude-code", placement: sandbox.E2B,
	}
	err := launcher.LaunchExecutor(t.Context(), runner.AgentRunRequest{
		Brief: "fix the failing test", Round: 1,
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).
			ForTurn(launcher.turn),
	})
	var refused *SandboxCredentialError
	if err != nil {
		t.Fatalf("the executor was refused for its run environment (credential error %t): %v",
			errors.As(err, &refused), err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want the executor's", len(started))
	}
	if value, present := started[0].Env["GITHUB_TOKEN"]; present {
		t.Errorf("the unresolved GITHUB_TOKEN reached the run as %q, want it absent", value)
	}

	// SAID, since nothing refuses it: one line for what the run lacks and one
	// for the engine's own fact it starts without.
	var gap, facts int
	for _, line := range logs.records("sandbox_env_unresolved", seat.Handle()) {
		detail, _ := line["detail"].(string)
		switch {
		case line["level"] == "WARN" && strings.Contains(detail, "the run starts without them") &&
			strings.Contains(detail, "${SWE_GITHUB_TOKEN}"):
			gap++
		case line["level"] == "WARN" && strings.Contains(detail, "${SWE_EMAIL_NOBODY_SETS}"):
			facts++
		}
	}
	if gap != 1 || facts != 1 {
		t.Errorf("sandbox_env_unresolved = %v: want one warning for the run's gap and one "+
			"for the seat's email", logs.records("sandbox_env_unresolved", seat.Handle()))
	}
}

// A FILE WHOSE SECRET IS SET TO NOTHING IS NOT WRITTEN EMPTY: it is the run's
// gap, said as empty rather than unset, as an env value set to nothing is.
//
// secrets.ReadContent answers an empty value as resolved — it reads CONTENT,
// and an empty body is a body — so an .npmrc whose secret was stored empty
// reached the box as an empty file and the registry as no credential.
func TestASetupFileWhoseSecretIsEmptyIsTheRunsGap(t *testing.T) {
	t.Parallel()
	c := specCompany(t, runEnvDoc)
	e := &Engine{}
	e.installSecrets(map[string]string{"CA_PEM": ""})
	seat := c.Org.AgentSeatByHandle("swe")
	steps := []sandbox.SetupStep{{
		Name: "certs", Files: map[string]string{"certs/ca.pem": "${CA_PEM}"},
	}}

	run := e.runEnvironment(t.Context(), c, seat, nil, steps)
	if run.gap == nil || !strings.Contains(run.gap.reason(), "${CA_PEM} is set to an empty value") {
		t.Fatalf("gap = %+v: want ${CA_PEM} named as set to an empty value", run.gap)
	}
	if _, written := run.setup[0].Files["certs/ca.pem"]; written {
		t.Error("a file whose secret is empty was written")
	}
	if len(run.files) != 1 || run.files[0] != "certs:certs/ca.pem" {
		t.Errorf("files = %v, want the step and path named", run.files)
	}
}
