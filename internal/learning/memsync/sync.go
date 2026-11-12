package memsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// hydrateWait bounds how long a seat waits for its memory to arrive.
//
// It is spent inside seat acquisition, before the mailbox attaches, so it is
// time the seat is not serving. Fifteen seconds is far past a replay from a
// local or clustered broker — the rows are already materialised on the stream
// and the read is sequential — and short enough that a broker having a bad
// minute delays a seat rather than stalling placement behind it.
//
// WHAT A HYDRATION SPENDS OF IT is round trips, most of them fixed, because
// each table is replayed on its own ([Syncer.Hydrate]): two to read the
// changelog's identity, before the barrier and after the last table; one to
// publish the barrier ([Syncer.barrier]); for each of the six carried tables,
// three for its consumer, which is created (the reply that creates it carries
// its count), fetched from and deleted ([Syncer.replay]) — one fetch takes a
// table's rows and its barrier together, and a table with no rows still
// fetches the barrier; and two for a consumer that counts the seat's rows of
// each retired table ([Syncer.held]). That is 23 for any seat, with rows or
// without, where one replay of the whole seat took 6: 2.3 s at 100 ms a round
// trip, a leaf link across regions; 115 ms at 5 ms; 23 ms at 1 ms. Past those,
// a fetch for each further 256 messages of a table ([fetchBatch]) or, where
// its rows are large, for each further pull's bytes ([Syncer.pullBytes]), and
// the bytes the rows carry; and where a table's replay ran on a copy behind
// the changelog's leader, the fetches that wait for the copy to apply the
// barrier, which is the whole changelog up to it, every seat's writes and not
// only the table's: a member still catching its copy up after a restart holds
// every replay the server places on it until it has, and a hydration whose
// budget runs out first is refused ([Syncer.replay], "A copy behind"). No
// round trip but a fetch carries a row: the barrier is a message of no bytes,
// and its acknowledgement and every consumer's reply carry a sequence or a
// count.
//
// SO FIFTEEN SECONDS STAYS. On the brokers it was chosen for, the replays
// cost about a tenth of a second of it. Across a 100 ms link they cost 1.7 s
// more than the one replay did, which leaves the rows 12.7 s, 127 round trips,
// where they had 14.4 s: a seat whose rows take longer than that to cross such
// a link is refused there, as one whose rows took longer than 14.4 s was. A
// larger figure would move that line rather than remove it, and every second
// it adds is a second a broker having a bad minute holds each seat of a sweep.
const hydrateWait = 15 * time.Second

// consumerCleanupTimeout bounds deleting one of a hydration's ephemeral
// consumers: a table's replay, or a retired table's count.
//
// Five seconds against one API round trip: far past a healthy answer, and
// bounded because this runs inside seat acquisition. What it costs when it
// expires is nothing an operator sees — the server reaps an idle ephemeral
// consumer on its own, which is why the deletion is hygiene rather than a
// step whose failure means anything.
//
// A HYDRATION OUTLIVES ITS BUDGET BY THIS AT MOST, though it deletes a consumer
// per table: each is deleted as the step that made it ends, inside the
// budget's time, and the step after a delete runs on the hydration's context,
// so only a delete under way as [hydrateWait] runs out runs past it.
const consumerCleanupTimeout = 5 * time.Second

// fetchBatch is the most messages one replay pull asks for: a table's rows,
// and once the barrier that ends its replay ([Syncer.replay]). It sets the
// ROUND TRIPS a table's rows take, where the bytes every pull is bounded by
// beside it ([Syncer.pullBytes]) set the MEMORY a pull holds.
//
// A pull ends at whichever bound it meets first. The rows a seat mostly holds,
// a diary note or an episode with the vector it carries (8 KiB at 1 536
// dimensions), are some 10 KiB, so 256 of them are about 2.5 MiB, inside a
// pull's bytes, and their count sets their pulls: a few hundred episodes take
// a couple, and a year of them, about eighteen thousand, 70. That is 70 ms of
// round trips at 1 ms a round trip, 0.35 s at 5 ms, and 7 s across a 100 ms
// leaf link, of the 12.7 s [hydrateWait] leaves a seat's rows there. Rows of
// more than 32 KiB on average, episodes with long asks, fill a pull's bytes
// first, at a pull per 8 MiB on the embedded broker. A pull's count is its
// transaction's too ([Syncer.replay]), which holds this store's write while it
// runs, and 256 of the common rows keep that to a few MiB of writes.
//
// NOT A BOUND ON MEMORY: a row is bounded only by the transport, so a pull
// bounded by its count alone held up to 256 rows of 8 MiB each, 2 GiB on the
// embedded broker, inside seat acquisition (crewlet/crewlet#1052).
const fetchBatch = 256

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

// exportPageBytes is how much of a seat's memory one read of a table holds
// before it is published: rows are read in watermark order until their
// carried values hold this many bytes, and always at least one row
// ([exportPage]).
//
// A BOUND ON MEMORY, IN BYTES, because a row has none short of the
// transport's: an episode keeps its ask whole, bounded only by the event that
// carried it, so one row can be megabytes (up to 8 MiB on the embedded
// broker; past that it cannot be published at all, crewlet/crewlet#349), and
// a page counted in rows bounds nothing. A drain releases every held seat at
// once (seat.Host.ReleaseAll), each flushing beside the cycle, so a node holds
// a page per released seat and the cycle's: about 51 MiB at fifty seats,
// where reading a backlog whole held all of it, which after a hydration is
// everything the changelog keeps of the seat (crewlet/crewlet#348): some
// 140 MiB of vectors for a year of episodes. That is the tables with a
// watermark: a whole-each-cycle table is still read whole by every publish
// ([exportWhole]), as large as the seat's profiles and skills have grown,
// until crewlet/crewlet#1040 carries those by change sequence too.
//
// NOT A BOUND ON ROUND TRIPS: each row is published with a broker round trip
// of its own whatever the page is, and the mark moves with each acknowledged
// row, so a cut loses the row in flight and nothing of the page. What a page
// costs is one seek on node migration 0041's (seat, change_seq) index, and a
// mebibyte is about a hundred rows at the vector a row carries (8 KiB as
// carried at 1 536 dimensions), so even at a millisecond a publish the seek is
// under a percent of what the page feeds. Much less pays a seek every handful
// of rows on a fast link; more buys nothing and multiplies the drain's peak.
// A judgement between those, not a measurement.
const exportPageBytes = 1 << 20

// AgentIDFor derives a seat's stable id from its handle (ADR-0013), or ""
// where there is nothing to derive it in. An empty id is no address on the
// changelog, so neither half touches it: [Syncer.Publish] carries nothing for
// it rather than publish to a subject with an empty seat token, which the
// broker refuses, and [Syncer.Hydrate] reads nothing for it rather than open a
// replay over such a token, which the broker refuses too.
//
// Injected rather than imported, because the derivation belongs to the
// organization model and this package needs exactly one function from it —
// the same rule every other seam in this tree follows.
type AgentIDFor func(handle string) string

// Syncer carries a seat's memory between the nodes that run it.
type Syncer struct {
	db      *store.DB
	js      jetstream.JetStream
	agentID AgentIDFor

	// marks is the per-seat, per-table watermark: the value of the table's
	// watermark column (table.watermark, a change sequence never handed out
	// twice, so a row written after the mark is always above it) of the
	// last row the changelog acknowledged. A publish moves it past each row
	// as that row is acknowledged, so a publish its bound cuts keeps what it
	// carried, and the seat's next one resumes at the first row the
	// changelog did not acknowledge.
	//
	// In memory only, and deliberately: the benefit is a subsystem with no
	// state of its own to keep correct, and the cost is a republish. A
	// restart zeroes every mark and a release ([Syncer.Forget]) the seat's,
	// so the seat's next holding here carries again everything this node
	// holds for it, which the stream collapses onto the same subjects: a page
	// at a time, across as many cycles as the link needs.
	mu    sync.Mutex
	marks map[string]int64

	// beforePage, when set, is called with a watermark table's name before
	// each of its pages' start is read: between one page's last mark and
	// the next page's read, a window no publish of a row reaches, where a
	// release's Forget meets a publish whose cursor is ahead of the zeroed
	// mark. A test's seam, unset in production, through which a case acts
	// in that window or watches what each page took.
	beforePage func(table string)

	// leftOut, when set, is called with what a hydration did not carry each
	// time its memory_rows_not_carried line says so ([Syncer.settleNotCarried]):
	// a test's seam, unset in production, through which a case reads what
	// the line reports.
	leftOut func(notCarried)
}

// New builds a syncer, or returns nil when this node cannot carry memory.
//
// Nil rather than a syncer that quietly does nothing: a node with no broker
// has no way to publish or replay, and a caller that got a working-looking
// object would believe its seats' memory was travelling when it was not.
//
// IT TAKES A JETSTREAM CLIENT rather than a connection, because the client is
// where the API a broker is addressed in is decided — the embedded fleet's
// domain, a leaf's, or an external cluster's account (see internal/jsapi) — and
// the caller holds the one its queue already speaks.
//
// BUILT OVER THE COORDINATION CONNECTION, never the queue's own (ADR-0001): a
// replay is an ordered consumer over a seat's whole history, and on the queue's
// connection it would sit on the read loop every mailbox consumes through. On
// an external broker the two are one connection; on the embedded one the
// coordination store's is the second, watched one, so losing it stops the node
// as losing the queue's does.
//
// AND THE SAME ON EVERY TOPOLOGY, a member's own connection or one that crosses
// a link to the changelog — a leaf's, or a client's of a broker somebody else
// runs — because nothing a hydration asks is answered with a row but its
// pulls, which are bounded by this connection's own limit ([Syncer.pullBytes]),
// so no answer of its can be larger than the link carries ([Syncer.replay]).
func New(db *store.DB, js jetstream.JetStream, agentID AgentIDFor) *Syncer {
	if db == nil || js == nil || agentID == nil {
		return nil
	}
	return &Syncer{db: db, js: js, agentID: agentID, marks: map[string]int64{}}
}

// seat resolves a handle into both spellings the schema uses.
func (s *Syncer) seat(handle string) seatRef {
	return seatRef{Handle: handle, AgentID: s.agentID(handle)}
}

// Carried is what one Publish got the changelog to acknowledge for a seat.
type Carried struct {
	// Rows is every row acknowledged, the whole-each-cycle tables' included.
	Rows int

	// Marked is the rows among them that moved a mark: the rows no later
	// publish of the seat sends again, until a restart or a release zeroes
	// the marks.
	Marked int

	// StoppedIn is the table a publish that did not reach its end stopped
	// in, and Whole says it is a whole-each-cycle table, which keeps nothing
	// of a cut publish.
	StoppedIn string
	Whole     bool
}

// Publish carries what is new in a seat's memory onto the changelog, and says
// what the changelog acknowledged.
//
// Called for the seats this node HOLDS, on a cadence (see the worker), and
// once more as a seat is released. A table with a watermark is carried above
// its mark a page at a time ([exportPage]: at most exportPageBytes and one
// row in memory), and the mark moves past each row the changelog
// acknowledges, so the steady-state cost is what the seat just learned, and a
// publish its context cuts keeps what it carried: the seat's next one resumes
// at the first row the changelog did not acknowledge. A wholeEachCycle table
// is carried whole by every publish, and keeps nothing of a cut one.
//
// EACH PAGE STARTS ABOVE THE LARGER of the seat's mark and the last row this
// call had acknowledged itself, its cursor. The two differ when another call
// has moved the mark further, whose rows this one then skips, and when a
// release's [Syncer.Forget] has zeroed the marks under a call in flight. That
// call goes on, as it always has, and its cursor keeps it from sending the
// table it is in again from the first row: it continues that table, starts
// each later one from the mark it finds there, and re-creates the seat's
// marks as its rows are acknowledged.
//
// It returns at the first error, keeping in the marks what was acknowledged
// before it and saying so in what it returns.
func (s *Syncer) Publish(ctx context.Context, handle string) (Carried, error) {
	var carried Carried
	if s == nil {
		return carried, nil
	}
	ref := s.seat(handle)
	if ref.AgentID == "" {
		return carried, nil
	}
	for _, spec := range tables {
		var err error
		if spec.wholeEachCycle {
			err = s.publishWhole(ctx, handle, ref, spec, &carried)
		} else {
			err = s.publishPages(ctx, handle, ref, spec, &carried)
		}
		if err != nil {
			carried.StoppedIn, carried.Whole = spec.name, spec.wholeEachCycle
			return carried, err
		}
	}
	if carried.Rows > 0 {
		log.DebugContext(ctx, "memory_published", "seat", handle, "rows", carried.Rows,
			"marked", carried.Marked)
	}
	return carried, nil
}

// publishWhole carries every one of the seat's rows of a wholeEachCycle table.
func (s *Syncer) publishWhole(ctx context.Context, handle string, ref seatRef, spec table,
	carried *Carried) error {

	rows, err := exportWhole(ctx, s.db.SQL(), spec, ref)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := s.publishRow(ctx, handle, ref, spec, row); err != nil {
			return err
		}
		carried.Rows++
	}
	return nil
}

// publishPages carries the seat's rows of a watermark table above its mark, a
// page at a time, moving the mark past each row the changelog acknowledges.
func (s *Syncer) publishPages(ctx context.Context, handle string, ref seatRef, spec table,
	carried *Carried) error {

	cursor := s.mark(handle, spec.name)
	for {
		if s.beforePage != nil {
			s.beforePage(spec.name)
		}
		page, more, err := exportPage(ctx, s.db.SQL(), spec, ref,
			max(cursor, s.mark(handle, spec.name)))
		if err != nil {
			return err
		}
		for _, row := range page {
			if err := s.publishRow(ctx, handle, ref, spec, row.row); err != nil {
				return err
			}
			// MOVED AFTER EACH ACKNOWLEDGED ROW, never before it. A
			// watermark moved first would skip, for the life of the
			// process, exactly the rows whose publish failed.
			s.setMark(handle, spec.name, row.mark)
			cursor = row.mark
			carried.Rows++
			carried.Marked++
		}
		if !more {
			return nil
		}
	}
}

// publishRow puts one row on its subject and waits for the changelog to
// acknowledge it.
func (s *Syncer) publishRow(ctx context.Context, handle string, ref seatRef, spec table, row Row) error {
	body, err := encode(row)
	if err != nil {
		return err
	}
	if _, err := s.js.Publish(ctx, spec.subject(ref.AgentID, row.Values), body); err != nil {
		return fmt.Errorf("memsync: publish a %s row for %s: %w", spec.name, handle, err)
	}
	return nil
}

// Hydrate brings a seat's memory into this node's store, and says how many
// rows it wrote.
//
// Called in seat acquisition BEFORE the mailbox attaches, so a seat never
// serves a turn against a store that has forgotten it. A failure refuses the
// seat, which is the honest outcome: a peer that takes the seat instead may
// have the memory, and a seat that runs with amnesia produces work its own
// history contradicts.
//
// # A table at a time, in the registry's order
//
// The changelog keeps no order across subjects (ADR-0014), and a seat's
// tables are not independent of each other: a skill's versions reference it,
// and foreign keys are on in this store. Every publish sends a seat's skills
// again, whole, and a skill's version once, so from the seat's second publish
// on a skill's only copy sits after the versions published before it. One
// replay of the seat's subjects in the changelog's order met those versions
// first, and on a store that did not already hold the skill their foreign key
// refused the fetch and the seat with it (crewlet/crewlet#1051): on a node
// that never held the seat, which is where placement moves it, on a `[seats]`
// satellite at every boot, and on a node that lost its store. So each table
// is replayed on its own ([Syncer.replay]), in the registry's order
// ([tables]), and every skill the changelog holds is in the store before the
// first version is written, whatever order the changelog holds them in. A
// version whose skill is still not here is left out rather than refusing the
// seat ([table.parent]): its skill is one the changelog does not hold at all,
// or one this store skipped for holding another skill of the seat under the
// same name.
//
// # Each table read to the seat's barrier
//
// A replay cannot end where the copy serving it says it holds nothing more:
// that copy can be behind the changelog's leader ([Syncer.replay]). So after
// it has read the changelog's identity, a hydration publishes a BARRIER to the
// seat's barrier subject ([Syncer.barrier]) and keeps the sequence the
// changelog acknowledged it at. The leader numbers the stream in the order it
// acknowledges, so every row the changelog had acknowledged before — every row
// the seat's last owner flushed before it let the seat go — lies below that
// sequence, and every table's replay reads until it has delivered a barrier at
// or past it and its copy holds nothing more. At or past it, because a
// hydration of the seat racing this one publishes its own barrier, which
// replaces this one's, and both replays end at it ([Syncer.barrier]).
//
// # A hydration writes
//
// The barrier is a WRITE, the one a hydration makes before it reads a row, so
// a broker that refuses this node's writes refuses its hydrations, as it
// already refuses the seat's mailbox and its memory's publishes. An account or
// a server at its storage limit answers the publish with its refusal, which
// refuses the seat wrapped whole. A user without publish permission on the
// memory subjects (crewlet.memory.>) is refused with no acknowledgement at
// all, only an error the broker sends the connection, and the hydration names
// that error once its budget has run out ([Syncer.barrier]). The barrier is no
// row and takes none away: its subject is no table's, and nothing on the
// changelog makes room for a message but the stream's one message per subject
// (topics.MemoryBarrierToken).
//
// # What it reports
//
// The rows its fetches' transactions committed, a failed hydration's
// included: a fetch's rows are counted once its transaction commits, so a
// hydration that fails reports what it left in the store and nothing it
// rolled back. What refuses the seat is the error alone.
func (s *Syncer) Hydrate(ctx context.Context, handle string) (int, error) {
	if s == nil {
		return 0, nil
	}
	ref := s.seat(handle)
	if ref.AgentID == "" {
		// NO ADDRESS ON THE CHANGELOG ([AgentIDFor]): nothing was published
		// for the seat, and a replay over an empty seat token is a consumer
		// the broker refuses to make.
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, hydrateWait)
	defer cancel()

	// CONTRACT 1 FIRST, before a single row is read.
	//
	// A recreated changelog is EMPTY, so every check below reads it as a
	// seat with nothing to carry and reports success. That is not a
	// degraded hydration — it is every seat on this node silently losing
	// its diary and its episodes, with no `memory_hydrated` line, as for a
	// seat that never wrote any, and nothing anywhere saying why.
	if err := s.checkIdentity(ctx); err != nil {
		return 0, err
	}
	// THE BARRIER, once the identity is read and before any table is: the
	// sequence every table's replay reads to.
	barrier, err := s.barrier(ctx, handle, ref.AgentID)
	if err != nil {
		return 0, err
	}

	carried := 0
	// The rows a replay found no parent for, by table: said once every
	// table is replayed, and never written.
	orphaned := map[string]int{}
	for _, spec := range tables {
		wrote, err := s.replay(ctx, handle, ref, spec, barrier, orphaned)
		carried += wrote
		if err != nil {
			return carried, err
		}
	}

	// AND AGAIN AFTER THE LAST TABLE, because the first reading vouches only
	// for a stream that could have been made again after it. A changelog
	// made again after the barrier is EMPTY and has lost it, so a replay on
	// it waits for a barrier that never comes and refuses the seat. What
	// the barrier cannot see, this reading does: a changelog made again
	// between the first reading and the barrier, which takes the barrier
	// itself, so every table's replay delivers it and nothing else; or one
	// made again between two tables whose sequences have run past the
	// barrier's and which holds a barrier of the seat's past it, another
	// hydration's. Either way the hydration reported success with the rows
	// read before the stream was made again, or none: the amnesia contract
	// 1 refuses, said by nothing. A recreation after this reading took
	// nothing from the replays, which were all done before it.
	if err := s.checkIdentity(ctx); err != nil {
		return carried, err
	}
	s.settleNotCarried(ctx, handle, ref.AgentID, orphaned)
	if carried > 0 {
		log.InfoContext(ctx, "memory_hydrated", "seat", handle, "rows", carried)
	}
	return carried, nil
}

// replay carries one table of a seat's memory into this node's store, and says
// how many rows it wrote: every row of the table the changelog acknowledged
// before the hydration's barrier, at sequence barrier, but one a publish
// racing the hydration moves past it ("What the copy's count can miss",
// below), and every one since that the copy serving the replay has counted by
// the time it ends, a pull at a time, each bounded in messages and in bytes
// ([fetchBatch], [Syncer.pullBytes]) and its rows written in one transaction.
// A row whose parent the store does not hold is not written, and is counted in
// orphaned instead ([table.parent]).
//
// # Why a replay reads to a barrier
//
// On a fleet the changelog is replicated, and a replay is an ephemeral R1
// consumer, which the server places on a RANDOM member of the stream
// (nats-server jetstream_cluster.go, createGroupForConsumer) and which
// delivers out of THAT member's copy. A follower applies a row a moment after
// the leader acknowledged it, so a replay hosted on one that is behind counts
// its own copy, delivers it, and stops — short of the rows the seat's last
// owner wrote just before placement moved it, which are the seat's newest.
// The hydration reported that as success, and the seat served with the
// amnesia this package exists to prevent.
//
// NO CONSUMER CAN BE ASKED FOR THE LEADER'S COUNT INSTEAD. The server puts an
// R1 consumer on a random active member, preferring those caught up in the
// METADATA group rather than in the stream's; a consumer's configuration names
// no placement to ask for the leader by; and the count a consumer reports is
// its own server's copy's. And a read of the leader that answers with a row is
// no way out either: it answers the row whole, base64-encoded in JSON, a third
// larger than the row, so the first row a copy behind the leader lacked, read
// that way, crossed a `[seats]` satellite's leaf link as a reply past the
// 8 MiB the leaf carries, and the leaf closed its whole link to the fleet,
// its lease renewals with it (nats-server leafnode.go, processLeafMsgArgs;
// crewlet/crewlet#1031).
//
// So the replay reads to the BARRIER ([Syncer.barrier]) through its OWN
// consumer, filtered to the table's subjects and the seat's barrier subject
// at once ([barrierSubject]). A copy applies the stream in its order, and a
// consumer delivers in that order too, so the copy hands over the table's rows
// below the barrier, then the barrier, once it has applied that far. The
// replay is done when it has received a barrier at or past the hydration's and
// its copy says it holds nothing more. AT OR PAST, because a hydration of the
// seat racing this one, on another node in crewlet/crewlet#511's window,
// publishes its own barrier after this one's, which replaces it (the changelog
// keeps one message per subject): this replay then receives the later
// barrier, past its own, and ends there as the other's does. Rows written
// since the barrier, by a publish racing the hydration, are read like any
// other once the copy has counted them; one it counts only after the replay
// has ended stays on the changelog for the seat's next hydration. A barrier
// below the hydration's is an earlier hydration's, on a copy that has not
// applied this one yet, and is skipped. While no barrier at or past it has
// arrived, the replay waits for its copy with the fetch of one message the
// loop makes anyway, and a copy that has not applied the barrier by the end of
// the hydration's budget refuses the seat, naming the barrier and how far the
// copy had come. Nothing crosses the link but the table's own rows, raw, in
// its pulls.
//
// # A copy behind
//
// The barrier lies past the WHOLE changelog as the leader held it, every
// seat's writes and not only this table's, so a copy behind the leader holds
// the replay until it has applied all of it, even where it already holds every
// row of the table. The server prefers members caught up in its metadata
// group, not in the stream, so a member still catching its copy up after a
// restart is given about a third of the replays on three members, and a
// hydration of six tables meets it nine times in ten: such a hydration waits
// for that member, and is refused if its budget runs out first, until the
// next sweep. An end that asked the leader where the table's rows end did not
// wait where the copy held them all; this one asks the leader nothing, and
// that is its price.
//
// # What the copy's count can miss
//
// The count the replay ends on is the server's RUNNING count of what the copy
// still holds for the consumer, moved as the server stores, delivers or
// removes a message (nats-server consumer.go), and a message that replaces
// another on its subject is the one change it does not make at once: it takes
// the older off the count as it removes it, and adds the newer a moment later,
// from the queue it signals consumers through (stream.go, signalConsumersLoop).
// So a publish racing the hydration (a releasing node's flush still landing, a
// node that lost the seat's lease still publishing it, crewlet/crewlet#511)
// can put a newer copy of a row the changelog held before the barrier past the
// barrier just before the replay reached the older one, and a barrier
// delivered in that moment says the copy holds nothing more: the replay ends
// with neither copy of the row, which stays on the changelog for the seat's
// next hydration. The same race can leave the count one message HIGH instead,
// where the server's delivery skips the removed row before the removal reaches
// the count (consumer.go, checkNumPending's note): the replay's last pull then
// waits for a message that never comes until its expiry, near the end of the
// budget, and the seat is refused as though the next row were larger than a
// pull ([errRowPastPull]), until the next sweep. Either needs a second
// publisher of the seat during its hydration; a pull that read the copy's
// store rather than its count would close both, at a wait on every table.
func (s *Syncer) replay(ctx context.Context, handle string, ref seatRef, spec table,
	barrier uint64, orphaned map[string]int) (int, error) {

	filter, subject := tableFilter(ref.AgentID, spec.name), barrierSubject(ref.AgentID)

	// An EPHEMERAL consumer over this table's subjects and the seat's
	// barrier's, from the start of the stream. Ephemeral because this is a
	// one-shot read of a keyed table rather than a subscription with a
	// position to remember: the next hydration wants the whole current
	// picture again, not what has changed since some previous node read it.
	consumer, err := s.js.CreateConsumer(ctx, topics.MemoryStream, jetstream.ConsumerConfig{
		FilterSubjects: []string{filter, subject},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		AckPolicy:      jetstream.AckNonePolicy,
	})
	if err != nil {
		return 0, fmt.Errorf("memsync: open a replay of %s's %s: %w", handle, spec.name, err)
	}
	defer s.dropConsumer(ctx, consumer)

	// HOW MANY MESSAGES THE REPLAY'S COPY HOLDS, BEFORE READING ANY. This
	// runs inside seat acquisition, so a blind fetch is not affordable: a
	// fetch asks for a number of messages and waits until it has them or its
	// budget is spent, and one asking for more than are there waits the
	// whole budget out, while the seat's first lease renewal comes due.
	// Measured — two seats waiting that out cost four seconds and the node
	// lost both leases. So every fetch asks for what the copy says it has, but
	// one: the fetch of one that waits for a copy behind the leader to apply
	// the barrier, which nothing but the hydration's budget bounds (below).
	//
	// READ OFF THE REPLY THAT CREATED THE CONSUMER, which is the consumer's
	// info as it started on the member the server placed it on (nats-server
	// consumer.go: setLeader snapshots it, initialInfo answers the create
	// with it), counted out of that member's copy as a later info would be.
	// Asked for again, the same count cost a round trip per table.
	at := cursor{pending: int(consumer.CachedInfo().NumPending)}

	// UNTIL A BARRIER AT OR PAST THE HYDRATION'S HAS ARRIVED AND THE COPY
	// HOLDS NOTHING MORE, never on the second alone: a copy behind the leader
	// says it holds nothing more while the leader holds the table's newest
	// rows and the barrier past them, and asked for one more message it hands
	// the next over as soon as it applies it, which is what the fetch of one
	// below waits for. At or past, never only at: a racing hydration's barrier
	// replaces this one's, and is the one the copy then holds.
	carried := 0
	for {
		if at.barrier >= barrier && at.pending == 0 {
			return carried, nil
		}
		want := 1
		if at.pending > 0 {
			want = min(at.pending, fetchBatch)
		}
		// ON THE HYDRATION'S OWN CONTEXT, so the fetches share its one
		// budget: each used to wait a whole hydrateWait of its own, so a
		// replay that stalled twice held seat acquisition for twice the
		// bound this package documents. AND BOUNDED IN BYTES as well as in
		// messages, because a pull is held whole until its transaction has
		// written it ([Syncer.pullBytes]).
		bound := s.pullBytes()
		batch, err := consumer.Fetch(want, jetstream.FetchContext(ctx),
			jetstream.FetchMaxBytes(bound))
		if err != nil {
			return carried, fmt.Errorf("memsync: replay %s's %s: %w", handle, spec.name, err)
		}
		rows, delivered, err := received(batch, handle, spec, subject, &at)
		if err != nil {
			return carried, err
		}
		if delivered == 0 && at.pending > 0 && ctx.Err() == nil {
			// A ROW LARGER THAN THIS NODE'S PULLS CARRY. The copy holds
			// more of the table's messages and the budget is not spent,
			// so the pull ended with none because the next is larger
			// than its bytes — a row, since a barrier has none: the
			// broker ends such a pull rather than send it (`409 Message
			// Size Exceeds MaxBytes`, which the client reads as the
			// pull's ordinary end), and every pull of this bound ends
			// the same way, on every node behind this limit, at every
			// sweep. Neither a copy behind its leader nor a spent
			// budget, so it is said as what it is, naming the limit to
			// align ([Syncer.pullBytes]). COUNTED IN MESSAGES, not rows:
			// a pull that delivered a barrier alone delivered something.
			return carried, fmt.Errorf("memsync: replay %s's %s: %w: the copy serving it "+
				"holds %d more of the table's messages and sent none in a pull of up to %d "+
				"bytes, one message at this node's connection's max_payload of %d bytes "+
				"and the room the broker counts beside it. Such a row was published "+
				"through a server, an account or a user that carries more than this "+
				"node's connection does: every node's connection to the broker must "+
				"carry the same max_payload (docs/guides/deployment.md, %q)",
				handle, spec.name, errRowPastPull, at.pending, bound, bound-deliveryOverhead,
				"The largest message every server must carry")
		}
		if delivered == 0 {
			// THE BUDGET RAN OUT SHORT OF THE BARRIER, or with the copy
			// holding more past it. Refused, never admitted short: what
			// the copy had not delivered is the table's NEWEST, and a
			// hydration that reported success without it is the amnesia
			// this package exists to prevent, said by no line anywhere.
			// The seat waits for the next placement sweep, as it does on
			// every other failure here.
			short := fmt.Sprintf("had not applied the barrier this hydration published at "+
				"sequence %d", barrier)
			if at.pending > 0 {
				short = fmt.Sprintf("holds %d more", at.pending)
			}
			return carried, fmt.Errorf("memsync: replay %s's %s: the copy serving it had "+
				"delivered the table's messages to sequence %d, and %s, when the "+
				"hydration's budget ran out", handle, spec.name, at.reached, short)
		}
		if len(rows) == 0 {
			// A PULL OF A BARRIER ALONE, which writes nothing.
			continue
		}
		var wrote, skipped int
		if err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			// COUNTED AFRESH BY EVERY RUN of the body, which the store
			// may run again ([store.DB.Tx]).
			wrote, skipped = 0, 0
			for _, row := range rows {
				held, err := spec.parentHeld(ctx, tx, row)
				if err != nil {
					return err
				}
				if !held {
					skipped++
					continue
				}
				if err := upsert(ctx, tx, spec, row); err != nil {
					return err
				}
				wrote++
			}
			return nil
		}); err != nil {
			return carried, err
		}
		// COUNTED ONCE THE TRANSACTION HAS COMMITTED, never as each row is
		// written: a row the store refuses rolls back every row of its
		// fetch, and a hydration that had counted them reported rows it
		// had rolled back, with the store holding none of them.
		carried += wrote
		if skipped > 0 {
			orphaned[spec.name] += skipped
		}
	}
}

// pullBytes is the byte bound of every replay pull: the largest message this
// node's connection to the broker carries, its max_payload, and the room the
// broker counts beside it ([deliveryOverhead]).
//
// It bounds what a hydration HOLDS of the changelog's rows. A pull is held
// whole until its transaction has written it, and a hydration makes one at a
// time ([Syncer.replay]), so it holds one pull's bytes of rows at once, and
// nothing beside them: 8 MiB and 4 KiB on the embedded broker, where a pull
// bounded by its row count alone ([fetchBatch]) held up to 2 GiB.
//
// # Never below the largest row the changelog can hold
//
// The broker sends a pull nothing past what is left of its bytes: it ends the
// pull instead (`409 Message Size Exceeds MaxBytes`, which the client reads as
// the pull's ordinary end). So a pull bounded below its next row ends with no
// row, and so does every pull after it with the same bound, and the seat is
// refused ([errRowPastPull]), on every node at every sweep for as long as the
// row is on the changelog. One message at the transport's limit, whole, is the
// least a pull can be bounded at, and it is what this is: a larger bound would
// save only the pulls of rows that large, and hold more of them at once.
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
	return int(s.js.Conn().MaxPayload()) + deliveryOverhead
}

// errRowPastPull is a pull that ended with no message while its copy held more
// of the table's and the hydration's budget was not spent: the next is a row
// larger than a pull of this node's bound carries ([Syncer.pullBytes]).
var errRowPastPull = errors.New("the next row is larger than a pull this node's connection carries")

// errMisroutedRow is a row a replay read on one table's subjects whose body
// names another table ([received]).
var errMisroutedRow = errors.New("a row on one table's subjects names another table, which no build publishes")

// cursor is where one table's replay stands ([Syncer.replay]), moved past each
// message it receives ([received]).
type cursor struct {
	// reached is the highest stream sequence the replay has delivered, a
	// barrier's included.
	reached uint64
	// pending is how many messages its copy says it still holds past them:
	// the table's rows and the seat's barrier, the two its consumer reads.
	pending int
	// barrier is the sequence of the newest barrier the replay has
	// delivered, and 0 while it has delivered none.
	barrier uint64
}

// received is one pull's rows, read whole off the wire and decoded, and how
// many messages it delivered, with at moved past each.
//
// A BARRIER IS TOLD APART BY ITS SUBJECT, the seat's one that is no table's
// ([barrierSubject]), never by its body, and it is no row: never decoded,
// written or counted. Only its sequence is kept, as at.barrier, where a
// barrier below the hydration's leaves the replay to go on ([Syncer.replay]).
// The count of messages is what says whether a pull ended with nothing
// ([errRowPastPull]): one that delivered a barrier alone delivered something,
// and wrote nothing.
//
// READ BEFORE THE FETCH'S TRANSACTION BEGINS, never inside it. A fetch hands
// its rows over as they cross the link, so a transaction that read them held
// this store's write, and so every other writer of the node, for as long as
// they took to arrive: the rest of the budget, for the fetch of one message
// that a copy behind its leader never answers. And the store may run a
// transaction's body again ([store.DB.Tx]), where a fetch read inside it hands
// over nothing the second time. Read first, the rows are what the transaction
// writes, however often it runs; the fetch held them in memory either way.
func received(batch jetstream.MessageBatch, handle string, spec table, barrier string,
	at *cursor) ([]Row, int, error) {

	var rows []Row
	delivered := 0
	for msg := range batch.Messages() {
		delivered++
		meta, err := msg.Metadata()
		if err != nil {
			return nil, delivered, fmt.Errorf("memsync: read the metadata of a replayed %s "+
				"message of %s: %w", spec.name, handle, err)
		}
		at.reached = max(at.reached, meta.Sequence.Stream)
		at.pending = int(meta.NumPending)
		if msg.Subject() == barrier {
			at.barrier = max(at.barrier, meta.Sequence.Stream)
			continue
		}
		row, err := decode(msg.Data())
		if err != nil {
			return nil, delivered, err
		}
		if row.Table != spec.name {
			// A ROW ON ANOTHER TABLE'S SUBJECTS, which no build publishes:
			// a row's subject names its own table ([table.subject]).
			// Written here, it would be written in this table's turn
			// rather than its own, ahead of the parents the turns exist
			// for, so it is refused, as a row that does not decode is.
			return nil, delivered, fmt.Errorf("memsync: replay %s's %s: %w, here one that says "+
				"it is a %q row", handle, spec.name, errMisroutedRow, row.Table)
		}
		rows = append(rows, row)
	}
	if err := batch.Error(); err != nil {
		return nil, delivered, fmt.Errorf("memsync: replay %s's %s: %w", handle, spec.name, err)
	}
	return rows, delivered, nil
}

// settleNotCarried takes off the changelog what of a seat's memory nobody will
// carry again, and says in one line what a hydration did not carry: the rows
// it left out for want of a parent, by table, and the retired tables the
// changelog still held rows of for the seat — each of which it purges of the
// seat's rows, for every node — with purged=false where a purge failed. A
// count or a purge that fails is said (memory_retired_rows_not_purged) and
// tried again by the next hydration.
//
// A RETIRED TABLE IS COUNTED, NEVER REPLAYED: a hydration needs to know only
// whether the changelog holds any of the seat's rows of it, which the reply
// that creates a consumer over the table's subjects says without delivering
// one ([Syncer.held]), and a retired table's rows are the ones nobody bounded,
// a conversation's ledger entry kept verbatim (crewlet/crewlet#349). Counted
// WITHOUT THE BARRIER the replays read to: the consumer counts a copy that may
// be behind, which costs nothing here, since only a build that still carried
// the table publishes its rows, and a row a copy had not applied yet is purged
// by the seat's next hydration.
//
// A table only a NEWER build carries is not read at all, and its rows stay on
// the changelog for that build: the replays read the tables this build knows,
// a table at a time, and finding any other would mean reading every one of the
// seat's rows.
func (s *Syncer) settleNotCarried(ctx context.Context, handle, agentID string, orphaned map[string]int) {
	said := notCarried{purged: true, orphaned: orphaned}
	for _, name := range retiredTables {
		filter := tableFilter(agentID, name)
		n, err := s.held(ctx, filter)
		if err == nil && n == 0 {
			continue
		}
		if err == nil {
			said.retired = append(said.retired, name)
			err = s.purge(ctx, filter)
		}
		if err != nil {
			said.purged = false
			log.WarnContext(ctx, "memory_retired_rows_not_purged", "seat", handle, "table", name,
				"error", err.Error(), "detail", "the next hydration of the seat tries again")
		}
	}
	if len(said.retired) == 0 && len(orphaned) == 0 {
		return
	}
	left := make([]string, 0, len(orphaned))
	for _, name := range slices.Sorted(maps.Keys(orphaned)) {
		left = append(left, fmt.Sprintf("%s:%d", name, orphaned[name]))
	}
	log.InfoContext(ctx, "memory_rows_not_carried", "seat", handle,
		"retired", strings.Join(said.retired, ","), "purged", said.purged && len(said.retired) > 0,
		"orphaned", strings.Join(left, ","))
	if s.leftOut != nil {
		s.leftOut(said)
	}
}

// notCarried is what one hydration did not carry, as its memory_rows_not_carried
// line says it: the retired tables the changelog held the seat's rows of,
// whether every count and purge of them went through, and the rows left out
// for want of a parent, by table.
type notCarried struct {
	retired  []string
	purged   bool
	orphaned map[string]int
}

// held is how many rows the changelog holds on filter's subjects, as the copy
// a consumer over them is served by counts them: the reply that creates the
// consumer says that, with no row delivered and no request of its own, as a
// table's replay reads its count ([Syncer.replay]).
func (s *Syncer) held(ctx context.Context, filter string) (int, error) {
	consumer, err := s.js.CreateConsumer(ctx, topics.MemoryStream, jetstream.ConsumerConfig{
		FilterSubject: filter,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckNonePolicy,
	})
	if err != nil {
		return 0, fmt.Errorf("memsync: open a count of %s: %w", filter, err)
	}
	defer s.dropConsumer(ctx, consumer)
	return int(consumer.CachedInfo().NumPending), nil
}

// dropConsumer deletes one of a hydration's ephemeral consumers as the step
// that made it ends.
//
// The consumer is ephemeral, so the server reaps it when it goes idle — but a
// node acquiring seats in a sweep would otherwise leave several per seat lying
// around until then.
//
// WithoutCancel because a cleanup that inherits the failure it is cleaning up
// after does nothing at all — and then its OWN bound, because WithoutCancel
// drops the deadline with the cancellation. This runs inside seat
// acquisition, so an unbounded call here would hang the acquisition on exactly
// the wedged broker that made the hydration fail ([consumerCleanupTimeout]).
func (s *Syncer) dropConsumer(ctx context.Context, consumer jetstream.Consumer) {
	cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), consumerCleanupTimeout)
	defer stop()
	_ = s.js.DeleteConsumer(cleanupCtx, topics.MemoryStream, consumer.CachedInfo().Name)
}

// purge removes every row on filter's subjects from the changelog, for every
// node.
func (s *Syncer) purge(ctx context.Context, filter string) error {
	stream, err := s.js.Stream(ctx, topics.MemoryStream)
	if err != nil {
		return fmt.Errorf("memsync: reach the changelog: %w", err)
	}
	if err := stream.Purge(ctx, jetstream.WithPurgeSubject(filter)); err != nil {
		return fmt.Errorf("memsync: purge %s: %w", filter, err)
	}
	return nil
}

// mark reads a watermark.
func (s *Syncer) mark(handle, table string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marks[handle+"\x00"+table]
}

// setMark advances a watermark, never backwards.
func (s *Syncer) setMark(handle, table string, high int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := handle + "\x00" + table
	if high > s.marks[key] {
		s.marks[key] = high
	}
}

// Forget drops a released seat's watermarks.
//
// It zeroes the seat's marks, so the seat's next holding here carries again
// everything this node holds for it, as a restart does: that republish is what
// puts this node's rows back on a changelog an operator accepted after a
// recreation, or one restored while the node ran. (Old marks would not skip
// what another node wrote meanwhile: a hydration stamps every row it inserts
// above any of them, node migration 0041.)
//
// A [Syncer.Publish] of the seat already in flight goes on: it continues the
// table it is in from its own cursor, starts each later table from the zeroed
// mark, and re-creates the seat's marks as its rows are acknowledged.
func (s *Syncer) Forget(handle string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.marks {
		// HasPrefix, not a slice: the map holds every seat this node
		// has published, and slicing a key shorter than this handle
		// panics rather than simply not matching.
		if strings.HasPrefix(key, handle+"\x00") {
			delete(s.marks, key)
		}
	}
}

// THE TWO CONTRACTS THIS PACKAGE DECLARES, and what each buys.
//
// # ReplayCompacted, declared rather than implemented privately
//
// The memory changelog is a compacted stream: one message retained per
// subject, so a replay from the start is the CURRENT picture rather than a
// history. Every rule in [Syncer.Hydrate] follows from that — the consumer is
// ephemeral because the next hydration wants the whole picture again, the
// upsert is idempotent because a redelivery is the same row, and deletes
// deliberately do not travel because a tombstone would be a second thing to
// keep correct forever.
//
// It was true here before it was said. Naming the protocol is what makes the
// difference between a package that happens to work under compaction and one
// that has DECLARED the shape it depends on — and the difference shows the
// first time somebody changes the stream's retention.
//
// # Stream identity, which it did not have at all
//
// See [statelog.IdentityOf]. A compacted stream has no checkpoint and no
// generation to answer a recreation with, so its only honest answer is to
// refuse the work that depended on it — which for this package means refusing
// the seat rather than admitting one with an empty memory.

// ReplayProtocol is the contract this package's stream is read under.
//
// DECLARED, so a change to the changelog's retention has something to
// contradict.
func (s *Syncer) ReplayProtocol() statelog.ReplayProtocol { return statelog.ReplayCompacted }

// checkIdentity refuses a hydration from a changelog that is not the one this
// node last saw.
//
// # Why it refuses rather than repairs
//
// There is nothing to repair. A recreated changelog holds nothing, and what
// this node needs is on the stream that is gone — so the choices are to admit
// the seat with an empty memory or to refuse it, and only one of those is
// visible. A seat admitted empty answers "I have never spoken to you before"
// to somebody it has worked with for months, and nothing anywhere says why.
//
// # And why an unreadable instant refuses too
//
// UNKNOWN IS NOT SAME. Reading a broker that would not answer as "the stream I
// know" resumes against a stream this node cannot identify, which is the whole
// failure this guard is about. It costs a delayed seat on a broker having a
// bad minute, which the caller retries.
func (s *Syncer) checkIdentity(ctx context.Context) error {
	// ASKED AS A METADATA READ IS ([jsprovision.ReadBounded]): only the
	// stream's leader answers its info, and a member that still believes in
	// a leader its group has just lost drops the request without a word, so
	// one request waited out the hydration's whole bound and refused the
	// seat over a reply nobody would send — where asked again, the new
	// leader answers. A handle of each request's own, because the requests
	// can be out together and the client caches each reply on its handle
	// with no lock.
	info, err := jsprovision.ReadBounded(ctx, s.js.Options().DefaultTimeout,
		func(ctx context.Context) (*jetstream.StreamInfo, error) {
			stream, err := s.js.Stream(ctx, topics.MemoryStream)
			if err != nil {
				return nil, err
			}
			return stream.CachedInfo(), nil
		}, nil)
	if err != nil {
		return fmt.Errorf("memsync: read the memory changelog's identity: %w — "+
			"a seat is not admitted from a stream this node cannot identify, "+
			"because a recreated one is EMPTY and would hydrate every seat to "+
			"nothing while reporting success", err)
	}
	// THE ONE READING of a stream's identity: the instant stamped at its
	// creation, which a restored changelog keeps and the broker's own
	// instant does not ([statelog.CreatedAtKey]).
	created, err := statelog.StreamCreatedAt(info)
	if err != nil {
		return fmt.Errorf("memsync: read the memory changelog's identity: %w", err)
	}

	recorded, known, err := statelog.RecordedIdentity(ctx, s.db, topics.MemoryStream)
	if err != nil {
		return err
	}
	switch state := statelog.IdentityOf(recorded, created, known); state {
	case statelog.StreamSame:
		return nil
	case statelog.StreamFirstSight:
		// A FRESH NODE, a fresh company, or the first boot after this
		// guard shipped. Recorded rather than refused: refusing here
		// would make every first boot an outage.
		if err := statelog.RecordIdentity(ctx, s.db, topics.MemoryStream,
			created, time.Now().UTC()); err != nil {
			return err
		}
		return nil
	case statelog.StreamUnknown:
		return fmt.Errorf("memsync: the memory changelog reports no creation "+
			"instant, so this node cannot tell it from a recreated one: %w",
			statelog.ErrStreamRecreated)
	default:
		log.ErrorContext(ctx, "memory_changelog_recreated",
			"stream", topics.MemoryStream,
			"recorded_created_at", recorded, "stream_created_at", created,
			"detail", "this node refuses to hydrate a seat's memory from a "+
				"changelog it has not seen before: a recreated stream is EMPTY, "+
				"so hydrating would report success and give every seat on this "+
				"node a blank diary. Nothing here can recover what was on the "+
				"old stream; an operator who intends the new one clears this "+
				"node's stream_identity row")
		return fmt.Errorf("memsync: the memory changelog was created at %s and "+
			"this node last saw one created at %s: %w",
			created.Format(time.RFC3339), recorded.Format(time.RFC3339),
			statelog.ErrStreamRecreated)
	}
}

// barrier publishes the seat's barrier, a message of no bytes on its barrier
// subject ([barrierSubject]), and says the stream sequence the changelog
// acknowledged it at: the leader numbers the stream in the order it
// acknowledges, so every row the changelog had acknowledged before lies below
// it, and every table's replay reads to it ([Syncer.replay]).
//
// # Asked again while nobody answers it
//
// As a metadata read is ([jsprovision.ReadBounded]): presumed lost after the
// read term and asked again a pause later, every request kept outstanding and
// the first acknowledgement taken. Only the stream's leader takes a publish —
// no other member subscribes to the stream's subjects (nats-server stream.go,
// setLeader) — and a member that still believes it leads after its group has
// moved on drops one without a reply: one it queued while it led and takes
// once it no longer does (jetstream_cluster.go, processClusteredInboundMsg),
// and one whose proposal its term outlived (jetstream_batching.go,
// commitSingleMsg). Sent once, such a publish waited out the hydration's whole
// budget and refused the seat until the next sweep. The client's own retry is
// no help there: it asks again only on `no responders`, the moment no member
// subscribes at all, twice a quarter of a second apart, and then gives up with
// [jetstream.ErrNoStreamResponse] — which is read here as the silence it is,
// so the hedge asks again after its pause rather than refusing the seat.
//
// A WRITE ASKED AS A READ IS, which jsprovision keeps from a metadata write: a
// create's reply is held until the object it made has a leader, and a second
// create beside the first is a second write. Neither holds here: a publish is
// answered once the stream has stored it, and a second copy of this one is no
// second fact. The stream keeps one message per subject, so a barrier
// published twice is one barrier, the later; and whichever acknowledgement is
// taken, the barrier the asking leaves on the stream lies at or past its
// sequence, which is all a replay holds a barrier to. A copy that has applied
// an earlier request's and not the later one's delivers a barrier below it,
// which the replay skips, as it skips an earlier hydration's.
//
// # Two hydrations of the seat at once
//
// For the same reason. A seat can be hydrated by two nodes at once while it
// moves, in crewlet/crewlet#511's window: a node still taking a seat whose
// claim has lapsed under it, and the node that has claimed it since. Each
// publishes a barrier to the seat's one subject, so the later replaces the
// earlier, and the earlier hydration's replays deliver the later one instead
// of their own: it lies past their own, so they end at it, as the later
// hydration's do. However many hydrations of a seat there have been, the
// changelog holds one barrier of it.
//
// # When nobody acknowledges it
//
// A hydration refused for its barrier names why. An answer is the broker's
// own, wrapped whole: an account or a server at its storage limit refuses the
// publish in its acknowledgement. A broker that refuses this node's
// connection publish permission on the memory subjects answers the publish
// with nothing at all: it sends the connection a permissions error instead,
// which the client hands to its error handler (jetstream_client_error) and
// keeps as the connection's last error, and the publish waits for an
// acknowledgement that never comes. So a barrier still unacknowledged when the
// budget runs out is refused with that error where the connection's last one
// names this subject, and otherwise with what can leave a publish unanswered
// that long.
func (s *Syncer) barrier(ctx context.Context, handle, agentID string) (uint64, error) {
	subject := barrierSubject(agentID)
	ack, err := jsprovision.ReadBounded(ctx, s.js.Options().DefaultTimeout,
		func(ctx context.Context) (*jetstream.PubAck, error) {
			ack, err := s.js.Publish(ctx, subject, nil)
			if errors.Is(err, jetstream.ErrNoStreamResponse) {
				return nil, fmt.Errorf("%w: %w", nats.ErrNoResponders, err)
			}
			return ack, err
		}, nil)
	switch {
	case err == nil:
		return ack.Sequence, nil
	case !unacknowledged(err):
		return 0, fmt.Errorf("memsync: publish %s's barrier to %s: %w", handle, subject, err)
	}
	if refused := s.refusedPublish(subject); refused != nil {
		return 0, fmt.Errorf("memsync: publish %s's barrier to %s: the broker refused it (%w): a "+
			"hydration writes the seat's barrier before it reads a row, so this node's "+
			"connection needs publish permission on %s>, as the seat's memory does: %w",
			handle, subject, refused, topics.MemoryPrefix, err)
	}
	return 0, fmt.Errorf("memsync: publish %s's barrier to %s: no member of the changelog "+
		"acknowledged it before the hydration's budget ran out: the changelog had no leader "+
		"that long, or the broker refused the publish without an answer, as it refuses a "+
		"connection without publish permission on %s> (jetstream_client_error says so): %w",
		handle, subject, topics.MemoryPrefix, err)
}

// unacknowledged reports a publish nobody answered: the silence the hedge
// heard, or the hydration's deadline.
func unacknowledged(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoResponders)
}

// refusedPublish is the broker's refusal of this node's connection publishing
// to subject, where the connection's last error from the broker is that
// refusal: a permissions violation, which the broker sends the connection
// rather than answering the publish (nats.go's processTransientError keeps it
// as the connection's last error), naming the subject it refused.
func (s *Syncer) refusedPublish(subject string) error {
	conn := s.js.Conn()
	if conn == nil {
		return nil
	}
	last := conn.LastError()
	if !errors.Is(last, nats.ErrPermissionViolation) ||
		!strings.Contains(strings.ToLower(last.Error()), strings.ToLower(`publish to "`+subject+`"`)) {
		return nil
	}
	return last
}
