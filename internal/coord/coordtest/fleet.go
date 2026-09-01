package coordtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
		{"payload", payloadCases},
		{"channels", channelCases},
		{"fires", fireCases},
		{"sandbox_runs", runCases},
		{"secrets", secretCases},
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
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Summary: "first", Payload: []byte(`{"name":"Acme"}`), At: h.now()})
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
			got, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: fmt.Sprintf("rev-%d", i), Payload: []byte("{}"), At: h.now()})
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
	// THE LOSER OF A RACE IS TOLD. There is no leader here — any node's
	// API may write the config — so without a compare-and-set two
	// operators editing at the same moment both succeed and the later
	// write silently wins. That is a lost edit with a 201 in the operator's
	// hand, and nothing anywhere to find it by.
	name: "an activation expecting a revision the fleet has left is refused",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now()}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		// Somebody else moves the pointer, the way a second node's API
		// would: expecting what this caller also read.
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte(`{"v":2}`), At: h.now(),
			Expect: "rev-1"}); err != nil {
			h.t.Fatalf("the second write was refused: %v", err)
		}
		// And now the first caller's edit, built on rev-1, arrives.
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-3", Payload: []byte(`{"v":3}`), At: h.now(),
			Expect: "rev-1"})
		if !errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("err = %v, want coord.ErrActivationRaced", err)
		}
		// AND IT CHANGED NOTHING. A refusal that had already moved the
		// pointer, or left rev-3's body behind for the next reader, is
		// the same lost edit wearing an error.
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		if target.RevisionID != "rev-2" {
			h.t.Fatalf("the fleet is on %s, want the write that won", target.RevisionID)
		}
		body, ok, err := h.f.Payload(h.ctx, "rev-2")
		if err != nil || !ok || string(body) != `{"v":2}` {
			h.t.Fatalf("payload = %q ok=%v err=%v, want the winner's", body, ok, err)
		}
	},
}, {
	// The matching expectation SUCCEEDS, or the guard above is just a way
	// of refusing every conditional write.
	name: "an activation expecting the current revision succeeds",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now()}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Summary: "second", Payload: []byte(`{"v":2}`),
			At: h.now(), Expect: "rev-1"})
		if err != nil {
			h.t.Fatalf("a matching expectation was refused: %v", err)
		}
		if got.RevisionID != "rev-2" {
			h.t.Fatalf("published %+v", got)
		}
	},
}, {
	// EXPECTING SOMETHING WHEN NOTHING IS ACTIVE IS NOT A RACE.
	//
	// The comparison is "if a pointer exists it must still name this", and
	// with no pointer there is no winner to have lost to. Refusing here
	// looks defensible and breaks a state nodes reach constantly: one
	// seeded from a file has a locally-active revision before it has
	// published anything, so every config write on it would fail until it
	// did — measured as a 409 on a single-node deployment that had done
	// nothing wrong.
	name: "an activation expecting a revision on an empty store still lands",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-0"}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-1" {
			h.t.Fatalf("target = %+v found=%v err=%v, want the write to land",
				target, found, err)
		}
	},
}, {
	// AN EMPTY EXPECTATION IS UNCONDITIONAL, which is the boot publish:
	// a node asserting what it holds rather than editing what it read.
	// Turning that into a race would make a first boot fail against a
	// fleet that had legitimately moved on.
	name: "an activation with no expectation overwrites whatever is there",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now()}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte("{}"), At: h.now()}); err != nil {
			h.t.Fatalf("an unconditional activation was refused: %v", err)
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-2" {
			h.t.Fatalf("target = %+v found=%v err=%v", target, found, err)
		}
	},
}, {
	// EXACTLY ONE OF N CONCURRENT EDITS ON ONE BASE LANDS. This is the
	// property the whole mechanism exists for, and it cannot be read off
	// the single-threaded cases: they prove the comparison happens, this
	// proves it is atomic.
	name: "concurrent edits on one base leave exactly one winner",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "base", Payload: []byte("{}"), At: h.now()}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		var wg sync.WaitGroup
		won := make(chan string, 8)
		raced := make(chan error, 8)
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("edit-%d", i)
				_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
					RevisionID: id, Payload: []byte("{}"), At: h.now(),
					Expect: "base"})
				switch {
				case err == nil:
					won <- id
				case errors.Is(err, coord.ErrActivationRaced):
					raced <- err
				default:
					h.t.Errorf("Activate: %v", err)
				}
			}()
		}
		wg.Wait()
		close(won)
		close(raced)
		if got := len(won); got != 1 {
			h.t.Fatalf("%d of 8 edits on one base were accepted, want exactly 1", got)
		}
		if got := len(raced); got != 7 {
			h.t.Fatalf("%d edits were told they lost, want 7", got)
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
				got, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: fmt.Sprintf("rev-%d", i), Payload: []byte("{}"), At: h.now()})
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

		// NEVER THROUGH A RUNE. The cut was a plain byte slice, so a driver
		// error naming a non-ASCII path ended in half a character — which
		// the KV's JSON encoding replaces with U+FFFD, delivering the fleet
		// view a garbled message rather than a shortened one.
		if err = h.f.RecordApply(h.ctx, coord.NodeApply{
			NodeID: "node-b", Epoch: 7, Status: "failed",
			Error: strings.Repeat("日本語", 1000), UpdatedAt: h.now(),
		}); err != nil {
			h.t.Fatalf("RecordApply: %v", err)
		}
		got, err = h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		for _, n := range got {
			if n.NodeID != "node-b" {
				continue
			}
			if !utf8.ValidString(n.Error) {
				h.t.Error("a non-ASCII failure text was cut through a rune")
			}
			if !strings.HasSuffix(n.Error, "…") {
				h.t.Errorf("the cut is unmarked: %q", n.Error[max(0, len(n.Error)-12):])
			}
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

// ---- the agent-to-agent channels --------------------------------------- //

func (h *fleetHarness) openChannel(id, requester, target string, at time.Time) {
	h.t.Helper()
	if err := h.f.OpenChannel(h.ctx, coord.Channel{
		ID: id, Requester: requester, Target: target, OpenedAt: at, LastAt: at,
	}); err != nil {
		h.t.Fatalf("OpenChannel(%s): %v", id, err)
	}
}

func (h *fleetHarness) channel(id string) (coord.Channel, bool) {
	h.t.Helper()
	ch, found, err := h.f.Channel(h.ctx, id)
	if err != nil {
		h.t.Fatalf("Channel(%s): %v", id, err)
	}
	return ch, found
}

func (h *fleetHarness) closeChannel(id string, at time.Time) (coord.Channel, bool) {
	h.t.Helper()
	ch, found, err := h.f.CloseChannel(h.ctx, id, at)
	if err != nil {
		h.t.Fatalf("CloseChannel(%s): %v", id, err)
	}
	return ch, found
}

func (h *fleetHarness) countChannel(id string, at time.Time) coord.Channel {
	h.t.Helper()
	ch, found, err := h.f.CountChannelMessage(h.ctx, id, at)
	if err != nil {
		h.t.Fatalf("CountChannelMessage(%s): %v", id, err)
	}
	if !found {
		h.t.Fatalf("CountChannelMessage(%s): channel not found", id)
	}
	return ch
}

func (h *fleetHarness) openChannels() []coord.Channel {
	h.t.Helper()
	got, err := h.f.OpenChannels(h.ctx)
	if err != nil {
		h.t.Fatalf("OpenChannels: %v", err)
	}
	return got
}

func channelIDs(chs []coord.Channel) []string {
	out := make([]string, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ch.ID)
	}
	return out
}

var channelCases = []fleetCase{{
	// The reason this moved off the node's own database. The requester's
	// node opens the channel; the ANSWER is published from whichever node
	// owns the target's seat, and that node reads the record to decide
	// whether the reply is authorized. Per-node, that read found nothing
	// and the answer was dropped.
	name: "a channel opened by one node is readable by another",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c1", "alice", "bob", at)

		ch, found := h.channel("c1")
		if !found {
			h.t.Fatal("the channel a peer opened is invisible — a cross-node answer is dropped")
		}
		if ch.Requester != "alice" || ch.Target != "bob" {
			h.t.Errorf("participants = %s -> %s, want alice -> bob", ch.Requester, ch.Target)
		}
		if !ch.Open() {
			h.t.Error("a freshly opened channel reads as closed")
		}
		if !ch.OpenedAt.Equal(at) {
			h.t.Errorf("opened at %v, want %v — a stamp must survive the boundary", ch.OpenedAt, at)
		}
		if ch.Messages != 0 {
			h.t.Errorf("messages = %d, want 0", ch.Messages)
		}
	},
}, {
	// The three-valued rule at its sharpest. "No such channel" is an
	// authorization refusal; only an ERROR may mean "the store could not
	// be read". Every read path has to answer (zero, false, nil).
	name: "an unknown channel is a refusal, never an error",
	fn: func(h *fleetHarness) {
		if _, found := h.channel("missing"); found {
			h.t.Error("Channel invented a record")
		}
		if _, found := h.closeChannel("missing", h.now()); found {
			h.t.Error("CloseChannel invented a record")
		}
		ch, found, err := h.f.CountChannelMessage(h.ctx, "missing", h.now())
		if err != nil {
			h.t.Fatalf("CountChannelMessage on an unknown channel: %v", err)
		}
		if found || ch.ID != "" {
			h.t.Errorf("CountChannelMessage invented a record: %+v", ch)
		}
	},
}, {
	name: "the message counter and the activity stamp both move",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c1", "alice", "bob", at)

		later := at.Add(time.Minute)
		if ch := h.countChannel("c1", later); ch.Messages != 1 {
			h.t.Errorf("count = %d, want 1", ch.Messages)
		}
		// One ask and one answer is the whole protocol, and the two are
		// counted from DIFFERENT nodes — which is why this is a
		// read-modify-write and not a blind put.
		if ch := h.countChannel("c1", later); ch.Messages != 2 {
			h.t.Errorf("count = %d, want 2", ch.Messages)
		}
		ch, _ := h.channel("c1")
		if ch.Messages != 2 {
			h.t.Errorf("stored count = %d, want 2", ch.Messages)
		}
		// The activity stamp moving is what keeps a live channel out of
		// the idle sweep.
		if !ch.LastAt.Equal(later) {
			h.t.Errorf("last activity = %v, want %v", ch.LastAt, later)
		}
	},
}, {
	name: "a second close keeps the first close's instant",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c1", "alice", "bob", at)
		first := at.Add(time.Hour)
		ch, found := h.closeChannel("c1", first)
		if !found {
			h.t.Fatal("CloseChannel did not find the channel it just opened")
		}
		if ch.Open() {
			h.t.Error("a closed channel reads as open")
		}
		// Both parties may close, and the second one is not a fault —
		// but it must not move the timestamp, because the first is when
		// it actually happened. The sweep tells the two apart by
		// comparing the returned instant against the one it passed in.
		second := first.Add(time.Hour)
		again, _ := h.closeChannel("c1", second)
		if !again.ClosedAt.Equal(first) {
			h.t.Errorf("closed at %v, want the first close at %v", again.ClosedAt, first)
		}
		if stored, _ := h.channel("c1"); stored.Open() {
			h.t.Error("the close was not persisted")
		}
	},
}, {
	// The idle sweep's read half. A CLOSED channel re-reported is a second
	// close event for one channel, which draws two closes on a dashboard.
	name: "only open channels are listed, in id order",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c-b", "alice", "bob", at)
		h.openChannel("c-a", "alice", "carol", at)
		h.openChannel("c-c", "alice", "dave", at)
		if _, found := h.closeChannel("c-c", at.Add(time.Minute)); !found {
			h.t.Fatal("CloseChannel lost a channel")
		}

		got := channelIDs(h.openChannels())
		want := []string{"c-a", "c-b"}
		if !slices.Equal(got, want) {
			h.t.Errorf("open channels = %v, want %v", got, want)
		}
	},
}, {
	name: "an open channel is never purged, however old",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("old", "alice", "bob", at)
		h.openChannel("recent", "alice", "carol", at)
		h.openChannel("still-open", "alice", "dave", at)
		if _, found := h.closeChannel("old", at); !found {
			h.t.Fatal("CloseChannel lost a channel")
		}
		if _, found := h.closeChannel("recent", at.Add(10*time.Hour)); !found {
			h.t.Fatal("CloseChannel lost a channel")
		}

		n, err := h.f.PurgeChannels(h.ctx, at.Add(time.Hour))
		if err != nil {
			h.t.Fatalf("PurgeChannels: %v", err)
		}
		if n != 1 {
			h.t.Errorf("purged %d, want 1", n)
		}
		if _, found := h.channel("old"); found {
			h.t.Error("the old channel survived the purge")
		}
		if _, found := h.channel("recent"); !found {
			h.t.Error("a recently-closed channel was purged")
		}
		// A long-running ask must not lose its authorization record
		// while its answer is still in flight — which is exactly why
		// this bucket has no TTL of its own.
		if _, found := h.channel("still-open"); !found {
			h.t.Error("an open channel was purged")
		}
	},
}, {
	// A retried publish of ONE ask presents the same id. It must not wipe
	// the counter or the participants: the second Open is a duplicate, not
	// a new channel.
	name: "a duplicate open is a retry, not a reset",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c1", "alice", "bob", at)
		h.countChannel("c1", at)

		if err := h.f.OpenChannel(h.ctx, coord.Channel{
			ID: "c1", Requester: "mallory", Target: "eve",
			OpenedAt: at.Add(time.Hour), LastAt: at.Add(time.Hour),
		}); err != nil {
			h.t.Fatalf("re-OpenChannel: %v", err)
		}
		ch, _ := h.channel("c1")
		if ch.Requester != "alice" || ch.Target != "bob" || ch.Messages != 1 {
			h.t.Errorf("a duplicate open rewrote the channel: %+v", ch)
		}
	},
}, {
	name: "a caller mutating what it read cannot reach the store",
	fn: func(h *fleetHarness) {
		h.openChannel("c1", "alice", "bob", h.now())
		ch, _ := h.channel("c1")
		ch.Requester = "mallory"
		ch.Messages = 99
		again, _ := h.channel("c1")
		if again.Requester != "alice" || again.Messages != 0 {
			h.t.Errorf("the store took a caller's mutation: %+v", again)
		}
	},
}}

// ---- the scheduled-fire claims ----------------------------------------- //

func (h *fleetHarness) claimFire(key string, at time.Time) bool {
	h.t.Helper()
	won, err := h.f.ClaimFire(h.ctx, key, at)
	if err != nil {
		h.t.Fatalf("ClaimFire(%s): %v", key, err)
	}
	return won
}

var fireCases = []fleetCase{{
	// The reason this moved off the node's own database. The scheduler is a
	// singleton DUTY, so it MOVES — a lease lapse, a drain, a rolling
	// upgrade. The new holder read an empty ledger and its catchup pass
	// re-fired everything the previous holder had already claimed.
	name: "a fire one node claimed is refused to the next holder of the duty",
	fn: func(h *fleetHarness) {
		key := "role|cto|standup|20260314T0900|cto"
		if !h.claimFire(key, h.now()) {
			h.t.Fatal("the first claim lost")
		}
		// A different node, a different tick, the same identity.
		if h.claimFire(key, h.now().Add(time.Minute)) {
			h.t.Error("the same fire was claimed twice — every company gets two standups")
		}
	},
}, {
	name: "a distinct identity is its own claim",
	fn: func(h *fleetHarness) {
		at := h.now()
		// Every component of the identity is part of the key. An `each`
		// fan-out mints one per member precisely so a slow member cannot
		// suppress its siblings, and the minute stamp is what lets the
		// next tick of the same schedule run at all.
		for _, key := range []string{
			"role|cto|standup|20260314T0900|cto",
			"role|cto|standup|20260314T0900|cto-2",
			"role|cto|standup|20260314T0901|cto",
			"role|cto|retro|20260314T0900|cto",
			"unit|platform|standup|20260314T0900|cto",
		} {
			if !h.claimFire(key, at) {
				h.t.Errorf("ClaimFire(%s) was refused — a distinct fire was suppressed", key)
			}
		}
	},
}, {
	// FAILS CLOSED is the caller's polarity, and it only works if a lost
	// race and an unreachable store are distinguishable. An empty key is
	// the one argument fault this can catch locally.
	name: "an unnamed fire is an error, not a lost race",
	fn: func(h *fleetHarness) {
		won, err := h.f.ClaimFire(h.ctx, "", h.now())
		if err == nil {
			t := h.t
			t.Error("ClaimFire accepted an empty identity")
		}
		if won {
			h.t.Error("ClaimFire granted an empty identity")
		}
	},
}}

// ---- the detached sandbox runs ----------------------------------------- //

func (h *fleetHarness) createRun(turnID, value string) bool {
	h.t.Helper()
	created, err := h.f.CreateSandboxRun(h.ctx, turnID, []byte(value))
	if err != nil {
		h.t.Fatalf("CreateSandboxRun(%s): %v", turnID, err)
	}
	return created
}

func (h *fleetHarness) run(turnID string) (coord.Record, bool) {
	h.t.Helper()
	record, found, err := h.f.SandboxRun(h.ctx, turnID)
	if err != nil {
		h.t.Fatalf("SandboxRun(%s): %v", turnID, err)
	}
	return record, found
}

func (h *fleetHarness) updateRun(turnID, value string, version uint64) bool {
	h.t.Helper()
	ok, err := h.f.UpdateSandboxRun(h.ctx, turnID, []byte(value), version)
	if err != nil {
		h.t.Fatalf("UpdateSandboxRun(%s): %v", turnID, err)
	}
	return ok
}

var runCases = []fleetCase{{
	// The reason this moved off the node's own database. A detached run
	// outlives its turn, its process and sometimes its node — and the node
	// that owns the seat AFTERWARDS is the one whose recovery pass has to
	// see it. Per-node, that pass found nothing: the suspended Execute
	// conversation became unreachable and a billed box was neither resumed
	// nor reaped.
	name: "a run one node launched is readable by the seat's next owner",
	fn: func(h *fleetHarness) {
		if !h.createRun("turn-1", `{"status":"running"}`) {
			h.t.Fatal("the first create lost")
		}
		record, found := h.run("turn-1")
		if !found {
			h.t.Fatal("the run a peer launched is invisible — its box leaks")
		}
		if string(record.Value) != `{"status":"running"}` {
			h.t.Errorf("value = %q", record.Value)
		}
		if record.Key != "turn-1" {
			h.t.Errorf("key = %q, want the turn id", record.Key)
		}
		if record.Version == 0 {
			h.t.Error("version = 0 — a caller cannot condition a write on it")
		}
	},
}, {
	// The kick-off turn's id IS the key, so a second create is a retried
	// launch and not a second run. Overwriting would replace the box
	// reference of a job that is already executing.
	name: "a duplicate launch is a retry, not a second run",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running","box":"sbx-1"}`)
		if h.createRun("turn-1", `{"status":"running","box":"sbx-2"}`) {
			h.t.Error("a second create reported itself as new")
		}
		record, _ := h.run("turn-1")
		if string(record.Value) != `{"status":"running","box":"sbx-1"}` {
			h.t.Errorf("value = %q — the retry overwrote a live run", record.Value)
		}
	},
}, {
	// The whole concurrency story. Every mutation on a run is a conditional
	// flip whose condition is one of the run's OWN fields — the at-most-once
	// tail claim above all — so a writer that read a version and lost it
	// must be told, not silently allowed through.
	name: "a stale version loses, and a fresh one wins",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		first, _ := h.run("turn-1")

		if !h.updateRun("turn-1", `{"status":"resumed"}`, first.Version) {
			h.t.Fatal("an update at the version just read was refused")
		}
		// The claim the other node was about to make. It read the same
		// version and must lose.
		if h.updateRun("turn-1", `{"status":"resumed-twice"}`, first.Version) {
			h.t.Error("two writers both won the same version — the tail ran twice")
		}
		second, _ := h.run("turn-1")
		if string(second.Value) != `{"status":"resumed"}` {
			h.t.Errorf("value = %q, want the first writer's", second.Value)
		}
		if second.Version == first.Version {
			h.t.Error("the version did not move, so the next write cannot be conditioned")
		}
		if !h.updateRun("turn-1", `{"status":"done"}`, second.Version) {
			h.t.Error("an update at the version just read was refused")
		}
	},
}, {
	name: "an update to a run that is gone is a lost race, not an error",
	fn: func(h *fleetHarness) {
		if h.updateRun("missing", `{}`, 1) {
			h.t.Error("an update invented a run")
		}
		if _, found := h.run("missing"); found {
			h.t.Error("SandboxRun invented a run")
		}
	},
}, {
	name: "a delete is conditional too",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"done"}`)
		record, _ := h.run("turn-1")

		// A terminal delete raced by a write that reopened the run must
		// not take the reopened row with it.
		gone, err := h.f.DeleteSandboxRun(h.ctx, "turn-1", record.Version+1)
		if err != nil {
			h.t.Fatalf("DeleteSandboxRun: %v", err)
		}
		if gone {
			h.t.Error("a delete at a version that never existed reported success")
		}
		gone, err = h.f.DeleteSandboxRun(h.ctx, "turn-1", record.Version)
		if err != nil {
			h.t.Fatalf("DeleteSandboxRun: %v", err)
		}
		if !gone {
			h.t.Error("a delete at the version just read was refused")
		}
		if _, found := h.run("turn-1"); found {
			h.t.Error("the run survived its delete")
		}
	},
}, {
	// Every listing this serves filters on fields coordination cannot see —
	// the seat, the status, the conversation key, the pause instant — so
	// there is one read and the caller decodes. Ordering by turn id is part
	// of the contract so two backends answer a recovery pass the same way.
	name: "every run is listed, by turn id",
	fn: func(h *fleetHarness) {
		h.createRun("turn-b", `{"seat":"cto"}`)
		h.createRun("turn-a", `{"seat":"ceo"}`)
		h.createRun("turn-c", `{"seat":"cto"}`)

		records, err := h.f.SandboxRuns(h.ctx)
		if err != nil {
			h.t.Fatalf("SandboxRuns: %v", err)
		}
		var keys []string
		for _, r := range records {
			keys = append(keys, r.Key)
		}
		if !slices.Equal(keys, []string{"turn-a", "turn-b", "turn-c"}) {
			h.t.Errorf("runs = %v, want every one of them in turn-id order", keys)
		}
		for _, r := range records {
			if r.Version == 0 || len(r.Value) == 0 {
				h.t.Errorf("listed run %s came back without its value or version: %+v", r.Key, r)
			}
		}
	},
}, {
	name: "a caller mutating a listed value cannot reach the store",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		record, _ := h.run("turn-1")
		for i := range record.Value {
			record.Value[i] = 'x'
		}
		again, _ := h.run("turn-1")
		if string(again.Value) != `{"status":"running"}` {
			h.t.Errorf("the store took a caller's mutation: %q", again.Value)
		}
	},
}, {
	name: "an unnamed run is an error, not a lost race",
	fn: func(h *fleetHarness) {
		created, err := h.f.CreateSandboxRun(h.ctx, "", []byte(`{}`))
		if err == nil {
			h.t.Error("CreateSandboxRun accepted a run with no turn id")
		}
		if created {
			h.t.Error("CreateSandboxRun created a run with no turn id")
		}
	},
}}

// ---- the revision payload ---------------------------------------------- //

var payloadCases = []fleetCase{{
	// The reason the payload travels with the pointer. A peer applies the
	// revision the pointer NAMES by reading it — and while the body lived
	// only in the node's own database, the peer read nothing. A live config
	// change reached exactly the node it was posted to, and every other
	// node served whatever it had booted with, reporting "no such revision"
	// once per reconcile tick for the life of the deployment.
	name: "the revision a peer activated is readable by every node",
	fn: func(h *fleetHarness) {
		body := []byte(`{"name":"Acme","agents":{}}`)
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Summary: "first", Payload: body, At: h.now()})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Payload(h.ctx, published.RevisionID)
		if err != nil {
			h.t.Fatalf("Payload: %v", err)
		}
		if !found {
			h.t.Fatal("the payload a peer activated is invisible — the node cannot converge")
		}
		if string(got) != string(body) {
			h.t.Errorf("payload = %q, want %q", got, body)
		}
	},
}, {
	// A node converging on epoch N must not be handed epoch N+1's body.
	// Applying whatever happened to be there would converge it on a
	// revision the fleet is not pointed at, and report success.
	name: "a superseded revision's payload is absent, not stale",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Summary: "first", Payload: []byte(`{"v":1}`), At: h.now()}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-2", Summary: "second", Payload: []byte(`{"v":2}`), At: h.now()}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if _, found, err := h.f.Payload(h.ctx, "rev-1"); err != nil || found {
			h.t.Errorf("the superseded payload is still served (found=%v err=%v)", found, err)
		}
		got, found, err := h.f.Payload(h.ctx, "rev-2")
		if err != nil || !found {
			h.t.Fatalf("the current payload is missing (found=%v err=%v)", found, err)
		}
		if string(got) != `{"v":2}` {
			h.t.Errorf("payload = %q, want the current revision's", got)
		}
	},
}, {
	name: "nothing activated means no payload, not an error",
	fn: func(h *fleetHarness) {
		got, found, err := h.f.Payload(h.ctx, "rev-1")
		if err != nil {
			h.t.Fatalf("Payload before any activation: %v", err)
		}
		if found || got != nil {
			h.t.Errorf("payload = %q found=%v, want nothing", got, found)
		}
	},
}, {
	// A caller mutating what it read must not reach the store: the body is
	// handed to a decoder that unseals in place on some paths.
	name: "a caller mutating a payload cannot reach the store",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now()}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, _, _ := h.f.Payload(h.ctx, "rev-1")
		for i := range got {
			got[i] = 'x'
		}
		again, _, _ := h.f.Payload(h.ctx, "rev-1")
		if string(again) != `{"v":1}` {
			h.t.Errorf("the store took a caller's mutation: %q", again)
		}
	},
}}

// ---- the sealed credentials -------------------------------------------- //

func (h *fleetHarness) putSecret(name, sealed, keyID string) {
	h.t.Helper()
	err := h.f.PutSecret(h.ctx, coord.SecretRecord{
		Name: name, Value: sealed, KeyID: keyID,
		UpdatedAt: h.now(), UpdatedBy: "operator", Source: "cli",
	})
	if err != nil {
		h.t.Fatalf("PutSecret(%s): %v", name, err)
	}
}

func (h *fleetHarness) secret(name string) (coord.SecretRecord, bool) {
	h.t.Helper()
	rec, found, err := h.f.Secret(h.ctx, name)
	if err != nil {
		h.t.Fatalf("Secret(%s): %v", name, err)
	}
	return rec, found
}

func (h *fleetHarness) secretNames() []string {
	h.t.Helper()
	rows, err := h.f.SecretValues(h.ctx)
	if err != nil {
		h.t.Fatalf("SecretValues: %v", err)
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names
}

var secretCases = []fleetCase{{
	// THE REASON THIS MOVED. A rotation reached the one node whose Tier A
	// file the CLI was pointed at; every other node kept what it booted
	// with, and nothing failed until a seat landed on one of them and the
	// vendor rejected a credential the operator believed they had replaced.
	name: "a credential one node stored is readable by every other",
	fn: func(h *fleetHarness) {
		h.putSecret("SLACK_BOT_TOKEN", "v1:sealed-envelope", "key-1")
		rec, found := h.secret("SLACK_BOT_TOKEN")
		if !found {
			h.t.Fatal("a peer cannot see the credential — the rotation half-landed")
		}
		if rec.Value != "v1:sealed-envelope" {
			h.t.Errorf("value = %q, want the envelope exactly as stored", rec.Value)
		}
		if rec.KeyID != "key-1" {
			h.t.Errorf("KeyID = %q — a rotation sweep cannot find stale rows without it", rec.KeyID)
		}
		if rec.UpdatedBy != "operator" || rec.Source != "cli" {
			h.t.Errorf("provenance lost: by=%q source=%q", rec.UpdatedBy, rec.Source)
		}
	},
}, {
	// The store holds an ENVELOPE and has no key. Anything that mangled
	// the bytes would produce a value that decrypts to nothing on the far
	// side — an auth failure attributed to the vendor rather than to the
	// store that corrupted it.
	name: "the sealed bytes come back byte-identical",
	fn: func(h *fleetHarness) {
		const envelope = `{"k":"key-1","n":"YWJj","c":"ZGVmZ2hpamts+/=="}`
		h.putSecret("WEBHOOK_SECRET", envelope, "key-1")
		rec, found := h.secret("WEBHOOK_SECRET")
		if !found {
			h.t.Fatal("stored and then absent")
		}
		if rec.Value != envelope {
			h.t.Fatalf("value = %q, want %q — the envelope was altered in transit",
				rec.Value, envelope)
		}
	},
}, {
	// ROTATION IS THE COMMON PATH and it is last-write-wins by contract:
	// two operators rotating at once must leave the store holding one of
	// their values, never a failure one of them has to notice and retry.
	name: "a rotation replaces the prior value",
	fn: func(h *fleetHarness) {
		h.putSecret("GITLAB_TOKEN", "v1:old", "key-1")
		h.putSecret("GITLAB_TOKEN", "v2:new", "key-2")
		rec, found := h.secret("GITLAB_TOKEN")
		if !found {
			h.t.Fatal("the rotation removed the credential")
		}
		if rec.Value != "v2:new" || rec.KeyID != "key-2" {
			h.t.Fatalf("value = %q key = %q, want the second write", rec.Value, rec.KeyID)
		}
		if names := h.secretNames(); len(names) != 1 {
			h.t.Fatalf("names = %v — a rotation added a row instead of replacing one", names)
		}
	},
}, {
	// The engine takes ONE snapshot at boot and on every apply, because
	// ${VAR} expansion happens per role, per provider, per MCP server. A
	// listing that missed a name resolves it to an empty string on every
	// node at once.
	name: "the listing is every credential, by name",
	fn: func(h *fleetHarness) {
		for _, name := range []string{"ZULU", "ALPHA", "MIKE"} {
			h.putSecret(name, "v1:"+name, "key-1")
		}
		got := h.secretNames()
		want := []string{"ALPHA", "MIKE", "ZULU"}
		if len(got) != len(want) {
			h.t.Fatalf("names = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				h.t.Fatalf("names = %v, want %v — the order differs between "+
					"backends, so a diff of two captures is unreadable", got, want)
			}
		}
	},
}, {
	// An absent credential is a THREE-VALUED answer's middle case, and it
	// has to be distinguishable from a read that failed: the first is an
	// operator who has not set it yet, the second is a store outage that
	// must never render as one.
	name: "an unset credential is absent rather than empty",
	fn: func(h *fleetHarness) {
		rec, found := h.secret("NEVER_SET")
		if found {
			h.t.Fatalf("an unset name answered with %+v", rec)
		}
	},
}, {
	name: "deleting reports whether it was there, and it stays gone",
	fn: func(h *fleetHarness) {
		h.putSecret("RETIRED", "v1:sealed", "key-1")
		removed, err := h.f.DeleteSecret(h.ctx, "RETIRED")
		if err != nil {
			h.t.Fatalf("DeleteSecret: %v", err)
		}
		if !removed {
			h.t.Fatal("deleting a stored credential reported it absent")
		}
		if _, found := h.secret("RETIRED"); found {
			h.t.Fatal("the credential survived its delete")
		}
		// AGAIN. An operator re-running the command, or two of them, must
		// get "there was nothing" rather than an error.
		again, err := h.f.DeleteSecret(h.ctx, "RETIRED")
		if err != nil {
			h.t.Fatalf("second DeleteSecret: %v", err)
		}
		if again {
			h.t.Error("a second delete claimed to have removed something")
		}
	},
}, {
	// A name is an environment-variable name and those are not restricted
	// to what a KV key permits. A backend that stored one and could not
	// list it back would drop the credential from the boot snapshot while
	// answering a direct read — the worst shape, because it works in a
	// test that looks it up by name.
	name: "an awkward name survives storage and listing",
	fn: func(h *fleetHarness) {
		const name = "acme.co/token-v2_PROD"
		h.putSecret(name, "v1:sealed", "key-1")
		if _, found := h.secret(name); !found {
			h.t.Fatal("a direct read cannot find it")
		}
		names := h.secretNames()
		if len(names) != 1 || names[0] != name {
			h.t.Fatalf("names = %v, want [%s] — the boot snapshot would miss it",
				names, name)
		}
	},
}, {
	// An unsealed value is a caller that forgot to encrypt, not a secret
	// whose value is empty. Storing it resolves as an empty ${VAR} on
	// every node — which is the failure this bucket exists to prevent, so
	// it is refused at the door rather than replicated.
	name: "an unsealed value is refused rather than replicated",
	fn: func(h *fleetHarness) {
		err := h.f.PutSecret(h.ctx, coord.SecretRecord{
			Name: "EMPTY", UpdatedAt: h.now(),
		})
		if err == nil {
			h.t.Fatal("a credential with no sealed value was accepted")
		}
		if _, found := h.secret("EMPTY"); found {
			h.t.Fatal("the refused write landed anyway")
		}
	},
}, {
	name: "a nameless credential is refused",
	fn: func(h *fleetHarness) {
		if err := h.f.PutSecret(h.ctx, coord.SecretRecord{Value: "v1:x"}); err == nil {
			h.t.Fatal("a credential with no name was accepted")
		}
		if _, _, err := h.f.Secret(h.ctx, ""); err == nil {
			h.t.Fatal("reading an empty name was accepted")
		}
		if _, err := h.f.DeleteSecret(h.ctx, ""); err == nil {
			h.t.Fatal("deleting an empty name was accepted")
		}
	},
}}
