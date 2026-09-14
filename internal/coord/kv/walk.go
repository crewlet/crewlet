package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// How this package reads a whole bucket, and why it is ONE REQUEST rather than
// a listing, a watch, or a name list followed by a Get per name.
//
// # The three shapes, in the order they were tried
//
// A key listing plus a Get per name is the obvious one and the worst: N+1
// round trips, and the listing half is not free either, because the client
// implements ListKeys as a watcher. Worse, it CANNOT REPORT A SHORT ANSWER —
// the lister's goroutine ends on a nil entry and a receive from the channel
// its own subscription closes on failure yields exactly that nil, so a
// listing cut off half way came back TRUNCATED WITH A NIL ERROR. That is this
// package's central rule inverted at every caller at once: "held",
// "definitively not held" and "the store could not be reached" are three
// different facts, and for the trim's published floor a short list with no
// error is not a degraded read but a delete of records a node still needs.
//
// [watchWalk] fixed the honesty half — it carries key and value together, and
// only the nil end-of-values marker ends a walk — but it is still a watch,
// and a watch is an ORDERED EPHEMERAL CONSUMER created and destroyed per
// call: two JetStream metadata proposals on a clustered bucket, for a
// question with no consumer in it. Five of this node's fifteen-second duty
// loops read a bucket on every tick and the state-log write fence reads the
// positions register on every first write to a subject, so that is paid
// continuously rather than at the edges.
//
// [directWalk] is the shape with neither cost: one `$JS.API.DIRECT.GET`
// carrying `multi_last`, which asks the broker for the LAST MESSAGE ON EVERY
// SUBJECT of the bucket — which is the latest revision of every key, by
// definition of what a KV bucket is. No consumer, no metadata proposal, no
// second round trip, and the values ride along with the keys.
//
// # Why both survive
//
// Not as a version fallback that will be deleted once everyone upgrades. The
// broker answers a batched read only under conditions that are facts about
// the DEPLOYMENT rather than about this build, and two of them can be true of
// a perfectly current cluster:
//
//   - `multi_last` reached nats-server in 2.11.0 (measured: the field is
//     absent from 2.10.29 and present from 2.11.0, whose JetStream API level
//     is the first one there has ever been). The embedded broker is far past
//     that, but `stream.type: nats` points at somebody else's cluster.
//   - A batched read is served off the stream's DIRECT endpoint, which exists
//     only where `AllowDirect` is set. Every bucket this package creates has
//     it, and a bucket ADOPTED from an older client may not — openBucket
//     deliberately observes an existing bucket rather than rewriting its
//     configuration.
//   - The broker answers at most 1024 SUBJECTS in one batch
//     (maxAllowedResponses, server/stream.go). Past that it refuses the whole
//     request rather than truncating it, and no amount of paging helps: the
//     cap is on the subjects the filter matched, which is resolved before the
//     first message is sent.
//
// So the transport is decided per walk, by the broker's own answer, and
// NOTHING IS CACHED. A remembered "this cluster cannot" would survive the
// rolling upgrade that made it untrue, and a remembered "this bucket is too
// big" would survive the sweep that shrank it. What it costs on a deployment
// that genuinely cannot serve a batched read is one extra request/reply —
// against a fallback that is already paying a consumer create and delete, so
// it is a fraction of what that walk costs anyway.
//
// # The fallback is only ever reachable before the first entry
//
// A walk that has already handed an entry to its caller can never restart on
// the other transport: the caller would see those entries twice. So the three
// "this broker cannot" answers — no responders, a request shape it does not
// understand, and too many subjects — are all raised BEFORE any message is
// sent (established from the server's own code), and directWalk reports them
// as DECLINED only while it has visited nothing. The same answer after an
// entry has been visited is an error, because the walk cannot be completed and
// a partial one must never look like a whole one. Nothing else is ever a
// decline either: once the request has gone out, a broker that stops answering
// is the third fact this package refuses to collapse, not a transport to swap.

const (
	// kvStreamPrefix names the stream behind a bucket. It is the client's
	// own kvBucketNameTmpl, and the name js.KeyValue looks a bucket up by
	// — so deriving it here is exactly as sound as the client's own
	// lookup, and if that ever stops being true the direct endpoint has no
	// responder and the walk takes the other transport rather than lying.
	kvStreamPrefix = "KV_"

	// kvSubjectPrefix is where a bucket's keys live as subjects. A key is
	// a subject token path under it — see coord/keys.go — so the last
	// message on every subject under this prefix is the latest revision of
	// every key.
	kvSubjectPrefix = "$KV."

	// directWalkMaxBytes caps ONE batch of a batched read.
	//
	// Unset, the broker inherits its own max_pending (MAX_PENDING_SIZE, 64
	// MB) — which is the write queue it will hold for this connection
	// before dropping it as a slow consumer, and the client's own default
	// subscription pending-bytes limit is the same 64 MB. Sizing a batch
	// at an eighth of both means one batch can never be the thing that
	// overruns either side, and what does not fit is PAGED rather than
	// dropped.
	//
	// It is also far above every bucket this engine keeps — the largest
	// record is a parked sandbox run's serialized executor loop — so the
	// paging path below is the exception rather than the rule.
	directWalkMaxBytes = 8 << 20

	// directReplyTimeout bounds the wait for each message of a batched
	// read.
	//
	// It matches the client's own default JetStream API timeout
	// (jetstream.defaultAPITimeout), which is what bounds the consumer
	// create on the other transport — so neither walk can hang longer than
	// the other on a broker that has stopped answering, and a caller
	// holding a long-lived context is not the thing deciding that.
	directReplyTimeout = 5 * time.Second
)

// The reply headers a batched read is read out of. The client does not export
// its status pseudo-headers, so they are spelled here; the rest are the
// server's own constants, from the same module the embedded broker is.
const (
	statusHeader      = "Status"
	descriptionHeader = "Description"

	statusEndOfBatch     = "204"
	statusNoResults      = "404"
	statusTooManyResults = "413"
	statusBadRequest     = "408"
	statusRequiredAPI    = "412"
)

// eachEntry walks the latest revision of every LIVE key in a bucket, handing
// each entry — its key AND its value together — to visit.
//
// One request where the broker can answer one, an ordered walk where it
// cannot; the file doc above says which conditions decide that and why
// neither transport is going away. A visit that returns an error stops the
// walk and that error is returned unwrapped, so a caller keeps its own
// vocabulary rather than having this function guess at it. Deletes are
// filtered on both transports, so a tombstoned key is never visited and no
// caller has to recognise one.
func eachEntry(ctx context.Context, nc *nats.Conn, kv jetstream.KeyValue,
	visit func(jetstream.KeyValueEntry) error) error {

	declined, err := directWalk(ctx, nc, kv, directWalkMaxBytes, visit)
	if !declined {
		return err
	}
	return watchWalk(ctx, kv, visit)
}

// directWalk reads a whole bucket with one `multi_last` direct get.
//
// It reports whether this deployment DECLINED the request — an older broker, a
// stream with no direct endpoint, or more subjects than one batch may carry —
// in which case the caller walks the bucket the other way. Declined is true
// only with a nil error and only while nothing has been visited, so the other
// transport always starts from a clean slate and no failure can reach it
// dressed as a capability answer.
//
// maxBytes is a parameter rather than the constant so a test can force the
// paging path with a byte or two instead of eight megabytes of records.
func directWalk(ctx context.Context, nc *nats.Conn, kv jetstream.KeyValue, maxBytes int,
	visit func(jetstream.KeyValueEntry) error) (bool, error) {

	bucket := kv.Bucket()
	what := "read " + bucket
	subject := fmt.Sprintf(server.JSDirectMsgGetT, kvStreamPrefix+bucket)
	prefix := kvSubjectPrefix + bucket + "."

	// SUBSCRIBED BEFORE THE REQUEST GOES OUT, and deliberately not flushed:
	// the SUB and the PUB are written to one connection in this order and
	// the broker reads that connection serially, so interest is registered
	// before the request is processed. A flush here would buy nothing and
	// cost the round trip this whole transport exists to save.
	inbox := nats.NewInbox()
	replies, err := nc.SubscribeSync(inbox)
	if err != nil {
		return false, unavailable(what, err)
	}
	defer func() { _ = replies.Unsubscribe() }()

	var (
		visited bool
		from    uint64 // the sequence this batch starts at
		upTo    uint64 // the sequence the FIRST batch pinned the read to
	)
	// Every exit below this point is the walk's own answer, declined=false:
	// once a request has gone out, anything short of the broker saying it
	// cannot serve one is a failure to report rather than a transport to
	// swap.
	for {
		request, err := json.Marshal(server.JSApiMsgGetRequest{
			MultiLastFor: []string{prefix + ">"},
			Seq:          from,
			UpToSeq:      upTo,
			MaxBytes:     maxBytes,
		})
		if err != nil {
			return false, fmt.Errorf("coord/kv: encode a batched read of %s: %w", bucket, err)
		}
		if err := nc.PublishRequest(subject, inbox, request); err != nil {
			return false, unavailable(what, err)
		}

		batch, err := readBatch(ctx, replies, prefix, bucket, visited, visit)
		switch {
		case err != nil:
			// AN OUTAGE IS NOT A CAPABILITY ANSWER. Only the broker
			// declining the request shape sends a walk to the other
			// transport; a store that stopped answering is the third
			// fact this package refuses to collapse, and retrying it
			// over a consumer would spend a second timeout to
			// rediscover it.
			return false, err
		case batch.declined:
			return true, nil
		}
		visited = visited || batch.visited
		if batch.pending == 0 {
			return false, nil
		}
		// A continuation reads the SAME snapshot: the broker reports the
		// sequence it pinned the first batch to, and every later batch
		// asks for that one rather than for whatever the stream has
		// grown into since.
		next := batch.lastSeq + 1
		if next <= from || batch.upTo == 0 {
			// The broker says there is more and does not say where it
			// starts. Looping on that is an unbounded read of the same
			// batch; reporting a short answer is the lie this package
			// exists to refuse.
			return false, fmt.Errorf("%w: %s: the broker reported %d more record(s) "+
				"and no sequence to continue from", coord.ErrUnavailable, what, batch.pending)
		}
		from, upTo = next, batch.upTo
	}
}

// batchResult is what one request's worth of replies concluded.
type batchResult struct {
	// declined is set by the answers that mean this deployment cannot serve
	// a batched read, and only ever while nothing has been visited.
	declined bool
	visited  bool
	pending  uint64 // records the broker still holds for this read
	lastSeq  uint64 // the sequence of the last record it sent
	upTo     uint64 // the sequence it pinned this read to
}

// readBatch consumes replies up to the end-of-batch marker.
//
// THE MARKER IS THE ONLY THING THAT ENDS A BATCH, for the reason watchWalk
// ends only on the nil entry: a read that stops because the replies stopped
// has not read the bucket, and saying so is the difference between a degraded
// answer and a wrong one.
func readBatch(ctx context.Context, replies *nats.Subscription, prefix, bucket string,
	visitedBefore bool, visit func(jetstream.KeyValueEntry) error) (batchResult, error) {

	what := "read " + bucket
	var out batchResult
	for {
		// Per message rather than per batch: a batch is as long as the
		// bucket, and only the GAP between messages says the broker has
		// stopped answering.
		wait, cancel := context.WithTimeout(ctx, directReplyTimeout)
		msg, err := replies.NextMsgWithContext(wait)
		cancel()
		if err != nil {
			// NOBODY SERVING THE DIRECT ENDPOINT is a decline rather
			// than an outage — the stream is there, it simply has no
			// AllowDirect — and the client raises it here rather than
			// as a status reply, having already turned the broker's
			// 503 into this sentinel.
			if errors.Is(err, nats.ErrNoResponders) && !visitedBefore && !out.visited {
				log.DebugContext(ctx, "coord_kv_batched_read_declined",
					"bucket", bucket, "reason", "no direct endpoint")
				return batchResult{declined: true}, nil
			}
			return out, unavailable(what, err)
		}

		switch status := msg.Header.Get(statusHeader); status {
		case "":
			// A record. Only a status reply carries that header, so an
			// empty one here is the bucket's own data.
			entry, ok, err := decodeDirect(msg, prefix, bucket)
			if err != nil {
				return out, err
			}
			out.lastSeq = entry.revision
			if !ok {
				// A tombstone: filtered here rather than by a
				// broker-side consumer option, which is what the
				// other transport uses, so no caller has to know
				// which walk it got.
				continue
			}
			if err := visit(entry); err != nil {
				out.visited = true
				return out, err
			}
			out.visited = true

		case statusEndOfBatch:
			if out.pending, err = headerNum(msg, server.JSNumPending); err != nil {
				return out, fmt.Errorf("%w: %s: %w", coord.ErrUnavailable, what, err)
			}
			if out.upTo, err = headerNum(msg, server.JSUpToSequence); err != nil {
				return out, fmt.Errorf("%w: %s: %w", coord.ErrUnavailable, what, err)
			}
			// The marker's own last-sequence is authoritative over
			// what this batch happened to send: a batch whose every
			// record was below the starting sequence sends none.
			last, err := headerNum(msg, server.JSLastSequence)
			if err != nil {
				return out, fmt.Errorf("%w: %s: %w", coord.ErrUnavailable, what, err)
			}
			out.lastSeq = max(out.lastSeq, last)
			return out, nil

		case statusNoResults:
			// No subject matched, which for a KV bucket means no keys
			// — an empty bucket, walked cleanly, exactly as the nil
			// end-of-values marker reads on the other transport.
			return out, nil

		case statusBadRequest, statusRequiredAPI, statusTooManyResults:
			// THE THINGS THIS DEPLOYMENT CANNOT DO, and the broker
			// raises every one of them before it sends a record — so
			// while nothing has been visited the walk can still start
			// over on the other transport. Once something has, it
			// cannot: a second walk would hand the caller the same
			// entries again.
			if visitedBefore || out.visited {
				return batchResult{visited: out.visited},
					fmt.Errorf("%w: %s: the broker abandoned a batched read it had "+
						"already begun answering (%s %s)", coord.ErrUnavailable, what,
						status, msg.Header.Get(descriptionHeader))
			}
			log.DebugContext(ctx, "coord_kv_batched_read_declined",
				"bucket", bucket, "status", status,
				"description", msg.Header.Get(descriptionHeader))
			return batchResult{declined: true}, nil

		default:
			return out, fmt.Errorf("%w: %s: the broker answered %s %s",
				coord.ErrUnavailable, what, status, msg.Header.Get(descriptionHeader))
		}
	}
}

// decodeDirect turns one reply into an entry, reporting false for a tombstone.
func decodeDirect(msg *nats.Msg, prefix, bucket string) (directEntry, bool, error) {
	subject := msg.Header.Get(server.JSSubject)
	if !strings.HasPrefix(subject, prefix) || len(subject) == len(prefix) {
		return directEntry{}, false, fmt.Errorf("%w: read %s: the broker answered with a "+
			"record on %q, which is not a key of this bucket", coord.ErrUnavailable, bucket, subject)
	}
	revision, err := headerNum(msg, server.JSSequence)
	if err != nil {
		return directEntry{}, false, fmt.Errorf("%w: read %s: %w", coord.ErrUnavailable, bucket, err)
	}
	stamp := msg.Header.Get(server.JSTimeStamp)
	created, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return directEntry{}, false, fmt.Errorf("%w: read %s: the record at %s carries an "+
			"unreadable timestamp %q: %w", coord.ErrUnavailable, bucket, subject, stamp, err)
	}
	delta, err := headerNum(msg, server.JSNumPending)
	if err != nil {
		return directEntry{}, false, fmt.Errorf("%w: read %s: %w", coord.ErrUnavailable, bucket, err)
	}

	entry := directEntry{
		bucket:   bucket,
		key:      subject[len(prefix):],
		value:    msg.Data,
		revision: revision,
		created:  created.UTC(),
		delta:    delta,
		op:       directOperation(msg),
	}
	return entry, entry.op == jetstream.KeyValuePut, nil
}

// directOperation reads what a record is, in the two spellings the broker
// uses.
//
// The same mapping the client's own watcher applies (nats.go jetstream/kv.go),
// and it has to be: the two transports must agree about which keys are alive,
// or which walk a caller happened to get would change what the bucket
// contains. KV-Operation marks a delete or a purge somebody wrote;
// Nats-Marker-Reason marks one the broker wrote itself when a limit reaped
// the key.
func directOperation(msg *nats.Msg) jetstream.KeyValueOp {
	switch msg.Header.Get(kvOperationHeader) {
	case kvOperationDelete:
		return jetstream.KeyValueDelete
	case kvOperationPurge:
		return jetstream.KeyValuePurge
	}
	switch msg.Header.Get(jetstream.MarkerReasonHeader) {
	case "MaxAge", "Purge":
		return jetstream.KeyValuePurge
	case "Remove":
		return jetstream.KeyValueDelete
	}
	return jetstream.KeyValuePut
}

// The tombstone headers, which the client does not export.
const (
	kvOperationHeader = "KV-Operation"
	kvOperationDelete = "DEL"
	kvOperationPurge  = "PURGE"
)

// headerNum reads one unsigned header, naming the one that was wrong.
//
// An ABSENT header is zero rather than an error: the broker omits
// Nats-Num-Pending on a record it did not send as part of a batch, and zero
// is what that means.
func headerNum(msg *nats.Msg, name string) (uint64, error) {
	raw := msg.Header.Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the broker's %s header is %q, which is not a sequence", name, raw)
	}
	return n, nil
}

// directEntry is a record of a batched read, in the shape every caller of a
// walk already reads.
//
// The same [jetstream.KeyValueEntry] the other transport hands over, so
// nothing above this file can tell which walk answered it — which is the
// point: two transports behind one contract, certified by one suite, is this
// package's own rule for a queue backend applied to its own reads.
type directEntry struct {
	bucket   string
	key      string
	value    []byte
	revision uint64
	created  time.Time
	delta    uint64
	op       jetstream.KeyValueOp
}

var _ jetstream.KeyValueEntry = directEntry{}

func (e directEntry) Bucket() string                  { return e.bucket }
func (e directEntry) Key() string                     { return e.key }
func (e directEntry) Value() []byte                   { return e.value }
func (e directEntry) Revision() uint64                { return e.revision }
func (e directEntry) Created() time.Time              { return e.created }
func (e directEntry) Delta() uint64                   { return e.delta }
func (e directEntry) Operation() jetstream.KeyValueOp { return e.op }

// watchWalk walks a bucket over an ordered ephemeral consumer.
//
// The transport for a broker that cannot answer a batched read — see the file
// doc for the three conditions. It carries the key AND the value together, so
// there is still no Get per name here, and its honesty rule is the one
// [directWalk] copies: the nil entry — and ONLY the nil entry — ends the walk,
// and a CLOSED CHANNEL is a failure named as one.
//
// It also owns the watcher, so there is no early-return path that leaks one —
// the abandoned-listing case the client's blocking 256-entry handoff could
// park a goroutine and a server-side consumer on for ever.
func watchWalk(ctx context.Context, kv jetstream.KeyValue, visit func(jetstream.KeyValueEntry) error) error {
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
			if err := visit(kve); err != nil {
				return err
			}
		}
	}
}
