package coordtest

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

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
	// THE RATE STORE'S WIDEST WINDOW IS ONE COUNT, and one past it is
	// counted too, in the store that keeps windows up to a day: a backend
	// that kept a window shorter than its caller counts over would restart
	// the count mid-window (the Dashboard Assistant's turn rate is per
	// minute), and one that refused a window it can keep would turn a
	// caller's rate into an error.
	name: "the rate store's widest window is one count, and one past it is counted too",
	fn: func(h *fleetHarness) {
		at := h.now().Truncate(coord.MaxRateWindow)
		if !h.allow("assistant.turn:p", 1, coord.MaxRateWindow, at) {
			h.t.Fatal("the first call in the widest window was refused")
		}
		if h.allow("assistant.turn:p", 1, coord.MaxRateWindow, at.Add(coord.MaxRateWindow-time.Second)) {
			h.t.Fatal("a later instant in the widest window got its own count")
		}
		wider := coord.MaxRateWindow + time.Second
		start := h.now().Truncate(wider)
		if !h.allow("assistant.turn:q", 1, wider, start) {
			h.t.Fatal("the first call in a window just past coord.MaxRateWindow was refused")
		}
		if h.allow("assistant.turn:q", 1, wider, start.Add(wider-time.Second)) {
			h.t.Fatal("a window just past coord.MaxRateWindow restarted its count mid-window")
		}
	},
}, {
	// A seat's atlas records are counted in a DAY'S window
	// (internal/agent/builtin's AtlasRecordsPerDay). A backend that kept a
	// window only as long as the valve's seconds would forget the day's
	// count between two records an hour apart and hand the day back its
	// whole allowance.
	// The window is the UTC day: midnight to midnight.
	name: "a day's window is one count from one UTC midnight to the next",
	fn: func(h *fleetHarness) {
		midnight := h.now().Truncate(24 * time.Hour)
		if midnight.Hour() != 0 || midnight.Minute() != 0 {
			h.t.Fatalf("a day's window starts at %s, not at a UTC midnight", midnight)
		}
		if !h.allow("atlas-records:ceo", 2, 24*time.Hour, midnight) {
			h.t.Fatal("the day's first charge was refused")
		}
		if !h.allow("atlas-records:ceo", 2, 24*time.Hour, midnight.Add(23*time.Hour)) {
			h.t.Fatal("a charge late in the day was refused with room left")
		}
		if h.allow("atlas-records:ceo", 2, 24*time.Hour, midnight.Add(24*time.Hour-time.Second)) {
			h.t.Fatal("a third charge in the day passed a limit of two: the day's count was forgotten")
		}
		if !h.allow("atlas-records:ceo", 2, 24*time.Hour, midnight.Add(24*time.Hour)) {
			h.t.Fatal("the next day inherited the previous one's count")
		}
	},
}, {
	// A window's record is its name, its width and its start, so a name
	// counted at two widths keeps two counts — at the instant both windows
	// start at as much as at any other.
	name: "one name counted at two widths keeps two counts",
	fn: func(h *fleetHarness) {
		midnight := h.now().Truncate(24 * time.Hour)
		if !h.allow("seat:ceo", 1, time.Second, midnight) {
			h.t.Fatal("the second-wide window's first call was refused")
		}
		if !h.allow("seat:ceo", 1, 24*time.Hour, midnight) {
			h.t.Fatal("the day-wide window shares the second-wide window's count")
		}
	},
}, {
	// No backend keeps a window past coord.MaxCounterWindow until it
	// closes, so counting one would restart it mid-window: an error, which
	// a caller cannot mistake for room or for a full window.
	name: "a window longer than a day is refused",
	fn: func(h *fleetHarness) {
		ok, err := h.f.Allow(h.ctx, "too-long", 1, coord.MaxCounterWindow+time.Hour, h.now())
		if err == nil || ok {
			h.t.Fatalf("Allow over a %s window = (%v, %v), want an error", coord.MaxCounterWindow+time.Hour,
				ok, err)
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
	// ACROSS FOUR NODES, and EXACTLY the limit with no error: every race a
	// caller loses is another caller's increment, so contention ends in an
	// answer — room, or a full window — and never in "could not say", which
	// the notification path takes as open. A limit above sixteen, so a
	// backend that bounds its retries by a count it guessed is found out.
	name: "concurrent callers on every node pass exactly the limit",
	fn: func(h *fleetHarness) {
		const limit, callers = 24, 64
		at := h.now()
		nodes := h.nodes(4)
		var (
			wg      sync.WaitGroup
			allowed atomic.Int32
		)
		errs := make(chan error, callers)
		for i := range callers {
			wg.Go(func() {
				ok, err := nodes[i%len(nodes)].Allow(h.ctx, "seat:hot", limit, time.Second, at)
				switch {
				case err != nil:
					errs <- err
				case ok:
					allowed.Add(1)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			h.t.Fatalf("a caller under contention was told the valve could not say: %v", err)
		}
		if got := allowed.Load(); got != limit {
			h.t.Fatalf("%d of %d concurrent callers passed a limit of %d, want exactly %d",
				got, callers, limit, limit)
		}
	},
}}

// ---- the delivery claims ------------------------------------------------ //

var claimCases = []fleetCase{{
	// A third-party app retrying a delivery to a DIFFERENT ingress node found no
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
	// EACH CLAIM LAPSES AT ITS OWN TTL. A webhook claims for five minutes
	// and a Mattermost socket for thirty, in one store; a backend that
	// held every claim for its bucket's age swallowed a webhook replay for
	// the socket's window, and one that held every claim for the shortest
	// let a reconnecting seat deliver a post twice.
	//
	// It travels the clock rather than waiting: the deadline is the
	// record's, judged against the `now` the next caller passes, so a
	// backend that reaped on its own clock instead could not pass it.
	name: "a claim lapses at its own ttl and can then be taken again once",
	fn: func(h *fleetHarness) {
		h.report(CheckClaimLapsesAtItsOwnTTL(h.ctx, h.f, h.now()))
	},
}, {
	// REFUSED, never clamped: a claim the store cannot keep for as long as
	// it was asked lets through the duplicate it was sized to stop.
	name: "a claim longer than the store can keep is refused",
	fn: func(h *fleetHarness) {
		won, err := h.f.Claim(h.ctx, "gitlab|forever", 1000*time.Hour, h.now())
		if !errors.Is(err, coord.ErrTTLTooLong) {
			h.t.Fatalf("Claim(1000h) = (%v, %v), want an error wrapping coord.ErrTTLTooLong", won, err)
		}
		if won {
			h.t.Fatal("a refused claim reported a win")
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
	// FAILS OPEN is the caller's polarity, and it only works if a lost
	// race and a store that could not answer are distinguishable. An empty
	// key is the one argument fault a backend can catch locally.
	name: "an unnamed record is an error, not a race somebody else won",
	fn:   func(h *fleetHarness) { h.report(CheckUnnamedRecordsAreRefused(h.ctx, h.f, h.now())) },
}, {
	// AND A RACE SOMEBODY ELSE WON IS NOT AN OUTAGE, above all over a
	// record just removed: a create over the marker a removal leaves is a
	// compare-and-set every loser of which was beaten by a first writer.
	// The check covers every verb whose record can be removed, not just
	// the claims — see it for why — and a replicated stream is where it
	// bites, which internal/queue/jetstream/jetstreamtest carries it to.
	name: "creates racing over a removed record are races, never outages",
	fn: func(h *fleetHarness) {
		h.report(CheckCreatesOverARemovedRecordAreRaces(h.ctx, h.f, h.now()))
	},
}, {
	name: "exactly one of many concurrent callers claims",
	fn: func(h *fleetHarness) {
		at := h.now()
		var wg sync.WaitGroup
		won := make(chan bool, 16)
		for range 16 {
			wg.Go(func() {
				ok, err := h.f.Claim(h.ctx, "mattermost|race", time.Minute, at)
				won <- err == nil && ok
			})
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
	// WHEN A KEY WAS WORKED is the first record's instant: the work pickup
	// spaces a recheck from the end of the item's last pickup by it.
	name: "a worked key says when its first record was made",
	fn: func(h *fleetHarness) {
		first := h.now().Add(-time.Hour).UTC().Truncate(time.Second)
		h.record("ceo", "wk-1", "turn-a", first)
		h.record("ceo", "wk-1", "turn-b", h.now())
		got := h.workedAt("ceo", "wk-1", "wk-2")
		if at, ok := got["wk-1"]; !ok || !at.Equal(first) {
			h.t.Fatalf("a key first recorded at %s answered %s (worked %v)", first, at, ok)
		}
		if _, ok := got["wk-2"]; ok {
			h.t.Fatal("an unrecorded key came back worked")
		}
		if _, ok := h.workedAt("cto", "wk-1")["wk-1"]; ok {
			h.t.Fatal("one seat's completion answered for another seat")
		}
	},
}, {
	// WHO WORKED A KEY is the first record's detail: a skipped delivery names
	// the turn whose run read it (internal/engine's skipJoined).
	name: "a worked key says which record worked it first",
	fn: func(h *fleetHarness) {
		h.record("ceo", "wk-1", "turn-a", h.now())
		h.record("ceo", "wk-1", "turn-b", h.now().Add(time.Second))
		got := h.workedBy("ceo", "wk-1", "wk-2")
		if by, ok := got["wk-1"]; !ok || by != "turn-a" {
			h.t.Fatalf("a key first recorded by turn-a answered %q (worked %v)", by, ok)
		}
		if _, ok := got["wk-2"]; ok {
			h.t.Fatal("an unrecorded key came back worked")
		}
		if _, ok := h.workedBy("cto", "wk-1")["wk-1"]; ok {
			h.t.Fatal("one seat's completion answered for another seat")
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
			wg.Go(func() {
				errs <- h.f.Record(h.ctx, "ceo", "wk-race",
					fmt.Sprintf("turn-%d", i), h.now())
			})
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

// ---- the scheduled-fire claims ----------------------------------------- //

func (h *fleetHarness) claimFire(minute, runner string, at time.Time) bool {
	h.t.Helper()
	won, err := h.f.ClaimFire(h.ctx, minute, runner, at)
	if err != nil {
		h.t.Fatalf("ClaimFire(%s, %q): %v", minute, runner, err)
	}
	return won
}

func (h *fleetHarness) fireMinuteClaimed(minute string) bool {
	h.t.Helper()
	held, err := h.f.FireMinuteClaimed(h.ctx, minute)
	if err != nil {
		h.t.Fatalf("FireMinuteClaimed(%s): %v", minute, err)
	}
	return held
}

var fireCases = []fleetCase{{
	// The reason this moved off the node's own database. The scheduler is a
	// singleton DUTY, so it MOVES: a lease lapse, a drain, a rolling
	// upgrade. The new holder read an empty ledger and its catchup pass
	// re-fired everything the previous holder had already claimed.
	name: "a fire one node claimed is refused to the next holder of the duty",
	fn: func(h *fleetHarness) {
		minute := "role|cto|standup|20260314T0900"
		if !h.claimFire(minute, "cto", h.now()) {
			h.t.Fatal("the first claim lost")
		}
		// A different node, a different tick, the same identity.
		if h.claimFire(minute, "cto", h.now().Add(time.Minute)) {
			h.t.Error("the same fire was claimed twice, so every company gets two standups")
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
		for _, c := range []struct{ minute, runner string }{
			{"role|cto|standup|20260314T0900", "cto"},
			{"role|cto|standup|20260314T0900", "cto-2"},
			{"role|cto|standup|20260314T0901", "cto"},
			{"role|cto|retro|20260314T0900", "cto"},
			{"unit|platform|standup|20260314T0900", "cto"},
		} {
			if !h.claimFire(c.minute, c.runner, at) {
				h.t.Errorf("ClaimFire(%s, %q) was refused, so a distinct fire was suppressed",
					c.minute, c.runner)
			}
		}
	},
}, {
	// A catchup skip resolved no runner and claims the minute under an
	// EMPTY one. That has to be a claim of its own: refused a second time,
	// and colliding with no named runner in either order, or recording the
	// skip would spend the claim a corrected later tick needs.
	name: "an empty runner is a claim of its own beside every named one",
	fn: func(h *fleetHarness) {
		minute := "role|cto|standup|20260314T0900"
		if !h.claimFire(minute, "", h.now()) {
			h.t.Fatal("the skip's claim lost on an empty minute")
		}
		if h.claimFire(minute, "", h.now()) {
			h.t.Error("the skip's claim was granted twice")
		}
		if !h.claimFire(minute, "cto", h.now()) {
			h.t.Error("a runner's claim lost to the skip's, so the skip suppressed a fire")
		}
		other := "role|cto|standup|20260314T1000"
		if !h.claimFire(other, "cto", h.now()) || !h.claimFire(other, "", h.now()) {
			h.t.Error("a skip's claim lost to a runner's claim on the same minute")
		}
	},
}, {
	// The question the catchup pass asks after a restart. A minute any
	// runner was claimed for ran, or was held back on purpose, and must
	// not read as missed: a skip written for it puts the same tick in the
	// schedule's history twice.
	name: "a minute reads claimed once any runner of it is, and only then",
	fn: func(h *fleetHarness) {
		minute := "unit|platform|standup|20260314T0900"
		if h.fireMinuteClaimed(minute) {
			h.t.Fatal("a minute nobody claimed reads claimed")
		}
		h.claimFire(minute, "eng-2", h.now())
		if !h.fireMinuteClaimed(minute) {
			h.t.Error("a minute a runner was claimed for reads unclaimed, so the catchup " +
				"pass records the fire it sent as missed")
		}

		skipped := "unit|platform|standup|20260315T0900"
		h.claimFire(skipped, "", h.now())
		if !h.fireMinuteClaimed(skipped) {
			h.t.Error("a minute a skip was recorded for reads unclaimed, so every restart " +
				"records it again")
		}
	},
}, {
	// Each minute's read is its own. The neighbours vary one part of the
	// minute at a time, and one of them EXTENDS the minute's text, which is
	// the shape a prefix match on a joined key gets wrong.
	name: "a claim on another minute does not answer for this one",
	fn: func(h *fleetHarness) {
		minute := "role|cto|standup|20260314T0900"
		for _, neighbour := range []string{
			"role|cto|standup|20260314T0901",
			"role|cto|standups|20260314T0900",
			"role|cto|standup|20260314T0900|cto",
			"role|cto|standup|20260314T0900.cto",
			"role|ct|standup|20260314T0900",
		} {
			h.claimFire(neighbour, "cto", h.now())
			h.claimFire(neighbour, "", h.now())
		}
		if h.fireMinuteClaimed(minute) {
			h.t.Error("a minute nobody claimed reads claimed because a neighbouring minute was")
		}
	},
}, {
	// FAILS CLOSED is the caller's polarity, and it only works if a lost
	// race and an unreachable store are distinguishable. An empty minute is
	// the one argument fault this can catch locally, and on the read it
	// matters as much: answered "unclaimed", it writes a skip.
	name: "an unnamed minute is an error, not a lost race or an empty answer",
	fn: func(h *fleetHarness) {
		won, err := h.f.ClaimFire(h.ctx, "", "cto", h.now())
		if err == nil {
			h.t.Error("ClaimFire accepted an empty minute")
		}
		if won {
			h.t.Error("ClaimFire granted an empty minute")
		}
		held, err := h.f.FireMinuteClaimed(h.ctx, "")
		if err == nil {
			h.t.Error("FireMinuteClaimed answered for an empty minute")
		}
		if held {
			h.t.Error("FireMinuteClaimed reported an empty minute claimed")
		}
	},
}}

// ---- the custody claims ------------------------------------------------ //

func (h *fleetHarness) claimCustody(batch, node string) string {
	h.t.Helper()
	keeper, err := h.f.ClaimCustody(h.ctx, batch, node)
	if err != nil {
		h.t.Fatalf("ClaimCustody(%s, %s): %v", batch, node, err)
	}
	return keeper
}

var custodyCases = []fleetCase{{
	// THE WHOLE POINT: two data nodes wrote one batch, and exactly one of
	// them keeps it. The one that claimed second is told who did, which is
	// what it deletes its own copy on.
	name: "the first claimant keeps a batch and the second is told who does",
	fn: func(h *fleetHarness) {
		if got := h.claimCustody("batch-1", "data-a"); got != "data-a" {
			h.t.Fatalf("the first claim answered %q, want the claimant", got)
		}
		if got := h.claimCustody("batch-1", "data-b"); got != "data-a" {
			h.t.Errorf("a second node's claim answered %q, want data-a — answered "+
				"itself, it would keep a second copy of a batch data-a keeps", got)
		}
	},
}, {
	// A node that crashed after claiming settles its copy at boot by
	// claiming AGAIN, and must be told it keeps it — or it deletes the
	// one copy the fleet has.
	name: "the keeper claiming again is told it keeps the batch",
	fn: func(h *fleetHarness) {
		h.claimCustody("batch-2", "data-a")
		if got := h.claimCustody("batch-2", "data-a"); got != "data-a" {
			h.t.Errorf("the keeper's second claim answered %q, want itself", got)
		}
	},
}, {
	name: "each batch is its own decision",
	fn: func(h *fleetHarness) {
		h.claimCustody("batch-3", "data-a")
		if got := h.claimCustody("batch-4", "data-b"); got != "data-b" {
			h.t.Errorf("a claim on another batch answered %q, want its own claimant", got)
		}
	},
}, {
	// CONCURRENT CLAIMS AGREE: every claimant is told the same keeper, or
	// two of them would each keep — or each delete — their copy.
	name: "racing claims are all told one keeper",
	fn: func(h *fleetHarness) {
		const claimants = 8
		answers := make(chan string, claimants)
		var wg sync.WaitGroup
		for i := range claimants {
			wg.Go(func() {
				keeper, err := h.f.ClaimCustody(h.ctx, "batch-race", fmt.Sprintf("data-%d", i))
				if err != nil {
					h.t.Errorf("ClaimCustody: %v", err)
					return
				}
				answers <- keeper
			})
		}
		wg.Wait()
		close(answers)
		var keeper string
		for got := range answers {
			if keeper == "" {
				keeper = got
			}
			if got != keeper {
				h.t.Fatalf("racing claims were told %q and %q — two nodes keep or "+
					"lose one batch", keeper, got)
			}
		}
	},
}, {
	// The caller's mistake, never coord.ErrCustodyUndecidable: that is the
	// one DEFINITE claim error, a fact about a record the store holds, and
	// a keeper acknowledges the delivery and steps over the batch on it —
	// so a backend that spent it on a malformed claim would have a batch
	// nobody claimed left unsettled as though the fleet had a record of it.
	name: "an unnamed batch or claimant is an error, not a keeper",
	fn: func(h *fleetHarness) {
		for _, c := range [][2]string{{"", "data-a"}, {"batch-5", ""}} {
			keeper, err := h.f.ClaimCustody(h.ctx, c[0], c[1])
			if err == nil || keeper != "" {
				h.t.Errorf("ClaimCustody(%q, %q) = (%q, %v), want an error and no keeper",
					c[0], c[1], keeper, err)
			}
			if errors.Is(err, coord.ErrCustodyUndecidable) {
				h.t.Errorf("ClaimCustody(%q, %q) = %v, which says the store holds a "+
					"record of the batch's keeper nothing can read — it holds none", c[0], c[1], err)
			}
		}
	},
}}
