package memsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A TABLE'S REPLAY, BOUNDED BY ITS PROGRESS (crewlet/crewlet#1065).
//
// A hydration has no deadline of its own. It takes what its bytes take, and
// TIME refuses it only where nothing arrives: a replay is refused when it
// hears nothing for its pull's STALL ([Syncer.stall]), or when no copy it is
// placed on delivers anything for a minute ([behindWait]). Every request it
// makes keeps its own bound ([Syncer.createConsumer], [Syncer.barrier],
// [Syncer.checkIdentity]), and the answers that refuse a seat whatever the
// time — a recreated changelog, a broker's refusal of a barrier, a row past
// what this node's connection carries, a row the store refuses — refuse it
// here too. A seat that is merely large is never refused for its size: no
// refusal for time can be produced while messages arrive.
//
// # What progress is
//
// A DELIVERED MESSAGE, a row or a barrier. The client hands a message over
// only once all of it has arrived (nats.go's processMsg), and a leaf's
// compressed link decodes what it flushed as one block, so nothing finer than
// a whole message proves bytes arrived. A heartbeat is no progress: a copy
// behind sends them for ever. It tells "the copy holds nothing for me now"
// apart from "nothing has been heard", and serves the two clocks:
//
//   - SILENCE runs from the pull's send, from the last word its inbox carried
//     — a delivery, a heartbeat, a status — and from the end of the replay's
//     own end barrier, which the replay publishes between two words and
//     which holds it from its inbox while words arrive. Past the pull's stall
//     the seat is refused: no bytes for a bounded time is the one failure a
//     hydration has. It refuses rather than placing the replay again,
//     because a dead link fails a new placement too, and nothing tells a dead
//     server from a pull still crossing at the floor. It refuses only once
//     the inbox holds nothing ([jsapi.Pulls.Ready]): a word that arrived
//     while the replay's own request held it from reading is heard first. A
//     probe's answer ([jsapi.Pulls.Quiet]) decides what a quiet replay does
//     next, never how long it may stay quiet: a consumer that keeps saying a
//     delivery is on its way while none arrives is held to the stall like
//     any other.
//   - BEHIND runs from the first word that a copy holds nothing for the
//     replay while the barrier it reads to is owed, or from the first time
//     the replay is placed again for any cause, whichever comes first since
//     its last delivery, across placements and the time spent making them,
//     and is reset by any delivery. A replay behind for [copyBehindWait] on
//     one copy is placed again ([tableReplay.place]); a replay nothing has
//     been delivered to for [behindWait], however many copies it was placed
//     on and whatever each said — behind, gone, refusing its pulls, never
//     pulled, losing a delivery — is refused ([tableReplay.behind]).
//
// # The end: a barrier at or past the hydration's, then an end barrier
//
// A replay reads until it has received a barrier at or past the hydration's
// (B, [Syncer.barrier]), then publishes the seat's barrier again, and reads on
// until it has received a barrier at or past that one's sequence (E). A row
// whose version the changelog acknowledged before B either arrived, or was
// removed by a later version before the replay reached it; that version was
// applied on the copy before B arrived, so before E was proposed, and raft
// orders the log, so it lies below E and arrives before E does, unless a
// version later still removed it first, which the same argument follows. So
// a row is missed only where it was republished twice inside one table's
// replay, once around B and again after E was proposed: two holders of the
// seat at once, crewlet/crewlet#511's general window, whose newest version
// stays on the changelog. The copy's running count of what follows a delivery
// is read for no decision: it runs low where a replacement's signal has not
// reached it, and high where a delivery skipped a row its removal had not
// yet taken off (nats-server consumer.go, checkNumPending), and an end read
// from it carried neither version of a row in the first case and waited for
// a message that never came in the second (crewlet/crewlet#1066's residuals).
//
// A writer that never stops cannot hold the replay open: it ends at the first
// barrier at or past E, which is one publish, made once, and reads nothing
// after it.

// fetchBatch is the most messages one replay pull asks for. The bytes every
// pull is bounded by beside it ([Syncer.pullBound]) set the MEMORY a pull
// holds; this sets the ROUND TRIPS a table's rows take where they are small.
//
// 1024, a decided knob (crewlet/crewlet#1065). The rows a seat mostly holds, a
// diary note or an episode with the vector it carries (8 KiB at 1 536
// dimensions), count some 10 550 bytes each against a pull as the broker
// counts them ([deliveryOverhead]), so a pull of the transport's 8 MiB holds
// 795 of them and its bytes bind first for any row past about 7.7 KiB: the
// count binds only for the small rows, profiles and markers, where 1024 of
// them are a few hundred KiB. A year of episodes, some eighteen thousand,
// takes 23 pulls in a member's own process, where 256 rows a pull took 71.
// Across a link a pull's window binds first for any row past 136 bytes
// ([Syncer.pullBound]). A pull is one transaction too ([tableReplay.write]),
// and its bytes, not its count, bound what that transaction writes.
const fetchBatch = 1024

// deliveryOverhead is what the broker counts against a pull's bytes beside a
// message's headers and payload: its subject, and the acknowledgement subject
// it is delivered with (nats-server's consumer.go counts the four together).
// A row's subject is at most 111 bytes: `crewlet.memory.`, a seat's
// 36-character id, the longest table's name and a hash of 32 hex digits
// ([table.subject]). Its acknowledgement subject is a couple of hundred:
// `$JS.ACK.`, the broker's domain, an account's 8-character hash, the stream's
// and the consumer's names, and five counters of up to twenty digits each.
// Four KiB holds both with room for any domain, and is the room
// statelog.MaxDeliveryBytes keeps for the same two beside a state log's
// largest record.
const deliveryOverhead = 4 << 10

// pullHeartbeat is the idle heartbeat every replay pull asks for: the server
// sends one each time it holds the pull this long with nothing to deliver.
//
// A QUARTER OF [quietWait], so a pull its copy holds nothing for is heard four
// times before the replay calls it quiet and asks the consumer what it says of
// itself; and at most half of [pullExpiry], which the server requires of a
// heartbeat (nats-server consumer.go, nextReqFromMsg).
const pullHeartbeat = 250 * time.Millisecond

// pullExpiry is how long the server holds a replay pull for messages it does
// not have yet.
//
// [copyBehindWait]'s second: a pull its copy holds nothing for ends at the
// moment the replay would call the copy behind, and the heartbeats queued
// behind a pull on its way are four at most.
const pullExpiry = time.Second

// quietWait is how long a replay hears nothing before it asks its consumer
// what it says of itself ([jsapi.Pulls.Quiet]): whether a delivery is still
// on its way, the consumer never got its pull, or its copy holds nothing.
//
// A SECOND, the same as internal/queue/jetstream's copy reads (letterQuiet)
// and internal/objstore/natsobj's reads (quietWait): twenty times the worst
// p99 of a pull's first delivery natsobj measured, 55 ms.
const quietWait = time.Second

// copyBehindWait is how long one copy may say it holds nothing for a replay
// that is still owed its barrier, since the replay's last delivery or its
// placement, before the replay is placed again on a copy the broker chooses
// afresh ([tableReplay.place]).
//
// A SECOND, raft's heartbeat (nats-server raft.go, hbInterval): raft sends an
// append when it commits, so a follower that is keeping up applies an
// acknowledged entry within a round trip; a copy still without the barrier a
// second after it heard the pull is catching up, and a draw elsewhere is
// likelier to land on one that is not. natsobj places again a read whose copy
// lags at the same cadence (natsobj.go, lagging).
const copyBehindWait = time.Second

// behindWait is how long a replay's copies may deliver nothing while the
// barrier it reads to is owed, every placement and the time spent making them
// counted, before the seat is refused ([tableReplay.nothing],
// [tableReplay.again]).
//
// [objstore.Stall], the minute after which the object store calls a transfer
// whose copies hold nothing for it stalled (natsobj's errBehind): a copy that
// sends only heartbeats for a minute carries no byte of the seat's memory for
// a minute, which is the one failure a hydration has. At a placement a
// second, fifty draws leave a refusal at (2/3)^50 where only the leader of
// three members is caught up, and the usual case is one or two draws.
const behindWait = objstore.Stall

// apiBytes is the most any of memsync's API requests or their replies carry
// beside a row: a consumer's configuration, its info, a stream's info, a
// publish's acknowledgement. A stream's info, the largest, is a few hundred
// bytes past its configuration's metadata; four KiB holds any of them with
// room.
const apiBytes = 4 << 10

// Config is how a syncer's node reaches the changelog.
type Config struct {
	// AcrossLink says the node's connection reaches the changelog across a
	// LINK, a leaf's link to its members or a dial to an external cluster,
	// rather than its own member in its own process: a replay's pulls are
	// then windowed ([Syncer.pullBound]) and every request is given the
	// link's crossing at the floor ([objstore.MiBPace]).
	AcrossLink bool
	// Clustered says the broker is a cluster, which is what a create's
	// term branches on ([jsprovision.AskTerm]).
	Clustered bool
}

// copies is where a replay's consumers are made and deleted: the changelog's
// copies, one of which the broker places each consumer on. Production's is
// [changelogCopies]; a case plays its own.
type copies interface {
	// open makes a consumer of the changelog with cfg and opens its pulls on
	// an inbox of their own.
	open(ctx context.Context, cfg jetstream.ConsumerConfig) (replayPulls, error)
	// drop deletes the consumer named name, as hygiene: a failure is left to
	// the broker's reaping.
	drop(ctx context.Context, name string)
	// maxPayload is the largest message this node's connection carries.
	maxPayload() int64
}

// replayPulls is one consumer's pulls, as a replay asks them (jsapi.Pulls).
type replayPulls interface {
	Ask(n, maxBytes int, expires, heartbeat time.Duration) error
	Next(ctx context.Context) (jsapi.Event, error)
	Ready() (jsapi.Event, bool, error)
	Quiet(ctx context.Context, arrival time.Duration) (jsapi.QuietSays, *jetstream.ConsumerInfo, error)
	Stop()
	Name() string
	// RequestBatch is the most messages the consumer takes in one pull, as
	// the reply that made it says, and 0 for no limit.
	RequestBatch() int
}

// replays is where s's replays make their consumers.
func (s *Syncer) replays() copies {
	if s.copies != nil {
		return s.copies
	}
	return changelogCopies{s: s}
}

// changelogCopies makes a replay's consumers on the changelog s's client
// speaks to, over its coordination connection.
type changelogCopies struct{ s *Syncer }

func (c changelogCopies) open(ctx context.Context, cfg jetstream.ConsumerConfig) (replayPulls, error) {
	cons, err := c.s.createConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	api, err := jsapi.Of(c.s.js)
	if err == nil {
		var pulls *jsapi.Pulls
		if pulls, err = jsapi.NewPulls(c.s.js.Conn(), api, topics.MemoryStream, cons, c.s.timing); err == nil {
			return namedPulls{Pulls: pulls, name: cfg.Name,
				batch: cons.CachedInfo().Config.MaxRequestBatch}, nil
		}
	}
	c.drop(ctx, cfg.Name)
	return nil, fmt.Errorf("memsync: open the pulls of a replay: %w", err)
}

func (c changelogCopies) drop(ctx context.Context, name string) { c.s.deleteConsumer(ctx, name) }

func (c changelogCopies) maxPayload() int64 { return c.s.js.Conn().MaxPayload() }

// namedPulls is a consumer's pulls, the name it is deleted by, and the most
// messages it takes in one pull.
//
// THE BROKER'S LIMIT, READ OFF THE CONSUMER IT MADE: a server whose JetStream
// limits set max_request_batch gives every pull consumer made with
// none of its own that limit (nats-server consumer.go,
// setConsumerConfigDefaults), and refuses a pull asking more
// (`409 Exceeded MaxRequestBatch of N`), so a replay asks no more than the
// consumer it was given takes.
type namedPulls struct {
	*jsapi.Pulls
	name  string
	batch int
}

func (p namedPulls) Name() string { return p.name }

func (p namedPulls) RequestBatch() int { return p.batch }

// createConsumer makes one of a hydration's consumers, NAMED, so asking again
// for one whose reply was lost answers the one the first request made.
//
// ASKED AS A WRITE IS ([jsprovision.Ask]): each request waits the create's
// term, which on a cluster spans a metadata commit and an election after it
// ([jsprovision.AskTerm]), plus the crossing of its request at the floor, and
// is abandoned before the next is sent, since every request is a proposal to
// the metadata group (nats-server jetstream_cluster.go). Under the longest
// silence the server keeps ([jsprovision.LookupBudget]) and the same crossing
// ([Syncer.createBudget]): a clustered create asks twice inside it, at none
// and at sixteen seconds, and a solo one once. Never shorter than the one
// request that had the rest of a fifteen-second hydration.
func (s *Syncer) createConsumer(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	ctx, cancel := context.WithTimeout(ctx, s.createBudget())
	defer cancel()
	var cons jetstream.Consumer
	err := jsprovision.Ask(ctx, s.timing.AskTerm+jsapi.Sending(apiBytes, s.pace),
		func(ctx context.Context) error {
			var err error
			cons, err = s.js.CreateConsumer(ctx, topics.MemoryStream, cfg)
			return err
		}, nil)
	return cons, err
}

// createBudget is how long one consumer's creation is asked for
// ([Syncer.createConsumer]).
func (s *Syncer) createBudget() time.Duration {
	return jsprovision.LookupBudget + jsapi.Sending(apiBytes, s.pace)
}

// deleteConsumer deletes one of a hydration's consumers as the step that made
// it ends: a table's replay, one of its placements, or a retired table's
// count.
//
// HYGIENE: the server reaps an idle consumer on its own ([Syncer.reapAfter]),
// so a deletion that fails costs a consumer lingering until then, and nothing
// else. WithoutCancel, because a cleanup that inherits the failure it is
// cleaning up after does nothing at all, and then its own bound
// ([consumerCleanupTimeout]), because WithoutCancel drops the deadline with the
// cancellation and this runs inside seat acquisition.
func (s *Syncer) deleteConsumer(ctx context.Context, name string) {
	cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx),
		consumerCleanupTimeout+jsapi.Sending(apiBytes, s.pace))
	defer stop()
	_ = s.js.DeleteConsumer(cleanupCtx, topics.MemoryStream, name)
}

// pullBytes is the largest bound a replay pull is given: the largest message
// this node's connection to the broker carries, its max_payload, and the room
// the broker counts beside it ([deliveryOverhead]).
//
// # Never below the largest row the changelog can hold
//
// The broker sends a pull nothing past what is left of its bytes: it ends the
// pull instead (`409 Message Size Exceeds MaxBytes`). One message at the
// transport's limit, whole, is the least a pull can be bounded at, so a pull
// that a row past its window ended with nothing is asked again at this bound
// for that one row ([Syncer.pullBound]), and a row past this bound refuses the
// seat ([errRowPastPull]).
//
// # Read off the connection, at each pull, never fixed at the contract
//
// Nothing holds the changelog's rows to the contract's 8 MiB
// (queue.MaxPayloadBytes): internal/queue/jetstream provisions the changelog
// with no largest message of its own, so a row is bounded by the max_payload
// of the connection that published it. On the embedded broker that is the
// contract, which the engine configures every member and leaf at. On an
// external one it is what the servers announce, and the engine refuses only a
// limit below the contract, so servers an operator runs at 16 MiB take rows of
// up to 16 MiB. A bound fixed at the contract would refuse every such row, and
// its seat, on every node at every sweep. This node's own limit holds the
// largest row any node published wherever every server announces the same
// limit, which the deployment guide asks of an external broker ("The largest
// message every server must carry"). Read at each pull, so a connection that
// has moved to another member is held to that member's.
func (s *Syncer) pullBytes() int {
	return int(s.replays().maxPayload()) + deliveryOverhead
}

// pullBound is the byte bound of an ordinary replay pull: [Syncer.pullBytes]
// in this node's own process, and across a link the WINDOW ([pullWindow]),
// what a link at the floor carries within a request's term less its own round
// trip, where that is smaller.
//
// # Why the window
//
// A replay rides the coordination connection (ADR-0001), which every lease
// renewal of the node rides too, and across a link every request the node
// sends after a pull — a read, a small write or a lease renewal, on that
// connection or on another crossing the same link or a bottleneck on it that
// queues first in first out — has its answer wait behind whatever of the
// pull's reply is still crossing. A pull of the transport's 8 MiB at the
// floor every link is promised ([objstore.MiBPace]) holds such a request four
// minutes.
// Windowed, it waits behind at most four seconds of this hydration's pulls at
// the floor, since a replay keeps one pull out at a time ([tableReplay.pull]),
// and is still answered inside its term: the rule internal/objstore/natsobj
// windows an upload by (linkWindow), counted in bytes here because a row has
// no fixed size.
//
// Its price is round trips on a fast distant link: a year of episodes, 1 380
// windows, takes about two and a half minutes with its writes across 100 ms at
// 100 Mbit/s, where 8 MiB pulls took about half a minute and windows sized by
// the shipped lease's heartbeat (512 KiB) about a minute.
//
// # What the window does not cover
//
// The window is this hydration's alone: the object store's upload window is a
// budget of its own on the same connection (natsobj, linkWindow), and an
// upload's bytes cross the link outbound while a pull's cross it inbound, so a
// request whose bytes cross behind an upload's window and whose reply crosses
// behind a pull's waits behind both — 7.75 seconds at the floor beyond its own
// round trip, past its term. (A node hydrates one seat at a time: the seat
// host's placement pass claims them one after another.) Nothing windows the
// rest of what crosses the link toward the node: a download holds four of the
// object store's 128 KiB messages in hand (natsobj, readAhead), fifteen
// seconds at the floor, for each download in flight, and an event delivered
// to one of the node's seats crosses whole. A reply queued behind them waits
// for all of it — a pull's next row among them, which past the replay's stall
// ([Syncer.stall], 9.1 s across a link) refuses the seat, to be taken again
// from the start of its memory (crewlet/crewlet#1035). One row larger than the
// window crosses alone, holding the link for its own crossing.
func (s *Syncer) pullBound() int {
	bound := s.pullBytes()
	if s.window > 0 {
		bound = min(bound, s.window)
	}
	return bound
}

// pullWindow is what a replay pull carries at most across a link: the bytes a
// link at the floor ([objstore.MiBPace]) carries within
// [jsapi.WindowCrossing], the request term less its own round trip — 139 810
// bytes, four seconds at the floor, whatever lease the node holds
// ([Syncer.pullBound]). A test holds it to the terms' own constants.
const pullWindow = int(int64(jsapi.WindowCrossing) * objstore.MiB / int64(objstore.MiBPace))

// stall is how long a replay pull of maxBytes may go without a word before
// the seat is refused: the client's request term (its DefaultTimeout), and
// what that many bytes take to reach this node whole, handled in the engine's
// processes and crossing its link at the floor ([jsapi.Delivering]). 13.0 s in
// a member's own process at the transport's bound; 9.1 s across a link at the
// window; 253 s for a pull of one row at the transport's bound across a link.
func (s *Syncer) stall(maxBytes int) time.Duration {
	return s.js.Options().DefaultTimeout + jsapi.Delivering(maxBytes, s.pace)
}

// reapAfter is how long the broker keeps a replay's consumer once its reader
// stops pulling (its InactiveThreshold), should the reader die before it
// deletes it.
//
// It counts from the consumer's last delivery or the expiry of its last pull
// (nats-server consumer.go, deleteNotActive and waiting.last), and is
// outlasted by nothing a live replay does before its next pull reaches the
// consumer: the last bytes of a pull of the transport's bound still crossing
// to it ([jsapi.Delivering]), then its end barrier, published behind those
// bytes and asked for as long as it is asked ([Syncer.endBarrierArrival],
// under [jsprovision.LookupBudget]). 46 s in a member's own process at the
// transport's bound; 526 s across a link. A replay that outruns it still —
// a transaction the store holds longer — finds its consumer gone, and is
// placed again past what it received ([placedGone]); a reader that died
// leaves its consumer that long, which costs the broker one idle consumer.
func (s *Syncer) reapAfter() time.Duration {
	n := s.pullBytes()
	return s.timing.Arriving(s.endBarrierArrival(n)).Lookup + jsapi.Delivering(n, s.pace)
}

// endBarrierArrival is how long a table's end barrier's acknowledgement takes
// to reach this node behind what it waits behind ([Syncer.barrier]): the
// publish's own crossing at the floor across a link, and the rest of the pull
// of maxBytes its replay has out, which the acknowledgement queues behind on
// the same connection.
func (s *Syncer) endBarrierArrival(maxBytes int) time.Duration {
	return jsapi.Sending(0, s.pace) + jsapi.Delivering(maxBytes, s.pace)
}

// errRowPastPull is a pull of the transport's bound that the broker ended in
// place of the table's next message, having delivered nothing: the next is a
// row larger than this node's connection carries ([Syncer.pullBytes]).
var errRowPastPull = errors.New("the next row is larger than a pull this node's connection carries")

// errMisroutedRow is a row a replay read on one table's subjects whose body
// names another table ([tableReplay.receive]).
var errMisroutedRow = errors.New("a row on one table's subjects names another table, which no build publishes")

// placeCause is why a replay is placed again on a copy the broker chooses
// afresh ([tableReplay.place]).
type placeCause string

const (
	// placedBehind is a copy that held nothing for the replay for
	// [copyBehindWait] while the barrier it reads to was still owed.
	placedBehind placeCause = "behind"
	// placedGone is a consumer the broker said is gone: a pull refused with
	// any status but its ordinary endings, or a probe that found no consumer.
	placedGone placeCause = "gone"
	// placedUnpulled is a consumer that never got its pull
	// ([jsapi.Unpulled]).
	placedUnpulled placeCause = "unpulled"
	// placedGap is a delivery the link lost ([jsapi.ErrDeliveryLost]).
	placedGap placeCause = "gap"
)

// Valid reports whether c is one of the reasons a replay is placed again.
func (c placeCause) Valid() bool {
	switch c {
	case placedBehind, placedGone, placedUnpulled, placedGap:
		return true
	}
	return false
}

// placeAgain is a pull's outcome that places the replay again, for why, and
// what the broker or the link said of it, where either said anything: a
// refusal's status line, a lost delivery's counts, a probe's error.
type placeAgain struct {
	why    placeCause
	detail string
}

func (p placeAgain) Error() string {
	if p.detail == "" {
		return "memsync: place the replay again: " + string(p.why)
	}
	return "memsync: place the replay again: " + string(p.why) + ": " + p.detail
}

// told is how a refusal says what ended a replay's last placement.
func (p placeAgain) told() string {
	said := map[placeCause]string{
		placedBehind:   "its copy held nothing for it",
		placedGone:     "the broker refused its pull or no longer had its consumer",
		placedUnpulled: "its consumer never got its pull",
		placedGap:      "the link lost a delivery",
	}[p.why]
	if p.detail != "" {
		said += " (" + p.detail + ")"
	}
	return said
}

// hydrationRun is what one hydration has done so far, across its tables: the
// figures its lines report.
type hydrationRun struct {
	handle     string
	start      time.Time
	slowAt     time.Time
	pulls      int
	placements int
	rows       int
	bytes      int64
}

// slow says once a minute that a hydration is still running
// (memory_hydration_slow), so an operator sees a slow link at work rather
// than a hung acquisition: at [objstore.Stall], the minute after which the
// object store calls a silent transfer stalled.
//
// rows_carried is every row the hydration has received so far: the rows its
// pulls' transactions committed, and those the pull still out has staged.
func (s *Syncer) slow(ctx context.Context, run *hydrationRun, table string, staged int) {
	now := time.Now()
	if now.Before(run.slowAt) {
		return
	}
	// A MINUTE FROM NOW, never from when the line was due: a step that held
	// the replay longer than a minute — a create or a barrier asked to its
	// ceiling — is said once as it ends, not once for each minute it took.
	run.slowAt = now.Add(objstore.Stall)
	elapsed := now.Sub(run.start)
	s.info(ctx, "memory_hydration_slow", "seat", run.handle, "table", table,
		"rows_carried", run.rows+staged, "elapsed", elapsed,
		"bytes_per_second", int64(float64(run.bytes)/elapsed.Seconds()))
}

// cursor is where one table's replay stands.
type cursor struct {
	// reached is the stream sequence of the last delivery the replay
	// received with no gap before it, a barrier's included: where a
	// placement after it begins ([tableReplay.place]).
	reached uint64
	// barrier is the newest barrier the replay has received, 0 for none.
	barrier uint64
	// target is the end barrier's sequence, 0 until it is published
	// ([tableReplay.receive]).
	target uint64
	// lastCount is what the last delivery's count said follows it, kept for
	// a refusal's sentence alone; counted says one was read.
	lastCount int
	counted   bool
	// placements is the consumers made for the table, and remakes the
	// placements in a row made for a pull a consumer never got.
	placements, remakes int
	// delivered is the deliveries the replay received since its last
	// placement.
	delivered    int
	placedAt     time.Time
	lastDelivery time.Time
	// behindSince is when the copies began delivering nothing to the
	// replay: the first word that one holds nothing for it, or the first
	// placement again, since its last delivery; zero while they have
	// delivered since ([tableReplay.behind]).
	behindSince time.Time
	// transport says the next pull asks one message at the transport's
	// bound, for a row past the window ([Syncer.pullBound]).
	transport bool
}

// tableReplay is one table's replay of a seat's memory.
type tableReplay struct {
	s        *Syncer
	run      *hydrationRun
	handle   string
	ref      seatRef
	spec     table
	orphaned map[string]int
	filter   string
	barrier  string
	// first is the hydration's barrier (B).
	first uint64

	at      cursor
	cons    replayPulls
	staged  []Row
	carried int
}

// replayPull is the pull a replay has out.
type replayPull struct {
	n         int
	maxBytes  int
	transport bool
	delivered int
}

// replay carries one table of a seat's memory into this node's store, and says
// how many rows it wrote: every row of the table the changelog acknowledged
// before the hydration's barrier, at sequence first, and any written since
// that the replay received before its end barrier (this file's head), a pull
// at a time, each bounded in messages and in bytes ([fetchBatch],
// [Syncer.pullBound]) and its rows written in one transaction. A row whose
// parent the store does not hold is not written, and is counted in orphaned
// instead ([table.parent]).
//
// # Why a replay reads to a barrier
//
// On a fleet the changelog is replicated, and a replay is an R1 consumer,
// which the server places on a RANDOM member of the stream (nats-server
// jetstream_cluster.go, createGroupForConsumer) and which delivers out of THAT
// member's copy. A follower applies a row a moment after the leader
// acknowledged it, so a replay hosted on one that is behind delivers its own
// copy and, read to the copy's count, stopped short of the rows the seat's
// last owner wrote just before placement moved it, which are the seat's
// newest. No consumer can be asked for the leader's count instead: its
// configuration names no placement, and the count it reports is its own
// server's copy's. And a read of the leader that answers with a row answers it
// base64-encoded in JSON, a third larger than the row, which crossed a
// `[seats]` satellite's leaf link as a reply past the 8 MiB the leaf carries,
// and the leaf closed its whole link to the fleet, its lease renewals with it
// (nats-server leafnode.go, processLeafMsgArgs; crewlet/crewlet#1031).
//
// So the replay reads to the BARRIER through its OWN consumer, filtered to the
// table's subjects and the seat's barrier subject at once ([barrierSubject]),
// and then to its end barrier. A copy applies the stream in its order, and a
// consumer delivers in that order too, so the copy hands over the table's rows
// below the barrier, then the barrier, once it has applied that far. AT OR
// PAST, because a hydration of the seat racing this one publishes its own
// barriers to the seat's one subject, which replace this one's: this replay
// then receives the later barrier and ends there as the other's does. A
// barrier below the hydration's is an earlier hydration's, on a copy that has
// not applied this one yet, and is read past. Nothing crosses the link but
// the table's own rows, raw, in its pulls.
//
// # A copy behind is placed again
//
// The barrier lies past the WHOLE changelog as the leader held it, every
// seat's writes and not only this table's, so a copy behind the leader holds
// the replay until it has applied all of it: a member still catching its copy
// up after a restart. A replay whose copy holds nothing for it for
// [copyBehindWait] while its barrier is owed is placed again: its consumer is
// deleted and made again from one past the last sequence it received, and
// the broker places the new one afresh, preferring the members its metadata
// leader counts caught up (nats-server jetstream_cluster.go,
// createGroupForConsumer), so a draw lands on a copy that holds the barrier at
// least one time in three on three members. So is a consumer the broker said
// is gone or whose pull it refused, one that never got its pull (up to
// [jsapi.FirstPullRemakes] times in a row), and one whose delivery the link
// lost; a placement that delivered nothing is followed by the next no sooner
// than [copyBehindWait] after it ([tableReplay.again]), so a broker that
// refuses every pull is asked for a consumer a second, not as fast as it
// answers. NOTHING IS SKIPPED: every message at
// or below where the new placement begins was received, or removed by a later
// version that lies past it, and sequences are the stream's, so the argument
// holds across copies. NOTHING IS READ TWICE: the new placement delivers
// nothing at or below where it begins, and the upsert is idempotent anyway.
// Nothing delivered for [behindWait], across every placement, refuses the seat
// ([tableReplay.behind]).
//
// # A pull's rows are written before anything else is done
//
// Whatever ended the pull, the rows it delivered are written in its
// transaction before the replay is placed again or refused: each was received
// with no gap before it, and a placement begins past the last of them. So a
// transaction the store fails fails the hydration, whatever ended the pull: a
// replay placed again past rows that never committed would carry the seat
// without them.
func (s *Syncer) replay(ctx context.Context, run *hydrationRun, handle string, ref seatRef, spec table,
	first uint64, orphaned map[string]int) (int, error) {

	r := &tableReplay{s: s, run: run, handle: handle, ref: ref, spec: spec, orphaned: orphaned,
		filter: tableFilter(ref.AgentID, spec.name), barrier: barrierSubject(ref.AgentID), first: first}
	defer r.close(ctx)
	if err := r.place(ctx, placeAgain{}); err != nil {
		return r.carried, err
	}
	for {
		done, err := r.pull(ctx)
		var again placeAgain
		placing := errors.As(err, &again)
		// THE PULL'S ROWS, whatever ended it ("A pull's rows are written
		// before anything else is done"). A pull's own failure outranks its
		// rows' — a cut, a refusal — since a transaction cut with it fails
		// for the same reason; a placement does not, since it would begin
		// past them.
		if werr := r.write(ctx); werr != nil && (err == nil || placing) {
			return r.carried, werr
		}
		switch {
		case placing:
			if perr := r.again(ctx, again); perr != nil {
				return r.carried, perr
			}
		case err != nil:
			return r.carried, err
		case done:
			return r.carried, nil
		}
	}
}

// again places the replay again for a, once the behind clock allows it: armed
// here where nothing armed it since the replay's last delivery, and refusing
// the seat once nothing has been delivered for [behindWait] ([tableReplay.behind]).
//
// PACED: a placement that delivered nothing is followed by the next no sooner
// than [copyBehindWait] after it was made, raft's heartbeat, by which a member
// the metadata leader passed over is counted caught up again — the cadence
// internal/queue/jetstream's copy reads place again at (placementPause). Every
// placement is a delete and a create, each a proposal to the metadata group,
// and a broker that refuses every pull at once was asked for them as fast as it
// answered.
func (r *tableReplay) again(ctx context.Context, a placeAgain) error {
	now := time.Now()
	if r.at.behindSince.IsZero() {
		r.at.behindSince = now
	}
	if now.Sub(r.at.behindSince) >= behindWait {
		return r.behind(ctx, a)
	}
	if r.at.delivered == 0 {
		if wait := time.Until(r.at.placedAt.Add(copyBehindWait)); wait > 0 {
			paced := time.NewTimer(wait)
			defer paced.Stop()
			select {
			case <-ctx.Done():
				return fmt.Errorf("memsync: replay %s's %s: %w", r.handle, r.spec.name, ctx.Err())
			case <-paced.C:
			}
		}
	}
	return r.place(ctx, a)
}

// close stops the replay's pulls and deletes its consumer.
func (r *tableReplay) close(ctx context.Context) {
	if r.cons == nil {
		return
	}
	r.cons.Stop()
	r.s.replays().drop(ctx, r.cons.Name())
	r.cons = nil
}

// place makes the replay's consumer, after deleting the one before it where
// there was one: from the stream's start for the table's first placement (a
// zero a), and from one past the last sequence the replay received after it,
// on a copy the broker chooses afresh, for a's cause.
func (r *tableReplay) place(ctx context.Context, a placeAgain) error {
	r.close(ctx)
	cfg := jetstream.ConsumerConfig{
		Name: nuid.Next(),
		// NAMED FOR WHAT IT READS, so a consumer the broker lists while it
		// stands, or until it is reaped, says whose replay it is.
		Description:    fmt.Sprintf("memsync: a replay of %s's %s", r.handle, r.spec.name),
		FilterSubjects: []string{r.filter, r.barrier},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		AckPolicy:      jetstream.AckNonePolicy,
		// REAPED BY THE BROKER should this node die before it deletes it.
		InactiveThreshold: r.s.reapAfter(),
	}
	if r.at.reached > 0 {
		cfg.DeliverPolicy, cfg.OptStartSeq = jetstream.DeliverByStartSequencePolicy, r.at.reached+1
	}
	cons, err := r.s.replays().open(ctx, cfg)
	switch {
	case err != nil && jsprovision.Unanswered(ctx, err):
		return fmt.Errorf("memsync: open a replay of %s's %s at sequence %d: the broker made no "+
			"consumer for it in %v: %w", r.handle, r.spec.name, r.at.reached+1, r.s.createBudget(), err)
	case err != nil:
		return fmt.Errorf("memsync: open a replay of %s's %s at sequence %d: %w", r.handle, r.spec.name,
			r.at.reached+1, err)
	}
	r.cons = cons
	r.at.placements++
	r.run.placements++
	r.at.placedAt, r.at.delivered = time.Now(), 0
	if a.why != "" {
		r.s.info(ctx, "memory_replay_placed_again", "seat", r.handle, "table", r.spec.name,
			"why", string(a.why), "detail", a.detail, "reached", r.at.reached, "target", r.target(),
			"placement", r.at.placements)
	}
	return nil
}

// target is the barrier the replay reads to now: its end barrier once
// published, and the hydration's before.
func (r *tableReplay) target() uint64 {
	if r.at.target > 0 {
		return r.at.target
	}
	return r.first
}

// ask sends the replay's next pull: [fetchBatch] messages at the replay's
// bound ([Syncer.pullBound]), or one message at the transport's for a row
// past that bound.
//
// No more messages than the consumer takes in one pull ([namedPulls]), where
// the broker set it fewer than [fetchBatch].
func (r *tableReplay) ask() (*replayPull, error) {
	p := &replayPull{n: fetchBatch, maxBytes: r.s.pullBound()}
	if most := r.cons.RequestBatch(); most > 0 {
		p.n = min(p.n, most)
	}
	if r.at.transport {
		p.n, p.maxBytes, p.transport = 1, r.s.pullBytes(), true
	}
	if err := r.cons.Ask(p.n, p.maxBytes, pullExpiry, pullHeartbeat); err != nil {
		return nil, fmt.Errorf("memsync: replay %s's %s: %w", r.handle, r.spec.name, err)
	}
	r.run.pulls++
	return p, nil
}

// pull asks one pull and reads it until it ends, the table ends, or the
// replay must be placed again ([placeAgain]) or refused, and says whether the
// table ended. One pull is out at a time: the window a pull is bounded by
// ([Syncer.pullBound]) is the most a lease renewal waits behind only while
// that holds.
func (r *tableReplay) pull(ctx context.Context) (bool, error) {
	p, err := r.ask()
	if err != nil {
		return false, err
	}
	stall := r.s.stall(p.maxBytes)
	heard := time.Now()
	for {
		r.s.slow(ctx, r.run, r.spec.name, len(r.staged))
		ev, err := r.next(ctx, p, heard.Add(stall), stall)
		switch {
		case errors.Is(err, errQuiet):
			if qerr := r.quiet(ctx, p, heard.Add(stall)); qerr != nil {
				return false, qerr
			}
			continue
		case err != nil:
			return false, err
		}
		switch ev.Kind {
		case jsapi.Delivered:
			done, err := r.receive(ctx, ev, p)
			if err != nil || done {
				return done, err
			}
			if ev.Ended {
				return false, nil
			}
		case jsapi.Heartbeat:
			if err := r.nothing(ctx); err != nil {
				return false, err
			}
		case jsapi.Expired:
			return false, r.nothing(ctx)
		case jsapi.PastMaxBytes:
			if p.delivered > 0 {
				// A WINDOW'S ORDINARY END.
				return false, nil
			}
			if p.transport || p.maxBytes >= r.s.pullBytes() {
				return false, r.rowPastPull(p)
			}
			// A ROW PAST THE WINDOW, which crosses alone at the
			// transport's bound.
			r.at.transport = true
			return false, nil
		case jsapi.BatchCompleted:
			return false, nil
		default:
			// REFUSED: the consumer deleted under the pull, its leader
			// moving, a request limit, nobody answering.
			return false, placeAgain{why: placedGone, detail: ev.Code + " " + ev.Description}
		}
		// HEARD ONCE THE WORD IS TAKEN, a delivery's end barrier included:
		// the publish holds the replay from its inbox, and what arrives
		// meanwhile is no silence of the copy's.
		heard = time.Now()
	}
}

// errQuiet is a replay that has heard nothing for [quietWait], with its
// silence clock still running ([tableReplay.next]).
var errQuiet = errors.New("memsync: the replay has heard nothing for a while")

// next is the next word on the replay's inbox for its pull p: waited for up
// to [quietWait] and never past until, the end of the pull's silence, and
// [errQuiet] where the quiet wait passes first. Once until has passed, the
// word the inbox already holds, if any ([jsapi.Pulls.Ready]): the replay's own
// requests — its end barrier, a probe of its consumer — hold it from its inbox,
// and what arrived meanwhile is heard before the pull is called silent
// ([tableReplay.silence]).
func (r *tableReplay) next(ctx context.Context, p *replayPull, until time.Time,
	stall time.Duration) (jsapi.Event, error) {

	if wait := min(quietWait, time.Until(until)); wait > 0 {
		waiting, cancel := context.WithTimeout(ctx, wait)
		ev, err := r.cons.Next(waiting)
		cancel()
		switch {
		case err == nil:
			return ev, nil
		case ctx.Err() != nil:
			return ev, fmt.Errorf("memsync: replay %s's %s: %w", r.handle, r.spec.name, ctx.Err())
		case !errors.Is(err, context.DeadlineExceeded):
			return ev, r.unread(err)
		case time.Now().Before(until):
			return ev, errQuiet
		}
	}
	ev, ok, err := r.cons.Ready()
	switch {
	case err != nil:
		return ev, r.unread(err)
	case !ok:
		return ev, r.silence(ctx, p, stall)
	}
	return ev, nil
}

// unread is err, a word on the replay's inbox that could not be read, as the
// pull's outcome: a delivery the link lost places the replay again past the
// last it received whole, the lost one discarded, never staged.
func (r *tableReplay) unread(err error) error {
	if errors.Is(err, jsapi.ErrDeliveryLost) {
		return placeAgain{why: placedGap, detail: err.Error()}
	}
	return fmt.Errorf("memsync: replay %s's %s: %w", r.handle, r.spec.name, err)
}

// quiet asks the consumer what it says of itself once the replay has heard
// nothing for [quietWait], under what is left of the silence clock, until,
// and acts on it: a delivery still crossing is waited on, a copy that holds
// nothing is behind, and a consumer that never got its pull is made again.
//
// ONCE [jsapi.FirstPullRemakes] REMAKES ARE SPENT, its pull is waited for
// like any other, under the silence clock, and nothing more is asked of it: a
// consumer still without its pull after three remakes is behind a link slow
// enough to hold the pull back, and the pull reaches it in the end. A second
// pull asked beside it on the same inbox would stand with it once it did, and
// with two pulls out every pull after them would be read against the other's
// end, and twice the window would be out ahead of a lease renewal, for the
// rest of the table.
//
// A probe unanswered by until returns with nothing to do: the pull's next
// read hears what arrived during it, or calls it silent ([tableReplay.next]).
func (r *tableReplay) quiet(ctx context.Context, p *replayPull, until time.Time) error {
	probe, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	says, _, err := r.cons.Quiet(probe, jsapi.Delivering(p.maxBytes, r.s.pace))
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return fmt.Errorf("memsync: replay %s's %s: %w", r.handle, r.spec.name, ctx.Err())
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		return placeAgain{why: placedGone, detail: err.Error()}
	case probe.Err() != nil:
		return nil
	default:
		// AN ANSWER THAT SAYS THE CONSUMER IS NOT THE ONE THE REPLAY READ:
		// one that counts fewer deliveries than arrived was made again under
		// it.
		return placeAgain{why: placedGone, detail: err.Error()}
	}
	switch says {
	case jsapi.StillCrossing:
		return nil
	case jsapi.Drained:
		return r.nothing(ctx)
	}
	if r.at.remakes < jsapi.FirstPullRemakes {
		r.at.remakes++
		return placeAgain{why: placedUnpulled}
	}
	return nil
}

// nothing is a copy saying it holds nothing for the replay now — a heartbeat,
// an expiry, a probe that found it drained — while the barrier the replay
// reads to is still owed: the behind clock, which places the replay again
// after [copyBehindWait] on one copy and refuses the seat after [behindWait]
// across them.
func (r *tableReplay) nothing(ctx context.Context) error {
	now := time.Now()
	if r.at.behindSince.IsZero() {
		r.at.behindSince = now
	}
	if now.Sub(r.at.behindSince) >= behindWait {
		return r.behind(ctx, placeAgain{why: placedBehind})
	}
	since := r.at.placedAt
	if r.at.lastDelivery.After(since) {
		since = r.at.lastDelivery
	}
	if now.Sub(since) >= copyBehindWait {
		return placeAgain{why: placedBehind}
	}
	return nil
}

// receive takes one delivery: a row, staged for the pull's transaction, or a
// barrier, and says whether the table has ended. The first barrier at or past
// the hydration's publishes the end barrier, at once, even while its pull is
// still delivering; the first at or past the end barrier ends the table, and
// nothing after it in the pull is read.
func (r *tableReplay) receive(ctx context.Context, ev jsapi.Event, p *replayPull) (bool, error) {
	p.delivered++
	r.at.delivered++
	seq := ev.Meta.Sequence.Stream
	r.at.reached = seq
	r.at.lastCount, r.at.counted = int(ev.Meta.NumPending), true
	r.at.lastDelivery, r.at.behindSince, r.at.remakes = time.Now(), time.Time{}, 0
	r.at.transport = false
	r.run.bytes += int64(ev.Msg.Size())
	if ev.Msg.Subject == r.barrier {
		// A BARRIER IS TOLD APART BY ITS SUBJECT, the seat's one that is no
		// table's, never by its body, and it is no row.
		r.at.barrier = max(r.at.barrier, seq)
		if r.at.target == 0 && r.at.barrier >= r.first {
			end, err := r.s.barrier(ctx, r.ref.AgentID,
				fmt.Sprintf("the end barrier of %s's %s", r.handle, r.spec.name),
				r.s.endBarrierArrival(p.maxBytes))
			if err != nil {
				return false, err
			}
			r.at.target = end
			// THE COPY IS HEARD FROM AGAIN ONLY ONCE THE PUBLISH HAS
			// RETURNED: what it held for the replay meanwhile waited on
			// the replay, not the replay on it ([copyBehindWait]).
			r.at.lastDelivery = time.Now()
		}
		return r.at.target > 0 && r.at.barrier >= r.at.target, nil
	}
	row, err := decode(ev.Msg.Data)
	if err != nil {
		return false, err
	}
	if row.Table != r.spec.name {
		// A ROW ON ANOTHER TABLE'S SUBJECTS, which no build publishes: a
		// row's subject names its own table ([table.subject]). Written
		// here, it would be written in this table's turn rather than its
		// own, ahead of the parents the turns exist for, so it is refused,
		// as a row that does not decode is.
		return false, fmt.Errorf("memsync: replay %s's %s: %w, here one that says it is a %q row",
			r.handle, r.spec.name, errMisroutedRow, row.Table)
	}
	r.staged = append(r.staged, row)
	return false, nil
}

// write commits the rows the replay staged in one transaction.
//
// STAGED BEFORE IT BEGINS, never read inside it: a pull hands its rows over as
// they cross the link, so a transaction that read them held this store's
// write, and every other writer of the node, for as long as they took to
// arrive; and the store may run a transaction's body again ([store.DB.Tx]).
// COUNTED ONCE IT HAS COMMITTED, never as each row is written: a row the
// store refuses rolls back every row of its pull, and a hydration that had
// counted them reported rows it had rolled back.
func (r *tableReplay) write(ctx context.Context) error {
	if len(r.staged) == 0 {
		return nil
	}
	rows := r.staged
	r.staged = nil
	var wrote, skipped int
	if err := r.s.db.Tx(ctx, func(tx *sql.Tx) error {
		wrote, skipped = 0, 0
		for _, row := range rows {
			held, err := r.spec.parentHeld(ctx, tx, row)
			if err != nil {
				return err
			}
			if !held {
				skipped++
				continue
			}
			if err := upsert(ctx, tx, r.spec, row); err != nil {
				return err
			}
			wrote++
		}
		return nil
	}); err != nil {
		return err
	}
	r.carried += wrote
	r.run.rows += wrote
	if skipped > 0 {
		r.orphaned[r.spec.name] += skipped
	}
	return nil
}

// silence refuses the seat for a pull of p's that nothing was heard of for
// its stall: the rows it received are written first, and said.
func (r *tableReplay) silence(ctx context.Context, p *replayPull, stall time.Duration) error {
	if err := r.write(ctx); err != nil {
		return err
	}
	where := "in this node's own process"
	if r.s.pace > 0 {
		where = fmt.Sprintf("across this node's link at the floor of %v a MiB", objstore.MiBPace)
	}
	held := "had not delivered the barrier"
	if r.at.counted {
		held = fmt.Sprintf("last said it held %d more", r.at.lastCount)
	}
	return fmt.Errorf("memsync: replay %s's %s: nothing arrived for %v, the %v request term and "+
		"the %v a pull of %d bytes takes to arrive %s: the copy serving it had delivered the "+
		"table's messages to sequence %d (%d rows carried) and %s; the barrier this replay reads "+
		"to is at sequence %d; %s for the table. The member serving the replay, or the link to "+
		"it, stopped answering: look at that member and this node's connection to the broker. "+
		"The seat is taken again at the next placement sweep, from the start of its memory",
		r.handle, r.spec.name, stall, r.s.js.Options().DefaultTimeout,
		jsapi.Delivering(p.maxBytes, r.s.pace), p.maxBytes, where, r.at.reached, r.carried, held,
		r.target(), consumersMade(r.at.placements))
}

// behind refuses the seat for a replay its copies delivered nothing to for
// [behindWait] while the barrier it reads to was owed, last for last's cause:
// the rows it received are written first, and said.
func (r *tableReplay) behind(ctx context.Context, last placeAgain) error {
	if err := r.write(ctx); err != nil {
		return err
	}
	return fmt.Errorf("memsync: replay %s's %s: no copy delivered anything past sequence %d for %v, "+
		"and the barrier at sequence %d it reads to never arrived: %s for the table, the last "+
		"ending because %s; %d rows carried. The changelog's copies were behind its leader, or "+
		"the broker refused or lost the replay's pulls, for that long: look at a member "+
		"restarting or catching up, and at what the last placement says. The seat is taken "+
		"again at the next placement sweep, from the start of its memory",
		r.handle, r.spec.name, r.at.reached, behindWait, r.target(), consumersMade(r.at.placements),
		last.told(), r.carried)
}

// consumersMade says how many consumers a refusal's replay made.
func consumersMade(n int) string {
	if n == 1 {
		return "1 consumer made"
	}
	return fmt.Sprintf("%d consumers made", n)
}

// rowPastPull refuses the seat for a row the broker would not send in a pull
// of the transport's bound ([errRowPastPull]), naming the limit to align.
func (r *tableReplay) rowPastPull(p *replayPull) error {
	return fmt.Errorf("memsync: replay %s's %s: %w: the broker ended a pull of up to %d bytes in "+
		"place of the table's next message past sequence %d, which is more than one message at "+
		"this node's connection's max_payload of %d bytes and the room the broker counts beside "+
		"it. Such a row was published through a server, an account or a user that carries more "+
		"than this node's connection does: every node's connection to the broker must carry the "+
		"same max_payload (docs/guides/deployment.md, %q)",
		r.handle, r.spec.name, errRowPastPull, p.maxBytes, r.at.reached, p.maxBytes-deliveryOverhead,
		"The largest message every server must carry")
}
