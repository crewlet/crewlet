// Package memory is the in-process coord.Backend — the twin every other
// backend is measured against, and the whole coordination layer for a
// single-node company.
//
// A semantic twin, not a stub. It models a STORE: records that outlive the
// leases written on them, an epoch counter that is only ever incremented, and
// its own clock. That last one is the part a naive in-process implementation
// gets wrong. Expiry is decided against the STORE's clock, never the caller's
// — a fleet where each node compares its own wall clock to a deadline hands
// two nodes the same seat the first time an NTP step separates them, and a
// twin that took the caller's time would certify that design.
//
// Records are never deleted, which is not a leak but the contract: releasing
// a lease expires it in place so the epoch counter survives, because a
// counter that restarts hands the next owner a token a zombie from the
// previous tenure is still fencing its writes with.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("coord.memory")

// Arguments a caller got wrong. They are errors rather than a (nil, nil)
// refusal on purpose: (nil, nil) means "somebody else holds this", and a
// blank owner has not lost a race to anybody. An error routes them to the
// contract's third answer — "no answer" — which a caller retries loudly
// instead of acting on a lie about a peer that does not exist.
var (
	errNoResource = errors.New("coord: resource is required")
	errNoOwner    = errors.New("coord: owner is required")
	errBadTTL     = errors.New("coord: ttl must be positive")
)

// Backend is the in-memory lease store. The zero value is ready to use and
// keeps time with time.Now.
type Backend struct {
	// Clock is the store's clock. Nil means time.Now. Injecting one
	// makes every expiry decision in the store deterministic, which is
	// what a caller's own test needs when it wants a lease to lapse at a
	// moment it chooses rather than at a moment it waits for.
	Clock func() time.Time

	mu    sync.Mutex
	rows  map[string]*record
	shift time.Duration
}

// New returns an empty store.
func New() *Backend { return &Backend{} }

// record is a resource's row. It outlives every lease written on it.
type record struct {
	resource  string
	owner     string
	epoch     int64
	expiresAt time.Time
	preferred string
	protocol  int
	meta      map[string]any
}

func (r *record) live(now time.Time) bool { return r.expiresAt.After(now) }

func (r *record) lease() coord.Lease {
	return coord.Lease{
		Resource:  r.resource,
		Owner:     r.owner,
		Epoch:     r.epoch,
		ExpiresAt: r.expiresAt,
		Preferred: r.preferred,
		Protocol:  r.protocol,
		// Copied on the way out for the same reason it is copied on the
		// way in: an out-of-process store cannot hand a caller a
		// reference into its own state, and a twin that did would let a
		// later mutation rewrite every peer's view of this node.
		Meta: copyMeta(r.meta),
	}
}

// Advance moves the store's clock forward. It is the coordtest.Advancer hook:
// the contract suite lapses a lease by outlasting its TTL, and this is how it
// does that without sleeping.
func (b *Backend) Advance(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.shift += d
}

// now is the store's clock. Callers must hold b.mu.
func (b *Backend) now() time.Time {
	clock := b.Clock
	if clock == nil {
		clock = time.Now
	}
	return clock().Add(b.shift).UTC()
}

// TryAcquire claims resource, or reports that someone else holds it.
func (b *Backend) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, error) {
	if err := unavailable(ctx); err != nil {
		return nil, err
	}
	if err := validateTTL(resource, opts.Owner, opts.TTL); err != nil {
		return nil, err
	}
	// Meta crosses a wire on every real backend, so it crosses one here.
	// See wireCopy: an in-process shortcut past this is a divergence the
	// twin would certify rather than catch.
	wire, err := wireCopy(opts.Meta)
	if err != nil {
		return nil, err
	}
	opts.Meta = wire
	lease := b.acquire(resource, opts)
	if lease == nil {
		return nil, nil
	}
	log.DebugContext(ctx, "lease_acquired", "resource", resource, "owner", opts.Owner, "epoch", lease.Epoch)
	return lease, nil
}

func (b *Backend) acquire(resource string, opts coord.AcquireOptions) *coord.Lease {
	protocol := opts.EffectiveProtocol()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rows == nil {
		b.rows = map[string]*record{}
	}
	now := b.now()

	// The mixed-version gate. Refuse while ANY live lease is held at an
	// older protocol — the disagreement is about what holding a lease
	// MEANS, so it is not scoped to the resource being claimed. Asymmetric
	// by construction: it only ever looks for a LOWER protocol, so an
	// older node (which has no such check to run) is never blocked.
	if !opts.Ungated {
		for _, r := range b.rows {
			if r.live(now) && r.protocol < protocol {
				return nil
			}
		}
	}

	row := b.rows[resource]
	switch {
	case row == nil:
		row = &record{resource: resource, epoch: 1}
		b.rows[resource] = row
	case row.live(now) && row.owner != opts.Owner:
		return nil
	case row.live(now):
		// An unbroken same-owner hold keeps its epoch: nothing was ever
		// unowned, so the holder's in-flight work stayed covered. This
		// branch is also the renew path — a claim doubles as one.
	default:
		// Takeover, or this same owner re-claiming after its own lease
		// lapsed. Both mint a new token, because during the gap the work
		// was covered by nothing and must be fenced against its own past
		// self.
		row.epoch++
	}

	row.owner = opts.Owner
	row.expiresAt = now.Add(opts.TTL)
	row.protocol = protocol
	// A claim that names no node leaves the hint alone: it records the
	// last deliberate placement, not who happens to hold the resource.
	if opts.Preferred != "" {
		row.preferred = opts.Preferred
	}
	// An empty payload keeps what is there — a rule about the PAYLOAD, not
	// about which resource carries it. A renew that forgets to re-send a
	// node's profile must not silently un-label it mid-flight, which peers
	// would read as a node matching no placement at all.
	if len(opts.Meta) > 0 {
		row.meta = copyMeta(opts.Meta)
	}

	lease := row.lease()
	return &lease
}

// Renew extends a lease the caller still holds at this epoch.
func (b *Backend) Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error) {
	if err := unavailable(ctx); err != nil {
		return false, err
	}
	if err := validateTTL(resource, owner, ttl); err != nil {
		return false, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	row := b.rows[resource]
	// A lapsed lease is deliberately NOT renewable. Re-acquiring is the
	// only way back and it mints a new epoch, which is what fences the gap
	// the lapse opened. Renewing across it would silently re-cover work
	// that ran unprotected.
	if row == nil || row.owner != owner || row.epoch != epoch || !row.live(now) {
		return false, nil
	}
	row.expiresAt = now.Add(ttl)
	return true, nil
}

// Release gives up a lease, predicated on owner AND epoch.
func (b *Backend) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if err := unavailable(ctx); err != nil {
		return false, err
	}
	if err := validate(resource, owner); err != nil {
		return false, err
	}
	if !b.release(resource, owner, epoch) {
		return false, nil
	}
	log.DebugContext(ctx, "lease_released", "resource", resource, "owner", owner, "epoch", epoch)
	return true, nil
}

func (b *Backend) release(resource, owner string, epoch int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	row := b.rows[resource]
	// The predicate is the whole point: an unqualified release lets a
	// departing owner clear its SUCCESSOR's live lease.
	//
	// Liveness is part of it. "Gives up a lease the caller holds" — and a
	// lapsed lease is not held, which is the same reading that makes Get
	// live-only. Releasing one is a no-op and must say so, or a drain that
	// lost a seat to a lapse is told it handed it back cleanly. The twin
	// answered true here while the KV backend answered false, and no case
	// sent the operation that would have shown it.
	if row == nil || row.owner != owner || row.epoch != epoch || !row.live(b.now()) {
		return false
	}
	// Expire in place, never delete. The epoch counter has to be monotonic
	// for the lifetime of the RESOURCE, not of a record — and expiring
	// also keeps the placement hint, so a rolling deploy can bring the
	// resource back to the node whose caches and MCP children are warm.
	row.owner = ""
	row.expiresAt = b.now()
	return true
}

// Get reads a resource's live lease, or nil when nothing holds it.
func (b *Backend) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if err := unavailable(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	row := b.rows[resource]
	// Lapsed reads as unheld. A Lease handed out by the store was live
	// when it was read, which is what lets a caller act on one without
	// re-checking a deadline against its own clock.
	if row == nil || !row.live(b.now()) {
		return nil, nil
	}
	lease := row.lease()
	return &lease, nil
}

// ListOwned returns the live leases this owner holds. A drain watches it
// converge to empty, so a lapsed or released lease must not appear.
func (b *Backend) ListOwned(ctx context.Context, owner string) ([]coord.Lease, error) {
	return b.list(ctx, func(r *record, now time.Time) bool {
		return r.owner == owner && r.live(now)
	})
}

// ListLive returns live leases under prefix. ListLive(coord.NodePrefix) is
// the membership read: counting live presence leases is how a node learns the
// fleet size it divides the seats by.
func (b *Backend) ListLive(ctx context.Context, prefix string) ([]coord.Lease, error) {
	return b.list(ctx, func(r *record, now time.Time) bool {
		return strings.HasPrefix(r.resource, prefix) && r.live(now)
	})
}

func (b *Backend) list(ctx context.Context, keep func(*record, time.Time) bool) ([]coord.Lease, error) {
	if err := unavailable(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var out []coord.Lease
	for _, r := range b.rows {
		if keep(r, now) {
			out = append(out, r.lease())
		}
	}
	// Sorted because a map iterates in a different order every time, and
	// an unstable listing turns any downstream ordering bug into one that
	// reproduces once in ten runs.
	slices.SortFunc(out, func(a, b coord.Lease) int {
		return strings.Compare(a.Resource, b.Resource)
	})
	return out, nil
}

// PreferredResources returns resources under prefix whose hint names nodeID,
// lapsed ones included — that is the hint's whole purpose. A live-only read
// would answer nothing in exactly the case it exists for: a node coming back
// from a restart looking for the seats it had warm.
func (b *Backend) PreferredResources(ctx context.Context, prefix, nodeID string) (map[string]struct{}, error) {
	if err := unavailable(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]struct{}{}
	for _, r := range b.rows {
		if strings.HasPrefix(r.resource, prefix) && r.preferred == nodeID {
			out[r.resource] = struct{}{}
		}
	}
	return out, nil
}

// FleetProtocolFloor returns the lowest protocol among live leases, and
// whether there were any. It is the observability half of the gate: TryAcquire
// can only answer yes or no, so a node stalled behind an older peer would
// otherwise look identical to one whose peers simply hold every seat.
func (b *Backend) FleetProtocolFloor(ctx context.Context) (int, bool, error) {
	if err := unavailable(ctx); err != nil {
		return 0, false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	floor, found := 0, false
	for _, r := range b.rows {
		if !r.live(now) {
			continue
		}
		if !found || r.protocol < floor {
			floor, found = r.protocol, true
		}
	}
	return floor, found, nil
}

// --- helpers --------------------------------------------------------------

// unavailable turns a dead context into the contract's third answer. The
// store is in-process and cannot be unreachable, but callers branch on
// unknown-versus-lost and that branch has to be reachable in a memory-backed
// test too, or it is only ever exercised in production.
func unavailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", coord.ErrUnavailable, err)
	}
	return nil
}

// validate checks the identity every call carries.
func validate(resource, owner string) error {
	switch {
	case resource == "":
		return errNoResource
	case owner == "":
		return errNoOwner
	}
	return nil
}

// validateTTL adds the deadline check for the calls that set one. A
// non-positive TTL would mint a lease that is already lapsed, which reads
// downstream as a seat nobody can hold.
func validateTTL(resource, owner string, ttl time.Duration) error {
	if err := validate(resource, owner); err != nil {
		return err
	}
	if ttl <= 0 {
		return errBadTTL
	}
	return nil
}

// wireCopy puts meta through the encoding a real backend puts it through.
//
// The twin's job is to be the store, and this is the one field that crosses a
// wire: adr-201 §2 records the ownership key carrying meta AS JSON, and
// placement.rolesFromMeta is written to accept "the []any a JSON round trip
// through the lease store returns" — so JSON shapes are what a deployment
// actually sees. An in-process store that skipped the encoding handed callers
// their own Go types back, measurably: an int stayed an int here and returned
// as float64 from the KV backend, a []string stayed a []string here and
// returned as []any. A caller asserting meta["n"].(int) then passes every
// memory-backed test in the tree and panics the first time it runs against
// the store a company is deployed on — a bug the twin would certify rather
// than catch.
//
// A payload that will not encode is a caller bug, not a store answer, so it
// is an error: a real backend's write would fail on it too, and discovering
// that in production instead of here is the whole failure this closes.
func wireCopy(meta map[string]any) (map[string]any, error) {
	if len(meta) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("coord: meta is not encodable: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("coord: meta did not survive its encoding: %w", err)
	}
	return out, nil
}

// copyMeta deep-copies the JSON-shaped payload a holder advertises about
// itself, so neither side can reach into the other's state after the call.
// The value is already in wire shape by the time it is stored, so this is
// isolation only — the encoding happened at the door, in wireCopy.
func copyMeta(meta map[string]any) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		out[k] = copyValue(v)
	}
	return out
}

func copyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return copyMeta(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = copyValue(item)
		}
		return out
	default:
		return v
	}
}

var _ coord.Backend = (*Backend)(nil)
