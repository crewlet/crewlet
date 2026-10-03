// How this package reads a whole bucket: ONE ORDERED PASS, CERTIFIED against
// the stream's own key index, rather than a listing plus a fetch per key.
//
// A KV bucket is a stream, and its keys are subjects under it. The obvious
// listing — ask for the key names, then Get each one — costs a round trip per
// key ON TOP of an ordered ephemeral consumer created and destroyed per call:
// two JetStream metadata proposals on a clustered bucket, for a question that
// is one pass over the stream. Several of a node's fifteen-second duty loops
// read a bucket on every tick and the state-log write fence lists the trim
// floors out of the positions register on every write at an expectation of
// zero, so that cost is paid continuously rather than at the edges.
//
// So the walk is ONE pass that carries each key AND its value together, and
// the filter goes to the BROKER rather than to a client-side test.
//
// # A pass cannot tell that it is complete, so it is certified
//
// The pass is the client's key watcher, and the watcher decides where "the
// initial values" end by GUESSING. nats.go (jetstream/kv.go, WatchFiltered)
// sends its end marker when `received >= initPending || delta == 0`: the
// count of messages delivered so far against the consumer's pending count at
// creation, or the consumer's num_pending on the delivery just made. Neither
// is exact on these buckets, and the reason is that every bucket here keeps
// ONE revision per key.
//
// An overwrite of a key the pass has not reached yet REMOVES the revision the
// pass was about to deliver and appends the replacement at the tail. The
// server decrements the consumer's num_pending for that removal synchronously,
// inside the write (stream.go storeUpdates -> consumer.decStreamPending), and
// increments it for the replacement ASYNCHRONOUSLY, off the stream's signal
// queue (signalConsumersLoop -> processStreamSignal). A delivery inside that
// window reports num_pending 0 while the key's only revision is still ahead of
// the cursor, and the watcher ends the initial values there. The count guess
// fails the other way round: a later revision of a key ALREADY delivered counts
// as one of the initial values, so the count is reached while a key whose
// revision was removed has not been delivered at all.
//
// Measured against the embedded broker, with five presence leases and three of
// them renewed in tight loops: 848 of 3634 ListLive(ClassNode) calls missed at
// least one live lease. One pass ended after `node.n1@4 pending=1` and
// `node.n3@8 pending=0`; n0, n2 and n4 — all three live throughout — arrived
// after the marker at revisions 353 to 355. Every membership read is a listing
// (seat placement divides by ListLive(ClassNode), the estate map is built from
// ListLive(ClassEstate)), so a miss is a live node that looks gone; and the
// trim takes a MINIMUM across the positions rows every node heartbeats, so a
// missed row raises the floor and deletes log records that node still needs.
//
// The one exact "the cursor is at the tail" fact is the consumer's own store
// EOF, and the broker does not hand it to a pass that could use it: a push
// consumer sees it only as an idle heartbeat seconds later, and a pull request
// reports it only after delivering something — its empty case is decided by
// the same pending counter up front, or by an expiry timer, which a request
// that merely waited in a busy server's queue also trips. A pass therefore
// cannot certify itself, and a listing that trusted one would be guessing.
//
// So every listing is CERTIFIED against the stream's per-subject index: a
// stream info with a subject filter names every key that has a message at one
// instant — answered by the stream LEADER, from the index the store keeps
// under its own lock, so it is not the replica-served, block-hinted read the
// last section rules out. A key it names that the pass did not deliver LIVE —
// never delivered at all, or delivered as a delete or purge marker — is read
// by itself, from the leader as well: "Every certifying answer is the
// LEADER's" below says why neither read is taken from anything else, and "A
// tombstone the pass delivered is not an answer" why a marker is read again.
// That closes the hole exactly: a key live from the moment the index is read
// until its own read is reported has a message at the first moment, so it is
// named by the index, and it is either delivered live by the pass or read.
//
// What that costs is ONE PASS, as before; one index read, which is two API
// requests (a fresh stream handle, then the filtered info on it) run BESIDE
// the pass rather than after it, so that on a network their round trips
// overlap the pass's own; and one leader read for each key the pass lost or
// delivered as a marker. The lost keys are zero without concurrent writes and
// bounded by the keys written while the listing ran. The markers are bounded
// by the ESTATE: a bucket the broker never ages keeps a record's marker until
// [FleetStore.SweepMarkers] removes it, [coord.MarkerRetention] after it was
// written, so a listing re-reads the records removed in the last few hours
// and never every record ever removed. It is never a read per key. Measured
// against the embedded broker, where the server's own work rather than the
// network is the cost, a listing of twenty quiet keys took 0.95 ms certified
// against 0.75 ms for the bare pass.
//
// Two more things fall out of certifying rather than streaming. Each key is
// handed to the caller ONCE, at the newest revision the listing read. The
// count guess above is how a pass delivers one key at two revisions —
// measured at 2 in 5000 passes with every key rewritten — and the callers that
// append (the budget usage, the fleet view, the sandbox runs, every positions
// class) listed it twice. And a listing that FAILS hands the caller nothing,
// rather than the half it read before the failure.
//
// # Every certifying answer is the LEADER's
//
// A certification is only as good as the store that answers it, and a
// clustered bucket has one store per replica. The bucket handle's own Get is
// a DIRECT get on every bucket here — the client's KV layer creates a bucket
// with allow_direct and reads a key through `$JS.API.DIRECT.GET` whenever the
// stream allows it — and a direct get is answered by whichever replica the
// broker picks from a queue group every replica joins once it is about 90%
// synchronised (nats-server stream.go subscribeToDirect, and the sync
// threshold in jetstream_cluster.go). None of them leaves that group when it
// falls behind, so a replica that has not applied a write answers NOT FOUND
// for a key the leader's index named. Measured on a three-member cluster with
// one member cut off: the quorum committed the key, and the cut member's
// direct get answered "key not found" to its own client. A certification that
// read back through Get took that answer as "deleted since the index was
// read", which is the one conclusion it exists to rule out, and the key live
// throughout the listing was absent from it.
//
// So the certifying read is `$JS.API.STREAM.MSG.GET` with `last_by_subj`,
// which only the stream leader answers — a member that is not the leader stays
// silent, and one whose group has no leader refuses — so a read that cannot
// reach the leader FAILS rather than answering from a copy. The client has no
// call for it on a stream that allows direct gets (jetstream/stream.go getMsg
// switches to the direct API on that flag alone), so this package asks for it
// on the wire, addressed in the API the client speaks ([jsapi.API.Subject]),
// and decodes the answer the way the client's own KV Get does.
//
// It is not only the certification's. A listing met the replica that is
// behind first, and every read of one key has the same exposure — a claim's
// read-back, a renew's, a fleet record's — so every single-key read in this
// package is this one, and none is a direct get: kv.go's "Every single-key
// read is the leader's" is that rule and what it costs.
//
// The index read has the matching hole, and it is closed the same way. A
// stream info is the leader's EXCEPT while the group has no leader, when every
// member answers from its own store rather than stay silent (jetstream_api.go
// jsStreamInfoRequest), and a member that had not applied the last writes
// before the leader went away names less than the quorum holds — measured on
// the same cut member, once it held its group leaderless: its index named one
// key of the two the quorum held. That answer names no leader, and
// [keysUnder] refuses it: a listing during an election is the third answer,
// never a short one.
//
// # A tombstone the pass delivered is not an answer
//
// The pass is an R1 ordered consumer, which the broker places on a random
// member of the stream's group (jetstream_cluster.go createGroupForConsumer),
// so it reads that member's copy rather than the leader's. For a key it
// delivered as a Put that is only lag — a stale revision, never a missing key.
// A MARKER is different: a key DELETED AND RE-CREATED before the listing
// began, read by a pass on a member that had applied the delete and not yet
// the re-creation, comes back as a tombstone although the key was live
// throughout. So a marker the pass delivered is treated exactly as a key it
// never delivered: when the index names the key it is read from the leader,
// and the leader's answer — the live value, a marker, or nothing — is what the
// listing holds. A marker for a key the index does NOT name needs no read:
// the leader held no message on it at an instant inside the listing, so the
// key was not live throughout, and absent is a correct answer for it.
//
// That read is paid once per marker the leader still holds, which is why the
// markers do not stay: every bucket the broker never ages is swept of the ones
// older than [coord.MarkerRetention] (markers.go), and that sweep is what
// keeps this read bounded by the records removed recently rather than by every
// record ever removed. The sweep changes no answer — a key whose marker is
// gone reads exactly as one never written, here and in [createKey] — so its
// horizon is a cost, never a correctness, bound.
//
// # What the certification does not close
//
// The broker's page size for that index, jetstream_api.go JSMaxSubjectDetails:
// 100,000 keys under one filter. Past it the client reads the index in pages
// by offset into a sorted list the server re-sorts per page, so a key removed
// before a page boundary between two page reads shifts a later key across it,
// and that later key goes uncertified — which matters only if the pass lost it
// too.
//
// # The batched direct read, and why it is not here
//
// `$JS.API.DIRECT.GET` with `multi_last` asks for the last message on every
// matching subject in ONE request/reply, with no consumer at all — which is
// exactly the shape of this question, and it was tried. It is not here, for
// two independent reasons, either of which alone is disqualifying:
//
//   - IT IS SERVED BY ANY REPLICA. That is the whole reason it is fast, and
//     it is why internal/queue/jetstream/stream.go sets allowDirect false on
//     every stream the framework reads a last-message answer from, and why the
//     certifying read above is not the bucket's own Get: a follower may be
//     behind an acknowledged write, so the read can miss a record the quorum
//     already has. Most of this estate would survive that — a liveness
//     judgement decides what to ATTEMPT and the CAS at the revision it read
//     decides what happens — but two reads have no arbitration behind them.
//     The protocol gate lets a claim through a mixed-version upgrade when it
//     misses an older-protocol peer, and the trim floor takes a MINIMUM
//     across the positions rows, so a row it cannot see RAISES the floor and
//     deletes log records a node has not applied yet. That one is data loss.
//
//   - IT ANSWERED WRONG ON ONE REPLICA, which is the reason that does not
//     depend on the first. A KV bucket keeps one message per subject, so
//     every Put deletes the one before it, and the server resolves multi_last
//     through a per-subject last-BLOCK index that this churn leaves stale
//     (filestore.go's multiLastSeqsByLastBlockLocked carries the unresolved
//     case explicitly). Measured against a single-node embedded broker, a
//     read of a populated key class came back EMPTY several times a run — and
//     an empty answer is the one failure this estate cannot survive, because
//     "no rows" is a legitimate answer everywhere it is asked.
//
// Do not reintroduce it without an answer to both. A benchmark is not one.

package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// eachEntry walks the latest revision of every LIVE key in a bucket, handing
// each entry — its key AND its value together — to visit, once per key and in
// key order.
//
// A visit that returns an error stops the walk and that error is returned
// unwrapped, so a caller keeps its own vocabulary rather than having this
// function guess at it. Deletes are filtered, so a tombstoned key is never
// visited and no caller has to recognise one.
func eachEntry(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntryUnder(ctx, js, kv, jetstream.AllKeys, kv.Bucket(), visit)
}

// eachEntryUnder is [eachEntry] over the keys matching one filter.
//
// THE BROKER DOES THE FILTERING, which is the whole point. A key is a subject
// token path under its bucket (coord/keys.go), so a class of keys written by
// [coord.DocumentKey] is a subject wildcard the broker can match — and the
// shared positions register holds EIGHT classes, so a walk that read the whole
// bucket moved all eight to use one. That read is not an edge case: the
// state-log write fence takes it, for the floors, on every write at an
// expectation of zero.
//
// The filter is in the KEY's vocabulary rather than the subject's — "floor.>"
// and not "$KV.crewlet_x.floor.>" — because a key is what the caller holds and
// what the watcher takes. Each walk composes its own, and there is
// deliberately no default: an empty filter read as "everything" would turn a
// caller that lost its class value into one that walks the whole register and
// decodes eight classes as one. The certification's index read narrows by the
// same filter, so the two halves of one listing can never be answering about
// different keys.
//
// `what` names the listing a failure could not complete — the bare name, which
// every message composes into "read <what>" — and a filtered walk names its
// LISTING rather than its bucket. Eight classes share the positions register,
// so "read crewlet_positions" is the same sentence for all of them: it names
// the file an operator would inspect and never the duty that stalled.
//
// `js` is the client both leader reads go through — the index, and each
// certifying read. See [keysUnder] for why the index is not the bucket
// handle's own status call, and [leaderReader] for why a certifying read is
// not the bucket handle's own Get.
func eachEntryUnder(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	keys, what string, visit func(jetstream.KeyValueEntry) error) error {

	// Resolved BEFORE the pass, although only a listing that lost a key
	// uses it: a client built on an API this engine does not speak would
	// otherwise fail only on the listings that happened to race a write,
	// which reads as a flaky broker rather than as the wiring mistake it is.
	read, err := newLeaderReader(js, kv)
	if err != nil {
		return fmt.Errorf("coord/kv: read %s: %w", what, err)
	}
	return listUnder(ctx, js, read, kv, keys, what, visit)
}

// listUnder is [eachEntryUnder] with its certifying reads handed in, so a
// test can point them somewhere the index read does not go.
func listUnder(ctx context.Context, js jetstream.JetStream, read *leaderReader, kv jetstream.KeyValue,
	keys, what string, visit func(jetstream.KeyValueEntry) error) error {

	// The index read runs BESIDE the pass. It is valid whenever it is taken
	// before the reads that act on it (see the file doc), so there is no
	// ordering to keep, and on a network its round trips overlap the ones
	// the pass spends creating and deleting its consumer.
	//
	// Its goroutine is this call's: cancelled and joined on every return,
	// including a pass that failed and no longer needs the answer.
	indexCtx, cancel := context.WithCancel(ctx)
	indexed := make(chan keyIndex, 1)
	go func() {
		names, err := keysUnder(indexCtx, js, kv, keys)
		indexed <- keyIndex{names: names, err: err}
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-indexed
		}
	}()

	latest, err := watchWalk(ctx, kv, keys, what)
	if err != nil {
		return err
	}
	index := <-indexed
	joined = true
	if index.err != nil {
		return unavailable("read "+what, fmt.Errorf("read its key index to certify the pass: %w", index.err))
	}
	if err := certify(ctx, read, what, latest, index.names); err != nil {
		return err
	}
	return visitLive(latest, visit)
}

// keyIndex is what the certification's index read answered.
type keyIndex struct {
	names []string
	err   error
}

// watchWalk is the PASS: one walk over an ordered ephemeral consumer, answering
// the newest revision it saw of every key it saw — a delete or purge marker
// included, because a key deleted WHILE the pass ran is delivered first as a
// value and then as its marker, and dropping the marker would leave the stale
// value standing. [certify] reads a marker again from the leader rather than
// trusting it (see the file doc).
//
// The only transport — the file doc says why the batched read is not one. It
// carries the key AND the value together, so there is still no Get per name
// here, and its honesty rule is the whole of it: the nil entry — and ONLY the
// nil entry — ends the pass, and a CLOSED CHANNEL is a failure named as one.
// The nil entry is where the pass ENDS, not proof that it saw every key; that
// proof is [certify]'s.
//
// It also owns the watcher, so there is no early-return path that leaks one —
// the abandoned-listing case the client's blocking 256-entry handoff could
// park a goroutine and a server-side consumer on for ever. The watcher is
// stopped the moment the pass ends, before anything is certified, for the same
// reason: every write landing after the end marker is pushed into that
// handoff, and nothing here reads it any more.
func watchWalk(ctx context.Context, kv jetstream.KeyValue, keys, what string) (
	map[string]jetstream.KeyValueEntry, error) {

	// Watch rather than WatchAll, so this transport narrows server-side too.
	//
	// NOT IgnoreDeletes: a key removed while the pass ran is delivered as its
	// value and then as its marker, and a pass that dropped the marker would
	// list the value it replaced. certify reads a marker from the leader and
	// visitLive is where a tombstone stops.
	w, err := kv.Watch(ctx, keys)
	if err != nil {
		return nil, unavailable("read "+what, err)
	}
	defer func() { _ = w.Stop() }()

	latest := map[string]jetstream.KeyValueEntry{}
	for {
		select {
		case <-ctx.Done():
			return nil, unavailable("read "+what, ctx.Err())
		case kve, ok := <-w.Updates():
			if !ok {
				return nil, unavailable("read "+what, errors.New("listing ended early"))
			}
			// nil marks the end of the initial values.
			if kve == nil {
				return latest, nil
			}
			keep(latest, kve)
		}
	}
}

// keep records kve unless the listing already holds a newer revision of its
// key. A pass delivers in stream order, so its later delivery of a key IS the
// newer one; the comparison is here so the rule does not depend on that.
func keep(latest map[string]jetstream.KeyValueEntry, kve jetstream.KeyValueEntry) {
	if prev, seen := latest[kve.Key()]; seen && prev.Revision() > kve.Revision() {
		return
	}
	latest[kve.Key()] = kve
}

// keysUnder reads the stream's per-subject index: every key under the filter
// that has a message at the instant the stream leader answers.
//
// THROUGH A HANDLE OF ITS OWN rather than the bucket's Status: the client
// caches the StreamInfo a status call fetched onto the shared bucket handle
// WITHOUT a lock (nats.go jetstream/stream.go Info), so two listings of one
// bucket at once would race in the client — the same reason lane.stream
// exists. js.Stream hands back a fresh handle per call and shares nothing.
//
// An answer that names NO LEADER is refused with [errIndexLeaderless]: it is
// a member's own copy, given while its group elects (see the file doc), and it
// can name less than the quorum committed. A solo broker and an R1 stream both
// name the server that answered, so only a replicated group between leaders
// is refused.
func keysUnder(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue, keys string) ([]string, error) {
	stream, err := js.Stream(ctx, bucketStream(kv))
	if err != nil {
		return nil, err
	}
	pre := bucketSubjects(kv)
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(pre+keys))
	if err != nil {
		return nil, err
	}
	if c := info.Cluster; c != nil && c.Leader == "" {
		return nil, errIndexLeaderless
	}
	names := make([]string, 0, len(info.State.Subjects))
	for subject := range info.State.Subjects {
		key, ok := strings.CutPrefix(subject, pre)
		if !ok {
			// The broker matched this subject against a filter that begins
			// with the prefix, so a subject without it is a broker that did
			// not do what it was asked. Dropping it would be the silent short
			// answer this file exists to prevent.
			return nil, fmt.Errorf("the key index answered %q, which is not under %q", subject, pre)
		}
		names = append(names, key)
	}
	return names, nil
}

// errIndexLeaderless is a key index a member answered from its own copy while
// its group had no leader.
var errIndexLeaderless = errors.New("the key index was answered while the " +
	"stream had no leader, from one member's own copy, which can name less " +
	"than the quorum committed")

// certify reads, one at a time, every key the index named that the pass did
// not deliver LIVE — a key it never delivered, and a key whose newest revision
// in the pass is a delete or purge marker — and records what each read found.
//
// The marker is read again because the pass may have been served by a replica
// that applied a key's delete and not yet its re-creation (see the file doc):
// its tombstone says what that replica held, not what the key holds.
//
// ErrKeyNotFound is an answer, not a failure: the leader holds no message on
// the key any more — it aged out under the bucket's TTL, was removed outright,
// or its marker was swept, between the index read and this one — and a key
// that is gone is correctly absent from the listing. A delete or purge marker
// the leader answers is recorded like any other revision and left out by
// [visitLive]. Anything else is the third answer — a listing that could not be
// certified is not a listing, and a read the leader did not answer is the case
// that matters most: it is the one a behind replica would have answered "not
// found".
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

// leaderReader reads a key's newest message from the stream LEADER: a
// `$JS.API.STREAM.MSG.GET` with `last_by_subj`, the one read of a single key
// that no replica answers from its own copy.
//
// NOT the bucket handle's Get, which on these buckets is a direct get any
// replica may answer — see the file doc for what that cost. And not the
// client's GetLastMsgForSubject either, which is the same direct get: the
// client picks the API from the stream's allow_direct flag alone and offers
// no way to ask for the other one.
type leaderReader struct {
	conn    *nats.Conn
	subject string // the MSG.GET subject of this bucket's stream, in the client's API
	bucket  string
	pre     string // the subject prefix a key is written under

	// timeout bounds a read whose context has no deadline — the client's
	// OWN default, taken from the client, because this read goes past the
	// client and would otherwise be the one request in a listing that
	// waited for ever on a leader that never answers.
	timeout time.Duration
}

// newLeaderReader addresses the leader reads of kv's stream in the API js
// speaks.
func newLeaderReader(js jetstream.JetStream, kv jetstream.KeyValue) (*leaderReader, error) {
	api, err := apiOf(js)
	if err != nil {
		return nil, err
	}
	subject, err := api.Subject(fmt.Sprintf(server.JSApiMsgGetT, bucketStream(kv)))
	if err != nil {
		return nil, err
	}
	return &leaderReader{
		conn:    js.Conn(),
		subject: subject,
		bucket:  kv.Bucket(),
		pre:     bucketSubjects(kv),
		timeout: js.Options().DefaultTimeout,
	}, nil
}

// apiOf names which of the engine's two JetStream APIs js was built on.
//
// This package is handed a client rather than an API, and a request that goes
// past the client has to be addressed where the client's own go or it is
// answered by nothing: a leaf's broker serves JetStream only under the
// embedded fleet's domain. [jsapi.API.Subject] is the one rule for that
// address, so this recovers the API from the client instead of spelling the
// address a second time. Every client the engine builds comes from
// [jsapi.API.Client], so any other shape is a wiring mistake and is named.
func apiOf(js jetstream.JetStream) (jsapi.API, error) {
	switch o := js.Options(); {
	case o.APIPrefix != "":
		return jsapi.API{}, fmt.Errorf("the JetStream client addresses the "+
			"custom API prefix %q, which is neither API this engine speaks — "+
			"build it with internal/jsapi", o.APIPrefix)
	case o.Domain == "":
		return jsapi.Account(), nil
	case o.Domain == jsapi.Domain:
		return jsapi.Embedded(), nil
	default:
		return jsapi.API{}, fmt.Errorf("the JetStream client addresses the "+
			"domain %q, which is neither API this engine speaks — build it "+
			"with internal/jsapi", o.Domain)
	}
}

// last answers the newest message on key's subject as the stream leader holds
// it, decoded exactly as the client's KV Get decodes one: a key with no
// message at all is ErrKeyNotFound, and a delete or purge marker is an entry
// whose Operation says so.
func (r *leaderReader) last(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	return r.get(ctx, key, server.JSApiMsgGetRequest{LastFor: r.pre + key})
}

// at answers key's message at one revision as the stream leader holds it —
// the leader's answer to what the bucket handle's GetRevision asks a replica.
// A revision the key no longer holds — a later write replaced it, or it was
// purged — is [jetstream.ErrKeyNotFound], which is what GetRevision says of
// one; a replica says it of a revision it has not applied YET as well, which
// is the answer this exists not to give.
func (r *leaderReader) at(ctx context.Context, key string, revision uint64) (jetstream.KeyValueEntry, error) {
	return r.get(ctx, key, server.JSApiMsgGetRequest{Seq: revision})
}

// get asks the leader one message question about key and decodes the answer
// exactly as the client's KV Get decodes one.
func (r *leaderReader) get(ctx context.Context, key string,
	ask server.JSApiMsgGetRequest) (jetstream.KeyValueEntry, error) {

	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	subject := r.pre + key
	req, err := json.Marshal(ask)
	if err != nil {
		return nil, fmt.Errorf("encode the read: %w", err)
	}
	reply, err := r.conn.RequestWithContext(ctx, r.subject, req)
	if err != nil {
		return nil, err
	}
	var resp server.JSApiMsgGetResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return nil, fmt.Errorf("decode the leader's answer: %w", err)
	}
	switch msg := resp.Message; {
	case resp.Error != nil && server.IsNatsErr(resp.Error, server.JSNoMessageFoundErr):
		return nil, jetstream.ErrKeyNotFound
	case resp.Error != nil:
		return nil, resp.Error
	case msg == nil:
		return nil, errors.New("the leader answered with neither a message nor an error")
	case msg.Subject != subject && ask.Seq != 0:
		// A REVISION ANOTHER KEY HOLDS is not this key's at that revision:
		// what GetRevision answers it, for the same reason.
		return nil, jetstream.ErrKeyNotFound
	case msg.Subject != subject:
		// A broker that answered about another subject did not do what it
		// was asked, and recording its message under this key would list a
		// value the key never held.
		return nil, fmt.Errorf("the leader answered with %q for %q", msg.Subject, subject)
	default:
		op, err := operationOf(msg.Header)
		if err != nil {
			return nil, err
		}
		return leaderEntry{
			bucket: r.bucket, key: key, value: msg.Data,
			revision: msg.Sequence, created: msg.Time, op: op,
		}, nil
	}
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

// operationOf reads what a stored KV message IS from its headers — the
// client's own rule (nats.go jetstream/kv.go get), because an entry this read
// decoded differently from the pass's would list a key the pass would have
// dropped or drop one it would have listed. The KV-Operation values are the
// KV protocol's (ADR-8), which neither library exports as constants.
func operationOf(raw []byte) (jetstream.KeyValueOp, error) {
	if len(raw) == 0 {
		return jetstream.KeyValuePut, nil
	}
	hdr, err := nats.DecodeHeadersMsg(raw)
	if err != nil {
		return 0, fmt.Errorf("decode the leader's message headers: %w", err)
	}
	if op := hdr.Get(server.KVOperation); op != "" {
		switch op {
		case kvOperationDelete:
			return jetstream.KeyValueDelete, nil
		case string(server.KVOperationValuePurge):
			return jetstream.KeyValuePurge, nil
		}
		return jetstream.KeyValuePut, nil
	}
	switch hdr.Get(jetstream.MarkerReasonHeader) {
	case server.JSMarkerReasonMaxAge, server.JSMarkerReasonPurge:
		return jetstream.KeyValuePurge, nil
	case server.JSMarkerReasonRemove:
		return jetstream.KeyValueDelete, nil
	}
	return jetstream.KeyValuePut, nil
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
}

func (e leaderEntry) Bucket() string                  { return e.bucket }
func (e leaderEntry) Key() string                     { return e.key }
func (e leaderEntry) Value() []byte                   { return e.value }
func (e leaderEntry) Revision() uint64                { return e.revision }
func (e leaderEntry) Created() time.Time              { return e.created }
func (e leaderEntry) Delta() uint64                   { return 0 }
func (e leaderEntry) Operation() jetstream.KeyValueOp { return e.op }

// visitLive hands each live key to visit, once, in key order.
//
// Key order because a map iterates in a different order every time, and an
// unstable listing turns any downstream ordering bug into one that reproduces
// once in ten runs.
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
