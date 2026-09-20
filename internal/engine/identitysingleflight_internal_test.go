package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/jira"
)

// countingForge answers one identity endpoint, counts the calls, and holds the
// FIRST of them open until the test says otherwise.
//
// The hold is what makes this a test of the cache rather than of the
// scheduler: two goroutines racing a miss overlap only sometimes, which is why
// the e2e case that covers this caught the race about one run in ten, while
// holding the first lookup open makes the overlap the overwhelmingly likely
// interleaving rather than a rare one.
//
// OVERWHELMINGLY LIKELY IS NOT CERTAIN, and this says so rather than claiming
// otherwise: the hold is released once the FIRST request lands, and nothing
// here observes the second caller, so a second caller that is not scheduled
// until the first has stored is an ordinary cache hit and never reaches the
// waiting path. [TestAWaiterIsProvablyParkedOnAPeersClaim] is the case with an
// actual barrier behind it; these three are the breadth across the surfaces.
type countingForge struct {
	url   string
	calls atomic.Int64

	arrived chan struct{} // closed by the first call, once it is in flight
	release chan struct{} // closed by let, to let the held calls answer
	let     func()        // closes release, once, however often it is called
}

func startCountingForge(t *testing.T, path, body string) *countingForge {
	t.Helper()
	f := &countingForge{arrived: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		f.calls.Add(1)
		once.Do(func() { close(f.arrived) })
		<-f.release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	f.let = unblockOnCleanup(t, f.release)
	f.url = srv.URL
	return f
}

// unblockOnCleanup returns a function that closes gate exactly once, and
// arranges for the test's end to call it if the test never did.
//
// REGISTERED AFTER the server's own Close, because [testing.T.Cleanup] runs
// LAST-REGISTERED-FIRST and the order is the whole point: a handler parked on
// this gate is an outstanding request, and [httptest.Server.Close] waits for
// those for ever. A test that fails BEFORE its own release — a barrier that
// timed out, an assertion that fataled — would otherwise hang rather than
// fail. Measured: a forced early failure in the parked-waiter case turned a
// one-line FAIL into a 45-second timeout panic, and CI runs the suite at
// -timeout 30m.
//
// ONCE, so the test's own release and this one cannot double-close.
func unblockOnCleanup(t *testing.T, gate chan struct{}) func() {
	t.Helper()
	var once sync.Once
	let := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(let)
	return let
}

// raceTwo runs resolve twice at once against f, releasing the held lookup only
// once it is in flight, and returns WHAT EACH CALLER SAW when its own resolve
// returned.
//
// Each caller's own observation, rather than one reading taken afterwards:
// the caller that waited is the one this is about, and a reading taken after
// both have finished is satisfied by the holder's store no matter what the
// waiter did. Measured — a version of this that read the cache at the end
// passed against a resolve that never waited at all.
func raceTwo(t *testing.T, f *countingForge, resolve func() string) []string {
	t.Helper()
	seen := make([]string, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() { seen[i] = resolve() })
	}
	select {
	case <-f.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("no identity lookup arrived at the forge")
	}
	f.let()
	wg.Wait()
	return seen
}

// ONE CREDENTIAL IS BOUGHT ONCE, HOWEVER MANY CALLERS WANT IT.
//
// Each cache's own doc promises it — "A rotated token is a cache miss and
// costs exactly one request" — and it did not hold. resolve read the map under
// the lock, RELEASED it around the network lookup, and re-locked to store, so
// two callers that both missed on one credential both bought it.
//
// Two concurrent callers is the ordinary case rather than a contrived one.
// Every surface has a start path and a rewire path and both run at boot:
// startIntegrations arms the reconcile loop (whose pass reaches rewire*)
// twenty-six lines before startNotifications reaches the start path, and the
// two were measured ~11 ms apart.
//
// AND A CALLER THAT WAITED HAS THE ANSWER when resolve returns. Every one of
// the six call sites goes on to register the seat from this cache's own
// snapshot, in the same call with nothing re-resolving in between, so
// returning early while a peer's lookup was still in flight would bind that
// seat to nobody — a misroute no later pass reports, because the cache is
// populated by then and the next resolve is a hit.
//
// ALL THREE CACHES, because all three carried the identical shape and
// therefore the identical race: a fix to one would have left two copies of the
// bug behind the same promise.
func TestOneCredentialIsResolvedOnceForConcurrentCallers(t *testing.T) {
	t.Parallel()
	const email = "ceo@example.com"
	cred := atlassian.Credential{Email: email, Token: "atl-ceo"}
	for name, tc := range map[string]struct {
		path, body string
		// resolve races two calls and returns what each SAW on return.
		resolve func(t *testing.T, f *countingForge) []string
		want    string
	}{
		"gitlab": {
			path: "/api/v4/user", body: `{"username":"Ceo-Bot"}`,
			want: "ceo-bot", // FOLDED by the client, which is what a mention carries.
			resolve: func(t *testing.T, f *countingForge) []string {
				ids := &gitlabIdentities{}
				return raceTwo(t, f, func() string {
					ids.resolve(context.Background(), f.url, []string{"glpat-ceo"})
					return ids.snapshot()["glpat-ceo"]
				})
			},
		},
		"github": {
			path: "/user", body: `{"login":"Ceo-Bot"}`,
			want: "ceo-bot",
			resolve: func(t *testing.T, f *countingForge) []string {
				ids := &githubIdentities{}
				return raceTwo(t, f, func() string {
					ids.resolve(context.Background(), f.url, f.url, []string{"ghp-ceo"})
					return ids.snapshot()["ghp-ceo"]
				})
			},
		},
		"jira": {
			path: "/rest/api/3/myself", body: `{"accountId":"5b10a2"}`,
			want: "5b10a2",
			resolve: func(t *testing.T, f *countingForge) []string {
				ids := &jiraIdentities{}
				return raceTwo(t, f, func() string {
					ids.resolve(context.Background(), f.url, jira.Cloud,
						[]atlassian.Credential{cred})
					return ids.snapshot()[cred]
				})
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := startCountingForge(t, tc.path, tc.body)
			seen := tc.resolve(t, f)
			if got := f.calls.Load(); got != 1 {
				t.Errorf("two callers racing one unknown credential spent %d "+
					"lookups, want the 1 the cache's doc promises", got)
			}
			for i, account := range seen {
				if account != tc.want {
					t.Errorf("caller %d saw %q when its resolve returned, want %q: "+
						"it registers whatever the snapshot holds when resolve "+
						"returns, so a caller that did not wait for the peer "+
						"holding the lookup binds that seat to nobody",
						i, account, tc.want)
				}
			}
		})
	}
}

// A LOOKUP THAT FAILED RELEASES ITS CLAIM, so the next pass buys it again.
//
// This is the one way a single-flight cache can be worse than the race it
// removes: a claim kept after a failure makes that credential permanently
// unbuyable, and every later caller either blocks on it for ever or returns
// with nothing while the cache reports itself busy. The surfaces' own contract
// is the opposite — "a seat whose lookup FAILS is left unresolved rather than
// failing the boot: the instance may be briefly down, and the next pass
// retries" — so a failure must leave the cache exactly as it found it.
func TestAFailedLookupIsRetriedByTheNextPass(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	var down atomic.Bool
	down.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if down.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"username":"ceo-bot"}`))
	}))
	t.Cleanup(srv.Close)

	ids := &gitlabIdentities{}
	ids.resolve(t.Context(), srv.URL, []string{"glpat-ceo"})
	if got := ids.snapshot()["glpat-ceo"]; got != "" {
		t.Fatalf("a refused lookup cached %q; an unresolved seat must stay unresolved", got)
	}

	// THE NEXT PASS, against an instance that is back. A claim still held
	// would either hang here or answer with nothing.
	down.Store(false)
	ids.resolve(t.Context(), srv.URL, []string{"glpat-ceo"})
	if got := ids.snapshot()["glpat-ceo"]; got != "ceo-bot" {
		t.Errorf("the retry resolved to %q, want the account: the failed lookup's "+
			"claim was never released", got)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("the credential was bought %d times, want 2: once refused, once retried", got)
	}
}

// AND A CALLER IS NOT HELD BY A PEER'S SLOW INSTANCE PAST ITS OWN DEADLINE.
//
// The wait exists so a caller registers what it asked for, not so it inherits
// somebody else's patience: the holder's lookup runs under the holder's
// context, and a waiter whose own caller has given up returns with nothing —
// exactly as a failed lookup does — rather than blocking on an instance that
// is not answering.
func TestAWaiterIsNotHeldPastItsOwnDeadline(t *testing.T) {
	t.Parallel()
	forge := startCountingForge(t, "/api/v4/user", `{"username":"ceo-bot"}`)
	ids := &gitlabIdentities{}

	held := make(chan struct{})
	go func() {
		defer close(held)
		ids.resolve(context.Background(), forge.url, []string{"glpat-ceo"})
	}()
	select {
	case <-forge.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("no identity lookup arrived at the forge")
	}

	// A SECOND CALLER ARRIVES while the first is still in flight, and its
	// own context is already done.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ids.resolve(cancelled, forge.url, []string{"glpat-ceo"})
	}()
	// WELL INSIDE THE HOLDER'S OWN TIMEOUT, which is the whole point: the
	// vendor clients carry one, so a window longer than it is satisfied by
	// the holder giving up rather than by the waiter respecting its own
	// deadline. Measured — at thirty seconds this passed against a resolve
	// that ignored the waiter's context entirely, because the held lookup
	// timed out at ten and released the claim.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("a caller whose own context was cancelled was held by a peer's lookup")
	}

	forge.let()
	<-held

	// AND NOTHING IS ASSERTED ABOUT THE FORGE HERE. A request issued on an
	// already-cancelled context fails before it dials, so a count taken
	// after this proves nothing about whether the waiter tried: it reads 1
	// against this implementation and against one with no single-flight at
	// all. What this case uniquely holds is the release above; the request
	// count is [TestOneCredentialIsResolvedOnceForConcurrentCallers]'s.
}

// A WAITER THAT IS PROVABLY PARKED ON A PEER'S CLAIM, with a barrier behind
// the word rather than a likely interleaving.
//
// The barrier is a SECOND CREDENTIAL that the waiter itself owns. resolve
// walks its keys under the lock — claiming what is free, collecting what a
// peer holds — and only then issues its own lookups, so the waiter's request
// for its own credential CANNOT reach the forge until it has already decided
// to wait for the peer's. That request arriving is the proof, and it is what
// releases the hold.
func TestAWaiterIsProvablyParkedOnAPeersClaim(t *testing.T) {
	t.Parallel()
	const held, owned = "glpat-held", "glpat-owned"

	var calls sync.Map // token -> *atomic.Int64
	count := func(token string) int64 {
		n, _ := calls.LoadOrStore(token, &atomic.Int64{})
		return n.(*atomic.Int64).Load()
	}
	hold := make(chan struct{})
	parked := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("PRIVATE-TOKEN")
		n, _ := calls.LoadOrStore(token, &atomic.Int64{})
		n.(*atomic.Int64).Add(1)
		if token == held {
			<-hold
		} else {
			// THE WAITER'S OWN LOOKUP, which it could only have
			// reached past the claim check.
			once.Do(func() { close(parked) })
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"username":"` + token + `"}`))
	}))
	t.Cleanup(srv.Close)
	// AFTER Close, so it runs BEFORE it: the holder's handler is parked on
	// hold, and a barrier below that fatals would otherwise leave Close
	// waiting on it for the rest of the suite's timeout.
	let := unblockOnCleanup(t, hold)

	ids := &gitlabIdentities{}
	holder := make(chan struct{})
	go func() {
		defer close(holder)
		ids.resolve(context.Background(), srv.URL, []string{held})
	}()
	// THE HOLDER IS IN FLIGHT, so the claim on `held` is outstanding and
	// nothing can have been stored for it.
	waitFor(t, "the holder's lookup to reach the forge", func() bool { return count(held) == 1 })

	var seen string
	waiter := make(chan struct{})
	go func() {
		defer close(waiter)
		ids.resolve(context.Background(), srv.URL, []string{held, owned})
		seen = ids.snapshot()[held]
	}()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the waiter never issued its own lookup, so it never passed the claim check")
	}

	let()
	<-holder
	<-waiter

	if got := count(held); got != 1 {
		t.Errorf("the held credential was bought %d times, want 1: the waiter "+
			"issued its own request for a credential a peer had claimed", got)
	}
	if seen != held {
		t.Errorf("the waiter saw %q for the peer's credential, want %q: it "+
			"returned before the claim it was parked on was stored", seen, held)
	}
}

// waitFor polls until cond holds, and fails the test rather than hanging.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ONE CALL THAT NAMES A CREDENTIAL TWICE STILL RETURNS.
//
// resolve claims what is free and collects what is already claimed, and the
// second mention of a repeated credential collects THIS CALL'S OWN claim —
// whose channel only this call's own store loop will close. So the lookups
// must run before the wait, and that ordering is load-bearing rather than
// tidy: moved the other way this self-deadlocks, for ever against a caller
// whose context has no deadline, which is what most of the production call
// sites' ancestors amount to. Nothing else in this suite passes a repeated
// key, so nothing else pins it.
func TestACredentialNamedTwiceInOneCallResolvesOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"username":"ceo-bot"}`))
	}))
	t.Cleanup(srv.Close)

	ids := &gitlabIdentities{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ids.resolve(context.Background(), srv.URL, []string{"glpat-ceo", "glpat-ceo", "glpat-ceo"})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		// A DEADLOCK IS THE FAILURE, and a bare hang would be a
		// thirty-minute suite timeout naming nothing.
		t.Fatal("a call naming one credential three times never returned: it " +
			"waited on a claim only it could release")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("one credential named three times was bought %d times, want 1", got)
	}
	if got := ids.snapshot()["glpat-ceo"]; got != "ceo-bot" {
		t.Errorf("the credential resolved to %q, want the account", got)
	}
}

// SNAPSHOT HANDS OUT A COPY, and that is a data race rather than a nicety.
//
// register and unresolved walk every seat in the org against what this
// returns, while the OTHER boot path — the concurrency this cache exists for —
// can be inside the store loop writing the same map. Handing out the live map
// makes that a genuine concurrent map read and write, reported by the detector
// only on the interleavings that happen to overlap: the same one-run-in-ten
// symptom this whole change removes, reintroduced one layer down.
func TestASnapshotIsACopyTheCallerMayHold(t *testing.T) {
	t.Parallel()
	ids := &gitlabIdentities{identityCache[string]{known: map[string]string{"glpat-ceo": "ceo-bot"}}}

	taken := ids.snapshot()
	taken["glpat-ceo"] = "somebody-else"
	delete(taken, "glpat-ceo")
	taken["glpat-new"] = "invented"

	again := ids.snapshot()
	if again["glpat-ceo"] != "ceo-bot" {
		t.Errorf("the cache holds %q after a caller edited its snapshot, want the "+
			"account: the snapshot is the live map", again["glpat-ceo"])
	}
	if _, invented := again["glpat-new"]; invented {
		t.Error("a caller's write to its snapshot reached the cache")
	}
}
