package coordtest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
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
//
// newFleet makes the fresh store one case runs against and answers how a NODE
// opens a handle on it. Most cases need one handle; the cases about what
// concurrent writers must agree on spread their writers across several
// ([fleetHarness.nodes]), because a backend may serialise one handle's writes
// before its store ever sees them — the KV store sends a node's concurrent
// writes to one token counter as a single compare-and-swap — and a race run
// through one handle would then certify nothing about how the store itself
// arbitrates between nodes. A backend whose store is its handle answers the
// same handle every time.
func RunFleet(t *testing.T, newFleet func(t *testing.T) (node func() coord.Fleet)) {
	t.Helper()
	groups := []struct {
		name  string
		cases []fleetCase
	}{
		{"valve", valveCases},
		{"claims", claimCases},
		{"ledger", ledgerCases},
		{"budgets", budgetCases},
		{"plane", planeCases},
		{"payload", payloadCases},
		{"payload_race", payloadRaceCases},
		{"channels", channelCases},
		{"follows", followCases},
		{"chat_reads", chatReadCases},
		{"chat_read_watch", chatReadWatchCases},
		{"fires", fireCases},
		{"custody", custodyCases},
		{"rebases", rebaseCases},
		{"sandbox_runs", runCases},
		{"secrets", secretCases},
		{"integrations", integrationCases},
		{"seat_access", seatAccessCases},
		{"mailboxes", mailboxCases},
		{"object stores", objectStoreCases},
		{"trash reaps", trashReapCases},
		{"upload reservations", uploadReservationCases},
		{"seat_pauses", seatPauseCases},
		{"model_retries", modelRetryCases},
		{"device_sign_ins", deviceSignInCases},
		{"mcp_listings", mcpListingCases},
		{"mcp_restarts", mcpRestartCases},
		{"maintenance", maintenanceCases},
		{"backups", backupCases},
		{"listings", listingCases},
		{"markers", markerCases},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					node := newFleet(t)
					f := node()
					if f == nil {
						t.Fatal("newFleet opened a nil backend")
					}
					// NOT t.Context(), for the reason [newHarness]
					// states: that one is cancelled before cleanup
					// runs, and the concurrency cases join goroutines
					// that are still mid-call at that point.
					ctx, cancel := context.WithTimeout(context.Background(), stallBudget)
					t.Cleanup(cancel)
					c.fn(&fleetHarness{t: t, ctx: ctx, f: f, node: node})
				})
			}
		})
	}
}

type fleetCase struct {
	name string
	fn   func(h *fleetHarness)
}

// anOrigin is the revision origin every activation in the suite carries but
// the cases about the origin itself: a backend refuses an activation without
// one, so a case about something else states one and moves on.
var anOrigin = coord.RevisionOrigin{
	Author: "suite", AuthorKind: iam.ActorOperator, Source: "api",
	CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

// fleetHarness is a backend plus the assertions the cases are written in.
type fleetHarness struct {
	t   *testing.T
	ctx context.Context
	f   coord.Fleet

	// node opens another handle on f's store, as another node would.
	node func() coord.Fleet
}

// nodes is n handles on the case's store, each a node of its own: f, and n-1
// opened beside it.
func (h *fleetHarness) nodes(n int) []coord.Fleet {
	h.t.Helper()
	out := []coord.Fleet{h.f}
	for range n - 1 {
		peer := h.node()
		if peer == nil {
			h.t.Fatal("newFleet opened a nil peer")
		}
		out = append(out, peer)
	}
	return out
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

// report hands the errors a shared check found to the test.
//
// The checks are functions returning errors rather than case bodies for the
// reason fleetchecks.go gives: a body that can only fail a
// *testing.T is a body nothing in this package can prove fails at all.
func (h *fleetHarness) report(errs []error) {
	h.t.Helper()
	for _, err := range errs {
		h.t.Error(err)
	}
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
	// THE WIDEST WINDOW IS COUNTED, and one past it is an error: a backend
	// that kept a window shorter than its caller counts over would restart
	// the count mid-window (the Dashboard Assistant's turn rate is per
	// minute), and one that took a wider window and reaped it would pass
	// far more than the caller asked for.
	name: "the widest window is counted and one past it is refused",
	fn: func(h *fleetHarness) {
		at := h.now().Truncate(coord.MaxRateWindow)
		if !h.allow("assistant.turn:p", 1, coord.MaxRateWindow, at) {
			h.t.Fatal("the first call in the widest window was refused")
		}
		if h.allow("assistant.turn:p", 1, coord.MaxRateWindow, at.Add(coord.MaxRateWindow-time.Second)) {
			h.t.Fatal("a later instant in the widest window got its own count")
		}
		if ok, err := h.f.Allow(h.ctx, "assistant.turn:q", 1, coord.MaxRateWindow+time.Second, at); err == nil {
			h.t.Fatalf("a window past coord.MaxRateWindow was counted (ok %v) rather than refused", ok)
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
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Summary: "first", Payload: []byte(`{"name":"Acme"}`),
			At: h.now(), Origin: anOrigin,
		})
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
	// THE AUTHOR TRAVELS WITH THE POINTER, whole. A peer meets a revision
	// for the first time when the pointer names it, so what the pointer
	// drops, every node but the origin records as unknown — which is how
	// every adopted revision came to read "peer" on the audit screen. The
	// credential is part of it: a person writing through their own machine
	// token and the same person signed in are one author and two
	// credentials, and a backend that dropped the second would have every
	// other node's history unable to say which.
	name: "a pointer carries the revision's origin to every reader",
	fn: func(h *fleetHarness) {
		written := h.now().Add(-time.Hour)
		origin := coord.RevisionOrigin{
			Author: "maya", AuthorKind: iam.ActorOperator,
			OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a",
			Source:     "api", CreatedAt: written,
		}
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Summary: "first", Payload: []byte("{}"),
			At: h.now(), Origin: origin,
		})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		for name, read := range map[string]coord.RevisionOrigin{
			"the activation answered": published.Origin, "the pointer read back": got.Origin,
		} {
			if read.Author != "maya" || read.AuthorKind != iam.ActorOperator ||
				read.OperatorID != origin.OperatorID || read.Source != "api" ||
				!read.CreatedAt.Equal(written) {
				h.t.Fatalf("%s carries origin %+v, want %+v", name, read, origin)
			}
		}
	},
}, {
	// AN AUTHOR KIND THIS BUILD CANNOT NAME IS CARRIED AS THE VALUE IT IS.
	// The kind is an enum read off the wire, so a newer peer's kind arrives
	// as a value nobody here has a constant for — and a backend that mapped
	// it onto a kind that exists would have every node's history attribute
	// the revision to the wrong sort of author, with nothing to say so.
	name: "an author kind this build does not know round-trips unchanged",
	fn: func(h *fleetHarness) {
		unknown := iam.ActorKind("a_kind_from_a_newer_build")
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Origin: coord.RevisionOrigin{
				Author: "x", AuthorKind: unknown, Source: "api", CreatedAt: h.now(),
			},
		})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		for name, read := range map[string]coord.RevisionOrigin{
			"the activation answered": published.Origin, "the pointer read back": got.Origin,
		} {
			if read.AuthorKind != unknown || read.AuthorKind.Valid() {
				h.t.Fatalf("%s carries kind %q, want %q unchanged", name, read.AuthorKind, unknown)
			}
		}
	},
}, {
	// A POINTER NAMING NOBODY IS REFUSED, and leaves nothing behind. Every
	// writer knows who it is, how it made the revision and when; a pointer
	// that left any of them out would put the revision on every peer with
	// an author, a source or an instant somebody would have to guess.
	name: "an activation without its revision's origin is refused",
	fn: func(h *fleetHarness) {
		for name, origin := range map[string]coord.RevisionOrigin{
			"no origin at all": {},
			"no author kind":   {Author: "maya", Source: "api", CreatedAt: h.now()},
			"no source":        {Author: "maya", AuthorKind: iam.ActorOperator, CreatedAt: h.now()},
			"no creation":      {Author: "maya", AuthorKind: iam.ActorOperator, Source: "api"},
		} {
			_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
				RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(), Origin: origin,
			})
			if !errors.Is(err, coord.ErrIncompleteOrigin) {
				h.t.Fatalf("%s: Activate = %v, want coord.ErrIncompleteOrigin", name, err)
			}
		}
		if got, found, err := h.f.Target(h.ctx); err != nil || found {
			h.t.Fatalf("Target = (%+v, %v, %v) after refused activations, want unset",
				got, found, err)
		}
		if _, found, err := h.f.Payload(h.ctx, "rev-1"); err != nil || found {
			h.t.Fatalf("Payload(rev-1) = (found=%v, %v): a refused activation left its body",
				found, err)
		}
		// The author's LABEL alone may be empty: the kind still says what
		// wrote it.
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Origin: coord.RevisionOrigin{AuthorKind: iam.ActorOperator, Source: "api", CreatedAt: h.now()},
		}); err != nil {
			h.t.Fatalf("an origin with no author label: %v", err)
		}
	},
}, {
	// The epoch is a FENCING token: a counter that went backwards would
	// hand a node a number an older revision already used.
	name: "the epoch only ever moves forward",
	fn: func(h *fleetHarness) {
		var last int64
		for i := range 4 {
			got, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: fmt.Sprintf("rev-%d", i), Payload: []byte("{}"), At: h.now(), Origin: anOrigin})
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
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		// Somebody else moves the pointer, the way a second node's API
		// would: expecting what this caller also read.
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte(`{"v":2}`), At: h.now(),
			Expect: "rev-1", Origin: anOrigin}); err != nil {
			h.t.Fatalf("the second write was refused: %v", err)
		}
		// And now the first caller's edit, built on rev-1, arrives.
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-3", Payload: []byte(`{"v":3}`), At: h.now(),
			Expect: "rev-1", Origin: anOrigin})
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
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Summary: "second", Payload: []byte(`{"v":2}`),
			At: h.now(), Expect: "rev-1", Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("a matching expectation was refused: %v", err)
		}
		if got.RevisionID != "rev-2" {
			h.t.Fatalf("published %+v", got)
		}
	},
}, {
	// EXPECTING A REVISION WHEN NOTHING IS ACTIVE IS A RACE: the write
	// was built on a pointer that is gone, so it edits a company the fleet
	// no longer runs. Letting it land is how a node whose own copy named
	// a revision the fleet had dropped published that copy's successor
	// over the fleet's loss — every config write named the node's local
	// revision, whatever the fleet ran. A writer that read no pointer
	// passes ExpectAbsent, which the case below lands.
	name: "an activation expecting a revision lands only while the pointer names it",
	fn: func(h *fleetHarness) {
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-0", Origin: anOrigin})
		if !errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("err = %v, want coord.ErrActivationRaced over no pointer", err)
		}
		// AND IT LEFT NOTHING BEHIND: no pointer, and no body for one.
		if target, found, err := h.f.Target(h.ctx); err != nil || found {
			h.t.Fatalf("target = %+v found=%v err=%v, want no activation", target, found, err)
		}
		if _, ok, err := h.f.Payload(h.ctx, "rev-1"); err != nil || ok {
			h.t.Fatalf("payload ok=%v err=%v, want the refused body unpublished", ok, err)
		}
	},
}, {
	// AN EXPECTED EPOCH PINS THE VERY ACTIVATION THE WRITER READ. With the
	// revision alone, another writer re-activating it in between lets the
	// write land a millisecond after that one — at an instant the writer
	// never planned — and the boot publish of an offline offer, which
	// records the instant it will land at before it publishes, then reads
	// its own landed publish as never made. Pinned, the re-activation is the
	// race it is, and a write at the epoch it read lands at exactly
	// ActivationAt(its instant, that activation's).
	name: "an activation expecting an epoch lands only on that activation",
	fn: func(h *fleetHarness) {
		read, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		again, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-1", Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("re-activate: %v", err)
		}
		planned := coord.ActivationAt(h.now().Add(time.Minute), read.At)
		_, err = h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte("{}"), At: planned,
			Expect: "rev-1", ExpectEpoch: read.Epoch, Origin: anOrigin})
		if !errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("an epoch the pointer has moved past: err = %v, want coord.ErrActivationRaced", err)
		}
		if target, _, _ := h.f.Target(h.ctx); target.Epoch != again.Epoch {
			h.t.Fatalf("the refused write moved the pointer to %+v", target)
		}
		planned = coord.ActivationAt(h.now().Add(time.Minute), again.At)
		landed, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte("{}"), At: planned,
			Expect: "rev-1", ExpectEpoch: again.Epoch, Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("the epoch the pointer is at was refused: %v", err)
		}
		if !landed.At.Equal(planned) {
			h.t.Fatalf("landed at %s, want exactly the planned %s", landed.At, planned)
		}
		if _, err = h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-3", Payload: []byte("{}"), At: h.now(),
			ExpectEpoch: landed.Epoch, Origin: anOrigin}); err == nil ||
			errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("an epoch with no revision: err = %v, want a refusal that is no race", err)
		}
	},
}, {
	// A RESTORE SAYS SO, AND ONLY UNTIL THE NEXT WRITE. A peer that knows the
	// fleet moved on replaces a restored pointer and never a chosen one, so a
	// mark that outlived its write would let it replace the next choice, and
	// one the backend dropped would leave a stale restore standing.
	name: "a restored pointer says so until the next write",
	fn: func(h *fleetHarness) {
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			ExpectAbsent: true, Restore: true, Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("restore: %v", err)
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || !target.Restored || !published.Restored {
			h.t.Fatalf("restored pointer reads %+v (published %+v, found=%v err=%v), "+
				"want it marked restored", target, published, found, err)
		}
		if _, err = h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-1", Origin: anOrigin}); err != nil {
			h.t.Fatalf("choose: %v", err)
		}
		if target, _, _ = h.f.Target(h.ctx); target.Restored {
			h.t.Fatalf("a chosen pointer reads %+v, want the restore's mark gone", target)
		}
	},
}, {
	// AN EMPTY EXPECTATION IS UNCONDITIONAL, which is `crewlet run
	// -import-company`: an operator asserting a company outright at one
	// node's start rather than editing what it read. It has nothing to have
	// raced with, so a pointer that moved under it is replaced, not a
	// refusal. Every other writer — the boot publish included — read the
	// pointer, or its absence, and passes what it read.
	name: "an activation with no expectation overwrites whatever is there",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte("{}"), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("an unconditional activation was refused: %v", err)
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-2" {
			h.t.Fatalf("target = %+v found=%v err=%v", target, found, err)
		}
	},
}, {
	// A CREATE-ONLY ACTIVATION LANDS ON AN EMPTY STORE. It is what a first
	// import is: a company built on nothing, asking to be the first.
	name: "an activation expecting no activation lands on an empty store",
	fn: func(h *fleetHarness) {
		got, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(),
			ExpectAbsent: true, Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("a create-only activation on an empty store was refused: %v", err)
		}
		if got.Epoch == 0 || got.RevisionID != "rev-1" {
			h.t.Fatalf("published %+v", got)
		}
		body, ok, err := h.f.Payload(h.ctx, "rev-1")
		if err != nil || !ok || string(body) != `{"v":1}` {
			h.t.Fatalf("payload = %q ok=%v err=%v, want the created revision's", body, ok, err)
		}
	},
}, {
	// AND IT IS REFUSED WHEN THE FLEET HAS ONE, whatever this node's own
	// store says. A node that joined a fleet and has not reconciled yet
	// believes nothing is configured; without this, its first import
	// replaces a running company, renaming it and orphaning the memory of
	// every seat, and nothing refuses it.
	name: "an activation expecting no activation is refused when one exists",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "the-fleets", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "a-second-company", Payload: []byte(`{"v":2}`), At: h.now(),
			ExpectAbsent: true, Origin: anOrigin})
		if !errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("err = %v, want coord.ErrActivationRaced", err)
		}
		// AND IT CHANGED NOTHING: the pointer still names the company the
		// fleet is running, and its payload is still there for every peer
		// that has yet to apply it. A refusal that had already published
		// the loser's body leaves the fleet pointed at a revision no peer
		// can read.
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "the-fleets" {
			h.t.Fatalf("target = %+v found=%v err=%v, want the fleet's own", target, found, err)
		}
		body, ok, err := h.f.Payload(h.ctx, "the-fleets")
		if err != nil || !ok || string(body) != `{"v":1}` {
			h.t.Fatalf("payload = %q ok=%v err=%v, want the fleet's own", body, ok, err)
		}
	},
}, {
	// EXACTLY ONE OF N CONCURRENT CREATIONS LANDS. Two operators pressing
	// Create on two nodes at the same moment is the shape this is for, and
	// it cannot be read off the single-threaded cases: they prove the
	// comparison happens, this proves it is atomic.
	name: "concurrent create-only activations leave exactly one winner",
	fn: func(h *fleetHarness) {
		var wg sync.WaitGroup
		won := make(chan string, 8)
		raced := make(chan error, 8)
		for i := range 8 {
			wg.Go(func() {
				id := fmt.Sprintf("company-%d", i)
				_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
					RevisionID: id, Payload: []byte("{}"), At: h.now(),
					ExpectAbsent: true, Origin: anOrigin})
				switch {
				case err == nil:
					won <- id
				case errors.Is(err, coord.ErrActivationRaced):
					raced <- err
				default:
					h.t.Errorf("Activate: %v", err)
				}
			})
		}
		wg.Wait()
		close(won)
		close(raced)
		if got := len(won); got != 1 {
			h.t.Fatalf("%d of 8 creations were accepted, want exactly 1", got)
		}
		if got := len(raced); got != 7 {
			h.t.Fatalf("%d creations were told they lost, want 7", got)
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		if winner := <-won; target.RevisionID != winner {
			h.t.Fatalf("the fleet is on %s, want the creation that won, %s",
				target.RevisionID, winner)
		}
	},
}, {
	// A WRITE CANNOT HAVE BEEN BUILT ON A REVISION AND ON NOTHING AT ONCE,
	// so asking for both is refused as the programming error it is rather
	// than resolved in favour of one.
	name: "an activation expecting both a revision and none is refused",
	fn: func(h *fleetHarness) {
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-0", ExpectAbsent: true, Origin: anOrigin})
		if err == nil {
			h.t.Fatal("an activation expecting a revision and no revision was accepted")
		}
		if errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("err = %v, want a refusal of the request rather than a race", err)
		}
		if _, found, ferr := h.f.Target(h.ctx); ferr != nil || found {
			h.t.Fatalf("a refused request published an activation (found=%v err=%v)", found, ferr)
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
			RevisionID: "base", Payload: []byte("{}"), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		var wg sync.WaitGroup
		won := make(chan string, 8)
		raced := make(chan error, 8)
		for i := range 8 {
			wg.Go(func() {
				id := fmt.Sprintf("edit-%d", i)
				_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
					RevisionID: id, Payload: []byte("{}"), At: h.now(),
					Expect: "base", Origin: anOrigin})
				switch {
				case err == nil:
					won <- id
				case errors.Is(err, coord.ErrActivationRaced):
					raced <- err
				default:
					h.t.Errorf("Activate: %v", err)
				}
			})
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
			wg.Go(func() {
				got, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: fmt.Sprintf("rev-%d", i), Payload: []byte("{}"), At: h.now(), Origin: anOrigin})
				if err == nil {
					epochs <- got.Epoch
				}
			})
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
	// A LATER ACTIVATION CARRIES A LATER INSTANT, whatever the activating
	// node's clock said. The instant is how a node tells which of two
	// activations is newer — a booting node that remembers the fleet on a
	// revision activated later than a RESTORED pointer's publishes that one
	// over it, by compare-and-set — so an activation from a node whose clock
	// runs behind, or a republish of an older `activated_at`, left a node
	// holding the replaced revision reading its own copy as the newer. See
	// [coord.ActivationAt].
	name: "an activation no later than the one it replaces is published after it",
	fn: func(h *fleetHarness) {
		first := h.now()
		activate := func(req coord.ActivationRequest) coord.Activation {
			h.t.Helper()
			req.Payload = []byte("{}")
			got, err := h.f.Activate(h.ctx, req)
			if err != nil {
				h.t.Fatalf("Activate(%s): %v", req.RevisionID, err)
			}
			target, found, err := h.f.Target(h.ctx)
			if err != nil || !found || !target.At.Equal(got.At) {
				h.t.Fatalf("the pointer carries %+v (found=%v err=%v), and the "+
					"activation reported %s", target, found, err, got.At)
			}
			return got
		}
		if got := activate(coord.ActivationRequest{RevisionID: "rev-1", At: first, Origin: anOrigin}); !got.At.Equal(first) {
			h.t.Fatalf("the first activation is at %s, want the %s it asked for", got.At, first)
		}
		for _, c := range []struct {
			name string
			req  coord.ActivationRequest
		}{
			{"an unconditional one from a clock behind",
				coord.ActivationRequest{RevisionID: "rev-2", At: first.Add(-time.Hour), Origin: anOrigin}},
			{"an expecting one at the same instant",
				coord.ActivationRequest{RevisionID: "rev-3", At: first, Expect: "rev-2", Origin: anOrigin}},
			{"one later by less than the stamp's resolution",
				coord.ActivationRequest{RevisionID: "rev-4", At: first.Add(2*time.Millisecond + 400*time.Microsecond), Origin: anOrigin}},
		} {
			before, _, err := h.f.Target(h.ctx)
			if err != nil {
				h.t.Fatalf("Target: %v", err)
			}
			got := activate(c.req)
			if got.At.UnixMilli() <= before.At.UnixMilli() {
				h.t.Fatalf("%s is published at %s, no later in milliseconds than "+
					"the %s it replaced", c.name, got.At, before.At)
			}
		}
		// AND A GENUINELY LATER ONE IS PUBLISHED AS IT ASKED.
		later := first.Add(time.Minute)
		if got := activate(coord.ActivationRequest{RevisionID: "rev-5", At: later, Origin: anOrigin}); !got.At.Equal(later) {
			h.t.Fatalf("a later activation is at %s, want the %s it asked for", got.At, later)
		}
	},
}, {
	// THE INSTANT IS DECIDED INSIDE THE COMPARE-AND-SET, so two activations
	// racing each other are never both told one instant: whichever lands
	// second is compared with the first.
	name: "concurrent activations are handed increasing instants",
	fn: func(h *fleetHarness) {
		type landed struct {
			epoch int64
			at    time.Time
		}
		var wg sync.WaitGroup
		results := make(chan landed, 8)
		for i := range 8 {
			wg.Go(func() {
				got, err := h.f.Activate(h.ctx, coord.ActivationRequest{
					RevisionID: fmt.Sprintf("rev-%d", i), Payload: []byte("{}"), At: h.now(), Origin: anOrigin})
				if err != nil {
					h.t.Errorf("Activate: %v", err)
					return
				}
				results <- landed{got.Epoch, got.At}
			})
		}
		wg.Wait()
		close(results)
		var all []landed
		for r := range results {
			all = append(all, r)
		}
		if len(all) != 8 {
			h.t.Fatalf("%d of 8 unconditional activations landed", len(all))
		}
		slices.SortFunc(all, func(a, b landed) int { return cmp.Compare(a.epoch, b.epoch) })
		for i := 1; i < len(all); i++ {
			if all[i].at.UnixMilli() <= all[i-1].at.UnixMilli() {
				h.t.Fatalf("epoch %d is at %s, no later than epoch %d's %s",
					all[i].epoch, all[i].at, all[i-1].epoch, all[i-1].at)
			}
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
	// THE TIE-BREAK IS PART OF THE CONTRACT, and it was implemented twice
	// with nothing holding either to it. A rolling apply lands every node's
	// report inside the same second, so a fleet view ordered by timestamp
	// alone reshuffles between reads — and an operator watching a deploy
	// converge would see the list jump with no node having changed.
	//
	// Newest first, the node id breaking the tie ASCENDING.
	name: "one instant's reports are ordered by node id",
	fn: func(h *fleetHarness) {
		at := h.now()
		// Recorded out of order, so a backend that answered in write
		// order rather than sorting would pass a same-order assertion.
		for _, node := range []string{"node-c", "node-a", "node-b"} {
			if err := h.f.RecordApply(h.ctx, coord.NodeApply{
				NodeID: node, Epoch: 7, Status: "ok", UpdatedAt: at,
			}); err != nil {
				h.t.Fatalf("RecordApply(%s): %v", node, err)
			}
		}
		// One node reporting LATER must still sort ahead of all three,
		// so the tie-break never outranks the timestamp.
		if err := h.f.RecordApply(h.ctx, coord.NodeApply{
			NodeID: "node-z", Epoch: 7, Status: "ok", UpdatedAt: at.Add(time.Second),
		}); err != nil {
			h.t.Fatalf("RecordApply(node-z): %v", err)
		}

		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		ids := make([]string, len(got))
		for i, row := range got {
			ids[i] = row.NodeID
		}
		want := []string{"node-z", "node-a", "node-b", "node-c"}
		if !slices.Equal(ids, want) {
			h.t.Fatalf("fleet order = %v, want %v — freshest first, then by node id", ids, want)
		}
	},
}, {
	// WHAT A NODE RUNS IS NOT WHAT THE EPOCH ASKED FOR when it refused
	// that revision, and the fleet view is where an operator sees a node
	// still serving the previous company. A backend that dropped the
	// field reported every refusing node as running nothing.
	//
	// AND WHAT IT BOOTS ON IS A THIRD REVISION while a write to its boot
	// row is being retried: the node runs rev-6 and would come back on
	// rev-5, and a backend that dropped that field reported a node as
	// booting on nothing — which is what a reader keeping a revision's
	// credentials acts on.
	//
	// AND THE EPOCH IT LAST APPLIED IS NOT THE ONE IT ATTEMPTED: the node
	// refused 7 and runs what it applied at 6, and a backend that dropped
	// that field reported it as having applied nothing — which a writer
	// judging whether its save reached the node reads as "not yet" for good.
	name: "an apply record carries what its node applied, serves and boots on",
	fn: func(h *fleetHarness) {
		if err := h.f.RecordApply(h.ctx, coord.NodeApply{
			NodeID: "node-a", Epoch: 7, RevisionID: "rev-7", Applied: 6, Serving: "rev-6",
			Boots: "rev-5", Status: "failed", Error: "refused", UpdatedAt: h.now(),
		}); err != nil {
			h.t.Fatalf("RecordApply: %v", err)
		}
		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		if len(got) != 1 || got[0].RevisionID != "rev-7" || got[0].Applied != 6 ||
			got[0].Serving != "rev-6" || got[0].Boots != "rev-5" {
			h.t.Fatalf("fleet = %+v, want node-a asked for rev-7 at epoch 7, having "+
				"applied epoch 6, serving rev-6 and booting on rev-5", got)
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

func (h *fleetHarness) allChannels() []coord.Channel {
	h.t.Helper()
	got, err := h.f.AllChannels(h.ctx)
	if err != nil {
		h.t.Fatalf("AllChannels: %v", err)
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
	// TWO LISTINGS WITH OPPOSITE RULES, and the store owes both.
	//
	// The idle sweep must see ONLY the open ones — a closed channel
	// re-reported is a second close for one channel, which draws two
	// closes on a dashboard. A read surface must see BOTH, or the record
	// this store keeps until the purge horizon is one nothing can ever
	// show, and a company's closed-ask history is reachable only through
	// the event log, one event at a time.
	name: "every channel is listable, closed ones included",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c-b", "alice", "bob", at)
		h.openChannel("c-a", "alice", "carol", at)
		h.openChannel("c-c", "alice", "dave", at)
		if _, found := h.closeChannel("c-c", at.Add(time.Minute)); !found {
			h.t.Fatal("CloseChannel lost a channel")
		}

		got := channelIDs(h.allChannels())
		want := []string{"c-a", "c-b", "c-c"}
		if !slices.Equal(got, want) {
			h.t.Errorf("all channels = %v, want %v — in id order, like the "+
				"open listing, so two walks cannot order one bucket two ways",
				got, want)
		}
		// AND THE CLOSE IS ON THE ROW, which is what makes the retained
		// record worth keeping: a listing that lost the instant would
		// answer "this channel exists" and nothing a reader came for.
		for _, ch := range h.allChannels() {
			if ch.ID != "c-c" {
				continue
			}
			if ch.Open() || ch.ClosedAt.IsZero() {
				h.t.Errorf("c-c is listed as %+v, want it closed with its instant", ch)
			}
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
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Summary: "first", Payload: body, At: h.now(), Origin: anOrigin})
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
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Summary: "first", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-2", Summary: "second", Payload: []byte(`{"v":2}`), At: h.now(), Origin: anOrigin}); err != nil {
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
	// THE BODY COMES BACK AS IT WENT IN. It is a sealed envelope, and what
	// a peer adopts into its own history is what it read here, so a store
	// that re-spelled it would hand every node a different copy of one
	// revision. The real backend embeds it inside a JSON record, where
	// encoding/json's default is to escape `<`, `>` and `&` — which a
	// ciphertext never holds and a document may — so the case carries all
	// three beside an envelope-shaped value.
	name: "a sealed body round-trips byte for byte",
	fn: func(h *fleetHarness) {
		body := []byte(`{"__encrypted__":"enc:v1:2026-01:q8+/Zm9vYmFy==","note":"a<b && c>d"}`)
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: body, At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Payload(h.ctx, "rev-1")
		if err != nil || !found {
			h.t.Fatalf("Payload: found=%v err=%v", found, err)
		}
		if string(got) != string(body) {
			h.t.Errorf("payload = %s, want the bytes activated, %s", got, body)
		}
	},
}, {
	// ONE FORM ON BOTH BACKENDS: the broker's record compacts what it
	// embeds, so the twin must too, or a case certified on the twin
	// certifies bytes the broker never hands back.
	name: "a body is served in its compact form",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("  {\n  \"name\" : \"Acme\",\n  \"agents\": { }\n}\n"),
			At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Payload(h.ctx, "rev-1")
		if err != nil || !found {
			h.t.Fatalf("Payload: found=%v err=%v", found, err)
		}
		if want := `{"name":"Acme","agents":{}}`; string(got) != want {
			h.t.Errorf("payload = %q, want %q", got, want)
		}
	},
}, {
	// A BODY NO NODE COULD DECODE IS NOT PUBLISHED. It is a company
	// document, sealed or not, so anything but a JSON object is a caller's
	// mistake — and a pointer naming it is a fleet that converges on
	// nothing. Refused before anything is written: the fleet stays where
	// it was.
	name: "a payload that is not a document is refused and moves nothing",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-good", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		for name, payload := range map[string][]byte{
			"nothing":     nil,
			"empty":       []byte(""),
			"not json":    []byte("name: Acme"),
			"truncated":   []byte(`{"name":`),
			"a string":    []byte(`"enc:v1:k:abc"`),
			"an array":    []byte(`[1,2]`),
			"json null":   []byte("null"),
			"blank space": []byte("   "),
		} {
			_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
				RevisionID: "rev-" + name, Payload: payload, At: h.now(), Origin: anOrigin})
			if !errors.Is(err, coord.ErrPayloadNotDocument) {
				h.t.Errorf("%s: Activate = %v, want ErrPayloadNotDocument", name, err)
			}
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-good" {
			h.t.Fatalf("a refused payload moved the pointer: %+v found=%v err=%v",
				target, found, err)
		}
		if got, found, err := h.f.Payload(h.ctx, "rev-good"); err != nil || !found ||
			string(got) != `{"v":1}` {
			h.t.Errorf("a refused payload disturbed the body: %q found=%v err=%v",
				got, found, err)
		}
	},
}, {
	// A caller mutating what it read must not reach the store: the body is
	// handed to a decoder that unseals in place on some paths.
	name: "a caller mutating a payload cannot reach the store",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
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

// payloadRaceCases hold the body and the pointer to ONE decision under
// concurrent writers.
var payloadRaceCases = []fleetCase{{
	// THE BODY AND THE POINTER CANNOT COME APART. They were two keys written
	// in two steps — the body, then the flip — so two nodes activating at
	// once could each write a body, and the one whose flip landed FIRST was
	// then pointed at by a pointer whose body the other had overwritten.
	// Every peer read "no such revision" for the epoch the fleet was on
	// until somebody activated again. Unconditional writers do exactly
	// this — two nodes started with -import-company each assert their file —
	// and an edit's compare-and-set only decided the pointer: the loser's
	// body had already landed.
	name: "racing activations leave the pointer's own body readable",
	fn: func(h *fleetHarness) {
		const writers = 8
		for round := range 6 {
			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					id := fmt.Sprintf("rev-%d-%d", round, w)
					req := coord.ActivationRequest{
						RevisionID: id, At: h.now(), Origin: anOrigin,
						Payload: []byte(fmt.Sprintf(`{"revision":%q}`, id)),
					}
					// Half race unconditionally, as two overriding nodes
					// do; half as editors naming what they read, where all
					// but one lose the compare-and-set.
					if w%2 == 1 && round > 0 {
						req.Expect = fmt.Sprintf("rev-%d-0", round-1)
					}
					_, err := h.f.Activate(h.ctx, req)
					if err != nil && !errors.Is(err, coord.ErrActivationRaced) {
						h.t.Errorf("Activate(%s): %v", id, err)
					}
				}()
			}
			wg.Wait()
			target, found, err := h.f.Target(h.ctx)
			if err != nil || !found {
				h.t.Fatalf("round %d: Target: %v found=%v", round, err, found)
			}
			body, found, err := h.f.Payload(h.ctx, target.RevisionID)
			if err != nil {
				h.t.Fatalf("round %d: Payload: %v", round, err)
			}
			if !found {
				h.t.Fatalf("round %d: the fleet points at %s and its body is "+
					"nowhere — every peer would fail to converge", round,
					target.RevisionID)
			}
			if want := fmt.Sprintf(`{"revision":%q}`, target.RevisionID); string(body) != want {
				h.t.Fatalf("round %d: the body of %s is %s", round, target.RevisionID, body)
			}
			// The next round's editors expect the round's first writer;
			// point the fleet there so half of them can win.
			if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
				RevisionID: fmt.Sprintf("rev-%d-0", round), At: h.now(), Origin: anOrigin,
				Payload: []byte(fmt.Sprintf(`{"revision":"rev-%d-0"}`, round)),
			}); err != nil {
				h.t.Fatalf("round %d: re-point: %v", round, err)
			}
		}
	},
}}

// ---- the sealed credentials -------------------------------------------- //

func (h *fleetHarness) putSecret(name, sealed, keyID string) {
	h.t.Helper()
	err := h.f.PutSecret(h.ctx, coord.SecretRecord{
		Name: name, Value: sealed, KeyID: keyID,
		UpdatedAt: h.now(), UpdatedBy: "jane.doe", UpdatedByKind: "operator",
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a", Source: "cli",
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
		// ALL FOUR, and the credential beside the author above all: a
		// store that kept only one of the two names lost whichever it
		// dropped — the author once the credential's row was swept, or
		// the credential for good.
		if rec.UpdatedBy != "jane.doe" || rec.UpdatedByKind != "operator" ||
			rec.OperatorID != "pat:0192f00d-0000-7000-8000-00000000000a" ||
			rec.Source != "cli" {
			h.t.Errorf("provenance lost: by=%q kind=%q operator=%q source=%q",
				rec.UpdatedBy, rec.UpdatedByKind, rec.OperatorID, rec.Source)
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
}, {
	// A CREATE IS THE FIRST WRITER'S. Two nodes minting the company's
	// blind-index key at once each derive blinds under the key they wrote,
	// and with a plain put the store keeps one of them — so the loser's
	// blinds match nothing any other node computes. The second create must
	// be told the row is somebody else's, and must not have touched it.
	name: "a create writes only where no row is stored",
	fn: func(h *fleetHarness) {
		created, err := h.f.CreateSecret(h.ctx, coord.SecretRecord{
			Name: "iam/blind-index-key", Value: "v1:first", KeyID: "key-1",
			UpdatedAt: h.now(),
		})
		if err != nil || !created {
			h.t.Fatalf("the first create = (%v, %v), want it written", created, err)
		}
		created, err = h.f.CreateSecret(h.ctx, coord.SecretRecord{
			Name: "iam/blind-index-key", Value: "v1:second", KeyID: "key-1",
			UpdatedAt: h.now(),
		})
		if err != nil || created {
			h.t.Fatalf("the second create = (%v, %v), want it refused as "+
				"somebody else's row", created, err)
		}
		if rec, _ := h.secret("iam/blind-index-key"); rec.Value != "v1:first" {
			h.t.Fatalf("a refused create replaced the row: %q", rec.Value)
		}
		// AND A DELETED ROW IS ABSENT AGAIN: a create is not refused over
		// the history of a key somebody destroyed.
		if _, err = h.f.DeleteSecret(h.ctx, "iam/blind-index-key"); err != nil {
			h.t.Fatalf("DeleteSecret: %v", err)
		}
		created, err = h.f.CreateSecret(h.ctx, coord.SecretRecord{
			Name: "iam/blind-index-key", Value: "v1:third", KeyID: "key-1",
			UpdatedAt: h.now(),
		})
		if err != nil || !created {
			h.t.Fatalf("a create after the row was deleted = (%v, %v), want "+
				"it written", created, err)
		}
	},
}, {
	// A CREATE'S OPERATION KEY IS KEPT, AND A PLAIN WRITE CLEARS IT. The key
	// is how the same create retried finishes what it sealed (fleetsecrets
	// Store.Reclaim); a store that dropped it would refuse every retry, and
	// one that kept it across a plain write would let a stale retry re-seal
	// over a value somebody else wrote.
	name: "a create's operation key round-trips and a plain write clears it",
	fn: func(h *fleetHarness) {
		created, err := h.f.CreateSecret(h.ctx, coord.SecretRecord{
			Name: "GATEWAY_KEY", Value: "v1:first", KeyID: "key-1",
			UpdatedAt: h.now(), OpID: "op-1",
		})
		if err != nil || !created {
			h.t.Fatalf("the keyed create = (%v, %v)", created, err)
		}
		rec, _ := h.secret("GATEWAY_KEY")
		if rec.OpID != "op-1" {
			h.t.Fatalf("the create's operation key reads back as %q", rec.OpID)
		}
		h.putSecret("GATEWAY_KEY", "v1:rotated", "key-1")
		if rec, _ = h.secret("GATEWAY_KEY"); rec.OpID != "" {
			h.t.Fatalf("a plain write left the operation key %q", rec.OpID)
		}
	},
}, {
	// A VERSION IS READ, AND EVERY WRITE MOVES IT. What a conditional write
	// is conditioned on has to be on what a read hands back, and a write
	// that left it where it was would let a caller holding the old one win
	// against a row it never saw.
	name: "every read carries the row's version and every write moves it",
	fn: func(h *fleetHarness) {
		h.putSecret("GITLAB_TOKEN", "v1:old", "key-1")
		first, _ := h.secret("GITLAB_TOKEN")
		if first.Version == 0 {
			h.t.Fatal("a stored row reads back with no version, so nothing " +
				"can be conditioned on it")
		}
		h.putSecret("GITLAB_TOKEN", "v2:new", "key-1")
		second, _ := h.secret("GITLAB_TOKEN")
		if second.Version == first.Version {
			h.t.Fatalf("a rotation left the version at %d", first.Version)
		}
		rows, err := h.f.SecretValues(h.ctx)
		if err != nil || len(rows) != 1 || rows[0].Version != second.Version {
			h.t.Fatalf("the listing = (%+v, %v), want the row at version %d",
				rows, err, second.Version)
		}
	},
}, {
	// AN UPDATE IS REFUSED OVER A ROW THAT MOVED, AND NEVER RESURRECTS ONE.
	// A rekey re-seals the value it READ: written over an operator's
	// rotation it silently reinstates the credential they replaced, and
	// written after an operator deleted a credential it brings it back.
	// Both are an update at a version the row no longer holds.
	name: "an update writes only while the row is at the version read",
	fn: func(h *fleetHarness) {
		h.putSecret("GITLAB_TOKEN", "v1:old", "key-1")
		read, _ := h.secret("GITLAB_TOKEN")
		h.putSecret("GITLAB_TOKEN", "v2:rotated", "key-1")
		wrote, err := h.f.UpdateSecret(h.ctx, coord.SecretRecord{
			Name: "GITLAB_TOKEN", Value: "v3:resealed-old", KeyID: "key-2",
			UpdatedAt: h.now(),
		}, read.Version)
		if err != nil || wrote {
			h.t.Fatalf("an update at a version the row moved past = (%v, %v), "+
				"want it refused", wrote, err)
		}
		if rec, _ := h.secret("GITLAB_TOKEN"); rec.Value != "v2:rotated" {
			h.t.Fatalf("the refused update replaced the rotation: %q", rec.Value)
		}
		current, _ := h.secret("GITLAB_TOKEN")
		wrote, err = h.f.UpdateSecret(h.ctx, coord.SecretRecord{
			Name: "GITLAB_TOKEN", Value: "v3:resealed", KeyID: "key-2",
			UpdatedAt: h.now(),
		}, current.Version)
		if err != nil || !wrote {
			h.t.Fatalf("an update at the current version = (%v, %v), want it "+
				"written", wrote, err)
		}
		// GONE IS NOT A VERSION.
		gone, _ := h.secret("GITLAB_TOKEN")
		if _, err = h.f.DeleteSecret(h.ctx, "GITLAB_TOKEN"); err != nil {
			h.t.Fatalf("DeleteSecret: %v", err)
		}
		wrote, err = h.f.UpdateSecret(h.ctx, coord.SecretRecord{
			Name: "GITLAB_TOKEN", Value: "v4:resurrected", KeyID: "key-2",
			UpdatedAt: h.now(),
		}, gone.Version)
		if err != nil || wrote {
			h.t.Fatalf("an update of a deleted row = (%v, %v), want it refused", wrote, err)
		}
		if _, found := h.secret("GITLAB_TOKEN"); found {
			h.t.Fatal("an update brought a deleted row back")
		}
		// NOR IS ZERO: a caller that never read one has judged nothing.
		if wrote, err := h.f.UpdateSecret(h.ctx, coord.SecretRecord{
			Name: "GITLAB_TOKEN", Value: "v5:blind", KeyID: "key-2",
			UpdatedAt: h.now(),
		}, 0); err != nil || wrote {
			h.t.Fatalf("an update at version zero = (%v, %v), want nothing written",
				wrote, err)
		}
		if _, found := h.secret("GITLAB_TOKEN"); found {
			h.t.Fatal("an update at version zero created the row")
		}
	},
}}

// ---- the integration reconcile status ---------------------------------- //

func (h *fleetHarness) putIntegration(kind, value string) {
	h.t.Helper()
	if err := h.f.PutIntegrationStatus(h.ctx, kind, []byte(value)); err != nil {
		h.t.Fatalf("PutIntegrationStatus(%q): %v", kind, err)
	}
}

func (h *fleetHarness) integrations() map[string][]byte {
	h.t.Helper()
	got, err := h.f.IntegrationStatuses(h.ctx)
	if err != nil {
		h.t.Fatalf("IntegrationStatuses: %v", err)
	}
	return got
}

func (h *fleetHarness) integrationNames() []string {
	h.t.Helper()
	names := slices.Collect(maps.Keys(h.integrations()))
	slices.Sort(names)
	return names
}

var integrationCases = []fleetCase{
	{name: "a recorded status reads back byte for byte", fn: func(h *fleetHarness) {
		// The value is OPAQUE to this store: internal/integration
		// marshals a phase, an actor, a sentence and a findings list into
		// it, and a backend that re-encoded on the way through would
		// hand the reader a document its producer never wrote.
		const status = `{"kind":"gitlab","report":{"phase":"degraded","actor":"admin"}}`
		h.putIntegration("gitlab", status)

		got := h.integrations()
		if string(got["gitlab"]) != status {
			t := h.t
			t.Fatalf("read back %q, want %q", got["gitlab"], status)
		}
	}},

	{name: "a surface with no status is absent rather than empty", fn: func(h *fleetHarness) {
		// The loop reads a missing key as "never reconciled, so due now".
		// A backend that answered with a zero-length value instead would
		// make that surface parse as a status whose phase is the empty
		// string, which is not a phase.
		h.putIntegration("slack", `{"kind":"slack"}`)
		if _, present := h.integrations()["gitlab"]; present {
			h.t.Fatal("a surface nothing recorded came back present")
		}
	}},

	{name: "the last write wins", fn: func(h *fleetHarness) {
		// No compare-and-set, deliberately — but NOT because the worker
		// duty makes one node the only writer, which is the reason this
		// used to give and is not true: the dashboard records a pass's
		// outcome, stamps an endpoint and marks a disconnect on whichever
		// node served the request. What makes last-write-wins safe is the
		// SURFACE's own provisioning lease, held by every writer across
		// the read, the work and the write alike. See [coord.Integrations].
		h.putIntegration("gitlab", `{"attempts":1}`)
		h.putIntegration("gitlab", `{"attempts":2}`)
		if got := string(h.integrations()["gitlab"]); got != `{"attempts":2}` {
			h.t.Fatalf("read back %q, want the second write", got)
		}
	}},

	{name: "every recorded surface is listed", fn: func(h *fleetHarness) {
		h.putIntegration("gitlab", `{}`)
		h.putIntegration("slack", `{}`)
		h.putIntegration("datadog", `{}`)
		want := []string{"datadog", "gitlab", "slack"}
		if got := h.integrationNames(); !slices.Equal(got, want) {
			h.t.Fatalf("listed %v, want %v", got, want)
		}
	}},

	{name: "a deleted status is gone and the rest survive", fn: func(h *fleetHarness) {
		h.putIntegration("gitlab", `{}`)
		h.putIntegration("slack", `{}`)
		if err := h.f.DeleteIntegrationStatus(h.ctx, "gitlab"); err != nil {
			h.t.Fatalf("DeleteIntegrationStatus: %v", err)
		}
		want := []string{"slack"}
		if got := h.integrationNames(); !slices.Equal(got, want) {
			h.t.Fatalf("listed %v, want %v", got, want)
		}
	}},

	{name: "deleting a status that is not there is the outcome asked for", fn: func(h *fleetHarness) {
		// The loop forgets every surface the company no longer declares,
		// on every tick, so this is the common path rather than an edge
		// case. Raising here would fill an operator's log with warnings
		// about work that was already done.
		if err := h.f.DeleteIntegrationStatus(h.ctx, "never-recorded"); err != nil {
			h.t.Fatalf("deleting an absent status: %v", err)
		}
	}},

	{name: "a surface name that is not a bare word survives the round trip", fn: func(h *fleetHarness) {
		// The KV backend escapes a key on the way in and unescapes it on
		// the way out, and the mapping has to be injective in both
		// directions. Nothing in integration.Kinds needs escaping today,
		// which is exactly why a backend that got this wrong would go
		// unnoticed until the first surface that did.
		const kind = "self-hosted:gitlab.example"
		h.putIntegration(kind, `{"kind":"gitlab"}`)
		if _, present := h.integrations()[kind]; !present {
			h.t.Fatalf("listed %v, want it to contain %q", h.integrationNames(), kind)
		}
	}},

	{name: "a status the caller mutates afterwards does not change the store", fn: func(h *fleetHarness) {
		// A caller reusing its marshalling buffer is ordinary, and a
		// backend that kept the caller's slice would have the next
		// reader see whatever that buffer holds by then.
		value := []byte(`{"attempts":1}`)
		if err := h.f.PutIntegrationStatus(h.ctx, "gitlab", value); err != nil {
			h.t.Fatalf("PutIntegrationStatus: %v", err)
		}
		copy(value, []byte(`{"attempts":9}`))
		if got := string(h.integrations()["gitlab"]); got != `{"attempts":1}` {
			h.t.Fatalf("read back %q after the caller reused its buffer", got)
		}
	}},
}

// ---- the chat thread-follows -------------------------------------------- //

// follow records reason whatever is held: the store ranks nothing, so the
// cases here are about what it does with a choice, not about which is right.
func (h *fleetHarness) follow(backend, handle, channel, thread, reason string) {
	h.t.Helper()
	got, err := h.f.Follow(h.ctx, backend, handle, channel, thread, h.now(), overwrite(reason))
	if err != nil {
		h.t.Fatalf("Follow(%s/%s/%s): %v", backend, handle, thread, err)
	}
	if got != reason {
		h.t.Fatalf("Follow(%s/%s/%s) answered %q, want the %q it wrote", backend, handle, thread, got, reason)
	}
}

// overwrite is the choice that writes reason whatever the record holds.
func overwrite(reason string) func(string, bool) string {
	return func(string, bool) string { return reason }
}

func (h *fleetHarness) following(backend, handle, channel, thread string) (string, bool) {
	h.t.Helper()
	reason, ok, err := h.f.Following(h.ctx, backend, handle, channel, thread)
	if err != nil {
		h.t.Fatalf("Following(%s/%s/%s): %v", backend, handle, thread, err)
	}
	return reason, ok
}

var followCases = []fleetCase{{
	// THE REASON THIS IS COMPANY-WIDE. An inbound chat message is claimed
	// and parsed by ONE node of the fleet, and the next reply in the same
	// thread by whichever node wins that time — so a follow only one node
	// can see is a thread reply that reaches its seat by chance.
	name: "a follow one caller records is visible to every other",
	fn: func(h *fleetHarness) {
		h.follow("mattermost", "agent-swe", "town-square", "root-1", "mention")
		reason, ok := h.following("mattermost", "agent-swe", "town-square", "root-1")
		if !ok {
			h.t.Fatal("the follow is not readable, so a reply on another node is dropped")
		}
		if reason != "mention" {
			h.t.Fatalf("reason = %q, want the one that was recorded", reason)
		}
	},
}, {
	// A re-assert writes what the CALLER chooses, and the caller chooses
	// against what is held: a seat first pulled into a thread by a
	// collective shout and later named personally follows for the mention,
	// and one named and then shouted at keeps the mention — a ranking the
	// store does not know and must not need to.
	name: "a re-assert writes the reason the caller chose against the record",
	fn: func(h *fleetHarness) {
		var shown []string
		record := func(reason string) func(string, bool) string {
			return func(held string, following bool) string {
				shown = append(shown, fmt.Sprintf("%s/%v", held, following))
				return reason
			}
		}
		at := h.now()
		for _, reason := range []string{"collective", "mention"} {
			got, err := h.f.Follow(h.ctx, "slack", "agent-pm", "C1", "1700.1", at, record(reason))
			if err != nil {
				h.t.Fatalf("Follow: %v", err)
			}
			if got != reason {
				h.t.Fatalf("Follow answered %q, want the %q chosen", got, reason)
			}
		}
		if want := []string{"/false", "collective/true"}; !slices.Equal(shown, want) {
			h.t.Fatalf("the choice was shown %v, want %v: a fresh thread holds nothing and "+
				"a re-assert is shown what the first wrote", shown, want)
		}
		if reason, _ := h.following("slack", "agent-pm", "C1", "1700.1"); reason != "mention" {
			h.t.Fatalf("reason = %q, want the chosen one", reason)
		}
	},
}, {
	// THE CHOICE IS MADE AGAINST THE RECORD THE WRITE LANDS ON. A choice
	// formed from an earlier read and written blind lands over whatever
	// another node wrote in between, so the reason ends up being the last
	// writer's rather than the one the caller would have kept — and the
	// reason decides whether a reply obliges an answer. Here one node
	// names the seat while seven others shout in the same thread, every
	// one of them keeping a mention it is shown; the mention must survive.
	name: "concurrent re-asserts never write over a reason their choice would keep",
	fn: func(h *fleetHarness) {
		keep := func(asserted string) func(string, bool) string {
			return func(held string, following bool) string {
				if following && held == "mention" {
					return held
				}
				return asserted
			}
		}
		at := h.now()
		var (
			wg    sync.WaitGroup
			once  sync.Once
			named = make(chan struct{})
		)
		name := func() { once.Do(func() { close(named) }) }
		for i, node := range h.nodes(8) {
			wg.Go(func() {
				if i == 0 {
					<-named
					if _, err := node.Follow(h.ctx, "slack", "agent-swe", "C1", "busy", at, keep("mention")); err != nil {
						h.t.Errorf("Follow(mention): %v", err)
					}
					return
				}
				// The mention goes in while the shouting is under way,
				// and goes in whatever happens to this writer.
				defer name()
				for n := range 10 {
					if i == 1 && n == 2 {
						name()
					}
					if _, err := node.Follow(h.ctx, "slack", "agent-swe", "C1", "busy", at, keep("collective")); err != nil {
						h.t.Errorf("Follow(collective): %v", err)
						return
					}
				}
			})
		}
		wg.Wait()
		if reason, _ := h.following("slack", "agent-swe", "C1", "busy"); reason != "mention" {
			h.t.Fatalf("reason = %q after a mention raced seven broadcasts, want the mention: "+
				"a broadcast decided before the mention was written over it", reason)
		}
	},
}, {
	// Every segment is part of the identity. Two backends' thread ids come
	// from different namespaces and are not comparable; two seats follow
	// for themselves; and one thread id under two channels is two threads.
	name: "the backend, the seat and the channel are all part of the identity",
	fn: func(h *fleetHarness) {
		h.follow("slack", "agent-swe", "C1", "t-1", "mention")

		if _, ok := h.following("mattermost", "agent-swe", "C1", "t-1"); ok {
			h.t.Error("another backend's thread id read as the same thread")
		}
		if _, ok := h.following("slack", "agent-pm", "C1", "t-1"); ok {
			h.t.Error("one seat's follow answered for another seat")
		}
		if _, ok := h.following("slack", "agent-swe", "C2", "t-1"); ok {
			h.t.Error("the same thread id under another channel read as the same thread")
		}
	},
}, {
	// A KEY IS A SUBJECT TOKEN PATH, and a chat backend's ids are not this
	// engine's to promise about. A separator inside a segment must not be
	// able to compose the key some other segmentation would.
	name: "a separator inside a segment does not collide with another key",
	fn: func(h *fleetHarness) {
		h.follow("slack", "agent-swe", "C1.C2", "t-1", "mention")
		if _, ok := h.following("slack", "agent-swe", "C1", "C2.t-1"); ok {
			h.t.Fatal("two different (channel, thread) pairs composed one key, " +
				"so one seat's follow answers for a thread it never joined")
		}
	},
}, {
	// AN INCOMPLETE IDENTITY IS REFUSED rather than written under a key
	// missing a segment, which would be a record no read could ever find.
	name: "an incomplete identity is refused",
	fn: func(h *fleetHarness) {
		at := h.now()
		for _, missing := range []struct{ backend, handle, thread string }{
			{"", "agent-swe", "t-1"},
			{"slack", "", "t-1"},
			{"slack", "agent-swe", ""},
		} {
			_, err := h.f.Follow(h.ctx, missing.backend, missing.handle, "C1", missing.thread, at, overwrite("mention"))
			if err == nil {
				h.t.Errorf("a follow with backend=%q handle=%q thread=%q was accepted",
					missing.backend, missing.handle, missing.thread)
			}
		}
	},
}, {
	// A read with an incomplete identity is NOT an error: it is the
	// ordinary shape of a top-level message, where there is no thread.
	name: "reading with no thread reports not following rather than failing",
	fn: func(h *fleetHarness) {
		reason, ok, err := h.f.Following(h.ctx, "slack", "agent-swe", "C1", "")
		if err != nil {
			h.t.Fatalf("Following with no thread: %v", err)
		}
		if ok || reason != "" {
			h.t.Fatal("a message with no thread read as a followed thread")
		}
	},
}}
