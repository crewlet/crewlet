package adsense

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// EVERY METHOD IS IN THE CATALOG, EACH CLASSED, AND EVERY TOOL IN ONE CLASS:
// the catalog validates, it lists the coverage matrix's 52 methods (39 read,
// 5 operate, 1 public, 7 admin), nothing spends or is refused, a live ad
// unit's size or type is the one field rule, five Management writes and
// every Platform API method answer only enabled projects, and there is no
// spend tool.
//
// Mutation: drop a method, class a write read, or drop the ad unit's field
// rule, and this test goes red.
func TestTheCatalogListsEveryMethodInExactlyOneClass(t *testing.T) {
	t.Parallel()
	c, ok := growth.CatalogOf(string(Kind))
	if !ok {
		t.Fatal("the catalog did not register")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	counts := map[growth.Class]int{}
	var ruled []string
	restrictedMgmt, platformOps := 0, 0
	for _, op := range c.Operations {
		counts[op.Class]++
		for _, f := range op.Fields {
			ruled = append(ruled, op.ID+":"+f.Field+":"+string(f.Class))
		}
		switch {
		case platformOnly(op):
			platformOps++
		case restricted(op):
			restrictedMgmt++
		}
	}
	if len(c.Operations) != 52 || counts[growth.ClassRead] != 39 || counts[growth.ClassOperate] != 5 ||
		counts[growth.ClassPublic] != 1 || counts[growth.ClassAdmin] != 7 {
		t.Errorf("%d operations by class %v, want 52: 39 read, 5 operate, 1 public and 7 admin", len(c.Operations), counts)
	}
	if want := []string{adUnitPatchOp + ":contentAdsSettings:public"}; !slices.Equal(ruled, want) {
		t.Errorf("the field rules are %v, want %v", ruled, want)
	}
	if restrictedMgmt != 5 || platformOps != 19 {
		t.Errorf("%d restricted Management methods and %d Platform methods, want 5 and 19", restrictedMgmt, platformOps)
	}
	if len(c.Tools) != 17 {
		t.Errorf("%d tools, want 13 curated and 4 generic", len(c.Tools))
	}
	for _, tool := range c.Tools {
		if tool.Class == growth.ClassSpend {
			t.Errorf("tool %s spends; AdSense pays the publisher", tool.Name)
		}
	}
}

// EVERY WRITE TO SOMETHING THAT EXISTS IS CLASSED AGAINST IT, READ FIRST: a
// write whose path names a resource (a patch, a delete, an act on one) has a
// read of exactly that resource, so its class and a held call's binding see
// its live state; a create reads at most what it is made in.
//
// Mutation: drop a getter, and the write is classed and bound on nothing.
func TestEveryWriteToSomethingThatExistsIsReadFirst(t *testing.T) {
	t.Parallel()
	c := catalog()
	for _, op := range c.Operations {
		if op.Class == growth.ClassRead {
			continue
		}
		req := googlegrowth.Request{Op: op, Address: op.Path}
		get, has := getters[op.ID]
		target := req.Resource()
		if target == "" {
			target = op.Path[:strings.LastIndex(op.Path, "/")]
		}
		switch g, ok := c.Operation(get); {
		case req.Resource() != "" && !has:
			t.Errorf("%s acts on %s and reads nothing first", op.ID, target)
		case has && (!ok || g.Method != "GET" || g.Path != target):
			t.Errorf("%s reads %s (%s) first, want a GET of %s", op.ID, get, g.Path, target)
		}
	}
}

// EVERY PATCH NAMES ITS MASK PATHS, AND EVERY QUERY KEY IS GOOGLE'S: each
// patch takes updateMask alone and lists the paths it may name, and each
// listing takes Google's paging.
func TestEveryPatchNamesItsMaskPaths(t *testing.T) {
	t.Parallel()
	for _, op := range catalog().Operations {
		switch {
		case op.Method == "PATCH" && (len(masks[op.ID]) == 0 || !slices.Equal(queryKeys(op), []string{"updateMask"})):
			t.Errorf("%s patches with mask paths %v and query keys %v", op.ID, masks[op.ID], queryKeys(op))
		case strings.HasSuffix(op.ID, ".list") && !slices.Contains(queryKeys(op), "pageToken") &&
			op.ID != "adsense.accounts.alerts.list" && op.ID != "adsense.accounts.payments.list":
			t.Errorf("%s lists without Google's paging", op.ID)
		}
	}
}
