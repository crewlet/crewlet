// How this package WRITES a coordination bucket: how long a write waits for
// its acknowledgement, and why no conditional write here is ever answered
// "lost" while the leader is still deciding it.
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
// answer is in flight, within the write's budget (below). A write still
// undecided at the end comes back as [jsinflight.Undecided], which wraps the
// context error and never the mismatch, so it reaches every caller as UNKNOWN
// rather than as a race somebody else won. That is also why
// [isWrongLastSequence] matches 10071 alone: by the time a caller asks it, the
// in-flight answer has been waited out.
//
// # A write across a link is given the time its bytes take to cross
//
// A write is acknowledged once its last byte is in, and a node that reaches
// the buckets across a LINK — a leaf's link to its members, a dial to an
// external cluster — sends its bytes at the link's pace, which the engine
// allows to be as slow as the object store's floor (internal/objstore's
// MiBPace, thirty seconds a mebibyte). Held to the client's request term
// alone, five seconds, a value larger than about a sixth of a mebibyte could
// not be acknowledged on a link at the floor, and failed as an outage while
// its bytes were still crossing: an MCP listing of up to 896 KiB, which every
// node with seats writes, leaves among them, needs about 26 s
// (crewlet/crewlet#1005's defect, in the coordination store). So every write
// of a settled bucket whose caller set no deadline waits the request term and,
// across a link, the crossing of what it carries — its key, its value and its
// headers — for every time it may be sent, up to internal/jsapi's WriteSends:
// the client re-sends a write nobody heard inside the one context it was
// handed, which no option of a KV write turns off, so each call of the client
// is given the budget's WHOLE bound, the request term and WriteSends
// crossings, fixed when the write starts ([settledBucket.budget]); and an
// attempt after an in-flight answer sends it whole again within that same
// bound, so a write met by in-flight answers for ever ends undecided by it.
// Whether the connection crosses a link is the caller's to state where a
// bucket is opened ([Reach]), because nothing here can see how the
// connection it was handed was made. A caller's own deadline is kept, but on
// an external broker — whose connection the buckets share with the queue — the
// client does not check it while a write waits for the connection's lock,
// which a large write holds for its whole socket Write, nor while the write is
// in the socket: a write here, a lease renewal among them, is held behind every
// large write ahead of it on the connection, each up to the dial's write
// deadline (internal/queue/jetstream's externalWriteDeadline, about eight
// minutes), and then for its own; bounding that needs the write windowed
// (crewlet/crewlet#1035).
//
// # A value is held to what its leader's read carries back
//
// Every read of a key is the stream LEADER's message read (kv.go's "Every
// single-key read is the leader's"), whose reply is JSON with the value
// base64-encoded inside it, a third larger than the value — and the server on
// a leaf's side of its link closes the WHOLE link on any message past its
// max_payload (nats-server v2.15.2, leafnode.go, processLeafMsgArgs →
// maxPayloadViolation), every lease and mailbox the node holds across it cut
// off with it (crewlet/crewlet#1031). A value of more than about six
// mebibytes, which the transport carries whole on its way in, would be one no
// node without `data` could ever read. So every value a settled bucket writes
// is held to the coordination store's own bound, [coord.MaxValueBytes] — the
// largest whose message, with the most header a write carries
// ([kvHeaderBytes]), that reply carries back (internal/jsapi's MaxReadable)
// — refused before it is sent ([settledBucket.carriedBack]), on every node
// alike, since a value a member wrote is one a leaf reads, and as
// [coord.ErrValueTooLarge]: a definite answer, which no caller may read as an
// outage. The values that come near it are the ones whose bound is that bound
// itself — a sandbox run's row and its parts, budgeted field by field to it
// (internal/sandbox's RowBytes), and the activation pointer, which carries the
// company's sealed document (internal/api/configapi holds a document to it
// before storing the revision).
//
// THE HANDLE IS WRAPPED ONCE, where a bucket is opened ([Open], [OpenFleet],
// [OpenEpochs]), rather than at each call site, because how a write is decided
// and how long it waits are properties of every write to a bucket, and a call
// site that forgot either is one nobody would see fail on a single in-process
// server: 10164 is a replicated stream's answer, and a link is a leaf's or an
// external cluster's.

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

	// pace is the slowest the connection may carry a mebibyte of a write:
	// the object store's floor across a link, none in process ([Reach]).
	pace time.Duration
}

// Reach is how a node's connection reaches the coordination buckets' streams,
// which decides how long a write's bytes are given to cross to them
// ([settledBucket]). It is STATED by whoever opens a store, where it knows how
// the connection was made — the engine, by its broker's kind — and REQUIRED:
// the zero value is refused rather than read as either, because a node
// reaching the buckets across a link that was taken to cross nothing holds a
// write of a large value to the request term alone, and fails it as an
// outage while its bytes are still crossing.
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

// pace is the slowest a connection that reaches the buckets as r may carry a
// mebibyte: the object store's floor across a link, the one pace the engine
// allows a link to be as slow as, and none in process.
func (r Reach) pace() time.Duration {
	if r == AcrossLink {
		return objstore.MiBPace
	}
	return 0
}

// valueCrossing is how long a write of a value at what one key holds
// ([coord.MaxValueBytes]) takes to cross a connection that reaches the buckets
// as r, for every send the client may make of it: what the store's own rule
// gives such a write beyond the request term ([settledBucket.budget], every
// one of [jsapi.WriteSends] crossings of its value and the most header a
// write carries, [kvHeaderBytes], behind the request's protocol line, which
// [jsapi.Sending] counts and the key rides), at r's pace — none in process.
// coord.MaxValueCrossing is this across a link, and a test holds it so.
func (r Reach) valueCrossing() time.Duration {
	return jsapi.WriteSends * jsapi.Sending(coord.MaxValueBytes+kvHeaderBytes, r.pace())
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
// beside its key and value, as the client encodes it: the operation a removal
// names, the rollup a purge asks for, the revision a conditional write is
// held to and the age a purge's marker keeps, each at its longest. The client
// writes them from options this package cannot read back, so a write is
// counted at the most any write carries — under two hundred bytes, a few
// milliseconds at the floor. "KV-Operation" and "PURGE" are the client's own
// words for the first, which it does not export.
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

// budget is the budget of a write of key and value — none for a removal —
// made by calls of the client's, each of which may send its message as many
// as internal/jsapi's WriteSends times inside the ONE context it is handed: a
// KV write takes no publish options, so its re-sends of a write nobody heard
// cannot be turned off and made one at a time as the queue's are, and across
// a link to an external cluster the answer that nobody heard it comes after
// it crossed whole, so each re-send crosses again. So each call waits in the
// budget's whole bound ([jsapi.Sends.Whole]), the request term and, across a
// link, a crossing of what it carries for every one of those sends: room a
// write heard the first time never uses, and no longer than a write nobody
// heard can take — and the same bound for every call, so a write sent again
// after an in-flight answer is given nothing more.
//
// NO CREATE IS BUDGETED HERE because none is made through a handle: the
// client's Create conditions on a marker the sweep may remove and asks a
// replica whether the key is one, so [createKey], through the settled
// Update, is this package's one create, and
// TestNothingCreatesThroughTheBucketHandleOrSweepsThroughTheClient holds the
// package to it.
func (b settledBucket) budget(key string, value []byte) *jsapi.Sends {
	return jsapi.Budget(b.js, jsapi.Sending(len(key)+len(value)+kvHeaderBytes, b.pace))
}

// carriedBack refuses value for key past what one key holds
// ([coord.MaxValueBytes]): the largest whose stream leader's read carries it
// back across a leaf's link — see this file's doc. Only a refusal names the
// bucket, which a value that fits never asks the handle for.
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
// stream leader DECIDES it, every attempt waiting in the one whole bound its
// budget fixed at the start ([settledBucket.budget], [jsapi.Sends.Whole]): the
// first attempt's call is given every crossing the write's sends may take, and
// a later one nothing more — see the file doc. A write that names no
// revision is never answered in flight, so it runs once. The op and key name
// an undecided write in its error.
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
