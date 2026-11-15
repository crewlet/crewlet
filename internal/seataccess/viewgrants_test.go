package seataccess_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// cloudflareView is a Cloudflare lane offer read at readAt: its levels, the
// lane's words for a grant's reach, and a catalogue of five groups. DNS Read is
// in the Read level; Zone Settings Write in Configure; Origin Rules: Edit is
// one the provisioner token does not hold.
func cloudflareView(readAt time.Time) seataccess.Options {
	return seataccess.Options{Lane: seataccess.LaneCloudflare, Live: true, ReadAt: readAt,
		Levels: []seataccess.Level{{ID: "read", Name: "Read"}, {ID: "operate", Name: "Operate"},
			{ID: "configure", Name: "Configure"}, {ID: "secure", Name: "Secure"}},
		Grants: []seataccess.GrantEntry{
			{ID: "DNS Read", Name: "DNS Read", Group: "Zone", ScopeKind: "zone", InLevel: "read"},
			{ID: "Origin Read", Name: "Origin Rules: Read", Group: "Zone", ScopeKind: "zone"},
			{ID: "Origin Write", Name: "Origin Rules: Edit", Group: "Zone", ScopeKind: "zone", Write: true,
				Unavailable: "The provisioner token does not hold Origin Rules: Edit",
				Missing:     []string{"Origin Rules: Edit"},
				ActionURL:   "https://dash.cloudflare.com/profile/api-tokens"},
			{ID: "Workers Scripts Write", Name: "Workers Scripts Write", Group: "Account",
				ScopeKind: seataccess.GrantScopeAccount, Write: true},
			{ID: "Zone Settings Write", Name: "Zone Settings Write", Group: "Zone", ScopeKind: "zone",
				Write: true, InLevel: "configure"},
		},
		GrantScope: []string{"example.com", "example.org"},
		GrantWhole: "every zone the company's agents reach", GrantAccount: "the account"}
}

// cloudflareAnswer is one SRE on Cloudflare's Read level holding grants, as the
// access route answers it.
func cloudflareAnswer(offer seataccess.Options, want seataccess.Desired,
	have seataccess.Observed) seataccess.Answer {
	return seataccess.Answer{Kind: integration.KindCloudflare, Options: []seataccess.Options{offer},
		Seats: []seataccess.SeatAnswer{{Handle: "sre", Name: "Site Reliability Engineer",
			Lanes: map[seataccess.Lane]seataccess.LaneAnswer{seataccess.LaneCloudflare: {
				Desired: want, Observed: have, Tool: seataccess.ToolLive, Plan: []seataccess.Step{}}}}}}
}

// AN AGENT'S GRANTS, ITS OPEN REQUESTS AND WHAT THE CONNECTION LACKS ARE READ
// ON ITS ACCESS, IN THE ENGINE'S WORDS. Each grant says what it gives by the
// catalogue's name, where, until when, who granted it and for which request,
// and whether the credential holds it yet; each of the agent's own requests on
// the lane names the two options of its question that grant it; the catalogue
// is counted beside the agent's level, and each permission a grant or request
// names is worded with what a person adds to the company's connection at the
// vendor and where. Mutation: drop the seat filter on the requests, word a
// permission by its id, count what the level holds, or leave out the action
// URL, and this fails.
func TestALaneViewSaysTheGrantsTheRequestsAndWhatTheConnectionLacks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 16, 0, 0, 0, time.UTC)
	readAt := now.Add(-40 * time.Second)
	want := seataccess.Desired{Enabled: true, Level: "read", Grants: []seataccess.Grant{
		{ID: "gaaaaaaaaaa", Permission: "Workers Scripts Write", ExpiresAt: now.Add(2 * time.Hour),
			GrantedBy: "Ada Lovelace", GrantedAt: now.Add(-2 * time.Hour), Reason: "deploy the route",
			Request: "zyxwv98765"},
		{ID: "gbbbbbbbbbb", Permission: "Origin Read", Scope: []string{"example.com"},
			GrantedBy: "Ada Lovelace", GrantedAt: now.Add(-time.Hour), Reason: "read the rules"},
	}}
	have := seataccess.Observed{Exists: true, Enabled: true, Level: "read", Grants: []seataccess.Grant{
		{Permission: "Workers Scripts Write"}}}
	asked := seataccess.GrantRequest{ID: "abcde12345", Kind: integration.KindCloudflare,
		Lane: seataccess.LaneCloudflare, Handle: "sre", Permission: "Origin Write",
		Scope: []string{"example.com"}, Hours: 4, Reason: "add the origin rule", Item: "OPS-77",
		Ask: "ada", AskedAt: now.Add(-10 * time.Minute)}
	other := asked
	other.ID, other.Handle, other.Permission = "fghij67890", "qa", "DNS Read"

	view, ok := seataccess.ViewOf(cloudflareAnswer(cloudflareView(readAt), want, have), "sre",
		seataccess.LaneCloudflare, seataccess.ViewQuery{Requests: []seataccess.GrantRequest{other, asked},
			Now: now})
	if !ok {
		t.Fatal("the agent's lane was not found")
	}
	for _, said := range []string{
		"Origin Rules: Read on example.com for good, granted by Ada Lovelace on 2026-11-15 " +
			"(not on its credential yet: the next sync adds it)",
		"Workers Scripts Write on the account until 2026-11-15 18:00 UTC, granted by Ada Lovelace " +
			"on 2026-11-15 for the agent's request zyxwv98765",
	} {
		if !strings.Contains(view.Grants, said) {
			t.Errorf("grants = %q\nwant it to say %q", view.Grants, said)
		}
	}
	if strings.Count(view.Grants, "; ") != 1 || strings.Contains(view.Grants, "zyxwv98765 (not") {
		t.Errorf("grants = %q, want two sentences and the held one not waiting on a sync", view.Grants)
	}
	wantRequest := "request abcde12345 asks for Origin Rules: Edit on example.com for 4 hours, on OPS-77 " +
		"on 2026-11-15; its question grants it with grant-rabcde12345-t (for 4 hours) or " +
		"grant-rabcde12345-p (for good)"
	if view.Requests != wantRequest {
		t.Errorf("requests = %q\nwant %q", view.Requests, wantRequest)
	}
	if view.Grantable != "Cloudflare offers 4 permissions beside this agent's level; the company's "+
		"connection lacks 1 of them" || !view.CatalogueReadAt.Equal(readAt) {
		t.Errorf("grantable = %q read at %v, want 4 beside the Read level, 1 lacking, read at %v",
			view.Grantable, view.CatalogueReadAt, readAt)
	}
	for _, said := range []string{
		"Workers Scripts Write (Account, changes things, in no level): the company's connection can grant it",
		"Origin Rules: Read (Zone, read-only, in no level): the company's connection can grant it",
		"Origin Rules: Edit (Zone, changes things, in no level): The provisioner token does not hold " +
			"Origin Rules: Edit; a person adds Origin Rules: Edit to the company's connection at " +
			"https://dash.cloudflare.com/profile/api-tokens",
	} {
		if !strings.Contains(view.Permissions, said) {
			t.Errorf("permissions = %q\nwant it to say %q", view.Permissions, said)
		}
	}
	if strings.Contains(view.Requests+view.Permissions, "fghij67890") ||
		strings.Contains(view.Permissions, "Zone Settings") || strings.Contains(view.Permissions, "DNS Read") {
		t.Errorf("the view words a request of another agent or a permission nothing named: %q / %q",
			view.Requests, view.Permissions)
	}
}

// A LOOKUP FINDS A PERMISSION BY PART OF ITS NAME, SAYS WHICH LEVEL HOLDS IT
// AND WHAT THE CONNECTION LACKS, AND IS BOUNDED. A lookup ignores case and
// punctuation, so "origin RULES" finds "Origin Rules: Edit"; each match says
// which level holds it, whether it changes things, and where the company's
// connection cannot grant it, what to add there and where; one that matches
// nothing says so; and one that matches more than a lookup answers says there
// are more. Mutation: match the identifier only, answer every match, or drop
// the level an entry is in, and this fails.
func TestALookupFindsAPermissionByPartOfItsNameAndIsBounded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 16, 0, 0, 0, time.UTC)
	want := seataccess.Desired{Enabled: true, Level: "read"}
	have := seataccess.Observed{Exists: true, Enabled: true, Level: "read"}
	view := func(t *testing.T, offer seataccess.Options, lookup string) seataccess.LaneView {
		t.Helper()
		v, ok := seataccess.ViewOf(cloudflareAnswer(offer, want, have), "sre", seataccess.LaneCloudflare,
			seataccess.ViewQuery{Lookup: lookup, Now: now})
		if !ok {
			t.Fatal("the agent's lane was not found")
		}
		return v
	}

	found := view(t, cloudflareView(now), "origin RULES")
	wantFound := []seataccess.LookupEntry{
		{ID: "Origin Read", Name: "Origin Rules: Read", Group: "Zone", Level: "No level", Grantable: "Yes"},
		{ID: "Origin Write", Name: "Origin Rules: Edit", Group: "Zone", Write: true, Level: "No level",
			Grantable: "The provisioner token does not hold Origin Rules: Edit",
			Missing:   "Origin Rules: Edit", ActionURL: "https://dash.cloudflare.com/profile/api-tokens"},
	}
	if !slices.Equal(found.Lookup, wantFound) || found.LookupMore || found.LookupNote != "" {
		t.Errorf("lookup = %+v (more %v, note %q)\nwant %+v", found.Lookup, found.LookupMore,
			found.LookupNote, wantFound)
	}
	if found.Permissions != "" {
		t.Errorf("a lookup was worded into the agent's own permissions: %q", found.Permissions)
	}
	for lookup, level := range map[string]string{"zone settings": "Configure and above",
		"dns": "Already in this agent's level"} {
		if got := view(t, cloudflareView(now), lookup).Lookup; len(got) != 1 || got[0].Level != level {
			t.Errorf("lookup %q = %+v, want one entry held at %q", lookup, got, level)
		}
	}
	if none := view(t, cloudflareView(now), "billing"); len(none.Lookup) != 0 ||
		none.LookupNote != `No permission in Cloudflare's catalogue matches "billing"` {
		t.Errorf("lookup = %+v note %q, want the lookup said as matching nothing", none.Lookup, none.LookupNote)
	}

	many := cloudflareView(now)
	many.Grants = nil
	for i := range seataccess.MaxLookup + 8 {
		name := fmt.Sprintf("Thing %02d Read", i)
		many.Grants = append(many.Grants, seataccess.GrantEntry{ID: name, Name: name, Group: "Zone",
			ScopeKind: "zone"})
	}
	if bounded := view(t, many, "thing"); len(bounded.Lookup) != seataccess.MaxLookup || !bounded.LookupMore {
		t.Errorf("a broad lookup answered %d entries (more %v), want %d and more", len(bounded.Lookup),
			bounded.LookupMore, seataccess.MaxLookup)
	}
}

// A LAPSED GRANT IS SAID AS LAPSED, and while its credential still holds it,
// as held until the next sync takes it off; a lane whose catalogue was not
// read words no catalogue at all rather than an empty one, and a lookup on it
// says the catalogue was not read rather than that nothing matched. Mutation:
// read every grant as live, or count a catalogue that was not read, and this
// fails.
func TestALapsedGrantAndAnUnreadCatalogueAreSaidAsSuch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 16, 0, 0, 0, time.UTC)
	want := seataccess.Desired{Enabled: true, Level: "read", Grants: []seataccess.Grant{
		{ID: "gaaaaaaaaaa", Permission: "Origin Read", ExpiresAt: now.Add(-time.Minute),
			GrantedBy: "Ada Lovelace", GrantedAt: now.Add(-4 * time.Hour), Reason: "read the rules"}}}
	have := seataccess.Observed{Exists: true, Enabled: true, Level: "read",
		Grants: []seataccess.Grant{{Permission: "Origin Read"}}}
	unread := cloudflareView(time.Time{})
	unread.Grants = nil
	view, ok := seataccess.ViewOf(cloudflareAnswer(unread, want, have), "sre", seataccess.LaneCloudflare,
		seataccess.ViewQuery{Lookup: "origin", Now: now})
	if !ok {
		t.Fatal("the agent's lane was not found")
	}
	if view.Grants != "Origin Read on every zone the company's agents reach, lapsed at 2026-11-15 15:59 UTC, "+
		"granted by Ada Lovelace on 2026-11-15 (still on its credential until the next sync takes it off)" {
		t.Errorf("grants = %q", view.Grants)
	}
	if view.Grantable != "" || view.Permissions != "" || !view.CatalogueReadAt.IsZero() ||
		len(view.Lookup) != 0 {
		t.Errorf("an unread catalogue was worded: %q / %q / %v / %+v", view.Grantable, view.Permissions,
			view.CatalogueReadAt, view.Lookup)
	}
	if view.LookupNote != "Cloudflare's catalogue of single permissions could not be read just now" {
		t.Errorf("lookup note = %q, want the catalogue said as unread", view.LookupNote)
	}
}

// THE OPEN REQUESTS ARE EVERY ONE STILL OPEN, OLDEST FIRST: one past its month
// or one this build cannot read is left for the sweep, never failing the read
// of the rest. Mutation: return every stored request, or fail on the
// unreadable one, and this fails.
func TestTheOpenRequestsAreEveryOneStillOpen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 16, 0, 0, 0, time.UTC)
	fleet := memory.NewFleet()
	requests, err := seataccess.NewCoordRequests(fleet)
	if err != nil {
		t.Fatal(err)
	}
	base := seataccess.GrantRequest{Kind: integration.KindCloudflare, Lane: seataccess.LaneCloudflare,
		Handle: "sre", Permission: "Origin Write", Hours: 4, Reason: "add the rule", Item: "OPS-77",
		Ask: "ada"}
	for id, age := range map[string]time.Duration{"aaaaa11111": time.Hour, "bbbbb22222": 3 * time.Hour,
		"ccccc33333": seataccess.RequestTTL + time.Hour} {
		r := base
		r.ID, r.AskedAt = id, now.Add(-age)
		if err := requests.Put(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := fleet.PutGrantRequest(t.Context(), "ddddd44444", []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	open, err := requests.Open(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].ID != "bbbbb22222" || open[1].ID != "aaaaa11111" {
		t.Fatalf("open = %+v, want the two within the month, oldest first", open)
	}
}

// WHAT TO ADD AT THE VENDOR IS ONE STEP: each permission the connection lacks,
// once, at the catalogue's address; none where nothing is missing.
func TestWhatToAddAtTheVendorIsOneStepFromTheCatalogue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 16, 0, 0, 0, time.UTC)
	want := seataccess.Desired{Enabled: true, Level: "read"}
	have := seataccess.Observed{Exists: true, Enabled: true, Level: "read"}
	asked := seataccess.GrantRequest{ID: "abcde12345", Kind: integration.KindCloudflare,
		Lane: seataccess.LaneCloudflare, Handle: "sre", Permission: "Origin Write", Hours: 4,
		Reason: "add the origin rule", Item: "ENG-58", Ask: "jane-founder", AskedAt: now}
	view, ok := seataccess.ViewOf(cloudflareAnswer(cloudflareView(now), want, have), "sre",
		seataccess.LaneCloudflare, seataccess.ViewQuery{Requests: []seataccess.GrantRequest{asked},
			Lookup: "origin", Now: now})
	if !ok {
		t.Fatal("the agent's lane was not found")
	}
	wantStep := &seataccess.VendorStep{Label: "Add to the company's connection at Cloudflare",
		Add: []string{"Origin Rules: Edit"}, URL: "https://dash.cloudflare.com/profile/api-tokens"}
	if !reflect.DeepEqual(view.AtVendor, wantStep) {
		t.Errorf("at the vendor = %+v, want %+v", view.AtVendor, wantStep)
	}
	held, _ := seataccess.ViewOf(cloudflareAnswer(cloudflareView(now), want, have), "sre",
		seataccess.LaneCloudflare, seataccess.ViewQuery{Lookup: "workers", Now: now})
	if held.AtVendor != nil {
		t.Errorf("a step at the vendor for permissions the connection holds: %+v", held.AtVendor)
	}
}
