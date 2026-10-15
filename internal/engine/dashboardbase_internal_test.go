package engine

import (
	"errors"
	"strings"
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
	if got, err := c.Config.Integrations.WebhookBase(env.LookupOK); got != "https://hooks.example.com" || err != nil {
		t.Errorf("a webhook is registered on %q (%v), want the vendors' address", got, err)
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

// A DASHBOARD BASE THE COMPANY NAMES AND THIS NODE CANNOT USE composes no link
// either — a variable holding a query would otherwise put
// `https://crewlet.example.com?tenant=a/work/ENG-1` in front of a person — and
// says why, naming the variable and never the value, which is what the apply's
// dashboard_base_unusable warning carries.
func TestAnUnusableDashboardBaseComposesNoLinkAndSaysWhy(t *testing.T) {
	t.Parallel()
	env := config.NewResolver(config.MapSource{"DASHBOARD_URL": "https://crewlet.example.com?tenant=sk-secret"})
	c := &Company{Config: &config.Company{Integrations: config.Integrations{DashboardBaseURL: "${DASHBOARD_URL}"}}}
	if vars := skillVariables(env, c); vars != nil {
		t.Errorf("skill variables = %v, want none: the dashboard base is no address", vars)
	}
	if got := pagesParserOptions(env, c).BaseURL; got != "" {
		t.Errorf("a page change links on %q, want no link", got)
	}
	_, err := dashboardBase(env, c)
	var base *config.BaseError
	if !errors.As(err, &base) || !strings.Contains(err.Error(), "DASHBOARD_URL") || strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("dashboardBase error = %v, want a BaseError naming DASHBOARD_URL and not the value", err)
	}
}

// A DISCONNECT REFUSES OVER A BASE IT CANNOT READ. It finds this deployment's
// hooks by their address, so withdrawing by an address nobody can read would
// finish the disconnect over hooks still delivering here; an unset base has
// nothing registered and finishes.
//
// Mutation: hand the teardown "" whatever the error, and the unusable case
// finishes.
func TestATeardownRefusesOverABaseItCannotRead(t *testing.T) {
	t.Parallel()
	unset := &Company{Config: &config.Company{}}
	if base, err := teardownBase(unset, config.NewResolver()); base != "" || err != nil {
		t.Errorf("an unset base: %q, %v; want nothing to withdraw", base, err)
	}
	named := &Company{Config: &config.Company{Integrations: config.Integrations{PublicBaseURL: "${CREWLET_PUBLIC_URL}"}}}
	_, err := teardownBase(named, config.NewResolver())
	if !errors.Is(err, config.ErrBaseUnresolved) || !strings.Contains(err.Error(), "CREWLET_PUBLIC_URL") ||
		!strings.Contains(err.Error(), "force the disconnect") {
		t.Errorf("an unresolved base: %v, want a refusal naming the variable and the way out", err)
	}
}
