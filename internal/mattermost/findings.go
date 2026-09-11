package mattermost

import "github.com/crewlet/crewlet/internal/integration"

// Findings reads this run as the integration-neutral vocabulary.
//
// SHORT, and that is the third-party app rather than an omission. Mattermost holds one
// outbound websocket per seat and verifies no inbound delivery, so there is no
// ingress to judge and no webhook to be missing: every finding this host can
// produce is about a seat's own identity or about the instance refusing to let
// this engine create one.
//
// A seat this run KEPT is not a finding, for the reason every other third-party app's
// mapping states: keeping is the successful outcome of a re-run, and reporting
// it would make a converged company look like it had work outstanding.
//
// NOR IS A SEAT THIS RUN DECOMMISSIONED. That branch existed and reported
// every disabled bot as [integration.FindingIdentityMissing] — a kind whose
// verdict is PhaseProvisioning / ActorEngine, which renders as "the engine is
// creating agent identities". [Result.Decommissioned] is exactly the seats the
// company document NO LONGER HAS: an operator passed -decommission and the run
// disabled their accounts, deliberately and successfully. So a destructive run
// that did precisely what was asked reported itself as outstanding work the
// engine was mid-way through, for handles that will never be created, on a row
// nothing ever clears. GitLab's mapping carried the same branch and lost it
// for the same reason.
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

	// THE ADMINISTRATOR TOKEN RESOLVED AND CANNOT WRITE.
	//
	// REJECTED rather than MISSING, which is the distinction that kind was
	// split on: a ${VAR} pointing at nothing is missing, and "an account
	// that lost the access it was issued with" is this. It never clears on
	// its own — every pass is refused identically until somebody gives that
	// account the system_admin role or supplies a token from one that has
	// it — so reporting it as a wait would leave an operator watching a
	// retry that cannot succeed.
	//
	// A CONVERGED COMPANY REACHES THIS. Nothing is written on a pass that
	// needs nothing, so the refusal is invisible until the day a seat is
	// added and every pass from then on fails. That is precisely the window
	// this is worth reporting in.
	if r.NotAdmin != "" {
		out = append(out, integration.Finding{
			Kind:    integration.FindingCredentialRejected,
			Subject: "integrations.mattermost.provisioning.admin_token",
			Detail:  r.NotAdmin,
		})
	}

	// AND THE INSTANCE'S OWN SWITCHES, which no credential can get past.
	//
	// The ADMIN's, not the operator's: what has to happen is in somebody's
	// System Console, which is what [integration.FindingApprovalRequired]
	// names — a capability a person has to turn on at the third-party app
	// before this engine can use it.
	for _, setting := range r.Disabled {
		out = append(out, integration.Finding{
			Kind:    integration.FindingApprovalRequired,
			Subject: "ServiceSettings." + setting.Key,
			Detail:  setting.Detail(),
		})
	}
	return out
}
