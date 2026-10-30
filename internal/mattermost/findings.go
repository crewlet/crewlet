package mattermost

import "github.com/crewlet/crewlet/internal/integration"

// Findings reads this run as the integration-neutral vocabulary.
//
// SHORT, and that is the third-party app rather than an omission. Mattermost
// holds one outbound websocket per seat and verifies no inbound delivery, so
// there is no ingress to judge and no webhook to be missing, and each agent's
// own bot is reported by the per-seat access sync. What is left is whether
// the instance will let this engine provision at all.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding

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
