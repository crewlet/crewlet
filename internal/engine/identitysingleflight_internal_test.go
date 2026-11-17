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
	"github.com/crewlet/crewlet/internal/atlassianhost"
)

// countingForge answers one identity endpoint, counts the calls, and holds the
// FIRST of them open until the test says otherwise, so two racing misses
// overlap almost always rather than sometimes. Almost always is not always: a
// second caller not scheduled until the first has stored is an ordinary cache
// hit. [TestAWaiterIsProvablyParkedOnAPeersClaim] is the case with a real
// barrier; these three are the breadth across the surfaces.
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
// Register it AFTER the server's own Close: cleanups run last-registered-first,
// and [httptest.Server.Close] waits for ever on a handler parked on this gate,
// so a test that fails before its own release would hang rather than fail.
func unblockOnCleanup(t *testing.T, gate chan struct{}) func() {
	t.Helper()
	var once sync.Once
	let := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(let)
	return let
}

// raceTwo runs resolve twice at once against f, releasing the held lookup only
// once it is in flight, and returns what each caller saw when its own resolve
// returned: a reading taken after both finish is satisfied by the holder's
// store whatever the waiter did.
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

// ONE CREDENTIAL IS BOUGHT ONCE, HOWEVER MANY CALLERS WANT IT, as each cache's
// doc promises. Two concurrent callers is the ordinary case: every surface's
// start path and rewire path both run at boot (startIntegrations' reconcile
// pass and startNotifications).
//
// AND A CALLER THAT WAITED HAS THE ANSWER when resolve returns: every call site
// registers the seat from this cache's snapshot straight after, so returning
// while a peer's lookup is in flight binds that seat to nobody, a misroute no
// later pass reports because the next resolve is a hit.
//
// All three caches, because all three share the shape.
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
					ids.resolve(context.Background(), f.url, atlassianhost.Cloud,
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

// A LOOKUP THAT FAILED RELEASES ITS CLAIM, so the next pass buys it again: a
// claim kept after a failure makes that credential permanently unbuyable,
// against the surfaces' contract that a failed lookup leaves the seat
// unresolved for the next pass to retry.
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

// AND A CALLER IS NOT HELD BY A PEER'S SLOW INSTANCE PAST ITS OWN DEADLINE: the
// holder's lookup runs under the holder's context, and a waiter whose own
// context is done returns with nothing, as a failed lookup does.
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
	// WELL INSIDE THE HOLDER'S OWN TIMEOUT (the vendor clients' ten seconds):
	// a longer window passes against a resolve that ignores the waiter's
	// context, once the holder times out and releases the claim.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("a caller whose own context was cancelled was held by a peer's lookup")
	}

	forge.let()
	<-held

	// NOTHING IS ASSERTED ABOUT THE FORGE HERE: a request on an
	// already-cancelled context fails before it dials, so the count reads 1
	// with or without single-flight. The count is
	// [TestOneCredentialIsResolvedOnceForConcurrentCallers]'s.
}

// A WAITER THAT IS PROVABLY PARKED ON A PEER'S CLAIM. The barrier is a second
// credential the waiter owns: resolve claims and collects under the lock before
// issuing its own lookups, so the waiter's request for its own credential
// reaching the forge proves it already decided to wait for the peer's, and
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
	// After Close, so it runs before it (unblockOnCleanup).
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

// ONE CALL THAT NAMES A CREDENTIAL TWICE STILL RETURNS. The second mention
// collects this call's own claim, whose channel only this call's store loop
// closes, so the lookups must run before the wait: the other way round it
// self-deadlocks, for ever under a context with no deadline. Nothing else here
// passes a repeated key.
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

// SNAPSHOT HANDS OUT A COPY: register and unresolved walk it while the other
// boot path may be in the store loop writing the same map, a concurrent map
// read and write the race detector catches only on overlapping interleavings.
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
