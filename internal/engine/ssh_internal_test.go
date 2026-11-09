package engine

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// sshPassDoc is a company with SSH connected and a rotation staged.
const sshPassDoc = `
name: Acme
providers:
  sandbox: {fake: true}
integrations:
  ssh:
    enabled: true
    provisioning:
      certificate_authority: "${SSH_CERTIFICATE_AUTHORITY}"
      next_certificate_authority: "${SSH_CERTIFICATE_AUTHORITY_NEXT}"
roles:
  - name: Owner
    handle: owner
    kind: human
`

// THE PASS HOLDS EACH AUTHORITY TO ONE THE ENGINE SIGNS WITH: two minted
// authorities raise nothing, and one that does not resolve, or resolves to a
// value that is no Ed25519 OpenSSH key, is a finding at its own field, so a
// value sealed by hand is reported before a seat needs a certificate from it.
func TestTheSSHPassReportsAnAuthorityItCannotSignWith(t *testing.T) {
	t.Parallel()
	minted := func() string {
		v, err := setup.GenerateSSHCertificateAuthority()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		name string
		env  config.MapSource
		want []string
	}{
		{"both minted", config.MapSource{"SSH_CERTIFICATE_AUTHORITY": minted(),
			"SSH_CERTIFICATE_AUTHORITY_NEXT": minted()}, nil},
		{"the current one unsealed", config.MapSource{"SSH_CERTIFICATE_AUTHORITY_NEXT": minted()},
			[]string{"integrations.ssh.provisioning.certificate_authority"}},
		{"the staged one not a key", config.MapSource{"SSH_CERTIFICATE_AUTHORITY": minted(),
			"SSH_CERTIFICATE_AUTHORITY_NEXT": "not a key"},
			[]string{"integrations.ssh.provisioning.next_certificate_authority"}},
	} {
		e := &Engine{environ: tc.env}
		e.epoch.current.Store(specCompany(t, sshPassDoc))
		findings, err := (&sshPass{engine: e}).run(t.Context(), setup.PassInput{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got []string
		for _, f := range findings {
			if f.Kind != integration.FindingCredentialMissing {
				t.Errorf("%s: finding %+v", tc.name, f)
			}
			got = append(got, f.Subject)
		}
		if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("%s: findings at %v, want %v", tc.name, got, tc.want)
		}
	}
	off := &Engine{}
	off.epoch.current.Store(specCompany(t, "name: Acme\nintegrations:\n  ssh: {enabled: false}\n"))
	if _, err := (&sshPass{engine: off}).run(t.Context(), setup.PassInput{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("a switched-off block = %v, want not configured", err)
	}
}
