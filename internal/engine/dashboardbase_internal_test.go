package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A LINK FOR A PERSON IS BUILT ON THE DASHBOARD'S ADDRESS, never the vendors'.
// Behind a public listener (Tier A api.public) public_base_url is a socket that
// serves the webhooks and answers the dashboard 404, so a skill's
// ${crewlet_base_url} and a page change's link composed on it would land every
// person on nothing — while a webhook registered on the dashboard's address
// would land every delivery on the 404 the other way round.
func TestPeopleAreLinkedToTheDashboardAndVendorsToThePublicBase(t *testing.T) {
	t.Parallel()
	env := config.NewResolver(config.MapSource{"DASHBOARD_URL": "https://crewlet.example.com/"})
	c := &Company{Config: &config.Company{Integrations: config.Integrations{
		PublicBaseURL:    "https://hooks.example.com",
		DashboardBaseURL: "${DASHBOARD_URL}",
	}}}

	if got := skillVariables(env, c)[config.ReservedBaseURLVariable]; got != "https://crewlet.example.com" {
		t.Errorf("${%s} = %q, want the dashboard's address", config.ReservedBaseURLVariable, got)
	}
	if got := pagesParserOptions(env, c).BaseURL; got != "https://crewlet.example.com" {
		t.Errorf("a page change links on %q, want the dashboard's address", got)
	}
	if got := c.Config.Integrations.WebhookBase(env.LookupOK); got != "https://hooks.example.com" {
		t.Errorf("a webhook is registered on %q, want the vendors' address", got)
	}
}

// NO DASHBOARD ADDRESS IS NO LINK, never the vendors' address borrowed: that is
// the wrong one exactly where the two differ, and a skill rendering
// ${crewlet_base_url} literally is the registry's signal that it is undefined.
func TestNoDashboardBaseComposesNoLink(t *testing.T) {
	t.Parallel()
	env := config.NewResolver()
	c := &Company{Config: &config.Company{Integrations: config.Integrations{
		PublicBaseURL: "https://hooks.example.com",
	}}}
	if vars := skillVariables(env, c); vars != nil {
		t.Errorf("skill variables = %v, want none: nothing names the dashboard", vars)
	}
	if got := pagesParserOptions(env, c).BaseURL; got != "" {
		t.Errorf("a page change links on %q, want no link", got)
	}
}
