// Package kv is the coord.Backend a fleet runs on: TTL leases with a fencing
// epoch, on NATS JetStream KV.
//
// The single-node company uses the in-memory twin. This is what the same
// engine becomes when a second node joins: the same contract, certified by
// the same suite (internal/coord/coordtest), with the mutual exclusion moved
// from a mutex to the broker's compare-and-swap.
//
// # Three buckets, and why the epoch needs its own
//
// Postgres kept ONE row per resource and expired it in place, so the epoch
// survived release. A KV deletes a key when it expires — and a deleted key
// restarts the counter, handing the next owner a token a zombie from the
// previous tenure is still fencing its writes with. So ownership and the
// fencing token live in different buckets:
//
//   - crewlet_leases, created with KeyValueConfig.TTL = the seat lease TTL
//     this node asks for, and ADOPTED at whatever a peer created it with.
//     It holds `seat:` and `node:` leases. That age is the STREAM's MaxAge,
//     which is the renewable one: every write refreshes the entry's age, so
//     Update at the current revision IS the renew, an unrenewed key expires
//     SERVER-SIDE, and a peer's Create then succeeds. The store's own expiry
//     is the arbiter clock (the role Postgres now() played), and nodes never
//     compare their own wall clocks.
//
//   - crewlet_duties, holding every OTHER lease: the `worker:` leases of the
//     fleet singletons, and every class a caller leases under a name of its
//     own (the tracker's `move:` and `merge:` claims). Named for the
//     duties, which are why it exists. See "Two lease buckets" below for why
//     none of them can share the first.
//
//   - crewlet_epochs, with NO TTL. One persistent record per resource holding
//     the monotonic counter and the placement hint, for both lease buckets —
//     save the taker leases, which count on ONE record for their whole class
//     (coord.CounterResource), since each names a process and a record per
//     resource would be one per process start, never removed. Measured to
//     survive the lease key's expiry, which is the entire fencing invariant.
//     Gaps in the counter are harmless; resets are not. It is also the
//     counter a single node's in-process leases mint from, because its tokens
//     outlive that process too ([Epochs], epochs.go).
//
// So nothing a process start leaves here accumulates: a taker lease's own key
// is in the duty bucket, whose age removes it three hours after its last
// write — its release's tombstone, or the last renewal of a process that
// died — and its tokens are its class counter's.
//
// # Per-key TTL cannot be used for this
//
// jetstream.KeyTTL is create-only by design — "the TTL is set when the key is
// created and cannot be changed later" — and Update clears it. Measured (see
// behavior_test.go, nats-server 2.14.5 / nats.go 1.53.1): a key created with
// a 1s KeyTTL and renewed through Update was still readable two seconds
// later. IMMORTAL. On a lease that means a dead node's seat could never be
// reclaimed, and every sweep would read healthy while the seat sat dark. The
// bucket-MaxAge form is the renewable one, and behavior_test.go asserts the
// trap so this cannot be "simplified" back.
//
// # One TTL per bucket
//
// MaxAge is a property of the bucket, so a bucket can only honour TTLs up to
// its own age. A per-call TTL LONGER than that is refused: the bucket would
// reap the record early and the deadline handed back would be a lie about when
// the lease ends. A per-call TTL SHORTER than it is honoured by an additional
// deadline carried in the record, judged against the STORE's clock (see
// Store.storeNow), never the caller's.
//
// # Two lease buckets, because seats and everything else want opposite TTLs
//
// A seat lease is renewed on the heartbeat, so its TTL is a few heartbeats,
// and so is a node's presence. Every other lease is sized from the work it
// guards: a duty is re-claimed once per tick, and a tick runs from ten seconds
// to an hour; a tracker walk's claim is renewed on the walk's own heartbeat
// and lasts a minute. So their TTLs run up to coord.MaxDutyTTL. One bucket
// cannot serve both. Its age at the seat TTL refused every duty longer than 45
// seconds, which is how the retention sweep, the integration reconcile, the
// learning passes and every integration pass never ran on any fleet — and,
// once the duties had moved and the rest had not, every tracker walk claim,
// which is how every cross-project move and merge failed on every fleet. Its
// age at the longest duty would put a clock read under every seat renew, and a
// dead node's seats would sit claimable only by deadline arithmetic rather
// than by the broker reaping them.
//
// So WHICH BUCKET is decided by the class, and by the one rule coord states
// for it (coord.HeldToSeatTTL): seats and presence in the seat lease bucket,
// every other class — the duties and any caller's — in the duty bucket. The
// duty bucket's age is coord.MaxDutyTTL and EVERY record in it is judged by
// its own deadline against that bucket's clock, while the seat bucket keeps
// the production shape where a record that can still be read is live. What
// lives in the duty bucket is claimed per tick or renewed on a walk's
// heartbeat rather than per seat heartbeat, so the clock read costs one round
// trip per tick of each duty and per beat of each running walk, fleet-wide.
//
// THE DUTY BUCKET'S AGE IS ONLY EVER RAISED, and it is the ONE value this
// store still writes to a bucket that already exists; every other bucket
// field, this one's replica count included, is adopted rather than rewritten
// (see openBucket). Open adopts a duty bucket that is already at least
// coord.MaxDutyTTL old and raises one that is younger, and never writes it
// lower. A reassertion of this node's own ceiling in either direction lets a
// build with a shorter one shrink the age under a peer that holds a longer
// duty, and the broker then reaps a live duty early. An age longer than a
// lease's TTL costs nothing, because no record there is judged by the age.
//
// # A claim is three writes, and the order carries the invariant
//
// The epoch must be committed to the untimed bucket BEFORE ownership is
// written: the other order leaves ownership at a token the counter has not
// committed, and after the lease key expires a zombie from that tenure fences
// straight through the next one. But advancing the counter first also means
// every LOSER of the ownership CAS has advanced it, so the winner of a
// fleet-wide stampede holds whatever it happened to reserve rather than 1.
//
// Both are satisfied by winning ownership in a CLAIMING state first — owner
// set, epoch 0 — then advancing the counter, then committing the token into the
// record already held. (Between the first two the claim reads its own record
// back by revision, for the store's timestamp on it — the tenure's start, which
// the commit has to carry. A read, not a write, but a step a claim can fail
// at all the same.) Epoch 0 is exactly the right marker because it is
// exactly the wrong token: a conditional write predicated on it matches an
// unset column, and TryAcquire has not returned, so nothing can be written
// under it. Exactly one claimant gets past step 1, so exactly one advances the
// counter. A renew is still one write, and where this store wrote the record
// last it is nothing else (below).
//
// A claim that fails AFTER step 1 GIVES THE RECORD BACK ([Store.abandon]). It
// used to leave it claiming until its TTL, on the reasoning that a claimant
// that failed is a claimant that died — and it is not: it is alive, it knows
// it failed, and the record it left holds the resource against everybody, its
// own owner included. A peer reads a claiming record as held, and the owner's
// own retries read it as a sibling one round trip from committing, so each
// spun through every compare-and-swap round and answered UNKNOWN for the whole
// TTL: an epochs bucket without a leader for a second kept a seat or a duty
// dark for a lease's length. The give-back is exact because no token was
// handed out — a counter the failed step did advance is a gap, and gaps are
// harmless — and because it is taken only of the record carrying this call's
// own claim (leaseValue.Claim), so it can never tear down a sibling's claim or
// a tenure that did commit. What still lapses on its TTL is the record of a
// claimant that could not give it back: one that died, or whose store refused
// the give-back too.
//
// # A renewal of a record this store last wrote is one round trip
//
// A renewal used to read the record from the leader and then write it at the
// revision it read: two round trips per seat, every renew period, which across
// a link of a hundred milliseconds is a fifth of a second a seat, and a pass
// renewing its seats one after another outran the floor TTL's renew period at
// twenty seats. Yet the read tells this store nothing it does not already know
// of a lease it holds: the store wrote the record itself, and the broker
// handed that write a revision it never hands out again. So the store keeps,
// per resource, the record it last wrote and the revision it got
// ([knownRecords], known.go), and [Store.Renew] of a tenure it knows sends ONE
// conditional write at that revision. It lands only while the record is still
// exactly that write — no peer's claim, no release, no reap since — which is
// everything the read would have established; one refused on its comparison
// reads and decides as before. [Store.TryAcquire] starts its first round from
// the same knowledge for a resource its own owner holds, so the presence's
// renewal is its write and settle's read-back, two round trips rather than
// three.
//
// ONLY A RECORD THAT CANNOT LAPSE WHILE IT CAN BE READ is known: a seat or
// presence lease taken at the seat lease bucket's whole age, which the bucket
// reaps at its deadline. A record judged by its deadline against the store's
// clock — every duty, and a seat lease shorter than its bucket's age — can
// lapse and still be there, and a write at its revision would renew a lapsed
// lease, which is deliberately not renewable. Such a renewal still reads the
// record and the store's clock first.
//
// # The protocol gate degrades, deliberately
//
// Postgres evaluated "no live lease at a lower protocol" as a subquery INSIDE
// the claim statement, because a read-then-claim loses the race it exists to
// prevent. A KV cannot express a cross-key predicate inside a CAS, so the
// shape here is check -> claim -> RE-CHECK -> release on violation. The window
// shrinks to the interval between our check and our claim, and its consequence
// changes from silent mixed-protocol operation to a claim we immediately give
// back. Combined with the gate's existing asymmetry — only higher-protocol
// nodes wait, lower ones are never gated — that is a faithful degradation and a
// deliberate difference, not an oversight. The gate judges the seat lease
// bucket alone, because the contract counts presence and seat leases and
// nothing else (coord.ProtocolGateCounts) — a duty lease outlives a crashed
// holder by up to coord.MaxDutyTTL, and counted, it kept a fleet from placing
// a seat for that long after the last older node died mid-upgrade. It judges
// it through a view kept by a watch and made exact by a sequence barrier,
// never a listing per claim ("The protocol gate's view").
//
// # The protocol gate's view
//
// The gate asks whether any live presence or seat lease
// ([coord.ProtocolGateCounts]) is held below the claim's protocol (ADR-0016).
// Listing the lease buckets for it, at every gated claim's check and re-check
// and in every FleetProtocolFloor, makes a claim cost a function of the leases
// already held, and the placement sweep claims seats one after another, so a
// fleet claiming from cold pays its square: a hundred nodes claiming ten
// thousand seats drove the process past 10 GB and the members' raft groups
// out of quorum. Nothing narrows at the broker: a record's protocol is in its
// body, not its key, and the lower-protocol builds the gate exists for write
// only their lease records, never a record a higher build could add. Three
// cheaper shapes each break the gate:
//
//   - Presence alone (a listing of the `node` class) misses a draining node,
//     which gives up presence at its first step and serves its seats until
//     each is handed over.
//   - A floor refreshed on a cadence misses a lower node that joined inside the
//     cadence, at the check and the re-check alike, widening the re-check's
//     one-claim window to the whole cadence.
//   - A per-owner protocol record is written only by the builds that publish
//     it.
//
// So [gateView] WATCHES the seat lease bucket, which holds only `seat:` and
// `node:` leases; the duty bucket holds nothing counted and is not watched.
// The view keeps only the classes ProtocolGateCounts counts rather than
// trusting which bucket a record was written to: the layout is this store's,
// the counted classes the contract's. A BARRIER makes it exact:
// [Store.streamPoint] reads the bucket's last sequence and clock from the
// stream LEADER, refusing an answer that names no leader as a listing's index
// read does, and the gate waits until the view has consumed through that
// sequence. The view then holds the newest record of every key written before
// the read, a certified listing's guarantee, whichever member the watch reads.
// A gate costs one stream info plus the lower-protocol records, of which there
// are none outside a rolling upgrade. The view's standing cost is every write
// to the bucket while it runs, so it stops [gateViewIdle] after the last gate.
// A node at its share pays nothing, nor does one whose every candidate a peer
// holds, since that claim is refused [coord.RefusedHeld] before any gate; a
// duty claim is ungated.
//
// # Every listing is ONE CERTIFIED PASS, and never the client's ListKeys
//
// Reading a whole bucket goes through [eachEntry], which hands over the KEY
// AND THE VALUE TOGETHER in a single ordered pass, narrows to one key class at
// the BROKER where the caller wants one, and CERTIFIES the pass against the
// stream's own key index before handing anything over, because a pass cannot
// tell a complete answer from one that lost a key being rewritten under it.
// Why a pass needs certifying, and why the batched direct read that would drop
// even the consumer is not used, are the sections after this one; what follows
// is why the obvious shape is worse than either.
//
// A key listing is not a cheap read. The client implements ListKeys as a
// watcher, so each call CREATES AND DELETES AN ORDERED EPHEMERAL CONSUMER —
// two JetStream metadata proposals on a clustered bucket — and then leaves the
// caller to spend N more round trips fetching the values it did not carry.
// Five of this node's fifteen-second duty loops read a bucket on every tick,
// so that cost is paid continuously rather than at the edges — and no
// state-log read or seat's write waits on a listing at all: the write fence's
// trim floor is one key read ([FleetStore.Floor]), because the consumer a
// listing creates is a metadata proposal a loaded cluster answers late. What
// still lists does so on a loop's own clock or once per operator gesture.
//
// And it could not report a short answer. The client's key lister ends its
// goroutine on a nil entry, and a receive from the channel its own
// subscription closes on failure yields exactly that nil — so a listing cut
// off half way came back TRUNCATED WITH A NIL ERROR. That is this package's
// central rule inverted: "held", "definitively not held" and "the store could
// not be reached" are three different facts, and a short list with no error
// collapses the third into the second at every caller at once. For the trim's
// published floor it is not a degraded read but a delete of records a node
// still needs. The pass ends on one explicit marker and ONLY on it — the nil
// entry — and anything else is [coord.ErrUnavailable], named as such. The
// marker is where the pass ends rather than proof that it saw every key,
// which is what the certification is for: it is the client's guess, and
// measured against three renewers it guessed early on 23% of listings.
//
// The ordered walk also owns its watcher, so there is no early-return path
// that leaks one — the abandoned-listing case the client's blocking 256-entry
// handoff could park a goroutine and a server-side consumer on for ever.
//
// # A pass cannot tell that it is complete, so it is certified
//
// The pass is the client's key watcher, which GUESSES where the initial values
// end (nats.go jetstream/kv.go WatchFiltered: `received >= initPending ||
// delta == 0`), and neither count is exact on a bucket keeping one revision
// per key. An overwrite of a key the pass has not reached REMOVES the revision
// ahead of the cursor and appends one at the tail; the server decrements the
// consumer's num_pending inside the write (stream.go storeUpdates ->
// consumer.decStreamPending) and increments it asynchronously
// (signalConsumersLoop -> processStreamSignal), so a delivery in that window
// reports 0 pending while a live key is still ahead. The other way round, a
// later revision of a key already delivered counts toward initPending. A
// missed key is a live node that looks gone to every membership read (seat
// placement divides by ListLive(ClassNode)), and a missed positions row raises
// the trim's MINIMUM floor and deletes log records a node still needs. The
// broker hands a pass no exact "cursor at the tail" fact: a push consumer sees
// EOF only as an idle heartbeat seconds later, and a pull request's empty case
// is the same pending counter or an expiry timer a queued request also trips.
//
// So every listing is certified against the stream's per-subject index. A
// stream info with a subject filter, answered by the stream LEADER from the
// index it keeps under its own lock, names every key with a message at one
// instant, and past the 100,000 keys one answer holds (jetstream_api.go
// JSMaxSubjectDetails) every key with a message throughout the pages
// ([jsapi.Leader.Subjects] overlaps them, since a key removed before a page
// boundary shifts the next page back across it). Each named key the pass did
// not deliver LIVE is read by itself from the leader ([certify],
// [leaderReader]), so a key live from the first index request until its own
// read is named and either delivered or read. While a group has no leader,
// every member answers stream info from its own copy (jetstream_api.go
// jsStreamInfoRequest), which can name less than the quorum holds, so an
// answer naming no leader is refused ([errIndexLeaderless]): a listing during
// an election is the third answer, never a short one.
//
// The cost: one pass; one index request per 100,000 keys, run beside the pass
// so their round trips overlap; and one leader read per key lost (zero without
// concurrent writes, bounded by the keys written meanwhile or by the unreached
// keys of a pass ended at [passIdle]) or delivered as a marker (bounded by the
// markers younger than [coord.MarkerRetention]). Certifying rather than
// streaming also hands each key over ONCE, at the newest revision read, where
// the count guess can deliver one twice, and a failed listing hands over
// nothing rather than the half it read.
//
// # A tombstone the pass delivered is not an answer
//
// The pass is an R1 ordered consumer the broker places on a random member of
// the stream's group (jetstream_cluster.go createGroupForConsumer), so it reads
// that member's copy. A stale Put it delivers is only lag, never a missing
// key; a MARKER is not: a key deleted and re-created before the listing began,
// read on a member that has applied the delete and not yet the re-creation,
// comes back a tombstone although it was live throughout. So [certify] treats
// a delivered marker as an undelivered key: when the index names the key, the
// leader's answer (the live value, a marker, or nothing) is what the listing
// holds; when it does not, the leader held no message on the key at an instant
// inside the listing, so absent is correct and nothing is read. That read is
// paid once per marker the leader still holds, which is why markers.go sweeps
// every bucket the broker never ages of markers older than
// [coord.MarkerRetention]. A swept marker reads as a key never written, here
// and in [createKey], so the sweep's horizon bounds cost, never correctness
// (TestATombstoneAReplicaServedIsReadFromTheLeaderOnACluster).
//
// # The batched direct read, and why it is not here
//
// `$JS.API.DIRECT.GET` with `multi_last` asks for the last message on every
// matching subject in ONE request with no consumer, exactly a listing's shape.
// Two independent reasons rule it out, either one enough:
//
//   - ANY REPLICA ANSWERS IT, so it can miss a record the quorum holds
//     (internal/queue/jetstream/stream.go sets allowDirect false on every
//     stream it reads a last message from, for the same reason). Most reads
//     here would survive that, since a CAS at the read revision decides, but
//     two have nothing behind them: the protocol gate would let a claim
//     through a mixed-version upgrade on missing an older-protocol peer, and
//     the trim floor's MINIMUM over the positions rows rises on a missed row
//     and deletes log records a node has not applied.
//   - IT ANSWERED WRONG ON ONE REPLICA: every KV Put deletes the message before
//     it, and the server resolves multi_last through a per-subject last-block
//     index this churn leaves stale (filestore.go
//     multiLastSeqsByLastBlockLocked), so a read of a populated key class came
//     back EMPTY, and "no rows" is a legitimate answer everywhere it is asked.
//
// Reintroducing it needs an answer to both; a benchmark is not one.
//
// # Every single-key read is the leader's
//
// A lease's read-back, a renew's and a release's read, Get, the epoch and
// hint reads, and every record the fleet store reads by key (fleet.go, the
// mailboxes, the positions, the follows, the secrets, the token counters, the
// object store's records, the seat pauses, the chat read state) go through ONE
// read: `$JS.API.STREAM.MSG.GET` with `last_by_subj`, which only the stream
// leader answers ([leaderReader], [getLatest]). NONE is the bucket handle's own
// Get, and none may be — TestNothingReadsAKeyThroughADirectGet reads this
// package's source for one, because the partition case below holds only the
// call sites it reads through, and every other read is a call site of its
// own. A listing's certifying read ([certify]) is this same read, so a listing
// never takes a behind replica's "not found" for "deleted since the index"
// (TestACertifyingReadIsNeverAnsweredByAReplicaThatIsBehind).
//
// That Get is a DIRECT get, and a direct get is answered by whichever replica
// the broker picks — the one on the caller's own member first — from its own
// copy, which can be behind a write the quorum has acknowledged: measured on
// three members with the CPU saturated, 5 to 8 of every 100 reads made
// straight after an acknowledged write returned the revision before it,
// whichever member the reader was connected to. The contract
// is that a backend serves its own writes per resource (coord.Backend), and
// every lease answer rests on it: TryAcquire reads its own write back to learn
// the store's deadline, and on the copy it read the claim's record was still
// the claiming one it had just replaced, so a claim that WON answered a
// refusal. Measured on a three-member cluster, a hundred leaf nodes
// claiming at once: 743 of 10,000 claims on fresh resources answered "not
// held" for a lease the store then held under their owner until its TTL — a
// singleton duty dark on every node for a TTL, since the one node that won
// believed a peer had it. The same read made a renew or a release through a
// member that was behind answer FALSE, "definitively not yours", which a seat
// host acts on by shedding the seat, and a fleet record read by key through
// it answered "absent" for a record the quorum held. shared_cluster_test.go
// holds both: the conformance case with its handles on members holding no
// copy, and a member cut off from its peers, whose reads must fail rather than
// answer from the copy it has.
//
// A read the leader cannot answer — none elected, or none reachable — FAILS,
// which is [coord.ErrUnavailable] to every caller: the contract's third answer,
// never "absent". A write needs that leader anyway, so a node that cannot read
// its lease could not have renewed it either.
//
// THE ONE COPY THAT IS BEHIND AND STILL ANSWERS is a leader cut off from its
// peers that has not yet noticed. The broker gives a stream leader no lease: a
// member that led a stream when it was cut goes on answering that stream's
// leader reads from its own copy until its metadata layer goes leaderless or
// its group steps down on lost quorum — measured at about ten seconds after the
// cut, nats-server's lostQuorumInterval — while the majority elects a leader
// of its own and moves on. So through a member on the minority side of a
// partition, a stream it led reads up to that far behind, and a record the
// majority wrote reads as absent. Nothing written through that member lands (a
// write needs its quorum), a tenure is fenced by its epoch, and a renew's or a
// release's compare-and-set fails there; what such a read cannot be trusted
// with is a decision that writes nothing. Closing the window takes a read that
// proves a quorum — a barrier append per read, as the state log's linearizable
// read is — which would make every heartbeat's read a write, so this package
// does not: the window is the bound it gives, stated here so a caller deciding
// on an absence knows it has one.
//
// The cost, measured on an in-process cluster: a leader read takes 40–220 µs
// at the median where a direct get takes 25–135 µs — the hop to the leader
// when the caller's member is not it — and 27,000 reads a second through one
// member's client where direct gets reached 50,000. A fleet's lease reads were
// its heartbeats, one read per held lease per renew, so ten thousand seats on
// the fifteen-second seat heartbeat asked for under seven hundred a second: a
// few percent of that ceiling. A renewal of a record this store last wrote
// reads nothing now (above), so what remains is the presence's read-back and
// the reads of a renewal whose record moved. What a direct get would buy back
// is served by nothing here, because every read in this package is one a
// caller acts on as current — which is why none of them stays direct.
//
// # A fleet record's writes are coalesced per key
//
// Every retry of a compare-and-swap on a fleet record (fleet.go) follows a
// LOST attempt: the store answered and somebody else's write landed — never the
// leader declining to compare a write still in flight, which is waited out
// before an attempt is called lost (below, and settle.go). So what
// bounds a loop is how many writers contend its key — [casAttempts]' rule for
// a lease, applied to the fleet's records — and a healthy contended write that
// runs out of attempts FAILS CLOSED as an outage, never as a refusal. The bound
// this replaced was sixteen, below the contract suite's own thirty-two
// concurrent charges, and the org's token counter — written by every round of
// every seat in the company — lost every one of its attempts through a leaf
// under load: a healthy round reported as a coordination outage.
//
// A bound can be sized for NODES and never for ROUNDS, which is why each node
// COALESCES the read-modify-writes it has in flight on one key into ONE
// compare-and-swap: a group commit per key, its batch decided in arrival order
// and each write answered exactly as a serial order would answer it alone, so
// the key's contenders are the nodes writing it however many rounds each has
// in flight. The creates answered by the record that won, the delivery claim
// and the unfollow are no read-modify-write and are not coalesced; what makes
// one of them repeat is a removal or a re-asserted follow between two of its
// round trips, far fewer than a node count, so the same bound covers them.
// The rate valve's bound is exact instead, where its limit is above the shared
// one: every attempt it loses is another node's increment, so it reads its
// window full within its limit of them ([FleetStore.Allow]).
// Between a lost attempt and the next, a loop waits a window counted in the
// ROUND TRIPS this node measures on the bucket rather than in a ceiling tuned
// on one topology, so a leaf or an external cluster spreads its writers by its
// own latency. casline.go is where each of these is argued: the line
// ([writeLines]), the head count it is bounded for ([fleetWriters]), the window
// ([casWindow]) and the round trip the window is counted in ([roundTrip]).
//
// # Every conditional write is DECIDED by the leader
//
// A compare-and-set here — a renew, a release, a claim's commit, an epoch's
// advance, every create (createKey's conditional publish at zero or at a
// marker's revision) and every fleet record's conditional update or removal —
// can be answered three ways, not two: it lands; it is refused on a
// comparison; or the leader declines to compare it at all because another
// write to the subject is still in flight, which decides nothing, since the
// write ahead may be this caller's own and already acknowledged. The client
// maps the last two to one revision mismatch. So every bucket handle this
// package holds is [settled]: its conditional writes are retried through
// [jsinflight.Decide] until the leader decides them, and one still undecided
// at its deadline is UNKNOWN rather than a race somebody else won (settle.go).
//
// # A write across a link waits as long as its bytes take to cross
//
// A node without the `data` role reaches these buckets across its leaf link,
// and every node on an external broker across the network, at a pace the
// engine allows to be as slow as the object store's floor, thirty seconds a
// mebibyte. A write whose caller set no deadline was held to the client's five
// seconds, and one whose value took longer to cross failed as the store being
// unavailable while its bytes were still on the link; an MCP listing of up to
// 896 KiB, which every node with seats writes, failed the same way under the
// fifteen seconds of the beat that wrote it, on any link below about 60 KiB a
// second at its bound. So every write of a settled bucket whose caller set no deadline
// waits the request term and, across a link, the crossing of what it carries
// for every send the client may make of it (settle.go), and each store is told
// how its connection reaches its buckets where it is opened ([Reach]: the
// engine states it from its broker's kind), since nothing it is handed can
// say.
package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("coord.kv")

// Arguments a caller got wrong: errors, not refusals, because a blank owner is
// neither "somebody else holds this" nor "a gate stopped you". An error is the
// contract's third answer, which a caller retries loudly.
var (
	// errNoJetStream is every opener's refusal of a missing client.
	errNoJetStream = errors.New("coord/kv: a JetStream client is required, built " +
		"over the stream's own connection in the API its broker speaks " +
		"(see internal/jsapi)")
	errNoOwner    = errors.New("coord/kv: owner is required")
	errBadTTL     = errors.New("coord/kv: ttl must be positive")
	errTTLTooLong = fmt.Errorf("coord/kv: ttl exceeds the seat lease bucket's age, "+
		"which is the TTL in force on the bucket rather than this node's "+
		"Config.TTL (Store.TTL reports it): %w", coord.ErrTTLTooLong)
)

const (
	// defaultBucketPrefix yields crewlet_leases, crewlet_duties and
	// crewlet_epochs.
	defaultBucketPrefix = "crewlet"

	leasesSuffix = "_leases"
	dutiesSuffix = "_duties"
	epochsSuffix = "_epochs"

	// minBucketTTL is nats-server's floor on a stream's MaxAge (server/stream.go),
	// checked here so Open fails with a named configuration error.
	minBucketTTL = 100 * time.Millisecond

	// maxReplicas is JetStream's cap on a stream's replica count.
	maxReplicas = 5
)

// casAttempts bounds the compare-and-swap retries of one lease operation.
//
// Every retry follows a LOST CAS: the store is answering and someone else won,
// so N callers on one key progress one per round, and the bound is a head
// count, not a timeout. The fleet's synchronized seat sweep and a node's own
// heartbeat, sweep and recovery re-claims put callers on one key at once; 64 is
// twice the contract suite's 32-way stampede and far above the placement
// model's per-group node count. Too high costs nothing; too low reports a
// healthy contended claim as UNKNOWN. Exhausting it is unknown, never a
// refusal ([contended]).
const casAttempts = 64

// validBucketName is nats.go's own bucket rule (jetstream/kv.go). Checking the
// prefix here names the problem instead of failing inside bucket creation.
var validBucketName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Config is what a Store needs at construction.
type Config struct {
	// TTL is the seat lease TTL this node ASKS the leases bucket for, as its
	// MaxAge. Required.
	//
	// It is the BUCKET's, not a call's (package doc, "One TTL per bucket"): a
	// longer per-call seat or presence TTL is refused, a shorter one honoured
	// against the store's clock. Every other class lives in the duty bucket,
	// bounded by coord.MaxDutyTTL. A bucket a peer created first is ADOPTED, so
	// [Store.TTL] reports the age actually in force; see [Open].
	TTL time.Duration

	// Clustered is whether this node's broker has PEERS — see the field of
	// the same name on [FleetConfig], and [jsprovision.Clustered] for why
	// it is not inferred from Replicas.
	Clustered bool

	// Replicas is the JetStream replica count for all three buckets; zero means
	// 1. A fleet wants 3: one replica hangs every seat's ownership on one broker
	// node.
	Replicas int

	// BucketPrefix names the three buckets "<prefix>_leases", "<prefix>_duties"
	// and "<prefix>_epochs"; empty means "crewlet". Two companies on one NATS
	// account need different prefixes, or the fleet-wide protocol gate couples
	// their claims.
	BucketPrefix string

	// Reach is how this node's connection reaches the buckets — in its own
	// process, or across a link — which decides how long a write's bytes
	// are given to cross ([Reach]). Required.
	Reach Reach
}

func (c *Config) normalize() error {
	if err := checkReach("Config", c.Reach); err != nil {
		return err
	}
	switch {
	case c.TTL <= 0:
		return fmt.Errorf("coord/kv: Config.TTL is required")
	case c.TTL < minBucketTTL:
		return fmt.Errorf("coord/kv: Config.TTL %v is below the broker's %v floor on a bucket TTL",
			c.TTL, minBucketTTL)
	}
	buckets := c.buckets()
	if err := buckets.normalize(); err != nil {
		return err
	}
	c.BucketPrefix, c.Replicas = buckets.BucketPrefix, buckets.Replicas
	return nil
}

// buckets is the part of the configuration the epochs bucket shares with the
// lease buckets: ONE prefix and one replica count, so a store and a counter
// opened from the same deployment name the same bucket (see [OpenEpochs]).
func (c *Config) buckets() EpochsConfig {
	return EpochsConfig{Clustered: c.Clustered, Replicas: c.Replicas, BucketPrefix: c.BucketPrefix,
		Reach: c.Reach}
}

// lane is one lease bucket and the rule its records expire by.
type lane struct {
	kv jetstream.KeyValue

	// stream names the bucket's backing stream, resolved at Open. storeNow reads
	// the clock through js.Stream, a fresh handle per call, because
	// KeyValue.Status caches StreamInfo on the shared handle with no lock
	// (nats.go 1.53.1, jetstream/stream.go Info): a data race. Per bucket, since
	// a record is stamped by ITS stream's leader and two streams may have two.
	stream string

	// read answers a record as the stream LEADER holds it, for every single-key
	// read of this bucket (package doc, "Every single-key read is the leader's").
	read *leaderReader

	// maxTTL is the longest TTL a claim on this bucket may ask for.
	maxTTL time.Duration

	// reapsAtMax reports that the bucket's age IS maxTTL, so a record claimed at
	// maxTTL expires by disappearing and needs no clock. True for seat leases;
	// the duty bucket's age is only ever raised, so it may exceed maxTTL.
	reapsAtMax bool
}

// Store is the JetStream KV coord.Backend.
type Store struct {
	js jetstream.JetStream

	// leases holds seat and presence leases, and nothing else.
	leases *lane
	// duties holds every other lease this build claims: every duty, and
	// every class a caller leases (see [Store.laneFor]).
	duties *lane
	// counter is the epochs bucket: every fencing token this store mints,
	// and every placement hint it keeps (epochs.go).
	counter *Epochs

	ttl time.Duration

	// gate is the protocol gate's view of the seat lease bucket (gate.go),
	// which is what every gated claim and FleetProtocolFloor judge instead
	// of listing the fleet's leases.
	gate *gateView

	// known is the last record this store wrote of each lease it holds
	// that cannot lapse while it can be read, at its revision: what makes
	// a renewal of one a single conditional write (known.go).
	known knownRecords

	// beforeMint (tests only; nil otherwise) runs as a won claim is about to
	// advance the counter, so the conformance suite can hold a claim mid-mint.
	beforeMint func(ctx context.Context, resource string)
	// beforeKnownWrite (tests only; nil otherwise) runs as a known record's
	// renewal is about to write, so a case can hold renewals in flight together.
	beforeKnownWrite func(resource string)
}

var _ coord.Backend = (*Store)(nil)

// Epochs is the counter this store mints its tokens from.
func (s *Store) Epochs() *Epochs { return s.counter }

// Open creates or adopts the three buckets and returns the backend: idempotent
// and safe concurrently from every node, since an existing bucket is ADOPTED,
// never rewritten. Every create goes through [openBucket], as on the rest of
// the boot path; the boot's sequence ceiling spans this and [OpenFleet], so
// internal/engine's attachCoordination applies it once over both.
//
// # What adopting costs, and why the TTL is read back rather than asserted
//
// The leases bucket's TTL IS the arbiter ([Store.validateTTL]), so its live
// value is carried and a difference logged, never resolved: a late node must
// neither redefine every lease's life nor believe a number not in force. The
// duty bucket's age is the exception ([openDuties]).
func Open(ctx context.Context, js jetstream.JetStream, cfg Config) (*Store, error) {
	if js == nil {
		return nil, errNoJetStream
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}

	// openBucket reads each bucket back, so the age and replicas in force come
	// with it, resolved once here before anything is concurrent (lane.stream).
	leases, leaseFacts, err := openBucket(ctx, js, cfg.Clustered, jetstream.KeyValueConfig{
		Bucket:      cfg.BucketPrefix + leasesSuffix,
		Description: "Crewlet seat and presence leases; the bucket TTL is the lease TTL and its expiry is the arbiter",
		TTL:         cfg.TTL,
		Replicas:    cfg.Replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("coord/kv: open %s: %w", cfg.BucketPrefix+leasesSuffix, err)
	}

	duties, dutyFacts, err := openDuties(ctx, js, cfg)
	if err != nil {
		return nil, err
	}

	counter, err := openEpochs(ctx, js, cfg.buckets())
	if err != nil {
		return nil, err
	}

	// THE TTL IN FORCE is the bucket's, not this node's config, when a peer
	// created it. Warned, not reconciled: the remedy is an operator's.
	if leaseFacts.age != cfg.TTL {
		log.WarnContext(ctx, "coord_kv_lease_ttl_differs",
			"bucket", leases.Bucket(), "in_force", leaseFacts.age, "this_node", cfg.TTL,
			"detail", "a peer created this bucket with a different lease TTL and "+
				"a booting node does not rewrite one; every lease on this node is "+
				"held to the TTL in force",
			"remedy", "make coordination.lease_ttl_seconds agree across the fleet, or "+
				"delete "+
				"the bucket while the fleet is down so the next boot re-creates it")
	}

	var readers [2]*leaderReader
	for i, bucket := range []jetstream.KeyValue{leases, duties} {
		if readers[i], err = newLeaderReader(js, bucket); err != nil {
			return nil, fmt.Errorf("coord/kv: address %s's leader: %w", bucket.Bucket(), err)
		}
	}

	log.DebugContext(ctx, "coord_kv_open", "leases", leases.Bucket(), "duties", duties.Bucket(),
		"epochs", counter.kv.Bucket(), "ttl", leaseFacts.age, "max_duty_ttl", coord.MaxDutyTTL)
	s := &Store{
		js: js,
		// The ceiling is the age IN FORCE, never this node's TTL: validateTTL
		// refuses against it and held() treats a claim AT it as reaped by the
		// bucket, so any other number accepts deadlines the bucket won't honour.
		leases: &lane{
			kv:         settled(js, leases, cfg.Reach),
			read:       readers[0],
			stream:     leaseFacts.stream,
			maxTTL:     leaseFacts.age,
			reapsAtMax: true,
		},
		// The duty bucket's ceiling is the CONTRACT's; openDuties made its age cover it.
		duties: &lane{
			kv:         settled(js, duties, cfg.Reach),
			read:       readers[1],
			stream:     dutyFacts.stream,
			maxTTL:     coord.MaxDutyTTL,
			reapsAtMax: false,
		},
		counter: counter,
		ttl:     leaseFacts.age,
	}
	s.gate = newGateView(s)
	return s, nil
}

// openDuties creates or adopts the duty bucket through [openBucket], like
// every bucket on the boot path, then raises its age to coord.MaxDutyTTL when
// it is younger, never lowering it.
//
// The one value this store applies to a bucket that exists: elsewhere a
// difference is a PREFERENCE the last node to boot may not redefine, but this
// age is the CEILING on the TTLs the bucket honours, so a shorter one left by
// an older build reaps live duty leases early and runs a duty on two nodes. A
// longer age costs nothing: every record is judged by its own deadline.
func openDuties(ctx context.Context, js jetstream.JetStream, cfg Config) (jetstream.KeyValue, bucketFacts, error) {
	name := cfg.BucketPrefix + dutiesSuffix
	want := jetstream.KeyValueConfig{
		Bucket: name,
		Description: "Crewlet duty leases and every other lease outside seats and presence; every " +
			"record is judged by its own deadline, and the bucket age is only ever raised to " +
			"cover the longest duty",
		TTL:      coord.MaxDutyTTL,
		Replicas: cfg.Replicas,
	}
	// No [reportRetention] here: an age ABOVE coord.MaxDutyTTL is this bucket's
	// healthy adopted state (a peer with a longer ceiling made it).
	bucket, facts, err := openBucket(ctx, js, cfg.Clustered, want)
	if err != nil {
		return nil, bucketFacts{}, fmt.Errorf("coord/kv: open %s: %w", name, err)
	}
	return raiseAge(ctx, js, cfg.Clustered, bucket, facts, want, "duty lease")
}

// raiseAge raises a bucket's age to want.TTL when it is younger and never
// lowers it ([openDuties] says why). facts are what [openBucket] read back as
// in force, and the facts returned are in force once this returns; holds names
// what the bucket keeps, for the error and the log.
func raiseAge(ctx context.Context, js jetstream.JetStream, clustered bool,
	bucket jetstream.KeyValue, facts bucketFacts, want jetstream.KeyValueConfig,
	holds string) (jetstream.KeyValue, bucketFacts, error) {

	// An age of zero is no age at all, which already outlives every record:
	// raising it would SHORTEN it.
	if facts.age == 0 || facts.age >= want.TTL {
		return bucket, facts, nil
	}
	// THE REPLICA COUNT IN FORCE, never this node's: openBucket refused one below
	// ours, so only a higher count is left, and writing ours would shrink it (the
	// durability loss observeBucket refuses).
	want.Replicas = facts.replicas
	// Its own budget and re-ask: a replicated stream-config write against the
	// same metadata group as every create on this boot, which nats.go's
	// five-second default would otherwise bound.
	writeCtx, cancelWrite := context.WithTimeout(ctx,
		jsprovision.Clustered(clustered).Budget())
	defer cancelWrite()
	var updated jetstream.KeyValue
	err := jsprovision.Place(writeCtx, jsprovision.Clustered(clustered).AskTerm(),
		func(ctx context.Context) error {
			var e error
			updated, e = js.UpdateKeyValue(ctx, want)
			return e
		}, nil)
	if err != nil {
		return nil, bucketFacts{}, fmt.Errorf("coord/kv: raise the age of %s from %v to %v so "+
			"it can hold a %s for its full TTL: %w", want.Bucket, facts.age, want.TTL, holds, err)
	}
	log.InfoContext(ctx, "coord_kv_bucket_age_raised", "bucket", want.Bucket,
		"holds", holds, "from", facts.age, "to", want.TTL)
	// The age in force is now the one written; the update carried the stream and
	// replica count forward.
	facts.age = want.TTL
	return updated, facts, nil
}

// bucketFacts is what one status read tells the boot path about a bucket:
// the stream behind it, and the two values IN FORCE on it rather than the ones
// this node asked for.
type bucketFacts struct {
	stream   string
	age      time.Duration
	replicas int
}

// readBucket reads them, re-asked through [jsprovision.Settle]: a bucket
// handle can come back before its metadata update is visible here, and one
// failed lookup in that window fails a clustered boot.
//
// Through a stream handle of each request's own, never the bucket's Status,
// which writes stream info into the shared handle unlocked: a read-back keeps
// a presumed-lost request outstanding while it asks again
// ([jsprovision.SettleWith]), so two answers can race.
func readBucket(ctx context.Context, js jetstream.JetStream, bucket jetstream.KeyValue) (bucketFacts, error) {
	info, err := jsprovision.Settle(ctx, func(ctx context.Context) (*jetstream.StreamInfo, error) {
		stream, err := js.Stream(ctx, bucketStream(bucket))
		if err != nil {
			return nil, err
		}
		return stream.CachedInfo(), nil
	})
	if err != nil {
		return bucketFacts{}, fmt.Errorf("coord/kv: read %s status: %w", bucket.Bucket(), err)
	}
	return bucketFacts{
		stream:   info.Config.Name,
		age:      info.Config.MaxAge,
		replicas: info.Config.Replicas,
	}, nil
}

// TTL reports the seat lease TTL IN FORCE: the seat lease bucket's own age,
// which expires a seat or presence lease and which [Store.validateTTL] holds
// every such claim to. It may not be this node's configured value, since the
// bucket is adopted ([Open]); [engine.effectiveLeaseTTL] says why a caller
// acquires with this. Every other lease's ceiling is coord.MaxDutyTTL.
func (s *Store) TTL() time.Duration { return s.ttl }

// each walks a whole bucket through [eachEntry], the one implementation.
func (s *Store) each(ctx context.Context, kv jetstream.KeyValue,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntry(ctx, s.js, kv, visit)
}

// create writes a key that must not hold a live value through [createKey];
// markers.go says why not the bucket handle's own.
func (s *Store) create(ctx context.Context, kv jetstream.KeyValue, key string, value []byte) (uint64, error) {
	return createKey(ctx, s.js, kv, key, value)
}

// eachUnder is [Store.each] over one resource class, narrowed at the broker —
// see [eachEntryUnder]. `what` names the listing a failure could not finish.
func (s *Store) eachUnder(ctx context.Context, kv jetstream.KeyValue, class coord.Class, what string,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntryUnder(ctx, s.js, kv, coord.DocumentFilter(string(class)), what, visit)
}

// checkClass refuses a class that cannot address a key: its filter would match
// NOTHING, which every caller reads as a class with no members. No sentinel:
// the classes are constants, so reaching this is a bug.
func checkClass(class coord.Class) error {
	if class.Valid() {
		return nil
	}
	return fmt.Errorf("coord/kv: %q is not a resource class: a class is the "+
		"leading segment of a resource name, so it must be non-empty and "+
		"contain no %q", string(class), coord.ResourceSeparator)
}

// laneFor is the bucket this build writes a resource's lease into: the seat
// lease bucket for what the seat TTL holds ([coord.HeldToSeatTTL]: seats and
// presence), the duty bucket for EVERY other class, so a caller's class (the
// tracker's 60-second walk claim) is not held to a shorter seat TTL.
func (s *Store) laneFor(resource string) *lane {
	if coord.HeldToSeatTTL(resource) {
		return s.leases
	}
	return s.duties
}

// laneOf is the one bucket a listing of one class reads: wherever laneFor
// writes it, so a heartbeat's membership read never opens the duty bucket.
func (s *Store) laneOf(class coord.Class) *lane {
	if class.HeldToSeatTTL() {
		return s.leases
	}
	return s.duties
}

// --- the lease surface ----------------------------------------------------

// TryAcquire claims resource for the owner, or reports that someone else holds
// it.
func (s *Store) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {
	c, err := s.tryAcquire(ctx, resource, opts, keptMeta(opts.Meta), nil)
	return c.lease, c.refused, err
}

// metaRule is the Meta a claim writes, computed in its round from the record it
// is about to replace: prior, nil where there is none, and held, whether prior
// is the claiming owner's live tenure, which the claim keeps.
type metaRule func(prior *entry, held bool) (map[string]any, error)

// tombMark edits the tombstone a release or a claim's give-back leaves over prior,
// before the one write that leaves it: its Meta, keeping the keys it does not set.
type tombMark func(prior entry, tomb *leaseValue) error

// keptMeta is TryAcquire's rule for the Meta a claim writes: the caller's, or,
// where it hands none, the prior record's. A renew that forgets a node's profile
// must not un-label it, which peers read as matching no placement.
func keptMeta(meta map[string]any) metaRule {
	return func(prior *entry, _ bool) (map[string]any, error) {
		if len(meta) == 0 && prior != nil {
			return prior.value.Meta, nil
		}
		return meta, nil
	}
}

// claimed is what [Store.tryAcquire] decided: the contract's answer, and what a
// typed claim reads beside it with no second round trip.
type claimed struct {
	lease   *coord.Lease
	refused coord.Refusal

	// fresh reports that a grant began a new tenure rather than keeping the
	// owner's live one.
	fresh bool

	// refuser is the record a coord.RefusedHeld was decided on, and the clock it
	// was judged on; mine is nil where the read-back found none.
	refuser snapshot
}

// tryAcquire is [Store.TryAcquire] with the Meta a claim writes computed by
// metaOf, and with mark editing the tombstone a failed claim gives back.
func (s *Store) tryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions,
	metaOf metaRule, mark tombMark) (claimed, error) {

	l := s.laneFor(resource)
	if err := s.validateTTL(l, resource, opts.Owner, opts.TTL); err != nil {
		return claimed{}, err
	}
	// A Meta past what a lease carries is refused before anything is sent, as
	// the twin does: a definite answer, which [unavailable] passes unmarked.
	if err := coord.HoldLeaseMeta(resource, opts.Meta); err != nil {
		return claimed{}, err
	}
	protocol := opts.EffectiveProtocol()
	key := encodeResource(resource)

	// sibling: the last round met this owner's own claim still minting, which
	// names the answer when the rounds run out.
	var sibling bool
	for attempt := range casAttempts {
		// The first round starts from what this store last wrote, where it names
		// this owner (known.go): a self-renewing claim writes at that revision
		// without reading, and a record that moved refuses the write.
		snap, err := s.claimSnapshot(ctx, l, resource, opts.Owner, attempt == 0)
		if err != nil {
			return claimed{}, err
		}

		mine := snap.mine
		held := false
		if mine != nil {
			if held, err = s.held(ctx, *mine, snap.clock); err != nil {
				return claimed{}, err
			}
		}
		if held && mine.value.Owner != opts.Owner {
			// A PEER HOLDS IT (coord.RefusedHeld) whatever the gate says, so no gate
			// is judged and a sweep over peers' seats never starts the gate view.
			return claimed{refused: coord.RefusedHeld, refuser: snap}, nil
		}
		// The gate, fleet-wide (coord.ProtocolGateCounts): the disagreement is
		// about what HOLDING A SEAT means, not about this resource. It only looks
		// for a LOWER protocol, so a lower-protocol node is never blocked.
		if !opts.Ungated {
			blocked, gateErr := s.gate.blocked(ctx, protocol)
			if gateErr != nil {
				return claimed{}, gateErr
			}
			if blocked {
				return claimed{refused: coord.RefusedProtocol}, nil
			}
		}
		sibling = held && mine.value.Epoch == claimingEpoch
		if sibling {
			// THIS owner's own concurrent claim holds the record uncommitted, and
			// taking it would fence the owner against itself. Go round: the sibling
			// is one round trip from committing or giving back ([Store.abandon]).
			// If it died, the attempts run out, this answers UNKNOWN, and the
			// record lapses on its TTL.
			continue
		}
		value := leaseValue{
			Resource:  resource,
			Owner:     opts.Owner,
			TTLNanos:  int64(opts.TTL),
			Protocol:  protocol,
			Preferred: opts.Preferred,
		}
		if mine != nil && opts.Preferred == "" {
			// A claim naming no node keeps the last DELIBERATE placement hint.
			value.Preferred = mine.value.Preferred
		}
		// The bound holds what is written, whoever computed it: refused before
		// this round writes anything, as at the top.
		if value.Meta, err = metaOf(mine, held); err != nil {
			return claimed{}, err
		}
		if err = coord.HoldLeaseMeta(resource, value.Meta); err != nil {
			return claimed{}, err
		}

		if held {
			// An unbroken same-owner hold (also the renew path) keeps its epoch and
			// its tenure's start: nothing was ever unowned.
			value.Epoch = mine.value.Epoch
			value.AcquiredAt = mine.value.AcquiredAt
			if opts.Preferred != "" && opts.Preferred != mine.value.Preferred {
				// The hint moved within the tenure: pin it on the epoch record FIRST,
				// so that bucket is never behind (PreferredResources reads only it).
				if err = s.counter.pin(ctx, coord.CounterResource(resource), opts.Preferred); err != nil {
					return claimed{}, err
				}
			}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			data, err := encodeValue(value)
			if err != nil {
				return claimed{}, err
			}
			// Update at the read revision is the fencing CAS: it fails if
			// anything at all wrote the record since we read it.
			if _, err := l.kv.Update(ctx, key, data, mine.revision); err != nil {
				s.known.forgetAt(resource, mine.revision)
				if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
					continue
				}
				return claimed{}, unavailable("renew lease "+resource, err)
			}
			return s.settle(ctx, l, resource, value, opts, protocol, false)
		}

		// Takeover, or this owner re-claiming after its lease lapsed: both mint a
		// new token, fencing the gap nothing covered. Three writes, in the order
		// the package doc defends ("A claim is three writes, and the order
		// carries the invariant"):
		//
		//  1. WIN THE RECORD claiming (epoch 0, no token): the exclusivity CAS.
		//  2. ADVANCE THE COUNTER ([coord.CounterResource]), uncontended.
		//  3. COMMIT the token into the record already held.
		//
		// A failure after step 1 GIVES THE RECORD BACK: every failure but a lost
		// CAS hands [Store.abandon] the claim it wrote, since even a write whose
		// answer was lost may have landed.
		claiming := value
		claiming.Epoch = claimingEpoch
		claiming.Claim = uuid.NewString()
		claimData, err := encodeValue(claiming)
		if err != nil {
			return claimed{}, err
		}
		var rev uint64
		if mine == nil {
			if rev, err = s.create(ctx, l.kv, key, claimData); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				err = unavailable("create lease "+resource, err)
				s.abandon(ctx, l, resource, claiming, mine, mark, err)
				return claimed{}, err
			}
		} else {
			if rev, err = l.kv.Update(ctx, key, claimData, mine.revision); err != nil {
				if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
					continue
				}
				err = unavailable("claim lease "+resource, err)
				s.abandon(ctx, l, resource, claiming, mine, mark, err)
				return claimed{}, err
			}
		}

		// The tenure starts at the store's own timestamp on the record just won,
		// read back by revision: the store's clock is the only shared one, and
		// settle's read-back sees the commit, which must already carry the stamp
		// for renewals to copy. Once per tenure change, before the counter
		// advances, so a record already taken costs no token.
		acquiredAt, taken, err := s.claimedAt(ctx, l, resource, rev)
		if err != nil {
			// No token was minted; the claiming record goes back, or a read the
			// caller gave up on would hold the resource against its owner for a TTL.
			s.abandon(ctx, l, resource, claiming, mine, mark, err)
			return claimed{}, err
		}
		if taken {
			continue
		}
		value.AcquiredAt = acquiredAt

		// value.Preferred, not opts.Preferred: a claim naming no node keeps the
		// lapsed record's hint, and re-pins it if the two drifted.
		if s.beforeMint != nil {
			s.beforeMint(ctx, resource)
		}
		// On [coord.CounterResource]'s counter: a taker lease names a process, so
		// it shares its class's one rather than leave a counter record per start.
		epoch, hint, err := s.counter.advance(ctx, coord.CounterResource(resource), value.Preferred)
		if err != nil {
			// A counter value taken is a harmless gap; the claiming record left
			// behind would hold the resource for its TTL.
			s.abandon(ctx, l, resource, claiming, mine, mark, err)
			return claimed{}, err
		}
		value.Epoch = epoch
		value.Preferred = hint
		data, err := encodeValue(value)
		if err != nil {
			s.abandon(ctx, l, resource, claiming, mine, mark, err)
			return claimed{}, err
		}
		if _, err := l.kv.Update(ctx, key, data, rev); err != nil {
			// Our claiming record lapsed and was taken; the minted token is a
			// harmless gap.
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			// UNKNOWN whether the commit landed: if it did, abandon leaves the
			// claimless record and the next claim renews it; if not, the claiming
			// record goes back.
			err = unavailable("commit lease "+resource, err)
			s.abandon(ctx, l, resource, claiming, mine, mark, err)
			return claimed{}, err
		}
		return s.settle(ctx, l, resource, value, opts, protocol, true)
	}
	if sibling {
		// THE LAST ROUND MET THIS OWNER'S OWN CLAIM STILL MINTING: the
		// unknown arm, named for what it is (see coord.ErrClaimInFlight).
		return claimed{}, fmt.Errorf("%w (%s, by %s)", coord.ErrClaimInFlight, resource, opts.Owner)
	}
	return claimed{}, contended("TryAcquire", resource)
}

// claimedAt is the store's timestamp on revision rev of resource's record,
// which this caller just wrote. taken reports the revision is gone (the
// record lapsed and was taken), so the claim goes round as after a lost CAS.
func (s *Store) claimedAt(ctx context.Context, l *lane, resource string, rev uint64) (time.Time, bool, error) {
	kve, err := l.read.at(ctx, encodeResource(resource), rev)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return time.Time{}, true, nil
	}
	if err != nil {
		// The caller gives the claiming record back ([Store.abandon]):
		// no token has been minted, so nothing is stranded either way.
		return time.Time{}, false, unavailable("read back the claim on "+resource, err)
	}
	return kve.Created().UTC(), false, nil
}

// settle reads the record back and re-runs the protocol gate. The read-back
// is how the store's own timestamp reaches coord.Lease.ExpiresAt, since the
// write returns only a revision. The gated re-check goes through the view's
// barrier (gate.go), so a lower-protocol record written before our claim is
// never missed.
func (s *Store) settle(
	ctx context.Context,
	l *lane,
	resource string,
	want leaseValue,
	opts coord.AcquireOptions,
	protocol int,
	fresh bool,
) (claimed, error) {
	snap, err := s.readForClaim(ctx, l, resource)
	if err != nil {
		return claimed{}, err
	}
	mine := snap.mine
	if mine == nil || mine.value.Owner != want.Owner || mine.value.Epoch != want.Epoch {
		// Superseded between write and read-back (only with a TTL that lapses
		// inside a round trip): by the time we looked, it was not ours.
		return claimed{refused: coord.RefusedHeld, refuser: snap}, nil
	}
	if !opts.Ungated {
		blocked, err := s.gate.blocked(ctx, protocol)
		if err != nil {
			return claimed{}, err
		}
		if blocked {
			if err := s.yield(ctx, resource, want, protocol, fresh); err != nil {
				return claimed{}, err
			}
			return claimed{refused: coord.RefusedProtocol}, nil
		}
	}
	// THE READ-BACK IS WHAT IS KNOWN, never the value written: it is the
	// leader's newest, so a sibling's renewal that landed in between is the
	// revision the next renewal writes at.
	s.known.remember(l, resource, mine.revision, mine.value)
	return claimed{lease: mine.lease(), fresh: fresh}, nil
}

// claimSnapshot is what a claim's round decides on: on the first round, the
// record this store last wrote of resource where it is known and names owner
// ([knownRecords.lookup]), and otherwise the record as the leader holds it
// ([Store.readForClaim]).
func (s *Store) claimSnapshot(ctx context.Context, l *lane, resource, owner string,
	first bool) (snapshot, error) {

	if first {
		if mine, ok := s.known.lookup(l, resource, owner); ok {
			return snapshot{mine: &mine, clock: s.newClock()}, nil
		}
	}
	return s.readForClaim(ctx, l, resource)
}

// yield gives back a NEW claim the gate's re-check refused. A re-claim of a
// lease this owner already held is kept: the gate stops a higher-protocol node
// TAKING work, and dropping a seat mid-turn prevents nothing; the refusal
// still stops the next sweep.
func (s *Store) yield(ctx context.Context, resource string, want leaseValue, protocol int, fresh bool) error {
	if !fresh {
		return nil
	}
	if _, err := s.Release(ctx, resource, want.Owner, want.Epoch); err != nil {
		return err
	}
	log.InfoContext(ctx, "coord_kv_claim_yielded_to_older_peer", "resource", resource, "owner", want.Owner,
		"epoch", want.Epoch, "protocol", protocol)
	return nil
}

// abandon gives back the claiming record a TryAcquire wrote and could not
// commit, so the resource is claimable at once rather than dark for its TTL;
// cause is why the claim failed. A teardown, so it runs WithoutCancel (the
// failure undone is often the cancellation), bounded by the client's API
// timeout instead.
//
// [Store.giveBack] is EXACT: it takes back only a record carrying this call's
// claim, and where the claiming write has not landed yet (a cancelled
// caller's request can land after the give-back looked) it writes against
// that write's own expectation, so the late write is refused. A refused
// give-back leaves the record to lapse on its TTL, and says so. mark is the
// claim's, nil for TryAcquire's.
func (s *Store) abandon(ctx context.Context, l *lane, resource string, claiming leaseValue,
	over *entry, mark tombMark, cause error) {

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.js.Options().DefaultTimeout)
	defer cancel()
	gaveBack, err := s.giveBack(ctx, l, resource, claiming, over, mark)
	switch {
	case err != nil:
		log.WarnContext(ctx, "coord_kv_claim_abandon_failed", "resource", resource,
			"owner", claiming.Owner, "cause", cause.Error(), "error", err.Error(),
			"detail", "the claim failed part way and its record could not be given back either, "+
				"so the resource reads as held until the record's TTL lapses")
	case gaveBack:
		log.InfoContext(ctx, "coord_kv_claim_abandoned", "resource", resource,
			"owner", claiming.Owner, "cause", cause.Error())
	}
}

// giveBack writes [Store.Release]'s tombstone over resource's record if it is
// this call's claiming record, or, to fence that write, over the key it
// expected (no live record for a create, the same revision for a takeover);
// it reports whether it wrote. over is the record the claiming write was made
// over, nil for a create. Each write is at the revision read, so a record
// that moved in between is read again. mark, where set, edits the tombstone
// written over a record that is there.
func (s *Store) giveBack(ctx context.Context, l *lane, resource string, claiming leaseValue,
	over *entry, mark tombMark) (bool, error) {

	key := encodeResource(resource)
	for range casAttempts {
		e, err := s.readOne(ctx, l, resource)
		if err != nil {
			return false, err
		}
		var (
			tomb   leaseValue
			landed bool
		)
		switch {
		case e != nil && e.value.Epoch == claimingEpoch && e.value.Claim == claiming.Claim:
			tomb, landed = e.value, true
		case over == nil && e == nil:
			tomb = claiming
		case over != nil && e != nil && e.revision == over.revision:
			tomb = e.value
		default:
			// The key moved on without this call's write: committed, taken, or a
			// takeover's record gone, which refuses that write by its expectation.
			return false, nil
		}
		tomb.Owner, tomb.Claim = "", ""
		tomb.TTLNanos = int64(l.maxTTL)
		if mark != nil && e != nil {
			if err = mark(*e, &tomb); err != nil {
				return false, err
			}
		}
		data, err := encodeValue(tomb)
		if err != nil {
			return false, err
		}
		if e == nil {
			_, err = s.create(ctx, l.kv, key, data)
		} else {
			_, err = l.kv.Update(ctx, key, data, e.revision)
		}
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, jetstream.ErrKeyExists),
			errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			// Moved between the read and the write — possibly by this
			// call's own claiming write landing. Read it again.
			continue
		default:
			if landed {
				return false, unavailable("give back the claim on "+resource, err)
			}
			return false, unavailable("fence the claim on "+resource, err)
		}
	}
	return false, contended("giveBack", resource)
}

// Renew extends a lease the caller still holds at this epoch. Never gated:
// refusing during a mixed-version window would drop a seat mid-turn and
// prevent nothing. Where this store wrote the record last ([knownRecords]),
// one write at its revision decides; otherwise, or once that write loses its
// comparison, it reads the record and decides.
func (s *Store) Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error) {
	w, err := s.renew(ctx, resource, owner, epoch, ttl)
	return w != nil, err
}

// writtenRecord is a record a write of this store left: its value and the revision
// the broker gave it.
type writtenRecord struct {
	revision uint64
	value    leaseValue
}

// renew is [Store.Renew], answering the record it wrote, or nil where the
// lease is definitively not the caller's.
func (s *Store) renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (*writtenRecord, error) {
	l := s.laneFor(resource)
	if err := s.validateTTL(l, resource, owner, ttl); err != nil {
		return nil, err
	}
	key := encodeResource(resource)

	if known, ok := s.known.lookup(l, resource, owner); ok && known.value.Epoch == epoch {
		if w, decided, err := s.renewKnown(ctx, l, key, known, ttl); decided {
			return w, err
		}
	}

	for range casAttempts {
		e, err := s.readOne(ctx, l, resource)
		if err != nil {
			return nil, err
		}
		// A lapsed lease is NOT renewable: only a re-acquire, minting a new
		// epoch, fences the gap. Owner AND epoch, because a zombie heartbeat
		// from before the gap carries the same owner.
		if e == nil || e.value.Owner != owner || e.value.Epoch != epoch {
			s.known.forgetTenure(resource, owner, epoch)
			return nil, nil
		}
		live, err := s.tenure(ctx, *e, s.newClock())
		if err != nil || !live {
			if err == nil {
				s.known.forgetTenure(resource, owner, epoch)
			}
			return nil, err
		}
		value := e.value
		value.TTLNanos = int64(ttl)
		data, err := encodeValue(value)
		if err != nil {
			return nil, err
		}
		rev, err := l.kv.Update(ctx, key, data, e.revision)
		if err != nil {
			// A lost CAS is NOT a lost lease: our own concurrent
			// heartbeat writes the same record. Re-read and re-decide
			// rather than telling the caller to shed every seat.
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return nil, unavailable("renew "+resource, err)
		}
		s.known.remember(l, resource, rev, value)
		return &writtenRecord{revision: rev, value: value}, nil
	}
	return nil, contended("Renew", resource)
}

// renewKnown renews the record this store last wrote ([knownRecords]) with one
// write at its revision, answering the record it wrote. decided is false when
// that write lost its comparison (the record moved, which says nothing about
// who holds it), so the caller reads and decides; any other failure is unknown.
func (s *Store) renewKnown(ctx context.Context, l *lane, key string, known entry,
	ttl time.Duration) (w *writtenRecord, decided bool, err error) {

	value := known.value
	value.TTLNanos = int64(ttl)
	data, err := encodeValue(value)
	if err != nil {
		return nil, true, err
	}
	if s.beforeKnownWrite != nil {
		s.beforeKnownWrite(known.resource)
	}
	rev, err := l.kv.Update(ctx, key, data, known.revision)
	switch {
	case err == nil:
		s.known.remember(l, known.resource, rev, value)
		return &writtenRecord{revision: rev, value: value}, true, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
		s.known.forgetAt(known.resource, known.revision)
		return nil, false, nil
	default:
		// It may have landed, at a revision nobody told us.
		s.known.forgetAt(known.resource, known.revision)
		return nil, true, unavailable("renew "+known.resource, err)
	}
}

// Release gives up a lease, predicated on owner AND epoch. It writes a
// tombstone (an empty owner, which every reader treats as unheld) rather than
// deleting the key, and never touches the epochs bucket: the counter must be
// monotonic for the RESOURCE's lifetime, not a record's.
func (s *Store) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	w, err := s.release(ctx, resource, owner, epoch, nil)
	return w != nil, err
}

// release is [Store.Release] with mark, where set, editing the tombstone before
// its write, answering the tombstone it wrote, or nil where the lease was not
// the caller's to give up.
func (s *Store) release(ctx context.Context, resource, owner string, epoch int64, mark tombMark) (*writtenRecord, error) {
	if err := validate(resource, owner); err != nil {
		return nil, err
	}
	l := s.laneFor(resource)
	key := encodeResource(resource)
	// Nothing renews a tenure being given up, whatever the store answers.
	s.known.forgetTenure(resource, owner, epoch)

	for range casAttempts {
		e, err := s.readOne(ctx, l, resource)
		if err != nil {
			return nil, err
		}
		// Unqualified, a departing owner or a straggler from the previous
		// tenure would clear its SUCCESSOR's live lease mid-turn.
		if e == nil || e.value.Owner != owner || e.value.Epoch != epoch {
			return nil, nil
		}
		live, err := s.tenure(ctx, *e, s.newClock())
		if err != nil || !live {
			return nil, err
		}
		tomb := e.value
		tomb.Owner = ""
		// The tombstone's full TTL means no reader needs a clock (an empty owner
		// is unheld; MaxAge reaps it), and the kept hint and epoch keep a
		// just-released resource's placement.
		tomb.TTLNanos = int64(l.maxTTL)
		if mark != nil {
			if err = mark(*e, &tomb); err != nil {
				return nil, err
			}
		}
		data, err := encodeValue(tomb)
		if err != nil {
			return nil, err
		}
		rev, err := l.kv.Update(ctx, key, data, e.revision)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return nil, unavailable("release "+resource, err)
		}
		log.DebugContext(ctx, "coord_kv_lease_released", "resource", resource, "owner", owner, "epoch", epoch)
		return &writtenRecord{revision: rev, value: tomb}, nil
	}
	return nil, contended("Release", resource)
}

// Get reads a resource's live lease, or nil when nothing holds it. A lapsed or
// released record reads as nil like an unclaimed one, so a caller can act on a
// returned Lease without checking a deadline against its own clock.
func (s *Store) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if err := coord.CheckResource(resource); err != nil {
		return nil, err
	}
	e, err := s.readOne(ctx, s.laneFor(resource), resource)
	if err != nil || e == nil {
		return nil, err
	}
	live, err := s.tenure(ctx, *e, s.newClock())
	if err != nil || !live {
		return nil, err
	}
	return e.lease(), nil
}

// ListOwned returns the live leases this owner holds; a drain watches it reach
// empty. Not narrowed at the broker: the owner is in the record, not the key.
func (s *Store) ListOwned(ctx context.Context, owner string) ([]coord.Lease, error) {
	return s.listLive(ctx, []*lane{s.duties, s.leases}, s.scan,
		func(e entry) bool { return e.value.Owner == owner })
}

// ListLive returns the live leases of one class. ListLive(coord.ClassNode) is
// the membership read: live presence leases are the fleet size the seats are
// divided by. The BROKER narrows it to the class, a subject token of the key,
// and it reads only [Store.laneOf]'s bucket.
func (s *Store) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	scan := func(ctx context.Context, l *lane) ([]entry, error) { return s.scanIn(ctx, l, class) }
	// No filter: the broker answered with this class alone.
	return s.listLive(ctx, []*lane{s.laneOf(class)}, scan, func(entry) bool { return true })
}

// Now reads the clock of [Store.laneOf]'s bucket for class, its stream
// leader's timestamp ([Store.storeNow]): the clock each record's created, and
// so its ExpiresAt, is stamped on. Its own round trip, never one a listing
// makes: a seat or presence listing reads no clock (liveWhilePresent), and only
// a reader saying how long a lease has left asks for it.
func (s *Store) Now(ctx context.Context, class coord.Class) (time.Time, error) {
	if err := checkClass(class); err != nil {
		return time.Time{}, err
	}
	return s.storeNow(ctx, s.laneOf(class))
}

// listLive reads the live leases keep accepts across lanes, which hold
// disjoint classes ([Store.laneFor]), so nothing is listed twice. scan is the
// caller's: a class listing narrows at the broker, an owner listing cannot.
func (s *Store) listLive(ctx context.Context, lanes []*lane,
	scan func(context.Context, *lane) ([]entry, error), keep func(entry) bool) ([]coord.Lease, error) {
	clk := s.newClock()
	var out []coord.Lease
	for _, l := range lanes {
		all, err := scan(ctx, l)
		if err != nil {
			return nil, err
		}
		for _, e := range all {
			if !keep(e) {
				continue
			}
			live, err := s.tenure(ctx, e, clk)
			if err != nil {
				return nil, err
			}
			if live {
				out = append(out, *e.lease())
			}
		}
	}
	// Sorted, so a downstream ordering bug does not reproduce once in ten runs.
	slices.SortFunc(out, func(a, b coord.Lease) int { return strings.Compare(a.Resource, b.Resource) })
	return out, nil
}

// PreferredResources returns resources of this class whose hint names nodeID,
// LAPSED ones included: the hint exists for a node back from a restart finding
// the seats it had warm. So it reads only the EPOCHS bucket, which has no TTL:
// every live lease has an epoch record, and every hint change writes it BEFORE
// the lease record, so that bucket is never behind, whichever lane the lease
// is in.
func (s *Store) PreferredResources(ctx context.Context, class coord.Class, nodeID string) (map[string]struct{}, error) {
	records, err := s.scanResourcesIn(ctx, class)
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	for _, r := range records {
		if r.Preferred == nodeID {
			out[r.Resource] = struct{}{}
		}
	}
	return out, nil
}

// FleetProtocolFloor returns the lowest protocol among the live leases the
// gate counts (coord.ProtocolGateCounts), and whether there were any: the
// gate's observability half, asked only after a [coord.RefusedProtocol] to
// name the protocol that stopped it. It counts exactly what the GATE counts
// (held presence and seat records, claiming ones included), not what Get or a
// listing returns, so it names neither a peer that is not there nor an older
// build nothing is refused for. Judged by the gate's view (gate.go), at a
// gate's cost.
func (s *Store) FleetProtocolFloor(ctx context.Context) (int, bool, error) {
	return s.gate.floor(ctx)
}

// --- reading --------------------------------------------------------------

// snapshot is what one claim decision reads: the record it is about, and the
// clock that record is judged against.
type snapshot struct {
	mine  *entry
	clock *clock
}

// readForClaim reads the record a claim is about from the stream LEADER. The
// claim writes at its revision and settle judges its own write by it, so it
// must be the quorum's newest: a copy one write behind shows settle the
// record this call replaced, refusing a lease this owner holds. The gate is
// not in it: the gate's view answers that (gate.go).
func (s *Store) readForClaim(ctx context.Context, l *lane, resource string) (snapshot, error) {
	mine, err := s.readOne(ctx, l, resource)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{mine: mine, clock: s.newClock()}, nil
}

// readOne reads a single lease record from a bucket, as the stream LEADER holds
// it. A missing key is (nil, nil); a read the leader did not answer is
// unknown, never missing.
func (s *Store) readOne(ctx context.Context, l *lane, resource string) (*entry, error) {
	kve, err := l.read.live(ctx, encodeResource(resource))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable("read lease "+resource, err)
	}
	e, ok := decodeEntry(kve, l)
	if !ok {
		// Answering "unheld" would invite a takeover of a lease that may be
		// live, so this is the third answer: no answer.
		return nil, fmt.Errorf("%w: lease record for %s is undecodable",
			coord.ErrUnavailable, resource)
	}
	return &e, nil
}

// scan reads every lease record of one bucket in one ordered pass, not a key
// listing plus a Get per key, whose cost would grow with the company
// ([eachEntry]).
func (s *Store) scan(ctx context.Context, l *lane) ([]entry, error) {
	return s.collect(ctx, l, func(visit func(jetstream.KeyValueEntry) error) error {
		return s.each(ctx, l.kv, visit)
	})
}

// scanIn is [Store.scan] over one class, narrowed at the BROKER rather than by
// a test here on what it sent.
func (s *Store) scanIn(ctx context.Context, l *lane, class coord.Class) ([]entry, error) {
	if err := checkClass(class); err != nil {
		return nil, err
	}
	return s.collect(ctx, l, func(visit func(jetstream.KeyValueEntry) error) error {
		return s.eachUnder(ctx, l.kv, class, "the live "+string(class)+" leases", visit)
	})
}

func (s *Store) collect(ctx context.Context, l *lane,
	walk func(func(jetstream.KeyValueEntry) error) error) ([]entry, error) {

	var out []entry
	err := walk(func(kve jetstream.KeyValueEntry) error {
		e, ok := decodeEntry(kve, l)
		if !ok {
			// A listing that invented a resource name would put a seat
			// nobody owns into a capacity calculation, so an unreadable
			// record is skipped — loudly.
			log.WarnContext(ctx, "coord_kv_undecodable_record", "bucket", l.kv.Bucket(), "key", kve.Key())
			return nil
		}
		// Once per resource: the walk hands each key over once, at the
		// newest revision the listing read, and a resource is one key.
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Sorted by RESOURCE: the walk yields KEY order, and a key's encoding
	// (another separator, escaped segments) orders differently.
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.resource, b.resource) })
	return out, nil
}

// scanResourcesIn reads the persistent resource records of one class,
// narrowed at the broker: the epochs bucket has NO TTL and holds every
// resource ever leased, and the seat sweep reads it on every tick.
func (s *Store) scanResourcesIn(ctx context.Context, class coord.Class) ([]resourceValue, error) {
	if err := checkClass(class); err != nil {
		return nil, err
	}
	byResource := map[string]resourceValue{}
	err := s.eachUnder(ctx, s.counter.kv, class, "the "+string(class)+" placement hints",
		func(kve jetstream.KeyValueEntry) error {
			resource, ok := decodeResource(kve.Key())
			if !ok {
				log.WarnContext(ctx, "coord_kv_undecodable_key", "bucket", s.counter.kv.Bucket(), "key", kve.Key())
				return nil
			}
			var v resourceValue
			//nolint:nilerr // An undecodable epoch record is SKIPPED — loudly,
			// as warned, rather than raised: a hint is advisory, and losing one costs
			// a seat its stickiness where failing the read stops all placement.
			if err := json.Unmarshal(kve.Value(), &v); err != nil {
				log.WarnContext(ctx, "coord_kv_undecodable_record", "bucket", s.counter.kv.Bucket(), "key", kve.Key())
				return nil
			}
			v.Resource = resource
			byResource[resource] = v
			return nil
		})
	if err != nil {
		return nil, err
	}
	out := make([]resourceValue, 0, len(byResource))
	for _, v := range byResource {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b resourceValue) int { return strings.Compare(a.Resource, b.Resource) })
	return out, nil
}

func decodeEntry(kve jetstream.KeyValueEntry, l *lane) (entry, bool) {
	resource, ok := decodeResource(kve.Key())
	if !ok {
		return entry{}, false
	}
	var v leaseValue
	if err := json.Unmarshal(kve.Value(), &v); err != nil {
		return entry{}, false
	}
	return entry{
		resource: resource,
		lane:     l,
		revision: kve.Revision(),
		created:  kve.Created().UTC(),
		value:    v,
	}, true
}

// --- expiry ---------------------------------------------------------------

// held reports whether a peer must not take a record: it names an owner and
// its deadline has not passed. This CLAIM-blocking question counts a record in
// the claiming state (see TryAcquire): its claimant holds the resource as
// firmly as a committed tenure.
func (s *Store) held(ctx context.Context, e entry, clk *clock) (bool, error) {
	if e.value.Owner == "" {
		// A tombstone: Release expired the record in place.
		return false, nil
	}
	// A seat lease at the bucket's full age expires by DISAPPEARING (MaxAge),
	// so a readable one is live with no clock: the production path for seats
	// and presence.
	if liveWhilePresent(e.lane, e.value) {
		return true, nil
	}
	now, err := clk.now(ctx, e.lane)
	if err != nil {
		return false, err
	}
	return e.created.Add(e.value.ttl()).After(now), nil
}

// tenure reports whether a record is a LEASE: held, with a committed fencing
// token. Only a tenure becomes a coord.Lease; a claiming record's epoch 0 is
// no token, and every conditional write would match it on an unset column.
func (s *Store) tenure(ctx context.Context, e entry, clk *clock) (bool, error) {
	if e.value.Epoch == claimingEpoch {
		return false, nil
	}
	return s.held(ctx, e, clk)
}

// clock is the store's clock for one decision, read at most once per bucket
// and only when a record needs it: a bucket's StreamInfo timestamp, never
// time.Now, since nodes comparing their own wall clocks to a store deadline
// hand one seat to two after an NTP step. Read AFTER the records, it can
// judge a just-renewed record lapsed, which costs no exclusivity: every
// takeover is a CAS at the read revision. Liveness decides what to ATTEMPT;
// the CAS decides what happens.
type clock struct {
	s  *Store
	at map[*lane]time.Time
}

func (s *Store) newClock() *clock { return &clock{s: s} }

func (c *clock) now(ctx context.Context, l *lane) (time.Time, error) {
	if at, ok := c.at[l]; ok {
		return at, nil
	}
	at, err := c.s.storeNow(ctx, l)
	if err != nil {
		return time.Time{}, err
	}
	if c.at == nil {
		c.at = map[*lane]time.Time{}
	}
	c.at[l] = at
	return at, nil
}

func (s *Store) storeNow(ctx context.Context, l *lane) (time.Time, error) {
	// The leader's stream info carries the server's timestamp ([leaderInfo]);
	// lane.stream says why not KeyValue.Status.
	info, err := leaderInfo(ctx, s.js, l.stream)
	if err != nil {
		return time.Time{}, unavailable("read the store clock", err)
	}
	if info == nil || info.TimeStamp.IsZero() {
		return time.Time{}, fmt.Errorf("%w: the broker did not report a stream timestamp",
			coord.ErrUnavailable)
	}
	return info.TimeStamp.UTC(), nil
}

// --- errors ---------------------------------------------------------------

// validate checks the identity every call carries. The resource goes through
// [coord.CheckResource], not an emptiness test: an empty SEGMENT builds a key
// nothing decodes, so the lease is listed nowhere and reads as a free seat.
func validate(resource, owner string) error {
	if err := coord.CheckResource(resource); err != nil {
		return err
	}
	if owner == "" {
		return errNoOwner
	}
	return nil
}

// validateTTL adds the deadline check for the calls that set one.
func (s *Store) validateTTL(l *lane, resource, owner string, ttl time.Duration) error {
	if err := validate(resource, owner); err != nil {
		return err
	}
	if ttl <= 0 {
		// A non-positive TTL would mint a lease that is already lapsed,
		// which reads downstream as a seat nobody can hold.
		return errBadTTL
	}
	if l == s.duties {
		// The duty bucket's ceiling IS the contract's, for every class laneFor
		// sends here.
		return coord.CheckLeaseTTL(resource, ttl)
	}
	if ttl > l.maxTTL {
		// MaxAge would reap the record before this deadline, so honouring it is
		// impossible (package doc, "One TTL per bucket").
		return fmt.Errorf("%w: %v > %v", errTTLTooLong, ttl, l.maxTTL)
	}
	return nil
}

// unavailable turns a transport failure into the contract's third answer. A
// refusal made before anything was sent ([coord.ErrValueTooLarge]) is
// definite and passes through unmarked: marked, a caller would read it as an
// outage and retry it for good.
func unavailable(what string, err error) error {
	if errors.Is(err, coord.ErrValueTooLarge) {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %w", coord.ErrUnavailable, what, err)
}

// writeUnknown is a write sent with no answer heard: unavailable, and marked
// [coord.ErrWriteUnknown] because it may have landed, so a method that reads
// before it writes does not report a possibly-made change as not made. A
// refusal before sending passes through as in [unavailable].
func writeUnknown(what string, err error) error {
	if errors.Is(err, coord.ErrValueTooLarge) {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%w: %w: %s: %w", coord.ErrUnavailable, coord.ErrWriteUnknown, what, err)
}

// contended reports a compare-and-swap that never settled.
//
// UNKNOWN, not a refusal: losing a CAS repeatedly says nothing about who holds
// the resource, and a caller that read it as "somebody else has it" would shed
// work over contention.
func contended(op, resource string) error {
	return fmt.Errorf("%w: %s(%s) lost every compare-and-swap attempt", coord.ErrUnavailable, op, resource)
}
