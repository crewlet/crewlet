package cloudflare_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/cloudflare"
)

// THE PROVISIONER'S HELP IS TWO LINES OF THE BUILDER'S OWN NAMES, generated
// from the levels: every permission it needs once, account and zone apart, and
// no quoted name, which the form would set as code.
func TestTheProvisionerHelpListsTheBuildersNames(t *testing.T) {
	t.Parallel()
	var help string
	for _, r := range cloudflare.Requirements(nil, nil) {
		if r.Field == "api_token" {
			help = r.Help
		}
	}
	lines := strings.Split(help, "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], "Account permissions: ") ||
		!strings.HasPrefix(lines[2], "Zone permissions (all zones): ") || lines[3] != "Create one on your" {
		t.Fatalf("help = %q, want a sentence, the account line, the zone line and the link's lead", help)
	}
	if strings.Contains(help, `"`) {
		t.Errorf("the help quotes a name: %q", help)
	}
	// THE LISTED ITEMS, as a person reads them off the two lines.
	var items []string
	for i, prefix := range []string{"Account permissions: ", "Zone permissions (all zones): "} {
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(lines[i+1], prefix), "."), ", ")
		// ONLY THE LIST'S LAST "and" SEPARATES: a name may hold one.
		last := parts[len(parts)-1]
		if at := strings.LastIndex(last, " and "); at >= 0 {
			parts = append(parts[:len(parts)-1], last[:at], last[at+len(" and "):])
		}
		items = append(items, parts...)
	}
	want := cloudflare.ProvisionerGroups()
	for i := range want {
		want[i] = cloudflare.DashboardName(want[i])
	}
	slices.Sort(items)
	slices.Sort(want)
	if !slices.Equal(items, want) {
		t.Errorf("the help lists %q, want every provisioner permission once: %q", items, want)
	}
	for name, want := range map[string]string{
		"Account API Tokens Write": "Account API Tokens: Edit", "Zone Read": "Zone: Read",
		"Cache Purge": "Cache Purge: Purge", "SSL and Certificates Read": "SSL and Certificates: Read",
		// THE BUILDER RENAMES the resource, so no rule derives it from the API name.
		"Cache Settings Read": "Cache Rules: Read", "Cache Settings Write": "Cache Rules: Edit",
	} {
		if got := cloudflare.DashboardName(name); got != want {
			t.Errorf("DashboardName(%q) = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(lines[1], "Account API Tokens: Edit") || !strings.Contains(lines[2], "Cache Purge: Purge") {
		t.Errorf("a group is on the wrong line: %q", lines[1:3])
	}
}
