package engine

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// WHETHER A CAPACITY WINDOW IS OPEN, read the one way every reader reads it.
//
// # Three answers, never two
//
// A log has a window open, has none, or this node couldn't read which. The
// third is not the second: a window stops every publisher in the fleet, and
// the moment somebody opens the retention screen to ask why the company has
// gone quiet is very often the coordination outage that stops the read. The
// report and the alarm used to skip a log whose window they couldn't read, so
// during that outage `crewlet retention status`, the dashboard's banner and
// `maintenance_open` all said there was no window, while a node's boot, which
// reads the same register, refused to start because there might be one.
//
// So there is ONE READ ([readWindows]) over ONE SET of logs (every log a
// window can be opened on, [maintenanceStreams]), and every reader takes its
// answer whole: the gestures' `open_windows` and `unread_windows`
// ([Engine.CapacityWindows]), the report's `maintenance` and
// `maintenance_unread`, and the alarm reading. Two readers walking two sets of
// logs, each deciding for itself what an error meant, is how they came to
// disagree.
//
// # What the alarm does with "couldn't read"
//
// It keeps what it last knew ([windowWatch]). A coordination blip that fails
// one read must neither clear a `maintenance_open` that is firing nor raise one,
// which is the rule the alarm table's other coordination inputs keep: a window
// this node last read open stays open for the alarm AS OLD AS IT WAS WHEN IT WAS
// READ, its detail saying when that was and that the window can't be read now,
// and a log it never read open raises nothing. Aged on this node's clock
// instead, a window last read open at 59 minutes crossed the hour during a
// two-minute blip and raised the alarm about a window that might have closed in
// the meantime. The screens say "couldn't read" outright, since they report
// this read rather than judge a condition over time.
//
// # Which read is newer
//
// The watch is fed by every report, and a report is assembled for the alarm
// heartbeat and for every API caller at once, so two reads of one log can reach
// it in the other order from the one they were taken in. Each log's read is
// numbered before it is sent ([windowReads]), and the watch keeps the answer of
// the newest read that answered: applied in arrival order, a read taken before
// a window closed and observed after one that found it closed put the window
// back, and the next blip raised the alarm about it.

// errNoFleet is why a node with no coordination store reads no window.
var errNoFleet = errors.New("engine: this node has no coordination store")

// windowReads numbers every read of one log's capacity window this process
// sends, so a watch can tell an older read from a newer one whatever order the
// two reach it in. Process-wide because every reader reads through
// [readWindows], the gestures' included.
var windowReads atomic.Uint64

// windowsRead is one read of every log's capacity window.
type windowsRead struct {
	// open is every window found open, the oldest first.
	open []coord.MaintenanceOperation
	// unread is every log whose window couldn't be read, in the register's
	// order.
	unread []string
	// errs is why each unread log couldn't be read, keyed by stream, for the
	// log line: the gestures write one per gesture ([Engine.CapacityWindows])
	// and the report's watch one each time a log starts failing and stops
	// ([windowWatch.observe]).
	errs map[string]error
	// taken is the number each log's read was sent under ([windowReads]),
	// keyed by stream.
	taken map[string]uint64
}

// readWindows reads whether a capacity window is open on every log one can be
// opened on.
//
// A NODE WITH NO COORDINATION STORE READS NOTHING, and says so for every log
// rather than answering that none has a window: it can't know.
func readWindows(ctx context.Context, fleet coord.Fleet) windowsRead {
	out := windowsRead{taken: map[string]uint64{}}
	for _, stream := range maintenanceStreams() {
		var (
			op    coord.MaintenanceOperation
			found bool
			err   = errNoFleet
		)
		out.taken[stream] = windowReads.Add(1)
		if fleet != nil {
			op, found, err = fleet.Maintenance(ctx, stream)
		}
		switch {
		case err != nil:
			out.unread = append(out.unread, stream)
			if out.errs == nil {
				out.errs = map[string]error{}
			}
			out.errs[stream] = err
		case found:
			out.open = append(out.open, op)
		}
	}
	// THE OLDEST FIRST, because it has been holding the fleet longest: two
	// windows open at once each wait on their own nodes, and the one that
	// has waited longest is the one a reader looks at first.
	sort.SliceStable(out.open, func(i, j int) bool {
		if !out.open[i].EnteredAt.Equal(out.open[j].EnteredAt) {
			return out.open[i].EnteredAt.Before(out.open[j].EnteredAt)
		}
		return out.open[i].Stream < out.open[j].Stream
	})
	return out
}

// windowWatch is what this node last read of each log's capacity window, for
// the alarm's third answer.
//
// IN MEMORY, because it is a fact about this node's reads and nobody else's,
// and because what it holds is only ever the last thing a read here found: a
// node that restarts mid-outage starts knowing nothing, and raises nothing
// for a log it never read.
type windowWatch struct {
	mu sync.Mutex
	// known is each log's window as the newest read that answered found it,
	// open or not, with when it was read and that read's number.
	known map[string]knownWindow
	// latest is the number of the newest read of each log the watch has taken
	// account of, answered or not, and failing whether that read failed: the
	// state the log line is written on the change of.
	latest  map[string]uint64
	failing map[string]bool
	// parts is every part of an open window the report couldn't read on its
	// last try (its acknowledgements, the admissions), keyed by stream and
	// part, so the log line is written once when a read starts failing and
	// once when it stops ([windowWatch.part]).
	parts map[string]bool
}

// knownWindow is one log's window as a read that answered found it.
type knownWindow struct {
	op    coord.MaintenanceOperation
	open  bool
	at    time.Time
	taken uint64
}

// newWindowWatch is a watch that has read nothing yet.
func newWindowWatch() *windowWatch {
	return &windowWatch{known: map[string]knownWindow{}, latest: map[string]uint64{},
		failing: map[string]bool{}, parts: map[string]bool{}}
}

// judgedWindow is a window the alarm judges: open as this node last read it,
// when that read was, and whether the window can't be read now.
type judgedWindow struct {
	op         coord.MaintenanceOperation
	readAt     time.Time
	unreadable bool
}

// observe takes one read at now and answers the windows the alarm judges: for
// every log, the window the newest read that answered found open there, if it
// found one, marked unreadable where the newest read of it failed.
//
// THE NEWEST READ WINS, by the number it was sent under, never by when it
// arrives here: an older read observed after a newer one changes nothing the
// newer one decided.
//
// A READ THE CALLER'S OWN CONTEXT ENDED says nothing about coordination, so it
// changes nothing remembered: a log it couldn't read is judged on what was
// known.
//
// A NIL WATCH REMEMBERS NOTHING, which judges exactly what this read found: an
// unread log raises nothing, since nothing here ever saw a window on it.
func (w *windowWatch) observe(ctx context.Context, now time.Time, read windowsRead) []judgedWindow {
	if w == nil {
		out := make([]judgedWindow, 0, len(read.open))
		for _, op := range read.open {
			out = append(out, judgedWindow{op: op, readAt: now})
		}
		return out
	}
	open := make(map[string]coord.MaintenanceOperation, len(read.open))
	for _, op := range read.open {
		open[op.Stream] = op
	}
	stopped := ctx.Err() != nil
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, stream := range maintenanceStreams() {
		taken := read.taken[stream]
		err, unread := read.errs[stream]
		if unread && stopped {
			continue
		}
		if !unread && taken > w.known[stream].taken {
			op, found := open[stream]
			w.known[stream] = knownWindow{op: op, open: found, at: now, taken: taken}
			if !found {
				// A LOG WITH NO WINDOW has no parts to read, so whatever
				// failed on the last one starts again from nothing.
				for _, part := range windowParts {
					delete(w.parts, partKey(stream, part))
				}
			}
		}
		if taken <= w.latest[stream] {
			continue
		}
		w.latest[stream] = taken
		switch {
		case unread && !w.failing[stream]:
			w.failing[stream] = true
			log.WarnContext(ctx, "capacity_window_unreadable", "stream", stream,
				"err", err, "detail", "this node can't read whether a capacity "+
					"window is open on this log, and the retention report and the "+
					"alarm say so until it can")
		case !unread && w.failing[stream]:
			delete(w.failing, stream)
			log.InfoContext(ctx, "capacity_window_readable", "stream", stream)
		}
	}
	var out []judgedWindow
	for _, stream := range maintenanceStreams() {
		if k := w.known[stream]; k.open {
			out = append(out, judgedWindow{op: k.op, readAt: k.at,
				unreadable: w.failing[stream]})
		}
	}
	return out
}

// The parts of an open window the report reads beside it, as its log line
// names them.
const (
	partAcks       = "acknowledgements"
	partAdmissions = "admissions"
)

var windowParts = []string{partAcks, partAdmissions}

func partKey(stream, part string) string { return stream + " " + part }

// part notes one read of a part of an open window, and writes the log line
// when its reads start failing and when they stop: the report says it couldn't
// read the part, and this is where the reason is. Once per change rather than
// per report, which the alarm heartbeat assembles every ten seconds. A NIL
// WATCH remembers nothing, so it writes every failure.
func (w *windowWatch) part(ctx context.Context, stream, part string, err error) {
	if err != nil && ctx.Err() != nil {
		return
	}
	if w == nil {
		if err != nil {
			log.WarnContext(ctx, "capacity_window_part_unreadable", "stream", stream,
				"part", part, "err", err)
		}
		return
	}
	key := partKey(stream, part)
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case err != nil && !w.parts[key]:
		w.parts[key] = true
		log.WarnContext(ctx, "capacity_window_part_unreadable", "stream", stream,
			"part", part, "err", err)
	case err == nil && w.parts[key]:
		delete(w.parts, key)
		log.InfoContext(ctx, "capacity_window_part_readable", "stream", stream,
			"part", part)
	}
}
