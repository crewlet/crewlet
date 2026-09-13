package github_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
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

// THE FORM ASKS ABOUT COVERAGE, AND OPENS ON THE ANSWER THAT NEEDS NOTHING.
//
// It asked about registration topology — "Organization hook, falling back to
// each repository" / "Organization hook only" / "One hook per repository" —
// and opened on the strictest of the three, which demands a scope most
// connectors do not have. So the ordinary dashboard connect produced Action
// required at once, and the card's own instruction (install each agent's app)
// could never clear it, because no app carries `admin:org_hook`.
//
// Two answers now, named by what they COVER. The first is the arrangement
// most companies are already in and needs nothing further; the second is the
// one that needs the token, and says so.
func TestTheCoverageChoiceOpensOnWhatNeedsNothing(t *testing.T) {
	t.Parallel()
	hook := byField(github.Requirements(nil, nil))["provisioning.org_webhook"]

	if hook.Default != string(config.ContainerWebhookNever) {
		t.Errorf("the coverage choice opens on %q, want the arrangement that "+
			"needs no further credential", hook.Default)
	}
	if len(hook.Choices) != 2 {
		t.Fatalf("the form offers %d answers: %+v", len(hook.Choices), hook.Choices)
	}
	// `auto` IS NOT AN ANSWER TO THIS QUESTION. It means "try for the whole
	// organization and quietly take less", which leaves a company believing
	// it has one hook when it has several.
	for _, c := range hook.Choices {
		if c.Value == string(config.ContainerWebhookAuto) {
			t.Errorf("the form offers %q, whose outcome a person cannot "+
				"predict from the answer they gave", c.Value)
		}
	}
	var offered bool
	for _, c := range hook.Choices {
		if c.Value == hook.Default {
			offered = true
		}
	}
	if !offered {
		t.Errorf("the default %q is not among the choices %+v", hook.Default, hook.Choices)
	}
}

// AND THE CONFIG KEEPS ALL THREE, which is the half a narrowed form must not
// take with it.
//
// `auto` and a `repos` list under `false` are real arrangements that a YAML
// company may hold and the pass still carries out. The form dropping a
// question is a decision about what is worth asking somebody connecting from
// a dashboard; it is not a decision about what the engine supports. And the
// enum is SHARED — GitLab's `group_webhook` validates against the same list
// and its reconcile branches on the same constants — so narrowing it here
// would silently change another vendor.
func TestNarrowingTheFormDoesNotNarrowTheConfig(t *testing.T) {
	t.Parallel()
	want := []config.ContainerWebhookMode{
		config.ContainerWebhookAuto,
		config.ContainerWebhookRequire,
		config.ContainerWebhookNever,
	}
	if !slices.Equal(config.ContainerWebhookModes, want) {
		t.Errorf("ContainerWebhookModes = %v, want %v: GitLab validates its "+
			"own group_webhook against this list", config.ContainerWebhookModes, want)
	}
}

// THE ORGANIZATION-WIDE ANSWER REQUIRES THE TOKEN, AND ONLY THAT ANSWER.
//
// Neither Required nor optional says this. Required blocked a connect that
// needed nothing; optional let the API store a choice it could not carry out
// — a company one apply later demanding an organization hook with nothing to
// register it with.
func TestTheOrgWideAnswerRequiresTheToken(t *testing.T) {
	t.Parallel()
	reqs := github.Requirements(nil, nil)
	token := byField(reqs)["token"]

	if token.RequiredWhen == nil {
		t.Fatal("the organization token is gated on nothing, so it is either " +
			"demanded of every connect or never demanded at all")
	}
	if token.RequiredWhen.Field != "provisioning.org_webhook" ||
		token.RequiredWhen.Equals != string(config.ContainerWebhookRequire) {
		t.Errorf("the token is gated on %+v, want the organization-wide "+
			"coverage answer", token.RequiredWhen)
	}
	// AND THE GATE IS SHUT ON THE DEFAULT ANSWER, which is the whole point:
	// a company taking the recommendation is asked for no credential.
	if setup.Requirement.Needed(token, reqs) {
		t.Error("the token is required of a company that has chosen nothing, " +
			"so the recommended arrangement cannot be connected without one")
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
