package jira

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Findings reads this run as the integration-neutral vocabulary.
//
// # Why the mapping lives here and the ordering does not
//
// What a [ProjectCheck] with Exists false MEANS is Jira's own business: it is
// a project key in the company document that this instance does not have, and
// only this package knows that is almost always a typo rather than a
// permission problem. Which of several such observations an operator is shown
// FIRST is not: that is a comparison across third-party apps, and five hand-written
// versions of it had already drifted (see integration.FindingKind.severity).
// So this says what it found and [integration.Classify] says what it means.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding

	// INGRESS. [Result.Hooked] is the webhook this RUN registered, empty in
	// three unlike states: a read-only pass; a working CLOUD company (its
	// webhook belongs to an app, events arrive via the Forge relay); a Data
	// Center instance with no address.
	//
	// So [Result.NoIngress] is the question: whether the pass HAD an address,
	// on the one deployment where a hook is how events arrive. Everything
	// else stays silent, since reading a healthy Cloud company as unhooked
	// would park it on a block nobody can clear. The finding names the Tier A
	// key the address was READ from (`api.public.external_url` or
	// `api.external_url`), which only the caller can say
	// ([Result.NoIngressKey]).
	if r.NoIngress != "" {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: setup.BaseKey(r.NoIngressKey),
			Detail:  r.NoIngress,
		})
	}

	for _, seat := range r.Seats {
		if seat.Routes() {
			continue
		}
		// THE ORGANIZATION'S GRANT STILL LANDING IS NOT A BROKEN SEAT.
		//
		// Atlassian accepts a grant immediately and applies it over about a
		// minute, and for that minute Jira answers its own 401 for the
		// credential this engine has just minted — which is exactly what a
		// wrong credential looks like. Reported as one, the card said
		// "Action required, you, at the third-party app" about a seat that
		// was working a minute later, and said it on EVERY reconnect,
		// because a reconnect creates a new account. See
		// [SeatIdentity.Activating] and [atlassian.GrantPropagation].
		if seat.Activating {
			out = append(out, integration.Finding{
				Kind:    integration.FindingGrantPending,
				Subject: seat.Handle,
				Detail: fmt.Sprintf(
					"Atlassian is still giving %s's new account access to Jira, "+
						"so the instance refuses its credential for about a "+
						"minute. Nothing has to be done; the next pass checks again",
					seat.Handle),
			})
			continue
		}
		// A seat with no usable account receives NO Jira events at all,
		// which is the one finding this command exists to surface.
		//
		// AN ADMIN'S, and that is still right for everything that reaches
		// here: a credential the instance refuses outside the grant window
		// is one somebody has to look at, and a seat with no credential on
		// a company whose Atlassian organization is not provisioning is a
		// person's to supply. (This used to say "nothing here can create an
		// Atlassian account", which internal/atlassian has done since the
		// loop started provisioning — and [integration.ConvergeOrder] runs
		// it FIRST precisely so it can.)
		out = append(out, integration.Finding{
			Kind:    integration.FindingIdentityFailed,
			Subject: seat.Handle,
			Detail: fmt.Sprintf("%s has no Jira account, so no issue reaches it: %s",
				seat.Handle, reasonOr(seat.Reason, "its credential resolved to nothing")),
		})
	}

	for _, project := range r.Projects {
		// ONE CASE, so an `if`. It was a switch over three arms until a
		// project read that failed stopped being a finding at all and
		// became the fault it is — see checkProjects.
		if !project.Exists {
			// BOTH HALVES OF THE 404: Jira answers the same status for a
			// project that is not there and for one this credential may
			// not browse (see [github.ensureRepoWebhook]). Told only the
			// first half, an operator hunts a typo in a key they can see.
			//
			// NOT AN ACCESS TIER: FindingUnknownTier's closed-set doc and
			// fallback sentence are about tiers, not project keys. A
			// routing path that will not fix itself, owed by the admin who
			// can grant permission or correct the key, is
			// FindingIngressBlocked.
			out = append(out, integration.Finding{
				Kind:    integration.FindingIngressBlocked,
				Subject: project.Key,
				Detail: fmt.Sprintf(
					"the company names Jira project %s and this instance "+
						"answered 404 for it, so every issue routed by that "+
						"project reaches nobody. Jira answers 404 both for a "+
						"project that does not exist and for one the "+
						"credential may not see, so check the key and check "+
						"that integrations.jira.token's account has Browse "+
						"Projects on it", project.Key),
			})
		}
	}
	return out
}

// reasonOr falls back to a sentence when the third-party app gave none, so a finding
// never renders as a bare handle with a colon after it.
func reasonOr(reason, fallback string) string {
	if reason != "" {
		return reason
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
