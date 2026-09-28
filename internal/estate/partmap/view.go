package partmap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A VIEW: this node's reading of the estate map and the estate leases, WATCHED
// — what a router asks "who serves this partition", and what a node's own join
// and leave read the map it acts on from.
//
// # Two halves, each kept current its own way
//
// The MAP is followed by a watch (coord.EstateMaps.WatchEstateMap), so a change
// reaches this node when it lands rather than an interval later — every
// router routes by it, and a node joins a partition because it names it. A
// watch that says nothing is either a map that has not changed or a watch
// that has stopped delivering, and the two cannot be told apart from inside
// it, so the map is also READ from the store every [ViewConfirm]: a read that
// answers confirms the view whatever it found. The LEASES are a
// [coord.LeaseView] of the `estate:` class — listed on the heartbeat, trusted
// for a TTL, three-valued (ADR-0005) — the same watched listing the fleet's
// presence view is, not a second one.
//
// # What it answers, and when it answers unknown
//
// Every answer is three-valued: a value, or an error wrapping
// [coord.ErrUnavailable] — never an empty holder list, an absent map or a
// layout standing in for "could not say". The map half is unknown until the
// store has answered once, and while the newest version it has is one this
// build cannot decode; the lease half is unknown as its lease view is.
//
// Which layout the fleet runs is the MAP's: a map that exists says its layout,
// whatever this node's build runs. A store that answers that there is NO map
// is layout 0 — every data node holding the one partition whole — only for a
// node that itself runs layout 0; a node running a partitioned layout with no
// map yet is a fleet whose partitions nobody has been named to hold, and that
// is an error, not a layout. Under a map a partition's servers are the map's
// serving holders — routing reads the holder table and nothing else.
//
// # Layout 0 routes by presence, never by the estate leases
//
// Under layout 0 the one partition's servers are EVERY LIVE DATA NODE, as this
// node's watched presence view names them ([Roster]) — the answer the estate's
// router gives there, so the two can never disagree about who serves a fleet
// with no map. The estate leases are deliberately not that answer, and either
// reason alone breaks routing. A build from before the estate lease claims
// none while it holds and serves the whole estate, so on a rolling upgrade
// that replaced the stateless nodes first, every seat tool on them would find
// nobody serving and fail with every data node up. And a lease's `serving` is
// its copy's own state, which a copy that falls behind leaves — a node holding
// the whole estate still answers for it, and says so itself when a read needs
// rows it has not applied. What the leases are for is what a MAP decides by: a
// joiner's word that it serves, and a leaver's that it has let go.
//
// # Freshness
//
// ROUTING may use the newest view whatever its age: a wrong route costs one
// "not the holder" and a retry, because the server is the authority. Anything
// that DECIDES from the view — the maintainer, a node's join and leave, the
// trim's counted set, a capacity window's participants, an eviction — asks
// [View.Fresh] first and treats a view whose map or leases were last confirmed
// more than [statelog.FloorCacheStale] ago as unknown, the age past which every
// cached coordination fact here is.

// ViewConfirm is how often a view reads the map from the store to confirm the
// watch has not gone silent.
//
// coord.ReconcileInterval — fifteen seconds — so a view is confirmed four times
// within [statelog.FloorCacheStale] (four of these), and three reads in a row
// may fail before anything deciding from it treats it as unknown: the ratio
// every other cached coordination fact here keeps. A read is one key from the
// stream's leader, so at this cadence a hundred nodes cost the store seven reads
// a second.
const ViewConfirm = coord.ReconcileInterval

// MapSource is where a view reads the estate map: the coordination store's
// read and watch of it.
type MapSource interface {
	EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error)
	WatchEstateMap(ctx context.Context) (<-chan coord.EstateMapRecord, error)
}

// Roster is the fleet's live data nodes as this node's watched presence view
// names them: layout 0's servers ([View.Serving]).
//
// FROM MEMORY, because a router asks it per request — the engine's presence
// view (a coord.LeaseView of the presence class) lists once per heartbeat.
type Roster interface {
	// LiveDataNodes is every live data node — or an error wrapping
	// [coord.ErrUnavailable] when that is not known, never an empty list
	// standing in for it, which would read as "nobody serves the estate".
	LiveDataNodes() ([]string, error)

	// Invalidate asks for a listing now, because a node it named did not
	// answer. It must not block.
	Invalidate()
}

// ViewOptions configure a [View].
type ViewOptions struct {
	// Maps is the estate map's store; Leases lists the estate leases.
	Maps   MapSource
	Leases coord.Lister

	// Running is the layout this node runs: what a store with no map is
	// read as, if it is layout 0.
	Running statelog.Layout

	// Roster is who serves layout 0's one partition while there is no map:
	// every live data node. REQUIRED where Running is layout 0, and unused
	// otherwise — a partitioned layout with no map has no servers at all.
	Roster Roster

	// Heartbeat is the estate leases' renew cadence, and TTL how long one
	// survives unrenewed — the lease view's cadence and its trust.
	Heartbeat, TTL time.Duration

	// Now is the clock, for tests. Nil is the wall clock.
	Now func() time.Time
}

// View is this node's watched reading of the estate map and the estate leases.
// Build it with [NewView] and run it with [View.Run]; its lifetime is the
// Run's.
type View struct {
	maps    MapSource
	leases  *coord.LeaseView
	roster  Roster
	running statelog.Layout
	now     func() time.Time

	mu sync.Mutex
	// known is whether the store has answered at all; found whether it
	// held a map when it last did, state that map and version its version.
	known   bool
	found   bool
	state   MapState
	version uint64
	// seen is the highest version this view has taken in, so an older
	// delivery arriving after a newer read is never taken over it.
	seen uint64
	// undecodable is why the newest version held could not be read, nil
	// while it could; lastErr is the last read or watch failure.
	undecodable, lastErr error
	// confirmedAt is when the store last answered, by a read or a
	// delivery.
	confirmedAt time.Time

	// watches are every [View.Watch] reader's slot.
	watches map[*viewWatch]struct{}
}

// NewView builds a view. It reads nothing until it runs.
func NewView(opts ViewOptions) (*View, error) {
	switch {
	case opts.Maps == nil:
		return nil, errors.New("estate/partmap: a view needs the estate map's store")
	case opts.Running.Validate() != nil:
		return nil, fmt.Errorf("estate/partmap: a view needs the layout this node runs: %w",
			opts.Running.Validate())
	case opts.Running.Number == 0 && opts.Roster == nil:
		return nil, errors.New("estate/partmap: a view on a node running layout 0 needs the " +
			"fleet's live data nodes, which serve that layout's one partition")
	}
	leases, err := coord.NewLeaseView(opts.Leases, coord.ClassEstate, coord.ViewOptions{
		Every: opts.Heartbeat, Trust: opts.TTL, Now: opts.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("estate/partmap: the estate leases' view: %w", err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &View{maps: opts.Maps, leases: leases, roster: opts.Roster, running: opts.Running,
		now: now, watches: map[*viewWatch]struct{}{}}, nil
}

// Run follows the map and lists the leases until ctx ends, and returns ctx's
// error; nothing else ends it. A watch that closes is opened again, and a
// failure is recorded and retried — the view answering unknown in the
// meantime once what it holds is older than its trust.
func (v *View) Run(ctx context.Context) error {
	var both sync.WaitGroup
	both.Add(1)
	go func() {
		defer both.Done()
		_ = v.leases.Run(ctx)
	}()
	v.follow(ctx)
	both.Wait()
	return ctx.Err()
}

// follow keeps the map half current: a watch for changes, a read every
// [ViewConfirm] for currency, and a watch opened again whenever it closes —
// never sooner than [coord.MinViewRefresh] after the last opening, so a store
// that closes every watch at once costs a watch a second rather than a spin.
func (v *View) follow(ctx context.Context) {
	confirm := time.NewTicker(ViewConfirm)
	defer confirm.Stop()
	var watch <-chan coord.EstateMapRecord
	var opened time.Time
	v.read(ctx)
	for ctx.Err() == nil {
		if watch == nil {
			if wait := coord.MinViewRefresh - v.now().Sub(opened); !opened.IsZero() && wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			opened = v.now()
			ch, err := v.maps.WatchEstateMap(ctx)
			if err != nil {
				v.failed(ctx, fmt.Errorf("watch the estate map: %w", err))
			} else {
				watch = ch
			}
		}
		select {
		case <-ctx.Done():
			return
		case rec, ok := <-watch:
			if !ok {
				watch = nil
				continue
			}
			v.fold(rec, true, 0)
		case <-confirm.C:
			v.read(ctx)
		}
	}
}

// read confirms the map half against the store, bounded by the confirmation
// interval so a store that hangs cannot hold the next one.
func (v *View) read(ctx context.Context) {
	asked, cancel := context.WithTimeout(ctx, ViewConfirm)
	defer cancel()
	before := v.seenVersion()
	rec, found, err := v.maps.EstateMap(asked)
	if err != nil {
		v.failed(ctx, fmt.Errorf("read the estate map: %w", err))
		return
	}
	v.fold(rec, found, before)
}

// seenVersion is the highest version this view has taken in.
func (v *View) seenVersion() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.seen
}

// failed records a failure the view is carrying, unless it is this view's own
// stop.
func (v *View) failed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	v.mu.Lock()
	v.lastErr = err
	v.mu.Unlock()
}

// fold takes in what the store answered — a version, or that there is no map —
// never an older version over a newer one, and hands a newly held map to every
// watch. before is, for a read, the highest version the view had taken in when
// the read was asked.
//
// VERSIONS ARE COMPARED, which coordtest certifies both backends for: a later
// write of the one key has a larger version, so a delivery at or below the
// newest version this view took in is one it already has or an older one a
// replica served late.
//
// AN ABSENCE HAS NO VERSION to compare, so a read that found no map is taken
// only if nothing newer was taken in while it was asked: a read answered as of
// an instant before the first map was written, arriving after the watch
// delivered that map, would otherwise put a fleet with a map back to one
// without.
func (v *View) fold(rec coord.EstateMapRecord, found bool, before uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if !found {
		if v.seen != before {
			// OVERTAKEN: the view took in a version while this read was
			// asked. The store answered, which confirms the view.
			v.confirmedAt, v.lastErr = now, nil
			return
		}
		// THE STORE HOLDS NO MAP NOW. Kept at the highest version seen,
		// so a delivery from before a removal is not taken back in.
		v.known, v.found, v.state, v.version, v.undecodable = true, false, MapState{}, 0, nil
		v.confirmedAt, v.lastErr = now, nil
		return
	}
	if v.known && rec.Version <= v.seen {
		// Already held, or older than what is: the store answered, so
		// the view is confirmed, and nothing else changes.
		v.confirmedAt, v.lastErr = now, nil
		return
	}
	v.seen, v.known, v.confirmedAt, v.lastErr = rec.Version, true, now, nil
	state, err := DecodeMapState(rec.Value)
	if err != nil {
		// A VERSION THIS BUILD CANNOT READ is the newest there is, so the
		// one before it is no longer the map: unknown until a readable
		// version replaces it.
		v.undecodable = err
		return
	}
	v.found, v.state, v.version, v.undecodable = true, state, rec.Version, nil
	for w := range v.watches {
		w.push(state.Map)
	}
}

// Map is the newest estate map this view holds and its version, false when the
// store holds none — or an error wrapping [coord.ErrUnavailable] when the view
// cannot say: never read, or its newest version undecodable. Whatever its age:
// see [View.Fresh] for a caller that decides.
func (v *View) Map() (Map, uint64, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.unknownLocked(); err != nil {
		return Map{}, 0, false, err
	}
	if !v.found {
		return Map{}, 0, false, nil
	}
	return v.state.Map.Clone(), v.version, true, nil
}

// unknownLocked is why the map half cannot answer, nil when it can. The caller
// holds the lock.
func (v *View) unknownLocked() error {
	switch {
	case !v.known && v.lastErr != nil:
		return fmt.Errorf("%w: the estate map has never been read: %w", coord.ErrUnavailable, v.lastErr)
	case !v.known:
		return fmt.Errorf("%w: the estate map has not been read yet", coord.ErrUnavailable)
	case v.undecodable != nil:
		return fmt.Errorf("%w: the newest estate map is one this build cannot read: %w",
			coord.ErrUnavailable, v.undecodable)
	}
	return nil
}

// Layout is the layout the fleet runs: the map's, or layout 0 where the store
// holds no map and this node runs layout 0 — see the file's doc.
func (v *View) Layout() (statelog.Layout, error) {
	m, _, found, err := v.Map()
	switch {
	case err != nil:
		return statelog.Layout{}, err
	case found:
		return m.Layout, nil
	case v.running.Number == 0:
		return v.running, nil
	}
	return statelog.Layout{}, fmt.Errorf("%w: this node runs layout %d and the fleet has no "+
		"estate map yet, so no partition has been placed", ErrNoMap, v.running.Number)
}

// Serving is p's serving holders in the order a router tries them, and the map
// epoch they were read at — 0 under layout 0, which has no map. An error is
// UNKNOWN, never "no holder": an empty list is a partition nobody serves.
func (v *View) Serving(p statelog.PartitionID) ([]string, uint64, error) {
	m, _, found, err := v.Map()
	switch {
	case err != nil:
		return nil, 0, err
	case found:
		if _, _, ok := m.table(p); !ok {
			return nil, 0, fmt.Errorf("%w: %q in layout %d", ErrUnknownPartition, p.String(),
				m.Layout.Number)
		}
		return m.Serving(p), m.Epoch, nil
	}
	nodes, err := v.wholeServers(p)
	return nodes, 0, err
}

// Serves reports whether node serves p — the map's half of a node's own
// account of what it may write (the join and leave executor holds the other).
// Three-valued, as [View.Serving] is.
func (v *View) Serves(node string, p statelog.PartitionID) (bool, error) {
	nodes, _, err := v.Serving(p)
	if err != nil {
		return false, err
	}
	return slices.Contains(nodes, node), nil
}

// wholeServers is p's servers where there is no map: under layout 0, every
// live data node — see the file's doc for why presence and not the estate
// leases.
func (v *View) wholeServers(p statelog.PartitionID) ([]string, error) {
	if v.running.Number != 0 {
		return nil, fmt.Errorf("%w: this node runs layout %d and the fleet has no estate "+
			"map yet, so no partition has been placed", ErrNoMap, v.running.Number)
	}
	if parts := v.running.Partitions(); len(parts) != 1 || parts[0] != p {
		return nil, fmt.Errorf("%w: %q in layout 0", ErrUnknownPartition, p.String())
	}
	nodes, err := v.roster.LiveDataNodes()
	if err != nil {
		return nil, fmt.Errorf("estate/partmap: who serves %s under layout 0: %w", p.String(), err)
	}
	out := slices.Clone(nodes)
	slices.Sort(out)
	return out, nil
}

// Read reads the map from the store NOW, takes it into the view, and answers
// it — for a caller told its routing is stale, which must not wait for the
// watch. [ErrNoMap] when the store holds none.
func (v *View) Read(ctx context.Context) (Map, uint64, error) {
	before := v.seenVersion()
	rec, found, err := v.maps.EstateMap(ctx)
	if err != nil {
		return Map{}, 0, fmt.Errorf("%w: read the estate map: %w", coord.ErrUnavailable, err)
	}
	v.fold(rec, found, before)
	m, version, held, err := v.Map()
	switch {
	case err != nil:
		return Map{}, 0, err
	case !held:
		return Map{}, 0, ErrNoMap
	}
	return m, version, nil
}

// Fresh reports whether both halves were confirmed within
// [statelog.FloorCacheStale]: the map by a read or a delivery the store
// answered, the leases by a listing. A caller that DECIDES from the view treats
// false as unknown — see the file's doc.
func (v *View) Fresh() bool {
	v.mu.Lock()
	confirmed := v.confirmedAt
	v.mu.Unlock()
	now := v.now()
	if confirmed.IsZero() || now.Sub(confirmed) > statelog.FloorCacheStale {
		return false
	}
	_, listed, err := v.leases.Leases()
	return err == nil && now.Sub(listed) <= statelog.FloorCacheStale
}

// Invalidate asks for a listing now, because a caller asked a node the view
// named and got no answer: of the estate leases ([coord.LeaseView.Invalidate]),
// and of presence where that is what named it.
func (v *View) Invalidate() {
	v.leases.Invalidate()
	if v.roster != nil {
		v.roster.Invalidate()
	}
}

// Watch delivers every map this view takes in from now until ctx ends — the one
// it holds first, then each newer one in order. It may skip a version replaced
// before it was handed over, as the store's own watch does: a reader acts on
// the newest map it holds. The channel closes when ctx ends.
func (v *View) Watch(ctx context.Context) (<-chan Map, error) {
	w := &viewWatch{wake: make(chan struct{}, 1)}
	v.mu.Lock()
	v.watches[w] = struct{}{}
	if v.found {
		w.push(v.state.Map)
	}
	v.mu.Unlock()
	out := make(chan Map)
	go func() {
		defer close(out)
		defer func() {
			v.mu.Lock()
			delete(v.watches, w)
			v.mu.Unlock()
		}()
		for {
			if m, ok := w.take(); ok {
				select {
				case out <- m:
				case <-ctx.Done():
					return
				}
				continue
			}
			select {
			case <-w.wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// viewWatch is one [View.Watch] reader's slot: the newest map not yet handed
// over.
type viewWatch struct {
	mu      sync.Mutex
	pending *Map
	wake    chan struct{}
}

// push puts a map in the slot, replacing one not yet handed over. Maps arrive
// under the view's lock in version order, so the slot only moves forward.
func (w *viewWatch) push(m Map) {
	c := m.Clone()
	w.mu.Lock()
	w.pending = &c
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// take empties the slot.
func (w *viewWatch) take() (Map, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == nil {
		return Map{}, false
	}
	m := *w.pending
	w.pending = nil
	return m, true
}
