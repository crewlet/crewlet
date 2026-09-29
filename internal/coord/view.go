package coord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// A LEASE VIEW: one class of live leases, WATCHED — listed on a cadence and
// on demand, and read from memory by every caller in between.
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
// than an id — a node's profile, what it advertises, a placement map read
// from the same heartbeat — reads it off the same answer rather than asking
// the store again.

// MinViewRefresh is the shortest interval between two listings a view makes
// because a caller invalidated it.
//
// ONE SECOND: long enough that a node which never answers costs at most one
// listing a second however many requests fail against it, and short enough
// that a lease released on a clean stop leaves the roster before a caller has
// retried more than once or twice.
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

// Lister is the one call a view makes of the store.
type Lister interface {
	ListLive(ctx context.Context, class Class) ([]Lease, error)
}

// LeaseView watches one class of leases. Build it with [NewLeaseView] and run
// it with [LeaseView.Run]; its lifetime is the Run's.
type LeaseView struct {
	lister Lister
	class  Class
	every  time.Duration
	trust  time.Duration
	now    func() time.Time

	// kick asks the loop for a listing now; capacity one, so any number
	// of invalidations between two listings is one listing.
	kick chan struct{}

	mu       sync.Mutex
	leases   []Lease
	loaded   bool
	listedAt time.Time
	lastErr  error
}

// NewLeaseView builds a view of one class. It lists nothing until it runs.
func NewLeaseView(lister Lister, class Class, opts ViewOptions) (*LeaseView, error) {
	switch {
	case lister == nil:
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
		lister: lister, class: class, every: opts.Every, trust: opts.Trust,
		now: now, kick: make(chan struct{}, 1),
	}, nil
}

// Run lists now and then on the view's cadence and on every invalidation,
// until ctx ends. It returns ctx's error, and nothing else ends it: a listing
// that fails is recorded, and the view answers unknown once its last good one
// is older than its trust.
func (v *LeaseView) Run(ctx context.Context) error {
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

// refresh takes one listing, bounded by the view's own cadence so a store that
// hangs cannot stop the next one.
func (v *LeaseView) refresh(ctx context.Context) {
	listing, cancel := context.WithTimeout(ctx, v.every)
	defer cancel()
	leases, err := v.lister.ListLive(listing, v.class)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			v.lastErr = err
		}
		return
	}
	v.leases, v.loaded, v.listedAt, v.lastErr = slices.Clone(leases), true, v.now(), nil
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

// ListedAt is when the last listing that answered was taken, whatever its age,
// and the zero time if none has — for a reader that REPORTS how stale the view
// is, which [LeaseView.Leases] cannot tell it once the listing is past its
// trust. Nothing may act on a listing because of this; that is Leases' job.
func (v *LeaseView) ListedAt() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.listedAt
}

// Invalidate asks for a listing now, because a caller asked a node this view
// named and got no answer. It does not wait, and it does not drop the answer
// the view has — see the file's doc.
func (v *LeaseView) Invalidate() {
	select {
	case v.kick <- struct{}{}:
	default:
	}
}
