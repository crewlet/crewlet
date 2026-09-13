package github_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// byField indexes a requirement list, so a case names the field it is about
// rather than a position that moves whenever the list grows.
func byField(reqs []setup.Requirement) map[string]setup.Requirement {
	out := map[string]setup.Requirement{}
	for _, r := range reqs {
		out[r.Field] = r
	}
	return out
}

// THE CONNECT FORM LEADS WITH THE ORGANIZATION, NOT WITH A HAND-MINTED TOKEN.
//
// Connect is an ordering: the fields that ESTABLISH the connection come
// first. The organization is the one answer only the operator has, and every
// credential an agent uses comes from that agent's own app, which the engine
// writes. The org token establishes nothing, so a form that opened with it
// asked for a personal access token before anything could be connected at
// all, for a credential no agent ever acts as.
func TestOnlyTheOrganizationLeadsTheConnectForm(t *testing.T) {
	t.Parallel()
	var leading []string
	for _, r := range github.Requirements(nil, nil) {
		if r.Connect {
			leading = append(leading, r.Field)
		}
	}
	if len(leading) != 1 || leading[0] != "provisioning.org" {
		t.Fatalf("the connect form opens with %v, want the organization alone", leading)
	}
}

// THE ORG TOKEN IS OFFERED, AND NEVER ON THE WAY IN.
//
// It led the connect form once, for a job it no longer has: participant
// fan-out is answered by each agent's own app now, scoped to what that agent
// may see. Asking every company for a personal access token before anything
// could be connected bought a permanent note on a card with nothing wrong
// with it, and that half of the decision stands — this pins it, through
// [TestOnlyTheOrganizationLeadsTheConnectForm] beside it.
//
// Removing the field entirely went one step too far. It kept its OTHER job,
// which is the whole organization-level client: with none, the pass registers
// nothing at GitHub, and the finding that says so names
// `integrations.github.token` — a field the dashboard then had no box for. So
// an operator installed every agent's app, which is what the card had been
// asking for, and watched the same warning sit there, because an App
// installation carries no `admin:org_hook` and never will.
//
// Optional, off the connect step, and declaring the finding it clears, so the
// screen puts it in front of whoever is reading that warning and nobody else.
func TestTheOrgTokenIsAskableWithoutLeadingTheForm(t *testing.T) {
	t.Parallel()
	reqs := byField(github.Requirements(nil, nil))

	token, listed := reqs["token"]
	if !listed {
		t.Fatal("the form offers no organization token, so the finding that " +
			"names one sends an operator to a field this screen cannot set")
	}
	if token.Required {
		t.Error("the organization token is required, so a company hooking each " +
			"agent's own app cannot connect without a credential it never uses")
	}
	if token.Connect {
		t.Error("the organization token is back on the connect step, which is " +
			"a personal access token demanded before anything works")
	}
	// THE JOIN IS WHAT PUTS IT ON THE RIGHT SCREEN. Without it the field is
	// offered to everybody and to nobody in particular, which is the same
	// as not offering it to the person reading the warning.
	if token.Blocks != integration.FindingIngressBlocked {
		t.Errorf("the token blocks %q, so the ingress finding that names it "+
			"offers no field that clears it", token.Blocks)
	}
	if !reqs["provisioning.org"].Required {
		t.Error("the organization is optional, so a company could connect GitHub " +
			"without saying where its repositories are")
	}
}

// AND THE HOOK CHOICE OPENS ON ONE ORGANIZATION HOOK.
//
// It covers repositories created after the run, which is the difference
// between a new repository routing on day one and routing whenever somebody
// remembers. `true` rather than `auto` because the two differ only in what
// happens when the hook cannot be made, and a silent fall back to
// per-repository hooks leaves a company believing it has one hook when it has
// several.
//
// Affordable only because the token above is askable: defaulting to `true`
// over a form with no box for it would put a permanent finding on every fresh
// connect, which is the bug noRegistrarReason was narrowed to remove.
func TestTheHookChoiceDefaultsToOneOrganizationHook(t *testing.T) {
	t.Parallel()
	reqs := byField(github.Requirements(nil, nil))
	hook := reqs["provisioning.org_webhook"]
	if hook.Default != "true" {
		t.Errorf("the hook choice opens on %q, want one organization hook",
			hook.Default)
	}
	// AND THE DEFAULT IS ONE OF THE OFFERED VALUES, or the form opens on a
	// selection that is not in its own list.
	var offered bool
	for _, c := range hook.Choices {
		if c.Value == hook.Default {
			offered = true
		}
	}
	if !offered {
		t.Errorf("the default %q is not among the choices %v", hook.Default, hook.Choices)
	}
}

// THE FIELDS THAT WERE ALREADY WORKING STILL WORK. Dropping the token changes
// nothing else: the secret the edge verifies with is still required and still
// mintable, the Enterprise address is still optional, and the hook mode still
// offers the config package's own closed set.
func TestDroppingTheTokenLeavesTheOtherFieldsAlone(t *testing.T) {
	t.Parallel()
	reqs := byField(github.Requirements(nil, nil))

	secret := reqs["webhook_secret"]
	if !secret.Required || !secret.Mintable {
		t.Errorf("the webhook secret is required=%v mintable=%v: a route with "+
			"nothing to verify against answers 503, and nobody should be asked "+
			"to invent the value", secret.Required, secret.Mintable)
	}
	if url, ok := reqs["url"]; !ok || url.Required || url.Connect {
		t.Errorf("the instance address is %+v, want an optional field that does "+
			"not ask an Enterprise question of everyone", url)
	}
	mode, ok := reqs["provisioning.org_webhook"]
	if !ok || len(mode.Choices) == 0 {
		t.Fatal("the hook mode offers no choices, so a form would collect free text " +
			"the config validator refuses")
	}
	for _, choice := range mode.Choices {
		if choice.Value == "" || choice.Label == "" {
			t.Errorf("a hook mode renders as %+v", choice)
		}
	}
}
