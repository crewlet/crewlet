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

// A LEASE VIEW: one class of live leases, listed on a cadence and on demand,
// WATCHED for claims and releases, and read from memory by every caller in
// between, since a listing is an O(fleet) read (across the leaf link on a node
// without `data`) for an answer that changes only when a node joins or leaves.
//
// The watch makes a join reach every view within about a second rather than a
// heartbeat, which a shorter cadence could only buy by listing the whole class
// more often. THE WATCH IS A HINT AND THE LISTING IS STILL THE ANSWER: a write
// that disagrees with the listing asks for a new one ([LeaseView.moves]) and is
// never applied to it, since what is live is the store's judgement on its own
// clock. A lapse is no write, so a stopped node leaves at the first listing past
// its TTL; while the watch is down the view is the plain listed view.

// MinViewRefresh is the shortest interval between two listings a view makes
// because a caller invalidated it or its watch heard the roster move.
//
// ONE SECOND: long enough that a node which never answers, or a fleet booting
// at once (every claim a write each view hears), costs a view at most a listing
// a second; short enough that a lease released on a clean stop leaves the roster
// before a caller has retried more than once or twice.
const MinViewRefresh = time.Second

// ViewOptions configure a [LeaseView].
type ViewOptions struct {
	// Every is the cadence it lists at. Required: it is the heartbeat of
	// the leases it watches, whatever that is on this deployment.
	Every time.Duration

	// Trust is how old a listing may be and still be an answer. Required,
	// and at least Every: it is the TTL of the leases it watches, past which
	// a listing may name a node that is gone and miss one that joined.
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
// it with [LeaseView.Run]; its lifetime is the Run's. It holds the LEASES, not
// a projection, so a caller needing a node's profile reads it off the answer.
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
	// THE WATCH FIRST, so nothing written between the two goes unheard. Its
	// first attempt is bounded by the store's request budget, and one that
	// fails lets the listing go ahead as the plain listed view.
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
// whenever it ends (after [MinViewRefresh], doubling to the cadence, past which
// a listing would have caught what it missed), and closes began once its first
// attempt has answered.
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
				// RESUMED: what was written while nobody listened is
				// answered by a listing now, not at the cadence.
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
// or epoch, or a release of a lease it names. A renewal agrees, so the watch
// costs a listing only when the roster moved; a renewal's fresh payload is
// read at the next listing. Before any listing every write moves it, at no
// cost beyond the listing already coming.
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
// DATED WHEN IT WAS ASKED, never when its answer arrived: a lease in it was
// live at some instant between the two, so it lapses a TTL after that instant.
// Dated on arrival, a slow listing would be trusted past what the store said
// and could name a lapsed lease as live.
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
// than its trust. THREE-VALUED: never an empty or stale list standing in for
// unknown (package doc, "The tri-state, and why Go gets it for free").
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
// FOR A GATE THAT WAITS ON THE FIRST LISTING: a node's seat claims are refused
// until the view routing its seats' calls has listed, and are asked again when
// this closes rather than at the next sweep. Never poll ListedAt for it.
func (v *LeaseView) Listed() <-chan struct{} { return v.listed }

// Invalidate asks for a listing now, because a caller asked a node this view
// named and got no answer, so a node that released its lease on a clean stop
// leaves the next answer. A REQUEST, not a wait: the view answers from the
// listing it has, still within trust, meanwhile. Requests coalesce and are held
// [MinViewRefresh] apart, so a silent node cannot make every request a listing.
func (v *LeaseView) Invalidate() {
	select {
	case v.kick <- struct{}{}:
	default:
	}
}
