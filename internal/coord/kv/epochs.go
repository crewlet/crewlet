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
// leased: the crewlet_epochs bucket, with NO TTL, and the placement hints on
// its records (package doc, "Three buckets, and why the epoch needs its own").
// It is a type of its own because the counter outlives the process even on one
// node: a single node's leases are in-process (internal/coord/memory), but the
// rows its tokens are compared with (an executor's owner_epoch) persist, and a
// counter restarted at boot refuses a seat's new holder as OLDER than its own
// parked runs. So [Store] opens it for its claims and the engine hands the same
// bucket to a single node's leases, keeping tokens monotone across a move to a
// fleet. It persists with the stream (stream.store_dir), as the rows do.
type Epochs struct {
	// js is the HOLD client every epoch write and read rides (package doc,
	// "Two clients").
	js jetstream.JetStream
	// kv is the bucket over js, settled (settle.go) like every other this
	// package writes.
	kv jetstream.KeyValue
	// read is the bucket's leader read over js: a counter read off a replica
	// that is behind would mint a token some tenure already holds.
	read *leaderReader
	// list is the bucket over the READ client, for the placement hints a
	// listing scans ([Store.PreferredResources]).
	list jetstream.KeyValue
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
	// Reach is how this node's connection reaches the bucket ([Reach]).
	// Required.
	Reach Reach
}

func (c *EpochsConfig) normalize() error {
	if err := checkReach("EpochsConfig", c.Reach); err != nil {
		return err
	}
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
// leases are not on this broker (see [Epochs]): provisioned over read, its
// counter written over hold ([Open]). Idempotent, and safe to call
// concurrently with [Open] on the same prefix, which opens the same bucket.
func OpenEpochs(ctx context.Context, hold, read jetstream.JetStream, cfg EpochsConfig) (*Epochs, error) {
	if hold == nil || read == nil {
		return nil, errNoJetStream
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return openEpochs(ctx, hold, read, cfg)
}

// openEpochs opens the bucket on a configuration already normalized.
func openEpochs(ctx context.Context, hold, js jetstream.JetStream, cfg EpochsConfig) (*Epochs, error) {
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
	// THE SHARPEST RETENTION IN THIS STORE: an epoch bucket with any age is a
	// counter that resets, handing a resource's next owner a token a zombie of
	// the previous tenure still writes under. A peer's aged bucket is reported,
	// not refused ([reportRetention]), with the remedy that fixes it.
	reportRetention(ctx, bucket.Bucket(), 0, facts)
	held, read, err := bindHold(ctx, hold, bucket, cfg.Reach, epochsHolding)
	if err != nil {
		return nil, err
	}
	return &Epochs{js: hold, kv: held, read: read, list: bucket}, nil
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
// FOR A TOKEN THE COUNTER DID NOT MINT, or minted and lost: a deployment whose
// counter lived in its process left executor rows stamped with tokens this
// bucket never held, and deleting a bucket while the fleet is down (the remedy
// every retention mismatch names) empties this one under rows that keep theirs.
// Either way the seat's next holder would be older than its own rows, and
// refused on every one.
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
			if _, err := createKey(ctx, e.js, e.kv, key, data, nil); err != nil {
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

// advance CAS-increments resource's counter and returns the new token with the
// placement hint that now stands. Its caller already holds the resource in the
// claiming state; the CAS loop is for a claiming record that lapsed while a
// peer took over. The counter only rises and its record is never deleted: gaps
// are harmless, resets are not.
//
// A claim that names no node leaves the hint alone: it records the last
// DELIBERATE placement, not the holder, and outlives the lease key it was set
// through because this record has no TTL.
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
			if _, err := createKey(ctx, e.js, e.kv, key, data, nil); err != nil {
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
			// Restarting the counter at 1 is the one unrecoverable mistake this
			// bucket prevents, so an unreadable record is UNKNOWN: no claim.
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
			// Only if the record vanished under a live lease, which nothing
			// here does; the lease record still carries the hint, so no failure.
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
