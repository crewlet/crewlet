package engine

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// cloudflareDoc is a company with a Cloudflare block whose provisioner token
// names a variable nothing answers.
func cloudflareDoc(enabled string) string {
	return `
name: Acme
seat_access_migrated: [cloudflare]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  cloudflare:
    enabled: ` + enabled + `
    provisioning:
      account_id: 0123456789abcdef0123456789abcdef
      api_token: "${CREWLET_TEST_UNSET_CLOUDFLARE_TOKEN}"
roles:
  - name: SRE
    handle: sre
    llm: zulu
    integrations: {cloudflare: {enabled: true}}
`
}

// A PROVISIONER TOKEN THAT DOES NOT RESOLVE IS THE MISSING CREDENTIAL IT IS,
// named by its field and never reported connected; a switched-off block is not
// configured at all.
func TestACloudflarePassReportsAnUnresolvedProvisioner(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	e.epoch.current.Store(specCompany(t, cloudflareDoc("true")))
	findings, err := (&cloudflarePass{engine: e}).Run(t.Context(), setup.PassInput{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) == 0 || findings[0].Kind != integration.FindingCredentialMissing ||
		findings[0].Subject != "integrations.cloudflare.provisioning.api_token" {
		t.Errorf("findings = %+v, want the provisioner token reported missing", findings)
	}

	off := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
	off.epoch.current.Store(specCompany(t, cloudflareDoc("false")))
	if _, err := (&cloudflarePass{engine: off}).Run(t.Context(), setup.PassInput{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("a switched-off block = %v, want not configured", err)
	}
}
