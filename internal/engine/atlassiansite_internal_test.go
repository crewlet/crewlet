package engine

import (
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

// SETUP SAYS WHAT THE FORGE RELAY IS REFUSING, naming the field, unless it
// has just recorded that product's id. Mutation: drop forgeHolds from
// recordSite's answer, or skip the recorded check, and this goes red.
func TestTheAtlassianPassNamesEachForgeHoldItCannotClear(t *testing.T) {
	t.Parallel()
	in := config.Integrations{
		ForgeAppID: "app-123",
		Jira:       &config.Jira{URL: "https://acme.atlassian.net"},
		Confluence: &config.Confluence{},
	}
	literal := func(v string) string { return v }

	got := forgeHolds(in, literal, map[string]bool{"confluence": true})
	if len(got) != 1 || got[0].Kind != integration.FindingIngressBlocked ||
		got[0].Detail != "Forge relay events for Jira are refused until "+
			"integrations.jira.cloud_id (with site_url) is set" {
		t.Fatalf("findings = %+v, want jira's hold alone", got)
	}
	if got := forgeHolds(in, literal, nil); len(got) != 2 {
		t.Errorf("findings = %+v, want both products held before anything is recorded", got)
	}
	in.ForgeAppID = ""
	if got := forgeHolds(in, literal, nil); len(got) != 0 {
		t.Errorf("findings = %+v with no Forge app id", got)
	}
}
