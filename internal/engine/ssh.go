package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// sshPass checks the company's half of SSH, its certificate authority, then
// runs the agents' access through the access sync, which takes no step: a
// seat's level and hosts are the document alone, and its certificates are
// signed at use. No Teardown: nothing is registered anywhere, and a server
// stops trusting the authority when its setup line is removed.
type sshPass struct{ engine *Engine }

func (*sshPass) Kind() integration.Kind { return integration.KindSSH }

// Needs is nil: the certificate authority is minted, never asked for.
func (*sshPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *sshPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

// run holds each certificate authority the document names to one the engine
// can sign with, so a value sealed by hand is a finding before a seat needs a
// certificate from it.
func (p *sshPass) run(context.Context, setup.PassInput) ([]integration.Finding, error) {
	cfg := p.engine.Company().Config.Integrations.SSH
	// DISABLED IS NOT CONFIGURED: switching the block off is how an operator
	// stops every certificate at once.
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	var findings []integration.Finding
	for _, ca := range []struct{ field, ref string }{
		{"certificate_authority", cfg.Provisioning.CertificateAuthority},
		{"next_certificate_authority", cfg.Provisioning.NextCertificateAuthority},
	} {
		if strings.TrimSpace(ca.ref) == "" {
			continue
		}
		subject := "integrations.ssh.provisioning." + ca.field
		value := strings.TrimSpace(env.Value(ca.ref))
		if value == "" {
			findings = append(findings, integration.Finding{
				Kind: integration.FindingCredentialMissing, Subject: subject,
				Detail: subject + " did not resolve, so no agent's certificate can be signed with it",
			})
			continue
		}
		if _, err := setup.ParseSSHCertificateAuthority(value); err != nil {
			findings = append(findings, integration.Finding{
				Kind: integration.FindingCredentialMissing, Subject: subject,
				Detail: strings.TrimPrefix(err.Error(), "setup: ") + "; disconnect SSH and connect it again to mint a new one",
			})
		}
	}
	return findings, nil
}
