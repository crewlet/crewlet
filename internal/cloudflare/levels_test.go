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
// one before it does and adds exactly its own, no level writes DNS or
// certificates, and the provisioner is asked for every group a level grants.
// The additions are spelled out rather than derived, so a group moved between
// levels fails here.
func TestEveryLevelGrantsAndTheProvisionerHoldsThemAll(t *testing.T) {
	t.Parallel()
	if !slices.Equal(cloudflare.Levels(), config.CloudflareLevels()) {
		t.Fatalf("levels %v, the document offers %v", cloudflare.Levels(), config.CloudflareLevels())
	}
	adds := map[string][]string{
		// READ COVERS WAF CUSTOM RULES: Firewall Services is the legacy
		// firewall alone, and the rulesets' entry point answers 403 without
		// Zone WAF; an allowlist matches on the account's IP lists.
		config.CloudflareLevelRead: {
			"Zone Read", "Zone Settings Read", "Analytics Read", "DNS Read", "Cache Settings Read",
			"Zone Transform Rules Read", "Firewall Services Read", "Zone WAF Read", "SSL and Certificates Read", "Account Analytics Read",
			"Account Rule Lists Read", "Account Settings Read",
		},
		config.CloudflareLevelOperate: {"Cache Purge", "Cache Settings Write"},
		config.CloudflareLevelConfigure: {
			"Zone Settings Write", "Response Compression Write", "Zone Transform Rules Write",
		},
		config.CloudflareLevelSecure: {"Zone WAF Write", "Account Rule Lists Write"},
	}
	prov := cloudflare.ProvisionerGroups()
	var below []string
	for _, level := range cloudflare.Levels() {
		groups, ok := cloudflare.Groups(level)
		if !ok {
			t.Fatalf("%s grants nothing", level)
		}
		if want := append(slices.Clone(below), adds[level]...); !slices.Equal(groups, want) {
			t.Errorf("%s grants %v, want %v", level, groups, want)
		}
		for _, forbidden := range []string{
			"DNS Write", "Firewall Services Write", "SSL and Certificates Write", "Page Rules Write",
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
