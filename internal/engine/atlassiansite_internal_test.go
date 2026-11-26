package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
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
