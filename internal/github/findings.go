package github

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
)

// Findings reads this run as the vendor-neutral vocabulary.
//
// See jira.Result.Findings for why the mapping is here and the ordering is
// not. What differs on this host is the SHAPE OF INGRESS: GitHub hooks a set
// of targets rather than one instance, so a run can land with some of them
// working and some refused, and reporting that as one webhook state would
// call a company healthy because its first repository was hooked.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding

	if r.Login == "" {
		// The run had no org credential to probe. Everything else it
		// reports was read without one, so this is said first and the
		// classifier ranks it above the rest.
		out = append(out, integration.Finding{
			Kind: integration.FindingCredentialMissing,
			Detail: "integrations.github.token resolved to nothing, so participant " +
				"fan-out is off and a thread's watchers hear nothing",
		})
	}

	if len(r.Hooks) == 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingIngressBlocked,
			Detail: "no webhook target is registered, so nothing GitHub does " +
				"reaches an agent. Set the engine's public base URL and re-run " +
				"`crewlet github provision`",
		})
	}
	for _, hook := range r.Hooks {
		if hook.Hooked() {
			continue
		}
		// PER TARGET, because a partial hook-up is the state this vendor
		// actually reaches: an org hook the credential may not create,
		// beside repositories it may.
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: hook.Target.String(),
			Detail: fmt.Sprintf("no webhook on %s, so its events reach nobody: %s",
				hook.Target, detailOr(hook.Detail, "the credential may not register one")),
		})
	}

	for _, seat := range r.Seats {
		if seat.Routes() {
			continue
		}
		out = append(out, integration.Finding{
			Kind:    integration.FindingIdentityFailed,
			Subject: seat.Handle,
			Detail: fmt.Sprintf("%s has no GitHub login, so no review request or "+
				"mention reaches it: %s",
				seat.Handle, detailOr(seat.Reason, "its credential resolved to nothing")),
		})
	}
	return out
}

// detailOr falls back to a sentence when the vendor gave none, so a finding
// never renders as a bare subject with a colon after it.
func detailOr(detail, fallback string) string {
	if detail != "" {
		return detail
	}
	return fallback
}
