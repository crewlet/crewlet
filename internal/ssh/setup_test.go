package ssh_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
	sshint "github.com/crewlet/crewlet/internal/ssh"
)

// THE CONNECT FORM ASKS FOR NOTHING BUT A SAVE: its one credential is minted
// into SSH_CERTIFICATE_AUTHORITY, and once held the form shows the line servers
// trust and nothing of the key.
func TestTheSSHFormMintsItsAuthorityAndShowsItsPublicLine(t *testing.T) {
	t.Parallel()
	reqs := sshint.Requirements(nil, nil)
	ca := reqs[0]
	if ca.Field != "certificate_authority" || !ca.Mintable || ca.Shape != setup.ShapeSSHCertificateAuthority ||
		ca.SecretName != sshint.CertificateAuthoritySecret || !ca.Required || ca.Kind != setup.KindSecret || ca.Present {
		t.Fatalf("an empty form asks %+v", ca)
	}
	if reqs[1].Field != "enabled" || !reqs[1].Hidden || reqs[1].Default != "true" {
		t.Errorf("the switch is %+v", reqs[1])
	}
	held, err := setup.ShapeSSHCertificateAuthority.Mint(nil)
	if err != nil {
		t.Fatal(err)
	}
	block := &config.SSH{Enabled: true, Provisioning: &config.SSHProvisioning{
		CertificateAuthority: "${" + sshint.CertificateAuthoritySecret + "}"}}
	resolve := func(name string) (string, bool) {
		return held, name == sshint.CertificateAuthoritySecret
	}
	got := sshint.Requirements(block, resolve)[0]
	authority, err := setup.ParseSSHCertificateAuthority(held)
	if err != nil {
		t.Fatal(err)
	}
	if got.Public == nil || strings.TrimSpace(got.Public.Text) != authority.PublicLine() {
		t.Fatalf("the saved form shows %+v, want %q", got.Public, authority.PublicLine())
	}
	if strings.Contains(got.Public.Text, "PRIVATE") || got.Stored != block.Provisioning.CertificateAuthority {
		t.Errorf("the saved form shows %q and stores %q", got.Public.Text, got.Stored)
	}
	broken := sshint.Requirements(block, func(string) (string, bool) { return "not a key", true })[0]
	if broken.Public != nil {
		t.Errorf("a value that is no authority is shown as %+v", broken.Public)
	}
}
