// Package coordtest is the contract suite every coord.Backend must pass.
//
// One suite, every backend — the in-memory twin, the embedded KV, any
// external coordination store. A backend the suite has not certified does not
// exist as far as the engine is concerned, and a semantic divergence between
// two backends becomes a failing test here instead of a production-only
// surprise on the one node that happens to run the other one.
//
// The cases are named for the invariant they defend, and those names are the
// documentation: the epoch that must not reset on release, the release that
// must not clear its successor's lease, the renew that must not report loss
// because the store was unreachable for two seconds. The last one is not
// hypothetical — in the Python engine an unreachable store answered exactly
// as a peer holding the resource does, and a blip had a node logging "another
// process holds this node id's presence lease" at an operator while it
// quietly stopped refreshing its own presence.
//
// # Bringing up a new backend: if a case fails, suspect the case
//
// Not politeness — the base rate. Every time this suite has disagreed with a
// backend that was not the twin, the suite has been wrong more often than the
// backend, and each instance below cost its author an investigation before
// anyone thought to doubt the assertion:
//
//   - It required a DEFINITE answer from a contended claim. A compare-and-swap
//     store under a stampede loses every swap inside its retry budget and
//     honestly cannot say whether a peer won or a record lapsed underneath it.
//     Unknown is the contract's third answer, available at any moment; the
//     suite was demanding the twin's single-mutex behaviour from everyone.
//   - It read an omitted Protocol as the OLDEST version, ported from a Python
//     keyword default of 1. Go's struct zero inverts that risk, and the
//     contract now says an omitted protocol claims at THIS build.
//   - It asserted a gate-refused claim leaves the record pristine. d-201 §3
//     permits a KV to touch it — check → claim → re-check → release means the
//     claim was made and given back.
//   - It asked for a TTL longer than LongTTL. A store sizes its retention to
//     the suite's advertised maximum and is RIGHT to refuse a TTL it cannot
//     honour rather than silently clamp it.
//   - It kept every meta payload JSON-shaped and called that a courtesy, which
//     is why no case could see that a value's Go type does not survive a real
//     round trip.
//
// So before concluding a backend is at fault: grep rewrite/decisions/ for the
// operation, and read rewrite/questions/coord-contract-*.md. If the case turns
// out to encode what the twin happens to do, the case is the defect. Two
// backends agreeing is not evidence when the suite is what made them agree.
//
// # Checks that do not depend on you being careful
//
// The section above is doctrine, and doctrine is the part that demonstrably
// does not work: it was written here first, read by its own author, and this
// suite then shipped four cases requiring more of a backend than the contract
// allows. Not for want of care — a blind spot cannot be inspected from inside
// itself, and a suite's fixtures, constants, helpers and doctrine are written
// by the same mind as its cases, so they fail together.
//
// What actually caught things was mechanical. Every entry below is here
// because it found something careful reading had already missed, in this
// package:
//
//   - A MUTATION MUST LAND IN THE BRANCH IT IS PROVING. Forcing a TTL to 1ns
//     to exercise a timing branch produced measurement noise instead; sleeping
//     before a claim to simulate a slow store consumed none of the TTL,
//     because the lease only starts when the record is written. Two
//     "verifications" that reached nothing they claimed to.
//
//   - A HARNESS MUST TELL "BUILD FAILED" FROM "CAUGHT", and both from "DID NOT
//     LAND". Keying on a non-zero exit made every non-compiling mutation
//     report as caught; thirty-six results rested on that until they were
//     re-run three-state. The fourth state is the dangerous one: a patch whose
//     anchor no longer matches leaves the file untouched, so an unmodified
//     tree passes and the harness calls it a HOLE. Three were reported that
//     way here — two guards living in a shared helper, and one whose
//     signature contains a close paren inside its return type, breaking the
//     pattern. A false green is a missed catch; a false finding sends someone
//     to change working code. Compare a marker count before and after, and
//     report a patch that did not apply as its own verdict.
//
//   - A CONTROL SET NEEDS A ROW EXPECTED TO COME BACK CLEAN, for a reason
//     written down. Without one, a harness biased toward CAUGHT and a suite
//     that catches everything are indistinguishable. The documented meta gap
//     is that row, and it re-checks the gap's boundary on every run.
//
//   - ANY INVARIANT ABOUT WHERE CODE SITS BELONGS ON THE SYNTAX TREE. A
//     fixed-window grep under-reports by stopping early; a brace scan
//     over-reports by running past a closure's end. Both are line-oriented
//     tools answering a question about scope.
//
//   - A GUARD ASSERTING AN ABSENCE MUST ASSERT IT MATCHED ITS SUBJECT. "Found
//     no violations" and "scanned nothing" are the same green otherwise.
//
//   - A DIAGNOSTIC MUST NOT ASSERT A CAUSE IT CANNOT DISTINGUISH. Two helpers
//     here blamed the suite and the backend respectively on no evidence beyond
//     a timer firing; both now report the measurement and name what each
//     reading implies.
//
//   - TEST A GUARD'S STATED LIMITS, NOT ONLY ITS GUARANTEE. A "what this does
//     not catch" paragraph is the claim with the least scrutiny on it
//     anywhere in a suite: it reads as candour, so it is read past rather
//     than checked, while an over-claim gets challenged on sight. Both of
//     this package's are now landing-checked and both held — but a teammate
//     audited theirs and found the named-as-uncaught form was the exact form
//     the guard existed to find, in two separate guards.
//
//   - A WRAPPER'S VERDICT IS NOT THE BACKEND'S VERDICT. Faulty short-circuits
//     ABOVE the backend, so the tri-state cases exercise the wrapper and never
//     reach a backend's own handling of an unreachable store — full coverage
//     in the contract suite, near-zero coverage of the code implementing it,
//     and the number looks perfect. Measured: FOUR of eight verbs in the twin
//     could lose their context guard with nothing failing — Renew, Release,
//     PreferredResources and FleetProtocolFloor.
//
//     That number was published as five until it was landing-checked, which
//     is the entry above earning its place on this list. ListOwned was in the
//     first count and does not belong there: it was covered, but only because
//     it shares one guard with ListLive, which the test did send. Coverage by
//     shared implementation rather than by test is its own trap — it holds
//     exactly until someone gives the verb its own guard, and nothing fails
//     at the moment the coverage evaporates.
//
//   - ENUMERATE THE LIFECYCLE POINTS AT WHICH EACH VERB IS SENT, which is a
//     different axis from what it is sent. This suite sent nine awkward
//     resource names and never sent a Release at the one moment that mattered
//     — a lease that had lapsed and was never re-claimed — where the two
//     backends had answered differently for as long as both existed. Build
//     the whole verb-by-state matrix; the verb you would suspect is chosen
//     from inside the blind spot.
//
//   - ENUMERATE WHAT THE SUITE SENDS, not only what it asserts. No mutation
//     can reveal an input that never arrives — both gaps found that way (no
//     resource name ever needed encoding, no TTL ever exceeded the maximum).
//
//   - COMPARING TIMES, ASSERT AN INTERVAL, NEVER AN ORDERING. "Later than"
//     admits a second reason: a store clamping both deadlines still stamps the
//     second one later, purely because it happened second. Measured at 487ns
//     of "later" satisfying an assertion meant to detect five minutes.
//
//   - A CHECK-THEN-ACT GUARD DIAGNOSES A RACE; IT CANNOT CLOSE ONE. stillLive
//     made two failures legible and both still happened, one round trip after
//     it passed. Legible is worth a great deal and is a different thing.
//
// None of these require anyone to be suspicious at the right moment, which is
// the only property that survives contact with a blind spot.
//
// # Before adding a case that says a backend must NOT do something
//
// Grep rewrite/decisions/ for the operation first. A recorded degradation is a
// PERMITTED exception, and the corpus is where permission lives — d-201 §3
// records that a KV cannot express the protocol gate inside a compare-and-swap
// and so does check → claim → re-check → release, which means a gate-refused
// claim may legitimately leave a touched record. A "must not" written without
// that grep forbids what the contract allows, and the way that surfaces is a
// correct backend failing, its author investigating, and this suite changing.
// It has already cost that twice in the sibling queue suite. The grep costs
// about fifteen seconds.
//
// And a gap in a suite comes in two kinds, which need different hunting:
//
//   - An OBSERVATION gap applies the stimulus and cannot see the result. A
//     mutation reveals it, so it belongs in a control set — the meta codec is
//     one, and it sits in the mutation set with CLEAN as its expected verdict
//     so the gap cannot silently close or widen.
//   - A STIMULUS gap never sends the input at all. No mutation can reveal it,
//     because nothing a backend does changes an answer the suite never asks
//     for; the only way to find one is to read what the cases SEND. Both of
//     this suite's were found that way — no resource name ever needed
//     encoding, and no TTL ever exceeded the advertised maximum.
//
// A stimulus gap is closeable when the effect reaches a value the API REPORTS:
// a silently clamped TTL is invisible in behaviour for five minutes but shows
// up immediately in the ExpiresAt of two claims. It stays open when the only
// observable is elapsed time, because then telling the two apart costs the
// wait. Assert the difference between two reported values, never the ordering
// of two timestamps — see expires_at_is_a_utc_deadline_on_the_stores_clock for
// why ordering admits a second reason.
//
// The largest thing this suite assumes without saying so is READ-YOUR-OWN-
// WRITE: every case reads back a claim it just made, and coord.go never
// promises that. Both certified backends make it true for free — one is a
// mutex over a map, the other a single-connection KV — so no case here can
// tell a store that guarantees it from one that happens to. A backend with
// asynchronous replication would fail case after case for a property nobody
// wrote down. It is enforced rather than relaxed because a claim you cannot
// read back cannot provide mutual exclusion, and the seat host reads ListLive
// and FleetProtocolFloor immediately after claiming — but see
// rewrite/questions/coord-contract-read-your-own-write.md, because that is the
// contract owner's call and not the suite's.
//
// The same applies to what a case QUIETLY assumes. Every meta payload here is
// JSON-shaped, which is why none of them could see that a value's Go type does
// not survive a real backend's round trip; that gap is stated at the meta
// cases rather than left for someone to discover in production.
//
// # What the suite does NOT require
//
// A backend may answer "unknown" at any moment. That is the contract's third
// answer, not a defect the suite gets to disallow, and a compare-and-swap
// store reaches it honestly under contention: it loses every swap inside its
// retry budget and genuinely cannot say whether a peer won or a record lapsed
// underneath it. So the contended cases come back for a definite answer the
// way a caller's next sweep does, instead of demanding one first time. An
// earlier version failed a correct KV backend for exactly this — which is
// what writing the memory twin's implementation strategy into a contract
// looks like from the outside.
//
// # Lapsing without sleeping
//
// A lease lapses when its TTL runs out, and a suite that waited out real TTLs
// would be both slow and flaky. So lapse is expressed as a SHORT TTL plus
// harness.lapse, which asks the backend to move its own clock (see Advancer)
// and falls back to sleeping for a backend whose clock it cannot reach. The
// lease a case intends to lapse gets ShortTTL; everything that must survive
// the lapse gets LongTTL. Nothing here pokes at a backend's records, so the
// same case certifies a store whose expiry is a per-key TTL and one where it
// is a column comparison.
package coordtest

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

const (
	// LongTTL is the TTL for any lease that must stay held for the whole
	// of a case. It is far longer than any lapse the suite simulates, so
	// travelling past a ShortTTL lease never disturbs it.
	//
	// It is also the MAXIMUM TTL the suite will ever ask a backend for, and
	// that is a promise, not an observation. A store sizes its retention to
	// the longest TTL it must honour — the embedded-NATS backend sets its
	// bucket from this very constant — and a backend that cannot honour a
	// TTL is right to refuse it rather than silently shorten it, since a
	// quietly-clamped deadline makes every heartbeat's next-tick arithmetic
	// wrong. A case that needs a second, distinguishable TTL takes a
	// FRACTION of this one; asking for a multiple makes a correct backend
	// fail an unrelated assertion with a confusing error.
	//
	// The rule is that no case may DEPEND on a TTL above this, not that
	// none may send one: exactly one case deliberately asks for more, to
	// certify that a store refuses rather than clamps, and it accepts a
	// refusal as the correct answer.
	LongTTL = 5 * time.Minute

	// ShortTTL is the TTL for a lease a case intends to lapse, and it is
	// only ever claimed as the LAST act before lapsing it.
	//
	// It is therefore NOT required to cover any store work — a case that
	// must do something while the lease is still live uses WindowTTL
	// instead. That distinction is the whole correction: this comment used
	// to claim 100ms was "comfortably more than a slow round trip", which
	// is simply false and was falsified by a real backend. Under -count=2
	// with the suite parallel under -race, a handful of round trips
	// against an embedded broker cost 200-330ms, and two cases failed
	// intermittently for months' worth of runs before anyone caught the
	// dump instead of truncating it.
	//
	// So the value now answers a much easier question: how long a lease
	// should live when the only thing that must happen before it expires
	// is nothing at all. Overrunning a zero-operation window is harmless —
	// the lease lapses, which is what the case wanted. 100ms keeps a
	// fully serialised sleeping run (-parallel 1) to a few seconds.
	ShortTTL = 100 * time.Millisecond

	// WindowTTL is ShortTTL's sibling for the few cases that must complete a
	// store operation WHILE the short lease is still live — a renew that
	// proves it extends, a re-acquire that proves it keeps its epoch. Those
	// windows are irreducible: the operation happening while live IS the
	// invariant, so no reordering closes them.
	//
	// Sized from a measurement, not a guess. Those cases run in 0.17-0.20s
	// against an embedded broker at rest; under -count=2 with the whole
	// suite in parallel under -race they were observed at 0.36-0.48s, so a
	// handful of round trips can cost 200-330ms. 2s leaves roughly six
	// times that. ShortTTL stayed at 100ms because every other lapsing case
	// claims its short lease as the LAST thing it does — a window of zero
	// operations cannot be overrun, and lapsing early is harmless there.
	WindowTTL = 2 * time.Second

	// lapseMargin is how far past ShortTTL harness.lapse travels. On the
	// sleeping path the store's clock is the real one, and a sleep is only
	// ever a lower bound in one direction — this is the other one.
	lapseMargin = 50 * time.Millisecond

	// stallBudget bounds how long the suite will wait on a backend before
	// calling it stuck.
	//
	// A contract call that never returns has to fail as a NAMED CASE, not
	// as a package timeout. The Go test binary's ten-minute panic dumps
	// every goroutine in the process and is attributed to whichever
	// package the deadline happens to land in — which is exactly how a
	// hang in one queue backend was first reported against a different
	// one, costing the wrong author the investigation. The heaviest case
	// here issues a few thousand store round trips, so the budget has to
	// clear that on a contended real store (90 s allows ~30 ms apiece).
	// Measured rather than only derived, because arithmetic in a comment
	// reads exactly like a measurement: the churn case runs in 8.8 s
	// against the embedded broker under -race — roughly 2,700 operations
	// at ~3 ms each — so the budget carries about ten times the observed
	// worst case
	// while still reporting well inside the default package timeout.
	stallBudget = 90 * time.Second
)

// Advancer is the optional hook a backend offers so the suite can outlast a
// TTL without sleeping through it.
//
// Optional, and deliberately so: a real coordination store's clock is its own
// and cannot be moved. Implementing it costs a backend one offset added to
// its now(); not implementing it costs the suite a sleep per lapsing case.
type Advancer interface {
	// Advance moves the STORE's clock forward.
	//
	// Every expiry decision a backend makes must be taken against that
	// clock and never against the caller's — a backend that compared the
	// caller's wall clock would pass this suite unchanged and then hand
	// two nodes the same seat the first time an NTP step separated them.
	Advance(d time.Duration)
}

// Run executes the contract suite against backends built by newBackend.
//
// newBackend is called once per case and must hand back an EMPTY store. The
// protocol gate is fleet-wide, so a leftover lease from one case silently
// refuses the next case's claims — a failure that reads as a broken gate
// rather than as a dirty fixture. A backend sharing one store across calls
// has to clear it; the suite checks and says so rather than letting it
// present as an invariant violation.
func Run(t *testing.T, newBackend func(t *testing.T) coord.Backend) {
	t.Helper()
	groups := []struct {
		name  string
		cases []testCase
	}{
		{"lease", leaseCases},
		{"read", readCases},
		{"protocol", protocolCases},
		{"tristate", tristateCases},
		{"concurrency", concurrencyCases},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					// Every case owns its own store, so they are
					// independent by construction — and on a backend
					// whose clock the suite cannot move, running them
					// in parallel overlaps the sleeps that lapsing
					// costs instead of summing them.
					t.Parallel()
					c.fn(newHarness(t, newBackend))
				})
			}
		})
	}
}

// testCase is one named invariant.
type testCase struct {
	name string
	fn   func(h *harness)
}

// harness is a backend plus the assertions the cases are written in. Every
// helper that cannot fail in a correct backend fails the test itself, so a
// case body reads as the invariant rather than as error plumbing.
type harness struct {
	t   *testing.T
	ctx context.Context
	b   coord.Backend

	// The most recent claim, so stillLive can tell the suite overrunning
	// its own window apart from a backend dropping a live lease. Written
	// and read only on the test goroutine — the concurrency cases call the
	// backend directly rather than through claim(), and -race would say so
	// if that ever changed.
	lastClaimAt  time.Time
	lastClaimTTL time.Duration
	// travelled records that the suite moved the STORE's clock since that
	// claim. Wall time is then no guide at all — see stillLive.
	travelled bool
}

func newHarness(t *testing.T, newBackend func(t *testing.T) coord.Backend) *harness {
	t.Helper()
	b := newBackend(t)
	if b == nil {
		t.Fatal("newBackend returned a nil backend")
	}
	// Every call the case makes carries the stall budget as a deadline, so
	// a backend that honours cancellation reports a hang as an error on
	// the call that hung instead of as a process-wide timeout. Not the
	// test's own context: this one has to stay live for the goroutines the
	// concurrency cases join at the end, and it is cancelled after them.
	ctx, cancel := context.WithTimeout(context.Background(), stallBudget)
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, b: b}
	if live := h.listLive(""); len(live) != 0 {
		t.Fatalf("newBackend must return an empty store, got %d live lease(s): %v",
			len(live), resources(live))
	}
	return h
}

// claim acquires and fails the case unless the backend granted the lease.
func (h *harness) claim(resource string, opts coord.AcquireOptions) *coord.Lease {
	h.t.Helper()
	h.lastClaimAt, h.lastClaimTTL, h.travelled = time.Now(), opts.TTL, false
	lease, err := h.b.TryAcquire(h.ctx, resource, opts)
	if err != nil {
		h.t.Fatalf("TryAcquire(%q, owner=%q): unexpected error: %v", resource, opts.Owner, err)
	}
	if lease == nil {
		h.t.Fatalf("TryAcquire(%q, owner=%q): refused, expected the claim to be granted",
			resource, opts.Owner)
	}
	if lease.Resource != resource || lease.Owner != opts.Owner {
		h.t.Fatalf("TryAcquire(%q, owner=%q) returned a lease for (%q, %q)",
			resource, opts.Owner, lease.Resource, lease.Owner)
	}
	if lease.Epoch < 1 {
		h.t.Fatalf("TryAcquire(%q): epoch %d — a fencing token starts at 1, and 0 is what an "+
			"unset column reads as", resource, lease.Epoch)
	}
	return lease
}

// refused asserts the DEFINITE refusal — (nil, nil). An error here means the
// backend collapsed "unknown" into "somebody else holds it", which is the
// conflation the whole tri-state exists to prevent.
func (h *harness) refused(resource string, opts coord.AcquireOptions) {
	h.t.Helper()
	lease, err := h.b.TryAcquire(h.ctx, resource, opts)
	if err != nil {
		h.t.Fatalf("TryAcquire(%q, owner=%q): a genuine refusal must be (nil, nil), got error: %v",
			resource, opts.Owner, err)
	}
	if lease != nil {
		h.t.Fatalf("TryAcquire(%q, owner=%q): granted at epoch %d, expected a refusal",
			resource, opts.Owner, lease.Epoch)
	}
}

func (h *harness) renew(resource, owner string, epoch int64, ttl time.Duration) bool {
	h.t.Helper()
	ok, err := h.b.Renew(h.ctx, resource, owner, epoch, ttl)
	if err != nil {
		h.t.Fatalf("Renew(%q, owner=%q, epoch=%d): unexpected error: %v", resource, owner, epoch, err)
	}
	return ok
}

func (h *harness) release(resource, owner string, epoch int64) bool {
	h.t.Helper()
	ok, err := h.b.Release(h.ctx, resource, owner, epoch)
	if err != nil {
		h.t.Fatalf("Release(%q, owner=%q, epoch=%d): unexpected error: %v", resource, owner, epoch, err)
	}
	return ok
}

func (h *harness) get(resource string) *coord.Lease {
	h.t.Helper()
	lease, err := h.b.Get(h.ctx, resource)
	if err != nil {
		h.t.Fatalf("Get(%q): unexpected error: %v", resource, err)
	}
	return lease
}

func (h *harness) listOwned(owner string) []coord.Lease {
	h.t.Helper()
	leases, err := h.b.ListOwned(h.ctx, owner)
	if err != nil {
		h.t.Fatalf("ListOwned(%q): unexpected error: %v", owner, err)
	}
	return leases
}

func (h *harness) listLive(prefix string) []coord.Lease {
	h.t.Helper()
	leases, err := h.b.ListLive(h.ctx, prefix)
	if err != nil {
		h.t.Fatalf("ListLive(%q): unexpected error: %v", prefix, err)
	}
	return leases
}

func (h *harness) preferred(prefix, nodeID string) map[string]struct{} {
	h.t.Helper()
	got, err := h.b.PreferredResources(h.ctx, prefix, nodeID)
	if err != nil {
		h.t.Fatalf("PreferredResources(%q, %q): unexpected error: %v", prefix, nodeID, err)
	}
	return got
}

func (h *harness) floor() (int, bool) {
	h.t.Helper()
	got, any, err := h.b.FleetProtocolFloor(h.ctx)
	if err != nil {
		h.t.Fatalf("FleetProtocolFloor: unexpected error: %v", err)
	}
	return got, any
}

// await joins the suite's own goroutines within the stall budget. what names
// what they were doing, so a stuck backend fails on a line that says which
// call never came back rather than on a goroutine dump.
func (h *harness) await(wg *sync.WaitGroup, what string) {
	h.t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stallBudget):
		// Evidence, not a verdict — the same correction stillLive needed.
		// This helper cannot see the difference between a call that never
		// returns and one merely slower than a budget THIS SUITE chose,
		// and "never returns" is a serious accusation to hand an author
		// on no more evidence than a timer firing.
		h.t.Fatalf("%s: still running after %v, which is stallBudget — a constant this "+
			"suite picked, not a promise the contract makes. Most likely a coord.Backend "+
			"call is not returning; but a store merely slower than that budget looks "+
			"identical from here, and if this backend is known to be slow then both "+
			"readings are live and this helper cannot separate them", what, stallBudget)
	}
}

// stillLive asserts a ShortTTL lease has not run out yet.
//
// Call it in any case that does store work between claiming a short lease and
// depending on it. On a backend with no Advancer that window is real time, and
// a slow store — or a loaded machine, or -race, or a suite that has grown
// since the constant was chosen — can spend the whole TTL inside it. What
// happens then is the expensive part: the case fails LATER, on an invariant
// the backend never broke, and reads as a takeover bug or a renew that
// wrongly reported loss. This turns that into a failure that names its own
// cause, so the next reader spends the time on the suite's timing assumption
// rather than on the backend.
func (h *harness) stillLive(resource string) {
	h.t.Helper()
	if lease := h.get(resource); lease != nil {
		return
	}
	// Two causes, and the first version of this guard asserted the one it
	// happened to be built for — which is the same misattribution it exists
	// to prevent, pointing the other way. A backend that DROPS a lease it
	// just granted trips this too, and telling its author "this is the
	// suite's timing assumption, not a backend defect" would send them
	// looking anywhere but at the bug. So it accuses whichever the elapsed
	// time actually implicates.
	// On a backend whose clock the suite can move, wall time stops being
	// evidence the moment it moves one: the lease is gone in STORE time
	// while no wall time has passed, which reads exactly like a backend
	// dropping a live lease. Accusing it would be a false find handed to
	// its author, so the guard says what it actually knows instead.
	if h.travelled {
		h.t.Fatalf("%s is unheld because THIS CASE advanced the store's clock past its TTL "+
			"since claiming it. Neither the backend nor the suite's timing window is "+
			"implicated — the case is asking whether a lease is live after deliberately "+
			"outlasting it", resource)
	}
	elapsed := time.Since(h.lastClaimAt)
	if elapsed >= h.lastClaimTTL {
		// Evidence, not a verdict. The first version of this branch said
		// flatly "this is not a backend defect", which is the same
		// over-claim in the other direction: a store that spent seconds
		// on three operations IS a finding, and that message told the one
		// reader whose problem was the backend to look elsewhere. So it
		// reports the number and names what each reading implies.
		h.t.Fatalf("%s lapsed before this case was ready for it: %v elapsed against a %v TTL. "+
			"The suite spent its own window, so the case should claim its short lease "+
			"later — but read the raw %v too: that is how long this store took for the "+
			"setup, and if it is slower than you expect then BOTH are live and this "+
			"helper cannot separate them. What it is not is a reason to raise the "+
			"constant here, which would blunt the check for every other backend",
			resource, elapsed, h.lastClaimTTL, elapsed)
	}
	h.t.Fatalf("%s is already unheld %v into a %v TTL — the store dropped a lease it had "+
		"just granted. This is a BACKEND defect: too little time has passed for the "+
		"suite's own timing to explain it", resource, elapsed, h.lastClaimTTL)
}

// lapse outlasts a ShortTTL lease. See the package doc: the suite never edits
// a backend's records, it lets a TTL run out.
func (h *harness) lapse() { h.lapsePast(ShortTTL) }

// lapsePast outlasts a lease of the given TTL. Cases holding a WindowTTL lease
// must travel past THAT, not past ShortTTL.
func (h *harness) lapsePast(ttl time.Duration) {
	h.t.Helper()
	d := ttl + lapseMargin
	if a, ok := h.b.(Advancer); ok {
		a.Advance(d)
		h.travelled = true
		return
	}
	time.Sleep(d)
}

// --- assertion helpers ----------------------------------------------------

func (h *harness) mustHold(resource, owner string) *coord.Lease {
	h.t.Helper()
	lease := h.get(resource)
	if lease == nil {
		h.t.Fatalf("Get(%q): no live lease, expected %q to hold it", resource, owner)
	}
	if lease.Owner != owner {
		h.t.Fatalf("Get(%q): held by %q, expected %q", resource, lease.Owner, owner)
	}
	return lease
}

func (h *harness) mustBeUnheld(resource string) {
	h.t.Helper()
	if lease := h.get(resource); lease != nil {
		h.t.Fatalf("Get(%q): held by %q at epoch %d, expected nothing to hold it",
			resource, lease.Owner, lease.Epoch)
	}
}

// requireUnchanged asserts a record survived an operation the backend refused
// or rejected exactly as it was.
//
// A "no" has two halves and the suite used to check only one. Returning the
// right answer while writing to the record anyway is the shape a
// read-modify-write backend reaches by validating after the write instead of
// before it, and every field here is one a refusal must not touch.
//
// Resource is compared too, which the first version of this helper omitted
// while its comment claimed to cover "every field a refusal must not touch".
// The answer was right — it caught every mutation put to it — and the stated
// reason was not, which is the pairing that survives any review that checks
// answers. Found by probing the claim rather than re-reading it, and the first
// probe was itself vacuous: it extracted zero field names and reported no
// omission, a measurement of nothing wearing the shape of a clean result.
//
// This generalises over the record only as far as it is kept in step with it:
// a field added to coord.Lease and not added here is a field every negative
// path may quietly write, and nothing fails to say so. Extend it with the
// struct.
//
// Measured, not assumed: dropping the Preferred comparison here while a
// backend writes the hint on a refused claim passes the whole suite, both
// mutations landing-checked. The limitation is real and this is what it costs.
func (h *harness) requireUnchanged(what string, before, after *coord.Lease) {
	h.t.Helper()
	if after == nil {
		h.t.Fatalf("%s: the record is gone", what)
		return
	}
	switch {
	case after.Resource != before.Resource:
		h.t.Fatalf("%s: the record now names resource %q, not %q",
			what, after.Resource, before.Resource)
	case after.Owner != before.Owner:
		h.t.Fatalf("%s: owner moved %q -> %q", what, before.Owner, after.Owner)
	case after.Epoch != before.Epoch:
		h.t.Fatalf("%s: epoch moved %d -> %d", what, before.Epoch, after.Epoch)
	case !after.ExpiresAt.Equal(before.ExpiresAt):
		h.t.Fatalf("%s: deadline moved %v -> %v — a refusal must not write the record "+
			"at all, and the direction it moves is whichever way the refused caller's "+
			"TTL happened to point",
			what, before.ExpiresAt, after.ExpiresAt)
	case after.Preferred != before.Preferred:
		h.t.Fatalf("%s: placement hint moved %q -> %q", what, before.Preferred, after.Preferred)
	case after.Protocol != before.Protocol:
		h.t.Fatalf("%s: protocol moved %d -> %d", what, before.Protocol, after.Protocol)
	case !reflect.DeepEqual(after.Meta, before.Meta):
		h.t.Fatalf("%s: meta moved %v -> %v", what, before.Meta, after.Meta)
	}
}

// requireSameValues asserts a meta payload came back with the same VALUES it
// went in with, saying nothing about the Go types carrying them.
//
// Canonical JSON is the comparison because it is exactly the distinction the
// contract draws and no more: under it int(3) and float64(3) are one value,
// []string{"a"} and []any{"a"} are one list, and a DROPPED or corrupted entry
// is still a difference. A backend is free to choose its wire format — so
// asserting types here would forbid what the contract permits — but no
// backend is free to lose what a holder advertised about itself, because a
// peer reads it to decide whether this node may run a seat.
func (h *harness) requireSameValues(what string, want, got map[string]any) {
	h.t.Helper()
	wantJSON, err := canonicalJSON(want)
	if err != nil {
		h.t.Fatalf("%s: the fixture does not encode: %v", what, err)
	}
	gotJSON, err := canonicalJSON(got)
	if err != nil {
		h.t.Fatalf("%s: the returned payload does not encode: %v", what, err)
	}
	if wantJSON != gotJSON {
		h.t.Fatalf("%s: meta came back as %s, want the same values as %s", what, gotJSON, wantJSON)
	}
}

// canonicalJSON renders a payload for value comparison. encoding/json sorts
// map keys, so two payloads differing only in iteration order agree.
func canonicalJSON(v map[string]any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// requireResources compares a listing against an expected set. Order is
// deliberately NOT part of the contract: coord.Backend does not promise one,
// and a suite that demanded it would fail a correct store for a detail no
// caller reads.
func (h *harness) requireResources(what string, got []coord.Lease, want ...string) {
	h.t.Helper()
	gotNames := resources(got)
	slices.Sort(gotNames)
	slices.Sort(want)
	if !slices.Equal(gotNames, want) {
		h.t.Fatalf("%s = %v, want %v", what, gotNames, want)
	}
}

func (h *harness) requireSet(what string, got map[string]struct{}, want ...string) {
	h.t.Helper()
	gotNames := make([]string, 0, len(got))
	for r := range got {
		gotNames = append(gotNames, r)
	}
	slices.Sort(gotNames)
	slices.Sort(want)
	if !slices.Equal(gotNames, want) {
		h.t.Fatalf("%s = %v, want %v", what, gotNames, want)
	}
}

func resources(leases []coord.Lease) []string {
	out := make([]string, 0, len(leases))
	for _, l := range leases {
		out = append(out, l.Resource)
	}
	return out
}
