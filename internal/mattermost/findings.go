package mattermost

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
)

// Findings reads this run as the vendor-neutral vocabulary.
//
// SHORT, and that is the vendor rather than an omission. Mattermost holds one
// outbound websocket per seat and verifies no inbound delivery, so there is no
// ingress to judge and no webhook to be missing: every finding this host can
// produce is about a seat's own identity.
//
// A seat this run KEPT is not a finding, for the reason every other vendor's
// mapping states: keeping is the successful outcome of a re-run, and reporting
// it would make a converged company look like it had work outstanding.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding
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
