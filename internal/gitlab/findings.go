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
//
// NOR IS A SEAT THIS RUN DECOMMISSIONED. That branch existed and reported
// each deleted account as [integration.FindingIdentityMissing], under a
// comment describing "a seat with no account AND NO TOKEN… it is in the
// plan". [Result.Decommissioned] is exactly the seats that are NOT in the
// plan: an operator passed -decommission and the run deleted the accounts of
// seats the company document no longer has. Reporting them made a successful
// destructive run classify as PhaseProvisioning/ActorEngine — "creating agent
// identities" — for handles that will never be created, on a row nothing
// clears.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	var out []integration.Finding

	// THE PASS COULD NOT RUN AT ALL, and this says so as the operator's
	// work rather than as a fault the engine is retrying. Provisioning
	// here creates an ACCOUNT and then a token on it, so a node with
	// nowhere to seal the token must not create the account either — see
	// [provision.CanMint].
	if r.NoKeyring {
		return append(out, integration.Finding{
			Kind:    integration.FindingCredentialMissing,
			Subject: "secrets.keys",
			Detail: "this node has no keyring, so a token minted for a seat " +
				"could not be sealed and no account was created — set " +
				"secrets.keys in the bootstrap configuration",
		})
	}

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
	// the same reason [Result.NoKeyring] is reported rather than raised —
	// no retry fixes it — but changing it also changes what
	// `crewlet gitlab provision` prints and the status it exits with, so it
	// is a coordinated change rather than a line here.
	if r.NoIngress != "" {
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: "integrations.public_base_url",
			Detail:  r.NoIngress,
		})
	}

	// A SEAT WHOSE ACCOUNT CANNOT AUTHENTICATE, which is the third state
	// this pass can leave and the one it used to express by minting another
	// token every few seconds instead of saying anything.
	//
	// identity_failed is the kind, because that is literally its definition —
	// "a seat's account could not be created or its credential was refused" —
	// and because the verdict it carries is the true one: DEGRADED, owed by
	// an ADMIN at GitLab. Nobody here can fix it and no retry will.
	// AN ACCOUNT GITLAB WILL NOT LET SIGN IN, named before the one that
	// signed in and was refused: they read alike on a card and have
	// different remedies, and this one is what an ordinary
	// disconnect-then-reconnect leaves behind, because GitLab's
	// service-account delete blocks rather than erases.
	//
	// identity_failed, and owed by an ADMIN: nothing this engine holds can
	// undo it. Unblocking is an instance-admin route and the ordinary
	// deployment provisions with a group Owner token, so the honest answer
	// is the account's name, the state GitLab reports, and the page where
	// somebody who can change it goes.
	for _, account := range r.Blocked {
		out = append(out, integration.Finding{
			Kind:      integration.FindingIdentityFailed,
			Subject:   account.Handle,
			ActionURL: r.AccountsURL,
			Detail: fmt.Sprintf(
				"%s's GitLab account %s is %s, so it can sign in nowhere and "+
					"holds no usable credential. A previous disconnect blocks "+
					"the account rather than deleting it, which is the usual "+
					"cause; unblock it at GitLab to bring this agent back",
				account.Handle, account.Username, account.State),
		})
	}

	for _, handle := range r.Unusable {
		out = append(out, integration.Finding{
			Kind: integration.FindingIdentityFailed, Subject: handle,
			Detail: handle + " has a GitLab account that cannot authenticate: a " +
				"token minted for it seconds earlier was refused, so it holds " +
				"no credential and receives no code-host work. Check the " +
				"account at GitLab — an unconfirmed address is the usual " +
				"cause — or delete it and let the next pass recreate it",
		})
	}
	return out
}
