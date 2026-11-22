// How this package reads a whole bucket: ONE ORDERED PASS over the bucket's key
// watcher, carrying each key with its value and filtered at the broker, then
// CERTIFIED against the stream leader's per-subject key index. Never a key
// listing plus a Get per key (a round trip per key on top of a consumer per
// call), and never the batched direct read, which any replica answers. A pass
// cannot tell that it is complete, so every key the index names that it did
// not deliver LIVE is read by itself from the leader ([certify]). The argument
// is the package doc's: "Every listing is ONE CERTIFIED PASS, and never the
// client's ListKeys", "A pass cannot tell that it is complete, so it is
// certified", "A tombstone the pass delivered is not an answer" and "The
// batched direct read, and why it is not here", which a reintroduction of that
// read must answer.

package kv

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// eachEntry hands visit the latest revision of every LIVE key in a bucket — key
// AND value together — once per key, in key order. A visit's error stops the
// walk and is returned unwrapped, so a caller keeps its own vocabulary. Deletes
// are filtered: no caller sees a tombstone.
func eachEntry(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntryUnder(ctx, js, kv, jetstream.AllKeys, kv.Bucket(), visit)
}

// eachEntryUnder is [eachEntry] over the keys matching one filter, which the
// BROKER applies: a key is a subject token path (coord/keys.go), and the
// positions register holds eight classes a whole-bucket walk would move on
// every read of one.
//
// The filter is in the KEY's vocabulary ("floor.>", not "$KV.crewlet_x.floor.>")
// and has no default: an empty filter read as "everything" would decode eight
// classes as one. The index read narrows by the same filter, so both halves of
// a listing answer about the same keys. `what` names the LISTING, not the
// bucket, in "read <what>", since eight classes share one bucket. `js` carries
// both leader reads: the index ([keysUnder]) and each certifying read
// ([leaderReader]).
func eachEntryUnder(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	keys, what string, visit func(jetstream.KeyValueEntry) error) error {

	// Resolved BEFORE the pass, though only a listing that lost a key uses it:
	// a client on an API this engine does not speak would otherwise fail only
	// on the listings that raced a write, reading as a flaky broker.
	read, err := newLeaderReader(js, kv)
	if err != nil {
		return fmt.Errorf("coord/kv: read %s: %w", what, err)
	}
	return listUnder(ctx, js, read, kv, keys, what, false, visit)
}

// listHeadersUnder is [eachEntryUnder] over a pass delivering each key's
// headers alone ([beginPass]): what a key's writer stated of it, never its
// value. A key the pass missed is still read WHOLE by [certify], value and
// headers, from the leader.
func listHeadersUnder(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	keys, what string, visit func(jetstream.KeyValueEntry) error) error {

	read, err := newLeaderReader(js, kv)
	if err != nil {
		return fmt.Errorf("coord/kv: read %s: %w", what, err)
	}
	return listUnder(ctx, js, read, kv, keys, what, true, visit)
}

// listUnder is [eachEntryUnder] with its certifying reads handed in, so a
// test can point them somewhere the index read does not go; headers passes
// over the keys' headers alone ([listHeadersUnder]).
func listUnder(ctx context.Context, js jetstream.JetStream, read *leaderReader, kv jetstream.KeyValue,
	keys, what string, headers bool, visit func(jetstream.KeyValueEntry) error) error {

	p, err := beginPass(ctx, js, kv, keys, what, headers)
	if err != nil {
		return err
	}
	latest, err := p.walk(ctx, what)
	// STOPPED THE MOMENT THE PASS ENDS, before certifying: later writes go into
	// the client's blocking 256-entry handoff, which nothing reads, and would
	// park a goroutine and a server-side consumer on it for ever.
	p.stop()
	if err != nil {
		return err
	}
	if err := certify(ctx, read, what, latest, p.index); err != nil {
		return err
	}
	return visitLive(latest, visit)
}

// watchUnder opens a WATCH over the keys matching one filter and answers its
// first answer CERTIFIED as a listing's is, beside the still-open watcher, so
// the caller reads every later change from the same consumer and no write
// falls between the two. The client's end marker is never waited on: it can
// come early (package doc, "A pass cannot tell that it is complete, so it is
// certified") or never ([passOver]).
//
// The answer keeps markers, so the caller knows the revision of each key it
// holds: the consumer can deliver one again after the answer, and a revision
// at or below the answer's is not a change. The caller owns and stops the
// watcher; on a failure there is none and nothing is left running.
func watchUnder(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	keys, what string) (map[string]jetstream.KeyValueEntry, jetstream.KeyWatcher, error) {

	// Resolved before the pass, for [eachEntryUnder]'s reason.
	read, err := newLeaderReader(js, kv)
	if err != nil {
		return nil, nil, fmt.Errorf("coord/kv: read %s: %w", what, err)
	}
	p, err := beginPass(ctx, js, kv, keys, what, false)
	if err != nil {
		return nil, nil, err
	}
	latest, err := p.walk(ctx, what)
	if err == nil {
		err = certify(ctx, read, what, latest, p.index)
	}
	if err != nil {
		p.stop()
		return nil, nil, err
	}
	return latest, p.w, nil
}

// pass is one certified pass under way: the key index it is certified
// against, and the watcher it reads. Stopping the watcher is the caller's: a
// listing stops it at the pass's end, and a watch reads on.
type pass struct {
	w     jetstream.KeyWatcher
	index []string      // the keys the leader's index named before the pass began
	quiet time.Duration // how long it may go without a delivery ([passQuiet])
}

// beginPass reads the key index under keys, then opens the watcher the pass
// reads: an index taken before the reads that act on it is valid. headers
// watches each key's headers alone, through a bucket settled where it was
// opened ([headerWatcher]); any other is refused, as [watchWalk] refuses it.
//
// THE INDEX FIRST, never beside the consumer: across a link its reply crosses
// behind whatever the consumer is already delivering, so asked once a run's
// value had started crossing it waited the minutes that value takes and was
// re-asked every second until the lookup ceiling failed the listing.
func beginPass(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	keys, what string, headers bool) (*pass, error) {

	h, ok := kv.(headerWatcher)
	if headers && !ok {
		return nil, fmt.Errorf("coord/kv: read %s: %s is not a bucket settled where it was opened, "+
			"so nothing watches its headers alone", what, kv.Bucket())
	}
	names, err := keysUnder(ctx, js, kv, keys)
	if err != nil {
		return nil, unavailable("read "+what, fmt.Errorf("read its key index to certify the pass: %w", err))
	}
	// EITHER PASS KEEPS THE KEYS' QUIET, its consumer beating at it: a headers
	// pass shares its connection, so its next delivery can wait behind another
	// read's run at its bound, and ended then it would read every record left
	// whole ([certify]).
	quiet := passQuiet(kv, keys)
	// Watch rather than WatchAll, so the filter narrows server-side too. NOT
	// IgnoreDeletes: a key removed mid-pass arrives as its value then its
	// marker, and dropping the marker would list the value it replaced.
	var w jetstream.KeyWatcher
	if headers {
		w, err = h.watchHeaders(ctx, keys, quiet)
	} else {
		w, err = kv.Watch(ctx, keys)
	}
	if err != nil {
		return nil, unavailable("read "+what, err)
	}
	return &pass{w: w, index: names, quiet: quiet}, nil
}

// walk runs the pass to its end; [passOver] gives both of its ends.
func (p *pass) walk(ctx context.Context, what string) (map[string]jetstream.KeyValueEntry, error) {
	return passOver(ctx, p.w, what, p.quiet)
}

// stop ends the pass's consumer.
func (p *pass) stop() { _ = p.w.Stop() }

// watchWalk is a pass NOTHING certifies — the marker sweep's — over a watcher
// it opens and stops on every return, so no early return leaks one.
//
// HEADERS ONLY ([headerWatcher]): every sweep decides on a key's operation,
// revision and instant, never its value, and a bucket of activation bodies or
// run logs walked whole moves megabytes across a link for nothing.
func watchWalk(ctx context.Context, kv jetstream.KeyValue, keys, what string) (map[string]jetstream.KeyValueEntry, error) {
	h, ok := kv.(headerWatcher)
	if !ok {
		return nil, fmt.Errorf("coord/kv: read %s: %s is not a bucket settled where it was opened, "+
			"so nothing watches its headers alone", what, kv.Bucket())
	}
	// Markers included, for [beginPass]'s reasons: the sweep is after them.
	w, err := h.watchHeaders(ctx, keys, passIdle)
	if err != nil {
		return nil, unavailable("read "+what, err)
	}
	defer func() { _ = w.Stop() }()
	return passOver(ctx, w, what, passIdle)
}

// passOver is the PASS: one walk over an ordered ephemeral consumer, answering
// the newest revision it saw of each key, markers included (a key deleted
// mid-pass arrives as its value, then its marker; [certify] rereads markers).
// A closed channel is a failure. The pass ENDS on the nil entry, or after
// quiet without a delivery: a listing's index answered before it began, and a
// marker the sweep misses goes next tick. The nil entry NEVER comes for a pass
// whose pending messages were all removed (aged, purged, swept) before it
// reached them, so waiting would hold a deadline-free caller and a
// server-side consumer for ever; ending is safe because [certify] reads every
// indexed key it lacks. The watcher stays running: the caller stops it
// ([listUnder]) or reads on ([watchUnder]).
func passOver(ctx context.Context, w jetstream.KeyWatcher, what string,
	quiet time.Duration) (map[string]jetstream.KeyValueEntry, error) {

	latest := map[string]jetstream.KeyValueEntry{}
	// Re-armed by each delivery.
	still := time.NewTimer(quiet)
	defer still.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, unavailable("read "+what, ctx.Err())
		case <-still.C:
			// QUIET for a whole quiet: waiting on removed messages or a
			// consumer that stopped delivering. certify reads what it lacks.
			return latest, nil
		case kve, ok := <-w.Updates():
			if !ok {
				return nil, unavailable("read "+what, errors.New("listing ended early"))
			}
			// nil marks the end of the initial values.
			if kve == nil {
				return latest, nil
			}
			keep(latest, kve)
			still.Reset(quiet)
		}
	}
}

// passIdle is how long a pass may go without a delivery in process before it
// is ended and [certify] reads what it lacks; across a link it grows by the crossing of the largest value the
// pass may deliver ([passQuiet]).
//
// Five seconds is the client's own idleness interval for an ordered consumer
// (nats.go js.go orderedHeartbeatsInterval), where the broker calls it idle. A
// delivering pass re-arms it on every entry. Shorter, a busy server's pause
// turns the rest of a large pass into a leader read per key; longer, a pass
// that met removed messages holds its caller longer. A cost bound, never a
// correctness one ([passOver]).
const passIdle = 5 * time.Second

// passQuiet is how long a pass over keys of kv may go without a delivery:
// the bucket's quiet for the largest value under keys ([settledBucket.quiet]),
// else [passIdle]. Across a link a run's value at its bound takes three
// minutes to cross, so at passIdle a pass that met one ended before it
// arrived and read every record after it again, one leader read each.
func passQuiet(kv jetstream.KeyValue, keys string) time.Duration {
	if b, ok := kv.(settledBucket); ok {
		return b.quiet(keys)
	}
	return passIdle
}

// keep records kve unless the listing already holds a newer revision of its
// key. A pass delivers in stream order, so its later delivery IS the newer one;
// the comparison keeps the rule from depending on that.
func keep(latest map[string]jetstream.KeyValueEntry, kve jetstream.KeyValueEntry) {
	if prev, seen := latest[kve.Key()]; seen && prev.Revision() > kve.Revision() {
		return
	}
	latest[kve.Key()] = kve
}

// keysUnder reads the stream's per-subject index: every key under the filter
// with a message at the instant the stream leader answers, one request a page,
// each asked again while nobody answers it ([jsapi.Leader.Subjects]): a member
// still believing in a lost leader drops the request without a word, so a page
// rather than the whole index is what is retried.
//
// An answer naming NO LEADER is refused with [errIndexLeaderless]: it is one
// member's own copy while its group elects, and can name less than the quorum
// committed. A solo broker and an R1 stream name the server that answered, so
// only a replicated group between leaders is refused.
func keysUnder(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue, keys string) ([]string, error) {
	leader, err := jsapi.NewLeader(js, bucketStream(kv))
	if err != nil {
		return nil, err
	}
	pre := bucketSubjects(kv)
	var names []string
	err = leader.Subjects(ctx, pre+keys, func(subjects map[string]uint64) error {
		for subject := range subjects {
			key, ok := strings.CutPrefix(subject, pre)
			if !ok {
				// The filter begins with the prefix, so a subject without it
				// is a broker answering something else; dropping it would be
				// the silent short answer this file exists to prevent.
				return fmt.Errorf("the key index answered %q, which is not under %q", subject, pre)
			}
			names = append(names, key)
		}
		return nil
	})
	if errors.Is(err, jsapi.ErrLeaderless) {
		return nil, errIndexLeaderless
	}
	if err != nil {
		return nil, err
	}
	return names, nil
}

// leaderInfo is stream's info as its LEADER answered it (a lease bucket's last
// write, the store's clock), asked as a metadata read ([jsprovision.ReadBounded]).
//
// ASKED AGAIN WHILE NOBODY ANSWERS: only the leader answers (jetstream_api.go
// jsStreamInfoRequest), so a member that still believes in a lost leader drops
// the request, and one request would wait out its caller's deadline or the
// client's five-second default where a retry reaches the new leader; a late
// request stays listened for while the next is out. A caller with no deadline
// is held to those five seconds. A HANDLE OF EACH REQUEST'S OWN, because they
// can be out together and the client caches each reply on its handle with no
// lock (lane.stream's reason).
func leaderInfo(ctx context.Context, js jetstream.JetStream, stream string) (*jetstream.StreamInfo, error) {
	return jsprovision.ReadBounded(ctx, js.Options().DefaultTimeout, func(ctx context.Context) (*jetstream.StreamInfo, error) {
		// js.Stream is itself the info request: the handle carries what it
		// just fetched, the server's timestamp among it.
		s, err := js.Stream(ctx, stream)
		if err != nil {
			return nil, err
		}
		return s.CachedInfo(), nil
	}, nil)
}

// errUndecodableHeaders is a message the leader holds whose headers do not
// decode, so nothing can say whether it is a value or a marker.
//
// A fact about the STORED message, not the read: the leader answers the same
// bytes until the key is rewritten. No writer here stores one (the client
// encodes every header), so it was published raw or corrupted. Every read
// answers it as unknown, except the custody claim, for which an unreadable
// record is a definite answer about one batch ([coord.ErrCustodyUndecidable]):
// read as an outage it would stop every batch the keeper settles after it.
var errUndecodableHeaders = errors.New("the leader holds a message whose headers do not decode")

// errIndexLeaderless is a key index a member answered from its own copy while
// its group had no leader.
var errIndexLeaderless = errors.New("the key index was answered while the " +
	"stream had no leader, from one member's own copy, which can name less " +
	"than the quorum committed")

// certify reads, one at a time, every key the index named that the pass did
// not deliver LIVE — never delivered, or delivered as a delete or purge marker,
// which may be a replica's view between a delete and a re-creation (package
// doc, "A tombstone the pass delivered is not an answer") — and records what
// each read found.
//
// ErrKeyNotFound is an answer: the leader no longer holds the key (aged out,
// removed, or its marker swept since the index read), so it is rightly absent.
// A marker the leader answers is recorded and dropped by [visitLive]. Anything
// else fails the listing, above all a read the leader did not answer: the
// case a replica that is behind would have answered "not found".
func certify(ctx context.Context, read *leaderReader, what string,
	latest map[string]jetstream.KeyValueEntry, indexed []string) error {

	for _, key := range indexed {
		if kve, seen := latest[key]; seen && kve.Operation() == jetstream.KeyValuePut {
			continue
		}
		kve, err := read.last(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			continue
		case err != nil:
			return unavailable("read "+what, fmt.Errorf("read %q, which the pass did not deliver live: %w", key, err))
		}
		keep(latest, kve)
	}
	return nil
}

// leaderReader reads a key's newest message from the stream LEADER, decoded as
// the bucket handle decodes an entry: the bucket's half of [jsapi.Leader], the
// one leader read the engine shares.
//
// NOT the bucket handle's Get, nor the client's GetLastMsgForSubject: on these
// buckets both are a direct get any replica may answer (package doc, "Every
// single-key read is the leader's"), and the client picks the API from the
// stream's allow_direct flag alone.
type leaderReader struct {
	leader *jsapi.Leader
	bucket string
	pre    string // the subject prefix a key is written under

	// holding and pace are the bucket's ([settledBucket]): each read waits
	// as its key's class says ([leaderReader.leaderFor]). A bucket opened
	// elsewhere declares none, and every read keeps the term.
	holding holding
	pace    time.Duration
	// size is a value size the caller knows from a record naming it
	// ([sizedReader]), in place of the class's bound; 0 reads by class.
	size int
}

// newLeaderReader addresses the leader reads of kv's stream in the API js
// speaks, each given the time its key's class says ([leaderReader.leaderFor]).
func newLeaderReader(js jetstream.JetStream, kv jetstream.KeyValue) (*leaderReader, error) {
	leader, err := jsapi.NewLeader(js, bucketStream(kv))
	if err != nil {
		return nil, err
	}
	r := &leaderReader{leader: leader, bucket: kv.Bucket(), pre: bucketSubjects(kv)}
	if b, ok := kv.(settledBucket); ok {
		r.holding, r.pace = b.holding, b.pace
	}
	return r, nil
}

// leaderFor is the leader a read of key asks: a reply as large as the value
// the caller named ([sizedReader]) is given its arrival (jsapi's
// Leader.Carrying); else key's class decides, at its bound plus
// [kvHeaderBytes] and the bucket's pace: a deciding class its crossing alone
// (Leader.Deciding, what [Reach.readCrossing] counts for a run's value), a
// carrying class its arrival, a class read at the term nothing more.
//
// THE CROSSING ALONE for a deciding class, never the handling: a request a
// member dropped across an election is asked again two seconds later, inside
// the shortest caller budget (five seconds in process), which counting the
// handling would push past (jsapi's Leader.Deciding).
func (r *leaderReader) leaderFor(key string) *jsapi.Leader {
	if r.size > 0 {
		return r.leader.Carrying(r.size+kvHeaderBytes, r.pace)
	}
	c := r.holding.of(key)
	switch c.reply {
	case replyDeciding:
		return r.leader.Deciding(c.bound+kvHeaderBytes, r.pace)
	case replyCarrying:
		return r.leader.Carrying(c.bound+kvHeaderBytes, r.pace)
	}
	return r.leader
}

// last answers the newest message on key's subject as the stream leader holds
// it, decoded exactly as the client's KV Get decodes one: a key with no
// message at all is ErrKeyNotFound, and a delete or purge marker is an entry
// whose Operation says so.
func (r *leaderReader) last(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	msg, err := r.leaderFor(key).Last(ctx, r.pre+key)
	if errors.Is(err, jsapi.ErrNoMessage) {
		return nil, jetstream.ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.entry(key, msg)
}

// at answers key's message at one revision as the stream leader holds it: the
// leader's answer to the bucket handle's GetRevision, which a replica gives. A
// revision the key no longer holds (replaced or purged) is
// [jetstream.ErrKeyNotFound], as GetRevision says; a replica also says it of a
// revision not applied YET, the answer this exists not to give.
func (r *leaderReader) at(ctx context.Context, key string, revision uint64) (jetstream.KeyValueEntry, error) {
	msg, err := r.leaderFor(key).At(ctx, revision)
	switch {
	case errors.Is(err, jsapi.ErrNoMessage):
		return nil, jetstream.ErrKeyNotFound
	case err != nil:
		return nil, err
	case msg.Subject != r.pre+key:
		// A REVISION ANOTHER KEY HOLDS is not this key's: GetRevision
		// answers it the same way.
		return nil, jetstream.ErrKeyNotFound
	}
	return r.entry(key, msg)
}

// entry is a message the leader answered, as the KV entry the client's own
// Get would have decoded it to.
func (r *leaderReader) entry(key string, msg jsapi.Message) (jetstream.KeyValueEntry, error) {
	op, header, err := operationOf(msg.Header)
	if err != nil {
		return nil, err
	}
	return leaderEntry{
		bucket: r.bucket, key: key, value: msg.Data,
		revision: msg.Sequence, created: msg.Time, op: op, header: header,
	}, nil
}

// live answers key's LIVE value as the leader holds it, the way the bucket
// handle's own Get answers one: a key with no message, and a key whose newest
// message is a delete or purge marker, are both [jetstream.ErrKeyNotFound].
// Every other failure — no leader, a leader that did not answer — is returned
// as it came, so a caller classifies it as unknown and never as absent.
func (r *leaderReader) live(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	kve, err := r.last(ctx, key)
	if err != nil {
		return nil, err
	}
	if kve.Operation() != jetstream.KeyValuePut {
		return nil, jetstream.ErrKeyNotFound
	}
	return kve, nil
}

// getLatest is EVERY single-key read in this package: key's live value in kv,
// as the stream leader holds it — see [leaderReader.live], and kv.go's "Every
// single-key read is the leader's" for why no read here is a direct get.
func getLatest(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	key string) (jetstream.KeyValueEntry, error) {

	read, err := newLeaderReader(js, kv)
	if err != nil {
		return nil, err
	}
	return read.live(ctx, key)
}

// getSized is [getLatest] of a key whose value the caller knows to be
// valueBytes, from a record that names it (the activation's body, its size on
// the pointer): its read is given the time that reply takes to arrive, its
// handling and its crossing at the bucket's pace (jsapi's Leader.Carrying),
// beyond the read term, so a reply of megabytes across a link is asked for
// once and waited for whole, and no longer than its own size takes.
func getSized(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	key string, valueBytes int) (jetstream.KeyValueEntry, error) {

	read, err := sizedReader(js, kv, valueBytes)
	if err != nil {
		return nil, err
	}
	return read.live(ctx, key)
}

// sizedReader is the leader reader [getSized] reads through: the bucket's,
// each read sized by valueBytes in place of its class's bound.
func sizedReader(js jetstream.JetStream, kv jetstream.KeyValue, valueBytes int) (*leaderReader, error) {
	read, err := newLeaderReader(js, kv)
	if err != nil {
		return nil, err
	}
	read.size = valueBytes
	return read, nil
}

// operationOf reads what a stored KV message IS from its headers by the
// client's own rule (nats.go jetstream/kv.go get), beside the headers it
// decoded (none for none): decoded differently from the pass, it would list a
// key the pass dropped or drop one it listed. Headers that do not decode are
// [errUndecodableHeaders].
func operationOf(raw []byte) (jetstream.KeyValueOp, nats.Header, error) {
	if len(raw) == 0 {
		return jetstream.KeyValuePut, nil, nil
	}
	hdr, err := nats.DecodeHeadersMsg(raw)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", errUndecodableHeaders, err)
	}
	return operationOfHeader(hdr), hdr, nil
}

// operationOfHeader is [operationOf] of headers already decoded, as a watch
// delivers them ([settledBucket.watch]). The KV-Operation values are the KV
// protocol's (ADR-8), which neither library exports.
func operationOfHeader(hdr nats.Header) jetstream.KeyValueOp {
	if op := hdr.Get(server.KVOperation); op != "" {
		switch op {
		case kvOperationDelete:
			return jetstream.KeyValueDelete
		case string(server.KVOperationValuePurge):
			return jetstream.KeyValuePurge
		}
		return jetstream.KeyValuePut
	}
	switch hdr.Get(jetstream.MarkerReasonHeader) {
	case server.JSMarkerReasonMaxAge, server.JSMarkerReasonPurge:
		return jetstream.KeyValuePurge
	case server.JSMarkerReasonRemove:
		return jetstream.KeyValueDelete
	}
	return jetstream.KeyValuePut
}

// kvOperationDelete is the KV-Operation value of a delete marker.
const kvOperationDelete = "DEL"

// leaderEntry is a key's newest message as the leader answered it. Delta is
// always zero: it is the newest by construction.
type leaderEntry struct {
	bucket, key string
	value       []byte
	revision    uint64
	created     time.Time
	op          jetstream.KeyValueOp
	header      nats.Header // the message's headers as stored, which the client's entry drops
}

func (e leaderEntry) Bucket() string                  { return e.bucket }
func (e leaderEntry) Key() string                     { return e.key }
func (e leaderEntry) Value() []byte                   { return e.value }
func (e leaderEntry) Revision() uint64                { return e.revision }
func (e leaderEntry) Created() time.Time              { return e.created }
func (e leaderEntry) Delta() uint64                   { return 0 }
func (e leaderEntry) Operation() jetstream.KeyValueOp { return e.op }

// headers is the message's headers as stored ([headersOf]).
func (e leaderEntry) headers() nats.Header { return e.header }

// headersOf is the headers an entry this package decoded carries, and none for
// an entry of the client's own watch, which drops them.
func headersOf(kve jetstream.KeyValueEntry) nats.Header {
	if h, ok := kve.(interface{ headers() nats.Header }); ok {
		return h.headers()
	}
	return nil
}

// visitLive hands each live key to visit, once, in key order: a map iterates
// differently every time, and an unstable listing turns a downstream ordering
// bug into one that reproduces once in ten runs.
func visitLive(latest map[string]jetstream.KeyValueEntry,
	visit func(jetstream.KeyValueEntry) error) error {

	live := make([]jetstream.KeyValueEntry, 0, len(latest))
	for _, kve := range latest {
		if kve.Operation() == jetstream.KeyValuePut {
			live = append(live, kve)
		}
	}
	slices.SortFunc(live, func(a, b jetstream.KeyValueEntry) int { return strings.Compare(a.Key(), b.Key()) })
	for _, kve := range live {
		if err := visit(kve); err != nil {
			return err
		}
	}
	return nil
}

// bucketStream and bucketSubjects are the KV layer's naming convention — the
// stream behind bucket `b` is `KV_b` and its keys are the subjects under
// `$KV.b.` — which the protocol fixes (ADR-8) and the client applies without
// exporting. The index read addresses the stream directly, so it needs both.
func bucketStream(kv jetstream.KeyValue) string { return "KV_" + kv.Bucket() }

func bucketSubjects(kv jetstream.KeyValue) string { return "$KV." + kv.Bucket() + "." }

// putSubject is the subject a write of key to b is published on, by the
// client's own rule (nats.go jetstream/kv.go updateRevision, and :1605 where a
// handle is opened): the key's subject under the bucket, behind the client's
// API prefix where it is not the account's own, so a leaf's domain client
// reaches the fleet's stream. No bucket here mirrors, so the client's mirror
// prefix never applies.
func putSubject(b settledBucket, key string) (string, error) {
	api, err := jsapi.Of(b.js)
	if err != nil {
		return "", fmt.Errorf("coord/kv: write %q in %s: %w", key, b.Bucket(), err)
	}
	if domain := api.Domain(); domain != "" {
		return "$JS." + domain + ".API." + bucketSubjects(b) + key, nil
	}
	return bucketSubjects(b) + key, nil
}

// validKey is the client's rule for a key it writes (nats.go jetstream/kv.go
// keyValid), which a publish of this package's own must keep: a key the
// client refuses would otherwise publish to a subject no stream holds.
func validKey(key string) bool {
	if key == "" || key[0] == '.' || key[len(key)-1] == '.' || strings.Contains(key, "..") {
		return false
	}
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '/', c == '_', c == '=', c == '.':
		default:
			return false
		}
	}
	return true
}
