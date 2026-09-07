package github

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
)

// Findings reads this run as the integration-neutral vocabulary.
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
		// OPTIONAL, NOT MISSING, and the difference is the whole card.
		//
		// Each agent acts through its OWN app now, so this token is not
		// what gives an agent an identity: it reads who else is taking
		// part in a thread. Reported as credential_missing it put the
		// integration in Failed, which is the phase for one that cannot
		// be talked to at all, and sent an operator looking for an outage
		// rather than at a line saying what they would gain by adding it.
		out = append(out, integration.Finding{
			Kind: integration.FindingOptionalMissing,
			Detail: "no organization read token is set, so a thread's other " +
				"participants are not looked up: an agent hears about work it " +
				"is assigned or mentioned in, and not about work it is merely " +
				"watching",
		})
	}

	// A TARGET THIS RUN TRIED AND COULD NOT HOOK, and only that.
	//
	// An EMPTY hook list is deliberately not a finding, because it is what
	// a read-only pass produces by construction: with no public base URL
	// the run registers nothing and reports nothing, and reading that as
	// "no webhook target is registered" would park every company on a
	// block nobody can clear. That URL is not on the integrations block
	// today (every integration subcommand takes it as -public-url), so ingress
	// convergence stays with the subcommand that has it.
	//
	// A target that WAS attempted and refused is a different fact, and it
	// is reported per target rather than once: a partial hook-up is the
	// state this third-party app actually reaches, an org hook the credential may
	// not create beside repositories it may.
	for _, hook := range r.Hooks {
		if hook.Hooked() {
			continue
		}
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

// detailOr falls back to a sentence when the third-party app gave none, so a finding
// never renders as a bare subject with a colon after it.
func detailOr(detail, fallback string) string {
	if detail != "" {
		return detail
	}
	return fallback
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error.
//
// The same accessor GitLab and Mattermost export, for the same reason: a
// caller deciding what a refusal MEANS needs the number, and the meaning is
// decided once, in [integration.Reject], rather than per integration.
func Status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}
