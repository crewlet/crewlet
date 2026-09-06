package jira

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
)

// Findings reads this run as the vendor-neutral vocabulary.
//
// # Why the mapping lives here and the ordering does not
//
// What a [ProjectCheck] with Exists false MEANS is Jira's own business: it is
// a project key in the company document that this instance does not have, and
// only this package knows that is almost always a typo rather than a
// permission problem. Which of several such observations an operator is shown
// FIRST is not: that is a comparison across vendors, and five hand-written
// versions of it had already drifted (see integration.FindingKind.severity).
// So this says what it found and [integration.Classify] says what it means.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding

	// INGRESS BEFORE IDENTITIES, which the classifier enforces by rank
	// rather than by the order they are appended here. Appended in this
	// order anyway, so a reader of the raw findings list sees them the way
	// the report will.
	if r.Hooked == "" {
		// A run with no WebhookBase skips registration deliberately
		// rather than guessing a host, and that is a configuration gap
		// rather than a vendor refusal: nothing at Jira will fix it.
		out = append(out, integration.Finding{
			Kind: integration.FindingIngressBlocked,
			Detail: "no webhook is registered on this instance, so nothing " +
				"Jira does reaches an agent. Set the engine's public base URL " +
				"and re-run `crewlet jira provision`",
		})
	}

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

// reasonOr falls back to a sentence when the vendor gave none, so a finding
// never renders as a bare handle with a colon after it.
func reasonOr(reason, fallback string) string {
	if reason != "" {
		return reason
	}
	return fallback
}
