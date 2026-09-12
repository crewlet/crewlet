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
	// An empty hook list used to be silence, on the reasoning that a
	// read-only pass produces one by construction and that the public base
	// "is not on the integrations block today". It IS — it is
	// `integrations.public_base_url`, and the reconcile loop feeds exactly
	// that value into every pass — so the silence meant a company that
	// never set it saw GitHub reported Ready while nothing at GitHub
	// pointed anywhere. That is the state this whole subsystem exists to
	// refuse.
	//
	// ONCE rather than per target, because there is nothing per-target
	// about it: no address means no hook anywhere, and one sentence naming
	// the field is the whole of what an operator has to do.
	if r.NoIngress != "" {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: "integrations.public_base_url",
			Detail:  r.NoIngress,
		})
	}

	// AND NO KEY TO SIGN WITH IS THE OTHER WAY TO HAVE NO INGRESS, said
	// against the field that fixes it.
	//
	// INGRESS RATHER THAN CREDENTIAL, on Jira's reasoning: a webhook secret
	// is not how this engine authenticates AT GitHub — it is what makes a
	// delivery verifiable when it arrives here, and a route with nothing to
	// verify with answers 503. It is also what [Requirements] declares:
	// `webhook_secret` says Blocks: FindingIngressBlocked, and that
	// declaration is the join the setup screen uses to offer the field that
	// clears this.
	if r.NoKeyring {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: "integrations.github.webhook_secret",
			Detail: "no webhook was registered because this deployment has no " +
				"secret to sign deliveries with and this node has no keyring " +
				"to seal a fresh one into: set secrets.keys in the bootstrap " +
				"configuration so a pass can mint it, or set the variable " +
				"integrations.github.webhook_secret points at and register the " +
				"hooks on the next pass",
		})
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
	// ONLY THAT SEAT. A seat with no login used to be one finding whether
	// or not it had ever asked for a credential, and on the shape this
	// integration actually ships — each agent acting as its OWN GitHub App,
	// with no `mcp_env.github` token anywhere by design — that was every
	// agent seat, for ever, in [integration.PhaseDegraded], saying "no
	// review request or mention reaches it" about seats whose mentions
	// resolve perfectly well through their app's slug ([SeatLookup] and
	// the identity registration that reads [BotLogin]).
	//
	// The sentence changed with it. What a personal access token actually
	// buys a seat is that its TOOLS act as somebody, so that is what a
	// refused one costs — claiming the seat is unreachable was a second
	// false statement, true only of a seat with no app either, which this
	// run cannot see and [ReconcileSeatApps] reports on.
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
