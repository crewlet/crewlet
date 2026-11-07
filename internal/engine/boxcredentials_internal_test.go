package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// boxLinesDoc is a company that provisions Expo, whose QA agent is on and
// still writes the EXPO_TOKEN line Crewlet now sets itself, beside a
// provider-wide step setting it for every box, and whose SRE agent is off at
// Expo and keeps a line of its own.
const boxLinesDoc = `
name: Acme
seat_access_migrated: [expo]
providers:
  llm:
    zulu:
      model: claude-sonnet-5
  sandbox:
    fake: true
    setup:
      - name: shared-expo
        env:
          EXPO_TOKEN: "${SHARED_EXPO}"
integrations:
  expo:
    enabled: true
    provisioning:
      account_name: acme
roles:
  - name: QA
    handle: qa
    llm: zulu
    integrations:
      expo: {enabled: true, level: runner}
    sandbox:
      env:
        EXPO_TOKEN: "${EXPO_TOKEN_QA}"
  - name: SRE
    handle: sre
    llm: zulu
    integrations:
      expo: {enabled: false}
    sandbox:
      env:
        EXPO_TOKEN: "${SHARED_EXPO}"
`

// A LINE CREWLET'S OWN CREDENTIAL REPLACES IS SAID WHERE IT IS WRITTEN: the on
// agent's own line, and the provider-wide step's, naming the agents it is
// ignored for. The off agent's line is its own, and never said. Mutation: drop
// the advisory, or warn the off agent, and this goes red.
func TestALineAnIntegrationsCredentialReplacesIsSaid(t *testing.T) {
	t.Parallel()
	c := specCompany(t, boxLinesDoc)
	got := map[string]string{}
	for _, w := range BoxCredentialWarnings(c.Config) {
		got[w.Path] = w.Seat + ": " + w.Message
	}
	own, ok := got["roles[0].sandbox.env.EXPO_TOKEN"]
	if !ok || !strings.HasPrefix(own, "qa: ") || !strings.Contains(own, "ignored") {
		t.Errorf("the on agent's own line is said as %q, want it ignored for qa (%v)", own, got)
	}
	shared, ok := got["providers.sandbox.setup[0].env.EXPO_TOKEN"]
	if !ok || !strings.Contains(shared, "the box of qa") || strings.Contains(shared, "sre") {
		t.Errorf("the provider-wide line is said as %q, want it ignored for qa alone (%v)", shared, got)
	}
	if _, said := got["roles[1].sandbox.env.EXPO_TOKEN"]; said || len(got) != 2 {
		t.Errorf("warnings = %v, want the two lines Crewlet replaces and nothing for the off agent", got)
	}

	// AND THE LAUNCH TAKES THE NAME FROM EVERY LAYER, so the seat's run is
	// handed its own token alone.
	e := &Engine{}
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"EXPO_TOKEN_QA": "fake-qa-token", "SHARED_EXPO": "fake-shared-token"})
	e.secretsMu.Unlock()
	seat := c.Org.AgentSeatByHandle("qa")
	run := e.runEnvironment(t.Context(), c, seat, seatSandbox(c, "qa"), nil, e.boxDefaults(c.Config, "qa"))
	if _, kept := run.env["EXPO_TOKEN"]; kept || run.credentials["EXPO_TOKEN"] != "fake-qa-token" {
		t.Errorf("env = %v, credentials = %v: want the agent's own token alone", run.env, run.credentials)
	}
}

// withheldDoc is a company whose SRE agent is on at DigitalOcean and whose
// executor runs OpenCode on DigitalOcean's own models, which OpenCode signs in
// to by DIGITALOCEAN_ACCESS_TOKEN, the name doctl reads the agent's token by.
const withheldDoc = `
name: Acme
seat_access_migrated: [digitalocean]
providers:
  sandbox: {fake: true}
  llm:
    oc:
      harness: opencode
      model: digitalocean/llama3.3-70b-instruct
      auth: {mode: env}
      env: {DIGITALOCEAN_ACCESS_TOKEN: "${DO_MODEL_ACCESS_KEY}"}
integrations:
  digitalocean:
    enabled: true
    provisioning:
      team: "${DO_TEAM}"
roles:
  - name: SRE
    handle: sre
    llm: oc
    integrations:
      digitalocean: {enabled: true, level: operate}
`

// AN AGENT'S TOOL TOKEN NEVER SIGNS ITS MODEL IN: under a name its executor
// signs in by, Crewlet withholds the token from the box, leaving the name to
// the entry, and says so at the seat. Mutation: lay the credentials over the
// entry's without the sign-in filter, and the agent's token takes the name.
func TestAToolTokenUnderASignInNameIsWithheldAndSaid(t *testing.T) {
	t.Parallel()
	c := specCompany(t, withheldDoc)
	warnings := BoxCredentialWarnings(c.Config)
	if len(warnings) != 1 || warnings[0].Path != "roles[0].integrations" ||
		!strings.Contains(warnings[0].Message, "does not put the agent's own DigitalOcean credential there") {
		t.Fatalf("warnings = %+v, want the withheld token said at the seat", warnings)
	}
	e := &Engine{}
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"DIGITALOCEAN_TOKEN_SRE": "fake-agent-token", "DO_MODEL_ACCESS_KEY": "fake-model-key"})
	e.secretsMu.Unlock()
	seat := c.Org.AgentSeatByHandle("sre")
	run := e.runEnvironment(t.Context(), c, seat, nil, nil, e.boxDefaults(c.Config, "sre"))
	member, err := c.Models.Head(seat, phase.Execute)
	if err != nil {
		t.Fatalf("executor chain: %v", err)
	}
	executor := newExecutorRun(member.Provider.(*harness.Provider), run)
	if executor.env["DIGITALOCEAN_ACCESS_TOKEN"] == "fake-agent-token" ||
		!slices.Equal(executor.withheld, []string{"DIGITALOCEAN_ACCESS_TOKEN"}) {
		t.Errorf("env = %v, withheld = %v: want the agent's token withheld from the model's name",
			executor.env, executor.withheld)
	}
}
