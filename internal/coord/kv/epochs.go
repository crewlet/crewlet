package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// Epochs is the fencing counter of every resource the deployment has ever
// leased: the crewlet_epochs bucket, with NO TTL, and the placement hints that
// ride on its records.
//
// # Why it is a type of its own
//
// Because the counter and the lease answer different questions, and on one
// node only one of them is about peers. A lease answers "who runs this seat",
// and a single node has nobody to fence against, so its leases are an
// in-process table (internal/coord/memory) that re-claims everything at boot.
// The COUNTER answers "is this writer older than the one that wrote this row",
// and the rows that question is asked of are the fleet store's (the owner_epoch
// on an executor's row above all), which outlive the process on one node
// exactly as they do on four. A counter kept beside the in-process leases
// restarted at 1 on every boot while those rows still carried the token minted
// before it, so after an ordinary graceful restart the seat's new holder was
// refused as OLDER than its own previous self on every run it had parked: an
// answer to a parked run was never recorded, and a run's resume, completion
// and reap were refused the same way.
//
// So the counter is one thing with one home, opened by the KV lease [Store]
// for its own claims and handed to the in-process leases by the engine on a
// single node. ONE BUCKET for both, by the same name, which is also what
// keeps a deployment's tokens monotone across a move from local coordination
// to a fleet: the fleet's first claim advances the counter the single node
// left, rather than starting one of its own.
//
// Its persistence is the stream's (stream.store_dir), the same as every fleet
// record the token is compared with: a deployment whose broker keeps nothing
// on disk loses the rows and the counter together, and never the one without
// the other.
type Epochs struct {
	js jetstream.JetStream
	// kv is the bucket, settled (settle.go) like every other this package
	// writes.
	kv jetstream.KeyValue
	// read is the bucket's leader read: a counter read off a replica that
	// is behind would mint a token some tenure already holds.
	read *leaderReader
}

// EpochsConfig is what the counter's bucket needs, and the part of [Config]
// the lease buckets share with it.
type EpochsConfig struct {
	// Clustered is whether this node's broker has PEERS; see [Config].
	Clustered bool
	// Replicas is the bucket's JetStream replica count. Zero means 1.
	Replicas int
	// BucketPrefix names the bucket "<prefix>_epochs". Empty means
	// "crewlet". It must be the prefix the deployment's lease store uses,
	// or the two count the same resource twice.
	BucketPrefix string
}

func (c *EpochsConfig) normalize() error {
	if c.BucketPrefix == "" {
		c.BucketPrefix = defaultBucketPrefix
	}
	if c.Replicas == 0 {
		c.Replicas = 1
	}
	switch {
	case c.Replicas < 0 || c.Replicas > maxReplicas:
		return fmt.Errorf("coord/kv: Config.Replicas %d is outside 1..%d", c.Replicas, maxReplicas)
	case !validBucketName.MatchString(c.BucketPrefix + leasesSuffix):
		return fmt.Errorf("coord/kv: Config.BucketPrefix %q is not a valid bucket name "+
			"(letters, digits, '-' and '_' only)", c.BucketPrefix)
	}
	return nil
}

// OpenEpochs creates or adopts the epochs bucket alone, for a node whose
// leases are not on this broker (see [Epochs]). Idempotent, and safe to call
// concurrently with [Open] on the same prefix, which opens the same bucket.
func OpenEpochs(ctx context.Context, js jetstream.JetStream, cfg EpochsConfig) (*Epochs, error) {
	if js == nil {
		return nil, errNoJetStream
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return openEpochs(ctx, js, cfg)
}

// openEpochs opens the bucket on a configuration already normalized.
func openEpochs(ctx context.Context, js jetstream.JetStream, cfg EpochsConfig) (*Epochs, error) {
	name := cfg.BucketPrefix + epochsSuffix
	bucket, facts, err := openBucket(ctx, js, cfg.Clustered, jetstream.KeyValueConfig{
		Bucket: name,
		Description: "Crewlet fencing epochs and placement hints; NO TTL, this must survive " +
			"the lease key's expiry or the counter resets",
		Replicas: cfg.Replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("coord/kv: open %s: %w", name, err)
	}
	// THE SHARPEST RETENTION IN THIS STORE, and until this line nothing
	// compared it: an epoch bucket with an age at all is a fencing counter
	// that resets, which hands the next owner of a resource a token a
	// zombie from the previous tenure is still writing under. A peer that
	// created it with one is reported here rather than refused, for
	// [reportRetention]'s reason (a booting node does not rewrite a bucket),
	// and the remedy it names is the one that fixes this.
	reportRetention(ctx, bucket.Bucket(), 0, facts)
	read, err := newLeaderReader(js, bucket)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: address %s's leader: %w", bucket.Bucket(), err)
	}
	return &Epochs{js: js, kv: settled(js, bucket), read: read}, nil
}

// Next mints resource's next fencing token: strictly above every token the
// counter has handed out for it, on this node or any other, before or after
// any restart. An error is UNKNOWN and mints nothing a caller may use; a
// value the counter took on the way is a gap, and gaps are harmless.
//
// It leaves the placement hint as it stands, which is what a claimant that
// keeps its hints elsewhere (the in-process leases) needs.
func (e *Epochs) Next(ctx context.Context, resource string) (int64, error) {
	epoch, _, err := e.advance(ctx, resource, "")
	return epoch, err
}

// Floor raises resource's counter to at least floor, so the next token it
// mints is above it, and reports whether it moved. It never lowers one.
//
// FOR A TOKEN THE COUNTER DID NOT MINT, or minted and lost: a record that
// carries an epoch from before the counter knew of it. Two things produce one.
// A deployment whose counter lived in its process (local coordination before
// it minted from this bucket) left its executors' rows stamped with tokens
// this bucket never held. And the remedy every retention mismatch names,
// deleting a bucket while the fleet is down so the next boot re-creates it,
// empties this one under rows that keep their tokens. Either way the seat's
// next holder would be older than its own rows, and refused on every one of
// them.
func (e *Epochs) Floor(ctx context.Context, resource string, floor int64) (bool, error) {
	key := encodeResource(resource)

	for range casAttempts {
		kve, err := e.read.live(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			data, err := encodeValue(resourceValue{Resource: resource, Epoch: floor})
			if err != nil {
				return false, err
			}
			if _, err := createKey(ctx, e.js, e.kv, key, data); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				return false, unavailable("floor epoch "+resource, err)
			}
			return true, nil
		}
		if err != nil {
			return false, unavailable("read epoch "+resource, err)
		}
		var cur resourceValue
		if err = json.Unmarshal(kve.Value(), &cur); err != nil {
			return false, fmt.Errorf("%w: decode epoch record for %s: %w",
				coord.ErrUnavailable, resource, err)
		}
		if cur.Epoch >= floor {
			return false, nil
		}
		cur.Resource, cur.Epoch = resource, floor
		data, err := encodeValue(cur)
		if err != nil {
			return false, err
		}
		if _, err := e.kv.Update(ctx, key, data, kve.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return false, unavailable("floor epoch "+resource, err)
		}
		return true, nil
	}
	return false, contended("floor", resource)
}

// advance CAS-increments a resource's counter and returns the new token
// together with the placement hint that now stands.
//
// Called only by a claimant that already holds the resource in the claiming
// state, so it is normally uncontended. The CAS loop is here for the one case
// that is not: a claimant whose claiming record lapsed under it while a peer
// took over. The counter is only ever incremented and the record is never
// deleted; gaps are harmless, resets are not.
//
// A claim that names no node leaves the stored hint alone: the hint records the
// last DELIBERATE placement, not who happens to hold the resource. That is also
// how a hint outlives the lease key it was set through: this record has no
// TTL, and the lease bucket's does the reaping.
func (e *Epochs) advance(ctx context.Context, resource, preferred string) (int64, string, error) {
	key := encodeResource(resource)

	for range casAttempts {
		kve, err := e.read.live(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			next := resourceValue{Resource: resource, Epoch: 1, Preferred: preferred}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			data, err := encodeValue(next)
			if err != nil {
				return 0, "", err
			}
			if _, err := createKey(ctx, e.js, e.kv, key, data); err != nil {
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
		if _, err := e.kv.Update(ctx, key, data, kve.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return 0, "", unavailable("bump epoch "+resource, err)
		}
		return next.Epoch, next.Preferred, nil
	}
	return 0, "", contended("advance", resource)
}

// pin records a placement hint without moving the counter, the case where
// a live holder is re-placed mid-tenure.
func (e *Epochs) pin(ctx context.Context, resource, preferred string) error {
	key := encodeResource(resource)

	for range casAttempts {
		kve, err := e.read.live(ctx, key)
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
		if _, err := e.kv.Update(ctx, key, data, kve.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return unavailable("pin hint "+resource, err)
		}
		return nil
	}
	return contended("pin", resource)
}
