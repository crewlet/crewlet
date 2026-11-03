package engine

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// expoRunDoc is a company that provisions its agents' Expo robots, whose QA
// agent is on and names its sealed token as the EAS CLI's EXPO_TOKEN, and whose
// SRE agent is off at Expo.
const expoRunDoc = `
name: Acme
seat_access_migrated: [expo]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  expo:
    enabled: true
    provisioning:
      account_name: example-org
      access_token: "${EXPO_ACCESS_TOKEN}"
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
`

// THE SEAT'S EXPO TOKEN REACHES ITS BOX BY THE ENGINE'S EXISTING RULE: the
// Expo pass declares EXPO_TOKEN_<HANDLE> for every agent that is on, so a run
// naming it waits on the expo integration until the access sync seals it, and
// carries it as EXPO_TOKEN once it is sealed. A seat that is off is declared
// nothing.
func TestAnExpoSeatsTokenReachesItsBoxAsExpoToken(t *testing.T) {
	t.Parallel()
	c := specCompany(t, expoRunDoc)
	e := &Engine{}
	if got, want := e.mintedFor(c, "qa"), map[string]integration.Kind{"EXPO_TOKEN_QA": integration.KindExpo}; !maps.Equal(got, want) {
		t.Fatalf("qa's minted variables = %v, want %v", got, want)
	}
	if got := e.mintedFor(c, "sre"); len(got) != 0 {
		t.Errorf("an agent that is off at Expo is declared %v", got)
	}

	seat := c.Org.AgentSeatByHandle("qa")
	settings := seatSandbox(c, "qa")
	run := e.runEnvironment(t.Context(), c, seat, settings, nil, nil)
	if run.gap == nil || !run.gap.waiting() {
		t.Fatalf("gap = %+v: want a run waiting for the expo integration", run.gap)
	}
	if said := "waiting for the expo integration to mint ${EXPO_TOKEN_QA}"; !strings.Contains(run.gap.reason(), said) {
		t.Errorf("the gap %q does not say %q", run.gap.reason(), said)
	}
	if _, present := run.env["EXPO_TOKEN"]; present {
		t.Error("an unsealed token reached the run")
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"EXPO_TOKEN_QA": "fake-expo-seat-token"})
	e.secretsMu.Unlock()
	run = e.runEnvironment(t.Context(), c, seat, settings, nil, nil)
	if run.gap != nil || run.env["EXPO_TOKEN"] != "fake-expo-seat-token" {
		t.Fatalf("env = %v, gap = %+v: want the sealed token as EXPO_TOKEN", run.env, run.gap)
	}
	if !strings.Contains(strings.Join(run.secrets, " "), "fake-expo-seat-token") {
		t.Error("the token is not among the run's secrets, so its session would not be scrubbed of it")
	}
}

// THE COMPANY HALF SAYS WHAT IT LACKS BEFORE IT CALLS EXPO: a provisioner
// token that resolves to nothing is the missing credential, named by its
// field, and a block switched off is not configured at all.
func TestTheExpoPassNamesWhatItLacks(t *testing.T) {
	t.Parallel()
	company := specCompany(t, expoRunDoc)
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	e.epoch.current.Store(company)

	findings, err := (&expoPass{engine: e}).run(t.Context(), setup.PassInput{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != integration.FindingCredentialMissing ||
		findings[0].Subject != "integrations.expo.provisioning.access_token" {
		t.Errorf("findings = %+v: want the unresolved provisioner token named", findings)
	}

	company.Config.Integrations.Expo.Provisioning = nil
	findings, err = (&expoPass{engine: e}).run(t.Context(), setup.PassInput{})
	if err != nil || len(findings) != 1 || findings[0].Subject != "integrations.expo.provisioning" {
		t.Errorf("no provisioning = %+v, %v: want it reported missing", findings, err)
	}

	company.Config.Integrations.Expo.Enabled = false
	if _, err := (&expoPass{engine: e}).run(t.Context(), setup.PassInput{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("a block switched off = %v, want not configured", err)
	}
}
