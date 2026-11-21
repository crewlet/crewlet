package github

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
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

	// NOTHING IS REPORTED FOR AN ABSENT ORG CREDENTIAL, and that is the
	// end of a road this finding was the last stretch of.
	//
	// It said participant fan-out was off, which was true while the lookup
	// needed a token an operator pasted in. The agents' own apps answer
	// that question now ([SeatLookup]), scoped to what each may see, so
	// there is nothing an org token adds to routing and no degradation to
	// report. What is left for it is the ORGANIZATION-level reconcile: a
	// company that wants one org-wide hook sets it and this run reads;
	// every other company never had a reason for one, and telling them
	// all, on every pass, that something optional was missing put a
	// permanent note on a card with nothing wrong with it.

	// NO ADDRESS TO DELIVER TO IS ONE FINDING, said once.
	//
	// An empty hook list is not silence: the address is Tier A's webhook
	// address, which every pass is fed, so silence would report GitHub Ready
	// while nothing at GitHub pointed anywhere, the state this subsystem
	// exists to refuse.
	//
	// ONCE rather than per target: no address means no hook anywhere, and one
	// sentence naming the field the address was READ from is the whole of
	// what an operator has to do. Only the caller holding the Tier A can say
	// which ([Result.NoIngressKey]).
	if r.NoIngress != "" {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: setup.BaseKey(r.NoIngressKey),
			Detail:  r.NoIngress,
		})
	}

	// AN ADDRESS GITHUB CANNOT REACH IS AN ADVISORY, said once: what does
	// not arrive, and what makes it arrive. See [Result.Undeliverable].
	if r.Undeliverable.Kind != "" {
		out = append(out, r.Undeliverable)
	}

	// AND NO CREDENTIAL TO REGISTER THE HOOK THE COMPANY ASKED FOR.
	//
	// The organization credential is optional and the form does not ask for
	// it, both deliberately — routing needs nothing from it, because each
	// agent's own app answers who is participating. What it IS still needed
	// for is the one thing `provisioning` asks for: a hook on an
	// organization or on a list of repositories. Without it the pass reads
	// nothing and writes nothing, and it said so in NOTES, which are not
	// findings — so a surface that had authenticated with nobody reported
	// READY with an empty finding list. See [Result.NoRegistrar].
	if r.NoRegistrar != "" {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: "integrations.github.token",
			Detail:  r.NoRegistrar,
		})
	}

	// WHAT THE COMPANY'S AGENTS HEAR ABOUT, where that is narrower than the
	// organization and is the ARRANGEMENT rather than a fault.
	//
	// An advisory, so the surface stays READY: these agents are receiving
	// events. It was reported as ingress_blocked once, which put a card
	// delivering events perfectly well at Action required and named the
	// organization token as the fix — beside a roster asking for apps to be
	// installed, which can never carry `admin:org_hook`. So the one
	// instruction on screen could not clear the one warning on screen.
	if r.Coverage.Kind != "" {
		out = append(out, r.Coverage)
	}

	// AND A TARGET THIS RUN TRIED AND COULD NOT HOOK.
	//
	// Reported per target rather than once: a partial hook-up is the state
	// this third-party app actually reaches, an org hook the credential may
	// not create beside repositories it may.
	for _, hook := range r.Hooks {
		// A SKIPPED TARGET IS NOT A BLOCK. An archived repository emits
		// no events, so it has nothing to hook rather than a hook that
		// failed — see [HookOutcome]. Reported as a block it parked the
		// company in PhaseDegraded, retried on the admin backoff for
		// ever, over a repository that is finished.
		if !hook.Blocks() {
			continue
		}
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: hook.Target.String(),
			Detail: fmt.Sprintf("no webhook on %s, so its events reach nobody: %s",
				hook.Target, detailOr(hook.Detail, "the credential may not register one")),
		})
	}

	// AND A SEAT WHOSE OWN CREDENTIAL DOES NOT AUTHENTICATE.
	//
	// ONLY THAT SEAT. Each agent normally acts as its OWN GitHub App with no
	// `mcp_env.github` token by design, and its mentions resolve through the
	// app's slug ([SeatLookup], [BotLogin]), so a seat with no login is not
	// degraded unless it asked for a credential.
	//
	// What a personal access token buys a seat is that its TOOLS act as
	// somebody, so that is what a refused one costs. Claiming the seat is
	// unreachable would be false of one with an app, and a seat with no app
	// either is the access sync's to report.
	for _, seat := range r.Seats {
		if !seat.Refused() {
			continue
		}
		out = append(out, integration.Finding{
			Kind:    integration.FindingIdentityFailed,
			Subject: seat.Handle,
			Detail: fmt.Sprintf("%s's own GitHub credential does not authenticate, "+
				"so every call its tools make is refused and anything routed on "+
				"that account reaches nobody: %s",
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
