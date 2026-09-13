// Package kv is the coord.Backend a fleet runs on: TTL leases with a fencing
// epoch, on NATS JetStream KV.
//
// The single-node company uses the in-memory twin. This is what the same
// engine becomes when a second node joins: the same contract, certified by
// the same suite (internal/coord/coordtest), with the mutual exclusion moved
// from a mutex to the broker's compare-and-swap.
//
// # Three buckets, and why the epoch needs its own
//
// Postgres kept ONE row per resource and expired it in place, so the epoch
// survived release. A KV deletes a key when it expires — and a deleted key
// restarts the counter, handing the next owner a token a zombie from the
// previous tenure is still fencing its writes with. So ownership and the
// fencing token live in different buckets:
//
//   - crewlet_leases, created with KeyValueConfig.TTL = the seat lease TTL.
//     It holds `seat:` and `node:` leases. That TTL is the STREAM's MaxAge,
//     which is the renewable one: every write refreshes the entry's age, so
//     Update at the current revision IS the renew, an unrenewed key expires
//     SERVER-SIDE, and a peer's Create then succeeds. The store's own expiry
//     is the arbiter clock (the role Postgres now() played), and nodes never
//     compare their own wall clocks.
//
//   - crewlet_duties, holding `worker:` leases, the fleet singletons. See
//     "Two lease buckets" below for why they cannot share the first.
//
//   - crewlet_epochs, with NO TTL. One persistent record per resource holding
//     the monotonic counter and the placement hint, for both lease buckets.
//     Measured to survive the lease key's expiry, which is the entire fencing
//     invariant. Gaps in the counter are harmless; resets are not.
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
// MaxAge is a property of the bucket, so a bucket can only honour TTLs up to
// its own age. A per-call TTL LONGER than that is refused: the bucket would
// reap the record early and the deadline handed back would be a lie about when
// the lease ends. A per-call TTL SHORTER than it is honoured by an additional
// deadline carried in the record, judged against the STORE's clock (see
// Store.storeNow), never the caller's.
//
// # Two lease buckets, because seats and duties want opposite TTLs
//
// A seat lease is renewed on the heartbeat, so its TTL is a few heartbeats. A
// duty lease is re-claimed once per tick of the work it guards, and a tick
// runs from ten seconds to an hour, so a duty TTL runs up to
// coord.MaxDutyTTL. One bucket cannot serve both. Its age at the seat TTL
// refused every duty longer than 45 seconds, which is how the retention sweep,
// the integration reconcile, the skill curator and every integration pass
// never ran on any fleet. Its age at the longest duty would put a clock read
// under every seat renew, and a dead node's seats would sit claimable only by
// deadline arithmetic rather than by the broker reaping them.
//
// So the duty bucket's age is coord.MaxDutyTTL and EVERY duty record is judged
// by its own deadline against the duty bucket's clock, while the seat bucket
// keeps the production shape where a record that can still be read is live. A
// duty is claimed per tick rather than per heartbeat, so the clock read costs
// one round trip per tick of each duty, fleet-wide.
//
// THE DUTY BUCKET'S AGE IS ONLY EVER RAISED. Open adopts a bucket that is
// already at least coord.MaxDutyTTL old and raises one that is younger, and
// never writes it lower. The alternative, CreateOrUpdate like every other
// bucket, lets a build with a shorter ceiling shrink the age under a peer that
// holds a longer duty, and the broker then reaps a live duty early. An age
// longer than a duty's TTL costs nothing, because no duty record is judged by
// the age.
//
// # The rolling upgrade across the duty bucket
//
// A build that predates the duty bucket claims duties in crewlet_leases, and
// cannot be taught to look anywhere else. Two builds locking one duty in two
// buckets would both hold it and both run it. So the rule, enforced here:
//
//	A duty claim is refused while any live record in the seat lease bucket
//	was written by a build that predates the duty bucket.
//
// Every record this build writes carries its layout (record.go); a record by an
// older build carries none. An older node renews its presence in that bucket
// for as long as it is alive, so while any older node is up, the newer nodes
// run no duties and the older ones run the duties they can; once the last
// older record lapses, the newer nodes take them in the duty bucket. The older
// build's own duty record lapses in that same bucket, so the two holdings
// never overlap.
//
// The check reads the whole seat lease bucket, once per duty claim, which is
// once per tick of each duty; a gated seat claim already reads it on every
// claim, so this adds no read that grows with anything but the duty count.
//
// The shape is check, claim, RE-CHECK, give back, the same degradation as the
// protocol gate below, because a KV cannot put a predicate over a second
// bucket inside a compare-and-swap. What it cannot close is an older node that
// was INVISIBLE when a newer node claimed (no live record at all, so already
// treated by the fleet as dead) and then comes back and claims the duty in its
// own bucket: the newer holder's next claim is refused and it stops, so the
// overlap is bounded by one tick of the duty. A downgrade across this layout
// needs a full drain, for the reason coord.ProtocolVersion gives.
//
// Reads follow the same rule rather than hiding the older build: Get,
// ListLive and ListOwned report a duty an older node holds in the seat lease
// bucket, so the fleet view shows who is actually running it during the
// upgrade.
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
// older ones were never gated — that is a faithful degradation and a
// deliberate difference, not an oversight. The gate reads both lease buckets,
// because the contract counts every live lease.
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
	errTTLTooLong = fmt.Errorf("coord/kv: ttl exceeds the seat lease bucket's TTL, "+
		"which is the store's Config.TTL: %w", coord.ErrTTLTooLong)
)

const (
	// defaultBucketPrefix yields crewlet_leases, crewlet_duties and
	// crewlet_epochs.
	defaultBucketPrefix = "crewlet"

	leasesSuffix = "_leases"
	dutiesSuffix = "_duties"
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

// dutyBucketAttempts bounds how often Open re-reads the duty bucket after
// losing its creation to a peer.
//
// Each lost round means a peer created the bucket between this node's read and
// its create, and the next read then finds it. Three rounds cover a fleet
// booting at once with room to spare; exhausting them means the bucket keeps
// vanishing between two requests, which is an operator deleting it rather than
// contention, and is reported as a failure to open.
const dutyBucketAttempts = 3

// validBucketName is nats.go's own bucket rule (jetstream/kv.go). Checking the
// prefix here names the problem instead of failing inside bucket creation.
var validBucketName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Config is what a Store needs at construction.
type Config struct {
	// TTL is the seat lease TTL and the leases bucket's MaxAge. Required.
	//
	// It is a property of the BUCKET, not of a call: see the package doc.
	// A per-call seat or presence TTL longer than this is refused; a shorter
	// one is honoured against the store's own clock. Duty TTLs do not depend
	// on it: they are bounded by coord.MaxDutyTTL.
	TTL time.Duration

	// Replicas is the JetStream replica count for all three buckets. Zero
	// means 1. In a real fleet this should be 3: a coordination store with
	// one replica makes the whole company's seat ownership depend on one
	// broker node staying up.
	Replicas int

	// BucketPrefix names the three buckets: "<prefix>_leases",
	// "<prefix>_duties" and "<prefix>_epochs". Empty means "crewlet". Two
	// companies sharing one NATS account are separated by giving them
	// different prefixes; sharing one would make each company's leases gate
	// the other's claims, because the protocol gate is deliberately
	// fleet-wide.
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

// lane is one lease bucket and the rule its records expire by.
type lane struct {
	kv jetstream.KeyValue

	// stream is the name of the stream backing the bucket, resolved once at
	// Open. storeNow needs it because the clock is read through js.Stream,
	// not through KeyValue.Status: Status caches the StreamInfo it fetched
	// onto the shared bucket handle WITHOUT a lock (nats.go 1.53.1,
	// jetstream/stream.go Info), so two goroutines reading the clock at once
	// is a data race in the client. js.Stream hands back a fresh handle per
	// call and shares nothing. It is per bucket rather than per store because
	// a record's timestamp is stamped by ITS stream's leader, and in a
	// cluster two streams may be led by two servers.
	stream string

	// maxTTL is the longest TTL a claim on this bucket may ask for.
	maxTTL time.Duration

	// reapsAtMax reports that the bucket's age IS maxTTL, so a record
	// claimed at maxTTL expires by disappearing and needs no clock. True for
	// the seat lease bucket; false for the duty bucket, whose age is only
	// ever raised and so may exceed maxTTL (see the package doc).
	reapsAtMax bool
}

// Store is the JetStream KV coord.Backend.
type Store struct {
	js jetstream.JetStream

	// leases holds seat and presence leases, and a duty an older build
	// claimed before the duty bucket existed.
	leases *lane
	// duties holds every duty lease this build claims.
	duties *lane
	epochs jetstream.KeyValue

	ttl time.Duration
}

var _ coord.Backend = (*Store)(nil)

// Open creates or adopts the three buckets and returns the backend.
//
// Idempotent, and safe to call concurrently from every node in the fleet:
// creating a bucket that already exists with the same shape is a no-op, and a
// changed seat lease TTL is applied as a stream update. The duty bucket's age
// is only ever raised; see the package doc.
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
		Description: "Crewlet seat and presence leases; the bucket TTL is the lease TTL and its expiry is the arbiter",
		TTL:         cfg.TTL,
		Replicas:    cfg.Replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("coord/kv: open %s: %w", cfg.BucketPrefix+leasesSuffix, err)
	}

	duties, err := openDuties(ctx, js, cfg)
	if err != nil {
		return nil, err
	}

	epochs, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: cfg.BucketPrefix + epochsSuffix,
		Description: "Crewlet fencing epochs and placement hints; NO TTL, this must survive " +
			"the lease key's expiry or the counter resets",
		Replicas: cfg.Replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("coord/kv: open %s: %w", cfg.BucketPrefix+epochsSuffix, err)
	}

	// Resolved once, here, where nothing is concurrent yet. See lane.stream
	// for why it is not read per call.
	leaseLane, err := newLane(ctx, leases, cfg.TTL, true)
	if err != nil {
		return nil, err
	}
	dutyLane, err := newLane(ctx, duties, coord.MaxDutyTTL, false)
	if err != nil {
		return nil, err
	}

	log.DebugContext(ctx, "coord_kv_open", "leases", leases.Bucket(), "duties", duties.Bucket(),
		"epochs", epochs.Bucket(), "ttl", cfg.TTL, "max_duty_ttl", coord.MaxDutyTTL)
	return &Store{
		js:     js,
		leases: leaseLane,
		duties: dutyLane,
		epochs: epochs,
		ttl:    cfg.TTL,
	}, nil
}

// openDuties creates or adopts the duty bucket, raising its age to
// coord.MaxDutyTTL when it is younger and never lowering it.
//
// Replicas are applied as every other bucket's are, so an operator changing
// stream.replicas gets the same answer from all three.
func openDuties(ctx context.Context, js jetstream.JetStream, cfg Config) (jetstream.KeyValue, error) {
	name := cfg.BucketPrefix + dutiesSuffix
	want := jetstream.KeyValueConfig{
		Bucket: name,
		Description: "Crewlet duty leases; every record is judged by its own deadline, and the " +
			"bucket age is only ever raised to cover the longest duty",
		TTL:      coord.MaxDutyTTL,
		Replicas: cfg.Replicas,
	}
	for range dutyBucketAttempts {
		bucket, err := js.KeyValue(ctx, name)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			created, createErr := js.CreateKeyValue(ctx, want)
			if errors.Is(createErr, jetstream.ErrBucketExists) {
				// A peer created it between the read and the create,
				// possibly with an older ceiling. Read what it made.
				continue
			}
			if createErr != nil {
				return nil, fmt.Errorf("coord/kv: create %s: %w", name, createErr)
			}
			return created, nil
		}
		if err != nil {
			return nil, fmt.Errorf("coord/kv: open %s: %w", name, err)
		}
		status, err := bucket.Status(ctx)
		if err != nil {
			return nil, fmt.Errorf("coord/kv: read %s status: %w", name, err)
		}
		info, ok := status.(*jetstream.KeyValueBucketStatus)
		if !ok || info.StreamInfo() == nil {
			return nil, fmt.Errorf("coord/kv: %s reported no backing stream", name)
		}
		// An age of zero is no age at all, which already outlives every
		// duty: raising it would SHORTEN it.
		age := info.TTL()
		ageCovers := age == 0 || age >= coord.MaxDutyTTL
		if ageCovers && info.StreamInfo().Config.Replicas == cfg.Replicas {
			return bucket, nil
		}
		if ageCovers {
			want.TTL = age
		}
		updated, err := js.UpdateKeyValue(ctx, want)
		if err != nil {
			return nil, fmt.Errorf("coord/kv: update %s: %w", name, err)
		}
		if age != want.TTL {
			log.InfoContext(ctx, "coord_kv_duty_bucket_age_raised", "bucket", name,
				"from", age, "to", want.TTL)
		}
		return updated, nil
	}
	return nil, fmt.Errorf("coord/kv: open %s: the bucket was missing on %d consecutive reads "+
		"and already existed on each create; something is deleting it, and every duty lease "+
		"in the fleet goes with it", name, dutyBucketAttempts)
}

// newLane resolves a bucket's backing stream.
func newLane(ctx context.Context, bucket jetstream.KeyValue, maxTTL time.Duration, reapsAtMax bool) (*lane, error) {
	status, err := bucket.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: read %s status: %w", bucket.Bucket(), err)
	}
	info, ok := status.(*jetstream.KeyValueBucketStatus)
	if !ok || info.StreamInfo() == nil {
		return nil, fmt.Errorf("coord/kv: %s reported no backing stream", bucket.Bucket())
	}
	return &lane{
		kv:         bucket,
		stream:     info.StreamInfo().Config.Name,
		maxTTL:     maxTTL,
		reapsAtMax: reapsAtMax,
	}, nil
}

// TTL reports the configured seat lease TTL, the one every seat and presence
// caller must claim with.
func (s *Store) TTL() time.Duration { return s.ttl }

// laneFor is the bucket a resource's lease is written into by this build.
func (s *Store) laneFor(resource string) *lane {
	if coord.IsWorkerResource(resource) {
		return s.duties
	}
	return s.leases
}

// --- the lease surface ----------------------------------------------------

// TryAcquire claims resource for the owner, or reports that someone else holds
// it.
func (s *Store) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, error) {
	l := s.laneFor(resource)
	if err := s.validateTTL(l, resource, opts.Owner, opts.TTL); err != nil {
		return nil, err
	}
	protocol := opts.EffectiveProtocol()
	key := encodeKey(resource)

	for range casAttempts {
		snap, err := s.readForClaim(ctx, l, resource, opts.Ungated)
		if err != nil {
			return nil, err
		}
		// The gate, fleet-wide: refuse while ANY live lease is held at an
		// older protocol. The disagreement is about what HOLDING A LEASE
		// means, so it is not scoped to the resource being claimed.
		// Asymmetric by construction — it only ever looks for a LOWER
		// protocol, so an older node (which has no such check to run) is
		// never blocked.
		if !opts.Ungated {
			blocked, gateErr := s.blockedByOlder(ctx, snap.all, snap.clock, protocol)
			if gateErr != nil {
				return nil, gateErr
			}
			if blocked {
				return nil, nil
			}
		}

		mine := snap.mine
		held := false
		if mine != nil {
			if held, err = s.held(ctx, *mine, snap.clock); err != nil {
				return nil, err
			}
		}
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
			// its TTL like any other, and the next call takes it.
			continue
		}
		if l == s.duties {
			// Checked only once no peer holds the duty here, so a node
			// that lost the duty to a peer pays no scan for it.
			waiting, layoutErr := s.olderLayoutHolds(ctx, snap)
			if layoutErr != nil {
				return nil, layoutErr
			}
			if waiting {
				log.DebugContext(ctx, "coord_kv_duty_waits_for_older_build", "resource", resource,
					"owner", opts.Owner, "detail", "a node of a build that keeps duties in the "+
						"seat lease bucket is still live; the duty stays with it until it lapses")
				return nil, nil
			}
		}

		value := leaseValue{
			Resource:  resource,
			Owner:     opts.Owner,
			TTLNanos:  int64(opts.TTL),
			Protocol:  protocol,
			Preferred: opts.Preferred,
			Meta:      opts.Meta,
			Layout:    layoutDutyLane,
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
			if _, err := l.kv.Update(ctx, key, data, mine.revision); err != nil {
				if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
					continue
				}
				return nil, unavailable("renew lease "+resource, err)
			}
			return s.settle(ctx, l, resource, value, opts, protocol, false)
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
			if rev, err = l.kv.Create(ctx, key, claimData); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				return nil, unavailable("create lease "+resource, err)
			}
		} else {
			if rev, err = l.kv.Update(ctx, key, claimData, mine.revision); err != nil {
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
			// its TTL, exactly as a lease whose owner died does. No token
			// was minted, so nothing is stranded.
			return nil, err
		}
		value.Epoch = epoch
		value.Preferred = hint
		data, err := encodeValue(value)
		if err != nil {
			return nil, err
		}
		if _, err := l.kv.Update(ctx, key, data, rev); err != nil {
			// Our claiming record was taken from us, which needs the
			// record to have lapsed under us. The token we minted becomes
			// a gap in the counter, which is harmless — resets are not.
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return nil, unavailable("commit lease "+resource, err)
		}
		return s.settle(ctx, l, resource, value, opts, protocol, true)
	}
	return nil, contended("TryAcquire", resource)
}

// settle reads the record back and re-runs the gates.
//
// The read-back is not paranoia: coord.Lease.ExpiresAt must be the STORE's
// deadline, and the write only returns a revision number. Reading the record
// we just wrote is how the server's own timestamp for it reaches the caller.
// On the gated path the same read doubles as the protocol gate's re-check, and
// on a duty the layout gate is re-checked too, so each degradation described
// in the package doc costs one read, not two.
func (s *Store) settle(
	ctx context.Context,
	l *lane,
	resource string,
	want leaseValue,
	opts coord.AcquireOptions,
	protocol int,
	fresh bool,
) (*coord.Lease, error) {
	snap, err := s.readForClaim(ctx, l, resource, opts.Ungated)
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
	if !opts.Ungated {
		blocked, err := s.blockedByOlder(ctx, snap.all, snap.clock, protocol)
		if err != nil {
			return nil, err
		}
		if blocked {
			return nil, s.yield(ctx, resource, want, protocol, fresh, "coord_kv_claim_yielded_to_older_peer")
		}
	}
	if l == s.duties {
		waiting, err := s.olderLayoutHolds(ctx, snap)
		if err != nil {
			return nil, err
		}
		if waiting {
			return nil, s.yield(ctx, resource, want, protocol, fresh, "coord_kv_duty_yielded_to_older_build")
		}
	}
	return mine.lease(), nil
}

// yield gives back a claim a gate's re-check refused, when the claim is new.
//
// This is the whole difference from Postgres's atomic subquery: the window did
// not close, it just changed what happens in it.
//
// A re-claim of a lease this owner already held is NOT released: the gates
// exist to stop a newer node TAKING work beside an older one, and dropping a
// seat mid-turn would not prevent anything. The refusal still propagates,
// which is what stops the next sweep, and the next tick of a duty.
func (s *Store) yield(ctx context.Context, resource string, want leaseValue, protocol int, fresh bool, event string) error {
	if !fresh {
		return nil
	}
	if _, err := s.Release(ctx, resource, want.Owner, want.Epoch); err != nil {
		return err
	}
	log.InfoContext(ctx, event, "resource", resource, "owner", want.Owner, "epoch", want.Epoch,
		"protocol", protocol)
	return nil
}

// Renew extends a lease the caller still holds at this epoch.
//
// Deliberately NOT gated: it extends a hold this node already has and is
// already acting on, so refusing it during a mixed-version window would drop a
// seat mid-turn rather than prevent anything.
func (s *Store) Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error) {
	l := s.laneFor(resource)
	if err := s.validateTTL(l, resource, owner, ttl); err != nil {
		return false, err
	}
	key := encodeKey(resource)

	for range casAttempts {
		e, err := s.readOne(ctx, l, resource)
		if err != nil {
			return false, err
		}
		// A lapsed lease is deliberately NOT renewable. Re-acquiring is
		// the only way back and it mints a new epoch, which is what fences
		// the gap the lapse opened. The predicate is owner AND epoch: a
		// zombie heartbeat from before the gap carries the same owner
		// string as the live tenure, and only the epoch tells them apart.
		if e == nil || e.value.Owner != owner || e.value.Epoch != epoch {
			return false, nil
		}
		live, err := s.tenure(ctx, *e, s.newClock())
		if err != nil || !live {
			return false, err
		}
		value := e.value
		value.TTLNanos = int64(ttl)
		value.Layout = layoutDutyLane
		data, err := encodeValue(value)
		if err != nil {
			return false, err
		}
		if _, err := l.kv.Update(ctx, key, data, e.revision); err != nil {
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
	l := s.laneFor(resource)
	key := encodeKey(resource)

	for range casAttempts {
		e, err := s.readOne(ctx, l, resource)
		if err != nil {
			return false, err
		}
		// The predicate is the whole point: an unqualified release lets a
		// departing owner clear its SUCCESSOR's live lease, and a
		// straggler from the previous tenure cleaning up would hand the
		// resource away while the current tenure is mid-turn.
		if e == nil || e.value.Owner != owner || e.value.Epoch != epoch {
			return false, nil
		}
		live, err := s.tenure(ctx, *e, s.newClock())
		if err != nil || !live {
			return false, err
		}
		tomb := e.value
		tomb.Owner = ""
		tomb.Layout = layoutDutyLane
		// The tombstone claims the bucket's full TTL so no reader needs a
		// clock to judge it — it is unheld because its owner is empty, and
		// the bucket's MaxAge reaps it in its own time. Keeping the hint
		// and the epoch on it means a resource released moments ago still
		// reads with its placement intact.
		tomb.TTLNanos = int64(l.maxTTL)
		data, err := encodeValue(tomb)
		if err != nil {
			return false, err
		}
		if _, err := l.kv.Update(ctx, key, data, e.revision); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return false, unavailable("release "+resource, err)
		}
		log.DebugContext(ctx, "coord_kv_lease_released", "resource", resource, "owner", owner, "epoch", epoch)
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
//
// A duty nobody holds in the duty bucket is also looked for in the seat lease
// bucket, where a node of an older build holds it during a rolling upgrade:
// answering nil there would report a duty free that another node is running.
func (s *Store) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if resource == "" {
		return nil, errNoResource
	}
	l := s.laneFor(resource)
	clk := s.newClock()
	lease, err := s.getFrom(ctx, l, resource, clk)
	if err != nil || lease != nil || l != s.duties {
		return lease, err
	}
	return s.getFrom(ctx, s.leases, resource, clk)
}

func (s *Store) getFrom(ctx context.Context, l *lane, resource string, clk *clock) (*coord.Lease, error) {
	e, err := s.readOne(ctx, l, resource)
	if err != nil || e == nil {
		return nil, err
	}
	live, err := s.tenure(ctx, *e, clk)
	if err != nil || !live {
		return nil, err
	}
	return e.lease(), nil
}

// ListOwned returns the live leases this owner holds. A drain watches it
// converge to empty, so a lapsed or released lease must not appear.
func (s *Store) ListOwned(ctx context.Context, owner string) ([]coord.Lease, error) {
	return s.listLive(ctx, []*lane{s.duties, s.leases}, func(e entry) bool { return e.value.Owner == owner })
}

// ListLive returns live leases under prefix. ListLive(coord.NodePrefix) is the
// membership read: counting live presence leases is how a node learns the
// fleet size it divides the seats by.
//
// The duty bucket is read only for a prefix that can name a duty, so the
// membership read on every heartbeat costs what it did before duties had a
// bucket of their own.
func (s *Store) ListLive(ctx context.Context, prefix string) ([]coord.Lease, error) {
	lanes := []*lane{s.leases}
	// A prefix can name a duty when either string starts with the other:
	// "" and "wor" cover every duty, "worker:sched" covers some.
	if n := min(len(prefix), len(coord.WorkerPrefix)); prefix[:n] == coord.WorkerPrefix[:n] {
		lanes = []*lane{s.duties, s.leases}
	}
	return s.listLive(ctx, lanes, func(e entry) bool { return strings.HasPrefix(e.resource, prefix) })
}

// listLive reads the live leases kept by keep across lanes.
//
// Lanes are read in order and a resource already listed is skipped, so the
// duty bucket, read first, wins over an older build's record of the same duty
// in the seat lease bucket. Both can be live only inside the one-tick window
// the package doc describes, and the duty bucket's holder is this build's.
func (s *Store) listLive(ctx context.Context, lanes []*lane, keep func(entry) bool) ([]coord.Lease, error) {
	clk := s.newClock()
	seen := map[string]bool{}
	var out []coord.Lease
	for _, l := range lanes {
		all, err := s.scan(ctx, l)
		if err != nil {
			return nil, err
		}
		for _, e := range all {
			if seen[e.resource] || !keep(e) {
				continue
			}
			live, err := s.tenure(ctx, e, clk)
			if err != nil {
				return nil, err
			}
			if live {
				seen[e.resource] = true
				out = append(out, *e.lease())
			}
		}
	}
	// Sorted because a map iterates in a different order every time, and an
	// unstable listing turns any downstream ordering bug into one that
	// reproduces once in ten runs.
	slices.SortFunc(out, func(a, b coord.Lease) int { return strings.Compare(a.Resource, b.Resource) })
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
// It counts what the GATE counts (held records in both lease buckets,
// including one still in the claiming state) rather than what Get returns.
// The two questions differ, and this one exists to explain a refusal: a floor
// that omitted the very record that caused one would send an operator looking
// for a peer that is not there.
func (s *Store) FleetProtocolFloor(ctx context.Context) (int, bool, error) {
	all, err := s.scanAll(ctx)
	if err != nil {
		return 0, false, err
	}
	clk := s.newClock()
	floor, found := 0, false
	for _, e := range all {
		p := coord.StoredProtocol(e.value.Protocol)
		if found && p >= floor {
			// Cannot lower the floor, so its liveness is not worth a
			// clock read.
			continue
		}
		held, err := s.held(ctx, e, clk)
		if err != nil {
			return 0, false, err
		}
		if held {
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
//
// A duty's counter is the same record whichever lease bucket the duty was
// claimed in, so a duty that moves from an older build's bucket to the duty
// bucket keeps a monotonic epoch across the move.
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
	// scannedLeases reports that all holds every record of the seat lease
	// bucket, so the layout gate can judge them without a second scan.
	scannedLeases bool
	clock         *clock
}

// readForClaim gathers what TryAcquire needs.
//
// An UNGATED claim reads one key instead of the whole of both lease buckets,
// and that is not a micro-optimisation: node presence is renewed on every
// heartbeat of every node, and scanning the fleet's leases to renew one's own
// presence would make the read cost of a heartbeat grow with the fleet.
func (s *Store) readForClaim(ctx context.Context, l *lane, resource string, ungated bool) (snapshot, error) {
	snap := snapshot{clock: s.newClock()}
	if ungated {
		e, err := s.readOne(ctx, l, resource)
		if err != nil {
			return snapshot{}, err
		}
		if e != nil {
			snap.all = []entry{*e}
			snap.mine = &snap.all[0]
		}
		return snap, nil
	}

	all, err := s.scanAll(ctx)
	if err != nil {
		return snapshot{}, err
	}
	snap.all, snap.scannedLeases = all, true
	for i := range all {
		if all[i].lane == l && all[i].resource == resource {
			snap.mine = &snap.all[i]
			break
		}
	}
	return snap, nil
}

// olderLayoutHolds reports whether a record written by a build that predates
// the duty bucket is live in the seat lease bucket, which is the rolling-
// upgrade rule in the package doc.
func (s *Store) olderLayoutHolds(ctx context.Context, snap snapshot) (bool, error) {
	entries := snap.all
	if !snap.scannedLeases {
		scanned, err := s.scan(ctx, s.leases)
		if err != nil {
			return false, err
		}
		entries = scanned
	}
	for _, e := range entries {
		if e.lane != s.leases || e.value.Layout >= layoutDutyLane {
			continue
		}
		held, err := s.held(ctx, e, snap.clock)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

// readOne reads a single lease record from a bucket. A missing key is
// (nil, nil).
func (s *Store) readOne(ctx context.Context, l *lane, resource string) (*entry, error) {
	kve, err := l.kv.Get(ctx, encodeKey(resource))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable("read lease "+resource, err)
	}
	e, ok := decodeEntry(kve, l)
	if !ok {
		// One resource's record is unreadable. Answering "unheld" would
		// invite a takeover of a lease that may well be live, so this is
		// the third answer: no answer.
		return nil, fmt.Errorf("%w: lease record for %s is undecodable",
			coord.ErrUnavailable, resource)
	}
	return &e, nil
}

// scanAll reads every record of both lease buckets.
func (s *Store) scanAll(ctx context.Context) ([]entry, error) {
	leases, err := s.scan(ctx, s.leases)
	if err != nil {
		return nil, err
	}
	duties, err := s.scan(ctx, s.duties)
	if err != nil {
		return nil, err
	}
	return append(leases, duties...), nil
}

// scan reads every lease record of one bucket in one pass.
//
// One ordered-consumer pass rather than a key listing plus a Get per key: a
// listing that costs a round trip per seat would make every heartbeat's read
// cost grow with the company.
func (s *Store) scan(ctx context.Context, l *lane) ([]entry, error) {
	byResource := map[string]entry{}
	err := s.eachEntry(ctx, l.kv, func(kve jetstream.KeyValueEntry) {
		e, ok := decodeEntry(kve, l)
		if !ok {
			// A listing that invented a resource name would put a seat
			// nobody owns into a capacity calculation, so an unreadable
			// record is skipped — loudly.
			log.WarnContext(ctx, "coord_kv_undecodable_record", "bucket", l.kv.Bucket(), "key", kve.Key())
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
			log.WarnContext(ctx, "coord_kv_undecodable_key", "bucket", s.epochs.Bucket(), "key", kve.Key())
			return
		}
		var v resourceValue
		if err := json.Unmarshal(kve.Value(), &v); err != nil {
			log.WarnContext(ctx, "coord_kv_undecodable_record", "bucket", s.epochs.Bucket(), "key", kve.Key())
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

func decodeEntry(kve jetstream.KeyValueEntry, l *lane) (entry, bool) {
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
		lane:     l,
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
func (s *Store) held(ctx context.Context, e entry, clk *clock) (bool, error) {
	if e.value.Owner == "" {
		// A tombstone: Release expired the record in place.
		return false, nil
	}
	// A seat lease taken at the configured TTL expires by DISAPPEARING (the
	// bucket's MaxAge reaps it), so a record that can still be read is
	// live by construction, and no clock is consulted at all. This is the
	// whole production path for seats and presence.
	if e.lane.reapsAtMax && e.value.ttl() >= e.lane.maxTTL {
		return true, nil
	}
	now, err := clk.now(ctx, e.lane)
	if err != nil {
		return false, err
	}
	return e.created.Add(e.value.ttl()).After(now), nil
}

// tenure reports whether a record is a LEASE — held, and carrying a committed
// fencing token.
//
// Only a tenure is ever handed out as a coord.Lease. A record still in the
// claiming state carries epoch 0, which is not a fencing token: returning one
// would give a caller a number every conditional write matches on an unset
// column.
func (s *Store) tenure(ctx context.Context, e entry, clk *clock) (bool, error) {
	if e.value.Epoch == claimingEpoch {
		return false, nil
	}
	return s.held(ctx, e, clk)
}

// clock is the store's clock for one decision, read at most once per bucket and
// only when a record actually needs it.
//
// Where it comes from matters more than what it is: a bucket's own StreamInfo
// timestamp, never time.Now. A fleet where each node compares its local wall
// clock to a store-assigned deadline hands two nodes the same seat the first
// time an NTP step separates them.
//
// It is read AFTER the records rather than before, which can in principle make
// a read momentarily conservative — a record renewed in between would be
// judged lapsed. That cannot cost exclusivity, because every takeover is a CAS
// at the revision the record was read at, and a renew in that window changes
// the revision and fails the CAS. The liveness judgement decides what to
// ATTEMPT; the CAS decides what happens.
type clock struct {
	s  *Store
	at map[*lane]time.Time
}

func (s *Store) newClock() *clock { return &clock{s: s} }

func (c *clock) now(ctx context.Context, l *lane) (time.Time, error) {
	if at, ok := c.at[l]; ok {
		return at, nil
	}
	at, err := c.s.storeNow(ctx, l)
	if err != nil {
		return time.Time{}, err
	}
	if c.at == nil {
		c.at = map[*lane]time.Time{}
	}
	c.at[l] = at
	return at, nil
}

func (s *Store) storeNow(ctx context.Context, l *lane) (time.Time, error) {
	// js.Stream is one API request that returns a FRESH handle carrying the
	// info it just fetched, including the server's own timestamp for the
	// reply. See lane.stream for why this is not KeyValue.Status.
	stream, err := s.js.Stream(ctx, l.stream)
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
//
// A record at this protocol or newer cannot block, so only an older one's
// liveness is judged, and a fleet running one build reads no clock for it.
func (s *Store) blockedByOlder(ctx context.Context, entries []entry, clk *clock, protocol int) (bool, error) {
	for _, e := range entries {
		if coord.StoredProtocol(e.value.Protocol) >= protocol {
			continue
		}
		held, err := s.held(ctx, e, clk)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
	}
	return false, nil
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
func (s *Store) validateTTL(l *lane, resource, owner string, ttl time.Duration) error {
	if err := validate(resource, owner); err != nil {
		return err
	}
	if ttl <= 0 {
		// A non-positive TTL would mint a lease that is already lapsed,
		// which reads downstream as a seat nobody can hold.
		return errBadTTL
	}
	if l == s.duties {
		// The duty bucket's ceiling IS the contract's, so the contract's
		// check and its message are the whole answer.
		return coord.CheckDutyTTL(resource, ttl)
	}
	if ttl > l.maxTTL {
		// The bucket's MaxAge would reap the record before this deadline,
		// so honouring it is impossible and reporting it would be a lie
		// about when the lease ends. One TTL per bucket — see the package
		// doc.
		return fmt.Errorf("%w: %v > %v", errTTLTooLong, ttl, l.maxTTL)
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
