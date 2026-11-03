package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The cases here hold the SWEEP: a generation left open and idle — a release
// whose carry-out failed, or a process that ended mid-call — is carried out by
// the next generation to open in the workspace, whichever seat that is, and
// never one a call is still running in.

// A SEAT'S KEPT REFRESH REACHES THE NEXT SEAT TO START. Seat A's call refreshed
// the login, rotating the refresh token, and its export failed, so the working
// token was kept in A's home for A's next call — and every other seat that
// started meanwhile was seeded with the token the vendor had rotated away,
// for as long as A stayed idle.
func TestAnotherSeatsKeptRefreshReachesTheNextSeatToStart(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "export-failed-once")
	p := storeProvider(t, map[string]string{
		"FAKE_STORE_REFRESH": "k-new", "FAKE_STORE_EXPORT_FAIL_ONCE": marker,
	}, nil)
	writeDocument(t, p, `[{"id":"cred_1","key":"k-old"}]`+"\n")
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}
	if _, err := p.Complete(llm.WithSeat(t.Context(), "seat-a"), req); err != nil {
		t.Fatalf("seat a: %v", err)
	}
	if got := readDocument(t, p); strings.Contains(got, "k-new") {
		t.Fatalf("an export that failed was written: %q", got)
	}

	p.env = fakeChildEnv(map[string]string{"FAKE_STORE": "1"})
	comp, err := p.Complete(llm.WithSeat(t.Context(), "seat-b"), req)
	if err != nil {
		t.Fatalf("seat b: %v", err)
	}
	if comp.Content != "key=k-new" {
		t.Errorf("seat b held %q, want the login seat a refreshed", comp.Content)
	}
	if got := readDocument(t, p); !strings.Contains(got, "k-new") {
		t.Errorf("the shared login is %q, want seat a's refresh carried out", got)
	}
	if _, err := os.Stat(filepath.Join(p.ws.SeatDir("seat-a"), generationFile)); !os.IsNotExist(err) {
		t.Errorf("seat a's generation is still recorded as open after it was carried out (%v)", err)
	}
}

// THE SWEEP FINDS A KEPT REFRESH WHATEVER THE STATE DIRECTORY IS CALLED. The
// directory is the operator's path, and the sweep built a glob from it: a
// `[prod]` in it was a character class that matched no seat, and an unclosed
// `[` a malformed pattern whose error was dropped — either way seat a's kept
// refresh was never carried out, and seat b was seeded with the token the
// vendor had rotated away.
//
// Mutation: list the seats with filepath.Glob over the root again, and the two
// bracketed cases are red (a `*` or `?` in the path happens to match itself).
func TestASweepFindsAKeptRefreshUnderAStateDirectoryWithPatternCharacters(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"crewlet[prod]", "crewlet[x", "state*?"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			marker := filepath.Join(t.TempDir(), "export-failed-once")
			p := storeProvider(t, map[string]string{
				"FAKE_STORE_REFRESH": "k-new", "FAKE_STORE_EXPORT_FAIL_ONCE": marker,
			}, nil)
			dir := filepath.Join(t.TempDir(), name)
			t.Cleanup(func() { forgetWorkspace(dir) })
			ws, err := Shared(dir, p.harness, p.profile)
			if err != nil {
				t.Fatalf("Shared: %v", err)
			}
			p.ws = ws
			writeDocument(t, p, `[{"id":"cred_1","key":"k-old"}]`+"\n")
			req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}
			if _, err := p.Complete(llm.WithSeat(t.Context(), "seat-a"), req); err != nil {
				t.Fatalf("seat a: %v", err)
			}
			if got := readDocument(t, p); strings.Contains(got, "k-new") {
				t.Fatalf("the premise: an export that failed was written: %q", got)
			}

			p.env = fakeChildEnv(map[string]string{"FAKE_STORE": "1"})
			comp, err := p.Complete(llm.WithSeat(t.Context(), "seat-b"), req)
			if err != nil {
				t.Fatalf("seat b: %v", err)
			}
			if comp.Content != "key=k-new" {
				t.Errorf("seat b held %q under %q, want the login seat a refreshed", comp.Content, dir)
			}
		})
	}
}

// A SEATS DIRECTORY THE SWEEP CANNOT LIST IS SAID OUT LOUD, never read as a
// workspace with nothing open: every seat starting meanwhile is seeded from a
// shared login a kept refresh may have rotated away.
//
// NOT PARALLEL: it reads the process's log.
//
// Mutation: return without the warning when the listing fails, and this is red.
func TestASweepThatCannotListTheSeatsSaysSo(t *testing.T) {
	logs := captureLogs(t)
	ws := newWorkspace(t)
	// A file where the seats directory belongs: listing it fails, and not
	// because it does not exist.
	writeFile(t, filepath.Join(ws.Root(), "seats"), "not a directory")
	ws.sweep(t.Context(), &stubStore{}, "dev")
	if got := logs.String(); !strings.Contains(got, "harness_sweep_failed") {
		t.Errorf("a sweep that could not list the seats said nothing: %q", got)
	}

	logs.Reset()
	fresh := newWorkspace(t)
	fresh.sweep(t.Context(), &stubStore{}, "dev")
	if got := logs.String(); strings.Contains(got, "harness_sweep_failed") {
		t.Errorf("a workspace with no seats yet was reported as a failed sweep: %q", got)
	}
}

// A GENERATION AN EARLIER PROCESS LEFT OPEN IS CARRIED OUT BY ANY SEAT. A
// restarted engine has no memory of the seat whose call it died in, so the
// record on disk is what finds it — or that seat's refresh waits for that seat.
func TestAGenerationAnEarlierProcessLeftOpenIsCarriedOutByAnySeat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profile := testProfile(t)
	open := func() *Workspace {
		ws, err := Shared(dir, "fake-cli", profile)
		if err != nil {
			t.Fatalf("Shared: %v", err)
		}
		return ws
	}
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws := open()
	writeCreds(t, &Provider{ws: ws}, `{"token":"R0"}`)
	co, err := ws.Acquire(t.Context(), "seat-a", "call", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	refreshFile(t, co, `{"token":"R1"}`)
	endProcess(co)
	forgetWorkspace(dir)

	next, err := open().Acquire(t.Context(), "seat-b", "call", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = next.Release(t.Context()) }()
	if got := readCreds(t, &Provider{ws: ws}); got != `{"token":"R1"}` {
		t.Errorf("the shared login is %q, want the refresh the earlier process never carried out", got)
	}
	if got, err := os.ReadFile(filepath.Join(next.Home, ".fake", "creds.json")); err != nil || string(got) != `{"token":"R1"}` {
		t.Errorf("seat b was seeded with %q (%v), want the refresh carried out before its seed", got, err)
	}
}

// A SWEEP NEVER CARRIES OUT A HOME A CALL IS RUNNING IN — in this process,
// where the seat is in flight, or in another, where only the seat's generation
// lock says so. Carried out mid-call, a refresh the harness had not finished
// writing would be read as the login, and the record the running generation
// releases against would be gone.
func TestASweepNeverCarriesOutAHomeACallIsRunningIn(t *testing.T) {
	t.Parallel()
	t.Run("in this process", func(t *testing.T) {
		t.Parallel()
		ws := newWorkspace(t)
		writeCreds(t, &Provider{ws: ws}, `{"token":"R0"}`)
		a, err := ws.Acquire(t.Context(), "seat-a", "call", nil)
		if err != nil {
			t.Fatalf("Acquire a: %v", err)
		}
		refreshFile(t, a, `{"token":"R1"}`)
		b, err := ws.Acquire(t.Context(), "seat-b", "call", nil)
		if err != nil {
			t.Fatalf("Acquire b: %v", err)
		}
		defer func() { _ = b.Release(t.Context()) }()
		if got := readCreds(t, &Provider{ws: ws}); got != `{"token":"R0"}` {
			t.Errorf("a sweep carried out a seat that was mid-call: %q", got)
		}
		if err := a.Release(t.Context()); err != nil {
			t.Fatalf("Release a: %v", err)
		}
		if got := readCreds(t, &Provider{ws: ws}); got != `{"token":"R1"}` {
			t.Errorf("seat a's own release did not carry its refresh out: %q", got)
		}
	})
	t.Run("in another process", func(t *testing.T) {
		t.Parallel()
		ws := newWorkspace(t)
		writeCreds(t, &Provider{ws: ws}, `{"token":"R0"}`)
		// Another process's generation of seat "other": its home holds a
		// refresh, its record is on disk, and its lock is held — by a second
		// open file description, which an flock treats as another process.
		other := ws.SeatDir("other")
		writeFile(t, filepath.Join(other, "home", ".fake", "creds.json"), `{"token":"R1"}`)
		if err := writeGeneration(filepath.Join(other, generationFile), generation{
			Mode: syncRefresh, Seeded: map[string]string{"creds.json": digestOf([]byte(`{"token":"R0"}`))},
		}); err != nil {
			t.Fatal(err)
		}
		release, err := hostbox.LockFile(t.Context(), filepath.Join(other, seatLockFile), 0, "the test")
		if err != nil {
			t.Fatalf("LockFile = %v", err)
		}
		b, err := ws.Acquire(t.Context(), "seat-b", "call", nil)
		if err != nil {
			t.Fatalf("Acquire b: %v", err)
		}
		if got := readCreds(t, &Provider{ws: ws}); got != `{"token":"R0"}` {
			t.Errorf("a sweep carried out another process's running generation: %q", got)
		}
		if err := b.Release(t.Context()); err != nil {
			t.Fatalf("Release b: %v", err)
		}

		// The other process ends; the next seat to start carries it out.
		release()
		c, err := ws.Acquire(t.Context(), "seat-c", "call", nil)
		if err != nil {
			t.Fatalf("Acquire c: %v", err)
		}
		defer func() { _ = c.Release(t.Context()) }()
		if got := readCreds(t, &Provider{ws: ws}); got != `{"token":"R1"}` {
			t.Errorf("the generation the other process left was not carried out once it ended: %q", got)
		}
	})
}

// A SEAT'S FIRST CALL NEVER PRUNES A HOME ANOTHER PROCESS IS RUNNING IN. An
// engine whose lease on the seat moved may still be finishing a call there;
// the call that finds the seat's generation lock held waits for it, and is
// refused as a timeout rather than seeding the home under the other harness.
func TestASeatsFirstCallWaitsForAnotherProcessesGeneration(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	dir := ws.SeatDir("dev")
	running := filepath.Join(dir, "home", ".fake", "sessions", "live.jsonl")
	writeFile(t, running, "a call in progress")
	release, err := hostbox.LockFile(t.Context(), filepath.Join(dir, seatLockFile), 0, "the test")
	if err != nil {
		t.Fatalf("LockFile = %v", err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if co, err := ws.Acquire(ctx, "dev", "call", nil); err == nil {
		_ = co.Release(t.Context())
		t.Fatal("a call opened a generation in a home another process holds")
	} else if kind := acquireFailureKind(err); kind != llm.KindTimeout {
		t.Errorf("kind = %v, want a timeout the chain moves past: %v", kind, err)
	}
	if _, err := os.Stat(running); err != nil {
		t.Errorf("the other process's session was pruned from under it: %v", err)
	}
}

// A SWEEP PACES A SEAT IT CANNOT CARRY OUT. The first attempt is immediate —
// that is the point of sweeping — but every attempt is an export made inside
// some other seat's call, and a carry that fails for good must not cost every
// seat's first call one.
func TestASweepPacesASeatItCannotCarryOut(t *testing.T) {
	t.Parallel()
	ws := storeWorkspace(t)
	writeLogin(t, ws, `{"token":"R0"}`, `[{"id":"c","refresh":"R0"}]`)
	stuck := ws.SeatDir("stuck")
	store := &routedStore{fail: filepath.Join(stuck, "home")}

	a, err := ws.Acquire(t.Context(), "stuck", "call", store)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := a.Release(t.Context()); err == nil {
		t.Fatal("a release whose export failed reported success")
	}
	start := func(seat string) {
		t.Helper()
		co, err := ws.Acquire(t.Context(), seat, "call", store)
		if err != nil {
			t.Fatalf("Acquire %s: %v", seat, err)
		}
		if err := co.Release(t.Context()); err != nil {
			t.Fatalf("Release %s: %v", seat, err)
		}
	}
	start("seat-b")
	if got := store.exportsOf(stuck); got != 2 {
		t.Fatalf("exports of the stuck home = %d, want the release's and one immediate sweep", got)
	}
	start("seat-c")
	if got := store.exportsOf(stuck); got != 2 {
		t.Errorf("exports of the stuck home = %d: a sweep tried again inside its backoff", got)
	}

	// The backoff runs out; the next seat to start tries again.
	ws.mu.Lock()
	r := ws.retries[seatSlug("stuck")]
	r.next = time.Now().Add(-time.Second)
	ws.retries[seatSlug("stuck")] = r
	ws.mu.Unlock()
	start("seat-d")
	if got := store.exportsOf(stuck); got != 3 {
		t.Errorf("exports of the stuck home = %d, want one more once the backoff ran out", got)
	}
	if _, err := os.Stat(filepath.Join(stuck, generationFile)); err != nil {
		t.Errorf("a generation that was never carried out lost its record: %v", err)
	}
}

// A SEAT'S OWN CARRY MOVES THE SWEEP'S PACE. Its own next call trying and
// failing is a failed attempt like a sweep's, so the sweeps back off from it;
// and its own call succeeding clears the backoff, so a later failed release is
// swept at once again rather than waiting out a pace that no longer applies.
func TestASeatsOwnCarryMovesTheSweepsPace(t *testing.T) {
	t.Parallel()
	ws := storeWorkspace(t)
	writeLogin(t, ws, `{"token":"R0"}`, `[{"id":"c","refresh":"R0"}]`)
	stuck := ws.SeatDir("stuck")
	store := &routedStore{fail: filepath.Join(stuck, "home")}
	acquire := func(seat string) (*Checkout, error) {
		t.Helper()
		return ws.Acquire(t.Context(), seat, "call", store)
	}
	start := func(seat string) {
		t.Helper()
		co, err := acquire(seat)
		if err != nil {
			t.Fatalf("Acquire %s: %v", seat, err)
		}
		if err := co.Release(t.Context()); err != nil {
			t.Fatalf("Release %s: %v", seat, err)
		}
	}

	co, err := acquire("stuck")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	_ = co.Release(t.Context()) // fails: export 1
	if _, err := acquire("stuck"); err == nil {
		t.Fatal("a seat ran on top of a login it could not carry out")
	} // its own retry fails: export 2
	start("seat-b")
	if got := store.exportsOf(stuck); got != 2 {
		t.Errorf("exports of the stuck home = %d: a sweep did not back off from the seat's own failed retry", got)
	}

	// The seat's export works again: its own call carries out, and the pace
	// it had built up goes with it.
	store.failing("")
	co, err = acquire("stuck") // export 3, carried out
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	store.failing(filepath.Join(stuck, "home"))
	_ = co.Release(t.Context()) // fails again: export 4
	start("seat-c")             // sweeps at once: export 5
	if got := store.exportsOf(stuck); got != 5 {
		t.Errorf("exports of the stuck home = %d: a sweep waited out a pace the seat's own carry had cleared", got)
	}
}

// THE BACKOFF doubles from carryRetryFloor and stops at carryRetryCeiling.
func TestTheSweepsBackoffDoublesToItsCeiling(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		30 * time.Minute, 30 * time.Minute,
	}
	for i, w := range want {
		if got := ws.carryFailed("dev"); got != w {
			t.Errorf("failure %d: backoff %s, want %s", i+1, got, w)
		}
	}
	ws.carried("dev")
	if !ws.sweepDue("dev") {
		t.Error("a seat carried out is still being backed off")
	}
	if got := ws.carryFailed("dev"); got != time.Minute {
		t.Errorf("after a success the backoff starts at %s, want the floor again", got)
	}
}

// routedStore is a [loginStore] whose export fails for one home and holds no
// login for any other, and that counts the exports made of each.
type routedStore struct {
	fail string

	mu      sync.Mutex
	exports map[string]int
}

func (s *routedStore) importLogin(context.Context, string, string, []byte) error { return nil }

func (s *routedStore) exportLogin(_ context.Context, home, _ string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exports == nil {
		s.exports = map[string]int{}
	}
	s.exports[home]++
	if home == s.fail {
		return nil, &auxiliaryExitError{command: "fake export", code: 1, stderr: "export broke"}
	}
	return []byte("[]"), nil
}

// failing sets the one home whose export fails, "" for none.
func (s *routedStore) failing(home string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = home
}

func (s *routedStore) providerKey() string { return "routed" }

// exportsOf is how many exports were made of the home in a seat directory.
func (s *routedStore) exportsOf(seatDir string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exports[filepath.Join(seatDir, "home")]
}

// writeFile writes body at path, creating its parents.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
