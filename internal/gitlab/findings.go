package gitlab

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
)

// Findings reads this run as the integration-neutral vocabulary.
//
// See jira.Result.Findings for why the mapping lives here and the ordering
// does not. What differs on this host is that a run CREATES things: a service
// account per seat, a token on each, a webhook. So the interesting states are
// not "what is missing" but "what did this run leave in a state a person has
// to look at", and there are only two of those.
//
// A seat this run KEPT is not a finding. Keeping is the successful outcome of
// a re-run, and reporting it as anything would make every converged company
// look like it had work outstanding.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding

	// A RUN THAT REGISTERED NO WEBHOOK, having been asked to. Empty
	// HookedOn with a non-empty target is the shape a refused registration
	// leaves: the run reached the instance, tried, and the credential
	// could not create one.
	if r.Hooked != "" && len(r.HookedOn) == 0 {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: r.Hooked,
			Detail: fmt.Sprintf(
				"no webhook was registered for %s, so its events reach nobody: "+
					"the credential may not administer the group or its projects",
				r.Hooked),
		})
	}

	// A SEAT WITH NO ACCOUNT AND NO TOKEN. It is in the plan, so the
	// company expects it to act on GitLab, and this run neither created
	// nor kept it, which means it cannot authenticate as itself.
	for _, handle := range r.Decommissioned {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIdentityMissing,
			Subject: handle,
			Detail: fmt.Sprintf(
				"%s no longer has a GitLab service account, so nothing it does "+
					"on the instance is attributable to it", handle),
		})
	}
	return out
}
