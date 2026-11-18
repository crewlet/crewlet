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

// A TABLE'S REPLAY, BOUNDED BY ITS PROGRESS. A hydration has no deadline of
// its own: time refuses it only where nothing arrives, nothing heard for a
// pull's stall ([Syncer.stall]) or nothing delivered by any copy for
// [behindWait], so a seat that is merely large is never refused for its size.
// Every request keeps its own bound ([Syncer.createConsumer], [Syncer.barrier],
// [Syncer.checkIdentity]), and an answer that refuses a seat whatever the time
// (a recreated changelog, a refused barrier, a row past this node's
// connection, a row the store refuses) refuses it here too. The package doc
// has the rest: "What progress is", "What silence is", "A copy behind is
// placed again" and "The end barrier".

// fetchBatch is the most messages one replay pull asks for: it sets the ROUND
// TRIPS a table's small rows take, where the pull's bytes ([Syncer.pullBound])
// set the MEMORY it holds and what its transaction writes.
//
// 1024: a diary note or an episode with its vector (8 KiB at 1 536
// dimensions) counts some 10 550 bytes against a pull ([deliveryOverhead]), so
// an 8 MiB pull holds 795 and its bytes bind first for any row past about
// 7.7 KiB; the count binds only for profiles and markers, where 1024 are a few
// hundred KiB. A year of episodes, some eighteen thousand, takes 23 pulls in a
// member's own process. Across a link the window binds past 136 bytes a row.
const fetchBatch = 1024

// deliveryOverhead is what the broker counts against a pull's bytes beside a
// message's headers and payload: its subject and its acknowledgement subject
// (nats-server consumer.go). A row's subject is at most 111 bytes
// ([table.subject]); an acknowledgement subject a couple of hundred (the
// domain, an account's hash, the stream's and consumer's names, five counters
// of up to twenty digits). Four KiB holds both for any domain, the room
// statelog.MaxDeliveryBytes keeps for the same two.
const deliveryOverhead = 4 << 10

// pullHeartbeat is the idle heartbeat every replay pull asks for: A QUARTER OF
// [quietWait], so an empty pull is heard four times before the replay calls it
// quiet, and at most half of [pullExpiry], which the server requires
// (nats-server consumer.go, nextReqFromMsg).
const pullHeartbeat = 250 * time.Millisecond

// pullExpiry is how long the server holds a replay pull for messages it does
// not have yet: [copyBehindWait], so an empty pull ends as the replay would
// call its copy behind, with at most four heartbeats queued behind it.
const pullExpiry = time.Second

// quietWait is how long a replay hears nothing before it asks its consumer
// about itself ([jsapi.Pulls.Quiet]). A SECOND, as internal/queue/jetstream's
// copy reads (letterQuiet) and internal/objstore/natsobj's reads (quietWait)
// use: twenty times the worst p99 of a pull's first delivery natsobj
// measured, 55 ms.
const quietWait = time.Second

// copyBehindWait is how long one copy may hold nothing for a replay still owed
// its barrier, since its last delivery or placement, before the replay is
// placed again ([tableReplay.place]).
//
// A SECOND, raft's heartbeat (nats-server raft.go, hbInterval): a follower
// keeping up applies an acknowledged entry within a round trip, so a copy
// still without the barrier a second on is catching up, and a fresh draw is
// likelier to land on one that is not. natsobj places a lagging read again at
// the same cadence (natsobj.go, lagging).
const copyBehindWait = time.Second

// behindWait is how long a replay's copies may deliver nothing while its
// barrier is owed, across every placement and the time making them, before the
// seat is refused ([tableReplay.nothing], [tableReplay.again]).
//
// [objstore.Stall], the minute after which the object store calls a transfer
// stalled (natsobj's errBehind): a minute of heartbeats alone carries no byte
// of the seat. At a placement a second, fifty draws leave a
// refusal at (2/3)^50 where only the leader of three members is caught up.
const behindWait = objstore.Stall

// apiBytes is the most any of memsync's API requests or replies carry beside a
// row (a consumer's configuration or info, a stream's info, a publish's
// acknowledgement). A stream's info, the largest, is a few hundred bytes past
// its configuration's metadata.
const apiBytes = 4 << 10

// Config is how a syncer's node reaches the changelog.
type Config struct {
	// AcrossLink says the node's connection reaches the changelog across a
	// LINK, a leaf's link to its members or a dial to an external cluster,
	// rather than its own member in its own process: a replay's pulls then
	// take from the link's Window ([Syncer.pullBound]) and every request is
	// given the link's crossing at the floor ([objstore.MiBPace]).
	AcrossLink bool
	// Clustered says the broker is a cluster, which is what a create's
	// term branches on ([jsprovision.AskTerm]).
	Clustered bool
	// Link is the meter of the link the node's connection crosses
	// (internal/queue/jetstream's LinkMeter), read only with AcrossLink: it
	// tells a replay held behind the link's other traffic from one whose
	// copy or link stopped answering (package doc, "What silence is"). Nil
	// reads no link, so time alone refuses a quiet replay, as on a member; a
	// Link without AcrossLink is refused, since a member has no link.
	Link Link
	// Window is the link's ONE window (internal/queue/jetstream's
	// LinkWindow), shared with the object store's transfers on the link:
	// required with AcrossLink and refused without it. Each replay pull takes
	// its bytes before it is asked and gives them back when it ends.
	Window *jsapi.Window
}

// Link is a node's link to the changelog as its meter reads it, at the
// link's own socket (internal/queue/jetstream's LinkMeter).
type Link interface {
	// Moving reports whether the link carried, in either direction, what a
	// link busy at the floor carries, over the last d.
	Moving(d time.Duration) bool
	// Carried is the bytes the far end acknowledged of what the node wrote
	// to the link, and the bytes the node read from it, since the meter was
	// made.
	Carried() (acked, read uint64)
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
// messages it takes in one pull, READ OFF THE CONSUMER THE BROKER MADE: a
// server whose limits set max_request_batch gives it to every pull consumer
// without its own (nats-server consumer.go, setConsumerConfigDefaults) and
// refuses a pull asking more (`409 Exceeded MaxRequestBatch of N`).
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
// term ([jsprovision.AskTerm]) plus its crossing at the floor, and is
// abandoned before the next, since every request is a proposal to the
// metadata group. Under [Syncer.createBudget] a clustered create asks twice,
// at none and at sixteen seconds, and a solo one once. Across a metered link
// that ceiling runs only while the link is still ([Syncer.waits]).
func (s *Syncer) createConsumer(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	ctx, cancel := s.waits(0).Ceiling(ctx, s.createBudget())
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
// ([Syncer.createConsumer]): the longest silence the server keeps
// ([jsprovision.LookupBudget]) and the request's crossing at the floor.
func (s *Syncer) createBudget() time.Duration {
	return jsprovision.LookupBudget + jsapi.Sending(apiBytes, s.pace)
}

// deleteConsumer deletes one of a hydration's consumers as the step that made
// it ends. HYGIENE: the broker reaps an idle consumer anyway
// ([Syncer.reapAfter]), so a failure costs a lingering consumer. WithoutCancel,
// so a cleanup inheriting the failure it cleans up after still runs, under its
// own bound ([consumerCleanupTimeout]), since this runs inside seat
// acquisition.
func (s *Syncer) deleteConsumer(ctx context.Context, name string) {
	cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx),
		consumerCleanupTimeout+jsapi.Sending(apiBytes, s.pace))
	defer stop()
	_ = s.js.DeleteConsumer(cleanupCtx, topics.MemoryStream, name)
}

// pullBytes is the largest bound a replay pull is given: this node's
// connection's max_payload and the room the broker counts beside it
// ([deliveryOverhead]). The broker ends a pull rather than send past its bytes
// (`409 Message Size Exceeds MaxBytes`), so a row past the window is asked
// again alone at this bound ([Syncer.pullBound]), and one past it refuses the
// seat ([errRowPastPull]).
//
// READ OFF THE CONNECTION AT EACH PULL, never fixed at the contract's 8 MiB:
// the changelog sets no largest message, so a fixed bound would refuse an
// external broker's 16 MiB rows at every sweep. The deployment guide asks
// every server for one limit ("The largest message every server must carry"),
// so this node's holds any node's row; a moved connection, its new member's.
func (s *Syncer) pullBytes() int {
	return int(s.replays().maxPayload()) + deliveryOverhead
}

// pullBound is the byte bound of an ordinary replay pull: [Syncer.pullBytes],
// and across a link no more than the link's ONE window ([Config.Window]).
//
// A replay rides the coordination connection (ADR-0001) with every lease
// renewal, and across a link any request sent after a pull, on any connection
// sharing a first-in-first-out bottleneck, waits behind what of the pull is
// still crossing: an 8 MiB pull at the floor ([objstore.MiBPace]) holds it four
// minutes, the window four seconds, inside its term, since one pull is out at a
// time ([tableReplay.pull]) and takes its bytes from the window the object
// store's transfers on the link share (ADR-0160). Its price is round trips on a
// fast distant link. What it leaves uncovered is the package doc's "A window
// across a link".
func (s *Syncer) pullBound() int {
	bound := s.pullBytes()
	if s.window != nil {
		bound = min(bound, s.window.Size())
	}
	return bound
}

// quietBytes is what a metered link may carry to this node while the replay
// hears nothing before it is placed again ([placedQuiet]): one message at the
// transport's bound ([Syncer.pullBytes]) and one of the replay's pulls
// ([Syncer.pullBound]), 8.5 MB across a link.
//
// A CHOICE, NOT A BOUND: the replay's own bytes ahead of a word are one pull's
// ([tableReplay.pull]), but nothing bounds the node's events and records on
// the same connection, which the link's window does not hold, so past this a
// fresh copy is judged cheaper than waiting. A word queued behind more costs
// a needless placement, which the behind clock does not read
// ([tableReplay.again]); at worst the replay is refused at [Syncer.quietCap].
func (s *Syncer) quietBytes() int {
	return s.pullBytes() + s.pullBound()
}

// quietCap is how long a replay may hear nothing from its copies, since its
// last word and across placements, while its metered link moves below
// [Syncer.quietBytes]: the stall of one pull at the transport's bound
// ([Syncer.stall]), 253 s at the floor. A link moving that long without
// carrying that much creeps below the promised floor, or carries this node's
// own traffic first, and holds the replay and its placement pass no longer
// than one such message would.
func (s *Syncer) quietCap() time.Duration {
	return s.stall(s.pullBytes())
}

// stall is how long a replay pull of maxBytes may go without a word before
// the seat is refused: the client's request term (its DefaultTimeout), and
// what that many bytes take to reach this node whole, handled in the engine's
// processes and crossing its link at the floor ([jsapi.Delivering]). 13.0 s in
// a member's own process at the transport's bound; 9.1 s across a link at the
// window; 253 s for a pull of one row at the transport's bound across a link.
func (s *Syncer) stall(maxBytes int) time.Duration {
	return s.js.Options().DefaultTimeout + jsapi.Delivering(maxBytes, s.pace)
}

// reapAfter is a replay consumer's InactiveThreshold, so the broker reaps it
// should its reader die first. It counts from the last delivery or pull expiry
// (nats-server consumer.go, deleteNotActive), so it outlasts all a live replay
// does between pulls: a pull at the transport's bound still crossing
// ([jsapi.Delivering]), then its end barrier asked to its ceiling
// ([Syncer.endBarrierArrival]), held while a metered link moves
// ([Syncer.waits]), which outlasts [Syncer.quietCap] too. 46 s in a member's
// own process, 526 s across a link, 774 s across a metered one. A replay
// outrunning it still, behind a slow transaction, finds its consumer gone and
// is placed again ([placedGone]); a dead reader costs one idle consumer.
func (s *Syncer) reapAfter() time.Duration {
	n := s.pullBytes()
	barrier := s.waits(s.endBarrierArrival(n))
	return barrier.Longest(barrier.Lookup) + jsapi.Delivering(n, s.pace)
}

// waits is the timing of one of a hydration's requests whose answer takes
// arrival to reach this node ([jsprovision.Timing.Arriving]): s's own, and
// across a metered link one whose ceilings run only while the link is still,
// held past them while it moves by what one message at the transport's bound
// takes to cross it at the floor ([Syncer.heldBehind]), the most one reply
// waits behind on it (package doc, "What silence is").
func (s *Syncer) waits(arrival time.Duration) jsprovision.Timing {
	t := s.timing.Arriving(arrival)
	if s.link != nil {
		t = t.Following(s.link, s.heldBehind())
	}
	return t
}

// heldBehind is what one message at the transport's bound takes to reach this
// node across its link at the floor ([Syncer.pullBytes], [jsapi.Delivering]):
// 248 s at the transport's 8 MiB, the most a word or a reply can be held
// behind on the link by one other message.
func (s *Syncer) heldBehind() time.Duration {
	return jsapi.Delivering(s.pullBytes(), s.pace)
}

// heldFor says how long a request asked at t under a ceiling of d was asked
// for, for a refusal: d, and across a metered link, d of the link standing
// still or d and what it may be held past it while the link moved.
func (s *Syncer) heldFor(t jsprovision.Timing, d time.Duration) string {
	if t.Progress == nil {
		return d.String()
	}
	return fmt.Sprintf("%v of this node's link carrying nothing at the floor, or %v while it moved",
		d, t.Longest(d))
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
	// placedQuiet is a replay that heard nothing while its link carried
	// more than a word can be held behind ([Syncer.quietBytes]): its copy's
	// words were lost on the way, or never sent (package doc, "What
	// silence is").
	placedQuiet placeCause = "quiet"
)

// Valid reports whether c is one of the reasons a replay is placed again.
func (c placeCause) Valid() bool {
	switch c {
	case placedBehind, placedGone, placedUnpulled, placedGap, placedQuiet:
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
		placedQuiet:    "nothing arrived for it while its link carried more than a word waits behind",
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

// slow says once a minute ([objstore.Stall]) that a hydration is still running
// (memory_hydration_slow), so an operator sees a slow link at work rather than
// a hung acquisition. rows_carried counts the rows committed and those the
// pull still out has staged.
func (s *Syncer) slow(ctx context.Context, run *hydrationRun, table string, staged int) {
	now := time.Now()
	if now.Before(run.slowAt) {
		return
	}
	// A MINUTE FROM NOW, never from when the line was due: a step that held
	// the replay for minutes is said once as it ends, not once a minute.
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
	// word is when the replay last heard a word from a copy — a delivery,
	// a heartbeat, a status — or was first placed, before any: what a quiet
	// replay on a moving link is held to across placements
	// ([Syncer.quietCap]).
	word time.Time
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
	// taken is what the pull holds of the link's window, given back once
	// ([Syncer.give]); 0 in a member's own process.
	taken int
}

// replay carries one table of a seat's memory into this node's store and says
// how many rows it wrote: every row the changelog acknowledged before the
// hydration's barrier (first), and any it received before its end barrier
// (package doc, "The end barrier"), a pull at a time ([fetchBatch],
// [Syncer.pullBound]), each pull's rows in one transaction. A row whose parent
// the store does not hold is counted in orphaned instead ([table.parent]).
//
// It reads through its OWN consumer, filtered to the table's subjects and the
// seat's barrier subject ([barrierSubject]), which a copy delivers in the
// stream's order: rows below the barrier before it. A copy behind is placed
// again past what the replay received, and a pull's rows commit before that
// or a refusal (package doc, "A copy behind is placed again").
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
		// THE PULL'S ROWS FIRST, whatever ended it. A pull's own failure (a
		// cut, a refusal) outranks its rows', since a transaction cut with it
		// fails for the same reason; a placement does not: it begins past them.
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

// again places the replay again for a once the behind clock allows: armed here
// if nothing armed it since the last delivery, and refusing the seat after
// [behindWait] ([tableReplay.behind]), for every cause but [placedQuiet].
//
// PACED: a placement that delivered nothing is followed by the next no sooner
// than [copyBehindWait] after it, by when the metadata leader counts a member
// it passed over caught up again (internal/queue/jetstream's placementPause).
// Each placement is a delete and a create, both metadata proposals, which a
// broker refusing every pull would otherwise be asked for as fast as it
// answers.
func (r *tableReplay) again(ctx context.Context, a placeAgain) error {
	// A QUIET REPLAY ON A MOVING LINK is no copy holding nothing: the link
	// may hold its words past a minute (package doc, "What silence is"), so
	// only [Syncer.quietCap] refuses it, never the behind clock.
	if a.why != placedQuiet {
		now := time.Now()
		if r.at.behindSince.IsZero() {
			r.at.behindSince = now
		}
		if now.Sub(r.at.behindSince) >= behindWait {
			return r.behind(ctx, a)
		}
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
			"consumer for it in %s: %w", r.handle, r.spec.name, r.at.reached+1,
			r.s.heldFor(r.s.waits(0), r.s.createBudget()), err)
	case err != nil:
		return fmt.Errorf("memsync: open a replay of %s's %s at sequence %d: %w", r.handle, r.spec.name,
			r.at.reached+1, err)
	}
	r.cons = cons
	r.at.placements++
	r.run.placements++
	r.at.placedAt, r.at.delivered = time.Now(), 0
	if r.at.word.IsZero() {
		r.at.word = r.at.placedAt
	}
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
//
// ACROSS A LINK THE PULL FIRST TAKES ITS BOUND FROM THE LINK'S WINDOW
// ([Config.Window]), under ctx alone and before its silence clock starts: a
// replay waiting its turn has asked the copy nothing. A pull at the
// transport's bound is let out alone. [tableReplay.pull] gives it back.
func (r *tableReplay) ask(ctx context.Context) (*replayPull, error) {
	p := &replayPull{n: fetchBatch, maxBytes: r.s.pullBound()}
	if most := r.cons.RequestBatch(); most > 0 {
		p.n = min(p.n, most)
	}
	if r.at.transport {
		p.n, p.maxBytes, p.transport = 1, r.s.pullBytes(), true
	}
	if r.s.window != nil {
		if err := r.s.window.Take(ctx, p.maxBytes); err != nil {
			return nil, fmt.Errorf("memsync: replay %s's %s: wait for the link's window: %w",
				r.handle, r.spec.name, err)
		}
		p.taken = p.maxBytes
	}
	if err := r.cons.Ask(p.n, p.maxBytes, pullExpiry, pullHeartbeat); err != nil {
		r.s.give(p)
		return nil, fmt.Errorf("memsync: replay %s's %s: %w", r.handle, r.spec.name, err)
	}
	r.run.pulls++
	return p, nil
}

// give returns what p took of the link's window, once.
func (s *Syncer) give(p *replayPull) {
	if p.taken > 0 {
		s.window.Give(p.taken)
		p.taken = 0
	}
}

// pull asks one pull and reads it until it ends, the table ends, or the
// replay must be placed again ([placeAgain]) or refused, and says whether the
// table ended. One pull is out at a time, its bytes held in the link's
// window from its ask to its end however it ends ([tableReplay.ask]).
func (r *tableReplay) pull(ctx context.Context) (bool, error) {
	p, err := r.ask(ctx)
	if err != nil {
		return false, err
	}
	defer r.s.give(p)
	stall := r.s.stall(p.maxBytes)
	clock := r.listen(stall)
	for {
		r.s.slow(ctx, r.run, r.spec.name, len(r.staged))
		ev, err := r.next(ctx, p, &clock)
		switch {
		case errors.Is(err, errQuiet):
			if qerr := r.quiet(ctx, p, clock.until); qerr != nil {
				return false, qerr
			}
			continue
		case errors.Is(err, errHeld):
			// HELD, AND ASKED ABOUT ITSELF until the next judgment, as a
			// quiet replay is before its stall: a consumer gone or never
			// pulled says so behind the link's traffic, and is placed
			// again sooner than the caps.
			if qerr := r.quiet(ctx, p, clock.until); qerr != nil {
				return false, qerr
			}
			continue
		case err != nil:
			return false, err
		}
		// A WORD, whatever it says and whatever it ends: what a quiet
		// replay on a moving link is held to since ([Syncer.quietCap]).
		r.at.word = time.Now()
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
		r.at.word = time.Now()
		clock = r.listen(stall)
	}
}

// quietClock is a pull's silence clock: when the replay last heard its inbox
// or sent the pull, what its link had carried by then, and when the replay is
// next judged for what it has not heard since ([tableReplay.judge]).
type quietClock struct {
	stall       time.Duration
	heard       time.Time
	acked, read uint64
	until       time.Time
}

// listen starts a pull's silence clock now, with stall the pull's: judged a
// stall from now, and across a metered link no later than the time cap since
// the replay's last word ([Syncer.quietCap]).
func (r *tableReplay) listen(stall time.Duration) quietClock {
	now := time.Now()
	c := quietClock{stall: stall, heard: now, until: now.Add(stall)}
	if r.s.link != nil {
		c.acked, c.read = r.s.link.Carried()
		c.until = earliest(c.until, r.at.word.Add(r.s.quietCap()))
	}
	return c
}

// earliest is the earlier of a and b.
func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// errHeld is a replay that heard nothing for its stall while its link moved:
// its silence clock runs on, to the next judgment a [quietWait] later
// ([tableReplay.judge]), its consumer asked about itself meanwhile.
var errHeld = errors.New("memsync: the replay has heard nothing while its link moves")

// judge decides, at c.until with nothing on its inbox, a pull unheard since
// c.heard (package doc, "What silence is"):
//
//   - across no metered link, or one still over the pull's stall: SILENCE,
//     refused ([tableReplay.silence]);
//   - nothing heard since the replay's last word for [Syncer.quietCap]:
//     refused ([tableReplay.creeping]);
//   - past [Syncer.quietBytes] read off the link since c.heard: placed again
//     ([placedQuiet]);
//   - otherwise judged again a [quietWait] later, or at the cap ([errHeld]),
//     so a link that falls still is refused within a stall and a second.
func (r *tableReplay) judge(ctx context.Context, p *replayPull, c *quietClock) error {
	if r.s.link == nil || !r.s.link.Moving(c.stall) {
		return r.silence(ctx, p, c)
	}
	now := time.Now()
	if now.Sub(r.at.word) >= r.s.quietCap() {
		return r.creeping(ctx, c)
	}
	if _, read := r.s.link.Carried(); read-c.read >= uint64(r.s.quietBytes()) {
		return placeAgain{why: placedQuiet, detail: fmt.Sprintf("%d bytes crossed the link to this "+
			"node in the %v since the replay last heard it", read-c.read, now.Sub(c.heard))}
	}
	c.until = earliest(now.Add(quietWait), r.at.word.Add(r.s.quietCap()))
	return errHeld
}

// errQuiet is a replay that has heard nothing for [quietWait], with its
// silence clock still running ([tableReplay.next]).
var errQuiet = errors.New("memsync: the replay has heard nothing for a while")

// next is the next word on the replay's inbox for its pull p: waited for up
// to [quietWait] and never past c.until, when the pull's silence clock is
// next judged, and [errQuiet] where the quiet wait passes first. Once c.until
// has passed, the word the inbox already holds, if any
// ([jsapi.Pulls.Ready]): the replay's own requests — its end barrier, a probe
// of its consumer — hold it from its inbox, and what arrived meanwhile is
// heard before the pull is judged ([tableReplay.judge]).
func (r *tableReplay) next(ctx context.Context, p *replayPull, c *quietClock) (jsapi.Event, error) {
	until := c.until
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
		return ev, r.judge(ctx, p, c)
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

// quiet asks the consumer about itself once the replay has heard nothing for
// [quietWait], under what is left of the silence clock (until): a delivery
// still crossing is waited on, a drained copy is behind, and a consumer that
// never got its pull is made again. A probe unanswered by until does nothing:
// the next read hears what arrived, or calls it silent ([tableReplay.next]).
//
// ONCE [jsapi.FirstPullRemakes] REMAKES ARE SPENT, the pull is only waited
// for: it is behind a slow link and arrives in the end, and a second pull
// beside it would leave two out, each read against the other's end, with
// twice the window ahead of a lease renewal for the rest of the table.
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
		// A ROW ON ANOTHER TABLE'S SUBJECTS, which no build publishes
		// ([table.subject]): written in this table's turn it could land
		// ahead of its parents, so it is refused like a row that does not
		// decode.
		return false, fmt.Errorf("memsync: replay %s's %s: %w, here one that says it is a %q row",
			r.handle, r.spec.name, errMisroutedRow, row.Table)
	}
	r.staged = append(r.staged, row)
	return false, nil
}

// write commits the rows the replay staged in one transaction.
//
// STAGED BEFORE IT BEGINS, never read inside it: rows arrive at the link's
// pace, and a transaction reading them would hold this store's write, every
// other writer of the node with it, that long; and the store may run a
// transaction's body again ([store.DB.Tx]). COUNTED ONCE IT HAS COMMITTED: a
// row the store refuses rolls back its whole pull.
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
// its stall, across no link that moved meanwhile: the rows it received are
// written first, and said, and so is what a metered link carried.
func (r *tableReplay) silence(ctx context.Context, p *replayPull, c *quietClock) error {
	if err := r.write(ctx); err != nil {
		return err
	}
	where := "in this node's own process"
	if r.s.pace > 0 {
		where = fmt.Sprintf("across this node's link at the floor of %v a MiB", objstore.MiBPace)
	}
	if r.s.link != nil {
		acked, read := r.s.link.Carried()
		where += fmt.Sprintf(", over which the link carried nothing at the floor (it read %d bytes, and "+
			"the far end acknowledged %d of what this node wrote, in the %v since the replay last "+
			"heard its copy or sent its pull)", read-c.read, acked-c.acked,
			time.Since(c.heard).Round(time.Millisecond))
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
		r.handle, r.spec.name, c.stall, r.s.js.Options().DefaultTimeout,
		jsapi.Delivering(p.maxBytes, r.s.pace), p.maxBytes, where, r.at.reached, r.carried, held,
		r.target(), consumersMade(r.at.placements))
}

// creeping refuses the seat for a replay its copies sent no word to for
// [Syncer.quietCap] since its last, while its link kept moving below what
// would place it again ([Syncer.quietBytes]): the rows it received are
// written first, and said.
func (r *tableReplay) creeping(ctx context.Context, c *quietClock) error {
	if err := r.write(ctx); err != nil {
		return err
	}
	_, read := r.s.link.Carried()
	return fmt.Errorf("memsync: replay %s's %s: no copy said anything to it for %v, what one pull at "+
		"the transport's bound of %d bytes takes to arrive across this node's link at the floor of %v "+
		"a MiB, while the link kept moving: it read %d bytes since the replay's last pull went out, "+
		"short of the %d past which every word behind one message at the transport's bound and the "+
		"replay's window has crossed. The copies had delivered the table's messages to sequence %d "+
		"(%d rows carried); the barrier this replay reads to is at sequence %d; %s for the table. "+
		"The link carries this node's other traffic too slowly for the replay's words to cross, or "+
		"the member serving the replay stopped answering behind it: look at what else this node "+
		"sends and receives across its link, and at that member. The seat is taken again at the "+
		"next placement sweep, from the start of its memory",
		r.handle, r.spec.name, r.s.quietCap(), r.s.pullBytes(), objstore.MiBPace, read-c.read,
		r.s.quietBytes(), r.at.reached, r.carried, r.target(), consumersMade(r.at.placements))
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
