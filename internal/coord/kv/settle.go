// How this package WRITES a coordination bucket conditionally, and why no
// conditional write here is ever answered "lost" while the leader is still
// deciding it.
//
// # The leader answers a compare-and-set three ways
//
// A conditional publish — the client's Update, and a Delete or Purge carrying
// a last revision — names the revision its subject must be at. The stream
// leader either lands it, or refuses it on a COMPARISON (10071, which names the
// revision the subject is actually at), or refuses it WITHOUT one (10164,
// "wrong last sequence" and no number): the leader marks a subject in flight
// when it proposes a conditional write and clears the mark only once it has
// applied it AND acknowledged it, so while the mark stands it compares nothing.
// The write ahead may be this caller's own, already confirmed. The client maps
// both refusals to one revision mismatch (nats.go jetstream/kv.go,
// mapRevisionMismatch), and a caller that believed it read the third answer as
// the second: measured on three members under load, a removal at the very
// revision its caller had just read back was refused, which left the create
// race after it with no winner, and a charge spent all sixteen of its
// compare-and-set rounds on refusals no other writer had caused. The package
// doc of internal/jsinflight has the server source it rests on.
//
// # What this file does instead
//
// Every bucket handle this package holds is [settled]: its conditional writes
// go through [jsinflight.Decide], which retries the IDENTICAL publish while the
// answer is in flight, within the write's own deadline or — when it has none —
// the client's DefaultTimeout, the patience the publish it replaces was given.
// A write still undecided at the end comes back as [jsinflight.Undecided],
// which wraps the context error and never the mismatch, so it reaches every
// caller as UNKNOWN rather than as a race somebody else won. That is also why
// [isWrongLastSequence] matches 10071 alone: by the time a caller asks it, the
// in-flight answer has been waited out.
//
// THE HANDLE IS WRAPPED ONCE, where a bucket is opened ([Open], [OpenFleet]),
// rather than at each call site, because a conditional write is a property of
// every write to a bucket and a call site that forgot to settle one is a call
// site nobody would ever see fail on a single server: 10164 is a replicated
// stream's answer.

package kv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsinflight"
)

// settledBucket is a bucket handle whose conditional writes the leader decides.
// Everything else is the client's handle, unchanged.
type settledBucket struct {
	jetstream.KeyValue

	// timeout bounds a write whose caller set no deadline: the client's own
	// DefaultTimeout, which is what it applies to a publish.
	timeout time.Duration
}

// settled wraps a bucket this node has opened. A handle already settled is
// returned as it is, so wrapping twice never doubles a retry.
func settled(js jetstream.JetStream, kv jetstream.KeyValue) jetstream.KeyValue {
	if _, done := kv.(settledBucket); done {
		return kv
	}
	return settledBucket{KeyValue: kv, timeout: js.Options().DefaultTimeout}
}

// Update writes a value only while the key is still at revision, SETTLED.
func (b settledBucket) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	var rev uint64
	err := b.settle(ctx, "update", key, func(ctx context.Context) error {
		var err error
		rev, err = b.KeyValue.Update(ctx, key, value, revision)
		return err
	})
	return rev, err
}

// Delete leaves a delete marker; one conditioned on a revision is SETTLED.
func (b settledBucket) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	return b.settle(ctx, "delete", key, func(ctx context.Context) error {
		return b.KeyValue.Delete(ctx, key, opts...)
	})
}

// Purge leaves a purge marker and drops the key's history; one conditioned on
// a revision is SETTLED.
func (b settledBucket) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	return b.settle(ctx, "purge", key, func(ctx context.Context) error {
		return b.KeyValue.Purge(ctx, key, opts...)
	})
}

// settle runs one write until the stream leader DECIDES it — see the file doc.
// A write that names no revision is never answered in flight, so it runs once.
// The op and key name an undecided write in its error.
func (b settledBucket) settle(ctx context.Context, op, key string, write func(context.Context) error) error {
	err := jsinflight.Decide(ctx, b.timeout, write)
	var undecided *jsinflight.Undecided
	if errors.As(err, &undecided) {
		return fmt.Errorf("coord/kv: %s %s in %s: %w", op, key, b.Bucket(), err)
	}
	return err
}
