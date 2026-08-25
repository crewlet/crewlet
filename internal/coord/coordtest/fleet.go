package coordtest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// RunFleet is the contract suite every [coord.Fleet] must pass.
//
// A SECOND entry point rather than more cases in [Run], because the two
// surfaces are separately implementable: a deployment can have a lease
// backend without the shared state (the memory twin serves a single node),
// and a case that could not be run without both would make the twin's own
// certification depend on the KV.
//
// The names are the invariants, and each one is a thing that was WRONG when
// this state lived on the node's own database: a valve that counted per node,
// a claim a peer could not see, a completion ledger a redelivery to another
// node walked straight past.
func RunFleet(t *testing.T, newFleet func(t *testing.T) coord.Fleet) {
	t.Helper()
	groups := []struct {
		name  string
		cases []fleetCase
	}{
		{"valve", valveCases},
		{"claims", claimCases},
		{"ledger", ledgerCases},
		{"cooldowns", cooldownCases},
		{"budgets", budgetCases},
		{"plane", planeCases},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					f := newFleet(t)
					if f == nil {
						t.Fatal("newFleet returned a nil backend")
					}
					ctx, cancel := context.WithTimeout(context.Background(), stallBudget)
					t.Cleanup(cancel)
					c.fn(&fleetHarness{t: t, ctx: ctx, f: f})
				})
			}
		})
	}
}

type fleetCase struct {
	name string
	fn   func(h *fleetHarness)
}

// fleetHarness is a backend plus the assertions the cases are written in.
type fleetHarness struct {
	t   *testing.T
	ctx context.Context
	f   coord.Fleet
}

// now is the suite's base instant.
//
// FIXED and in the past, not time.Now(): every case that reasons about a
// window boundary or a lapsed deadline has to be able to name both sides of
// it, and a case anchored to the wall clock passes or fails depending on
// where in a second it happened to start.
func (h *fleetHarness) now() time.Time {
	return time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
}

func (h *fleetHarness) allow(bucket string, limit int, window time.Duration, at time.Time) bool {
	h.t.Helper()
	ok, err := h.f.Allow(h.ctx, bucket, limit, window, at)
	if err != nil {
		h.t.Fatalf("Allow(%s): %v", bucket, err)
	}
	return ok
}

func (h *fleetHarness) claim(key string, ttl time.Duration, at time.Time) bool {
	h.t.Helper()
	ok, err := h.f.Claim(h.ctx, key, ttl, at)
	if err != nil {
		h.t.Fatalf("Claim(%s): %v", key, err)
	}
	return ok
}

func (h *fleetHarness) worked(scope string, keys ...string) map[string]bool {
	h.t.Helper()
	got, err := h.f.Worked(h.ctx, scope, keys)
	if err != nil {
		h.t.Fatalf("Worked(%s): %v", scope, err)
	}
	return got
}

func (h *fleetHarness) record(scope, key, detail string, at time.Time) {
	h.t.Helper()
	if err := h.f.Record(h.ctx, scope, key, detail, at); err != nil {
		h.t.Fatalf("Record(%s/%s): %v", scope, key, err)
	}
}

func (h *fleetHarness) charge(seat string, tokens, orgLimit, seatLimit int) coord.Spend {
	h.t.Helper()
	got, err := h.f.Charge(h.ctx, seat, tokens, orgLimit, seatLimit)
	if err != nil {
		h.t.Fatalf("Charge(%s, %d): %v", seat, tokens, err)
	}
	return got
}

func (h *fleetHarness) used(scope string) int {
	h.t.Helper()
	got, err := h.f.Used(h.ctx, scope)
	if err != nil {
		h.t.Fatalf("Used(%s): %v", scope, err)
	}
	return got
}

// ---- the valve ---------------------------------------------------------- //

var valveCases = []fleetCase{{
	// The reason this moved off the node's own database. Four nodes on one
	// company each ran their own counter, so a seat configured for five
	// notifications a second could emit twenty.
	name: "one bucket is one count however many callers share it",
	fn: func(h *fleetHarness) {
		at := h.now()
		for i := range 3 {
			if !h.allow("seat:ceo", 3, time.Second, at) {
				h.t.Fatalf("call %d was refused inside the limit", i+1)
			}
		}
		if h.allow("seat:ceo", 3, time.Second, at) {
			h.t.Fatal("a fourth call passed a limit of three")
		}
	},
}, {
	name: "a fresh window starts at zero",
	fn: func(h *fleetHarness) {
		at := h.now()
		for range 2 {
			h.allow("seat:ceo", 2, time.Second, at)
		}
		if h.allow("seat:ceo", 2, time.Second, at) {
			h.t.Fatal("the window did not close")
		}
		if !h.allow("seat:ceo", 2, time.Second, at.Add(time.Second)) {
			h.t.Fatal("the next window inherited the previous one's count")
		}
	},
}, {
	// The window is FIXED, so two instants inside one window share a count
	// whether or not they are the same instant. A sliding window would let
	// a caller pace itself past the limit.
	name: "two instants inside one window share the count",
	fn: func(h *fleetHarness) {
		at := h.now().Truncate(time.Second)
		if !h.allow("seat:ceo", 1, time.Second, at) {
			h.t.Fatal("the first call was refused")
		}
		if h.allow("seat:ceo", 1, time.Second, at.Add(900*time.Millisecond)) {
			h.t.Fatal("a later instant in the same window got its own count")
		}
	},
}, {
	name: "buckets do not share a count",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.allow("seat:ceo", 1, time.Second, at)
		if !h.allow("seat:cto", 1, time.Second, at) {
			h.t.Fatal("one seat's notifications closed another seat's valve")
		}
	},
}, {
	// Concurrency is the whole point: the counter has to be atomic, or the
	// fleet's combined rate is whatever the last writer happened to see.
	name: "concurrent callers cannot exceed the limit",
	fn: func(h *fleetHarness) {
		const limit = 5
		at := h.now()
		var wg sync.WaitGroup
		passed := make(chan bool, 32)
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := h.f.Allow(h.ctx, "seat:hot", limit, time.Second, at)
				// An error under contention is the honest third
				// answer, and it is NOT a pass.
				passed <- err == nil && ok
			}()
		}
		wg.Wait()
		close(passed)
		allowed := 0
		for ok := range passed {
			if ok {
				allowed++
			}
		}
		if allowed > limit {
			h.t.Fatalf("%d of 32 concurrent callers passed a limit of %d", allowed, limit)
		}
		if allowed == 0 {
			h.t.Fatal("no caller passed at all, so the valve is shut rather than limiting")
		}
	},
}}

// ---- the delivery claims ------------------------------------------------ //

var claimCases = []fleetCase{{
	// A vendor retrying a delivery to a DIFFERENT ingress node found no
	// claim on the node-local store, and the same push woke the seat twice.
	name: "only the first caller claims a delivery",
	fn: func(h *fleetHarness) {
		at := h.now()
		if !h.claim("gitlab|abc123", time.Minute, at) {
			h.t.Fatal("the first claim was refused")
		}
		if h.claim("gitlab|abc123", time.Minute, at) {
			h.t.Fatal("a second caller claimed the same delivery")
		}
	},
}, {
	name: "different deliveries do not collide",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.claim("gitlab|abc123", time.Minute, at)
		if !h.claim("gitlab|def456", time.Minute, at) {
			h.t.Fatal("one delivery's claim suppressed another")
		}
	},
}, {
	// A released claim must be RE-CLAIMABLE. A tombstone that still
	// refused would swallow an operator's deliberate replay for the whole
	// retention window, which is the failure mode a purge exists to avoid.
	name: "a released claim can be made again",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.claim("plane|xyz", time.Minute, at)
		if err := h.f.Release(h.ctx, "plane|xyz"); err != nil {
			h.t.Fatalf("Release: %v", err)
		}
		if !h.claim("plane|xyz", time.Minute, at) {
			h.t.Fatal("a released delivery could not be claimed again")
		}
	},
}, {
	name: "releasing a claim nobody made is not an error",
	fn: func(h *fleetHarness) {
		if err := h.f.Release(h.ctx, "never|claimed"); err != nil {
			h.t.Fatalf("Release on an unclaimed key: %v", err)
		}
	},
}, {
	name: "exactly one of many concurrent callers claims",
	fn: func(h *fleetHarness) {
		at := h.now()
		var wg sync.WaitGroup
		won := make(chan bool, 16)
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := h.f.Claim(h.ctx, "mattermost|race", time.Minute, at)
				won <- err == nil && ok
			}()
		}
		wg.Wait()
		close(won)
		winners := 0
		for ok := range won {
			if ok {
				winners++
			}
		}
		if winners != 1 {
			h.t.Fatalf("%d callers claimed one delivery, want exactly 1", winners)
		}
	},
}}

// ---- the completion ledger ---------------------------------------------- //

var ledgerCases = []fleetCase{{
	// A redelivery that landed on a peer had no ledger to consult, so the
	// turn ran again — which is what the ledger exists to stop.
	name: "a recorded key is worked for every reader",
	fn: func(h *fleetHarness) {
		h.record("ceo", "wk-1", "turn-a", h.now())
		got := h.worked("ceo", "wk-1", "wk-2")
		if !got["wk-1"] {
			h.t.Fatal("a recorded key came back unworked")
		}
		if got["wk-2"] {
			h.t.Fatal("an unrecorded key came back worked")
		}
	},
}, {
	name: "scopes do not see each other's work",
	fn: func(h *fleetHarness) {
		h.record("ceo", "wk-1", "turn-a", h.now())
		if h.worked("cto", "wk-1")["wk-1"] {
			h.t.Fatal("one seat's completion suppressed another seat's turn")
		}
	},
}, {
	// FIRST WRITER WINS, and losing is not an error: two nodes completing
	// one trigger is the case being collapsed, not a failure to report.
	name: "recording a key twice is not an error",
	fn: func(h *fleetHarness) {
		h.record("ceo", "wk-1", "turn-a", h.now())
		h.record("ceo", "wk-1", "turn-b", h.now().Add(time.Second))
		if !h.worked("ceo", "wk-1")["wk-1"] {
			h.t.Fatal("the key stopped being worked after a second record")
		}
	},
}, {
	name: "asking about no keys is not an error",
	fn: func(h *fleetHarness) {
		if got := h.worked("ceo"); len(got) != 0 {
			h.t.Fatalf("an empty query answered %v", got)
		}
	},
}, {
	name: "concurrent records of one key all succeed",
	fn: func(h *fleetHarness) {
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- h.f.Record(h.ctx, "ceo", "wk-race",
					fmt.Sprintf("turn-%d", i), h.now())
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				h.t.Fatalf("a concurrent Record failed: %v", err)
			}
		}
		if !h.worked("ceo", "wk-race")["wk-race"] {
			h.t.Fatal("the key is not worked after 16 concurrent records")
		}
	},
}}

// ---- the credential cooldowns ------------------------------------------- //

var cooldownCases = []fleetCase{{
	name: "a cooled credential is visible to every node",
	fn: func(h *fleetHarness) {
		until := h.now().Add(10 * time.Minute)
		if err := h.f.Cool(h.ctx, "anthropic:sk-…abcd", until); err != nil {
			h.t.Fatalf("Cool: %v", err)
		}
		got, err := h.f.Since(h.ctx, h.now())
		if err != nil {
			h.t.Fatalf("Since: %v", err)
		}
		if !got["anthropic:sk-…abcd"].Equal(until) {
			h.t.Fatalf("cooldowns = %v, want the instant it was cooled until", got)
		}
	},
}, {
	name: "a lapsed cooldown is not reported",
	fn: func(h *fleetHarness) {
		if err := h.f.Cool(h.ctx, "openai:sk-…wxyz", h.now().Add(time.Minute)); err != nil {
			h.t.Fatalf("Cool: %v", err)
		}
		got, err := h.f.Since(h.ctx, h.now().Add(2*time.Minute))
		if err != nil {
			h.t.Fatalf("Since: %v", err)
		}
		if _, still := got["openai:sk-…wxyz"]; still {
			h.t.Fatal("a cooldown that had lapsed was still reported")
		}
	},
}, {
	// The LONGER cooldown survives. Both nodes saw a real refusal, and
	// shortening a peer's would send this node straight back at a key the
	// peer already knows is spent.
	name: "the longer of two cooldowns wins",
	fn: func(h *fleetHarness) {
		short := h.now().Add(time.Minute)
		long := h.now().Add(30 * time.Minute)
		if err := h.f.Cool(h.ctx, "key", long); err != nil {
			h.t.Fatalf("Cool: %v", err)
		}
		if err := h.f.Cool(h.ctx, "key", short); err != nil {
			h.t.Fatalf("Cool: %v", err)
		}
		got, err := h.f.Since(h.ctx, h.now())
		if err != nil {
			h.t.Fatalf("Since: %v", err)
		}
		if !got["key"].Equal(long) {
			h.t.Fatalf("cooldown = %v, want the longer %v", got["key"], long)
		}
	},
}, {
	name: "nothing cooled reports nothing",
	fn: func(h *fleetHarness) {
		got, err := h.f.Since(h.ctx, h.now())
		if err != nil {
			h.t.Fatalf("Since: %v", err)
		}
		if len(got) != 0 {
			h.t.Fatalf("an empty ledger reported %v", got)
		}
	},
}}

// ---- the config plane --------------------------------------------------- //

var planeCases = []fleetCase{{
	// "Nothing activated" and "the store cannot be read" send a node down
	// opposite paths, so the read reports the two separately.
	name: "an unset pointer is reported as unset rather than empty",
	fn: func(h *fleetHarness) {
		_, found, err := h.f.Target(h.ctx)
		if err != nil {
			h.t.Fatalf("Target: %v", err)
		}
		if found {
			h.t.Fatal("a store with no activation reported one")
		}
	},
}, {
	name: "an activation is readable with the epoch the store assigned",
	fn: func(h *fleetHarness) {
		published, err := h.f.Activate(h.ctx, "rev-1", "first", h.now())
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if published.Epoch <= 0 {
			h.t.Fatalf("epoch = %d, want a positive sequence", published.Epoch)
		}
		got, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		if got.Epoch != published.Epoch || got.RevisionID != "rev-1" {
			h.t.Fatalf("target = %+v, want the published %+v", got, published)
		}
	},
}, {
	// The epoch is a FENCING token: a counter that went backwards would
	// hand a node a number an older revision already used.
	name: "the epoch only ever moves forward",
	fn: func(h *fleetHarness) {
		var last int64
		for i := range 4 {
			got, err := h.f.Activate(h.ctx, fmt.Sprintf("rev-%d", i), "", h.now())
			if err != nil {
				h.t.Fatalf("Activate: %v", err)
			}
			if got.Epoch <= last {
				h.t.Fatalf("epoch %d did not advance past %d", got.Epoch, last)
			}
			last = got.Epoch
		}
	},
}, {
	// Two nodes activating at once must be handed two epochs, not one:
	// that is the whole reason the store assigns it.
	name: "concurrent activations get distinct epochs",
	fn: func(h *fleetHarness) {
		var wg sync.WaitGroup
		epochs := make(chan int64, 8)
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := h.f.Activate(h.ctx, fmt.Sprintf("rev-%d", i), "", h.now())
				if err == nil {
					epochs <- got.Epoch
				}
			}()
		}
		wg.Wait()
		close(epochs)
		seen := map[int64]bool{}
		for epoch := range epochs {
			if seen[epoch] {
				h.t.Fatalf("epoch %d was handed out twice", epoch)
			}
			seen[epoch] = true
		}
		if len(seen) == 0 {
			h.t.Fatal("no activation succeeded at all")
		}
	},
}, {
	// The fleet view was each node reading its own row and drawing a fleet
	// of one.
	name: "every node's apply status is in the fleet view",
	fn: func(h *fleetHarness) {
		at := h.now()
		for i, node := range []string{"node-a", "node-b", "node-c"} {
			if err := h.f.RecordApply(h.ctx, coord.NodeApply{
				NodeID: node, Epoch: 7, Status: "ok",
				UpdatedAt: at.Add(time.Duration(i) * time.Second),
			}); err != nil {
				h.t.Fatalf("RecordApply(%s): %v", node, err)
			}
		}
		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		if len(got) != 3 {
			h.t.Fatalf("the fleet view holds %d nodes, want 3: %+v", len(got), got)
		}
		if got[0].NodeID != "node-c" {
			h.t.Fatalf("fleet[0] = %s, want the freshest report first", got[0].NodeID)
		}
	},
}, {
	name: "a node's later report replaces its earlier one",
	fn: func(h *fleetHarness) {
		at := h.now()
		for _, status := range []string{"applying", "ok"} {
			if err := h.f.RecordApply(h.ctx, coord.NodeApply{
				NodeID: "node-a", Epoch: 7, Status: status, UpdatedAt: at,
			}); err != nil {
				h.t.Fatalf("RecordApply: %v", err)
			}
			at = at.Add(time.Second)
		}
		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		if len(got) != 1 || got[0].Status != "ok" {
			h.t.Fatalf("fleet = %+v, want one node at its latest status", got)
		}
	},
}, {
	// A driver's message with a query in it can be kilobytes, and this
	// value is read by every peer on every reconcile tick.
	name: "a node's failure text is bounded",
	fn: func(h *fleetHarness) {
		huge := make([]byte, 8000)
		for i := range huge {
			huge[i] = 'x'
		}
		if err := h.f.RecordApply(h.ctx, coord.NodeApply{
			NodeID: "node-a", Epoch: 7, Status: "failed",
			Error: string(huge), UpdatedAt: h.now(),
		}); err != nil {
			h.t.Fatalf("RecordApply: %v", err)
		}
		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		if len(got) != 1 {
			h.t.Fatalf("fleet = %+v", got)
		}
		if len(got[0].Error) >= len(huge) {
			h.t.Fatalf("the failure text was stored whole (%d bytes)", len(got[0].Error))
		}
	},
}}

// ---- the token counters ------------------------------------------------- //

const testSeat = "agent:11111111-1111-1111-1111-111111111111"

var budgetCases = []fleetCase{{
	// The reason this moved off the node's own database. Four nodes on one
	// company each kept their own counter, so `token_budget: 500000` was
	// silently four times that — and the config number was decoration.
	name: "one counter however many callers share it",
	fn: func(h *fleetHarness) {
		for i := range 4 {
			if got := h.charge(testSeat, 100, 500, 0); !got.OK {
				h.t.Fatalf("charge %d was refused inside the cap: %+v", i+1, got)
			}
		}
		if got := h.charge(testSeat, 100, 500, 0); !got.OK {
			h.t.Fatalf("the charge that exactly fills the cap was refused: %+v", got)
		}
		got := h.charge(testSeat, 1, 500, 0)
		if got.OK {
			h.t.Fatal("a charge past the org cap was accepted")
		}
		if got.RefusedScope != "org" || got.RefusedLimit != 500 {
			h.t.Fatalf("refusal = %+v, want the org scope and its limit", got)
		}
		if h.used(coord.OrgScope) != 500 {
			h.t.Fatalf("org spend = %d, want the 500 that fit", h.used(coord.OrgScope))
		}
	},
}, {
	name: "a limit of zero is unlimited, not an empty allowance",
	fn: func(h *fleetHarness) {
		// `token_budget: 0` is how an operator says "no ceiling".
		// Reading it as "no allowance" stops every company that never
		// set one — which is most of them.
		if got := h.charge(testSeat, 1_000_000, 0, 0); !got.OK {
			h.t.Fatalf("an unlimited budget refused a charge: %+v", got)
		}
		if h.used(coord.OrgScope) != 1_000_000 {
			h.t.Fatal("an unlimited charge was not counted")
		}
	},
}, {
	name: "a refused seat leaves the org uncharged",
	fn: func(h *fleetHarness) {
		// The property a single SQL transaction used to give for free,
		// and the whole reason this contract cannot be two calls:
		// charging the company for a turn that never ran lets it
		// exhaust its budget on work it did not do. On a KV backend
		// there is no transaction, so the org bump made a moment ago
		// has to be UNWOUND by hand — this is the case that proves it.
		if got := h.charge(testSeat, 90, 0, 100); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		before := h.used(coord.OrgScope)
		got := h.charge(testSeat, 90, 0, 100)
		if got.OK {
			h.t.Fatal("a charge past the seat cap was accepted")
		}
		if got.RefusedScope != "agent" {
			h.t.Fatalf("refusal = %+v, want the agent scope", got)
		}
		if after := h.used(coord.OrgScope); after != before {
			h.t.Fatalf("org spend moved from %d to %d on a REFUSED turn", before, after)
		}
	},
}, {
	name: "a refused org leaves the seat uncharged",
	fn: func(h *fleetHarness) {
		// The other direction. Nothing to unwind here — the org is
		// charged first, so its refusal happens before the seat is
		// touched — which is exactly the property being pinned: a
		// backend that wrote the seat first would burn a seat's own
		// allowance on turns the company refused.
		if got := h.charge(testSeat, 90, 100, 1000); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		before := h.used(testSeat)
		got := h.charge(testSeat, 90, 100, 1000)
		if got.OK {
			h.t.Fatal("a charge past the org cap was accepted")
		}
		if got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope", got)
		}
		if after := h.used(testSeat); after != before {
			h.t.Fatalf("seat spend moved from %d to %d on a REFUSED turn", before, after)
		}
	},
}, {
	name: "an exhausted company is reported before an exhausted seat",
	fn: func(h *fleetHarness) {
		// Both scopes are out of room. "The company is out" is the fact
		// that matters: raising this seat's ceiling against an
		// exhausted org changes nothing, and an operator sent to the
		// seat first finds that out the slow way.
		if got := h.charge(testSeat, 100, 100, 100); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		got := h.charge(testSeat, 50, 100, 100)
		if got.OK {
			h.t.Fatal("a charge past both caps was accepted")
		}
		if got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope reported first", got)
		}
	},
}, {
	name: "a charge larger than the whole cap is refused before anything is written",
	fn: func(h *fleetHarness) {
		// The first-ever charge, against an empty counter. A backend
		// that only checked "existing + delta" on an UPDATE path would
		// accept this one, because there is nothing to update yet.
		got := h.charge(testSeat, 1_000_000, 10, 0)
		if got.OK {
			h.t.Fatal("a charge larger than the entire cap was accepted")
		}
		if got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope", got)
		}
		if h.used(coord.OrgScope) != 0 || h.used(testSeat) != 0 {
			h.t.Fatal("a refused charge still moved a counter")
		}
	},
}, {
	name: "a zero-token charge is neither an error nor a charge",
	fn: func(h *fleetHarness) {
		// A phase whose provider reported no usage still RAN. Refusing
		// it would stop a company over a backend that omits the field.
		if got := h.charge(testSeat, 0, 10, 10); !got.OK {
			h.t.Fatalf("a zero-token charge was refused: %+v", got)
		}
		if h.used(coord.OrgScope) != 0 {
			h.t.Fatal("a zero-token charge moved the counter")
		}
	},
}, {
	name: "two seats spend the org's allowance together",
	fn: func(h *fleetHarness) {
		// Per-SEAT counters are separate; the org's is not. A backend
		// that keyed the org counter per caller would let each seat
		// spend the whole company allowance.
		other := "agent:22222222-2222-2222-2222-222222222222"
		if got := h.charge(testSeat, 60, 100, 0); !got.OK {
			h.t.Fatalf("the first seat was refused: %+v", got)
		}
		got := h.charge(other, 60, 100, 0)
		if got.OK {
			h.t.Fatal("a second seat spent the org allowance the first had already spent")
		}
		if h.used(other) != 0 {
			h.t.Fatal("a refused seat was charged")
		}
	},
}, {
	name: "usage lists the org first, then the seats",
	fn: func(h *fleetHarness) {
		// The operator surface does not sort, and "org" does NOT sort
		// before "agent:…" alphabetically — so a backend that left the
		// order to its own collation would put the company's counter
		// in the middle of its seats.
		h.charge("agent:zzzz", 10, 0, 0)
		h.charge("agent:aaaa", 20, 0, 0)
		rows, err := h.f.Usage(h.ctx)
		if err != nil {
			h.t.Fatalf("Usage: %v", err)
		}
		var scopes []string
		for _, row := range rows {
			scopes = append(scopes, row.Scope)
		}
		want := []string{coord.OrgScope, "agent:aaaa", "agent:zzzz"}
		if !slices.Equal(scopes, want) {
			h.t.Fatalf("scopes = %v, want %v", scopes, want)
		}
		for _, row := range rows {
			if row.Scope == coord.OrgScope && row.Used != 30 {
				h.t.Fatalf("org used = %d, want both seats' spend", row.Used)
			}
		}
	},
}, {
	name: "an unspent scope has spent nothing rather than erroring",
	fn: func(h *fleetHarness) {
		if got := h.used("agent:never-ran"); got != 0 {
			h.t.Fatalf("used = %d for a scope never charged, want 0", got)
		}
	},
}, {
	name: "a reset clears one scope and leaves the rest",
	fn: func(h *fleetHarness) {
		// An OPERATOR action. The counter has no retention precisely so
		// that nothing else can do this: a horizon that rolled the
		// numbers over would silently re-arm a company somebody stopped.
		h.charge(testSeat, 40, 0, 0)
		other := "agent:33333333-3333-3333-3333-333333333333"
		h.charge(other, 40, 0, 0)
		n, err := h.f.Reset(h.ctx, testSeat)
		if err != nil {
			h.t.Fatalf("Reset: %v", err)
		}
		if n != 1 {
			h.t.Fatalf("cleared %d scopes, want 1", n)
		}
		if h.used(testSeat) != 0 {
			h.t.Fatal("the reset scope still has spend")
		}
		if h.used(other) != 40 {
			h.t.Fatal("a reset of one scope cleared another")
		}
		if h.used(coord.OrgScope) != 80 {
			h.t.Fatal("a reset of one seat cleared the org counter")
		}
	},
}, {
	name: "a reset with no scope clears everything",
	fn: func(h *fleetHarness) {
		h.charge(testSeat, 40, 0, 0)
		if _, err := h.f.Reset(h.ctx, ""); err != nil {
			h.t.Fatalf("Reset: %v", err)
		}
		rows, err := h.f.Usage(h.ctx)
		if err != nil {
			h.t.Fatalf("Usage: %v", err)
		}
		if len(rows) != 0 {
			h.t.Fatalf("usage = %v after a full reset, want none", rows)
		}
	},
}, {
	name: "a cleared scope stops being listed at all",
	fn: func(h *fleetHarness) {
		// Not merely zero: an operator who cleared a counter must not
		// still find the scope in `crewlet budgets`, or every seat that
		// ever ran accumulates forever in a view meant to show spend.
		h.charge(testSeat, 40, 0, 0)
		if _, err := h.f.Reset(h.ctx, testSeat); err != nil {
			h.t.Fatalf("Reset: %v", err)
		}
		rows, err := h.f.Usage(h.ctx)
		if err != nil {
			h.t.Fatalf("Usage: %v", err)
		}
		for _, row := range rows {
			if row.Scope == testSeat {
				h.t.Fatalf("the cleared scope is still listed: %+v", row)
			}
		}
	},
}, {
	name: "concurrent charges add up",
	fn: func(h *fleetHarness) {
		// The property a compare-and-swap buys and a read-modify-write
		// does not. Without it N concurrent rounds count as one, and
		// the cap is whatever the last writer happened to see.
		const callers = 8
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = h.f.Charge(h.ctx, testSeat, 10, 0, 0)
			}()
		}
		wg.Wait()
		if got := h.used(coord.OrgScope); got != callers*10 {
			h.t.Fatalf("org spend = %d after %d concurrent charges of 10, want %d",
				got, callers, callers*10)
		}
	},
}}
