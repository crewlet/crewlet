package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// AN ENABLED DATADOG BLOCK WITH NO PROVISIONING IS REPORTED AS THE MISSING KEYS
// IT IS, never as connected: those keys register the webhook alerts arrive by,
// so without them the integration serves a route and receives nothing. Config
// refuses such a block, so it is built here by hand.
func TestADatadogBlockWithNoProvisioningIsReportedAsItsMissingKeys(t *testing.T) {
	t.Parallel()
	company := specCompany(t, provisioningDoc(true))
	company.Config.Integrations.Datadog.Provisioning = nil
	// A fleet store, as every engine [New] builds has: the pass's access sync
	// reads the seat access ledger there.
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	e.epoch.current.Store(company)

	findings, err := (&datadogPass{engine: e}).Run(t.Context(), setup.PassInput{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// THE MISSING KEYS LEAD: what keeps the card from reading connected. The
	// agents' accounts, with no authority here to create them, are reported
	// as managed outside Crewlet beside it.
	if len(findings) == 0 || findings[0].Kind != integration.FindingCredentialMissing ||
		findings[0].Subject != "integrations.datadog.provisioning" {
		t.Errorf("findings = %+v: want the provisioning block reported missing, which "+
			"is what keeps the card from reading connected", findings)
	}
}
