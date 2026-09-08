package github_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/github"
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

// THIS FORM ASKS FOR NO PERSONAL ACCESS TOKEN, at any point.
//
// It led the form once, then it was optional; now nothing here wants it. Its
// one job in routing was the list of who is participating in a thread, and
// the agents' own apps answer that, scoped to what each may see rather than
// to whatever the person who minted the token could reach. Asking every
// company for one bought a permanent note on a card with nothing wrong with
// it.
//
// The organization stays REQUIRED, and the contrast is the point: without it
// the engine does not know where to install an app or register a hook.
func TestTheConnectFormAsksForNoPersonalAccessToken(t *testing.T) {
	t.Parallel()
	reqs := byField(github.Requirements(nil, nil))

	if token, listed := reqs["token"]; listed {
		t.Errorf("the form asks for %q, and no agent acts through it: each has "+
			"its own app, and those answer the one question this was read for",
			token.Label)
	}
	if !reqs["provisioning.org"].Required {
		t.Error("the organization is optional, so a company could connect GitHub " +
			"without saying where its repositories are")
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
