// How this package WRITES a coordination bucket, under the package doc's
// "Every conditional write is DECIDED by the leader" and "A write across a
// link waits as long as its bytes take to cross". The leader refuses a
// compare-and-set on a comparison (10071) or, while an earlier write to the
// subject is in flight, without one (10164); the client maps both to one
// mismatch (internal/jsinflight's package doc has the server source). So every
// handle is [settled] once, where it is opened ([Open], [OpenFleet],
// [OpenEpochs]), never per call site, which no single in-process server would
// show forgotten: its conditional writes go through [jsinflight.Decide], one
// still undecided ends as [jsinflight.Undecided] (UNKNOWN, never a lost race),
// and so [isWrongLastSequence] matches 10071 alone. Values are held to
// [coord.MaxValueBytes] ([settledBucket.carriedBack]).

package kv

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsinflight"
	"github.com/crewlet/crewlet/internal/objstore"
)

// settledBucket is a bucket handle whose conditional writes the leader decides.
// Everything else is the client's handle, unchanged.
type settledBucket struct {
	jetstream.KeyValue

	// js is the client the bucket was opened on, whose DefaultTimeout — the
	// request term — a write whose caller set no deadline waits.
	js jetstream.JetStream

	// pace is the slowest the connection may carry a mebibyte of a write,
	// or of a read's reply: the object store's floor across a link, none in
	// process ([Reach]).
	pace time.Duration

	// carries is how large a value may be in a bucket [holdingLarge] marks, so
	// each leader read is given that reply's crossing ([newLeaderReader]);
	// none elsewhere, whose replies of a few KiB arrive inside the read term.
	carries int
}

// Reach is how a node's connection reaches the coordination buckets' streams,
// which decides how long a write's bytes are given to cross ([settledBucket]).
// The opener STATES it (the engine, by its broker's kind), since nothing here
// can see how a connection was made, and the zero value is refused: a link
// taken to cross nothing holds a large write to the request term alone and
// fails it as an outage mid-crossing.
type Reach string

const (
	// InProcess is the node's own member, in its own process: a write
	// crosses nothing, and waits the request term alone.
	InProcess Reach = "in_process"

	// AcrossLink is a leaf's link to its members, or a dial to an external
	// cluster: a write crosses the link the node's leases and events ride,
	// which the engine allows to be as slow as the object store's floor.
	AcrossLink Reach = "across_link"
)

// Valid reports whether r is a reach this package knows.
func (r Reach) Valid() bool { return r == InProcess || r == AcrossLink }

// pace is the slowest a connection reaching the buckets as r may carry a
// mebibyte: the object store's floor across a link, none in process.
func (r Reach) pace() time.Duration {
	if r == AcrossLink {
		return objstore.MiBPace
	}
	return 0
}

// valueCrossing is how long a CHANGE of a run's value at [coord.MaxValueBytes]
// takes to cross as r: the leader read its decision is taken on
// ([Reach.readCrossing]) plus [jsapi.WriteSends] [sendCrossing]s of the value
// under [longestRunKey], what [settledBucket.budget] gives such a write beyond
// the request term. None in process; coord.MaxValueCrossing is this across a
// link, and a test holds the two equal.
//
// THE READ TOO: a change is a read-decide-write ([FleetStore.MutateSandboxRun])
// whose reply crosses the same link, a third larger than the value. Built from
// the store's own rules, never a copy, so a send more or a byte of header
// cannot part them silently.
func (r Reach) valueCrossing() time.Duration {
	return r.readCrossing() + jsapi.WriteSends*sendCrossing(longestRunKey, coord.MaxValueBytes, r.pace())
}

// readCrossing is how long a leader's reply to a read of a value at
// [coord.MaxValueBytes] takes to cross as r: the value and [kvHeaderBytes],
// base64-encoded (internal/jsapi's Crossing), at r's pace, none in process.
// It is exactly what such a read gets beyond the read term ([newLeaderReader]):
// the reply's handling is CPU the read term covers off the race detector
// (internal/jsapi's Leader.Deciding).
func (r Reach) readCrossing() time.Duration {
	return jsapi.Crossing(coord.MaxValueBytes+kvHeaderBytes, r.pace())
}

// sendCrossing is how long ONE send of a write of a keyBytes key and a
// valueBytes value takes at pace: both plus [kvHeaderBytes], behind the
// protocol line [jsapi.Sending] counts; none at no pace. The one rule of a
// write's crossing, which [settledBucket.budget] and [Reach.valueCrossing]
// both apply.
func sendCrossing(keyBytes, valueBytes int, pace time.Duration) time.Duration {
	return jsapi.Sending(keyBytes+valueBytes+kvHeaderBytes, pace)
}

// checkReach refuses a configuration that does not say how its connection
// reaches the buckets, naming the field.
func checkReach(config string, r Reach) error {
	if !r.Valid() {
		return fmt.Errorf("coord/kv: %s.Reach %q is not a reach: say whether this node's "+
			"connection reaches the buckets in its own process (%q) or across a link — a "+
			"leaf's, or a dial to an external cluster (%q)", config, r, InProcess, AcrossLink)
	}
	return nil
}

// kvHeaderBytes is the most header a write of the client's KV handle carries
// beside key and value: a removal's operation, a purge's rollup, a conditional
// write's revision and a purge marker's age, each at its longest. The client
// sets them from options this package cannot read back, so every write is
// counted at the most (under 200 bytes, milliseconds at the floor).
// "KV-Operation" and "PURGE" are the client's words, which it does not export.
var kvHeaderBytes = (&nats.Msg{Header: nats.Header{
	"KV-Operation":                      {"PURGE"},
	jetstream.MsgRollup:                 {jetstream.MsgRollupSubject},
	jetstream.ExpectedLastSubjSeqHeader: {strconv.FormatUint(math.MaxUint64, 10)},
	jetstream.MsgTTLHeader:              {time.Duration(math.MaxInt64).String()},
}}).Size()

// settled wraps a bucket this node has opened, its writes held to the pace
// its connection reaches it at. A handle already settled is returned as it
// is, so wrapping twice never doubles a retry.
func settled(js jetstream.JetStream, kv jetstream.KeyValue, reach Reach) jetstream.KeyValue {
	if _, done := kv.(settledBucket); done {
		return kv
	}
	return settledBucket{KeyValue: kv, js: js, pace: reach.pace()}
}

// holdingLarge marks kv, a bucket opened through [settled], as one whose values
// are budgeted to [coord.MaxValueBytes] (a sandbox run's record and parts,
// internal/sandbox's RowBytes): every leader read of its keys (a key's own, a
// listing's certifying read, a create's read) is given the reply's crossing
// ([newLeaderReader]) at the slowest pace a link is allowed, none in process.
//
// THE CROSSING ALONE, never the reply's handling: a request a member dropped
// across an election is asked again two seconds later, inside the shortest
// caller budget (five seconds in process), which counting the handling would
// push past (internal/jsapi's Leader.Deciding). ONLY SUCH A BUCKET: a reply of
// a few KiB crosses any link inside the read term. Declared where the bucket
// is opened ([OpenFleet]), since its writers size its values.
func holdingLarge(kv jetstream.KeyValue) jetstream.KeyValue {
	b, ok := kv.(settledBucket)
	if !ok {
		// Every bucket is settled where it is opened; one that is not is
		// a wiring mistake no read should paper over.
		panic(fmt.Sprintf("coord/kv: holdingLarge is handed %T, not a bucket settled where it was opened", kv))
	}
	b.carries = coord.MaxValueBytes
	return b
}

// budget is the budget of a write of key and value (none for a removal). A KV
// write takes no publish options, so the client re-sends one nobody heard up to
// [jsapi.WriteSends] times in the ONE context it is handed, each crossing the
// link whole; every call waits the same whole bound ([jsapi.Sends.Whole]), the
// request term plus a crossing per send, so a resend after an in-flight answer
// gets nothing more. On an external broker the client checks no caller's
// deadline behind the connection's lock or in the socket, so a write (a lease
// renewal too) waits behind each large one ahead, up to
// internal/queue/jetstream's externalWriteDeadline each (crewlet/crewlet#1035).
// No create is budgeted: [createKey], via the settled Update, is the one create;
// the client's Create conditions on a marker the sweep may remove and asks a
// replica (TestNothingCreatesThroughTheBucketHandleOrSweepsThroughTheClient).
func (b settledBucket) budget(key string, value []byte) *jsapi.Sends {
	return jsapi.Budget(b.js, sendCrossing(len(key), len(value), b.pace))
}

// carriedBack refuses a value past [coord.MaxValueBytes], whose doc has why,
// as [coord.ErrValueTooLarge]: a definite answer, never an outage, on every
// node alike since a value a member wrote is one a leaf reads. Only a refusal
// names the bucket, which a value that fits never asks the handle for.
func (b settledBucket) carriedBack(key string, value []byte) error {
	if len(value) <= coord.MaxValueBytes {
		return nil
	}
	return coord.HoldValue(b.Bucket()+"/"+key, len(value))
}

// Put writes a value whatever the key holds, held to what its leader's read
// carries back ([settledBucket.carriedBack]), waiting as long as its bytes
// take to cross ([settledBucket.budget]).
func (b settledBucket) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if err := b.carriedBack(key, value); err != nil {
		return 0, err
	}
	send, done := b.budget(key, value).Whole(ctx)
	defer done()
	return b.KeyValue.Put(send, key, value)
}

// PutString is [settledBucket.Put] of a string, never the client's own,
// which would put it unbudgeted.
func (b settledBucket) PutString(ctx context.Context, key string, value string) (uint64, error) {
	return b.Put(ctx, key, []byte(value))
}

// Update writes a value only while the key is still at revision, SETTLED, held
// to what its leader's read carries back ([settledBucket.carriedBack]).
func (b settledBucket) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if err := b.carriedBack(key, value); err != nil {
		return 0, err
	}
	var rev uint64
	err := b.settle(ctx, "update", key, value, func(ctx context.Context) error {
		var err error
		rev, err = b.KeyValue.Update(ctx, key, value, revision)
		return err
	})
	return rev, err
}

// Delete leaves a delete marker; one conditioned on a revision is SETTLED.
func (b settledBucket) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	return b.settle(ctx, "delete", key, nil, func(ctx context.Context) error {
		return b.KeyValue.Delete(ctx, key, opts...)
	})
}

// Purge leaves a purge marker and drops the key's history; one conditioned on
// a revision is SETTLED.
func (b settledBucket) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	return b.settle(ctx, "purge", key, nil, func(ctx context.Context) error {
		return b.KeyValue.Purge(ctx, key, opts...)
	})
}

// settle runs one write of key and value (none for a removal) until the
// stream leader DECIDES it, every attempt within the one whole bound
// [settledBucket.budget] fixed at the start. A write naming no revision is
// never answered in flight, so it runs once. op and key name an undecided
// write in its error.
func (b settledBucket) settle(ctx context.Context, op, key string, value []byte,
	write func(context.Context) error) error {

	sends := b.budget(key, value)
	err := jsinflight.Decide(ctx, sends, func(ctx context.Context) error {
		send, done := sends.Whole(ctx)
		defer done()
		return write(send)
	})
	var undecided *jsinflight.Undecided
	if errors.As(err, &undecided) {
		return fmt.Errorf("coord/kv: %s %s in %s: %w", op, key, b.Bucket(), err)
	}
	return err
}
