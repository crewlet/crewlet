package atlas_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
)

// chain is a small company in the shape a request travels: a host name in a
// zone resolves to a server, a service runs on it and depends on a database
// that runs on it too, and the zone belongs to a DNS account.
func chain(t *testing.T) atlas.View {
	t.Helper()
	return atlas.Fold(chainClaims(t), now)
}

// chainClaims is the claims chain folds.
func chainClaims(t *testing.T) []atlas.Claim {
	t.Helper()
	m := must(t)
	zone := atlas.MustRef(atlas.KindDomain, "acme.example")
	host := atlas.MustRef(atlas.KindHostname, "api.acme.example")
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	svc := atlas.MustRef(atlas.KindService, "checkout-api")
	db := atlas.MustRef(atlas.KindDatastore, "on/ssh/web-1/postgres/5432")
	cf := atlas.MustRef(atlas.KindAccount, "cloudflare/0123456789abcdef0123456789abcdef")
	old := atlas.MustRef(atlas.KindService, "legacy-api")
	retired := m(atlas.Exists(old, ada, now.Add(-time.Hour)))
	return []atlas.Claim{
		m(atlas.Exists(zone, settings, now)), m(atlas.Exists(host, settings, now)), m(atlas.Exists(web, settings, now)),
		m(atlas.Exists(svc, ada, now)), m(atlas.Exists(db, ada, now)), m(atlas.Exists(cf, settings, now)),
		m(atlas.Says(cf, atlas.FactVendor, "cloudflare", settings, now)),
		m(atlas.Relates(host, atlas.RelationInZone, zone, settings, now)),
		m(atlas.Relates(host, atlas.RelationResolvesTo, web, ada, now)),
		m(atlas.Relates(svc, atlas.RelationRunsOn, web, ada, now)),
		m(atlas.Relates(svc, atlas.RelationDependsOn, db, ada, now)),
		m(atlas.Relates(db, atlas.RelationRunsOn, web, ada, now)),
		m(atlas.Relates(zone, atlas.RelationBelongsTo, cf, settings, now)),
		m(atlas.Relates(old, atlas.RelationRunsOn, web, ada, now.Add(-time.Hour))),
		retired, m(atlas.Denial(retired, ada, now)),
	}
}

// A TRACE WALKS AGAINST THE ARROWS FOR WHAT DEPENDS ON AN ENTRY, along them
// for what it depends on, hop by hop, through structural relations unless
// asked for all, and never through a retired entry.
func TestATraceWalksWhatDependsOnAnEntry(t *testing.T) {
	t.Parallel()
	v := chain(t)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1").ID()
	up, found, err := v.Trace(web, atlas.DirectionUp, atlas.TraceDepthMax, false, atlas.TraceMax)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	keys := func(tr atlas.Trace) [][]string {
		var out [][]string
		for _, h := range tr.Hops {
			var hop []string
			for _, e := range h.Entries {
				hop = append(hop, e.Key)
			}
			out = append(out, hop)
		}
		return out
	}
	want := [][]string{{"api.acme.example", "checkout-api", "on/ssh/web-1/postgres/5432"}}
	if got := keys(up); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("what depends on web-1: %v, want %v", got, want)
	}
	zone := atlas.MustRef(atlas.KindDomain, "acme.example").ID()
	z, _, _ := v.Trace(zone, atlas.DirectionUp, 2, false, atlas.TraceMax)
	if got := keys(z); fmt.Sprint(got) != "[[api.acme.example]]" {
		t.Errorf("what depends on the zone: %v", got)
	}
	down, _, _ := v.Trace(zone, atlas.DirectionDown, 1, false, atlas.TraceMax)
	if len(down.Hops) != 0 {
		t.Errorf("belongs_to is drawn on selection, and a structural trace walked it: %v", keys(down))
	}
	down, _, _ = v.Trace(zone, atlas.DirectionDown, 1, true, atlas.TraceMax)
	if got := keys(down); fmt.Sprint(got) != "[[cloudflare/0123456789abcdef0123456789abcdef]]" {
		t.Errorf("every relation down from the zone: %v", got)
	}
	if _, _, err := v.Trace(web, atlas.DirectionUp, atlas.TraceDepthMax+1, false, atlas.TraceMax); !errors.Is(err, atlas.ErrQuery) {
		t.Errorf("a trace past its depth bound: %v", err)
	}
}

// A LIST PAGES ON A CURSOR: every current entry once, in the asked order,
// however small the page, and a cursor of another order is refused.
func TestAListPagesOnItsCursor(t *testing.T) {
	t.Parallel()
	v := chain(t)
	entries := v.Select(atlas.Query{})
	for _, sort := range atlas.Sorts {
		var seen []string
		after := ""
		for range 10 {
			page, err := atlas.List(entries, v.Summaries(false), sort, after, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range page.Rows {
				seen = append(seen, r.Key)
			}
			if page.Total != len(entries) {
				t.Errorf("%s: total %d, want %d", sort, page.Total, len(entries))
			}
			if after = page.Next; after == "" {
				break
			}
		}
		if len(seen) != len(entries) || len(slices.Compact(slices.Sorted(slices.Values(seen)))) != len(entries) {
			t.Errorf("%s paged %v over %d entries", sort, seen, len(entries))
		}
		if slices.Contains(seen, "legacy-api") {
			t.Errorf("%s listed a retired entry by default", sort)
		}
	}
	page, _ := atlas.List(entries, v.Summaries(false), atlas.SortName, "", 2)
	if _, err := atlas.List(entries, v.Summaries(false), atlas.SortKey, page.Next, 2); !errors.Is(err, atlas.ErrQuery) {
		t.Errorf("a cursor of another order: %v", err)
	}
}

// THE HEALTH ORDER READS THE MOST URGENT FIRST, then the map's order, on
// every page: a list grouped by health never meets a group twice.
func TestAListByHealthReadsTheMostUrgentFirst(t *testing.T) {
	t.Parallel()
	v := chain(t)
	for i := range v.Entries {
		switch v.Entries[i].Ref.Key {
		case "checkout-api":
			v.Entries[i].Review = true
		case "ssh/web-1":
			v.Entries[i].Signals = []atlas.Signal{{Kind: atlas.SignalServerTestFailed, Severity: atlas.SeverityDanger}}
		}
	}
	entries := v.Select(atlas.Query{})
	var got []atlas.Health
	after := ""
	for range len(entries) + 1 {
		page, err := atlas.List(entries, v.Summaries(false), atlas.SortHealth, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Rows {
			got = append(got, r.Health)
		}
		if after = page.Next; after == "" {
			break
		}
	}
	want := []atlas.Health{atlas.HealthAlerting, atlas.HealthNeedsLook}
	for range len(entries) - len(want) {
		want = append(want, atlas.HealthQuiet)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the health order paged %v, want %v", got, want)
	}
}

// A FACET COUNTS OVER EVERY OTHER FILTER, so a menu says what picking it
// leaves; a vendor is an account's, or the account an entry belongs to.
func TestAFacetCountsOverEveryOtherFilter(t *testing.T) {
	t.Parallel()
	v := chain(t)
	f := v.Facets(atlas.Query{Kind: atlas.KindHost})
	if f.Kinds[atlas.KindService] != 1 || f.Kinds[atlas.KindHost] != 1 {
		t.Errorf("kinds under a kind filter: %v", f.Kinds)
	}
	if f.Lanes[atlas.LaneRuntime] != 1 || f.Lanes[atlas.LaneServices] != 0 {
		t.Errorf("lanes under a kind filter: %v", f.Lanes)
	}
	if got := v.Facets(atlas.Query{}).Vendors[atlas.VendorCloudflare]; got != 2 {
		t.Errorf("Cloudflare holds %d entries, want its account and the zone that belongs to it", got)
	}
	if err := (atlas.Query{Kind: "server"}).Validate(); !errors.Is(err, atlas.ErrQuery) {
		t.Errorf("a kind outside the set: %v", err)
	}
}

// AN UNCONFIRMED ENTRY STAYS OUT OF A READ UNTIL IT IS ASKED FOR: an agent's
// report nothing backs is hidden until a person confirms it, while the word's
// own facet still counts it, so a screen can say what showing it would add.
func TestAnUnconfirmedEntryStaysOutUntilAskedFor(t *testing.T) {
	t.Parallel()
	m := must(t)
	zone := atlas.MustRef(atlas.KindDomain, "acme.example")
	guess := atlas.MustRef(atlas.KindService, "billing-api")
	unbacked := atlas.AgentSource("sre", "turn-2", atlas.EvidenceNone)
	v := atlas.Fold([]atlas.Claim{m(atlas.Exists(zone, settings, now)), m(atlas.Exists(guess, unbacked, now))}, now)
	names := func(q atlas.Query) []string {
		var out []string
		for _, e := range v.Select(q) {
			out = append(out, e.Name)
		}
		return out
	}
	if got := names(atlas.Query{}); !slices.Equal(got, []string{"acme.example"}) {
		t.Errorf("a read asking for nothing showed %v, want the unconfirmed entry left out", got)
	}
	for _, q := range []atlas.Query{{Unconfirmed: true}, {Word: atlas.WordUnconfirmed}} {
		if got := names(q); !slices.Contains(got, "billing-api") {
			t.Errorf("%+v showed %v, want the unconfirmed entry", q, got)
		}
	}
	if got := v.Facets(atlas.Query{}).Words[atlas.WordUnconfirmed]; got != 1 {
		t.Errorf("the word facet counts %d unconfirmed entries, want the hidden one counted", got)
	}
	if got := v.Map(atlas.Query{}, nil, false).Nodes; len(got) != 1 {
		t.Errorf("the map drew %d entries, want the unconfirmed one left off it", len(got))
	}
}

// A MAP DRAWS EVERY LANE AND CLOSES GROUPS PAST ITS BOUND, largest first, so
// the answer stays bounded and says which it closed; edges are structural.
func TestAMapClosesGroupsPastItsBound(t *testing.T) {
	t.Parallel()
	m := must(t)
	var claims []atlas.Claim
	for i := range atlas.MapMax + 10 {
		r := atlas.MustRef(atlas.KindHost, fmt.Sprintf("ssh/srv-%04d", i))
		claims = append(claims, m(atlas.Exists(r, settings, now)))
	}
	for i := range 5 {
		r := atlas.MustRef(atlas.KindService, fmt.Sprintf("svc-%d", i))
		claims = append(claims, m(atlas.Exists(r, ada, now)),
			m(atlas.Relates(r, atlas.RelationRunsOn, atlas.MustRef(atlas.KindHost, "ssh/srv-0000"), ada, now)))
	}
	mp := atlas.Fold(claims, now).Map(atlas.Query{}, nil, false)
	if len(mp.Lanes) != len(atlas.Lanes) {
		t.Errorf("%d lanes drawn, want every lane", len(mp.Lanes))
	}
	if !slices.Equal(mp.Collapsed, []string{"runtime"}) || len(mp.Nodes) != 5 {
		t.Errorf("collapsed %v with %d open nodes, want the runtime group closed and the services open",
			mp.Collapsed, len(mp.Nodes))
	}
	if len(mp.Edges) != 0 {
		t.Errorf("edges to a closed group were drawn: %v", mp.Edges)
	}
	open := chain(t).Map(atlas.Query{}, nil, false)
	for _, e := range open.Edges {
		if !e.Relation.Structural() {
			t.Errorf("the map drew %s at rest", e.Relation)
		}
	}
	if len(open.Edges) != 5 || len(open.Collapsed) != 0 {
		t.Errorf("the small map: %d edges, collapsed %v", len(open.Edges), open.Collapsed)
	}
	closed := chain(t).Map(atlas.Query{}, []string{"runtime"}, false)
	if slices.ContainsFunc(closed.Nodes, func(s atlas.Summary) bool { return s.Lane == atlas.LaneRuntime }) {
		t.Error("a group the reader closed was drawn open")
	}
}

// A READER SEES ONLY THE SIGNALS THEY ARE IN THE AUDIENCE OF: an integration's
// standing is an administrator's, and the health it adds up to follows.
func TestAReaderSeesOnlyTheSignalsTheyMayRead(t *testing.T) {
	t.Parallel()
	m := must(t)
	cf := atlas.MustRef(atlas.KindAccount, "cloudflare/0123456789abcdef0123456789abcdef")
	v := atlas.Join(atlas.Fold([]atlas.Claim{m(atlas.Exists(cf, settings, now))}, now), atlas.Joined{
		Signals: map[atlas.Ref][]atlas.Signal{cf: {{Kind: atlas.SignalIntegrationFailing, Severity: atlas.SeverityDanger,
			Audience: atlas.AudienceAdmins, Since: now}}},
	}, now)
	if e := entry(t, atlas.For(v, atlas.Reader{Admin: true}), cf); len(e.Signals) != 1 || e.Health() != atlas.HealthAlerting {
		t.Errorf("an administrator reads %v, %s", e.Signals, e.Health())
	}
	member := atlas.For(v, atlas.Reader{})
	if e := entry(t, member, cf); len(e.Signals) != 0 || e.Health() != atlas.HealthQuiet {
		t.Errorf("a member reads %v, %s", e.Signals, e.Health())
	}
	if e := entry(t, v, cf); len(e.Signals) != 1 {
		t.Error("narrowing for a reader changed the view it was given")
	}
}
