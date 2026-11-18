package gitlab

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Findings reads this run as the integration-neutral vocabulary.
//
// See jira.Result.Findings for why the mapping lives here and the ordering
// does not. This pass holds the company-level half of GitLab, so its findings
// are about the company's connection: a deployment GitLab cannot deliver to,
// and a provisioning token about to lapse. A seat's account is per-seat
// access, and its findings come from the shared sync.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding

	// A RUN THAT LEFT THE INSTANCE WITH NOWHERE TO DELIVER TO.
	//
	// Keyed on NoIngress, not on a refused registration: every route through
	// `ensureHooks` returns a non-empty list or an ERROR, so "hooked on
	// nothing" is unreachable, while the state this pass does reach (no public
	// base URL to point a hook at) would classify as READY. See
	// [Result.NoIngress].
	//
	// A credential that may not administer a hook fails the whole pass as a
	// fault, not a finding. No retry fixes that either, but reporting it
	// instead would change what `crewlet gitlab provision` prints and exits
	// with.
	if r.NoIngress != "" {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: setup.BaseKey(r.NoIngressKey),
			Detail:  r.NoIngress,
		})
	}

	// THE CREDENTIAL THIS PASS RUNS ON IS ABOUT TO LAPSE. Advisory — it
	// works today — and the operator's, because the replacement is a value
	// in this deployment's secret store whoever mints it at GitLab. Named
	// by its config path, the same way every other finding here names what
	// to change, and by the date GitLab published.
	if !r.CredentialExpires.IsZero() {
		out = append(out, integration.Finding{
			Kind:    integration.FindingCredentialExpiring,
			Subject: "integrations.gitlab.provisioning.admin_token",
			Title:   "Admin token expires soon",
			Detail: fmt.Sprintf("the group Owner token this integration runs on "+
				"expires on %s. From then on no pass can provision or repair "+
				"an agent", r.CredentialExpires.Format(time.DateOnly)),
			Remedy: "Store a new group Owner token with the `api` scope as " +
				"`integrations.gitlab.provisioning.admin_token`",
			ExpiresAt: r.CredentialExpires,
		})
	}

	return out
}
