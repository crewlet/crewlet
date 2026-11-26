package engine

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// A BLOCK THAT NAMES ITS OWN SITE IS LEFT ALONE: config refuses a url and a
// cloud id together, so a patch adding one beside a url fails on every pass.
// Mutation: drop the URL check in sitePatch and the jira case goes red.
func TestTheDiscoveredSiteIsRecordedOnlyWhereNoAddressIsNamed(t *testing.T) {
	t.Parallel()
	in := config.Integrations{
		Jira:       &config.Jira{URL: "https://acme.atlassian.net"},
		Confluence: &config.Confluence{},
	}
	patch := sitePatch(in, atlassian.Site{CloudID: "c1", HostURL: "https://acme.atlassian.net"})
	if _, named := patch["jira"]; named {
		t.Errorf("patch = %v: a cloud id beside jira's url", patch)
	}
	if got := patch["confluence"]; got["cloud_id"] != "c1" || got["site_url"] != "https://acme.atlassian.net/wiki" {
		t.Errorf("confluence patch = %v, want the discovered site", got)
	}
	if patch := sitePatch(in, atlassian.Site{}); len(patch) != 0 {
		t.Errorf("an unknown site patched %v", patch)
	}
}

// EACH PRODUCT'S PASS SAYS WHAT THE FORGE RELAY IS REFUSING, in the words
// every surface uses, and looks a url-named site up as the integration
// starts. Mutation: drop the lookup, report a site whose id was read, or
// report anything without an app id, and a check goes red.
func TestAProductsPassNamesItsForgeHold(t *testing.T) {
	t.Parallel()
	in := config.Integrations{
		ForgeAppID: "app-123",
		Jira:       &config.Jira{URL: "https://down.atlassian.net"},
		Confluence: &config.Confluence{},
	}
	literal := func(v string) string { return v }
	sites := &failingSites{}

	got := forgeFindings(t.Context(), in, literal, sites, "jira")
	want := "Forge relay events for Jira are refused: the site's cloud id could not be read " +
		"from https://down.atlassian.net/_edge/tenant_info (HTTP 502); check the site is " +
		"reachable, or set integrations.jira.cloud_id with site_url in place of url"
	// The title is how the integrations answer finds it in the shared store.
	if len(got) != 1 || got[0].Kind != integration.FindingIngressBlocked ||
		got[0].Title != config.ForgeHoldTitle || got[0].Detail != want {
		t.Fatalf("jira findings = %+v, want the lookup's hold", got)
	}
	if sites.asks != 1 {
		t.Errorf("the pass asked the site %d times, want 1", sites.asks)
	}
	if got := forgeFindings(t.Context(), in, literal, sites, "confluence"); len(got) != 1 ||
		got[0].Detail != "Forge relay events for Confluence are refused until "+
			"integrations.confluence.cloud_id (with site_url) is set" {
		t.Errorf("confluence findings = %+v, want the static hold", got)
	}
	sites.ok = true
	if got := forgeFindings(t.Context(), in, literal, sites, "jira"); len(got) != 0 {
		t.Errorf("findings = %+v for a site whose id was read", got)
	}
	in.ForgeAppID = ""
	if got := forgeFindings(t.Context(), in, literal, sites, "confluence"); len(got) != 0 {
		t.Errorf("findings = %+v with no Forge app id", got)
	}
}

// failingSites answers every lookup with a 502 until ok.
type failingSites struct {
	ok   bool
	asks int
}

func (f *failingSites) Resolve(_ context.Context, site string) (string, error) {
	f.asks++
	if f.ok {
		return "0f3a6c1e-0000-4000-8000-00000000c0de", nil
	}
	return "", &atlassian.TenantInfoError{URL: site + "/_edge/tenant_info", Cause: "HTTP 502"}
}
