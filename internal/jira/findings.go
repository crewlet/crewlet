package jira

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
)

// Findings reads this run as the third-party app-neutral vocabulary.
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

	// # NOTHING IS SAID ABOUT INGRESS, and the silence is deliberate
	//
	// [Result.Hooked] is the webhook this RUN registered, not the one the
	// instance holds, and it is empty in two states that are nothing like
	// each other. A read-only pass registers nothing by construction, so
	// it is always empty there. And on Cloud it is always empty even for a
	// fully working company, because a Cloud webhook belongs to an app
	// rather than to an API token and those events arrive through the
	// Forge relay instead.
	//
	// Reading either as "no webhook is registered" would park a healthy
	// integration on a block nobody can clear. Answering honestly needs
	// the instance's own hook list compared against this deployment's
	// public base URL, and that URL is not on the integrations block at
	// all today: every third-party app subcommand takes it as -public-url. So this
	// reports what a read of the instance can actually establish, and
	// ingress stays with the subcommand that has the URL.

	for _, seat := range r.Seats {
		if seat.Routes() {
			continue
		}
		// A seat with no account receives NO Jira events at all, which is
		// the one finding this command exists to surface. It is the
		// ENGINE's own work only in the sense that a credential is
		// missing; nothing here can create an Atlassian account, so the
		// reason carries what a person has to do.
		out = append(out, integration.Finding{
			Kind:    integration.FindingIdentityFailed,
			Subject: seat.Handle,
			Detail: fmt.Sprintf("%s has no Jira account, so no issue reaches it: %s",
				seat.Handle, reasonOr(seat.Reason, "its credential resolved to nothing")),
		})
	}

	for _, project := range r.Projects {
		switch {
		case project.Detail != "":
			// COULD NOT BE READ is not the same as does not exist, and
			// the two get different kinds: a read that failed is
			// something the next pass may well succeed at.
			out = append(out, integration.Finding{
				Kind:    integration.FindingGrantPending,
				Subject: project.Key,
				Detail: fmt.Sprintf("could not read project %s: %s",
					project.Key, project.Detail),
			})
		case !project.Exists:
			out = append(out, integration.Finding{
				Kind:    integration.FindingUnknownTier,
				Subject: project.Key,
				Detail: fmt.Sprintf(
					"the company names Jira project %s and this instance does "+
						"not have it, so every issue routed by that project "+
						"reaches nobody", project.Key),
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
// decided once, in [integration.Reject], rather than per third-party app.
func Status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}
