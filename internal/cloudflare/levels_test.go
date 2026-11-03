package cloudflare_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
)

// catalogueOf is the fake account's groups as the client reads them.
func catalogueOf(f *fakeCloudflare) []cloudflare.PermissionGroup {
	var out []cloudflare.PermissionGroup
	for _, g := range f.groups {
		out = append(out, cloudflare.PermissionGroup{ID: g.ID, Name: g.Name, Scopes: g.Scopes,
			Selectable: g.Selectable == nil || *g.Selectable})
	}
	return out
}

// EVERY LEVEL THE DOCUMENT OFFERS GRANTS GROUPS, each grants everything the
// one before it does, configure adds the zone settings and compression rules,
// no level writes DNS, WAF or SSL, and the provisioner is asked for every group
// a level grants.
func TestEveryLevelGrantsAndTheProvisionerHoldsThemAll(t *testing.T) {
	t.Parallel()
	if !slices.Equal(cloudflare.Levels(), config.CloudflareLevels()) {
		t.Fatalf("levels %v, the document offers %v", cloudflare.Levels(), config.CloudflareLevels())
	}
	prov := cloudflare.ProvisionerGroups()
	var below []string
	for _, level := range cloudflare.Levels() {
		groups, ok := cloudflare.Groups(level)
		if !ok || len(groups) <= len(below) {
			t.Fatalf("%s grants %v, which adds nothing to %v", level, groups, below)
		}
		for _, g := range below {
			if !slices.Contains(groups, g) {
				t.Errorf("%s does not grant %s, which the level below it does", level, g)
			}
		}
		for _, forbidden := range []string{
			"DNS Write", "Zone WAF Write", "Firewall Services Write", "SSL and Certificates Write", "Page Rules Write",
		} {
			if slices.Contains(groups, forbidden) {
				t.Errorf("%s grants %s, which no level does", level, forbidden)
			}
		}
		for _, g := range groups {
			if !slices.Contains(prov, g) {
				t.Errorf("the provisioner is not asked for %s, which %s grants", g, level)
			}
		}
		if cloudflare.LevelName(level) == level || cloudflare.LevelNote(level) == "" {
			t.Errorf("%s has no name or note a person reads", level)
		}
		below = groups
	}
	operate, _ := cloudflare.Groups(config.CloudflareLevelOperate)
	configure, _ := cloudflare.Groups(config.CloudflareLevelConfigure)
	if slices.Contains(operate, "Zone Settings Write") {
		t.Error("operate grants Zone Settings Write, which only configure does")
	}
	if added := configure[len(operate):]; !slices.Equal(added, []string{"Zone Settings Write", "Response Compression Write"}) {
		t.Errorf("configure adds %v to operate", added)
	}
	if !slices.Contains(prov, "Account API Tokens Write") {
		t.Error("the provisioner is not asked for Account API Tokens Write")
	}
	if _, ok := cloudflare.Groups("admin"); ok {
		t.Error("an unknown level grants groups")
	}
}

// EVERY LEVEL'S POLICY READS BACK AS THAT LEVEL, and never as the one below or
// above it.
func TestEachLevelsPolicyReadsBackAsItself(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	groups := catalogueOf(f)
	for _, level := range cloudflare.Levels() {
		policies, err := cloudflare.PoliciesFor(testAccount, groups, f.zones, nil, level)
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		if got := cloudflare.LevelOfPolicies(testAccount, groups, f.zones, nil, policies); got != level {
			t.Errorf("the %s policy reads back as %q", level, got)
		}
	}
}

// A LEVEL'S POLICY IS ONE STATEMENT FOR THE ACCOUNT AND ONE FOR THE ZONES, the
// zones the company names resolved to IDs, a group a token cannot be granted
// left out, and the policy a token holds reads back as its level.
func TestAPolicyIsBuiltFromTheLiveCatalogueAndReadsBack(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	groups := catalogueOf(f)
	configured := []string{"Example.com", zoneOtherName}
	policies, err := cloudflare.PoliciesFor(testAccount, groups, f.zones, configured, config.CloudflareLevelOperate)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 2 {
		t.Fatalf("policies = %+v, want an account statement and a zone statement", policies)
	}
	zones, _ := json.Marshal(policies[1].Resources)
	want := `{"com.cloudflare.api.account.zone.` + zoneExampleID + `":"*","com.cloudflare.api.account.zone.` + zoneOtherID + `":"*"}`
	if string(zones) != want {
		t.Errorf("zone resources = %s, want %s", zones, want)
	}
	for _, g := range policies[1].PermissionGroups {
		if g.ID == "pg-legacy-cache-purge" {
			t.Error("a group a token cannot be granted was granted")
		}
	}
	if got := cloudflare.LevelOfPolicies(testAccount, groups, f.zones, configured, policies); got != config.CloudflareLevelOperate {
		t.Errorf("the operate policy reads back as %q", got)
	}
	if got := cloudflare.LevelOfPolicies(testAccount, groups, f.zones, nil, policies); got != "" {
		t.Errorf("a policy over two zones read as %q against every zone", got)
	}

	// EVERY ZONE where the company names none.
	all, err := cloudflare.PoliciesFor(testAccount, groups, f.zones, nil, config.CloudflareLevelRead)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(all[1].Resources)
	if string(raw) != `{"com.cloudflare.api.account.`+testAccount+`":{"com.cloudflare.api.account.zone.*":"*"}}` {
		t.Errorf("every-zone resources = %s", raw)
	}
	if got := cloudflare.LevelOfPolicies(testAccount, groups, f.zones, nil, all); got != config.CloudflareLevelRead {
		t.Errorf("the read policy reads back as %q", got)
	}
}

// A GROUP THE CATALOGUE LACKS IS A REFUSAL NAMING IT, never a narrower grant.
func TestALevelWhoseGroupIsMissingDoesNotResolve(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	groups := slices.DeleteFunc(catalogueOf(f), func(g cloudflare.PermissionGroup) bool {
		return g.Name == "Cache Purge" && g.Selectable
	})
	_, err := cloudflare.PoliciesFor(testAccount, groups, f.zones, nil, config.CloudflareLevelOperate)
	if err == nil || !strings.Contains(err.Error(), "Cache Purge") {
		t.Fatalf("operate without Cache Purge = %v, want a refusal naming it", err)
	}
	if _, err := cloudflare.PoliciesFor(testAccount, groups, f.zones, nil, config.CloudflareLevelRead); err != nil {
		t.Errorf("read, which needs no Cache Purge, = %v", err)
	}
}
