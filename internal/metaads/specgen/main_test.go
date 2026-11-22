package main

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/metaads"
)

// sampleSpecs are three node types as Meta's specs write them: an ad set with
// an update and an edge, a campaign it is reached from, and an ad account.
func sampleSpecs() map[string]spec {
	return map[string]spec{
		"AdAccount": {APIs: []api{{Method: "GET", Endpoint: "campaigns", Return: "Campaign"}},
			Fields: []field{{Name: "account_id"}, {Name: "id"}}},
		"Campaign": {APIs: []api{{Method: "GET", Endpoint: "adsets", Return: "AdSet"}},
			Fields: []field{{Name: "account_id"}}},
		"AdSet": {APIs: []api{
			{Name: "#update", Method: "POST", Return: "AdSet", Params: []field{{Name: "status"}, {Name: "daily_budget"}}},
			{Method: "GET", Endpoint: "copies", Return: "AdSet"},
		}, Fields: []field{{Name: "account_id"}}},
	}
}

func sampleRefs() []metaads.SpecRef {
	return []metaads.SpecRef{
		{ID: "ad_account.campaigns.list", Method: "GET", Path: "/act_{ad_account_id}/campaigns", Resource: "AdAccount",
			Spec: "AdAccount.json", API: "GET campaigns", Returns: "Campaign"},
		{ID: "campaign.adsets.list", Method: "GET", Path: "/{campaign_id}/adsets", Resource: "Campaign",
			Spec: "Campaign.json", API: "GET adsets", Returns: "AdSet"},
		{ID: "ad_set.update", Method: "POST", Path: "/{ad_set_id}", Resource: "AdSet", Spec: "AdSet.json",
			API: "POST #update", Returns: "AdSet", Fields: []string{"status", "daily_budget"}},
	}
}

// The check holds every table row to its spec (its operation, path, rule
// fields and answer type), finds a spec operation nothing classes, a node
// type nothing classes, a version other than the table's and an account-owned
// set the specs disagree with; an agreeing table answers nothing.
//
// Mutation: skip any one comparison, and its drift passes the check.
func TestTheCheckFindsEveryDisagreement(t *testing.T) {
	t.Parallel()
	owned := []string{"AdAccount", "AdSet", "Campaign"}
	left := map[[2]string]string{{"AdSet.json", "GET copies"}: "not ads"}
	if got := check(sampleSpecs(), metaads.Version, sampleRefs(), owned, left, nil); len(got) != 0 {
		t.Fatalf("an agreeing table answered %v", got)
	}
	drift := func(name string, f func(specs map[string]spec, refs []metaads.SpecRef) (string, []string), want string) {
		t.Helper()
		specs, refs := sampleSpecs(), sampleRefs()
		version, ownedNow := metaads.Version, owned
		if v, o := f(specs, refs); v != "" || o != nil {
			version, ownedNow = v, o
			if version == "" {
				version = metaads.Version
			}
			if ownedNow == nil {
				ownedNow = owned
			}
		}
		got := strings.Join(check(specs, version, refs, ownedNow, left, nil), "\n")
		if !strings.Contains(got, want) {
			t.Errorf("%s: the check said %q, want %q", name, got, want)
		}
	}
	drift("a path edited", func(_ map[string]spec, refs []metaads.SpecRef) (string, []string) {
		refs[2].Path = "/{ad_set_id}/update"
		return "", nil
	}, "changed: ad_set.update's path")
	drift("an operation gone", func(specs map[string]spec, _ []metaads.SpecRef) (string, []string) {
		s := specs["AdSet"]
		s.APIs = s.APIs[1:]
		specs["AdSet"] = s
		return "", nil
	}, "gone: ad_set.update")
	drift("a rule's field gone", func(specs map[string]spec, _ []metaads.SpecRef) (string, []string) {
		s := specs["AdSet"]
		s.APIs[0].Params = s.APIs[0].Params[:1]
		specs["AdSet"] = s
		return "", nil
	}, "rule names daily_budget")
	drift("an answer retyped", func(_ map[string]spec, refs []metaads.SpecRef) (string, []string) {
		refs[0].Returns = "AdSet"
		return "", nil
	}, "answers \"AdSet\" in the table")
	drift("a new operation", func(specs map[string]spec, _ []metaads.SpecRef) (string, []string) {
		s := specs["Campaign"]
		s.APIs = append(s.APIs, api{Method: "DELETE", Name: "#delete"})
		specs["Campaign"] = s
		return "", nil
	}, "new: Campaign.json DELETE #delete")
	drift("a new node type", func(specs map[string]spec, _ []metaads.SpecRef) (string, []string) {
		specs["AdLabel"] = spec{APIs: []api{{Method: "GET", Name: "#get"}}}
		s := specs["AdAccount"]
		s.APIs = append(s.APIs, api{Method: "GET", Endpoint: "adlabels", Return: "list<AdLabel>"})
		specs["AdAccount"] = s
		return "", nil
	}, "node type AdLabel")
	drift("another version", func(map[string]spec, []metaads.SpecRef) (string, []string) {
		return "v99.0", nil
	}, "version: the specs are v99.0")
	drift("an owner unlisted", func(map[string]spec, []metaads.SpecRef) (string, []string) {
		return "", []string{"AdAccount"}
	}, "placed by account_id")
	if got := snake("IGUser") + " " + snake("AdSet") + " " + snake("ProductCatalogAppsAndSoftwareItem"); got !=
		"ig_user ad_set product_catalog_apps_and_software_item" {
		t.Errorf("snake case read %q", got)
	}
}
