// How this package READS a coordination bucket, and why no read here is ever
// answered by a replica that may be behind.
//
// # A replicated bucket has several copies and the leader's is the current one
//
// Every write to a bucket — a put, a conditional update, a removal — is a
// publish the stream LEADER arbitrates and acknowledges once a quorum holds
// it. A FOLLOWER learns the write on the leader's next append and applies it
// after that, so for a while after the acknowledgement a follower's copy is the
// bucket as it was before the write. The client's own reads do not ask for the
// leader's copy, in three places:
//
//   - A GET IS A DIRECT GET. nats.go creates every bucket with allow_direct
//     on, and its Get reads through `$JS.API.DIRECT.GET` whenever the stream's
//     cached config says so (jetstream/stream.go, getMsg). Every replica
//     subscribes to that subject in one queue group (server/stream.go,
//     subscribeToDirect) — a follower as soon as it is within 90% of the
//     leader, and it is never unsubscribed while it falls behind
//     (server/jetstream_cluster.go, monitorStream) — and a request from a
//     client goes to a RANDOM member of the group, local or routed
//     (server/client.go, processMsgResults). Measured on three members with
//     the CPU saturated: 5 to 8 of every 100 reads made straight after an
//     acknowledged write returned the revision before it, whichever member
//     the reader was connected to, and a member cut off from the cluster went
//     on answering from whatever it had.
//
//   - A LISTING IS AN ORDERED CONSUMER, and an R1 consumer on a replicated
//     stream is placed on a random active peer
//     (server/jetstream_cluster.go, createGroupForConsumer), which delivers out
//     of ITS OWN copy. Measured: a listing hosted on a follower missed the
//     newest write in up to 3 of every 100 passes, and one hosted on the
//     leader never did.
//
//   - THE CLIENT'S CREATE READS TOO. It steps over a removal's marker by
//     reading the subject through that same direct get (jetstream/kv.go,
//     Create), so a replica still holding the removed record told every racer
//     the key existed, and a create race over a released claim had no winner.
//
// Each of those surfaced as a wrong answer rather than an error — a charge
// counted short, a run just created read back as absent, a reset that missed
// the counter it was resetting, a create race nobody won — which is the worst
// shape under this package's three-valued rule: a definite answer that is
// false. And they are not confined to one bucket. A lease read that misses
// this node's own acquire answers "definitively not held"; a completion that
// misses the write a peer made a moment ago runs the turn twice; the trim
// floor, a MINIMUM over the position rows, rises over a row it cannot see and
// deletes log records a node has not applied.
//
// # What this file does instead
//
// A point read is `$JS.API.STREAM.MSG.GET`, which only the stream leader
// answers (server/jetstream_api.go, jsMsgGetRequest: "only the stream leader
// should answer"); a member that sees no leader answers an error or nothing
// at all, which is the third value rather than a stale one. A create is a
// conditional publish at zero, and a marker it has to step over is read from
// the leader. A listing is the ordered pass followed by one question to the
// leader — is anything in this class past what the pass delivered? — see
// walk.go.
//
// THERE IS NO READ IN THIS PACKAGE THAT A REPLICA ANSWERS, and none is kept
// where a stale answer looked harmless. The cost of asking the leader is one
// hop to whichever member holds it, and every read here is either followed by
// a write conditioned on what it saw (where a stale revision only buys a lost
// compare-and-set and another round) or decides something on its own (where a
// stale answer is a wrong decision). "Harmless" would be a claim about every
// future caller of a read, and the client's handle is held behind an
// interface with no read on it, so the claim never has to be made: a read a
// replica answers does not compile here.
//
// # What a leader read does not rule out
//
// A leader cut off from its quorum believes it leads until nats-server's
// lost-quorum check notices — ten seconds without a quorum, checked every ten
// (server/raft.go) — and answers this read from its own copy meanwhile, while
// the members it lost elect a successor and accept writes. Nothing it is asked
// to WRITE in that window commits. That is why no read here pays a barrier
// append to close it (the state logs' linearizable level does, and says so):
// every read whose answer is acted on irreversibly is followed by a write to
// the same bucket — a compare-and-set at the revision it read, the trim's
// floor published before its purge — and that write is the one thing a former
// leader cannot carry. TestACoordinationReadIsNeverAnsweredByAMemberThatIsBehind
// (internal/queue/jetstream/jetstreamtest) holds a FOLLOWER to never answering
// at all, and holds a former leader to stopping once it has noticed.
//
// # Why the bucket's allow_direct flag is left as it is
//
// Switching direct gets off on the bucket is the obvious fix, and on a fleet
// that already runs it is an outage. A bucket keeps the configuration it was
// created with, and every handle the client binds CACHES that flag: switched
// off under a running node, the server drops every replica's direct-get
// subscription (server/stream.go, update) and every Get through a handle bound
// before the switch fails with "no responders" — measured. A rolling upgrade is
// exactly that: the first upgraded node to flip the flag would take every
// coordination read away from every node still on the previous build. So the
// guarantee is the READER's rather than the bucket's, which is also what makes
// it hold on a bucket an earlier build created, with nobody rewriting it.

package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsinflight"
)

// clientBucket is what this package may call on the client's bucket handle:
// the writes, each a publish the stream leader arbitrates; the ordered pass a
// listing starts from (walk.go makes it complete); and the bucket's name.
//
// NO READ IS ON IT — not Get, not Create, not a key lister — because every one
// of those goes through a direct get any replica may answer. See the file doc.
type clientBucket interface {
	Bucket() string
	Put(ctx context.Context, key string, value []byte) (uint64, error)
	Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error)
	Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error
	Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error
	Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error)
}

// leaderBucket is one coordination bucket as this package uses it: writes go
// through the client, and every read is answered by the stream leader.
type leaderBucket struct {
	client clientBucket

	nc *nats.Conn

	// prefix is the subject a key sits under, "$KV.<bucket>.", read off the
	// stream at open rather than spelled here: it is the stream's subject
	// space that decides what a read by subject finds.
	prefix string

	// msgGet is the leader-only read endpoint for the bucket's stream.
	//
	// UNDER THE DEFAULT API PREFIX because this package's JetStream context
	// is built with none ([Open] and [OpenFleet] both call jetstream.New on
	// the bare connection), so this is the subject that context's own
	// requests travel on.
	msgGet string

	// timeout bounds a read whose caller set no deadline — the context's own
	// DefaultTimeout, which is what the client applies to its reads, so a
	// leader read is held to the same patience the read it replaced was.
	timeout time.Duration
}

// newLeaderBucket wraps a bucket this node has opened, with the facts the open
// read back.
func newLeaderBucket(js jetstream.JetStream, kv jetstream.KeyValue, facts bucketFacts) *leaderBucket {
	return &leaderBucket{
		client:  kv,
		nc:      js.Conn(),
		prefix:  facts.prefix,
		msgGet:  jetstream.DefaultAPIPrefix + "STREAM.MSG.GET." + facts.stream,
		timeout: js.Options().DefaultTimeout,
	}
}

// Bucket names the bucket.
func (b *leaderBucket) Bucket() string { return b.client.Bucket() }

// Put writes a value unconditionally. It names no revision, so the leader has
// nothing to refuse and nothing to settle.
func (b *leaderBucket) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	return b.client.Put(ctx, key, value)
}

// Update writes a value only if the key is still at revision, and is SETTLED:
// see [leaderBucket.settle].
func (b *leaderBucket) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	var rev uint64
	err := b.settle(ctx, "update", key, func(ctx context.Context) error {
		var err error
		rev, err = b.client.Update(ctx, key, value, revision)
		return err
	})
	return rev, err
}

// Delete leaves a delete marker; one conditioned on a revision is SETTLED.
func (b *leaderBucket) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	return b.settle(ctx, "delete", key, func(ctx context.Context) error {
		return b.client.Delete(ctx, key, opts...)
	})
}

// Purge leaves a purge marker and drops the key's history; one conditioned on
// a revision is SETTLED.
func (b *leaderBucket) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	return b.settle(ctx, "purge", key, func(ctx context.Context) error {
		return b.client.Purge(ctx, key, opts...)
	})
}

// settle runs one conditional write until the stream leader DECIDES it —
// [jsinflight.Decide], within the write's own deadline or, when it has none,
// the client's DefaultTimeout.
//
// The leader answers a conditional write with a third answer beside "landed"
// and "refused": another write to the key is still in flight, which decides
// nothing, since the write ahead may be this caller's own and already
// acknowledged. The client reports it as the same revision mismatch as a real
// refusal, and every caller here read that as a lost race: under load a
// removal at the very revision its caller had just read back was refused, so a
// create race over that record had no winner, and a charge spent all sixteen
// of its compare-and-set rounds on refusals no other writer had caused. The
// package doc of internal/jsinflight has the server source it rests on.
//
// A write still undecided at its deadline comes back as [jsinflight.Undecided],
// which carries no mismatch, so it reaches every caller as UNKNOWN rather than
// as a race somebody else won. The op and key name the write in the error.
func (b *leaderBucket) settle(ctx context.Context, op, key string, write func(context.Context) error) error {
	err := jsinflight.Decide(ctx, b.timeout, write)
	var undecided *jsinflight.Undecided
	if errors.As(err, &undecided) {
		return fmt.Errorf("coord/kv: %s %s in %s: %w", op, key, b.Bucket(), err)
	}
	return err
}

// Get reads a key's current value from the stream leader. A key that was
// never written, or whose newest message is a removal's marker, is
// [jetstream.ErrKeyNotFound] — the client's own Get answers both that way.
func (b *leaderBucket) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if !validKey(key) {
		return nil, jetstream.ErrInvalidKey
	}
	e, found, err := b.latest(ctx, key)
	switch {
	case err != nil:
		return nil, err
	case !found || e.op != jetstream.KeyValuePut:
		return nil, jetstream.ErrKeyNotFound
	}
	return e, nil
}

// createAttempts bounds how often [leaderBucket.Create] re-asks.
//
// A round only repeats when the subject CHANGED BETWEEN THIS CALL'S OWN TWO
// STEPS without anybody creating the key — a removal landing over the marker
// it was about to step over, or the bucket's age reaping the last message —
// because a creator that stepped over the marker first ends every other
// racer's loop with a live value to report. Sixteen is the same head count
// [fleetCASRetries] sizes for: far past what any key here is rewritten at,
// and the cost of exhausting it is an error, never an invented answer.
const createAttempts = fleetCASRetries

// Create writes a value only where the key holds none, reporting a key that
// does as [jetstream.ErrKeyExists].
//
// # Why this is not the client's Create
//
// The client publishes at revision zero and, when that is refused, READS the
// subject to see whether what is there is a removal's marker it may step over
// — through the direct get the file doc describes. A replica that had not
// applied the removal answered with the removed record, and every racer was
// told the key existed. The two steps here are the same; the read is the
// leader's.
//
// And every refusal it acts on is a DECIDED one: both publishes are settled
// (see [leaderBucket.settle]), so "a write ahead of this one is still in
// process" is waited out rather than read as a key that exists, and a lost
// step-over is re-read rather than reported — the client handed that second
// refusal back unmapped.
func (b *leaderBucket) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	if !validKey(key) {
		return 0, jetstream.ErrInvalidKey
	}
	revision := uint64(0)
	for range createAttempts {
		// SETTLED, so a refusal here is decided: a write ahead of this one
		// still in process is waited out rather than read as a key that
		// exists.
		rev, err := b.Update(ctx, key, value, revision)
		switch {
		case err == nil:
			return rev, nil
		case !errors.Is(err, jetstream.ErrKeyRevisionMismatch) && !isWrongLastSequence(err):
			return 0, err
		}
		// REFUSED: the subject is not at the revision the publish was
		// conditioned on. Whether that is a live value or a marker is the
		// leader's to say.
		e, found, rerr := b.latest(ctx, key)
		switch {
		case rerr != nil:
			return 0, rerr
		case !found:
			// The bucket's age reaped the subject between the two
			// steps, so nothing is there now: ask again at zero.
			revision = 0
		case e.op == jetstream.KeyValuePut:
			return 0, fmt.Errorf("%w: %w", err, jetstream.ErrKeyExists)
		default:
			// A REMOVAL'S MARKER, which leaves the key absent: the
			// create is a conditional publish on the marker itself.
			revision = e.revision
		}
	}
	return 0, fmt.Errorf("coord/kv: create %s in %s: the key changed under every one of "+
		"%d attempts without anybody creating it", key, b.Bucket(), createAttempts)
}

// latest asks the leader for a key's newest message, whatever it is — a value
// or a removal's marker.
func (b *leaderBucket) latest(ctx context.Context, key string) (*storedEntry, bool, error) {
	m, found, err := b.ask(ctx, msgGetRequest{LastFor: b.prefix + key})
	if err != nil || !found {
		return nil, found, err
	}
	e, err := b.entryOf(m)
	if err != nil {
		return nil, false, err
	}
	return e, true, nil
}

// msgGetRequest is the read the leader is asked: the newest message on a
// subject, or the first at or after a sequence on a filter. The field names
// are the server's (JSApiMsgGetRequest).
type msgGetRequest struct {
	Seq     uint64 `json:"seq,omitempty"`
	LastFor string `json:"last_by_subj,omitempty"`
	NextFor string `json:"next_by_subj,omitempty"`
}

// msgGetResponse is the leader's answer (JSApiMsgGetResponse).
type msgGetResponse struct {
	Error   *jetstream.APIError `json:"error,omitempty"`
	Message *storedMessage      `json:"message,omitempty"`
}

// storedMessage is one message as the server stores it (StoredMsg).
type storedMessage struct {
	Subject  string    `json:"subject"`
	Sequence uint64    `json:"seq"`
	Header   []byte    `json:"hdrs,omitempty"`
	Data     []byte    `json:"data,omitempty"`
	Time     time.Time `json:"time"`
}

// ask sends one read to the stream leader. A message that is not there is
// (nil, false, nil): "nothing here" is the leader's answer, not a failure.
func (b *leaderBucket) ask(ctx context.Context, req msgGetRequest) (*storedMessage, bool, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.timeout)
		defer cancel()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, false, fmt.Errorf("coord/kv: encode a read of %s: %w", b.Bucket(), err)
	}
	reply, err := b.nc.RequestWithContext(ctx, b.msgGet, body)
	if err != nil {
		return nil, false, err
	}
	var resp msgGetResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return nil, false, fmt.Errorf("coord/kv: decode the leader's answer for %s: %w", b.Bucket(), err)
	}
	switch {
	case resp.Error != nil && resp.Error.ErrorCode == jetstream.JSErrCodeMessageNotFound:
		return nil, false, nil
	case resp.Error != nil:
		return nil, false, resp.Error
	case resp.Message == nil:
		return nil, false, fmt.Errorf("coord/kv: the leader answered a read of %s with "+
			"neither a message nor an error", b.Bucket())
	}
	return resp.Message, true, nil
}

// The client's wire spelling of a removal, which is what a stored message
// carries and what [operationOf] reads (nats.go jetstream/kv.go: kvop, kvdel,
// kvpurge). Unexported there, so restated here — and held to the client by
// TestLeaderReadsSeeTheClientsOwnRemovals, which writes each kind through the
// client and reads it back through these.
const (
	kvOperationHeader = "KV-Operation"
	kvOperationDelete = "DEL"
	kvOperationPurge  = "PURGE"
)

// operationOf reads what a message did to its key, in the client's own order:
// an operation header first, then the reason the server stamps on a marker it
// wrote itself.
func operationOf(h nats.Header) jetstream.KeyValueOp {
	if op := h.Get(kvOperationHeader); op != "" {
		switch op {
		case kvOperationDelete:
			return jetstream.KeyValueDelete
		case kvOperationPurge:
			return jetstream.KeyValuePurge
		}
		return jetstream.KeyValuePut
	}
	switch h.Get(jetstream.MarkerReasonHeader) {
	case "MaxAge", "Purge":
		return jetstream.KeyValuePurge
	case "Remove":
		return jetstream.KeyValueDelete
	}
	return jetstream.KeyValuePut
}

// entryOf turns a stored message into the entry a caller reads.
func (b *leaderBucket) entryOf(m *storedMessage) (*storedEntry, error) {
	key, ok := strings.CutPrefix(m.Subject, b.prefix)
	if !ok || key == "" {
		return nil, fmt.Errorf("coord/kv: the leader answered a read of %s with a message "+
			"on %q, outside the bucket's subjects", b.Bucket(), m.Subject)
	}
	op := jetstream.KeyValuePut
	if len(m.Header) > 0 {
		h, err := nats.DecodeHeadersMsg(m.Header)
		if err != nil {
			return nil, fmt.Errorf("coord/kv: decode the headers of %s in %s: %w", key, b.Bucket(), err)
		}
		op = operationOf(h)
	}
	return &storedEntry{
		bucket:   b.Bucket(),
		key:      key,
		value:    m.Data,
		revision: m.Sequence,
		created:  m.Time,
		op:       op,
	}, nil
}

// storedEntry is a message the leader answered with, as the
// [jetstream.KeyValueEntry] every caller in this package already reads.
type storedEntry struct {
	bucket, key string
	value       []byte
	revision    uint64
	created     time.Time
	op          jetstream.KeyValueOp
}

func (e *storedEntry) Bucket() string                  { return e.bucket }
func (e *storedEntry) Key() string                     { return e.key }
func (e *storedEntry) Value() []byte                   { return e.value }
func (e *storedEntry) Revision() uint64                { return e.revision }
func (e *storedEntry) Created() time.Time              { return e.created }
func (e *storedEntry) Operation() jetstream.KeyValueOp { return e.op }

// Delta is how many messages followed this one in a watch, which a read that
// is not a watch has no answer to.
func (e *storedEntry) Delta() uint64 { return 0 }

// The client's key grammar (nats.go jetstream/kv.go: validKeyRe,
// validSearchKeyRe). A read here composes a SUBJECT from a key, so a key that
// carried a wildcard would read the newest message of a whole class and hand
// it back as the key's — the client refused such a key before its own read
// did, and so does this one.
var (
	validKeyRe    = regexp.MustCompile(`^[-/_=\.a-zA-Z0-9]+$`)
	validFilterRe = regexp.MustCompile(`^[-/_=\.a-zA-Z0-9*]*[>]?$`)
)

func validKey(key string) bool {
	return wellDotted(key) && validKeyRe.MatchString(key)
}

func validFilter(keys string) bool {
	return wellDotted(keys) && validFilterRe.MatchString(keys)
}

func wellDotted(s string) bool {
	return s != "" && s[0] != '.' && s[len(s)-1] != '.' && !strings.Contains(s, "..")
}
