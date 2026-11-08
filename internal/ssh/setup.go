package ssh

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// The names the engine seals the company's certificate authorities under.
const (
	CertificateAuthoritySecret     = "SSH_CERTIFICATE_AUTHORITY"
	NextCertificateAuthoritySecret = "SSH_CERTIFICATE_AUTHORITY_NEXT"
	// RetiredCertificateAuthoritiesSecret holds the public lines of the
	// authorities a rotation retired, one a line, newest last: no key.
	RetiredCertificateAuthoritiesSecret = "SSH_CERTIFICATE_AUTHORITY_RETIRED"
)

// MaxRetiredAuthorities is how many retired public lines are kept.
const MaxRetiredAuthorities = 4

// trustNote is the sentence beside the authority's public line.
const trustNote = "Servers trust this line. Its private key never leaves the engine."

// Requirements says what this company still needs for SSH: the certificate
// authority, which Save mints. Hosts are not requirements; they have routes of
// their own. A nil block answers every field absent, so the form still renders.
func Requirements(in *config.SSH, resolve func(string) (string, bool)) []setup.Requirement {
	var prov config.SSHProvisioning
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			prov = *in.Provisioning
		}
	}
	reqs := []setup.Requirement{
		{
			Field: "certificate_authority", Label: "Certificate authority", Kind: setup.KindSecret,
			ConfigPath: "integrations.ssh.provisioning.certificate_authority",
			SecretName: CertificateAuthoritySecret, Required: true, Connect: true,
			// MINTABLE, because a server needs only the public line: the
			// engine makes the authority, seals its key, and nobody
			// downloads a credential at all.
			Mintable: true, Shape: setup.ShapeSSHCertificateAuthority,
			Hint: "Crewlet makes it when you save.",
			More: "Crewlet seals it as " + CertificateAuthoritySecret + ". " + trustNote + " Each server's " +
				"setup adds the line, and each agent's certificate lasts minutes.",
			CheckNote: "Each server proves it with Test connection.",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field: "enabled", Label: "Let agents reach servers", Kind: setup.KindToggle,
			ConfigPath: "integrations.ssh.enabled", Required: true, Hidden: true, Default: "true",
		},
	}
	reqs[0].Present, reqs[0].Resolved, reqs[0].Stored = setup.Held(prov.CertificateAuthority, resolve)
	if reqs[0].Resolved != nil && *reqs[0].Resolved {
		reqs[0].Public = publicView(setup.Deref(prov.CertificateAuthority, resolve))
	}
	reqs[1].Present, reqs[1].Resolved, reqs[1].Stored = setup.Toggle(enabled)
	return reqs
}

// publicView is the held authority's public line, nil where the value is not
// an authority the engine can sign with.
func publicView(held string) *setup.PublicView {
	ca, err := setup.ParseSSHCertificateAuthority(held)
	if err != nil {
		return nil
	}
	view, err := setup.PublicOf(held, ca.PublicLine()+"\n", "text")
	if err != nil {
		return nil
	}
	return view
}

// Guide is what the connect form says around its one field.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Agents reach your servers with certificates Crewlet signs for minutes at a time. No SSH key " +
			"is stored on a server or in an agent's box.",
		Prepare: []setup.Step{
			{Text: "Save to create Crewlet's certificate authority.", Yields: "certificate_authority"},
			{Text: "Add each server, compare its host key with the server's console, and run its setup as root."},
			{Text: "Press Test connection on each server, then turn agents on from the Agents tab."},
		},
	}
}

// Probe answers the connect form's Test button, which SSH has nothing for: the
// form's one credential is minted, and each server is proven by its own Test
// connection. httpClient is accepted for the shape every probe has.
func Probe(*http.Client) setup.Probe {
	return func(_ context.Context, check setup.Check) (setup.Verdict, error) {
		return setup.Verdict{}, setup.NotCheckable("SSH", check.Field)
	}
}
