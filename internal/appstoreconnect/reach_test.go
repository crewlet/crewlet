package appstoreconnect_test

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// reachProduct is the product over company, calling s as Apple.
func reachProduct(t *testing.T, s *store, company *config.Company) *appstoreconnect.Product {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	key := p8(t)
	return appstoreconnect.New(appstoreconnect.Deps{
		Company: func() *config.Company { return company },
		Resolve: func(v string) string {
			if v == "${K}" {
				return key
			}
			return v
		},
		HTTP: srv.Client(), Base: srv.URL, Audit: &audit{t: t}, Leases: &leases{},
		Now: func() time.Time { return time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC) },
	})
}

// offer is the lane's options as the sync and the access routes read them,
// from a session handed reads.
func offer(t *testing.T, company *config.Company, reads seataccess.PassReads) seataccess.Options {
	t.Helper()
	v, ok := seataccess.Lookup(integration.KindAppStoreConnect)
	if !ok {
		t.Fatal("no App Store Connect access vendor is registered")
	}
	roster, err := company.Organization()
	if err != nil {
		t.Fatal(err)
	}
	in := seataccess.OpenInput{Config: company, Org: roster, Reads: reads}
	sess, err := v.Open(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := seataccess.ReadOptions(t.Context(), v, sess, in, seataccess.LaneAppStoreConnect)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

// THE CHOOSER OFFERS EVERY APP THE KEYS REACH, BY NAME: read live with the
// least-privileged key, each named with its bundle ID beside it, ordered by
// name, and the reach stays open, so an app typed by its ID is still taken.
// A company that names apps offers only those, still by name.
//
// Mutation: drop the session's names, and the chooser offers bare IDs; keep
// the vendor's list where the company names apps, and an agent is offered an
// app the company keeps from it.
func TestTheAppChooserOffersTheKeysAppsByName(t *testing.T) {
	t.Parallel()
	company := reachCompany(marketing(), nil)
	p := reachProduct(t, &store{}, company)
	reach, err := p.Reach(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	opts := offer(t, company, reach)
	if !slices.Equal(opts.Scope, []string{"111", "222"}) || !opts.ScopeOpen || !opts.ScopeLive {
		t.Fatalf("an open reach offers %v (open %v, read %v), want both apps, open", opts.Scope, opts.ScopeOpen,
			opts.ScopeLive)
	}
	if opts.ScopeNames["111"] != "Notes" || opts.ScopeDetails["111"] != "com.example.notes" ||
		opts.ScopeNames["222"] != "Other" || opts.ScopeDetails["222"] != "com.example.other" {
		t.Fatalf("the apps are named %v with %v", opts.ScopeNames, opts.ScopeDetails)
	}
	if !opts.Reaches("6763222842") {
		t.Fatal("an app the list does not show is refused, though the company names none")
	}
	named := reachCompany(marketing(), []string{"222"})
	opts = offer(t, named, reach)
	if !slices.Equal(opts.Scope, []string{"222"}) || opts.ScopeOpen || opts.ScopeNames["222"] != "Other" {
		t.Fatalf("a company naming one app offers %v named %v (open %v)", opts.Scope, opts.ScopeNames, opts.ScopeOpen)
	}
	if opts.Reaches("111") {
		t.Fatal("an app the company does not name is within reach")
	}
	// NOTHING HANDED IN, nothing listed: the reach is still the document's.
	if bare := offer(t, company, nil); len(bare.Scope) != 0 || !bare.ScopeOpen {
		t.Fatalf("a session with no reads offers %v (open %v)", bare.Scope, bare.ScopeOpen)
	}
}

// THE ADMIN KEY NEVER LISTS APPS: a team with only it says why its apps are
// not listed and what to add, Apple is never called, and its agents may still
// name any app by its ID.
func TestATeamWithOnlyAnAdminKeyListsNoApps(t *testing.T) {
	t.Parallel()
	s := &store{}
	company := reachCompany(adminOnly(), nil)
	p := reachProduct(t, s, company)
	reach, err := p.Reach(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(reach) != 1 || reach[0].Read || !strings.Contains(reach[0].Unread, "other than Admin") {
		t.Fatalf("the Admin-only team reads %+v", reach)
	}
	if len(s.kids) != 0 {
		t.Fatalf("Apple was called with the Admin key: %v", s.kids)
	}
	if opts := offer(t, company, reach); len(opts.Scope) != 0 || !opts.ScopeOpen {
		t.Fatalf("the Admin-only team offers %v (open %v)", opts.Scope, opts.ScopeOpen)
	}
}

// A PASS READS THE APPS AGAIN: a read inside reachTTL is the one held, a fresh
// read (a pass, Check now) asks Apple, and the held reach answers without
// asking.
func TestAFreshReadAsksAppleAgain(t *testing.T) {
	t.Parallel()
	s := &store{}
	p := reachProduct(t, s, reachCompany(marketing(), nil))
	lists := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for _, k := range s.kids {
			if strings.HasPrefix(k, "GET /v1/apps ") {
				n++
			}
		}
		return n
	}
	if held := p.HeldReach(); len(held) != 1 || held[0].Read {
		t.Fatalf("a team never read is held as %+v", held)
	}
	for _, fresh := range []bool{false, false, true} {
		if _, err := p.Reach(t.Context(), fresh); err != nil {
			t.Fatal(err)
		}
	}
	if got := lists(); got != 2 {
		t.Fatalf("Apple listed the apps %d times, want 2: once, then once fresh", got)
	}
	held := p.HeldReach()
	if got := lists(); got != 2 || len(held) != 1 || len(held[0].Apps) != 2 || held[0].Apps[0].Name != "Notes" {
		t.Fatalf("the held reach is %+v after %d lists", held, got)
	}
}

// THE CONNECTION'S CARD SAYS WHAT IT HOLDS AND REACHES: each key's Apple role
// (the slot it was added under), and the apps it reaches by name with their
// bundle IDs: every app its keys list where the company names none, the named
// ones it lists where it names some, and the named ones by ID while unread.
func TestTheConnectionCardNamesItsRolesAndApps(t *testing.T) {
	t.Parallel()
	read := appstoreconnect.Reach{{Connection: "main", Read: true, Apps: []appstoreconnect.App{
		{ID: "111", Name: "Notes", BundleID: "com.example.notes"}, {ID: "222", Name: "Other"}}}}
	keys := config.AppStoreConnectKeys{Admin: adminOnly().Admin, Marketing: marketing().Marketing}
	open := appstoreconnect.Connections(reachCompany(keys, nil).Integrations.AppStoreConnect, read)
	if len(open) != 1 || !slices.Equal(open[0].RolesHeld, []string{appstoreconnect.RoleAdmin,
		appstoreconnect.RoleMarketing}) || !open[0].Open {
		t.Fatalf("an open connection's card: %+v", open)
	}
	if a := open[0].Accounts; len(a) != 2 || a[0].ID != "111" || a[0].Name != "Notes" ||
		a[0].Detail != "com.example.notes" || a[0].State != "" {
		t.Fatalf("an open connection reaches %+v", a)
	}
	named := appstoreconnect.Connections(reachCompany(keys, []string{"222", "333"}).Integrations.AppStoreConnect, read)
	if a := named[0].Accounts; named[0].Open || len(a) != 1 || a[0].ID != "222" || a[0].Name != "Other" {
		t.Fatalf("a connection naming 222 and 333 reaches %+v", a)
	}
	unread := appstoreconnect.Connections(reachCompany(adminOnly(), []string{"333"}).Integrations.AppStoreConnect,
		appstoreconnect.Reach{{Connection: "main", Unread: "no key lists apps"}})
	if a := unread[0].Accounts; len(a) != 1 || a[0].ID != "333" || unread[0].Unread != "no key lists apps" ||
		!slices.Equal(unread[0].RolesHeld, []string{appstoreconnect.RoleAdmin}) {
		t.Fatalf("an unread connection naming 333: %+v", unread[0])
	}
	if never := appstoreconnect.Connections(reachCompany(adminOnly(), nil).Integrations.AppStoreConnect, nil); len(
		never[0].Accounts) != 0 || !never[0].Open {
		t.Fatalf("an open connection never read: %+v", never[0])
	}
}
