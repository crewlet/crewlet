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
// and so [isWrongLastSequence] matches 10071 alone. Every value is held to
// the class its bucket declares for its key where it is opened ([holding]).

package kv

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsinflight"
	"github.com/crewlet/crewlet/internal/objstore"
)

// settledBucket is a bucket handle whose conditional writes the leader decides
// and whose watches are its own ([settledBucket.Watch]). Everything else is
// the client's handle, unchanged.
type settledBucket struct {
	jetstream.KeyValue

	// js is the client the bucket was opened on, whose DefaultTimeout — the
	// request term — a write whose caller set no deadline waits.
	js jetstream.JetStream

	// pace is the slowest the connection may carry a mebibyte of a write,
	// or of a read's reply: the object store's floor across a link, none in
	// process ([Reach]).
	pace time.Duration

	// holding is the bucket's key classes, declared where it is opened: what
	// each key's value may be, and how its reads wait for it.
	holding holding
}

// reply is how long a class's leader reads wait for a reply beyond the read
// term (internal/jsapi's Leader).
type reply int

const (
	// replyTerm is the read term alone: a reply of a few KiB crosses any
	// link inside it ([coord.SmallValueBytes]).
	replyTerm reply = iota
	// replyDeciding adds the reply's crossing at the class's bound and none
	// of its handling (jsapi's Leader.Deciding): the class's reads decide
	// writes a caller waits on.
	replyDeciding
	// replyCarrying adds the reply's arrival, handling and crossing at the
	// class's bound (jsapi's Leader.Carrying): no writer waits on its reads.
	replyCarrying
)

// class is a kind of value a bucket holds: the most one may be and how its
// reads wait.
type class struct {
	bound int
	reply reply
}

// sized is the value size a read or a delivery of the class is given time
// for beyond the term: none for [replyTerm], else its bound.
func (c class) sized() int {
	if c.reply == replyTerm {
		return 0
	}
	return c.bound
}

// keyClass is one class and the keys it holds: a filter in the key's
// vocabulary ("body.*"), never the bucket's subject.
type keyClass struct {
	keys  string
	class class
}

// holding is a bucket's key classes, declared where it is opened: the first
// whose keys match a key is its class, and rest is every other key's. It
// bounds every write ([settledBucket.carriedBack]) and times every read
// ([leaderReader.leaderFor]), pass and watch ([settledBucket.quiet]). Per
// bucket, never per key name: one name means different things in different
// buckets.
type holding struct {
	classes []keyClass
	rest    class
}

// of is key's class.
func (h holding) of(key string) class {
	for _, k := range h.classes {
		if server.SubjectMatchesFilter(key, k.keys) {
			return k.class
		}
	}
	return h.rest
}

// same reports whether h and o declare the same classes in the same order.
func (h holding) same(o holding) bool {
	return h.rest == o.rest && slices.Equal(h.classes, o.classes)
}

// most is the largest class a key under the filter keys may hold: a literal
// key's own, else the largest of rest and every class whose keys a key under
// the filter could match.
func (h holding) most(keys string) class {
	if server.IsValidLiteralSubject(keys) {
		return h.of(keys)
	}
	most := h.rest
	for _, k := range h.classes {
		if server.SubjectsCollide(keys, k.keys) && k.class.sized() > most.sized() {
			most = k.class
		}
	}
	return most
}

var (
	// undeclared is a bucket's that declares no class yet: held to
	// [coord.MaxValueBytes] and read at the term, as its records of a few
	// KiB by their fields allow but nothing holds them to. #1814 declares
	// each bucket's classes and then makes the default SmallValueBytes.
	undeclared = class{bound: coord.MaxValueBytes, reply: replyTerm}
	// runValue is a sandbox run's record or part, budgeted to
	// [coord.MaxValueBytes] (internal/sandbox's RowBytes), whose reads decide
	// the run's changes.
	runValue = class{bound: coord.MaxValueBytes, reply: replyDeciding}
	// pointerClass is the activation pointer, held to [coord.MaxPointerBytes]
	// so every probe's read of it keeps the term.
	pointerClass = class{bound: coord.MaxPointerBytes, reply: replyTerm}
	// bodyClass is the activation's body, the company document sealed, held
	// to [coord.MaxValueBytes]; no writer waits on its reads.
	bodyClass = class{bound: coord.MaxValueBytes, reply: replyCarrying}

	// runsHolding is the runs bucket's: every key a run's value.
	runsHolding = holding{rest: runValue}
	// configHolding is the config bucket's: the pointer, and the bodies it
	// names.
	configHolding = holding{classes: []keyClass{
		{keys: activationKey, class: pointerClass},
		{keys: bodyPrefix + "*", class: bodyClass},
	}, rest: undeclared}
	// undeclaredHolding is every bucket's but the runs, the config, the
	// positions register, the uploads, the chat reads, the secrets, the three
	// atlas buckets, the integrations and the node status.
	undeclaredHolding = holding{rest: undeclared}

	// The positions register's classes, each at the bound its writer holds
	// a record to (ADR-0175, "The positions register declares its classes"):
	// deciding where a read decides a write somebody waits on, carrying where
	// none does, and a small value read at the term.
	mcpListingValue    = class{bound: coord.MaxMCPListingBytes, reply: replyCarrying}
	mcpDefinitionValue = class{bound: coord.MaxMCPDefinitionBytes, reply: replyCarrying}
	mcpCheckValue      = class{bound: coord.MaxMCPCheckBytes, reply: replyDeciding}
	accessReviewValue  = class{bound: coord.MaxAccessReviewBytes, reply: replyDeciding}
	seatAccessValue    = class{bound: coord.MaxSeatAccessBytes, reply: replyDeciding}
	homeBoardValue     = class{bound: coord.MaxHomeBoardRecordBytes, reply: replyDeciding}
	smallValue         = class{bound: coord.SmallValueBytes, reply: replyTerm}

	// positionsHolding is the positions register's: every class above, and
	// the rest undeclared, every class a boot lists among them, so no boot
	// window moves.
	positionsHolding = holding{classes: []keyClass{
		{keys: coord.DocumentFilter(coord.MCPListingClass), class: mcpListingValue},
		{keys: coord.DocumentFilter(coord.MCPDefinitionClass), class: mcpDefinitionValue},
		{keys: coord.DocumentFilter(coord.MCPCheckClass), class: mcpCheckValue},
		{keys: coord.DocumentFilter(coord.AccessReviewClass), class: accessReviewValue},
		{keys: coord.DocumentFilter(coord.SeatAccessClass), class: seatAccessValue},
		{keys: coord.DocumentFilter(coord.HomeBoardClass), class: homeBoardValue},
		{keys: coord.DocumentFilter(coord.AtlasWakeClass), class: smallValue},
		{keys: coord.DocumentFilter(coord.DeviceSignInClass), class: smallValue},
	}, rest: undeclared}

	// The uploads bucket's classes (ADR-0175, "The uploads bucket declares
	// its classes"): every read of each decides a compare-and-set a caller
	// waits on. No other key is written there.
	uploadLedgerValue = class{bound: coord.MaxUploadLedgerBytes, reply: replyDeciding}
	uploadRecordValue = class{bound: coord.MaxUploadRecordBytes, reply: replyDeciding}
	saveLedgerValue   = class{bound: coord.MaxSaveLedgerBytes, reply: replyDeciding}

	// uploadsHolding is the uploads bucket's: the three classes, and the
	// rest undeclared until #1814 makes the default small.
	uploadsHolding = holding{classes: []keyClass{
		{keys: coord.UploadLedgerKey, class: uploadLedgerValue},
		{keys: coord.DocumentFilter(coord.UploadRecordClass), class: uploadRecordValue},
		{keys: coord.DocumentFilter(coord.SaveLedgerClass), class: saveLedgerValue},
	}, rest: undeclared}

	// chatReadValue is a seat's chat read record (ADR-0175, "The chat reads
	// bucket declares its class"), whose every read decides a flush a person
	// waits on.
	chatReadValue = class{bound: coord.MaxChatReadBytes, reply: replyDeciding}

	// chatReadsHolding is the chat reads bucket's: the record, and the rest
	// undeclared until #1814 makes the default small.
	chatReadsHolding = holding{classes: []keyClass{
		{keys: coord.DocumentFilter(coord.ChatReadClass), class: chatReadValue},
	}, rest: undeclared}

	// secretValue is a credential's record (ADR-0175, "The secrets bucket
	// declares its class"), whose reads decide the writes of a put, a
	// create, a rekey and a grant's renewal.
	secretValue = class{bound: coord.MaxSecretRecordBytes, reply: replyDeciding}

	// secretsHolding is the secrets bucket's: every key a credential's
	// record, one per name.
	secretsHolding = holding{rest: secretValue}

	// The atlas buckets' classes (ADR-0175, "The atlas buckets declare their
	// classes"): a source's standing, whose read decides a person's ask for
	// a read, and its misses, read by the collect duty alone, no caller
	// waiting.
	atlasStandingValue = class{bound: coord.MaxAtlasSourceStandingBytes, reply: replyDeciding}
	atlasMissesValue   = class{bound: coord.MaxAtlasSourceMissesBytes, reply: replyCarrying}

	// atlasSourcesHolding is the atlas sources bucket's: the two classes,
	// and the rest undeclared until #1814 makes the default small.
	atlasSourcesHolding = holding{classes: []keyClass{
		{keys: coord.DocumentFilter(coord.AtlasSourceStandingClass), class: atlasStandingValue},
		{keys: coord.DocumentFilter(coord.AtlasSourceMissesClass), class: atlasMissesValue},
	}, rest: undeclared}
	// atlasSmallHolding is the refresh asks' bucket's and the code scans':
	// every key an ask of about 800 bytes, or a standing of at most about
	// 14 KB, each a few counts, instants and a cut error read at the term.
	atlasSmallHolding = holding{rest: smallValue}

	// integrationFindingsValue is a surface's findings (ADR-0175, "The
	// integrations bucket declares its classes"), on which no reader decides
	// a write; its status is a smallValue, read at the term by every reader
	// that decides one, the duty's tick, a turn's atlas and the beat.
	integrationFindingsValue = class{bound: coord.MaxIntegrationFindingsBytes, reply: replyCarrying}

	// integrationsHolding is the integrations bucket's: the status and the
	// findings, and the rest undeclared until #1814 makes the default small.
	integrationsHolding = holding{classes: []keyClass{
		{keys: coord.DocumentFilter(coord.IntegrationStatusClass), class: smallValue},
		{keys: coord.DocumentFilter(coord.IntegrationFindingsClass), class: integrationFindingsValue},
	}, rest: undeclared}

	// nodeStatusRowValue is one row of a node's status (ADR-0175, "The node
	// status bucket declares its classes"), read at the term by every alarm,
	// page and atlas read of the rows.
	nodeStatusRowValue = class{bound: coord.MaxNodeStatusRowBytes, reply: replyTerm}

	// nodeStatusHolding is the node status bucket's: a node's own row, each
	// MCP server's, each model entry's and its growth signals, and the rest,
	// which no writer writes, small.
	nodeStatusHolding = holding{classes: []keyClass{
		{keys: coord.DocumentFilter(string(coord.NodeStatusOwn)), class: nodeStatusRowValue},
		{keys: coord.DocumentFilter(string(coord.NodeStatusMCP)), class: nodeStatusRowValue},
		{keys: coord.DocumentFilter(string(coord.NodeStatusModel)), class: nodeStatusRowValue},
		{keys: coord.DocumentFilter(string(coord.NodeStatusGrowth)), class: nodeStatusRowValue},
	}, rest: smallValue}
)

// quiet is how long a pass or a watch over keys may go without a delivery
// before it is presumed stalled: [passIdle], and the RAW crossing of the
// largest value a key under keys may hold at the bucket's pace (a delivery
// carries a value as stored, never base64), behind the protocol line
// [jsapi.Sending] counts. 185 s for a run's value across a link; 5 s in
// process and for a class read at the term.
func (b settledBucket) quiet(keys string) time.Duration {
	sized := b.holding.most(keys).sized()
	if sized == 0 {
		return passIdle
	}
	return passIdle + jsapi.Sending(sized+kvHeaderBytes, b.pace)
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
// base64-encoded (internal/jsapi's Crossing), at r's pace, none in process;
// coord.MaxValueReadCrossing across a link, which a test holds equal.
// It is exactly what such a read gets beyond the read term ([leaderReader.leaderFor]):
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

// kvHeaderBytes is the most header a write carries beside key and value: a
// removal's operation, a purge's rollup, a conditional write's revision and a
// purge marker's age, each at its longest, and a run's standing at its widest
// ([standingHeader]). The client sets the first four from options this package
// cannot read back, so every write is counted at the most (342 bytes,
// milliseconds at the floor). "KV-Operation" and "PURGE" are the client's
// words, which it does not export.
var kvHeaderBytes = func() int {
	h := nats.Header{
		"KV-Operation":                      {"PURGE"},
		jetstream.MsgRollup:                 {jetstream.MsgRollupSubject},
		jetstream.ExpectedLastSubjSeqHeader: {strconv.FormatUint(math.MaxUint64, 10)},
		jetstream.MsgTTLHeader:              {time.Duration(math.MaxInt64).String()},
	}
	maps.Copy(h, standingHeader(&widestStanding))
	return (&nats.Msg{Header: h}).Size()
}()

// settled wraps a bucket this node has opened, its writes held to the pace
// its connection reaches it at and to the classes h declares. A handle
// already settled with h is returned as it is, so wrapping twice never
// doubles a retry.
func settled(js jetstream.JetStream, kv jetstream.KeyValue, reach Reach, h holding) jetstream.KeyValue {
	// Every bucket declares its classes where it is opened, once; one that
	// does not, or a second declaration the handle would silently ignore, is
	// a wiring mistake no write or read should paper over.
	if h.rest.bound == 0 {
		panic(fmt.Sprintf("coord/kv: %s is settled with no holding declared", kv.Bucket()))
	}
	if b, done := kv.(settledBucket); done {
		if !b.holding.same(h) {
			panic(fmt.Sprintf("coord/kv: %s is settled again with a holding other than its own", kv.Bucket()))
		}
		return kv
	}
	return settledBucket{KeyValue: kv, js: js, pace: reach.pace(), holding: h}
}

// budget is the budget of a write of key and value (none for a removal). A KV
// write takes no publish options, so the client re-sends one nobody heard up to
// [jsapi.WriteSends] times in the ONE context it is handed, each crossing the
// link whole; every call waits the same whole bound ([jsapi.Sends.Whole]), the
// request term plus a crossing per send, so a resend after an in-flight answer
// gets nothing more. On an external broker the client checks no caller's
// deadline behind the connection's lock or in the socket, so a write waits
// behind each large one ahead on its connection (internal/queue/jetstream's
// externalWriteDeadline each); a hold rides its own (ADR-0174).
// No create is budgeted: [createKey], via the settled Update, is the one create;
// the client's Create conditions on a marker the sweep may remove and asks a
// replica (TestNothingCreatesThroughTheBucketHandleOrSweepsThroughTheClient).
func (b settledBucket) budget(key string, value []byte) *jsapi.Sends {
	return jsapi.Budget(b.js, sendCrossing(len(key), len(value), b.pace))
}

// carriedBack refuses a value past the bound of key's class
// ([holding.of]) as [coord.ErrValueTooLarge]: a definite answer, never an
// outage, on every node alike since a value a member wrote is one a leaf
// reads, in the time its class gives. Only a refusal names the bucket, which
// a value that fits never asks the handle for.
func (b settledBucket) carriedBack(key string, value []byte) error {
	bound := b.holding.of(key).bound
	if len(value) <= bound {
		return nil
	}
	return coord.HoldValue(b.Bucket()+"/"+key, len(value), bound)
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
// to what its leader's read carries back ([settledBucket.carriedBack]):
// [settledBucket.update] stating no standing.
func (b settledBucket) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	return b.update(ctx, key, value, revision, nil)
}

// update is every conditional write of a value: ONE publish of the value and
// the standing it states (nil: none) together, conditioned on revision, as the
// client's own Update publishes (nats.go jetstream/kv.go updateRevision), so
// the standing lands at the value's revision or not at all. A STANDING, never
// any header: [kvHeaderBytes] counts its widest, so no message outgrows what a
// leaf's read carries back. The client's Update takes no header, and a second
// path beside it would part the two rules. Its refusal is mapped as the
// client maps it (kv.go mapRevisionMismatch).
func (b settledBucket) update(ctx context.Context, key string, value []byte, revision uint64,
	standing *coord.RunStanding) (uint64, error) {

	if err := b.carriedBack(key, value); err != nil {
		return 0, err
	}
	if !validKey(key) {
		return 0, fmt.Errorf("coord/kv: update %q in %s: %w", key, b.Bucket(), jetstream.ErrInvalidKey)
	}
	subject, err := putSubject(b, key)
	if err != nil {
		return 0, err
	}
	var rev uint64
	err = b.settle(ctx, "update", key, value, func(ctx context.Context) error {
		// A HEADER OF ITS OWN each attempt: the publish stamps its
		// expectation into the header it is handed.
		sent := nats.Header{}
		maps.Copy(sent, standingHeader(standing))
		ack, sendErr := b.js.PublishMsg(ctx, &nats.Msg{Subject: subject, Header: sent, Data: value},
			jetstream.WithExpectLastSequencePerSubject(revision))
		if sendErr != nil {
			return revisionMismatch(sendErr)
		}
		rev = ack.Sequence
		return nil
	})
	return rev, err
}

// revisionMismatch marks a leader's "wrong last sequence" refusal, decided or
// in flight, as [jetstream.ErrKeyRevisionMismatch], the client's own mapping
// of its Update's refusal, keeping the API error for [isWrongLastSequence].
func revisionMismatch(err error) error {
	var api *jetstream.APIError
	if errors.As(err, &api) && (api.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence ||
		api.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequenceConstant) {
		return fmt.Errorf("%w: %w", err, jetstream.ErrKeyRevisionMismatch)
	}
	return err
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
