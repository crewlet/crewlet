package cloudflare_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/cloudflare"
)

// THE PROVISIONER'S STEPS LIST THE BUILDER'S OWN NAMES, generated from the
// levels: every permission it needs once, account and zone apart, and no
// quoted name, which a form would set as a value rather than a name.
func TestTheProvisionerStepsListTheBuildersNames(t *testing.T) {
	t.Parallel()
	steps := cloudflare.Guide().Prepare
	var account, zone []string
	for _, step := range steps {
		switch step.Text {
		case "Give it these account permissions:":
			account = step.Grants
		case "And these zone permissions, on all zones:":
			zone = step.Grants
		}
	}
	if len(account) == 0 || len(zone) == 0 {
		t.Fatalf("steps = %+v, want an account step and a zone step with their permissions", steps)
	}
	items := append(slices.Clone(account), zone...)
	for _, item := range items {
		if strings.Contains(item, `"`) {
			t.Errorf("a step quotes a name: %q", item)
		}
	}
	want := cloudflare.ProvisionerGroups()
	for i := range want {
		want[i] = cloudflare.DashboardName(want[i])
	}
	slices.Sort(items)
	slices.Sort(want)
	if !slices.Equal(items, want) {
		t.Errorf("the steps list %q, want every provisioner permission once: %q", items, want)
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
	if !slices.Contains(account, "Account API Tokens: Edit") || !slices.Contains(zone, "Cache Purge: Purge") {
		t.Errorf("a group is in the wrong step: account %q, zone %q", account, zone)
	}
	// THE SAME LIST AS PROSE, for a person replacing the token once the steps
	// are put away.
	for _, r := range cloudflare.Requirements(nil, nil) {
		if r.Field != "api_token" {
			continue
		}
		for _, name := range items {
			if !strings.Contains(r.More, name) {
				t.Errorf("the token's More leaves out %q: %q", name, r.More)
			}
		}
	}
}
