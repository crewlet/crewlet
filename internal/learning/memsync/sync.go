package memsync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// consumerCleanupTimeout bounds deleting one of a hydration's consumers
// ([Syncer.deleteConsumer]), beside its request's crossing at the floor across
// a link. Five seconds is far past a healthy round trip, and bounded because
// this runs inside seat acquisition. Asked once: a lost deletion costs nothing,
// since the server reaps an idle consumer itself ([Syncer.reapAfter]).
const consumerCleanupTimeout = 5 * time.Second

// boundaryReadTimeout bounds the read of where a seat's own writes begin
// ([Syncer.recordBoundary]). It reads one row of this node's own store (node
// migration 0041's change sequence) and crosses no link, so a flat bound fits;
// moving it across a link needs a bound that follows the link's pace, as
// [Syncer.barrier]'s does. Bounded because it runs inside seat acquisition on a
// context with no deadline. Five seconds is the store's default lock wait
// (store.Options.BusyTimeout): a read still waiting then is a store in
// trouble, and the seat is refused until the next sweep.
const boundaryReadTimeout = 5 * time.Second

// exportPageBytes is how many bytes of carried values one read of a watermark
// table holds before publishing them, always at least one row ([exportPage]);
// a whole-each-cycle table is still read whole ([exportWhole]) until
// crewlet/crewlet#1040.
//
// Bytes, not rows: one row can hold up to the transport's 8 MiB, and a drain
// flushes every held seat at once (seat.Host.ReleaseAll), a page each: about
// 51 MiB at fifty seats. It bounds memory, not round trips: each row is its
// own publish and moves the mark as it is acknowledged. A mebibyte is about a
// hundred rows of 1 536-dimension vectors, so a page's one index seek is under
// a percent of what it feeds; less pays a seek every few rows, more only
// raises the drain's peak. A judgement, not a measurement.
const exportPageBytes = 1 << 20

// AgentIDFor derives a seat's stable id from its handle (ADR-0013), or "" where
// there is none to derive. An empty id is no address on the changelog (the
// broker refuses an empty seat token), so [Syncer.PublishOwn],
// [Syncer.PublishBacklog] and [Syncer.Hydrate] do nothing for it and record no
// boundary. Injected because the derivation belongs to the organization model
// and this package needs only this one function of it.
type AgentIDFor func(handle string) string

// Syncer carries a seat's memory between the nodes that run it.
type Syncer struct {
	db      *store.DB
	js      jetstream.JetStream
	agentID AgentIDFor

	// marks is the per-seat, per-table, per-range watermark: the watermark
	// column's value (a change sequence never handed out twice, so a later row
	// is above it) of the range's last row the changelog acknowledged. OWN is
	// rows stamped above the holding's boundary ([seatHolding]),
	// [Syncer.PublishOwn]'s; BACKLOG those at or below it,
	// [Syncer.PublishBacklog]'s. Each has its own mark, moved past each
	// acknowledged row, so a cut publish keeps what it carried.
	//
	// In memory only, deliberately: no state to keep correct, at the cost of a
	// republish after a restart or a release ([Syncer.Forget]), which the
	// stream collapses onto the same subjects. A mark moves only under the
	// holding its call began in, so an ended holding skips no rows of the next.
	mu    sync.Mutex
	marks map[markKey]int64

	// holdings is this node's holding of each seat it may publish
	// ([seatHolding]): live once its hydration here completes
	// ([Syncer.recordBoundary]), releasing once its release begins
	// ([Syncer.Releasing]), gone once the release forgets it ([Syncer.Forget]).
	// A seat with none is published by nobody here. Under mu, in memory, as
	// marks are.
	holdings map[string]*seatHolding

	// beforePage, a test's seam unset in production, is called with a range and
	// a watermark table before each page's start is read: the window between
	// one page's last mark and the next page's read.
	beforePage func(of markRange, table string)

	// beforeBoundary, a test's seam unset in production, is called just before
	// a hydration reads its boundary ([Syncer.recordBoundary]).
	beforeBoundary func()

	// said, a test's seam unset in production, receives each line
	// [Syncer.info] logs.
	said func(msg string, args ...any)

	// timing is the provisioning timing of the broker this node reaches
	// ([Config.Clustered]): the cadences a hydration's requests are asked again
	// at.
	timing jsprovision.Timing

	// pace is how long a mebibyte takes to cross this node's link at the
	// promised floor ([objstore.MiBPace]), and 0 in a member's own process
	// ([Config.AcrossLink]): each hydration request's term for its bytes.
	pace time.Duration

	// window is what a replay pull carries at most across a link
	// ([pullWindow], [Syncer.pullBound]), and 0 in a member's own process.
	window int

	// link is the meter of the link this node's connection crosses
	// ([Config.Link]), and nil where it crosses none or nobody meters it.
	link Link

	// copies, a test's seam nil in production, is where a hydration's replays
	// make their consumers ([Syncer.replays]); nil means the changelog's own
	// ([changelogCopies]).
	copies copies

	// leftOut, a test's seam unset in production, receives what a hydration did
	// not carry each time its memory_rows_not_carried line says so
	// ([Syncer.settleNotCarried]).
	leftOut func(notCarried)
}

// New builds a syncer, or returns nil when this node cannot carry memory: a
// working-looking syncer with no broker would let a caller believe its seats'
// memory travelled.
//
// It takes a JetStream client, which decides the domain or account the broker
// is addressed in (internal/jsapi). Build it over the coordination connection,
// never the queue's (ADR-0001): a replay's ordered consumer would sit on the
// read loop every mailbox consumes through. It is the same on every topology
// because no answer but a pull's carries a row, and a pull is bounded by the
// connection's own limit ([Syncer.pullBytes]); the topology changes only each
// request's term and a pull's window across a link ([Config]). A link meter
// handed to a node in its broker's own process is refused.
func New(db *store.DB, js jetstream.JetStream, agentID AgentIDFor, cfg Config) (*Syncer, error) {
	if db == nil || js == nil || agentID == nil {
		return nil, nil
	}
	s := &Syncer{db: db, js: js, agentID: agentID, marks: map[markKey]int64{},
		holdings: map[string]*seatHolding{}, timing: jsprovision.Clustered(cfg.Clustered).Timing()}
	if cfg.AcrossLink {
		s.pace, s.window, s.link = objstore.MiBPace, pullWindow, cfg.Link
	} else if cfg.Link != nil {
		return nil, errors.New("memsync: a node whose connection reaches the memory changelog " +
			"in its own process crosses no link, so it has none to meter (Config.Link is set " +
			"without Config.AcrossLink)")
	}
	return s, nil
}

// Window is the most a replay pull carries across this node's link
// ([pullWindow]), or 0 where [Config.AcrossLink] said the connection crosses
// none and a pull carries the connection's own bound ([Syncer.pullBytes]). The
// engine's test of its choice, by broker kind, reads it here.
func (s *Syncer) Window() int { return s.window }

// info logs one of a hydration's lines at info, and hands it to the said seam
// where a case set one.
func (s *Syncer) info(ctx context.Context, msg string, args ...any) {
	log.InfoContext(ctx, msg, args...)
	if s.said != nil {
		s.said(msg, args...)
	}
}

// holding is who this node's holding of a seat lets publish its memory: the
// memory sync cycle while live, the release's flush while releasing, never both
// but for the one row a cycle was sending as the release began
// ([Syncer.stillHolds]), and neither before the hydration here completes or
// after the release forgets it. The changelog is last writer wins per row, so
// it is safe only with one publisher of a seat at a time (package doc, "How it
// is fixed").
type holding string

const (
	// live is a seat whose hydration here completed and whose release has
	// not begun: the memory sync cycle carries it ([Syncer.PublishOwn],
	// [Syncer.PublishBacklog]).
	live holding = "live"

	// releasing is a seat whose release has begun ([Syncer.Releasing]): the
	// release's flush carries it ([Syncer.FlushOwn], [Syncer.FlushBacklog]),
	// and a cycle partway through it stops at its next row.
	releasing holding = "releasing"
)

// seatHolding is one holding of a seat on this node, from the hydration that
// completed it to the release that forgot it, and where its writes begin.
// Compared by address: a publish carries a row or moves a mark only while the
// holding it began under is still the seat's, so no call goes on under the
// next holding or its boundary.
type seatHolding struct {
	// state is which caller the holding lets publish the seat, under
	// [Syncer.mu].
	state holding

	// boundary is where the holding's own writes begin: this node's change
	// sequence as the hydration completed ([Syncer.recordBoundary]). Fixed for
	// the holding's life.
	boundary int64
}

// ErrNotHeld is what a publish answers when this node's holding of the seat
// does not let that call carry it ([holding]): at once when the call begins,
// or at its next row when the holding changes under it. To a cycle it is
// nothing to report; a release flushes only a holding [Syncer.Releasing]
// turned, so to a flush it is a holding that ended under it.
// errors.Is-comparable.
var ErrNotHeld = errors.New("memsync: this node's holding of the seat does not let this " +
	"call publish it")

// markRange is one of the two ranges a seat's rows of a watermark table fall
// in about its boundary: each is carried by a call of its own and marked
// apart.
type markRange string

const (
	// ownRange is the rows stamped above the seat's boundary: what its
	// holding here has written, [Syncer.PublishOwn]'s.
	ownRange markRange = "own"

	// backlogRange is the rows stamped at or below it: what this node held
	// for the seat when its hydration here completed,
	// [Syncer.PublishBacklog]'s.
	backlogRange markRange = "backlog"
)

// markKey names one mark: a seat's, of one table, over one range. A struct
// rather than a joined string, so a seat's marks are found by its handle,
// never by a prefix match.
type markKey struct {
	handle string
	table  string
	of     markRange
}

// seat resolves a handle into both spellings the schema uses.
func (s *Syncer) seat(handle string) seatRef {
	return seatRef{Handle: handle, AgentID: s.agentID(handle)}
}

// Carried is what one publish of a seat got the changelog to acknowledge: its
// own pass's ([Syncer.PublishOwn]) or its backlog's ([Syncer.PublishBacklog]),
// which the caller tells apart by the call it made.
type Carried struct {
	// Rows is every row acknowledged: of an own pass, the whole-each-cycle
	// tables' included.
	Rows int

	// Marked is the rows among them that moved a mark of the call's range:
	// the rows no later publish of that range sends again, until a restart
	// or a release zeroes the marks. Of a backlog, every row it carried.
	Marked int

	// StoppedIn is the table a publish that did not reach its end stopped
	// in, and Whole says it is a whole-each-cycle table, which keeps nothing
	// of a cut publish: only an own pass carries one.
	StoppedIn string
	Whole     bool
}

// PublishOwn is the seat's OWN PASS, which the engine's memory sync cycle gives
// every held seat ahead of the turn their backlogs share (a release calls
// [Syncer.FlushOwn]): what this node's holding wrote, then the tables sent
// whole, so a cut likelier falls on those than on the seat's new rows
// ([ownPassOrder]). It answers [ErrNotHeld] and reads nothing unless the
// holding is live, and checks again before each row ([Syncer.stillHolds]). A
// watermark table goes a page at a time ([exportPage]) above the larger of the
// boundary and its own mark, read again each page since another call of the
// range may have moved it; the mark moves past each acknowledged row, so a cut
// publish keeps what it carried. A whole-each-cycle table keeps nothing of a
// cut one. It returns at the first error, with what was acknowledged.
func (s *Syncer) PublishOwn(ctx context.Context, handle string) (Carried, error) {
	return s.publish(ctx, handle, live, ownRange, ownPassOrder)
}

// PublishBacklog carries the seat's BACKLOG, its rows at or below the boundary
// (what this node held as its hydration here completed; package doc, "How it
// is fixed"), and says what the changelog acknowledged. The memory sync cycle
// calls it after the held seats' own passes; a release calls
// [Syncer.FlushBacklog]. Live holdings only, checked as [Syncer.PublishOwn]'s
// are. Each watermark table ([backlogPassOrder]) goes a page at a time
// ([exportBacklogPage]) above its backlog mark, up to the boundary. A table
// read to the boundary, every row acknowledged, has its mark set there and is
// never read again: every row at or below it had committed when the boundary
// was read. That move counts nothing in [Carried.Marked]. It returns at the
// first error, with what was acknowledged.
func (s *Syncer) PublishBacklog(ctx context.Context, handle string) (Carried, error) {
	return s.publish(ctx, handle, live, backlogRange, backlogPassOrder)
}

// Releasing turns the seat's live holding here to releasing, and says whether
// there was one: from now on its memory is the release's flush's to carry
// ([Syncer.FlushOwn], [Syncer.FlushBacklog]), and a cycle stops at its next
// row. The release flushes only on true, so a seat whose hydration here failed
// or never ran flushes nothing, rather than put this node's possibly older
// profiles, skills and onboarding marker over the changelog's. A taking that
// fails after its hydration completed is live, and flushes back the
// changelog's own copies, which puts nothing older over them.
//
// The answer, not the flush's [ErrNotHeld], decides the flush: a release that
// forgot to turn the holding would otherwise lose the seat's last writes
// without a line.
func (s *Syncer) Releasing(handle string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.holdings[handle]
	if held == nil || held.state != live {
		return false
	}
	held.state = releasing
	return true
}

// FlushOwn is [Syncer.PublishOwn] for the release's last turn, from the marks
// the cycles left, carried only while the holding is releasing
// ([Syncer.Releasing]); otherwise it answers [ErrNotHeld] and reads nothing.
func (s *Syncer) FlushOwn(ctx context.Context, handle string) (Carried, error) {
	return s.publish(ctx, handle, releasing, ownRange, ownPassOrder)
}

// FlushBacklog is [Syncer.PublishBacklog] for the release's last turn, after
// [Syncer.FlushOwn]: the same backlog, carried only while the seat's holding
// here is releasing.
func (s *Syncer) FlushBacklog(ctx context.Context, handle string) (Carried, error) {
	return s.publish(ctx, handle, releasing, backlogRange, backlogPassOrder)
}

// publish carries one range of a seat's memory, the tables in order, under the
// seat's holding in state under, and says what the changelog acknowledged of
// it: the body of [Syncer.PublishOwn], [Syncer.PublishBacklog],
// [Syncer.FlushOwn] and [Syncer.FlushBacklog].
func (s *Syncer) publish(ctx context.Context, handle string, under holding, of markRange,
	order []table) (Carried, error) {

	var carried Carried
	if s == nil {
		return carried, nil
	}
	ref := s.seat(handle)
	if ref.AgentID == "" {
		return carried, nil
	}
	// The holding is read once, with its boundary: every row is carried under
	// it or not at all, so one boundary divides all the call's rows.
	held := s.holdingIn(handle, under)
	if held == nil {
		return carried, ErrNotHeld
	}
	for _, spec := range order {
		var err error
		if spec.wholeEachCycle {
			err = s.publishWhole(ctx, handle, ref, spec, held, under, &carried)
		} else {
			err = s.publishPages(ctx, handle, ref, spec, held, under, of, &carried)
		}
		if err != nil {
			carried.StoppedIn, carried.Whole = spec.name, spec.wholeEachCycle
			return carried, err
		}
	}
	if carried.Rows > 0 {
		log.DebugContext(ctx, "memory_published", "seat", handle, "range", of,
			"under", under, "rows", carried.Rows, "marked", carried.Marked)
	}
	return carried, nil
}

// publishWhole carries every one of the seat's rows of a wholeEachCycle table,
// while held is the seat's holding in state under.
func (s *Syncer) publishWhole(ctx context.Context, handle string, ref seatRef, spec table,
	held *seatHolding, under holding, carried *Carried) error {

	rows, err := exportWhole(ctx, s.db.SQL(), spec, ref)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !s.stillHolds(handle, held, under) {
			return ErrNotHeld
		}
		if err := s.publishRow(ctx, handle, ref, spec, row); err != nil {
			return err
		}
		carried.Rows++
	}
	return nil
}

// publishPages carries a watermark table's rows in one range a page at a time,
// while held is the seat's holding in state under, moving the range's mark
// past each acknowledged row: own above the larger of mark and boundary,
// backlog above the mark up to the boundary, where reaching it ends the range.
func (s *Syncer) publishPages(ctx context.Context, handle string, ref seatRef, spec table,
	held *seatHolding, under holding, of markRange, carried *Carried) error {

	boundary := held.boundary
	if of == backlogRange && s.mark(handle, spec.name, of) >= boundary {
		// The backlog is carried to the boundary already, or there is none.
		return nil
	}
	for {
		if s.beforePage != nil {
			s.beforePage(of, spec.name)
		}
		// The mark, read again each page rather than a cursor of the call's
		// own, so it also skips what another call of the range carried.
		after := s.mark(handle, spec.name, of)
		var page []exported
		var more bool
		var err error
		if of == ownRange {
			page, more, err = exportPage(ctx, s.db.SQL(), spec, ref, max(after, boundary))
		} else {
			page, more, err = exportBacklogPage(ctx, s.db.SQL(), spec, ref, after, boundary)
		}
		if err != nil {
			return err
		}
		for _, row := range page {
			if !s.stillHolds(handle, held, under) {
				return ErrNotHeld
			}
			if err := s.publishRow(ctx, handle, ref, spec, row.row); err != nil {
				return err
			}
			// After the acknowledgement, never before: a mark moved first
			// would skip, for the process's life, the rows whose publish
			// failed.
			s.setMark(handle, spec.name, of, row.mark, held)
			carried.Rows++
			carried.Marked++
		}
		if !more {
			if of == backlogRange {
				// Read to the boundary, all acknowledged: every row at or
				// below it had committed when it was read, so the range
				// holds nothing more.
				s.setMark(handle, spec.name, of, boundary, held)
			}
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

// Hydrate brings a seat's memory into this node's store, in seat acquisition
// before the mailbox attaches, and says how many rows its committed pulls
// wrote, a failed hydration's included. A failure refuses the seat: a peer may
// have the memory, and a seat run with amnesia contradicts its own history.
// Tables replay one at a time in the registry's order, each to the seat's
// barrier and then an end barrier, with no deadline but its progress; the
// barriers are its only writes (package doc, "How it is fixed"). A lost lease,
// a drain or a stop cuts it through ctx (docs/concepts/seat-ownership.md,
// "What waits for a long hydration").
//
// It forgets the seat as it begins ([Syncer.Forget]), and as it completes
// records the boundary and begins the live holding ([Syncer.recordBoundary]).
func (s *Syncer) Hydrate(ctx context.Context, handle string) (int, error) {
	if s == nil {
		return 0, nil
	}
	// Nothing here publishes the seat until this hydration completes, and no
	// mark of an earlier holding skips rows of this one's backlog.
	s.Forget(handle)
	ref := s.seat(handle)
	if ref.AgentID == "" {
		// No address on the changelog ([AgentIDFor]): nothing was published,
		// and the broker refuses a replay over an empty seat token.
		return 0, nil
	}
	run := &hydrationRun{handle: handle, start: time.Now()}
	run.slowAt = run.start.Add(objstore.Stall)

	// Contract 1 first, before a row is read: a recreated changelog is EMPTY,
	// and would hydrate every seat to nothing while reporting success.
	if err := s.checkIdentity(ctx); err != nil {
		return 0, err
	}
	// The barrier, after the identity and before any table: the sequence
	// every table's replay reads to.
	barrier, err := s.barrier(ctx, ref.AgentID, handle+"'s barrier", jsapi.Sending(0, s.pace))
	if err != nil {
		return 0, err
	}

	carried := 0
	// Rows left out for want of a parent ([table.parent]), by table, rather
	// than refusing the seat: a skill version whose skill the changelog does
	// not hold, or one this store skipped as a same-named duplicate. Said once
	// every table is replayed, never written.
	orphaned := map[string]int{}
	for _, spec := range tables {
		wrote, err := s.replay(ctx, run, handle, ref, spec, barrier, orphaned)
		carried += wrote
		if err != nil {
			return carried, err
		}
	}

	// Again after the last table: a recreation after the barrier loses it and
	// the replay waiting for it refuses the seat, but the barrier cannot see
	// one between the first reading and the barrier (which then holds the
	// barrier alone), or one between two tables once its sequences and another
	// hydration's barrier have run past this one's. Either would report success
	// with the rows read before it: the amnesia contract 1 refuses. A
	// recreation after this reading took nothing from the replays.
	if err := s.checkIdentity(ctx); err != nil {
		return carried, err
	}
	// The boundary, once the last step that can fail the hydration has
	// answered and before the steps that fail nothing ([Syncer.recordBoundary]).
	if err := s.recordBoundary(ctx, handle); err != nil {
		return carried, err
	}
	s.settleNotCarried(ctx, handle, ref.AgentID, orphaned)
	if carried > 0 {
		s.info(ctx, "memory_hydrated", "seat", handle, "rows", carried, "took", time.Since(run.start),
			"pulls", run.pulls, "placements", run.placements)
	}
	return carried, nil
}

// settleNotCarried says in one memory_rows_not_carried line what a hydration
// did not carry: rows left out for want of a parent, by table, and the retired
// tables the changelog still held the seat's rows of, which it purges for
// every node (purged=false where one failed). A failed count or purge is said
// (memory_retired_rows_not_purged) and tried again by the next hydration.
//
// A retired table is counted, never replayed: its rows are ones nobody bounded,
// and the reply that creates a consumer counts them without delivering one
// ([Syncer.held]). Not to the barrier: a copy behind costs nothing, since only
// a build that still carried the table publishes its rows, and the next
// hydration purges what this one missed. A table only a newer build carries is
// not read at all, and its rows stay on the changelog for that build.
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

// held is how many rows the changelog holds on filter's subjects, as counted by
// the reply that creates a consumer over them, with no row delivered. Made as a
// replay's consumer is ([Syncer.createConsumer]), deleted as the count ends.
func (s *Syncer) held(ctx context.Context, filter string) (int, error) {
	name := nuid.Next()
	consumer, err := s.createConsumer(ctx, jetstream.ConsumerConfig{
		Name:              name,
		Description:       "memsync: a count of " + filter,
		FilterSubject:     filter,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		AckPolicy:         jetstream.AckNonePolicy,
		InactiveThreshold: s.reapAfter(),
	})
	if err != nil {
		return 0, fmt.Errorf("memsync: open a count of %s: %w", filter, err)
	}
	defer s.deleteConsumer(ctx, name)
	return int(consumer.CachedInfo().NumPending), nil
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

// markOf names the mark of a seat's table over one range.
func markOf(handle, table string, of markRange) markKey {
	return markKey{handle: handle, table: table, of: of}
}

// mark reads a watermark.
func (s *Syncer) mark(handle, table string, of markRange) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marks[markOf(handle, table, of)]
}

// setMark advances a watermark, never backwards, and only while held is still
// the seat's holding, in either state: a row a cycle had acknowledged as the
// release began is carried, so the flush may skip it.
func (s *Syncer) setMark(handle, table string, of markRange, high int64, held *seatHolding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holdings[handle] != held {
		return
	}
	key := markOf(handle, table, of)
	if high > s.marks[key] {
		s.marks[key] = high
	}
}

// holdingIn is the seat's holding here when it is in state under, and nil
// otherwise: none, or one in the other state.
func (s *Syncer) holdingIn(handle string, under holding) *seatHolding {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held := s.holdings[handle]; held != nil && held.state == under {
		return held
	}
	return nil
}

// stillHolds says held is still the seat's holding here, in state under: asked
// before each row a publish carries.
//
// Asked before the send and not held across it, so the one row in flight as
// the holding changes can still land after a release's flush carried a newer
// copy of it. Closing that would hold the lock across a broker round trip, on
// which every other seat's publish and the release itself would wait.
func (s *Syncer) stillHolds(handle string, held *seatHolding, under holding) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holdings[handle] == held && held.state == under
}

// recordBoundary reads where this node's change sequence stands (node
// migration 0041) and, under one lock, begins the seat's live holding with it
// as the boundary, so the cycle carries the seat from then and never before.
// Read once the replays have committed and the identity's re-reading (which
// writes no memory row) has answered: the store is single-writer, so every row
// the seat has here is stamped at or below it and every later write above. Read
// as the hydration began, it would count every row the hydration inserted as
// the seat's own, sent ahead of what it writes. A failed read refuses the seat.
// It is no stream sequence and is never compared with a barrier's. On the
// hydration's own context, bounded by [boundaryReadTimeout], and read before the
// retired tables' count and purge, which fail nothing and so spend none of its
// time.
func (s *Syncer) recordBoundary(ctx context.Context, handle string) error {
	if s.beforeBoundary != nil {
		s.beforeBoundary()
	}
	readCtx, stop := context.WithTimeout(ctx, boundaryReadTimeout)
	defer stop()
	var last int64
	if err := s.db.SQL().QueryRowContext(readCtx,
		"SELECT last FROM memory_change_sequence WHERE singleton = 1").Scan(&last); err != nil {
		return fmt.Errorf("memsync: read where %s's own writes begin on this node: %w", handle, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdings[handle] = &seatHolding{state: live, boundary: last}
	return nil
}

// Forget ends a seat's holding here and drops its marks of both ranges and its
// boundary. Called by the release once its flush has returned, and by a
// hydration as it begins, so a holding a release never forgot leaves nothing.
// The next holding's new boundary and zeroed marks make its backlog everything
// this node holds for the seat, carried back after what it writes (package doc,
// "How it is fixed"); old marks would not skip what another node wrote
// meanwhile anyway, since a hydration stamps every row it inserts above them. A
// publish in flight stops at its next row, and the row it was sending moves no
// mark.
func (s *Syncer) Forget(handle string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.marks {
		if key.handle == handle {
			delete(s.marks, key)
		}
	}
	delete(s.holdings, handle)
}

// The two contracts this package declares.
//
// ReplayCompacted: the changelog keeps one message per subject, so a replay
// from the start is the current picture, not a history. The ephemeral
// consumer, the idempotent upsert and the deletes that do not travel all
// follow from it.
//
// Stream identity ([statelog.IdentityOf]): a compacted stream has no
// checkpoint or generation to answer a recreation with, so the only honest
// answer is to refuse the seat rather than admit it with an empty memory.

// ReplayProtocol is the contract this package's stream is read under, declared
// so a change to the changelog's retention has something to contradict.
func (s *Syncer) ReplayProtocol() statelog.ReplayProtocol { return statelog.ReplayCompacted }

// checkIdentity refuses a hydration from a changelog that is not the one this
// node last saw. Nothing can be repaired: a recreated changelog is empty and
// the rows went with the old stream, so the choice is between admitting the
// seat with no memory, which nothing anywhere would say, and refusing it. An
// unreadable identity refuses too, since unknown is not same; it costs a
// delayed seat on a broker having a bad minute, which the caller retries.
func (s *Syncer) checkIdentity(ctx context.Context) error {
	// Asked as a metadata read is ([jsprovision.ReadWith]): only the leader
	// answers, and a member still believing in a lost leader drops the request
	// silently, where asking again reaches the new one. Every two seconds, for
	// the longest silence the server keeps ([jsprovision.LookupBudget]), each
	// given its reply's crossing at the link's floor. Each request gets its own
	// handle: they can be out together, and the client caches each reply on its
	// handle with no lock.
	timing := s.waits(jsapi.Crossing(apiBytes, s.pace))
	info, err := jsprovision.ReadWith(ctx, timing,
		func(ctx context.Context) (*jetstream.StreamInfo, error) {
			stream, err := s.js.Stream(ctx, topics.MemoryStream)
			if err != nil {
				return nil, err
			}
			return stream.CachedInfo(), nil
		}, nil)
	if err != nil && jsprovision.Unanswered(ctx, err) {
		return fmt.Errorf("memsync: read the memory changelog's identity: nobody answered it in %s: %w — "+
			"a seat is not admitted from a stream this node cannot identify, "+
			"because a recreated one is EMPTY and would hydrate every seat to "+
			"nothing while reporting success", s.heldFor(timing, timing.Lookup), err)
	}
	if err != nil {
		return fmt.Errorf("memsync: read the memory changelog's identity: %w — "+
			"a seat is not admitted from a stream this node cannot identify, "+
			"because a recreated one is EMPTY and would hydrate every seat to "+
			"nothing while reporting success", err)
	}
	// The instant stamped at the stream's creation, which a restored changelog
	// keeps and the broker's own instant does not ([statelog.CreatedAtKey]).
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
		// A fresh node or company: recorded, since refusing would make every
		// first boot an outage.
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
// subject ([barrierSubject]), which is no table's and takes no row's room
// (topics.MemoryBarrierToken), and returns the sequence the changelog
// acknowledged it at: the leader numbers in acknowledgement order, so every row
// acknowledged before lies below. A hydration publishes one before its first
// table, and each replay one more, its END BARRIER ([Syncer.replay]); what
// names which for a refusal ("eng's barrier"). Publishing twice is safe: the
// subject keeps one message, so a repeated request, or a racing hydration of
// the seat, leaves a later barrier, and a replay ends at any at or past its own.
// arrival, the publish's crossing ([jsapi.Sending]) plus an end barrier's pull
// ([Syncer.endBarrierArrival]), keeps a merely queued one from being re-asked.
func (s *Syncer) barrier(ctx context.Context, agentID, what string, arrival time.Duration) (uint64, error) {
	subject := barrierSubject(agentID)
	// Asked as a metadata read is ([jsprovision.ReadWith]): presumed lost after
	// the read term and arrival, asked again a pause later with every request
	// kept outstanding and the first acknowledgement taken, for the longest
	// silence the server keeps ([jsprovision.LookupBudget]) and arrival. Only
	// the leader takes a publish, and one that no longer leads drops it without
	// a reply (nats-server jetstream_cluster.go, processClusteredInboundMsg;
	// jetstream_batching.go, commitSingleMsg). The client retries only on `no
	// responders`, so [jetstream.ErrNoStreamResponse] is read as that silence.
	timing := s.waits(arrival)
	ack, err := jsprovision.ReadWith(ctx, timing,
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
		return 0, fmt.Errorf("memsync: publish %s to %s: %w", what, subject, err)
	}
	// Unanswered for the whole bound. A broker refusing this connection
	// publish permission sends no acknowledgement, only a permissions error on
	// the connection ([Syncer.refusedPublish]); a refusal it does answer, such
	// as a storage limit, was wrapped whole above.
	if refused := s.refusedPublish(subject); refused != nil {
		return 0, fmt.Errorf("memsync: publish %s to %s: the broker refused it (%w): a "+
			"hydration writes the seat's barrier before it reads a row, so this node's "+
			"connection needs publish permission on %s>, as the seat's memory does: %w",
			what, subject, refused, topics.MemoryPrefix, err)
	}
	return 0, fmt.Errorf("memsync: publish %s to %s: no member of the changelog acknowledged it "+
		"in the %s it is asked for: the changelog had no leader that long, or the broker refused "+
		"the publish without an answer, as it refuses a connection without publish permission "+
		"on %s> (jetstream_client_error says so): %w",
		what, subject, s.heldFor(timing, timing.Lookup), topics.MemoryPrefix, err)
}

// unacknowledged reports a publish nobody answered: the hedge's silence, or the
// ceiling's or the caller's deadline.
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
