package engine

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// scalewayRunDoc is a company that provisions its agents' Scaleway
// applications, whose SRE agent is on in one project and names its sealed key
// as the scw CLI's two variables, and whose QA agent is off at Scaleway.
const scalewayRunDoc = `
name: Acme
seat_access_migrated: [scaleway]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  scaleway:
    enabled: true
    provisioning:
      organization_id: "${SCW_ORG}"
      access_key: SCWPROVISIONERKEY001
      secret_key: "${SCALEWAY_SECRET_KEY}"
roles:
  - name: SRE
    handle: sre
    llm: zulu
    integrations:
      scaleway: {enabled: true, level: operate, projects: [aaaaaaaa-0000-4000-8000-000000000002, aaaaaaaa-0000-4000-8000-000000000001]}
    sandbox:
      env:
        SCW_SECRET_KEY: "${SCALEWAY_SECRET_KEY_SRE}"
        SCW_ACCESS_KEY: "${SCALEWAY_ACCESS_KEY_SRE}"
  - name: QA
    handle: qa
    llm: zulu
    integrations:
      scaleway: {enabled: false}
`

// A SCALEWAY SEAT'S KEY REACHES ITS BOX WHOLE: the pass declares both the
// secret and the access key it seals ([provision.Seat.AlsoVars]), so a run
// naming either waits on the scaleway integration rather than reporting a
// secret somebody forgot, and once both are sealed the box carries them beside
// the engine's own Organization, default project and update switch. A seat
// that is off is declared nothing. Mutation: drop AlsoVars from the plan, and
// the access key reads as a missing secret.
func TestAScalewaySeatsKeyReachesItsBoxWhole(t *testing.T) {
	t.Parallel()
	c := specCompany(t, scalewayRunDoc)
	e := &Engine{}
	want := map[string]integration.Kind{
		"SCALEWAY_SECRET_KEY_SRE": integration.KindScaleway, "SCALEWAY_ACCESS_KEY_SRE": integration.KindScaleway,
	}
	if got := e.mintedFor(c, "sre"); !maps.Equal(got, want) {
		t.Fatalf("sre's minted variables = %v, want %v", got, want)
	}
	if got := e.mintedFor(c, "qa"); len(got) != 0 {
		t.Errorf("an agent that is off at Scaleway is declared %v", got)
	}

	seat := c.Org.AgentSeatByHandle("sre")
	settings := seatSandbox(c, "sre")
	boxDefaults := seataccess.BoxDefaults(c.Config, "sre")
	run := e.runEnvironment(t.Context(), c, seat, settings, nil, boxDefaults)
	if run.gap == nil || !run.gap.waiting() {
		t.Fatalf("gap = %+v: want a run waiting for the scaleway integration", run.gap)
	}
	if said := "waiting for the scaleway integration to mint ${SCALEWAY_ACCESS_KEY_SRE} and ${SCALEWAY_SECRET_KEY_SRE}"; !strings.Contains(run.gap.reason(), said) {
		t.Errorf("the gap %q does not say %q", run.gap.reason(), said)
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{
		"SCALEWAY_SECRET_KEY_SRE": "00000000-0000-4000-8000-0000000000aa",
		"SCALEWAY_ACCESS_KEY_SRE": "SCWFAKE0000000000001",
		"SCW_ORG":                 "11111111-2222-4333-8444-555555555555",
	})
	e.secretsMu.Unlock()
	run = e.runEnvironment(t.Context(), c, seat, settings, nil, boxDefaults)
	if run.gap != nil || run.env["SCW_SECRET_KEY"] != "00000000-0000-4000-8000-0000000000aa" ||
		run.env["SCW_ACCESS_KEY"] != "SCWFAKE0000000000001" {
		t.Fatalf("env = %v, gap = %+v: want the sealed key as the scw CLI's variables", run.env, run.gap)
	}
	defaults := map[string]string{
		"SCW_DEFAULT_ORGANIZATION_ID": "11111111-2222-4333-8444-555555555555",
		"SCW_DEFAULT_PROJECT_ID":      "aaaaaaaa-0000-4000-8000-000000000001",
		"SCW_DISABLE_CHECK_VERSION":   "true",
	}
	if !maps.Equal(run.defaults, defaults) {
		t.Errorf("box defaults = %v, want %v", run.defaults, defaults)
	}
	if !strings.Contains(strings.Join(run.secrets, " "), "00000000-0000-4000-8000-0000000000aa") {
		t.Error("the secret key is not among the run's secrets, so its session would not be scrubbed of it")
	}
}

// THE COMPANY HALF NAMES WHAT IT LACKS by the field that clears it, and a
// block switched off is not configured at all.
func TestTheScalewayPassNamesWhatItLacks(t *testing.T) {
	t.Parallel()
	company := specCompany(t, scalewayRunDoc)
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	e.epoch.current.Store(company)

	findings, err := (&scalewayPass{engine: e}).run(t.Context(), setup.PassInput{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != integration.FindingCredentialMissing ||
		findings[0].Subject != "integrations.scaleway.provisioning.secret_key" {
		t.Errorf("findings = %+v: want the unresolved secret key named", findings)
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"SCALEWAY_SECRET_KEY": "00000000-0000-4000-8000-00000000c0de", "SCW_ORG": "acme"})
	e.secretsMu.Unlock()
	findings, err = (&scalewayPass{engine: e}).run(t.Context(), setup.PassInput{})
	if err != nil || len(findings) != 1 || findings[0].Subject != "integrations.scaleway.provisioning.organization_id" {
		t.Errorf("an Organization ID that is no UUID = %+v, %v: want it named", findings, err)
	}

	company.Config.Integrations.Scaleway.Enabled = false
	if _, err := (&scalewayPass{engine: e}).run(t.Context(), setup.PassInput{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("a block switched off = %v, want not configured", err)
	}
}
