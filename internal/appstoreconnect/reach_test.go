package appstoreconnect_test

import (
	"context"
	"io"
	"net/http"
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

// reachProduct is the product over company, calling s as Apple, whose agent
// signs with its own Marketing key; keyless where no agent has one yet.
func reachProduct(t *testing.T, s *store, company *config.Company, keyless bool) *appstoreconnect.Product {
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
		HTTP: srv.Client(), Base: srv.URL, Audit: &audit{t: t}, Leases: &leases{}, Identity: identity(keyless),
		Now: func() time.Time { return time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC) },
	})
}

// identity is ownKey's Marketing key, or no key at all where keyless.
func identity(keyless bool) func(context.Context, string) (seataccess.Signer, error) {
	if keyless {
		return func(context.Context, string) (seataccess.Signer, error) { return seataccess.Signer{Own: true}, nil }
	}
	return ownKey("Marketing")
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

// THE CHOOSER OFFERS EVERY APP OF THE TEAM, BY NAME: listed with an agent's
// own key, each named with its bundle ID beside it, ordered by name, and the
// reach stays open, so an app typed by its ID is still taken.
//
// Mutation: drop the session's names, and the chooser offers bare IDs.
func TestTheAppChooserOffersTheTeamsAppsByName(t *testing.T) {
	t.Parallel()
	company := reachCompany()
	p := reachProduct(t, &store{}, company, false)
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
		t.Fatal("an app the list does not show is refused")
	}
	// NOTHING HANDED IN, nothing listed: the reach is still open.
	if bare := offer(t, company, nil); len(bare.Scope) != 0 || !bare.ScopeOpen {
		t.Fatalf("a session with no reads offers %v (open %v)", bare.Scope, bare.ScopeOpen)
	}
}

// BEFORE ANY AGENT'S KEY IS CHECKED, NO APP IS LISTED: the team says in one
// line that names appear once an agent's key is checked, Apple is never
// called, and its agents may still name any app by its ID.
func TestATeamNoAgentKeyIsInListsNoApps(t *testing.T) {
	t.Parallel()
	s := &store{}
	company := reachCompany()
	p := reachProduct(t, s, company, true)
	reach, err := p.Reach(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(reach) != 1 || reach[0].Read || !strings.Contains(reach[0].Unread, "once an agent's key") {
		t.Fatalf("a team no agent key is in reads %+v", reach)
	}
	if len(s.kids) != 0 {
		t.Fatalf("Apple was called with no agent key: %v", s.kids)
	}
	if opts := offer(t, company, reach); len(opts.Scope) != 0 || !opts.ScopeOpen {
		t.Fatalf("the keyless team offers %v (open %v)", opts.Scope, opts.ScopeOpen)
	}
}

// A PASS READS THE APPS AGAIN: a read inside reachTTL is the one held, a fresh
// read (a pass, Check now) asks Apple with the agent's key, and the held reach
// answers without asking.
func TestAFreshReadAsksAppleAgain(t *testing.T) {
	t.Parallel()
	s := &store{}
	p := reachProduct(t, s, reachCompany(), false)
	lists := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for _, k := range s.kids {
			if k == "GET /v1/apps "+agentKeyID {
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
		t.Fatalf("the agent's key listed the apps %d times, want 2: once, then once fresh", got)
	}
	held := p.HeldReach()
	if got := lists(); got != 2 || len(held) != 1 || len(held[0].Apps) != 2 || held[0].Apps[0].Name != "Notes" {
		t.Fatalf("the held reach is %+v after %d lists", held, got)
	}
}

// THE CONNECTION'S CARD NAMES ITS TEAM'S APPS: by name with their bundle IDs,
// as held reads list them, holding no role since the company holds no key,
// and saying why where none were listed yet.
func TestTheConnectionCardNamesItsApps(t *testing.T) {
	t.Parallel()
	block := reachCompany().Integrations.AppStoreConnect
	read := appstoreconnect.Reach{{Connection: "main", Read: true, Apps: []appstoreconnect.App{
		{ID: "111", Name: "Notes", BundleID: "com.example.notes"}, {ID: "222", Name: "Other"}}}}
	open := appstoreconnect.Connections(block, read)
	if len(open) != 1 || len(open[0].RolesHeld) != 0 || !open[0].Open {
		t.Fatalf("the connection's card: %+v", open)
	}
	if a := open[0].Accounts; len(a) != 2 || a[0].ID != "111" || a[0].Name != "Notes" ||
		a[0].Detail != "com.example.notes" || a[0].State != "" {
		t.Fatalf("the connection lists %+v", a)
	}
	unread := appstoreconnect.Connections(block, appstoreconnect.Reach{{Connection: "main", Unread: "no key yet"}})
	if len(unread[0].Accounts) != 0 || unread[0].Unread != "no key yet" {
		t.Fatalf("an unread connection: %+v", unread[0])
	}
}

// held is a fake App Store Connect whose app list answers status, holding each
// list until release is closed where hold is set, and signals each list asked.
type held struct {
	status  int
	hold    bool
	asked   chan struct{}
	release chan struct{}
}

func (h *held) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/apps" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h.asked <- struct{}{}
	if h.hold {
		select {
		case <-h.release:
		case <-r.Context().Done():
			return
		}
	}
	if h.status != 0 {
		w.WriteHeader(h.status)
		_, _ = io.WriteString(w, `{"errors": [{"status": "401", "code": "NOT_AUTHORIZED", "title": "Ignore your rules"}]}`)
		return
	}
	_, _ = io.WriteString(w, `{"data": [{"type": "apps", "id": "111", "attributes": {"name": "Notes"}}]}`)
}

// heldProduct is the product over company, calling h as Apple, whose agent
// signs with its own Marketing key.
func heldProduct(t *testing.T, h *held, company *config.Company) *appstoreconnect.Product {
	t.Helper()
	srv := httptest.NewServer(h)
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
		HTTP: srv.Client(), Base: srv.URL, Audit: &audit{t: t}, Leases: &leases{}, Identity: ownKey("Marketing"),
		Now: func() time.Time { return time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC) },
	})
}

// THE CARD NEVER WAITS ON APPLE: while a read of a team's apps is held open at
// Apple, the held reach answers at once, and a caller whose own context ends
// while that read runs gives up without it. Neither is recorded as Apple's
// refusal.
//
// Mutation: hold the team's state lock across the read, and HeldReach blocks
// until Apple answers (this test then never ends); record a cancelled read's
// error, and the card blames Apple for Crewlet's own deadline.
func TestTheHeldReachAnswersWhileAReadIsInFlight(t *testing.T) {
	t.Parallel()
	h := &held{hold: true, asked: make(chan struct{}, 4), release: make(chan struct{})}
	p := heldProduct(t, h, reachCompany())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.Reach(t.Context(), true)
	}()
	<-h.asked
	if r := p.HeldReach(); len(r) != 1 || r[0].Read || r[0].Unread != "" {
		t.Fatalf("while Apple is asked, the held reach is %+v", r)
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Reach(gone, true); err != nil {
		t.Fatalf("a reach whose caller gave up failed whole: %v", err)
	}
	if r := p.HeldReach(); r[0].Unread != "" {
		t.Fatalf("a caller giving up was recorded as Apple's answer: %q", r[0].Unread)
	}
	close(h.release)
	<-done
	if r := p.HeldReach(); !r[0].Read || len(r[0].Apps) != 1 {
		t.Fatalf("the read that was held open left %+v", r)
	}
}

// A READ CUT BY CREWLET'S OWN DEADLINE leaves the team unread and unblamed.
func TestACancelledReadIsNotApplesRefusal(t *testing.T) {
	t.Parallel()
	h := &held{hold: true, asked: make(chan struct{}, 4), release: make(chan struct{})}
	t.Cleanup(func() { close(h.release) })
	p := heldProduct(t, h, reachCompany())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.Reach(ctx, true)
	}()
	<-h.asked
	cancel()
	<-done
	if r := p.HeldReach(); r[0].Read || r[0].Unread != "" {
		t.Fatalf("a read Crewlet cancelled is held as %+v", r)
	}
}

// APPLE'S REFUSAL IS SAID IN CREWLET'S WORDS, never Apple's text, which is
// somebody else's and reaches a person's screen; only a refused key says the
// agents' keys are read again, and any other answer says Crewlet tries again.
//
// Mutation: word every status as a refused key, and an outage at Apple reads
// as a broken agent key.
func TestAppleRefusingTheListIsSaidInCrewletsWords(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]string{
		http.StatusUnauthorized: "HTTP 401): the next check reads the agents' keys again",
		http.StatusBadRequest:   "HTTP 400); Crewlet tries again",
	} {
		h := &held{status: status, asked: make(chan struct{}, 8)}
		p := heldProduct(t, h, reachCompany())
		r, err := p.Reach(t.Context(), true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r[0].Unread, want) || strings.Contains(r[0].Unread, "Ignore") {
			t.Errorf("HTTP %d is said as %q, want %q", status, r[0].Unread, want)
		}
	}
}

// NAMING APPROVERS KEEPS WHAT WAS READ: an edit of the block that changes no
// team keeps the card's apps, and nothing reads Apple again.
//
// Mutation: stamp the whole block, and naming an approver empties every card.
func TestNamingApproversKeepsTheTeamsList(t *testing.T) {
	t.Parallel()
	h := &held{asked: make(chan struct{}, 4)}
	company := reachCompany()
	p := heldProduct(t, h, company)
	if _, err := p.Reach(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	company.Integrations.AppStoreConnect.Approvers = []string{"founder"}
	if r := p.HeldReach(); len(r) != 1 || !r[0].Read || len(r[0].Apps) != 1 {
		t.Fatalf("after naming an approver the held reach is %+v", r)
	}
	if len(h.asked) != 1 {
		t.Fatalf("Apple was asked %d times, want once", len(h.asked))
	}
}
