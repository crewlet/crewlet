package engine

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// An executor run's own environment, held to the rule a tool server's values
// are (toolcredentials.go): every reference resolves, or the run starts
// without it and a line says why — unless its key is declared optional, and
// then it is left out without a word.

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
      env:
        GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"
        DATABASE_URL: "${SWE_TEST_DATABASE}"
      optional_keys: [DATABASE_URL]
`

// launchExecutor launches turnID's executor on seat through the real launcher,
// as the runner would hand it the turn's ask.
func launchExecutor(t *testing.T, e *Engine, c *Company, seat *org.Role, turnID string) error {
	t.Helper()
	l := launcherOn(e, c, seat, turnID)
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	return err
}

// AN EXECUTOR'S RUN CARRIES WHAT ITS ENVIRONMENT RESOLVES, and leaves out what
// it declared it may go without — and once a value it lacked is sealed, the
// next launch carries that too.
//
// It started with the sealed code-host token, the seat's own optional keys
// left out WHOLE rather than written empty: an empty DATABASE_URL is a run
// pointed at no database where an absent one is a run that knows it has none.
func TestAnExecutorRunCarriesWhatItsEnvironmentResolves(t *testing.T) {
	t.Parallel()
	c := specCompany(t, runEnvDoc)
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	seat := c.Org.AgentSeatByHandle("swe")

	if err := launchExecutor(t, e, c, seat, "t1"); err != nil {
		t.Fatalf("an executor was refused for a code-host token nothing has sealed yet: %v", err)
	}
	if value, present := jobs.Started()[0].Env["GITLAB_TOKEN"]; present {
		t.Errorf("the unset GITLAB_TOKEN reached the run as %q, want it absent", value)
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"GITLAB_TOKEN_SWE": "glpat-swe"})
	e.secretsMu.Unlock()
	if err := launchExecutor(t, e, c, seat, "t2"); err != nil {
		t.Fatalf("LaunchExecutor once the token was sealed: %v", err)
	}
	started := jobs.Started()
	if len(started) != 2 {
		t.Fatalf("%d jobs started, want the two launches", len(started))
	}
	env := started[1].Env
	if env["GITLAB_TOKEN"] != "glpat-swe" {
		t.Errorf("the run's GITLAB_TOKEN is %q, want the sealed token", env["GITLAB_TOKEN"])
	}
	for _, key := range []string{"DATABASE_URL", "NPM_TOKEN"} {
		if value, present := env[key]; present {
			t.Errorf("the optional %s reached the run as %q, want it left out", key, value)
		}
	}
}

// A RUN WAITING FOR A CREDENTIAL A PASS IS MINTING SAYS WHAT IT WAITS FOR, and
// is logged at info, as a seat's child waiting for one is — a warning only for
// a value nothing will ever supply.
func TestARunWaitingForAMintedCredentialSaysWhatItWaitsFor(t *testing.T) {
	logs := captureEngineLogs(t)
	c := specCompany(t, provisioningDoc(true))
	seat := c.Org.AgentSeatByHandle("swe")
	e := &Engine{}

	run := e.runEnvironment(t.Context(), c, seat, &org.RoleSandbox{
		Env: map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_SWE}"}}, nil)
	if run.gap == nil || !run.gap.waiting() {
		t.Fatalf("gap = %+v: want a run waiting for the gitlab integration", run.gap)
	}
	if said := "waiting for the gitlab integration to mint ${GITLAB_TOKEN_SWE}"; !strings.Contains(run.gap.reason(), said) {
		t.Errorf("the gap %q does not say %q", run.gap.reason(), said)
	}
	run.logGap(t.Context(), seat.Handle(), "the run starts without them")
	if lines := logs.records("sandbox_env_unresolved", "swe"); len(lines) != 1 ||
		lines[0]["level"] != "INFO" {
		t.Errorf("sandbox_env_unresolved = %v: want one line at INFO", lines)
	}

	nobody := e.runEnvironment(t.Context(), c, seat, &org.RoleSandbox{
		Env: map[string]string{"OTHER_TOKEN": "${SWE_NOBODY_SETS}"}}, nil)
	nobody.logGap(t.Context(), seat.Handle(), "the run starts without them")
	if lines := logs.records("sandbox_env_unresolved", "swe"); len(lines) != 2 ||
		lines[1]["level"] != "WARN" ||
		!strings.Contains(lines[1]["detail"].(string), "${SWE_NOBODY_SETS} is not set") {
		t.Errorf("sandbox_env_unresolved = %v: want a second line, a warning naming the "+
			"variable", lines)
	}

	// THE PASS THAT MINTS IT WAITING ON A PERSON is no wait at all: the gap
	// says what the integration needs.
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
		Env: map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_SWE}"}}, nil)
	if stopped.gap == nil || stopped.gap.waiting() {
		t.Fatalf("gap = %+v: want a run the blocked gitlab pass will not mint for", stopped.gap)
	}
	reason := stopped.gap.reason()
	for _, said := range []string{"the gitlab integration will not mint", "until a person acts",
		"admin token"} {
		if !strings.Contains(reason, said) {
			t.Errorf("the gap %q does not say %q", reason, said)
		}
	}

	// DECLARED OPTIONAL, IT IS STILL WAITED FOR: the declaration is for a
	// value nothing will supply, and started without its code-host identity
	// the run would spend its box cloning nothing.
	declared := e.runEnvironment(t.Context(), c, seat, &org.RoleSandbox{
		Env:          map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_SWE}"},
		OptionalKeys: []string{"GITLAB_TOKEN"}}, nil)
	if declared.gap == nil || !declared.gap.waiting() {
		t.Errorf("gap = %+v: a key a pass mints, declared optional, must still wait for it",
			declared.gap)
	}
}

// A MINT GITHUB DID NOT ANSWER IS A WAIT, AND ONE IT REFUSED IS A PERSON'S, in
// the sentence the run's gap is logged with and a seat's tool server is held
// back with. The seat's GitHub App token is issued as the run's environment is
// read, and a mint that met GitHub failing on its side, at its rate limit or
// not reached at all is asked for again on the restart schedule, which the gap
// says. One GitHub refused (a suspended installation, a revoked key) is
// refused again at every ask until somebody fixes the app, and the gap says
// that rather than a wait that ends nothing; beside a variable nobody set, the
// gap tells both.
//
// Mutation: read every unissued mint as passing and the refused rows go red;
// read every one as refused and the waiting rows do; read a 403 by its status
// alone and the rate limit's row does.
func TestAMintGitHubDidNotAnswerIsAWaitAndOneItRefusedIsAPersons(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	const (
		askedAgain = "GitHub is asked for it again on the restart schedule"
		fixTheApp  = "until somebody fixes the agent's GitHub App"
	)
	for name, c := range map[string]struct {
		// status is what the token endpoint answers, with its rate limit's
		// header where limited is set, and url where GitHub is when it is
		// not the fixture: one nothing listens on.
		status  int
		limited bool
		url     string
		also    map[string]string
		says    []string
	}{
		"a 503":                        {status: http.StatusServiceUnavailable, says: []string{askedAgain}},
		"a 429":                        {status: http.StatusTooManyRequests, says: []string{askedAgain}},
		"a 403 at GitHub's rate limit": {status: http.StatusForbidden, limited: true, says: []string{askedAgain}},
		"GitHub not reached":           {url: closed.URL, says: []string{askedAgain}},
		"a suspended installation":     {status: http.StatusForbidden, says: []string{fixTheApp}},
		"a revoked key":                {status: http.StatusUnauthorized, says: []string{fixTheApp}},
		"a 503 beside a variable nobody set": {status: http.StatusServiceUnavailable,
			also: map[string]string{"OTHER_TOKEN": "${BUILDER_NOBODY_SETS}"},
			says: []string{"${BUILDER_NOBODY_SETS} is not set", askedAgain}},
	} {
		gh := newAppGitHub(t)
		gh.status, gh.limited = c.status, c.limited
		at := gh.url
		if c.url != "" {
			at = c.url
		}
		co := appCompany(t, at, true)
		builder := co.Org.AgentSeatByHandle("builder")
		env := map[string]string{"GITHUB_TOKEN": "${GITHUB_APP_TOKEN_BUILDER}"}
		maps.Copy(env, c.also)
		run := appEngine(t, co).runEnvironment(t.Context(), co, builder,
			&org.RoleSandbox{Env: env}, nil)
		if run.gap == nil {
			t.Errorf("%s: the run was given a token GitHub did not issue", name)
			continue
		}
		said := run.gap.reason()
		if !strings.Contains(said, "${GITHUB_APP_TOKEN_BUILDER} could not be issued") {
			t.Errorf("%s: the gap says %q, want the mint's failure", name, said)
		}
		for _, want := range c.says {
			if !strings.Contains(said, want) {
				t.Errorf("%s: the gap says %q, want %q", name, said, want)
			}
		}
	}
}

// OPTIONAL KEYS WRITTEN IN THE DOCUMENT REACH THE RUN, on a provider-wide
// step and on a seat's own: each crosses a conversion — the config into the
// org chart, the org chart or the config into the box's steps — and a field
// one of them dropped would leave every declared key required and every run
// started without it said as a gap, on a document that reads as though it said
// otherwise.
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
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"GITLAB_TOKEN_SWE": "glpat-swe"})
	e.secretsMu.Unlock()
	seat := c.Org.AgentSeatByHandle("swe")
	seat.Email = "${SWE_EMAIL_NOBODY_SETS}"

	if err := launchExecutor(t, e, c, seat, "t1"); err != nil {
		t.Fatalf("a run whose only missing value is the seat's email was refused: %v", err)
	}
	started := jobs.Started()
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

// AN EXECUTOR STARTS WITHOUT WHAT ITS ENVIRONMENT LACKS, AND SAYS SO.
//
// The executor is the turn itself: refused, the phase fails and the turn with
// it, so every message the seat could answer through its bridge would fail for
// a token a pass may mint in a minute. So it starts, and the lines say what it
// started without.
func TestAnExecutorStartsWithoutWhatItsEnvironmentLacks(t *testing.T) {
	logs := captureEngineLogs(t)
	c, seat := executorCompany(t)
	seat.Sandbox = &org.RoleSandbox{Env: map[string]string{"GITHUB_TOKEN": "${SWE_GITHUB_TOKEN}"}}
	seat.Email = "${SWE_EMAIL_NOBODY_SETS}"
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	err := launchExecutor(t, e, c, seat, "t1")
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
