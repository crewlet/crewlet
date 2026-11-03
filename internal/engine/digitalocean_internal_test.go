package engine

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
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
