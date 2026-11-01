package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("coord")

// A LEASE VIEW: one class of live leases, WATCHED — listed on a cadence and
// on demand, told of every claim and release as it lands, and read from
// memory by every caller in between.
//
// # Why a view, and not a listing per question
//
// A listing is an O(fleet) read of the coordination store, and on a node
// without the `data` role it crosses the leaf link as well. Asked per request
// — which data nodes may this tool call go to, which nodes divide this search
// — it was one such read per tool call and per keystroke of search, for an
// answer that only changes when a node joins, leaves or lets its lease lapse.
// A view reads it once per heartbeat and answers every caller from memory.
//
// # What it answers, and when it answers UNKNOWN
//
// [LeaseView.Leases] is THREE-VALUED, on this package's rule that an
// unreachable store is never read as an empty one: the leases as of the last
// listing, or an error wrapping [ErrUnavailable] — never an empty or a stale
// list standing in for "could not say". It is unknown when the view has never
// listed, and when its last successful listing is older than the TRUST it was
// built with: a lease is renewed within its TTL or it lapses, so a listing a
// TTL old may name a node that is gone and miss one that has joined, and past
// that a stale roster is a guess rather than an answer.
//
// # Why it watches as well as lists
//
// A roster listed once a heartbeat learns of a node that JOINED up to a
// heartbeat late, and so did every view of it: a node joining was missing from
// every peer's routing for up to fifteen seconds, and a fleet booted at once
// — every node's first listing taken before any presence lease, its own
// included, existed — routed for its first heartbeat as though each node were
// alone. A node holding the estate committed every push it received itself
// rather than meeting its peers at the repository's one committer, and a node
// without it had no data node to ask and refused every push, fetch and read.
// A shorter cadence only shrinks that window by listing the whole class more
// often, which is the cost the view exists to remove.
//
// So the view WATCHES its class ([Backend.WatchLive]) for as long as it runs,
// and a write that disagrees with what it holds — a holder it does not name, a
// tenure it names under another owner or epoch, a release of a lease it names
// — asks for a listing at once, held to [MinViewRefresh] like an invalidation.
// A renewal agrees with the listing and asks for nothing, so the watch costs a
// node the class's write rate (for presence, one message per node per
// heartbeat) and a listing only when the roster moved. A join therefore
// reaches every view within about a second of its claim, and a clean
// departure as fast.
//
// THE WATCH IS A HINT AND THE LISTING IS STILL THE ANSWER: a write is held
// against the last listing and never applied to it, because what a listing
// holds is the store's judgement of what is live (a backend decides liveness
// on its own clock) and a write is one message about one key. What the watch
// cannot say is what the cadence is still for: a lease that LAPSES is no write
// at all, so a node that stopped without releasing leaves the roster at the
// first listing past its TTL, as it always did; a payload that moves with no
// change of holder (a renewal's fresh profile) is read at the next listing; and
// while the watch is down the view is exactly the listed view it was before.
//
// The watch begins BEFORE the first listing, so nothing written between the
// two goes unheard: what landed before the watch is in the listing, and what
// landed after is delivered. A watch that ends is begun again — after
// [MinViewRefresh], doubling to the view's cadence, since past that a listing
// would have caught what it missed anyway — and asks for a listing as it
// resumes, for whatever was written while nobody was listening.
//
// # Invalidation
//
// A caller that asked a node the view named and got nothing back says so
// ([LeaseView.Invalidate]), and the view lists again rather than waiting out
// its cadence — a node that released its lease on a clean stop is gone from
// the next answer. It is a REQUEST, not a wait: the view goes on answering
// from the listing it has, which is still within its trust, and the caller
// already knows the node it asked is suspect. Repeated invalidations are
// coalesced and held to [MinViewRefresh] apart, so a node that stays silent
// cannot turn the view back into a listing per request.
//
// # What it is kept small for
//
// It holds the LEASES, not a projection of them, so a caller that needs more
// than an id — a node's profile, what it advertises — reads it off the same
// answer rather than asking the store again.

// MinViewRefresh is the shortest interval between two listings a view makes
// because a caller invalidated it or its watch heard the roster move.
//
// ONE SECOND: long enough that a node which never answers costs at most one
// listing a second however many requests fail against it, and that a fleet
// booting at once — every node's claim a write each view hears — costs each
// view a listing a second while it boots rather than one per claim; and short
// enough that a lease released on a clean stop leaves the roster before a
// caller has retried more than once or twice.
const MinViewRefresh = time.Second

// ViewOptions configure a [LeaseView].
type ViewOptions struct {
	// Every is the cadence it lists at. Required: it is the heartbeat of
	// the leases it watches, whatever that is on this deployment.
	Every time.Duration

	// Trust is how old a listing may be and still be an answer. Required,
	// and at least Every: it is the TTL of the leases it watches.
	Trust time.Duration

	// Now is the clock, for tests. Nil is the wall clock.
	Now func() time.Time
}

// ViewStore is what a view asks of the store: a listing of its class, and a
// watch of the writes to it. Every [Backend] is one.
type ViewStore interface {
	ListLive(ctx context.Context, class Class) ([]Lease, error)
	WatchLive(ctx context.Context, class Class) (<-chan LeaseWrite, error)
}

// LeaseView watches one class of leases. Build it with [NewLeaseView] and run
// it with [LeaseView.Run]; its lifetime is the Run's.
type LeaseView struct {
	store ViewStore
	class Class
	every time.Duration
	trust time.Duration
	now   func() time.Time

	// kick asks the loop for a listing now; capacity one, so any number
	// of invalidations between two listings is one listing.
	kick chan struct{}

	// listed is closed by the first listing that answers ([LeaseView.Listed]).
	listed chan struct{}

	mu       sync.Mutex
	leases   []Lease
	loaded   bool
	listedAt time.Time
	lastErr  error
	// held is the last listing by resource, for judging a watched write
	// against it without a walk of the roster per message.
	held map[string]Lease
}

// NewLeaseView builds a view of one class. It lists nothing until it runs.
func NewLeaseView(store ViewStore, class Class, opts ViewOptions) (*LeaseView, error) {
	switch {
	case store == nil:
		return nil, errors.New("coord: a lease view needs a store to list")
	case !class.Valid():
		return nil, fmt.Errorf("coord: a lease view needs a class, and %q is not one", class)
	case opts.Every <= 0:
		return nil, errors.New("coord: a lease view needs the cadence it lists at")
	case opts.Trust < opts.Every:
		return nil, fmt.Errorf("coord: a lease view trusts a listing for %v and "+
			"lists every %v — it would answer unknown between every two "+
			"listings", opts.Trust, opts.Every)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &LeaseView{
		store: store, class: class, every: opts.Every, trust: opts.Trust,
		now: now, kick: make(chan struct{}, 1), listed: make(chan struct{}),
	}, nil
}

// Run watches the class, lists it once its watch has begun, and then lists on
// the view's cadence, on every invalidation and whenever the watch hears the
// roster move, until ctx ends. It returns ctx's error, and nothing else ends
// it: a listing that fails is recorded, and the view answers unknown once its
// last good one is older than its trust; a watch that fails is begun again,
// and the view lists on its cadence meanwhile.
func (v *LeaseView) Run(ctx context.Context) error {
	watching, stopWatching := context.WithCancel(ctx)
	began := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		v.watch(watching, began)
	}()
	defer func() {
		stopWatching()
		<-watched
	}()
	// THE WATCH FIRST, so the first listing answers for everything written
	// before the watch could hear it. Its first attempt is bounded by the
	// store's own request budget, and one that fails lets the listing go
	// ahead: the view is then the listed view it would have been anyway.
	select {
	case <-began:
	case <-ctx.Done():
		return ctx.Err()
	}

	ticker := time.NewTicker(v.every)
	defer ticker.Stop()
	var last time.Time
	for {
		v.refresh(ctx)
		last = v.now()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-v.kick:
			// HELD APART, so a node that never answers costs one
			// listing per MinViewRefresh however many requests fail.
			if wait := MinViewRefresh - v.now().Sub(last); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			// WHAT ARRIVED DURING THE WAIT IS ANSWERED BY THE LISTING
			// ABOUT TO BE TAKEN, so it is not a second one.
			select {
			case <-v.kick:
			default:
			}
		}
	}
}

// watch runs the view's watch of its class until ctx ends, beginning it again
// whenever it ends, and closes began once its first attempt has answered.
func (v *LeaseView) watch(ctx context.Context, began chan struct{}) {
	first := true
	failures := 0
	for {
		writes, err := v.store.WatchLive(ctx, v.class)
		if first {
			close(began)
		}
		if err == nil {
			if !first {
				// RESUMED: whatever was written while no watch was
				// listening is answered by a listing, not by waiting
				// out the cadence for it.
				v.Invalidate()
				if failures > 0 {
					log.InfoContext(ctx, "coord_view_watch_resumed", "class", string(v.class))
				}
			}
			failures = 0
			for w := range writes {
				if v.moves(w) {
					v.Invalidate()
				}
			}
		}
		first = false
		if ctx.Err() != nil {
			return
		}
		failures++
		if failures == 1 {
			detail := "the watch stopped hearing the store"
			if err != nil {
				detail = err.Error()
			}
			log.WarnContext(ctx, "coord_view_watch_failed", "class", string(v.class),
				"error", detail,
				"detail", "the view lists the class on its cadence and hears no join "+
					"or departure between listings until the watch begins again")
		}
		timer := time.NewTimer(backoff.Doubling(failures, MinViewRefresh, v.every))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// moves reports whether a watched write disagrees with the last listing — a
// holder the listing does not name, the tenure it names under another owner
// or epoch, or a release of a lease it names — which is a roster that may
// have moved since. A renewal of a lease the listing names agrees with it.
//
// Before any listing, every write moves it: the listing about to be taken
// answers for it, and asking costs nothing a listing in flight was not
// already going to pay.
func (v *LeaseView) moves(w LeaseWrite) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.loaded {
		return true
	}
	held, named := v.held[w.Resource]
	switch {
	case w.Owner == "":
		return named
	case !named:
		return true
	default:
		return held.Owner != w.Owner || held.Epoch != w.Epoch
	}
}

// refresh takes one listing, bounded by the view's own cadence so a store that
// hangs cannot stop the next one.
//
// DATED WHEN IT WAS ASKED, never when its answer arrived. What a listing
// answers held at some instant between the two, and a lease in it was live
// then — so it is renewed or lapsed a TTL after that instant, not after the
// answer: dated on arrival, a listing that took ten seconds was trusted ten
// seconds past anything the store said, and could name a node whose lease had
// already lapsed as a live answer.
func (v *LeaseView) refresh(ctx context.Context) {
	listing, cancel := context.WithTimeout(ctx, v.every)
	defer cancel()
	asked := v.now()
	leases, err := v.store.ListLive(listing, v.class)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			v.lastErr = err
		}
		return
	}
	if !v.loaded {
		close(v.listed)
	}
	held := make(map[string]Lease, len(leases))
	for _, l := range leases {
		held[l.Resource] = l
	}
	v.leases, v.held, v.loaded, v.listedAt, v.lastErr = slices.Clone(leases), held, true, asked, nil
}

// Leases is the view's answer: every lease of its class live as of its last
// listing, and when that listing was taken — or an error wrapping
// [ErrUnavailable] when the view has never listed or its last listing is older
// than its trust. Never an empty list standing in for unknown.
//
// The slice is the caller's own.
func (v *LeaseView) Leases() ([]Lease, time.Time, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case !v.loaded && v.lastErr != nil:
		return nil, time.Time{}, fmt.Errorf("%w: the %s leases have never been "+
			"listed: %w", ErrUnavailable, v.class, v.lastErr)
	case !v.loaded:
		return nil, time.Time{}, fmt.Errorf("%w: the %s leases have not been "+
			"listed yet", ErrUnavailable, v.class)
	}
	if age := v.now().Sub(v.listedAt); age > v.trust {
		detail := "no listing since"
		if v.lastErr != nil {
			detail = "every listing since has failed, the last with: " + v.lastErr.Error()
		}
		return nil, time.Time{}, fmt.Errorf("%w: the %s leases were last listed "+
			"%v ago, past the %v a lease survives unrenewed (%s)",
			ErrUnavailable, v.class, age.Round(time.Millisecond), v.trust, detail)
	}
	return slices.Clone(v.leases), v.listedAt, nil
}

// ListedAt is when the last listing that answered was ASKED, whatever its age,
// and the zero time if none has — for a reader that REPORTS how stale the view
// is, which [LeaseView.Leases] cannot tell it once the listing is past its
// trust. Nothing may act on a listing because of this; that is Leases' job.
func (v *LeaseView) ListedAt() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.listedAt
}

// Listed is closed once the view's first listing has answered — the moment
// [LeaseView.ListedAt] stops reading zero — and never before, however many
// listings failed first.
//
// FOR A GATE THAT WAITS ON THE FIRST LISTING, which no reading of the view
// can learn without asking again: a node's seat claims are refused while the
// view that routes its seats' calls has never listed the fleet, and the
// claims are asked again when this closes rather than at the next sweep. Read
// it, never poll ListedAt for it.
func (v *LeaseView) Listed() <-chan struct{} { return v.listed }

// Invalidate asks for a listing now, because a caller asked a node this view
// named and got no answer. It does not wait, and it does not drop the answer
// the view has — see the file's doc.
func (v *LeaseView) Invalidate() {
	select {
	case v.kick <- struct{}{}:
	default:
	}
}
