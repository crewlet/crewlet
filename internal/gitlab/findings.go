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
	// This used to read `r.Hooked != "" && len(r.HookedOn) == 0`, described
	// as the shape a refused registration leaves. It is not a shape
	// [Reconcile] can produce: every route through `ensureHooks` either
	// returns a non-empty list or returns an ERROR, so the branch could
	// never fire and the one ingress state this pass really does reach —
	// no public base URL to point a hook at — reported nothing at all and
	// classified as READY. See [Result.NoIngress].
	//
	// The refusal that branch was written for is still a fault rather than
	// a finding: a credential that may not administer the group's or a
	// project's hooks fails the whole pass. That is the wrong verdict for
	// the same reason [Result.NoIngress] is reported rather than raised —
	// no retry fixes it — but changing it also changes what
	// `crewlet gitlab provision` prints and the status it exits with, so it
	// is a coordinated change rather than a line here.
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
			Detail: fmt.Sprintf("the group Owner token this integration runs on "+
				"expires on %s, and from then on no pass can provision or repair "+
				"an agent", r.CredentialExpires.Format(time.DateOnly)),
			Remedy: "create a new personal access token with the api scope for " +
				"a group owner at GitLab and store it as the integration's " +
				"admin token",
			ExpiresAt: r.CredentialExpires,
		})
	}

	return out
}
