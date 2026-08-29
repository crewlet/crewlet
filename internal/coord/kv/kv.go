// Package kv is the coord.Backend a fleet runs on: TTL leases with a fencing
// epoch, on NATS JetStream KV.
//
// The single-node company uses the in-memory twin. This is what the same
// engine becomes when a second node joins: the same contract, certified by
// the same suite (internal/coord/coordtest), with the mutual exclusion moved
// from a mutex to the broker's compare-and-swap.
//
// # Two buckets, and why the epoch needs its own
//
// Postgres kept ONE row per resource and expired it in place, so the epoch
// survived release. A KV deletes a key when it expires — and a deleted key
// restarts the counter, handing the next owner a token a zombie from the
// previous tenure is still fencing its writes with. So ownership and the
// fencing token live in two buckets:
//
//   - crewlet_leases, created with KeyValueConfig.TTL = the lease TTL. That
//     TTL is the STREAM's MaxAge, which is the renewable one: every write
//     refreshes the entry's age, so Update at the current revision IS the
//     renew, an unrenewed key expires SERVER-SIDE, and a peer's Create then
//     succeeds. The store's own expiry is the arbiter clock — the role
//     Postgres now() played — and nodes never compare their own wall clocks.
//
//   - crewlet_epochs, with NO TTL. One persistent record per resource holding
//     the monotonic counter and the placement hint. Measured to survive the
//     lease key's expiry, which is the entire fencing invariant. Gaps in the
//     counter are harmless; resets are not.
//
// # Per-key TTL cannot be used for this
//
// jetstream.KeyTTL is create-only by design — "the TTL is set when the key is
// created and cannot be changed later" — and Update clears it. Measured (see
// behavior_test.go, nats-server 2.14.5 / nats.go 1.53.1): a key created with
// a 1s KeyTTL and renewed through Update was still readable two seconds
// later. IMMORTAL. On a lease that means a dead node's seat could never be
// reclaimed, and every sweep would read healthy while the seat sat dark. The
// bucket-MaxAge form is the renewable one, and behavior_test.go asserts the
// trap so this cannot be "simplified" back.
//
// # One TTL per bucket
//
// MaxAge is a property of the bucket, so the backend takes its TTL at
// construction. A per-call TTL LONGER than the configured one is refused: the
// bucket would reap the record early and the deadline handed back would be a
// lie about when the lease ends. A per-call TTL SHORTER than the configured
// one is honoured by an additional deadline carried in the record — see
// Store.resolveNow for how "now" is obtained, which is never from the
// caller's clock. In production every caller passes the configured TTL (seats,
// singleton duties and node presence all run on the one heartbeat), the two
// deadlines coincide, and the server's own expiry decides everything.
//
// # A claim is three writes, and the order carries the invariant
//
// The epoch must be committed to the untimed bucket BEFORE ownership is
// written: the other order leaves ownership at a token the counter has not
// committed, and after the lease key expires a zombie from that tenure fences
// straight through the next one. But advancing the counter first also means
// every LOSER of the ownership CAS has advanced it, so the winner of a
// fleet-wide stampede holds whatever it happened to reserve rather than 1.
//
// Both are satisfied by winning ownership in a CLAIMING state first — owner
// set, epoch 0 — then advancing the counter, then committing the token into the
// record already held. Epoch 0 is exactly the right marker because it is
// exactly the wrong token: a conditional write predicated on it matches an
// unset column, and TryAcquire has not returned, so nothing can be written
// under it. Exactly one claimant gets past step 1, so exactly one advances the
// counter. A renew is still one write.
//
// # The protocol gate degrades, deliberately
//
// Postgres evaluated "no live lease at a lower protocol" as a subquery INSIDE
// the claim statement, because a read-then-claim loses the race it exists to
// prevent. A KV cannot express a cross-key predicate inside a CAS, so the
// shape here is check -> claim -> RE-CHECK -> release on violation. The window
// shrinks to the interval between our check and our claim, and its consequence
// changes from silent mixed-protocol operation to a claim we immediately give
// back. Combined with the gate's existing asymmetry — only newer nodes wait,
// older ones were never gated — that is a faithful degradation and a recorded
// difference, not an oversight (decisions/201-coordination-contract.md
// §3).
package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("coord.kv")

// Arguments a caller got wrong. They are errors rather than a (nil, nil)
// refusal on purpose: (nil, nil) means "somebody else holds this", and a blank
// owner has not lost a race to anybody. An error routes them to the contract's
// third answer — "no answer" — which a caller retries loudly instead of acting
// on a lie about a peer that does not exist.
var (
	errNoResource = errors.New("coord/kv: resource is required")
	errNoOwner    = errors.New("coord/kv: owner is required")
	errBadTTL     = errors.New("coord/kv: ttl must be positive")
	errTTLTooLong = errors.New("coord/kv: ttl exceeds the bucket's configured TTL")
)

const (
	// defaultBucketPrefix yields crewlet_leases and crewlet_epochs, the
	// names decisions/201 records.
	defaultBucketPrefix = "crewlet"

	leasesSuffix = "_leases"
	epochsSuffix = "_epochs"

	// minBucketTTL is nats-server's own floor on a stream's MaxAge
	// ("max age needs to be >= 100ms", server/stream.go). Checking it here
	// turns a confusing API error at Open into a named configuration
	// failure.
	minBucketTTL = 100 * time.Millisecond

	// maxReplicas is JetStream's cap on a stream's replica count.
	maxReplicas = 5
)

// casAttempts bounds the compare-and-swap retries of one lease operation.
//
// Every retry follows a LOST CAS, which means the store is answering and
// somebody else wrote the record. So the number that matters is not a timeout
// but a HEAD COUNT: N callers contending one key make progress one per round,
// because each round has exactly one CAS winner, so a caller can need up to N
// rounds. Two things put callers on one key at the same instant — the fleet
// sweeping for unclaimed seats on a synchronized tick, and a single node's own
// heartbeat, sweep and recovery paths re-claiming a seat it already holds.
//
// 64 is sized to exceed both: it is twice the 32-way stampede the contract
// suite runs, and far more than the placement model's per-group node count.
// The cost of setting it too high is nil — retries only happen while the store
// is healthy and someone is winning — and the cost of too low is a healthy
// contended claim reported as UNKNOWN. Exhausting it IS reported as unknown,
// never as a refusal: losing a CAS repeatedly says nothing about who holds the
// resource, and a caller that read it as "somebody else has it" would shed
// work over contention.
const casAttempts = 64

// validBucketName is nats.go's own bucket rule (jetstream/kv.go). Checking the
// prefix here names the problem instead of failing inside bucket creation.
var validBucketName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Config is what a Store needs at construction.
type Config struct {
	// TTL is the lease TTL and the leases bucket's MaxAge. Required.
	//
	// It is a property of the BUCKET, not of a call: see the package doc.
	// A per-call TTL longer than this is refused; a shorter one is honoured
	// against the store's own clock.
	TTL time.Duration

	// Replicas is the JetStream replica count for both buckets. Zero means
	// 1. In a real fleet this should be 3: a coordination store with one
	// replica makes the whole company's seat ownership depend on one
	// broker node staying up.
	Replicas int

	// BucketPrefix names the two buckets — "<prefix>_leases" and
	// "<prefix>_epochs". Empty means "crewlet". Two companies sharing one
	// NATS account are separated by giving them different prefixes; sharing
	// one would make each company's leases gate the other's claims, because
	// the protocol gate is deliberately fleet-wide.
	BucketPrefix string
}

func (c *Config) normalize() error {
	if c.BucketPrefix == "" {
		c.BucketPrefix = defaultBucketPrefix
	}
	if c.Replicas == 0 {
		c.Replicas = 1
	}
	switch {
	case c.TTL <= 0:
		return fmt.Errorf("coord/kv: Config.TTL is required")
	case c.TTL < minBucketTTL:
		return fmt.Errorf("coord/kv: Config.TTL %v is below the broker's %v floor on a bucket TTL",
			c.TTL, minBucketTTL)
	case c.Replicas < 0 || c.Replicas > maxReplicas:
		return fmt.Errorf("coord/kv: Config.Replicas %d is outside 1..%d", c.Replicas, maxReplicas)
	case !validBucketName.MatchString(c.BucketPrefix + leasesSuffix):
		return fmt.Errorf("coord/kv: Config.BucketPrefix %q is not a valid bucket name "+
			"(letters, digits, '-' and '_' only)", c.BucketPrefix)
	}
	return nil
}

// Store is the JetStream KV coord.Backend.
type Store struct {
	js     jetstream.JetStream
	leases jetstream.KeyValue
	epochs jetstream.KeyValue

	// leaseStream is the name of the stream backing the leases bucket,
	// resolved once at Open. storeNow needs it because the clock is read
	// through js.Stream, not through KeyValue.Status: Status caches the
	// StreamInfo it fetched onto the shared bucket handle WITHOUT a lock
	// (nats.go 1.53.1, jetstream/stream.go Info), so two goroutines reading
	// the clock at once is a data race in the client. js.Stream hands back a
	// fresh handle per call and shares nothing.
	leaseStream string

	ttl time.Duration
}

var _ coord.Backend = (*Store)(nil)

// Open creates or adopts the two buckets and returns the backend.
//
// Idempotent, and safe to call concurrently from every node in the fleet:
// creating a bucket that already exists with the same shape is a no-op, and a
// changed TTL is applied as a stream update.
func Open(ctx context.Context, nc *nats.Conn, cfg Config) (*Store, error) {
	if nc == nil {
		return nil, errors.New("coord/kv: a NATS connection is required")
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: jetstream context: %w", err)
	}

	leases, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      cfg.BucketPrefix + leasesSuffix,
		Description: "Crewlet lease ownership; the bucket TTL is the lease TTL and its expiry is the arbiter",
		TTL:         cfg.TTL,
		Replicas:    cfg.Replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("coord/kv: open %s: %w", cfg.BucketPrefix+leasesSuffix, err)
	}

	epochs, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: cfg.BucketPrefix + epochsSuffix,
		Description: "Crewlet fencing epochs and placement hints; NO TTL — this must survive " +
			"the lease key's expiry or the counter resets",
		Replicas: cfg.Replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("coord/kv: open %s: %w", cfg.BucketPrefix+epochsSuffix, err)
	}

	// Resolved once, here, where nothing is concurrent yet — see the
	// leaseStream field for why it is not read per call.
	status, err := leases.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: read %s status: %w", leases.Bucket(), err)
	}
	bucket, ok := status.(*jetstream.KeyValueBucketStatus)
	if !ok || bucket.StreamInfo() == nil {
		return nil, fmt.Errorf("coord/kv: %s reported no backing stream", leases.Bucket())
	}

	log.Debug("coord_kv_open", "leases", leases.Bucket(), "epochs", epochs.Bucket(), "ttl", cfg.TTL)
	return &Store{
		js:          js,
		leases:      leases,
		epochs:      epochs,
		leaseStream: bucket.StreamInfo().Config.Name,
		ttl:         cfg.TTL,
	}, nil
}

// TTL reports the configured lease TTL — the one every caller must claim with.
func (s *Store) TTL() time.Duration { return s.ttl }

// --- the lease surface ----------------------------------------------------

// TryAcquire claims resource for the owner, or reports that someone else holds
// it.
func (s *Store) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, error) {
	if err := s.validateTTL(resource, opts.Owner, opts.TTL); err != nil {
		return nil, err
	}
	protocol := opts.EffectiveProtocol()
	key := encodeKey(resource)

	for range casAttempts {
		snap, err := s.readForClaim(ctx, resource, opts.Ungated)
		if err != nil {
			return nil, err
		}
		// The gate, fleet-wide: refuse while ANY live lease is held at an
		// older protocol. The disagreement is about what HOLDING A LEASE
		// means, so it is not scoped to the resource being claimed.
		// Asymmetric by construction — it only ever looks for a LOWER
		// protocol, so an older node (which has no such check to run) is
		// never blocked.
		if !opts.Ungated && s.blockedByOlder(snap.all, snap.now, protocol) {
			return nil, nil
		}

		mine := snap.mine
		held := mine != nil && s.held(*mine, snap.now)
		if held && mine.value.Owner != opts.Owner {
			return nil, nil
		}
		if held && mine.value.Epoch == claimingEpoch {
			// One of THIS owner's own concurrent claims holds the record
			// and has not committed its token yet. There is no epoch to
			// keep, and taking it over would fence this owner against
			// itself for no reason. Go round again; the sibling call is
			// one round trip from committing. If it never does — it
			// failed and abandoned the record — the attempts run out and
			// this answers UNKNOWN, which is honest: the record lapses on
			// the bucket's TTL like any other, and the next call takes it.
			continue
		}

		value := leaseValue{
			Resource:  resource,
			Owner:     opts.Owner,
			TTLNanos:  int64(opts.TTL),
			Protocol:  protocol,
			Preferred: opts.Preferred,
			Meta:      opts.Meta,
		}
		if mine != nil {
			// An empty payload keeps what is there — a rule about the
			// PAYLOAD, not about which resource carries it. A renew that
			// forgets to re-send a node's profile must not silently
			// un-label it mid-flight, which peers would read as a node
			// matching no placement at all. The hint follows the same
			// rule: a claim that names no node records the last
			// DELIBERATE placement, not who happens to hold the resource.
			if len(opts.Meta) == 0 {
				value.Meta = mine.value.Meta
			}
			if opts.Preferred == "" {
				value.Preferred = mine.value.Preferred
			}
		}

		if held {
			// An unbroken same-owner hold keeps its epoch: nothing was
			// ever unowned, so the holder's in-flight work stayed
			// covered. This branch is also the renew path — a claim
			// doubles as one.
			value.Epoch = mine.value.Epoch
			if opts.Preferred != "" && opts.Preferred != mine.value.Preferred {
				// The hint moved without the tenure moving. Pin it on
				// the persistent record FIRST, so the epoch bucket is
				// never behind the lease bucket — that ordering is what
				// lets PreferredResources read one bucket.
				if err = s.pinHint(ctx, resource, opts.Preferred); err != nil {
					return nil, err
				}
			}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			data, err := encodeValue(value)
			if err != nil {
				return nil, err
			}
			// Update at the read revision is the fencing CAS: it fails if
			// anything at all wrote the record since we read it.
			if _, err := s.leases.Update(ctx, key, data, mine.revision); err != nil {
				if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
					continue
				}
				return nil, unavailable("renew lease "+resource, err)
			}
			return s.settle(ctx, resource, value, opts, protocol, false)
		}

		// Takeover, or this same owner re-claiming after its own lease
		// lapsed. Both mint a new token, because during the gap the work
		// was covered by nothing and must be fenced against its own past
		// self.
		//
		// Three writes, in this order, and the order is the whole design:
		//
		//  1. WIN THE RECORD in the claiming state — owner set, epoch 0.
		//     This Create/Update is the exclusivity CAS, so exactly one
		//     claimant proceeds. Epoch 0 is not a fencing token and this
		//     call has not returned, so nothing can be written under it.
		//  2. ADVANCE THE COUNTER, uncontended because we hold the record.
		//  3. COMMIT the token into the record we already hold.
		//
		// Advancing the counter FIRST — the obvious order — is wrong twice
		// over. Every loser of the exclusivity CAS would have advanced it,
		// so the first winner of a fleet-wide stampede would hold epoch 32
		// rather than 1; and worse, ownership would briefly exist at a
		// token the persistent counter had not committed, which is the
		// exact state fencing exists to prevent.
		claiming := value
		claiming.Epoch = claimingEpoch
		claimData, err := encodeValue(claiming)
		if err != nil {
			return nil, err
		}
		var rev uint64
		if mine == nil {
			if rev, err = s.leases.Create(ctx, key, claimData); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				return nil, unavailable("create lease "+resource, err)
			}
		} else {
			if rev, err = s.leases.Update(ctx, key, claimData, mine.revision); err != nil {
				if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
					continue
				}
				return nil, unavailable("claim lease "+resource, err)
			}
		}

		// value.Preferred, not opts.Preferred: a claim that names no node
		// carries forward the hint the lapsed record held, and passing that
		// through re-pins it on the persistent record if the two ever drift.
		epoch, hint, err := s.bumpEpoch(ctx, resource, value.Preferred)
		if err != nil {
			// The record is left in the claiming state and expires with
			// the bucket's TTL, exactly as a lease whose owner died does.
			// No token was minted, so nothing is stranded.
			return nil, err
		}
		value.Epoch = epoch
		value.Preferred = hint
		data, err := encodeValue(value)
		if err != nil {
			return nil, err
		}
		if _, err := s.leases.Update(ctx, key, data, rev); err != nil {
			// Our claiming record was taken from us, which needs the
			// record to have lapsed under us. The token we minted becomes
			// a gap in the counter, which is harmless — resets are not.
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return nil, unavailable("commit lease "+resource, err)
		}
		return s.settle(ctx, resource, value, opts, protocol, true)
	}
	return nil, contended("TryAcquire", resource)
}

// settle reads the record back and re-runs the gate.
//
// The read-back is not paranoia: coord.Lease.ExpiresAt must be the STORE's
// deadline, and the write only returns a revision number. Reading the record
// we just wrote is how the server's own timestamp for it reaches the caller.
// On the gated path the same read doubles as the gate's re-check, so the
// degradation described in the package doc costs one round trip, not two.
func (s *Store) settle(
	ctx context.Context,
	resource string,
	want leaseValue,
	opts coord.AcquireOptions,
	protocol int,
	fresh bool,
) (*coord.Lease, error) {
	snap, err := s.readForClaim(ctx, resource, opts.Ungated)
	if err != nil {
		return nil, err
	}
	mine := snap.mine
	if mine == nil || mine.value.Owner != want.Owner || mine.value.Epoch != want.Epoch {
		// Superseded between our write and our read-back. Rare, and only
		// reachable with a TTL short enough to lapse inside one round
		// trip — but the honest answer is the definite one: by the time we
		// looked, it was not ours.
		return nil, nil
	}
	if !opts.Ungated && s.blockedByOlder(snap.all, snap.now, protocol) {
		if fresh {
			// Give the claim straight back. This is the whole difference
			// from Postgres's atomic subquery: the window did not close,
			// it just changed what happens in it.
			if _, err := s.Release(ctx, resource, want.Owner, want.Epoch); err != nil {
				return nil, err
			}
			log.Info("coord_kv_claim_yielded_to_older_peer", "resource", resource,
				"owner", want.Owner, "epoch", want.Epoch, "protocol", protocol)
		}
		// A re-claim of a lease we already held is NOT released: the gate
		// exists to stop a newer node TAKING work beside an older one, and
		// dropping a seat mid-turn would not prevent anything. The refusal
		// still propagates, which is what stops the next sweep.
		return nil, nil
	}
	return mine.lease(), nil
}

// Renew extends a lease the caller still holds at this epoch.
//
// Deliberately NOT gated: it extends a hold this node already has and is
// already acting on, so refusing it during a mixed-version window would drop a
// seat mid-turn rather than prevent anything.
func (s *Store) Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error) {
	if err := s.validateTTL(resource, owner, ttl); err != nil {
		return false, err
	}
	key := encodeKey(resource)

	for range casAttempts {
		e, err := s.readOne(ctx, resource)
		if err != nil {
			return false, err
		}
		if e == nil {
			return false, nil
		}
		now, err := s.nowFor(ctx, *e)
		if err != nil {
			return false, err
		}
		// A lapsed lease is deliberately NOT renewable. Re-acquiring is
		// the only way back and it mints a new epoch, which is what fences
		// the gap the lapse opened. The predicate is owner AND epoch: a
		// zombie heartbeat from before the gap carries the same owner
		// string as the live tenure, and only the epoch tells them apart.
		if e.value.Owner != owner || e.value.Epoch != epoch || !s.tenure(*e, now) {
			return false, nil
		}
		value := e.value
		value.TTLNanos = int64(ttl)
		data, err := encodeValue(value)
		if err != nil {
			return false, err
		}
		if _, err := s.leases.Update(ctx, key, data, e.revision); err != nil {
			// A lost CAS is NOT a lost lease: our own concurrent
			// heartbeat writes the same record. Re-read and re-decide
			// rather than telling the caller to shed every seat.
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return false, unavailable("renew "+resource, err)
		}
		return true, nil
	}
	return false, contended("Renew", resource)
}

// Release gives up a lease, predicated on owner AND epoch.
//
// It EXPIRES THE RECORD IN PLACE by writing a tombstone — a record with an
// empty owner, which every reader treats as unheld — and never deletes the
// key. Deleting would take the epoch record's sibling with it in spirit: the
// counter has to be monotonic for the lifetime of the RESOURCE, not of a
// record. The epochs bucket is not touched here at all, which is what makes
// release safe.
func (s *Store) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if err := validate(resource, owner); err != nil {
		return false, err
	}
	key := encodeKey(resource)

	for range casAttempts {
		e, err := s.readOne(ctx, resource)
		if err != nil {
			return false, err
		}
		if e == nil {
			return false, nil
		}
		now, err := s.nowFor(ctx, *e)
		if err != nil {
			return false, err
		}
		// The predicate is the whole point: an unqualified release lets a
		// departing owner clear its SUCCESSOR's live lease, and a
		// straggler from the previous tenure cleaning up would hand the
		// resource away while the current tenure is mid-turn.
		if e.value.Owner != owner || e.value.Epoch != epoch || !s.tenure(*e, now) {
			return false, nil
		}
		tomb := e.value
		tomb.Owner = ""
		// The tombstone claims the full bucket TTL so no reader needs a
		// clock to judge it — it is unheld because its owner is empty, and
		// the bucket's MaxAge reaps it in its own time. Keeping the hint
		// and the epoch on it means a resource released moments ago still
		// reads with its placement intact.
		tomb.TTLNanos = int64(s.ttl)
		data, err := encodeValue(tomb)
		if err != nil {
			return false, err
		}
		if _, err := s.leases.Update(ctx, key, data, e.revision); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return false, unavailable("release "+resource, err)
		}
		log.Debug("coord_kv_lease_released", "resource", resource, "owner", owner, "epoch", epoch)
		return true, nil
	}
	return false, contended("Release", resource)
}

// Get reads a resource's live lease, or nil when nothing holds it.
//
// A Lease handed out by the store was live when it was read, which is what
// lets a caller act on one without re-checking a deadline against its own wall
// clock. So a lapsed or released record reads as nil, exactly as an unclaimed
// one does.
func (s *Store) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if resource == "" {
		return nil, errNoResource
	}
	e, err := s.readOne(ctx, resource)
	if err != nil || e == nil {
		return nil, err
	}
	now, err := s.nowFor(ctx, *e)
	if err != nil {
		return nil, err
	}
	if !s.tenure(*e, now) {
		return nil, nil
	}
	return e.lease(), nil
}

// ListOwned returns the live leases this owner holds. A drain watches it
// converge to empty, so a lapsed or released lease must not appear.
func (s *Store) ListOwned(ctx context.Context, owner string) ([]coord.Lease, error) {
	return s.listLive(ctx, func(e entry) bool { return e.value.Owner == owner })
}

// ListLive returns live leases under prefix. ListLive(coord.NodePrefix) is the
// membership read: counting live presence leases is how a node learns the
// fleet size it divides the seats by.
func (s *Store) ListLive(ctx context.Context, prefix string) ([]coord.Lease, error) {
	return s.listLive(ctx, func(e entry) bool { return strings.HasPrefix(e.resource, prefix) })
}

func (s *Store) listLive(ctx context.Context, keep func(entry) bool) ([]coord.Lease, error) {
	all, err := s.scanLeases(ctx)
	if err != nil {
		return nil, err
	}
	now, err := s.resolveNow(ctx, all)
	if err != nil {
		return nil, err
	}
	var out []coord.Lease
	for _, e := range all {
		if s.tenure(e, now) && keep(e) {
			out = append(out, *e.lease())
		}
	}
	return out, nil
}

// PreferredResources returns resources under prefix whose hint names nodeID,
// LAPSED ones included — that is the hint's whole purpose. A live-only read
// would answer nothing in exactly the case it exists for: a node coming back
// from a restart looking for the seats whose MCP children and caches it had
// warm.
//
// So it reads the EPOCHS bucket, which has no TTL and therefore still holds a
// hint whose lease key was reaped an hour ago. Scanning the live leases as
// well would be redundant: a live lease's resource necessarily has an epoch
// record (its token was minted from one), and every path that changes a hint
// writes that record BEFORE the lease record, so the epochs bucket is never
// behind.
func (s *Store) PreferredResources(ctx context.Context, prefix, nodeID string) (map[string]struct{}, error) {
	records, err := s.scanResources(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	for _, r := range records {
		if r.Preferred == nodeID && strings.HasPrefix(r.Resource, prefix) {
			out[r.Resource] = struct{}{}
		}
	}
	return out, nil
}

// FleetProtocolFloor returns the lowest protocol among live leases, and
// whether there were any. It is the observability half of the gate: TryAcquire
// can only answer yes or no, so a node stalled behind an older peer would
// otherwise look identical to one whose peers simply hold every seat.
//
// It counts what the GATE counts — held records, including one still in the
// claiming state — rather than what Get returns. The two questions differ, and
// this one exists to explain a refusal: a floor that omitted the very record
// that caused one would send an operator looking for a peer that is not there.
func (s *Store) FleetProtocolFloor(ctx context.Context) (int, bool, error) {
	all, err := s.scanLeases(ctx)
	if err != nil {
		return 0, false, err
	}
	now, err := s.resolveNow(ctx, all)
	if err != nil {
		return 0, false, err
	}
	floor, found := 0, false
	for _, e := range all {
		if !s.held(e, now) {
			continue
		}
		if p := coord.StoredProtocol(e.value.Protocol); !found || p < floor {
			floor, found = p, true
		}
	}
	return floor, found, nil
}

// --- the epochs bucket ----------------------------------------------------

// bumpEpoch CAS-increments a resource's counter and returns the new token
// together with the placement hint that now stands.
//
// Called only by a claimant that already holds the lease record in the claiming
// state, so it is normally uncontended — the CAS loop is here for the one case
// that is not: a claimant whose claiming record lapsed under it while a peer
// took over. The counter is only ever incremented and the record is never
// deleted; gaps are harmless, resets are not.
//
// A claim that names no node leaves the stored hint alone: the hint records the
// last DELIBERATE placement, not who happens to hold the resource. That is also
// how a hint outlives the lease key it was set through — this record has no
// TTL, and the lease bucket's does the reaping.
func (s *Store) bumpEpoch(ctx context.Context, resource, preferred string) (int64, string, error) {
	key := encodeKey(resource)

	for range casAttempts {
		kve, err := s.epochs.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			next := resourceValue{Resource: resource, Epoch: 1, Preferred: preferred}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			data, err := encodeValue(next)
			if err != nil {
				return 0, "", err
			}
			if _, err := s.epochs.Create(ctx, key, data); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				return 0, "", unavailable("mint epoch "+resource, err)
			}
			return next.Epoch, next.Preferred, nil
		}
		if err != nil {
			return 0, "", unavailable("read epoch "+resource, err)
		}

		var cur resourceValue
		if err = json.Unmarshal(kve.Value(), &cur); err != nil {
			// Restarting the counter at 1 would be the one unrecoverable
			// mistake this bucket exists to prevent, so an unreadable
			// record is UNKNOWN and the claim does not proceed.
			return 0, "", fmt.Errorf("%w: decode epoch record for %s: %w",
				coord.ErrUnavailable, resource, err)
		}
		next := resourceValue{Resource: resource, Epoch: cur.Epoch + 1, Preferred: cur.Preferred}
		if preferred != "" {
			next.Preferred = preferred
		}
		data, err := encodeValue(next)
		if err != nil {
			return 0, "", err
		}
		if _, err := s.epochs.Update(ctx, key, data, kve.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return 0, "", unavailable("bump epoch "+resource, err)
		}
		return next.Epoch, next.Preferred, nil
	}
	return 0, "", contended("bumpEpoch", resource)
}

// pinHint records a placement hint without moving the counter — the case where
// a live holder is re-placed mid-tenure.
func (s *Store) pinHint(ctx context.Context, resource, preferred string) error {
	key := encodeKey(resource)

	for range casAttempts {
		kve, err := s.epochs.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Only reachable if the persistent record vanished under a
			// live lease, which nothing in this backend does. The lease
			// record still carries the new hint, so there is nothing to
			// fail the claim over.
			return nil
		}
		if err != nil {
			return unavailable("read epoch "+resource, err)
		}
		var cur resourceValue
		if err = json.Unmarshal(kve.Value(), &cur); err != nil {
			return fmt.Errorf("%w: decode epoch record for %s: %w",
				coord.ErrUnavailable, resource, err)
		}
		if cur.Preferred == preferred {
			return nil
		}
		cur.Preferred = preferred
		cur.Resource = resource
		data, err := encodeValue(cur)
		if err != nil {
			return err
		}
		if _, err := s.epochs.Update(ctx, key, data, kve.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return unavailable("pin hint "+resource, err)
		}
		return nil
	}
	return contended("pinHint", resource)
}

// --- reading --------------------------------------------------------------

// snapshot is what one claim decision reads: every lease record it must judge,
// the one it is about, and the clock those judgements are taken against.
type snapshot struct {
	all  []entry
	mine *entry
	// now is the STORE's clock, or the zero time when nothing in this
	// snapshot needs one. See resolveNow.
	now time.Time
}

// readForClaim gathers what TryAcquire needs.
//
// An UNGATED claim reads one key instead of the whole bucket, and that is not
// a micro-optimisation: node presence is renewed on every heartbeat of every
// node, and scanning the fleet's leases to renew one's own presence would make
// the read cost of a heartbeat grow with the fleet.
func (s *Store) readForClaim(ctx context.Context, resource string, ungated bool) (snapshot, error) {
	if ungated {
		e, err := s.readOne(ctx, resource)
		if err != nil {
			return snapshot{}, err
		}
		if e == nil {
			return snapshot{}, nil
		}
		now, err := s.nowFor(ctx, *e)
		if err != nil {
			return snapshot{}, err
		}
		return snapshot{all: []entry{*e}, mine: e, now: now}, nil
	}

	all, err := s.scanLeases(ctx)
	if err != nil {
		return snapshot{}, err
	}
	now, err := s.resolveNow(ctx, all)
	if err != nil {
		return snapshot{}, err
	}
	snap := snapshot{all: all, now: now}
	for i := range all {
		if all[i].resource == resource {
			snap.mine = &all[i]
			break
		}
	}
	return snap, nil
}

// readOne reads a single lease record. A missing key is (nil, nil).
func (s *Store) readOne(ctx context.Context, resource string) (*entry, error) {
	kve, err := s.leases.Get(ctx, encodeKey(resource))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable("read lease "+resource, err)
	}
	e, ok := decodeEntry(kve)
	if !ok {
		// One resource's record is unreadable. Answering "unheld" would
		// invite a takeover of a lease that may well be live, so this is
		// the third answer: no answer.
		return nil, fmt.Errorf("%w: lease record for %s is undecodable",
			coord.ErrUnavailable, resource)
	}
	return &e, nil
}

// scanLeases reads every lease record in one pass.
//
// One ordered-consumer pass rather than a key listing plus a Get per key: a
// listing that costs a round trip per seat would make every heartbeat's read
// cost grow with the company.
func (s *Store) scanLeases(ctx context.Context) ([]entry, error) {
	byResource := map[string]entry{}
	err := s.eachEntry(ctx, s.leases, func(kve jetstream.KeyValueEntry) {
		e, ok := decodeEntry(kve)
		if !ok {
			// A listing that invented a resource name would put a seat
			// nobody owns into a capacity calculation, so an unreadable
			// record is skipped — loudly.
			log.Warn("coord_kv_undecodable_record", "bucket", s.leases.Bucket(), "key", kve.Key())
			return
		}
		// A write landing mid-listing can report a key twice; the later
		// revision is the record.
		if prev, seen := byResource[e.resource]; seen && prev.revision > e.revision {
			return
		}
		byResource[e.resource] = e
	})
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(byResource))
	for _, e := range byResource {
		out = append(out, e)
	}
	// Sorted because a map iterates in a different order every time, and an
	// unstable listing turns any downstream ordering bug into one that
	// reproduces once in ten runs.
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.resource, b.resource) })
	return out, nil
}

// scanResources reads every persistent resource record.
func (s *Store) scanResources(ctx context.Context) ([]resourceValue, error) {
	byResource := map[string]resourceValue{}
	err := s.eachEntry(ctx, s.epochs, func(kve jetstream.KeyValueEntry) {
		resource, ok := decodeKey(kve.Key())
		if !ok {
			log.Warn("coord_kv_undecodable_key", "bucket", s.epochs.Bucket(), "key", kve.Key())
			return
		}
		var v resourceValue
		if err := json.Unmarshal(kve.Value(), &v); err != nil {
			log.Warn("coord_kv_undecodable_record", "bucket", s.epochs.Bucket(), "key", kve.Key())
			return
		}
		v.Resource = resource
		byResource[resource] = v
	})
	if err != nil {
		return nil, err
	}
	out := make([]resourceValue, 0, len(byResource))
	for _, v := range byResource {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b resourceValue) int { return strings.Compare(a.Resource, b.Resource) })
	return out, nil
}

// eachEntry walks the latest revision of every key in a bucket.
func (s *Store) eachEntry(ctx context.Context, kv jetstream.KeyValue, visit func(jetstream.KeyValueEntry)) error {
	w, err := kv.WatchAll(ctx, jetstream.IgnoreDeletes())
	if err != nil {
		return unavailable("list "+kv.Bucket(), err)
	}
	defer func() { _ = w.Stop() }()

	for {
		select {
		case <-ctx.Done():
			return unavailable("list "+kv.Bucket(), ctx.Err())
		case kve, ok := <-w.Updates():
			if !ok {
				return unavailable("list "+kv.Bucket(), errors.New("listing ended early"))
			}
			// nil marks the end of the initial values.
			if kve == nil {
				return nil
			}
			visit(kve)
		}
	}
}

func decodeEntry(kve jetstream.KeyValueEntry) (entry, bool) {
	resource, ok := decodeKey(kve.Key())
	if !ok {
		return entry{}, false
	}
	var v leaseValue
	if err := json.Unmarshal(kve.Value(), &v); err != nil {
		return entry{}, false
	}
	return entry{
		resource: resource,
		revision: kve.Revision(),
		created:  kve.Created().UTC(),
		value:    v,
	}, true
}

// --- expiry ---------------------------------------------------------------

// held reports whether a record is one a peer must not take: it names an owner
// and its deadline has not passed.
//
// This is the CLAIM-blocking question, and it deliberately includes a record in
// the claiming state (see TryAcquire): a claimant that has won the exclusivity
// CAS and is one round trip from committing its token holds the resource just
// as firmly as a committed tenure does.
func (s *Store) held(e entry, now time.Time) bool {
	if e.value.Owner == "" {
		// A tombstone: Release expired the record in place.
		return false
	}
	// A lease taken at the configured TTL expires by DISAPPEARING — the
	// bucket's MaxAge reaps it — so a record that can still be read is live
	// by construction, and no clock is consulted at all. This is the whole
	// production path.
	if e.value.ttl() >= s.ttl || now.IsZero() {
		return true
	}
	return e.created.Add(e.value.ttl()).After(now)
}

// tenure reports whether a record is a LEASE — held, and carrying a committed
// fencing token.
//
// Only a tenure is ever handed out as a coord.Lease. A record still in the
// claiming state carries epoch 0, which is not a fencing token: returning one
// would give a caller a number every conditional write matches on an unset
// column.
func (s *Store) tenure(e entry, now time.Time) bool {
	return e.value.Epoch != claimingEpoch && s.held(e, now)
}

// resolveNow answers with the STORE's clock, and only when a record in this
// snapshot actually needs one.
//
// Where it comes from matters more than what it is: the leases stream's own
// StreamInfo timestamp, never time.Now. A fleet where each node compares its
// local wall clock to a store-assigned deadline hands two nodes the same seat
// the first time an NTP step separates them.
//
// It is read AFTER the records rather than before, which can in principle make
// a read momentarily conservative — a record renewed in between would be
// judged lapsed. That cannot cost exclusivity, because every takeover is a CAS
// at the revision the record was read at, and a renew in that window changes
// the revision and fails the CAS. The liveness judgement decides what to
// ATTEMPT; the CAS decides what happens.
func (s *Store) resolveNow(ctx context.Context, entries []entry) (time.Time, error) {
	for _, e := range entries {
		if e.value.Owner != "" && e.value.ttl() < s.ttl {
			return s.storeNow(ctx)
		}
	}
	return time.Time{}, nil
}

// nowFor is resolveNow for a single record.
func (s *Store) nowFor(ctx context.Context, e entry) (time.Time, error) {
	if e.value.Owner != "" && e.value.ttl() < s.ttl {
		return s.storeNow(ctx)
	}
	return time.Time{}, nil
}

func (s *Store) storeNow(ctx context.Context) (time.Time, error) {
	// js.Stream is one API request that returns a FRESH handle carrying the
	// info it just fetched, including the server's own timestamp for the
	// reply. See the leaseStream field for why this is not KeyValue.Status.
	stream, err := s.js.Stream(ctx, s.leaseStream)
	if err != nil {
		return time.Time{}, unavailable("read the store clock", err)
	}
	info := stream.CachedInfo()
	if info == nil || info.TimeStamp.IsZero() {
		return time.Time{}, fmt.Errorf("%w: the broker did not report a stream timestamp",
			coord.ErrUnavailable)
	}
	return info.TimeStamp.UTC(), nil
}

// blockedByOlder is the mixed-version gate's predicate.
func (s *Store) blockedByOlder(entries []entry, now time.Time, protocol int) bool {
	for _, e := range entries {
		if s.held(e, now) && coord.StoredProtocol(e.value.Protocol) < protocol {
			return true
		}
	}
	return false
}

// --- errors ---------------------------------------------------------------

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

// validateTTL adds the deadline check for the calls that set one.
func (s *Store) validateTTL(resource, owner string, ttl time.Duration) error {
	if err := validate(resource, owner); err != nil {
		return err
	}
	switch {
	case ttl <= 0:
		// A non-positive TTL would mint a lease that is already lapsed,
		// which reads downstream as a seat nobody can hold.
		return errBadTTL
	case ttl > s.ttl:
		// The bucket's MaxAge would reap the record before this deadline,
		// so honouring it is impossible and reporting it would be a lie
		// about when the lease ends. One TTL per bucket — see the package
		// doc.
		return fmt.Errorf("%w: %v > %v", errTTLTooLong, ttl, s.ttl)
	}
	return nil
}

// unavailable turns a transport failure into the contract's third answer.
func unavailable(what string, err error) error {
	return fmt.Errorf("%w: %s: %w", coord.ErrUnavailable, what, err)
}

// contended reports a compare-and-swap that never settled.
//
// UNKNOWN, not a refusal: losing a CAS repeatedly says nothing about who holds
// the resource, and a caller that read it as "somebody else has it" would shed
// work over contention.
func contended(op, resource string) error {
	return fmt.Errorf("%w: %s(%s) lost every compare-and-swap attempt", coord.ErrUnavailable, op, resource)
}
