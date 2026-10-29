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
//
// # The counter outlives the process; the leases do not
//
// A single node's leases are rightly this process's: it has nobody to fence
// against, and it re-claims everything at boot. Its fencing tokens are not,
// because the rows they are compared with (a sandbox run's owner_epoch above
// all) are the fleet store's and outlive the process. So the counter is
// pluggable ([Backend.Epochs]): the engine hands a single node the same
// persistent counter a fleet's lease store mints from (coord/kv.Epochs), and
// a token minted after a restart is strictly above every token minted before
// it. Nil keeps the counter in this process, which is right for a test and
// for nothing that persists a token.
//
// A token is minted OUTSIDE the store's lock, because a persistent counter is
// a round trip and every renew, release and read of every other resource
// waits on that lock. The resource is held meanwhile by a CLAIMING marker,
// the in-process form of the KV store's claiming record: a peer reads it as
// held, and a claim that cannot mint gives it back.
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

// Arguments a caller got wrong. They are errors rather than a refusal on
// purpose: a refusal means "somebody else holds this" or "a gate stopped you",
// and a blank owner has done neither. An error routes them to the contract's
// third answer — "no answer" — which a caller retries loudly instead of acting
// on a lie about a peer that does not exist.
var (
	errNoOwner = errors.New("coord: owner is required")
	errBadTTL  = errors.New("coord: ttl must be positive")
)

// EpochCounter mints fencing tokens. Next returns a token strictly above every
// one it has returned for the resource, or an error that mints nothing usable.
// coord/kv.Epochs is the persistent one.
type EpochCounter interface {
	Next(ctx context.Context, resource string) (int64, error)
}

// Backend is the in-memory lease store. The zero value is ready to use and
// keeps time with time.Now.
type Backend struct {
	// Clock is the store's clock. Nil means time.Now. Injecting one
	// makes every expiry decision in the store deterministic, which is
	// what a caller's own test needs when it wants a lease to lapse at a
	// moment it chooses rather than at a moment it waits for.
	Clock func() time.Time

	// Epochs mints every fencing token this store hands out. Nil counts in
	// this process, so a token restarts with it: right for a test, and
	// wrong for any deployment whose tokens are written to records that
	// outlive the process. See the package doc.
	Epochs EpochCounter

	mu    sync.Mutex
	rows  map[string]*record
	shift time.Duration
	// minted is the in-process counter Epochs stands in for when nil.
	minted map[string]int64
	// claims numbers the claims in flight, so a claim gives back or
	// commits only the marker it set.
	claims uint64
}

// New returns an empty store.
func New() *Backend { return &Backend{} }

// record is a resource's row. It outlives every lease written on it.
type record struct {
	resource  string
	owner     string
	epoch     int64
	expiresAt time.Time
	// acquiredAt is when the current epoch was minted; see
	// coord.Lease.AcquiredAt.
	acquiredAt time.Time
	preferred  string
	protocol   int
	meta       map[string]any
	// claiming is the claim minting this record's next token, or nil. Only
	// a record with no live lease is ever claimed.
	claiming *claim
}

// claim is one TryAcquire between deciding to mint a token and committing it.
type claim struct {
	id       uint64
	owner    string
	protocol int
}

func (r *record) live(now time.Time) bool { return r.expiresAt.After(now) }

// held reports whether the record keeps the resource from a claimant: a live
// lease, or a claim minting one. The KV store reads its claiming record the
// same way.
func (r *record) held(now time.Time) bool { return r.live(now) || r.claiming != nil }

// heldBy is the owner [record.held] holds the resource for.
func (r *record) heldBy() string {
	if r.claiming != nil {
		return r.claiming.owner
	}
	return r.owner
}

// gated reports whether the record is one the protocol gate judges, and at
// which protocol: held, and of a class the gate counts
// (coord.ProtocolGateCounts). A claim counts at its own protocol, as the KV
// store counts a claiming record.
func (r *record) gated(now time.Time) (int, bool) {
	if !r.held(now) || !coord.ProtocolGateCounts(r.resource) {
		return 0, false
	}
	if r.claiming != nil {
		return r.claiming.protocol, true
	}
	return r.protocol, true
}

func (r *record) lease() coord.Lease {
	return coord.Lease{
		Resource:   r.resource,
		Owner:      r.owner,
		Epoch:      r.epoch,
		ExpiresAt:  r.expiresAt,
		AcquiredAt: r.acquiredAt,
		Preferred:  r.preferred,
		Protocol:   r.protocol,
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
func (b *Backend) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {
	if err := unavailable(ctx); err != nil {
		return nil, "", err
	}
	if err := validateTTL(resource, opts.Owner, opts.TTL); err != nil {
		return nil, "", err
	}
	// Meta crosses a wire on every real backend, so it crosses one here.
	// See wireCopy: an in-process shortcut past this is a divergence the
	// twin would certify rather than catch.
	wire, err := wireCopy(opts.Meta)
	if err != nil {
		return nil, "", err
	}
	opts.Meta = wire
	lease, refused, pending, err := b.decide(resource, opts)
	if err != nil || pending == nil {
		return lease, refused, err
	}
	// THE MARKER IS GIVEN BACK ON EVERY EXIT, a counter that panics
	// included: left set, it held the resource against every claimant, its
	// own owner's retries too, for the life of the process. commit clears it
	// on every path it takes, so this finds nothing to do unless commit
	// never ran.
	defer b.giveBack(resource, pending)
	// THE ONE STEP OUTSIDE THE LOCK: see the package doc.
	epoch, err := b.next(ctx, resource)
	lease, err = b.commit(resource, opts, pending, epoch, err)
	if err != nil {
		return nil, "", err
	}
	log.DebugContext(ctx, "lease_acquired", "resource", resource, "owner", opts.Owner, "epoch", lease.Epoch)
	return lease, "", nil
}

// next mints resource's next token from [Backend.Epochs], or from this
// process's own count when there is none.
func (b *Backend) next(ctx context.Context, resource string) (int64, error) {
	if b.Epochs != nil {
		epoch, err := b.Epochs.Next(ctx, resource)
		if err != nil {
			return 0, fmt.Errorf("%w: mint the fencing epoch of %s: %w", coord.ErrUnavailable, resource, err)
		}
		return epoch, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.minted == nil {
		b.minted = map[string]int64{}
	}
	b.minted[resource]++
	return b.minted[resource], nil
}

// decide is a claim's first half, under the lock: a refusal, a renew of a
// lease the owner still holds (which keeps its token and is the whole answer),
// or a CLAIM set on the record, for [Backend.commit] to finish once a new
// token is minted.
func (b *Backend) decide(resource string, opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, *claim, error) {
	protocol := opts.EffectiveProtocol()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rows == nil {
		b.rows = map[string]*record{}
	}
	now := b.now()

	row := b.rows[resource]
	if row != nil && row.held(now) && row.heldBy() != opts.Owner {
		// A PEER HOLDS IT, or is minting the token to, which is the answer
		// whatever the gate would say: a claim that cannot write has
		// nothing for a gate to stop. First, so the twin gives the reason
		// the KV backend gives (see coord.RefusedHeld).
		return nil, coord.RefusedHeld, nil, nil
	}
	if row != nil && row.claiming != nil {
		// THIS OWNER is already minting a token for it, on another call.
		// Neither a grant nor a refusal is true yet, so it is the third
		// answer, as the KV store gives an owner whose sibling claim is
		// one round trip from committing.
		return nil, "", nil, fmt.Errorf("%w (%s, by %s)", coord.ErrClaimInFlight,
			resource, opts.Owner)
	}

	// The mixed-version gate. Refuse while any live lease the gate counts
	// — presence and seats, never a duty (coord.ProtocolGateCounts) — is
	// held at a lower protocol. The disagreement is about what holding a
	// seat MEANS, so it is not scoped to the resource being claimed.
	// Asymmetric by construction: it only ever looks for a LOWER protocol,
	// so a lower-protocol node is never blocked.
	if !opts.Ungated {
		for _, r := range b.rows {
			if held, ok := r.gated(now); ok && held < protocol {
				return nil, coord.RefusedProtocol, nil, nil
			}
		}
	}

	if row != nil && row.live(now) {
		// An unbroken same-owner hold keeps its epoch: nothing was ever
		// unowned, so the holder's in-flight work stayed covered. This
		// branch is also the renew path — a claim doubles as one.
		b.write(row, opts, now)
		lease := row.lease()
		return &lease, "", nil, nil
	}

	// Unclaimed, a takeover, or this same owner re-claiming after its own
	// lease lapsed. Each mints a new token, because during the gap the work
	// was covered by nothing and must be fenced against its own past self.
	if row == nil {
		row = &record{resource: resource}
		b.rows[resource] = row
	}
	b.claims++
	row.claiming = &claim{id: b.claims, owner: opts.Owner, protocol: protocol}
	return nil, "", row.claiming, nil
}

// commit is a claim's second half: it gives the record back when no token
// could be minted, and otherwise starts the new tenure at the token minted.
//
// A TOKEN AT OR BELOW THE ONE THE RECORD ALREADY CARRIES IS REFUSED, as
// UNKNOWN: it can only mean the counter was reset under this process, and
// handing it out is the one mistake the counter exists to prevent.
func (b *Backend) commit(resource string, opts coord.AcquireOptions, pending *claim, epoch int64, minted error) (*coord.Lease, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	row := b.rows[resource]
	if row == nil || row.claiming == nil || row.claiming.id != pending.id {
		// Nothing but this claim clears its own marker, so this is a bug.
		return nil, fmt.Errorf("%w: the claim of %s lost its marker while minting its epoch",
			coord.ErrUnavailable, resource)
	}
	row.claiming = nil
	if minted != nil {
		return nil, minted
	}
	if epoch <= row.epoch {
		return nil, fmt.Errorf("%w: the epoch counter for %s minted %d, which is not above "+
			"the %d this store already handed out; the counter was reset, and a reset "+
			"counter hands out tokens a past tenure still holds", coord.ErrUnavailable,
			resource, epoch, row.epoch)
	}
	// A new token is a new tenure, so it gets its own start.
	now := b.now()
	row.epoch, row.acquiredAt = epoch, now
	b.write(row, opts, now)
	lease := row.lease()
	return &lease, nil
}

// giveBack clears pending's claiming marker if it is still set, and only
// pending's: a later claim's marker is never another call's to clear.
func (b *Backend) giveBack(resource string, pending *claim) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if row := b.rows[resource]; row != nil && row.claiming != nil && row.claiming.id == pending.id {
		row.claiming = nil
	}
}

// write sets what every grant sets, a renew's included.
func (b *Backend) write(row *record, opts coord.AcquireOptions, now time.Time) {
	row.owner = opts.Owner
	row.expiresAt = now.Add(opts.TTL)
	row.protocol = opts.EffectiveProtocol()
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
	// The twin has no key encoding to be defeated by an empty segment, so
	// this refusal buys it nothing on its own — it is here because the
	// twin's job is to answer what the real backend answers, and a twin
	// that accepted a name the store refuses would certify a contract
	// nothing keeps.
	if err := coord.CheckResource(resource); err != nil {
		return nil, err
	}
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

// ListLive returns the live leases of one class. ListLive(coord.ClassNode) is
// the membership read: counting live presence leases is how a node learns the
// fleet size it divides the seats by.
//
// The twin matches on the class PREFIX where the real backend narrows at the
// broker, and the two must select the same set — which is why the class is
// refused here too rather than quietly matching nothing.
func (b *Backend) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	if err := checkClass(class); err != nil {
		return nil, err
	}
	return b.list(ctx, func(r *record, now time.Time) bool {
		return class.Holds(r.resource) && r.live(now)
	})
}

// checkClass refuses a class that cannot address a key, so the twin and the
// real backend refuse the same arguments — a twin that answered where the
// broker-backed store refused would certify a contract nothing keeps.
func checkClass(class coord.Class) error {
	if class.Valid() {
		return nil
	}
	return fmt.Errorf("coord/memory: %q is not a resource class: a class is the "+
		"leading segment of a resource name, so it must be non-empty and "+
		"contain no %q", string(class), coord.ResourceSeparator)
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

// PreferredResources returns resources of this class whose hint names nodeID,
// lapsed ones included — that is the hint's whole purpose. A live-only read
// would answer nothing in exactly the case it exists for: a node coming back
// from a restart looking for the seats it had warm.
func (b *Backend) PreferredResources(ctx context.Context, class coord.Class, nodeID string) (map[string]struct{}, error) {
	if err := checkClass(class); err != nil {
		return nil, err
	}
	if err := unavailable(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]struct{}{}
	for _, r := range b.rows {
		if class.Holds(r.resource) && r.preferred == nodeID {
			out[r.resource] = struct{}{}
		}
	}
	return out, nil
}

// FleetProtocolFloor returns the lowest protocol among the live leases the
// gate counts (coord.ProtocolGateCounts), and whether there were any. It is
// the observability half of the gate: a claim refused [coord.RefusedProtocol]
// says a lease at a lower protocol stopped it, and this names that protocol —
// so it counts what the gate counts and nothing else. It is asked after such
// a refusal and never otherwise.
func (b *Backend) FleetProtocolFloor(ctx context.Context) (int, bool, error) {
	if err := unavailable(ctx); err != nil {
		return 0, false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	floor, found := 0, false
	for _, r := range b.rows {
		protocol, ok := r.gated(now)
		if !ok {
			continue
		}
		if !found || protocol < floor {
			floor, found = protocol, true
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
//
// The resource goes through [coord.CheckResource] rather than an emptiness
// test, because a name with an empty SEGMENT is the one that fails silently:
// it builds a key nothing can decode, so the lease is written and then
// returned by no listing at all — which reads to every node as a free seat.
func validate(resource, owner string) error {
	if err := coord.CheckResource(resource); err != nil {
		return err
	}
	if owner == "" {
		return errNoOwner
	}
	return nil
}

// validateTTL adds the deadline check for the calls that set one. A
// non-positive TTL would mint a lease that is already lapsed, which reads
// downstream as a seat nobody can hold.
//
// The twin could honour a TTL of any length, and refuses one above
// coord.MaxDutyTTL on every lease outside seats and presence anyway — a duty,
// a tracker walk's claim, any class of a caller's — through the contract's own
// check (coord.CheckLeaseTTL). A twin that accepted what the KV store refuses
// is exactly how every long duty claim passed every single-node test while no
// fleet ever ran those duties, and how the tracker's walk claims did the same
// after the twin learned the duties' ceiling and nobody else's.
//
// A seat or presence lease is not bounded here: the KV store holds one to the
// seat lease TTL it was opened with, a property of the deployment the twin is
// never given, and its callers acquire at the TTL that store reports.
func validateTTL(resource, owner string, ttl time.Duration) error {
	if err := validate(resource, owner); err != nil {
		return err
	}
	if ttl <= 0 {
		return errBadTTL
	}
	return coord.CheckLeaseTTL(resource, ttl)
}

// wireCopy puts meta through the encoding a real backend puts it through.
//
// The twin's job is to be the store, and this is the one field that crosses a
// wire: the ownership key carries meta AS JSON, and
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
