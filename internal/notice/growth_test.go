package notice_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/notice"
)

var growthNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// catalog is a growth catalog of kind calling api at version, retiring on
// retires (zero for none announced).
func catalog(kind, api, version string, retires time.Time) growth.Catalog {
	return growth.Catalog{Kind: kind, API: api, Version: version, Retires: retires}
}

// AN API VERSION THIS BUILD CALLS IS A NOTICE FROM NINETY DAYS BEFORE ITS
// VENDOR RETIRES IT, keyed by its kind and version and worded with the date: a
// retirement further off, or none announced, is no notice, and one already
// past says so.
//
// Mutation: open the notice at any announced date, or compare the window the
// wrong way, and a retirement a year off reaches the administrators today.
func TestAnAPIVersionIsANoticeFromNinetyDaysBeforeItRetires(t *testing.T) {
	t.Parallel()
	soon := growthNow.Add(notice.RetiringWithin - time.Hour)
	got := notice.FromCatalogs([]growth.Catalog{
		catalog("fakeads", "Fake Ads API", "v1", soon),
		catalog("laterads", "Later Ads API", "v2", growthNow.Add(notice.RetiringWithin+time.Hour)),
		catalog("steadyads", "Steady Ads API", "v3", time.Time{}),
		catalog("goneads", "Gone Ads API", "v4", growthNow.Add(-48*time.Hour)),
	}, nil, growthNow)
	want := []notice.Notice{
		{Class: notice.ClassAPIVersionRetiring, Since: growthNow.Add(-48 * time.Hour).Add(-notice.RetiringWithin),
			Subject: advisor.Subject{Kind: advisor.SubjectIntegration, Name: "goneads/v4"},
			Title:   "Gone Ads API v4 is retiring",
			Person:  "This Crewlet build calls Gone Ads API v4, which the vendor retired on 19 November 2026. Update Crewlet now."},
		{Class: notice.ClassAPIVersionRetiring, Since: soon.Add(-notice.RetiringWithin),
			Subject: advisor.Subject{Kind: advisor.SubjectIntegration, Name: "fakeads/v1"},
			Title:   "Fake Ads API v1 is retiring",
			Person:  "This Crewlet build calls Fake Ads API v1, which the vendor retires on 19 February 2027. Update Crewlet before then."},
	}
	if !slices.EqualFunc(got, want, equalNotice) {
		t.Errorf("the notices are\n%+v\nwant\n%+v", got, want)
	}
}

// A VENDOR SAYING A VERSION IS DEPRECATED OPENS THE SAME NOTICE, on the nodes
// that heard it: one heard for a version the catalog already retires is that
// notice, naming the node; one the catalog announces nothing for is its own,
// in the catalog's API name where the build has one and without a date where
// the vendor gave none; and a node that stops hearing it closes it.
//
// Mutation: key the signal's notice on its kind alone and two versions of one
// product read as one.
func TestAVendorsDeprecationOpensTheSameNotice(t *testing.T) {
	t.Parallel()
	retires := growthNow.Add(30 * 24 * time.Hour)
	catalogs := []growth.Catalog{catalog("fakeads", "Fake Ads API", "v1", retires),
		catalog("steadyads", "Steady Ads API", "v3", time.Time{})}
	heard := growthNow.Add(-time.Hour)
	nodes := []notice.NodeGrowth{
		{Node: "n2", Signals: coord.GrowthSignals{Deprecations: []coord.GrowthDeprecation{
			{Product: "fakeads", Version: "v1", Retires: retires, Since: heard},
			{Product: "steadyads", Version: "v3", Since: heard},
			{Product: "fakeads", Version: "v0", Since: heard}}}},
	}
	got := notice.FromCatalogs(catalogs, nodes, growthNow)
	names := map[string]notice.Notice{}
	for _, n := range got {
		names[n.Subject.Name] = n
	}
	if len(got) != 3 || len(names) != 3 {
		t.Fatalf("the notices are %+v, want one per kind and version", got)
	}
	if n := names["fakeads/v1"]; !slices.Equal(n.Nodes, []string{"n2"}) || !n.Since.Equal(retires.Add(-notice.RetiringWithin)) {
		t.Errorf("the catalog's and the vendor's retirement of one version read as %+v", n)
	}
	if n := names["steadyads/v3"]; n.Person != "This Crewlet build calls Steady Ads API v3, which the vendor has "+
		"deprecated. Update Crewlet before the vendor retires it." || !n.Since.Equal(heard) {
		t.Errorf("a deprecation with no date reads %+v", n)
	}
	if got := notice.FromCatalogs(catalogs[1:], nil, growthNow); len(got) != 0 {
		t.Errorf("a deprecation no node hears any more is still %+v", got)
	}
}

// A CLOCK SKEW IS A NOTICE WHILE A NODE REPORTS IT, one per product naming
// every node it stands on, worded for the node furthest off and which way;
// once no node reports it, it is closed.
//
// Mutation: word it for the first node rather than the furthest off, or drop
// the sign, and an administrator fixes the wrong clock in the wrong direction.
func TestAClockSkewIsANoticeWhileANodeReportsIt(t *testing.T) {
	t.Parallel()
	since := growthNow.Add(-10 * time.Minute)
	one := notice.FromClockSkews([]notice.NodeGrowth{{Node: "n1", Signals: coord.GrowthSignals{
		ClockSkews: []coord.GrowthClockSkew{{Product: "appstoreconnect", Vendor: "Apple", OffsetSeconds: 75,
			Since: since}}}}})
	want := notice.Notice{Class: notice.ClassClockSkew, Since: since, Nodes: []string{"n1"},
		Subject: advisor.Subject{Kind: advisor.SubjectIntegration, Name: "appstoreconnect"},
		Title:   "Apple refuses the tokens Crewlet signs",
		Person: "This node's clock is 1m 15s ahead of Apple's, so Apple refuses the tokens Crewlet signs. " +
			"Fix the node's time synchronisation."}
	if len(one) != 1 || !equalNotice(one[0], want) {
		t.Errorf("one node's skew is %+v, want %+v", one, want)
	}
	two := notice.FromClockSkews([]notice.NodeGrowth{
		{Node: "n1", Signals: coord.GrowthSignals{ClockSkews: []coord.GrowthClockSkew{
			{Product: "appstoreconnect", Vendor: "Apple", OffsetSeconds: 75, Since: since}}}},
		{Node: "n2", Signals: coord.GrowthSignals{ClockSkews: []coord.GrowthClockSkew{
			{Product: "appstoreconnect", Vendor: "Apple", OffsetSeconds: -120, Since: since.Add(time.Minute)}}}},
	})
	if len(two) != 1 || !slices.Equal(two[0].Nodes, []string{"n1", "n2"}) || two[0].Person != "The clocks of 2 "+
		"nodes are up to 2m behind Apple's, so Apple refuses the tokens Crewlet signs. Fix their time "+
		"synchronisation." {
		t.Errorf("two nodes' skews are %+v", two)
	}
	if got := notice.FromClockSkews([]notice.NodeGrowth{{Node: "n1"}}); len(got) != 0 {
		t.Errorf("a node reporting no skew raised %+v", got)
	}
}

// THE GROWTH NOTICES ARE READ WITH EVERY OTHER: notice.Read lists what the
// growth products meet beside the findings, the alarms and the holds.
//
// Mutation: leave the vendors' list out of Read and the notices are built and
// never said.
func TestTheGrowthNoticesAreReadWithEveryOther(t *testing.T) {
	t.Parallel()
	vendors := notice.FromCatalogs([]growth.Catalog{catalog("fakeads", "Fake Ads API", "v1",
		growthNow.Add(24*time.Hour))}, nil, growthNow)
	got := notice.Read(nil, nil, nil, vendors, nil, growthNow)
	if len(got) != 1 || got[0].Class != notice.ClassAPIVersionRetiring {
		t.Errorf("the notices read are %+v", got)
	}
}

// equalNotice compares two notices field by field.
func equalNotice(a, b notice.Notice) bool {
	return a.Class == b.Class && a.Subject == b.Subject && a.Since.Equal(b.Since) &&
		slices.Equal(a.Nodes, b.Nodes) && a.Title == b.Title && a.Person == b.Person && a.Cause == b.Cause &&
		a.Alarm == b.Alarm && a.Finding == b.Finding
}
