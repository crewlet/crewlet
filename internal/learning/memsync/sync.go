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

// consumerCleanupTimeout bounds deleting one of a hydration's consumers: a
// table's replay, one of its placements, or a retired table's count
// ([Syncer.deleteConsumer]), beside the crossing of its request at the floor
// across a link.
//
// Five seconds against one API round trip: far past a healthy answer, and
// bounded because this runs inside seat acquisition. What it costs when it
// expires is nothing an operator sees — the server reaps an idle consumer on
// its own ([Syncer.reapAfter]), which is why the deletion is hygiene rather
// than a step whose failure means anything. One request, never asked again: a
// deletion lost is a consumer lingering until it is reaped.
const consumerCleanupTimeout = 5 * time.Second

// boundaryReadTimeout bounds the read of where a seat's own writes begin on
// this node: the boundary a hydration records as it completes
// ([Syncer.Hydrate], "Where this holding's writes begin").
//
// THE READ IS OF THIS NODE'S OWN STORE, and crosses no link: one row of its
// change sequence (node migration 0041), which answers in well under a
// millisecond, waiting only for one of the pool's connections or for the
// store's lock, which the store's own busy wait bounds (five seconds unless
// store.Options.BusyTimeout says otherwise). A flat bound is right for such a
// read alone. One that crossed a link to the broker would need a bound that
// follows the link's pace rather than a flat figure, as every request a
// hydration makes of the broker has ([Syncer.createConsumer],
// [Syncer.barrier], [Syncer.checkIdentity]), so a change that moves this read
// across a link replaces this bound.
//
// BOUNDED, because it runs inside seat acquisition, on the hydration's own
// context, which has no deadline of its own, and an unbounded wait there would
// hang the acquisition on a wedged store. FIVE SECONDS is the store's default
// lock wait and the bound a consumer's deletion has
// ([consumerCleanupTimeout]): a read still waiting then is a store in trouble,
// and the seat is refused until the next sweep, as on any failure of its
// store.
const boundaryReadTimeout = 5 * time.Second

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
// changelog, so neither half touches it: [Syncer.PublishOwn] and
// [Syncer.PublishBacklog] carry nothing for it rather than publish to a
// subject with an empty seat token, which the broker refuses, and
// [Syncer.Hydrate] reads nothing for it rather than open a replay over such a
// token, which the broker refuses too, and records no boundary for it.
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

	// marks is the per-seat, per-table, per-range watermark: the value of the
	// table's watermark column (table.watermark, a change sequence never
	// handed out twice, so a row written after the mark is always above it)
	// of the last row of the range the changelog acknowledged. A seat's rows
	// of a table fall in two ranges about its holding's boundary
	// ([seatHolding]): its OWN, stamped above it, which [Syncer.PublishOwn]
	// carries, and its BACKLOG, stamped at or below it, which
	// [Syncer.PublishBacklog] carries.
	// Each range has a mark of its own, so neither call's progress is read as
	// the other's. A publish moves its range's mark past each row as that row
	// is acknowledged, so a publish its bound cuts keeps what it carried, and
	// the seat's next one resumes at the first row of the range the changelog
	// did not acknowledge.
	//
	// In memory only, and deliberately: the benefit is a subsystem with no
	// state of its own to keep correct, and the cost is a republish. A
	// restart zeroes every mark and a release ([Syncer.Forget]) the seat's,
	// so the seat's next holding here carries again everything this node
	// holds for it, which the stream collapses onto the same subjects: a page
	// at a time, across as many cycles as the link needs, as its backlog,
	// after what it writes.
	//
	// A MARK MOVES ONLY UNDER THE HOLDING ITS CALL BEGAN IN (holdings,
	// below): a row the changelog acknowledges after the release has
	// forgotten the seat moves no mark, so nothing of a holding that has
	// ended is left to skip rows of the next one's backlog.
	mu    sync.Mutex
	marks map[markKey]int64

	// holdings is this node's HOLDING of each seat whose memory it may
	// publish ([seatHolding]): begun live as the seat's hydration here
	// completes ([Syncer.Hydrate], "What a hydration begins"), turned to
	// releasing as its release begins ([Syncer.Releasing]), and ended as the
	// release forgets it ([Syncer.Forget]). A seat with none, one still being
	// taken, one whose hydration failed, or one released, is published by
	// nobody here (crewlet/crewlet#511). Under mu beside the marks, in memory
	// for the same reason, and dropped with them by a restart and a release.
	holdings map[string]*seatHolding

	// beforePage, when set, is called with a range and a watermark table's
	// name before each of the table's pages of that range has its start
	// read: between one page's last mark and the next page's read, a window
	// no publish of a row reaches. A test's seam, unset in production,
	// through which a case acts in that window or watches what each page
	// took.
	beforePage func(of markRange, table string)

	// beforeBoundary, when set, is called just before a hydration reads where
	// the seat's own writes begin ([Syncer.recordBoundary]): a test's seam,
	// unset in production, through which a case cuts the hydration there.
	beforeBoundary func()

	// said, when set, is called with each line [Syncer.info] logs: a test's
	// seam, unset in production, through which a case reads what a
	// hydration's lines report.
	said func(msg string, args ...any)

	// timing is the provisioning timing of the broker this node reaches
	// ([Config.Clustered]): the cadences every request a hydration makes is
	// asked again at.
	timing jsprovision.Timing

	// pace is how long a mebibyte takes to cross this node's link at the
	// floor every link is promised ([objstore.MiBPace]), and 0 in a member's
	// own process ([Config.AcrossLink]): what every request a hydration makes
	// is given beside its term for its bytes to cross.
	pace time.Duration

	// window is what a replay pull carries at most across a link
	// ([Syncer.pullBound]), and 0 in a member's own process.
	window int

	// link is the meter of the link this node's connection crosses
	// ([Config.Link]), and nil where it crosses none or nobody meters it.
	link Link

	// copies, when set, is where a hydration's replays make their consumers
	// ([Syncer.replays]): a test's seam, nil in production, where they are
	// the changelog's own ([changelogCopies]).
	copies copies

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
// What the topology changes is how long each request is given and how much a
// pull carries across a link ([Config]). A node across a link that names no
// lease heartbeat is refused, by name.
func New(db *store.DB, js jetstream.JetStream, agentID AgentIDFor, cfg Config) (*Syncer, error) {
	if db == nil || js == nil || agentID == nil {
		return nil, nil
	}
	s := &Syncer{db: db, js: js, agentID: agentID, marks: map[markKey]int64{},
		holdings: map[string]*seatHolding{}, timing: jsprovision.Clustered(cfg.Clustered).Timing()}
	if cfg.AcrossLink {
		if cfg.LeaseHeartbeat <= 0 {
			return nil, fmt.Errorf("memsync: a node that reaches the memory changelog across a "+
				"link needs its lease heartbeat (Config.LeaseHeartbeat), which sizes the window "+
				"its replays' pulls share with its lease renewals; got %v", cfg.LeaseHeartbeat)
		}
		s.pace, s.window, s.link = objstore.MiBPace, windowFor(cfg.LeaseHeartbeat), cfg.Link
	} else if cfg.Link != nil {
		return nil, errors.New("memsync: a node whose connection reaches the memory changelog " +
			"in its own process crosses no link, so it has none to meter (Config.Link is set " +
			"without Config.AcrossLink)")
	}
	return s, nil
}

// info logs one of a hydration's lines at info, and hands it to the said seam
// where a case set one.
func (s *Syncer) info(ctx context.Context, msg string, args ...any) {
	log.InfoContext(ctx, msg, args...)
	if s.said != nil {
		s.said(msg, args...)
	}
}

// holding is what this node's holding of a seat lets publish the seat's
// memory: the memory sync cycle while the seat is held, or the release's flush
// once it is being given back, never both at once but for the one row a cycle
// was sending as the release began ([Syncer.stillHolds]), and neither before
// the seat's hydration here has completed or after its release has forgotten
// it.
//
// A seat's memory on the changelog is last writer wins, one message per row
// (codec.go, upsert), which is safe only while ONE publisher at a time carries
// the seat. Without a holding a node had three more (crewlet/crewlet#511): its
// cycle published a seat whose hydration was still running, every row of it
// as its own, over the newer rows the hydration had yet to read; a cycle that
// had begun before the seat's release went on publishing it beside the
// release's flush, or started on it after the release had forgotten it, and
// put older copies of its profiles, skills and onboarding marker over what
// the flush and the next holder wrote; and the release of a seat whose
// hydration failed flushed this node's own copies of them, which can be older
// than the changelog's.
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
// completed it to the release that forgot it, and where the writes it made
// begin.
//
// A POINTER in [Syncer.holdings], compared by its address: a publish keeps
// the holding it began under and carries a row, or moves a mark, only while
// that same holding is still the seat's, so no call that began under one
// holding goes on under the next, nor under its boundary.
type seatHolding struct {
	// state is which caller the holding lets publish the seat, under
	// [Syncer.mu].
	state holding

	// boundary is where the holding's own writes begin: the value of the
	// node's change sequence as the seat's hydration here completed
	// ([Syncer.Hydrate], "Where this holding's writes begin"). Fixed for
	// the holding's life.
	boundary int64
}

// ErrNotHeld is what a publish of a seat answers when this node's holding of
// the seat does not let that call carry it ([holding]): a cycle's call for a
// seat still being taken, being released, or released; a flush's call for a
// seat whose hydration failed or never ran, or whose holding is still live.
// Answered at once when the call begins without such a holding, and at the
// call's next row when the holding changes under it, after which nothing more
// of the seat is carried. To a cycle it is nothing to report: the seat is not,
// or no longer, the cycle's to publish. A release flushes only a holding
// [Syncer.Releasing] turned, so to a flush it is a holding that ended under
// it. errors.Is-comparable.
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

// markKey names one mark: a seat's, of one table, over one range.
//
// A STRUCT, rather than the handle and the table joined into one string, so a
// seat's marks are found by comparing its handle, and no key is taken apart or
// matched by a prefix to find them.
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

// PublishOwn carries onto the changelog what this node's holding of a seat has
// written, and then the tables sent whole, and says what the changelog
// acknowledged: the seat's OWN PASS, as the engine's memory sync cycle gives
// it to every seat this node holds, ahead of the turn the seats' backlogs
// share. A release's last turn calls [Syncer.FlushOwn] instead.
//
// ONLY WHILE THE SEAT'S HOLDING HERE IS LIVE ([holding]): from the moment its
// hydration completed until its release begins. A seat with no live holding,
// one still being taken, being released or released, is carried by no cycle's
// call here: the call answers [ErrNotHeld] at once and reads nothing. And the
// call checks the holding again before each row, so a release that begins
// while a cycle is partway through the seat stops that cycle at its next row:
// only the row it was publishing as the release began can still land beside
// the flush (crewlet/crewlet#511).
//
// A table with a watermark is carried above the larger of its own mark and the
// holding's boundary ([Syncer.Hydrate], "Where this holding's writes begin"), a
// page at a time ([exportPage]: at most exportPageBytes and one row in
// memory), and the own mark moves past each row the changelog acknowledges, so
// the steady-state cost is what the seat just learned, and a publish its
// context cuts keeps what it carried: the seat's next one resumes at the first
// row the changelog did not acknowledge. A wholeEachCycle table is carried
// whole by every own pass, and keeps nothing of a cut one.
//
// IN [ownPassOrder], the tables with a watermark before the tables sent whole,
// so what the holding wrote goes ahead of the seat's profiles, skills and
// onboarding marker, where a cut is likeliest to fall.
//
// EACH PAGE STARTS ABOVE THE LARGER of the seat's own mark and the boundary,
// the mark read again at each page: another call of the range can have moved
// it further, a release's flush beside the row a cycle was publishing as the
// release began, and this one then skips the rows that call carried.
//
// It returns at the first error, keeping in the marks what was acknowledged
// before it and saying so in what it returns.
func (s *Syncer) PublishOwn(ctx context.Context, handle string) (Carried, error) {
	return s.publish(ctx, handle, live, ownRange, ownPassOrder)
}

// PublishBacklog carries onto the changelog the seat's rows at or below its
// boundary, and says what the changelog acknowledged: the seat's BACKLOG,
// what this node held for it when its hydration here completed, which the
// hydration wrote from the changelog or found already present.
//
// Called by the engine's memory sync cycle, in a turn the held seats' backlogs
// share after their own passes ([Syncer.PublishOwn]); a release's last turn
// calls [Syncer.FlushBacklog] instead. It is what the changelog mostly holds
// already, the republish node migration 0041 keeps: it puts this node's rows
// back on a changelog an operator accepted after a recreation, or one
// restored while the node ran, and carries what an earlier holding's flush
// did not reach.
//
// ONLY WHILE THE SEAT'S HOLDING HERE IS LIVE, checked as the call begins and
// again before each row, as [Syncer.PublishOwn] is: a seat with no live
// holding answers [ErrNotHeld] and reads nothing.
//
// Each table with a watermark, in the registry's order ([backlogPassOrder]), is
// carried above its backlog mark and up to the boundary, a page at a time
// ([exportBacklogPage]), the backlog mark moving past each row the changelog
// acknowledges. A table READ TO THE BOUNDARY, with nothing past its last page
// and every row of the page acknowledged, has its backlog mark set to the
// boundary and is not read again: every row stamped at or below the boundary
// had committed when the boundary was read, so none can join the range after,
// and a drained seat costs no read a cycle. That move counts no row in
// [Carried.Marked]. The tables sent whole are not in it: the own pass sends
// them.
//
// It returns at the first error, keeping in the marks what was acknowledged
// before it and saying so in what it returns.
func (s *Syncer) PublishBacklog(ctx context.Context, handle string) (Carried, error) {
	return s.publish(ctx, handle, live, backlogRange, backlogPassOrder)
}

// Releasing turns the seat's live holding here to releasing, and says whether
// it had one to turn: the release of the seat has begun, and from now on its
// memory is the release's flush's to carry ([Syncer.FlushOwn],
// [Syncer.FlushBacklog]), never the cycle's. A cycle partway through the seat
// stops at its next row ([ErrNotHeld]), and one that reaches it later carries
// nothing of it.
//
// Called by the engine's release of the seat, before its flush, which it runs
// only on true. A seat with no live holding is left as it is, and answers
// false: one whose hydration here failed or never ran has none, so its release
// flushes nothing, and never puts this node's own copies of its profiles,
// skills and onboarding marker, which can be older than the changelog's, over
// them (crewlet/crewlet#511). A taking that fails AFTER its hydration
// completed has a live holding, and its release flushes what the hydration
// just wrote, the changelog's own copies of those tables, which puts nothing
// older over them.
//
// THE ANSWER IS WHAT DECIDES THE FLUSH, rather than the flush's own
// [ErrNotHeld]: a release that forgot to turn the holding would otherwise
// flush nothing, and a flush that is silent on ErrNotHeld would say nothing of
// it, so every release would lose the seat's last writes without a line.
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

// FlushOwn is [Syncer.PublishOwn] for the release's last turn: the same own
// pass, from the marks the cycles left, carried only while the seat's holding
// here is RELEASING ([Syncer.Releasing]). A seat with none, whose hydration
// here never completed, or one whose holding is still live, answers
// [ErrNotHeld] and reads nothing.
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
	// THE HOLDING ONCE, AS THE CALL BEGINS, and its boundary with it: every
	// row after is carried under that same holding or not at all, so one
	// boundary divides the call's rows from its first page to its last.
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

// publishPages carries the seat's rows of a watermark table in one range about
// held's boundary, a page at a time, while held is the seat's holding in state
// under, moving that range's mark past each row the changelog acknowledges:
// the own range above the larger of its mark and the boundary, and the backlog
// above its mark and up to the boundary, where a read that reaches it ends the
// range for good.
func (s *Syncer) publishPages(ctx context.Context, handle string, ref seatRef, spec table,
	held *seatHolding, under holding, of markRange, carried *Carried) error {

	boundary := held.boundary
	if of == backlogRange && s.mark(handle, spec.name, of) >= boundary {
		// THE BACKLOG IS CARRIED TO THE BOUNDARY ALREADY, or the seat has
		// none: nothing at or below it is left above the mark, and nothing
		// is read.
		return nil
	}
	for {
		if s.beforePage != nil {
			s.beforePage(of, spec.name)
		}
		// THE MARK, READ AGAIN AT EACH PAGE, never a cursor of the call's
		// own: it moves only forward while held is the seat's, which it is
		// for as long as this call carries a row, and past whatever another
		// call of the range carried.
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
			// MOVED AFTER EACH ACKNOWLEDGED ROW, never before it. A
			// watermark moved first would skip, for the life of the
			// process, exactly the rows whose publish failed.
			s.setMark(handle, spec.name, of, row.mark, held)
			carried.Rows++
			carried.Marked++
		}
		if !more {
			if of == backlogRange {
				// READ TO THE BOUNDARY, every row of it acknowledged: every
				// row stamped at or below it had committed when it was
				// read, so the range holds nothing more, and the next
				// backlog of the seat reads none of it.
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
// # Each table read to the seat's barrier, then to an end barrier
//
// A replay cannot end where the copy serving it says it holds nothing more:
// that copy can be behind the changelog's leader ([Syncer.replay]). So after
// it has read the changelog's identity, a hydration publishes a BARRIER to the
// seat's barrier subject ([Syncer.barrier]) and keeps the sequence the
// changelog acknowledged it at. The leader numbers the stream in the order it
// acknowledges, so every row the changelog had acknowledged before — every row
// the seat's last owner flushed before it let the seat go — lies below that
// sequence, and every table's replay reads until it has delivered a barrier at
// or past it. At or past, because a hydration of the seat racing this one
// publishes its own barrier, which replaces this one's, and both replays end
// at it ([Syncer.barrier]). Then the replay publishes the seat's barrier
// again, its END BARRIER, and reads on until it has delivered a barrier at or
// past that one: a row replaced before the replay reached it lies below the
// end barrier on the changelog's order, so it is read, whatever the copy's
// count of what follows said ([Syncer.replay], replay.go's head). Seven
// barriers a hydration, on the seat's one subject, which keeps the last.
//
// # Bounded by its progress, never by a total
//
// A hydration has no deadline of its own: it takes what its bytes take, and
// time refuses it only where nothing arrives for a pull's stall, or where no
// copy a replay is placed on delivers anything for a minute (replay.go's
// head); the answers that refuse a seat whatever the time refuse it too. Every request it makes keeps its own bound. What waits for a long
// hydration is the seat host's placement pass that runs it (docs/concepts/
// seat-ownership.md, "What waits for a long hydration"); its lease is renewed
// throughout, and a lost lease, a drain or a stop cuts it, through ctx.
//
// # A hydration writes
//
// The barriers are WRITES, the only ones a hydration makes, the first before
// it reads a row, so a broker that refuses this node's writes refuses its
// hydrations, as it already refuses the seat's mailbox and its memory's
// publishes. An account or a server at its storage limit answers the publish
// with its refusal, which refuses the seat wrapped whole. A user without
// publish permission on the memory subjects (crewlet.memory.>) is refused
// with no acknowledgement at all, only an error the broker sends the
// connection, and the hydration names that error once the publish has gone
// unanswered for as long as it is asked ([Syncer.barrier]). A barrier is no
// row and takes none away: its subject is no table's, and nothing on the
// changelog makes room for a message but the stream's one message per subject
// (topics.MemoryBarrierToken).
//
// # Where this holding's writes begin
//
// As it completes, a hydration records the seat's BOUNDARY: where this node's
// change sequence stands (node migration 0041's counter), read once the
// changelog's identity has been read again after the last table, the last step
// that can fail the hydration. Every row the replays wrote has committed by
// then, each insert stamped by the store as it was written and each vector it
// took over a stored one stamped again, and the identity's reading writes no
// memory row, so every row of the seat in this store when the hydration
// completes, the ones it found already present included, is stamped at or
// below the boundary, and every row written after it is stamped above: the
// store is single-writer, so the value read is at least every committed stamp,
// and a transaction still open commits its stamps above it. Rows above the
// boundary are the seat's OWN, what its holding here writes, which
// [Syncer.PublishOwn] carries first; rows at or below it are its BACKLOG,
// [Syncer.PublishBacklog]'s, which is what an acquisition or a restart leaves
// this node to carry back. Read as the hydration began instead, the boundary
// would count every row the hydration inserted as the seat's own and send them
// ahead of what it writes, which is what a seat's new memory waited behind
// (crewlet/crewlet#1039).
//
// ON THE HYDRATION'S OWN CONTEXT, bounded by [boundaryReadTimeout]: a
// hydration has no deadline of its own, so the read is held only to its own
// bound, and a cut of the seat's taking (a lost lease, a drain, a stop)
// refuses it, as it should. Read BEFORE the count and purge of the retired
// tables, which fail no hydration, so their round trips neither delay the read
// nor spend its time. A read the store fails refuses the seat: a store that
// cannot answer one row of its own just after it committed the replays is
// failing, and the seat waits for the next sweep, as on every other failure
// here. A hydration that fails records no boundary, and neither does one of a
// seat with no address on the changelog.
//
// THE BOUNDARY IS NO STREAM SEQUENCE, and nothing compares it with the
// barrier's: the barrier's sequence is where the CHANGELOG stood, as its
// leader acknowledged the barrier, and the boundary is where this NODE's own
// change sequence stood.
//
// # What a hydration begins
//
// The seat's HOLDING here ([holding]), live, recorded with the boundary and
// under the same lock, so the memory sync cycle carries the seat from the
// moment its hydration has completed, and never before: a cycle that reaches
// the seat while it is still being taken carries nothing of it
// ([ErrNotHeld]), where it carried every row this node held for it, as its
// own, over the newer rows the hydration had yet to read (crewlet/crewlet#511).
// A hydration that fails begins none, so the release that gives the seat back
// flushes nothing ([Syncer.Releasing]). And a hydration FORGETS the seat as it
// begins ([Syncer.Forget]), which a release has done first anyway: it ends any
// holding of the seat and drops its marks, so a seat being taken is not
// published, whatever this node held of it before, and no mark of an earlier
// holding skips rows of this one's backlog.
//
// # What it reports
//
// The rows its pulls' transactions committed, a failed hydration's included:
// a pull's rows are counted once its transaction commits, so a hydration that
// fails reports what it left in the store and nothing it rolled back. What
// refuses the seat is the error alone. Its memory_hydrated line says how long
// it took, the pulls it asked and the consumers it made; one that runs past a
// minute says so once a minute while it runs (memory_hydration_slow), and a
// replay placed again says why (memory_replay_placed_again).
func (s *Syncer) Hydrate(ctx context.Context, handle string) (int, error) {
	if s == nil {
		return 0, nil
	}
	// THE SEAT IS BEING TAKEN, so nothing here publishes it until this
	// hydration completes, and nothing of a holding before this one is left
	// to skip rows of this one's backlog ("What a hydration begins").
	s.Forget(handle)
	ref := s.seat(handle)
	if ref.AgentID == "" {
		// NO ADDRESS ON THE CHANGELOG ([AgentIDFor]): nothing was published
		// for the seat, and a replay over an empty seat token is a consumer
		// the broker refuses to make.
		return 0, nil
	}
	run := &hydrationRun{handle: handle, start: time.Now()}
	run.slowAt = run.start.Add(objstore.Stall)

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
	barrier, err := s.barrier(ctx, ref.AgentID, handle+"'s barrier", jsapi.Sending(0, s.pace))
	if err != nil {
		return 0, err
	}

	carried := 0
	// The rows a replay found no parent for, by table: said once every
	// table is replayed, and never written.
	orphaned := map[string]int{}
	for _, spec := range tables {
		wrote, err := s.replay(ctx, run, handle, ref, spec, barrier, orphaned)
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
	// WHERE THIS HOLDING'S OWN WRITES BEGIN, once the last step that can fail
	// the hydration has answered, and before the steps that fail nothing
	// ("Where this holding's writes begin").
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
// consumer says that, with no row delivered and no request of its own. Made
// as a replay's consumer is ([Syncer.createConsumer]), and deleted as the
// count ends ([Syncer.deleteConsumer]).
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
// the seat's holding ([Syncer.marks]): whatever its state, since a row a cycle
// had acknowledged as the release began is carried, and the flush may skip it.
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

// stillHolds says held is still the seat's holding here, in state under: what
// a publish asks before each row it carries.
//
// ASKED BEFORE THE ROW IS SENT, and not held across the send, so the one row
// a call was publishing as the holding changed can still land after the
// change: a release's flush can then have carried the same row before it, and
// the cycle's copy, read before the flush's, replace it. Any row can be that
// one: a profile, skill or onboarding marker rewritten since the cycle read
// it, or a diary note or episode whose vector was filled since
// (crewlet/crewlet#511). Closing that row too would hold the lock across a
// broker round trip, on which every other seat's publish and the release
// itself would wait.
func (s *Syncer) stillHolds(handle string, held *seatHolding, under holding) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holdings[handle] == held && held.state == under
}

// recordBoundary reads where this node's change sequence stands and begins the
// seat's live holding with it as the boundary ([Syncer.Hydrate], "Where this
// holding's writes begin" and "What a hydration begins"): on the hydration's
// own context, bounded by [boundaryReadTimeout].
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

// Forget ends a released seat's holding here, and drops its marks, of both its
// ranges, and its boundary with the holding. Called by the release once its
// flush has returned, and by a hydration as it begins ([Syncer.Hydrate],
// "What a hydration begins"), so a holding a release never forgot leaves
// nothing behind either.
//
// The seat's next holding here takes a new boundary as its hydration
// completes, and both of its ranges start from 0: its backlog is then
// everything this node holds for the seat, which it carries back after what
// it writes, as after a restart. That republish is what puts this node's rows
// back on a changelog an operator accepted after a recreation, or one
// restored while the node ran. (Old marks would not skip what another node
// wrote meanwhile: a hydration stamps every row it inserts above any of them,
// node migration 0041.) Until that hydration completes nothing here publishes
// the seat ([ErrNotHeld]).
//
// A publish of the seat still in flight stops at its next row, and the row it
// was sending moves no mark once it is acknowledged, so a cycle that took its
// snapshot of the held seats before the release carries nothing of the seat
// after it, and leaves nothing of this holding behind to skip rows of the
// next one's (crewlet/crewlet#511).
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
	// ASKED AS A METADATA READ IS ([jsprovision.ReadWith]): only the
	// stream's leader answers its info, and a member that still believes in
	// a leader its group has just lost drops the request without a word, so
	// one request waited out the hydration's whole bound and refused the
	// seat over a reply nobody would send — where asked again, the new
	// leader answers. Every two seconds, under the longest silence the
	// server keeps ([jsprovision.LookupBudget]), each given the crossing of
	// its reply at the floor across a link: a hydration has no deadline of
	// its own to hold it to less. A handle of each request's own, because
	// the requests can be out together and the client caches each reply on
	// its handle with no lock.
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
// it. A hydration publishes one before its first table, which every table's
// replay reads to, and each replay one more, its END BARRIER, once it has
// received a barrier at or past the first, which it reads to in turn
// ([Syncer.replay]). what names which, for a refusal: "eng's barrier", "the
// end barrier of eng's agent_diary".
//
// # Asked again while nobody answers it
//
// As a metadata read is ([jsprovision.ReadWith]): presumed lost after the read
// term and arrival, and asked again a pause later, every request kept
// outstanding and the first acknowledgement taken, for no longer than the
// longest silence the server keeps ([jsprovision.LookupBudget]) and arrival.
// arrival is how long the acknowledgement takes to reach this node behind what
// it waits behind: the publish's own crossing at the floor across a link
// ([jsapi.Sending]), and for an end barrier the rest of the pull its replay
// has out, which the acknowledgement queues behind on the same connection
// ([Syncer.endBarrierArrival]) — so an end barrier is presumed lost after
// about 9.0 seconds in a member's own process at the transport's bound and
// asked again at 10, and across a link at the window after about 16.5 seconds
// and asked again at 17.5, rather than asked again behind the bytes the first
// is waiting on; the identity reads and the hydration's first barrier, which
// wait behind no pull, are asked every two seconds. Only the stream's leader takes a publish — no other member
// subscribes to the stream's subjects (nats-server stream.go, setLeader) — and
// a member that still believes it leads after its group has moved on drops
// one without a reply: one it queued while it led and takes once it no longer
// does (jetstream_cluster.go, processClusteredInboundMsg), and one whose
// proposal its term outlived (jetstream_batching.go, commitSingleMsg). Sent
// once, such a publish waited out the whole bound and refused the seat until
// the next sweep. The client's own retry is no help there: it asks again only
// on `no responders`, the moment no member subscribes at all, twice a quarter
// of a second apart, and then gives up with [jetstream.ErrNoStreamResponse] —
// which is read here as the silence it is, so the hedge asks again after its
// pause rather than refusing the seat.
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
// which the replay reads past, as it reads past an earlier hydration's.
//
// # Two hydrations of the seat at once
//
// For the same reason. A seat can be hydrated by two nodes at once while it
// moves, in crewlet/crewlet#511's window: a node still taking a seat whose
// claim has lapsed under it, and the node that has claimed it since. Each
// publishes its barriers to the seat's one subject, so the later replaces the
// earlier, and the earlier hydration's replays deliver the later one instead
// of their own: it lies past their own, so they end at it, as the later
// hydration's do. However many hydrations of a seat there have been, the
// changelog holds one barrier of it.
//
// # When nobody acknowledges it
//
// A hydration refused for a barrier names why. An answer is the broker's own,
// wrapped whole: an account or a server at its storage limit refuses the
// publish in its acknowledgement. A broker that refuses this node's connection
// publish permission on the memory subjects answers the publish with nothing
// at all: it sends the connection a permissions error instead, which the
// client hands to its error handler (jetstream_client_error) and keeps as the
// connection's last error, and the publish waits for an acknowledgement that
// never comes. So a barrier still unacknowledged when it has been asked for as
// long as it is asked is refused with that error where the connection's last
// one names this subject, and otherwise with what can leave a publish
// unanswered that long.
func (s *Syncer) barrier(ctx context.Context, agentID, what string, arrival time.Duration) (uint64, error) {
	subject := barrierSubject(agentID)
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

// unacknowledged reports a publish nobody answered: the silence the hedge
// heard, or a deadline: the ceiling's, or the caller's own.
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
