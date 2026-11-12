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

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
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
const hydrateWait = 15 * time.Second

// consumerCleanupTimeout bounds deleting the ephemeral replay consumer.
//
// Five seconds against one API round trip: far past a healthy answer, and
// bounded because this runs inside seat acquisition. What it costs when it
// expires is nothing an operator sees — the server reaps an idle ephemeral
// consumer on its own, which is why the deletion is hygiene rather than a
// step whose failure means anything.
const consumerCleanupTimeout = 5 * time.Second

// fetchBatch is how many rows one replay pull asks for.
//
// It sets the replay's round trips: 256 clears a seat with a few hundred
// episodes in a couple of pulls. It is no bound on memory: a pull holds up to
// this many rows whole, and a row is bounded only by the transport
// ([exportPageBytes]), so where a publish holds a page, a pull can hold up to
// 2 GiB on the embedded broker (crewlet/crewlet#1052).
const fetchBatch = 256

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
// changelog: [Syncer.Publish] carries nothing for it rather than publish to a
// subject with an empty seat token, which the broker refuses, and
// [Syncer.Hydrate]'s filter over such a token matches nothing.
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
	// leader reads the changelog's newest rows from its leader
	// ([Syncer.leaderEnd]).
	leader *jsapi.Leader

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
func New(db *store.DB, js jetstream.JetStream, agentID AgentIDFor) (*Syncer, error) {
	if db == nil || js == nil || agentID == nil {
		return nil, nil
	}
	leader, err := jsapi.NewLeader(js, topics.MemoryStream)
	if err != nil {
		return nil, fmt.Errorf("memsync: address the memory changelog's leader: %w", err)
	}
	return newSyncer(db, js, agentID, leader), nil
}

// newSyncer is [New] past its checks: a syncer over js, whose changelog's
// end [Syncer.Hydrate] reads through leader.
//
// A NIL LEADER IS A SYNCER THAT ONLY PUBLISHES, which is how this package's
// cases build one over a changelog of their own: [jsapi.NewLeader] reads the
// client's connection and options, and a stand-in client has neither. Such a
// syncer must never hydrate.
func newSyncer(db *store.DB, js jetstream.JetStream, agentID AgentIDFor, leader *jsapi.Leader) *Syncer {
	return &Syncer{db: db, js: js, agentID: agentID, leader: leader, marks: map[string]int64{}}
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

// Hydrate brings a seat's memory into this node's store.
//
// Called in seat acquisition BEFORE the mailbox attaches, so a seat never
// serves a turn against a store that has forgotten it. A failure refuses the
// seat, which is the honest outcome: a peer that takes the seat instead may
// have the memory, and a seat that runs with amnesia produces work its own
// history contradicts.
func (s *Syncer) Hydrate(ctx context.Context, handle string) (int, error) {
	if s == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, hydrateWait)
	defer cancel()
	ref := s.seat(handle)

	// CONTRACT 1 FIRST, before a single row is read.
	//
	// A recreated changelog is EMPTY, so every check below reads it as a
	// seat with nothing to carry and reports success. That is not a
	// degraded hydration — it is every seat on this node silently losing
	// its diary and its episodes, with a `memory_hydrated` line saying
	// zero rows and nothing anywhere saying why.
	if err := s.checkIdentity(ctx); err != nil {
		return 0, err
	}
	filter := topics.MemoryPrefix + ref.AgentID + ".>"

	// THE LEADER'S END OF THIS SEAT'S ROWS, before the replay is opened —
	// see [Syncer.leaderEnd] for why the replay alone cannot be trusted to
	// have reached it. Nothing there is the leader's answer that the seat
	// has no memory, which is every seat on a fresh company, and it costs
	// one round trip rather than a consumer.
	end, err := s.leaderEnd(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("memsync: find where %s's memory ends: %w", handle, err)
	}
	if end == 0 {
		return 0, nil
	}

	// An EPHEMERAL consumer over this seat's subjects, from the start of
	// the stream. Ephemeral because this is a one-shot read of a keyed
	// table rather than a subscription with a position to remember: the
	// next hydration wants the whole current picture again, not what has
	// changed since some previous node read it.
	consumer, err := s.js.CreateConsumer(ctx, topics.MemoryStream, jetstream.ConsumerConfig{
		FilterSubject: filter,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckNonePolicy,
	})
	if err != nil {
		return 0, fmt.Errorf("memsync: open a replay for %s: %w", handle, err)
	}

	defer func() {
		// The consumer is ephemeral, so the server reaps it when it goes
		// idle — but a node acquiring seats in a sweep would otherwise
		// leave one per seat lying around until then.
		//
		// WithoutCancel because a cleanup that inherits the failure it is
		// cleaning up after does nothing at all — and then its OWN bound,
		// because WithoutCancel drops the deadline with the cancellation.
		// This runs before Hydrate returns, inside seat acquisition, so an
		// unbounded call here would hang the acquisition on exactly the
		// wedged broker that made the hydration fail.
		cleanupCtx, stop := context.WithTimeout(
			context.WithoutCancel(ctx), consumerCleanupTimeout)
		defer stop()
		_ = s.js.DeleteConsumer(cleanupCtx,
			topics.MemoryStream, consumer.CachedInfo().Name)
	}()

	// HOW MANY ROWS THE REPLAY'S COPY HOLDS, BEFORE READING ANY. This runs
	// inside seat acquisition, so a blind fetch is not affordable: a fetch
	// asks for a number of rows and waits until it has them or its budget
	// is spent, and one asking for more than are there waits the whole
	// budget out, while the seat's first lease renewal comes due. Measured
	// — two seats waiting that out cost four seconds and the node lost
	// both leases. So every fetch asks for what the copy says it has.
	info, err := consumer.Info(ctx)
	if err != nil {
		// FAILS THE HYDRATION rather than reading as "nothing to carry".
		// Those are not the same fact, and collapsing them here is worse
		// than anywhere else in this package: every other failure path
		// refuses the seat, so a swallowed error would be the one way a
		// seat is admitted with an empty memory AND a success reported.
		return 0, fmt.Errorf("memsync: count %s's memory: %w", handle, err)
	}
	pending := int(info.NumPending)

	// UNTIL THE REPLAY HAS REACHED THE LEADER'S END, and not merely until
	// the copy serving it has nothing more to say. A copy that is behind
	// says it has nothing more while the leader holds the seat's newest
	// rows; asked for one more row, it hands that row over as soon as it
	// applies it, which is what the fetch of one below waits for.
	carried := 0
	// The rows this build does not carry, by table: a retired table's are
	// purged after the replay, a newer build's are left for it.
	notCarried := map[string]int{}
	var reached uint64
	for reached < end {
		want := 1
		if pending > 0 {
			want = min(pending, fetchBatch)
		}
		// ON THE HYDRATION'S OWN CONTEXT, so the fetches share its one
		// budget: each used to wait a whole hydrateWait of its own, so a
		// replay that stalled twice held seat acquisition for twice the
		// bound this package documents.
		batch, err := consumer.Fetch(want, jetstream.FetchContext(ctx))
		if err != nil {
			return carried, fmt.Errorf("memsync: replay %s: %w", handle, err)
		}
		got := 0
		if err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			for msg := range batch.Messages() {
				got++
				meta, metaErr := msg.Metadata()
				if metaErr != nil {
					return metaErr
				}
				reached = max(reached, meta.Sequence.Stream)
				pending = int(meta.NumPending)
				row, spec, known, decodeErr := decode(msg.Data())
				if decodeErr != nil {
					return decodeErr
				}
				if !known {
					// Skipped rather than fatal: a rolling upgrade
					// puts a newer build's tables on this stream.
					notCarried[row.Table]++
					continue
				}
				if err := upsert(ctx, tx, spec, row); err != nil {
					return err
				}
				carried++
			}
			return batch.Error()
		}); err != nil {
			return carried, err
		}
		if got == 0 {
			// THE BUDGET RAN OUT SHORT OF THE LEADER'S END. Refused,
			// never admitted short: the rows past what the copy
			// delivered are the seat's NEWEST, and a hydration that
			// reported success without them is the amnesia this
			// package exists to prevent, said by no line anywhere.
			// The seat waits for the next placement sweep, as it does
			// on every other failure here.
			return carried, fmt.Errorf("memsync: replay %s: the copy serving it "+
				"reached sequence %d of the %d the stream's leader holds for "+
				"this seat before the hydration's budget ran out", handle, reached, end)
		}
	}
	s.settleNotCarried(ctx, handle, ref.AgentID, notCarried)
	log.InfoContext(ctx, "memory_hydrated", "seat", handle, "rows", carried)
	return carried, nil
}

// settleNotCarried purges the seat's rows of each retired table a hydration
// met and says, in one line, what it did not carry: a retired table's rows
// once, a newer build's on every hydration that meets them. A purge that fails
// is said and tried again by the next hydration.
func (s *Syncer) settleNotCarried(ctx context.Context, handle, agentID string, notCarried map[string]int) {
	if len(notCarried) == 0 {
		return
	}
	var retired, newer []string
	purged := true
	for _, name := range slices.Sorted(maps.Keys(notCarried)) {
		entry := fmt.Sprintf("%s:%d", name, notCarried[name])
		if !slices.Contains(retiredTables, name) {
			newer = append(newer, entry)
			continue
		}
		retired = append(retired, entry)
		if err := s.purge(ctx, agentID, name); err != nil {
			purged = false
			log.WarnContext(ctx, "memory_retired_rows_not_purged", "seat", handle, "table", name,
				"error", err.Error(), "detail", "the next hydration of the seat purges them")
		}
	}
	log.InfoContext(ctx, "memory_rows_not_carried", "seat", handle,
		"retired", strings.Join(retired, ","), "purged", purged && len(retired) > 0,
		"newer", strings.Join(newer, ","))
}

// purge removes the seat's rows of table from the changelog, for every node.
func (s *Syncer) purge(ctx context.Context, agentID, table string) error {
	stream, err := s.js.Stream(ctx, topics.MemoryStream)
	if err != nil {
		return fmt.Errorf("memsync: reach the changelog: %w", err)
	}
	subject := topics.MemoryPrefix + agentID + "." + table + ".>"
	if err := stream.Purge(ctx, jetstream.WithPurgeSubject(subject)); err != nil {
		return fmt.Errorf("memsync: purge %s: %w", subject, err)
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

// leaderEnd is the stream sequence of the newest row the changelog's LEADER
// holds for one seat's subjects, and 0 when it holds none.
//
// # Why the replay is held to it
//
// On a fleet the changelog is replicated, and the replay is an ephemeral R1
// consumer, which the server places on a RANDOM member of the stream
// (nats-server jetstream_cluster.go, createGroupForConsumer) and which
// delivers out of THAT member's copy. A follower applies a row a moment after
// the leader acknowledged it, so a replay hosted on one that is behind counts
// its own copy, delivers it, and stops — short of the rows the seat's last
// owner wrote just before placement moved it, which are the seat's newest. The
// hydration reported that as success, and the seat served with the amnesia
// this package exists to prevent: the same stale-replica read internal/coord/kv
// reads through the leader to avoid (walk.go's "Every certifying answer is the
// LEADER's" has the measurements).
//
// So the leader is asked first, and the replay is not done until it has
// delivered a row at or past this sequence.
//
// # Why this read is the leader's
//
// It is a `$JS.API.STREAM.MSG.GET` through internal/jsapi's [jsapi.Leader],
// which only the stream's leader answers WHATEVER the stream's allow_direct
// says — the read coord/kv's certified listings and the object store's
// metadata take, for the same reason. The client's own last-message read
// picks its API from that flag alone, so the day the changelog allowed direct
// gets it would be answered by any replica and prove nothing. Its filter is a
// WILDCARD over the seat's subjects, which [jsapi.Leader.Last] takes, the
// answer then the newest message on any subject it matches; and
// [jsapi.ErrNoMessage] is the leader's own word that the seat holds no row.
//
// It is ASKED AGAIN while nobody answers it, as every leader read is: a member
// that still believes in a leader its group has just lost drops the request
// without a word, and one request held for the hydration's bound refused the
// seat over a reply nobody would send, where asked again the new leader
// answers. A member that sees no leader answers an error, which fails the
// hydration rather than reading as a seat with nothing to carry.
func (s *Syncer) leaderEnd(ctx context.Context, filter string) (uint64, error) {
	msg, err := s.leader.Last(ctx, filter)
	switch {
	case errors.Is(err, jsapi.ErrNoMessage):
		return 0, nil
	case err != nil:
		return 0, err
	}
	return msg.Sequence, nil
}
