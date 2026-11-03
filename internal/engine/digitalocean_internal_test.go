package engine

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// digitaloceanRunDoc is a company whose SRE agent is on at DigitalOcean and
// names its sealed token as doctl's DIGITALOCEAN_ACCESS_TOKEN, and whose QA
// agent is off there.
const digitaloceanRunDoc = `
name: Acme
seat_access_migrated: [digitalocean]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  digitalocean:
    enabled: true
    provisioning:
      team: "${DO_TEAM}"
roles:
  - name: SRE
    handle: sre
    llm: zulu
    integrations:
      digitalocean: {enabled: true, level: operate}
    sandbox:
      env:
        DIGITALOCEAN_ACCESS_TOKEN: "${DIGITALOCEAN_TOKEN_SRE}"
  - name: QA
    handle: qa
    llm: zulu
    integrations:
      digitalocean: {enabled: false}
`

// THE SEAT'S TOKEN REACHES ITS BOX BY THE ENGINE'S EXISTING RULE: the pass
// declares DIGITALOCEAN_TOKEN_<HANDLE> for every agent that is on, so a run
// naming it waits until the access sync seals it, and carries it as doctl's
// variable once it is sealed, among the secrets the run is scrubbed of. A seat
// that is off is declared nothing.
func TestADigitalOceanSeatsTokenReachesItsBox(t *testing.T) {
	t.Parallel()
	c := specCompany(t, digitaloceanRunDoc)
	e := &Engine{}
	want := map[string]integration.Kind{"DIGITALOCEAN_TOKEN_SRE": integration.KindDigitalOcean}
	if got := e.mintedFor(c, "sre"); !maps.Equal(got, want) {
		t.Fatalf("sre's minted variables = %v, want %v", got, want)
	}
	if got := e.mintedFor(c, "qa"); len(got) != 0 {
		t.Errorf("an agent that is off at DigitalOcean is declared %v", got)
	}

	seat := c.Org.AgentSeatByHandle("sre")
	settings := seatSandbox(c, "sre")
	run := e.runEnvironment(t.Context(), c, seat, settings, nil, nil)
	if run.gap == nil || !run.gap.waiting() {
		t.Fatalf("gap = %+v: want a run waiting for the digitalocean integration", run.gap)
	}
	if _, present := run.env["DIGITALOCEAN_ACCESS_TOKEN"]; present {
		t.Error("an unsealed token reached the run")
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"DIGITALOCEAN_TOKEN_SRE": "fake-digitalocean-seat-token"})
	e.secretsMu.Unlock()
	run = e.runEnvironment(t.Context(), c, seat, settings, nil, nil)
	if run.gap != nil || run.env["DIGITALOCEAN_ACCESS_TOKEN"] != "fake-digitalocean-seat-token" {
		t.Fatalf("env = %v, gap = %+v: want the sealed token as DIGITALOCEAN_ACCESS_TOKEN", run.env, run.gap)
	}
	if !strings.Contains(strings.Join(run.secrets, " "), "fake-digitalocean-seat-token") {
		t.Error("the token is not among the run's secrets, so its session would not be scrubbed of it")
	}
}

// THE COMPANY HALF IS THE TEAM NAME: one that resolves to nothing is named by
// its field, and a block switched off is not configured at all.
func TestTheDigitalOceanPassNamesWhatItLacks(t *testing.T) {
	t.Parallel()
	company := specCompany(t, digitaloceanRunDoc)
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	e.epoch.current.Store(company)

	findings, err := (&digitaloceanPass{engine: e}).run(t.Context(), setup.PassInput{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != integration.FindingCredentialMissing ||
		findings[0].Subject != "integrations.digitalocean.provisioning.team" {
		t.Errorf("findings = %+v: want the unresolved team named", findings)
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"DO_TEAM": "Example Team"})
	e.secretsMu.Unlock()
	if findings, err = (&digitaloceanPass{engine: e}).run(t.Context(), setup.PassInput{}); err != nil || len(findings) != 0 {
		t.Errorf("a resolved team = %+v, %v: want nothing to report", findings, err)
	}

	company.Config.Integrations.DigitalOcean.Enabled = false
	if _, err := (&digitaloceanPass{engine: e}).run(t.Context(), setup.PassInput{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("a block switched off = %v, want not configured", err)
	}
}

// A PASTE FOR A SEAT THAT IS NOT WAITING FOR ONE IS REFUSED BEFORE
// DIGITALOCEAN IS ASKED: a handle that is no agent seat, and an agent off there.
func TestADigitalOceanPasteIsRefusedForASeatNotWaiting(t *testing.T) {
	t.Parallel()
	company := specCompany(t, digitaloceanRunDoc)
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	e.epoch.current.Store(company)
	api := &SeatAccessAPI{engine: e}
	q := setupapi.AccessQuery{Kind: integration.KindDigitalOcean, Company: company.Config, Roster: company.Org}
	by := iam.Actor{Name: "test"}
	lane := seataccess.LaneDigitalOcean
	if _, err := api.HandIn(t.Context(), q, "nobody", lane, "fake-token", by); !errors.Is(err, setupapi.ErrNoSuchAccessSeat) {
		t.Errorf("an unknown seat = %v", err)
	}
	if _, err := api.HandIn(t.Context(), q, "qa", lane, "fake-token", by); !errors.Is(err, setupapi.ErrNotAwaitingHandIn) {
		t.Errorf("a seat off at DigitalOcean = %v", err)
	}
}

// A SEAT'S TOOL CREDENTIAL IS ITS TOOL'S, NEVER OPENCODE'S SIGN-IN. OpenCode
// reads DIGITALOCEAN_ACCESS_TOKEN as its DigitalOcean provider's key and
// GITLAB_TOKEN as its GitLab provider's, so the agent's own token, handed to
// doctl or glab in its box, reaches the box but is judged out of the run's
// sign-in: an OpenCode seat with no model key of its own is refused a remote
// box rather than launched on the agent's tool token. Mutation: judge the
// sign-in on the seat's whole env, and both runs are launched.
func TestASeatsToolCredentialInTheBoxNeverSignsOpenCodeIn(t *testing.T) {
	t.Parallel()
	const doLine = `        DIGITALOCEAN_ACCESS_TOKEN: "${DIGITALOCEAN_TOKEN_SRE}"`
	for _, tc := range []struct{ name, env, sealed, line string }{
		{name: "doctl's DigitalOcean token", env: "DIGITALOCEAN_ACCESS_TOKEN", sealed: "DIGITALOCEAN_TOKEN_SRE",
			line: doLine},
		{name: "glab's GitLab token", env: "GITLAB_TOKEN", sealed: "GITLAB_TOKEN_SRE",
			line: `        GITLAB_TOKEN: "${GITLAB_TOKEN_SRE}"`},
	} {
		doc := strings.Replace(digitaloceanRunDoc, "    zulu:\n      model: claude-sonnet-5\n",
			"    zulu:\n      harness: opencode\n      model: openrouter/qwen\n", 1)
		c := specCompany(t, strings.Replace(doc, doLine, tc.line, 1))
		e := &Engine{}
		e.secretsMu.Lock()
		e.installSecrets(map[string]string{tc.sealed: "fake-seat-tool-token"})
		e.secretsMu.Unlock()

		seat := c.Org.AgentSeatByHandle("sre")
		run := e.runEnvironment(t.Context(), c, seat, seatSandbox(c, "sre"), nil, nil)
		if !slices.Equal(run.tools, []string{tc.env}) {
			t.Fatalf("%s: tools = %v, want %s alone", tc.name, run.tools, tc.env)
		}
		member, err := c.Models.Head(seat, phase.Execute)
		if err != nil {
			t.Fatalf("%s: executor chain: %v", tc.name, err)
		}
		agent := member.Provider.(*harness.Provider)
		if !agent.SignsInThroughEnv() {
			t.Fatalf("%s: the entry is not OpenCode, so this case tests nothing", tc.name)
		}
		coding := newCodingRun(agent, run)
		if coding.env[tc.env] != "fake-seat-tool-token" {
			t.Errorf("%s: the tool's token did not reach the box: %v", tc.name, coding.env)
		}
		if _, present := coding.signIn[tc.env]; present {
			t.Errorf("%s: the agent's tool token is judged as part of the run's sign-in", tc.name)
		}
		if err := sandboxCredentials(agent, member.Key, "sre", sandbox.E2B, coding.env); err != nil {
			t.Fatalf("%s: the token is not credential-named, so this case tests nothing: %v", tc.name, err)
		}
		if err := sandboxCredentials(agent, member.Key, "sre", sandbox.E2B, coding.signIn); err == nil {
			t.Errorf("%s: the agent's tool token signed OpenCode in to a remote box", tc.name)
		}
	}
}
