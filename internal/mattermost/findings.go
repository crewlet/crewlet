package mattermost

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
)

// Findings reads this run as the integration-neutral vocabulary.
//
// SHORT, and that is the third-party app rather than an omission. Mattermost holds one
// outbound websocket per seat and verifies no inbound delivery, so there is no
// ingress to judge and no webhook to be missing: every finding this host can
// produce is about a seat's own identity.
//
// A seat this run KEPT is not a finding, for the reason every other third-party app's
// mapping states: keeping is the successful outcome of a re-run, and reporting
// it would make a converged company look like it had work outstanding.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding

	// THE PASS COULD NOT RUN AT ALL, and this says so as the operator's
	// work rather than as a fault the engine is retrying. Provisioning
	// here creates an ACCOUNT and then a token on it, so a node with
	// nowhere to seal the token must not create the account either — see
	// [provision.CanMint].
	if r.NoKeyring {
		return append(out, integration.Finding{
			Kind:    integration.FindingCredentialMissing,
			Subject: "secrets.keys",
			Detail: "this node has no keyring, so a token minted for a seat " +
				"could not be sealed and no account was created — set " +
				"secrets.keys in the bootstrap configuration",
		})
	}
	for _, handle := range r.Decommissioned {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIdentityMissing,
			Subject: handle,
			Detail: fmt.Sprintf(
				"%s no longer has a Mattermost bot account, so it can neither "+
					"read its channels nor post as itself", handle),
		})
	}
	return out
}
