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

// WHETHER A CAPACITY WINDOW IS OPEN, read one way by every reader. A log has a
// window open, has none, or this node couldn't read which, and the third is
// never the second: a window stops every publisher, and the outage that blocks
// the read is often why somebody looks. So there is ONE READ ([readWindows])
// over ONE SET of logs ([maintenanceStreams]), and every reader takes it whole:
// the gestures ([Engine.CapacityWindows]), the report's `maintenance` and
// `maintenance_unread`, and the alarm.
//
// On "couldn't read" the alarm keeps what this node last read ([windowWatch]):
// a blip neither clears nor raises `maintenance_open`, a held window is aged as
// of that read rather than on this node's clock, and the newest read wins by
// the number it was sent under ([windowReads]), never by arrival order.

// errNoFleet is why a node with no coordination store reads no window.
var errNoFleet = errors.New("engine: this node has no coordination store")

// windowReads numbers every read of one log's capacity window this process
// sends, so a watch tells an older read from a newer one whatever order they
// arrive in: reports are assembled concurrently. Process-wide because every
// reader reads through [readWindows].
var windowReads atomic.Uint64

// windowsRead is one read of every log's capacity window.
type windowsRead struct {
	// open is every window found open, the oldest first.
	open []coord.MaintenanceOperation
	// unread is every log whose window couldn't be read, in the register's
	// order.
	unread []string
	// errs is why each unread log couldn't be read, keyed by stream, for the
	// log lines of [Engine.CapacityWindows] and [windowWatch.observe].
	errs map[string]error
	// taken is each log's read number ([windowReads]), keyed by stream.
	taken map[string]uint64
}

// readWindows reads whether a capacity window is open on every log one can be
// opened on. A NODE WITH NO COORDINATION STORE names every log unread rather
// than answering that none has a window: it can't know.
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
	// THE OLDEST FIRST: it has held the fleet longest, so a reader looks at
	// it first.
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
// IN MEMORY: it is a fact about this node's reads alone, so a node that
// restarts mid-outage knows nothing and raises nothing for a log it never read.
type windowWatch struct {
	mu sync.Mutex
	// known is each log's window as the newest read that answered found it,
	// open or not, with when it was read and that read's number.
	known map[string]knownWindow
	// latest is the number of each log's newest read taken account of,
	// answered or not, and failing whether it failed: the log line is written
	// when failing changes.
	latest  map[string]uint64
	failing map[string]bool
	// parts is every part of an open window ([windowParts]) the report's last
	// try couldn't read, keyed by [partKey], so its log line is written once
	// per change ([windowWatch.part]).
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
// every log, the window the newest answering read found open there, marked
// unreadable where the newest read of it failed.
//
// THE NEWEST READ WINS by the number it was sent under, never by arrival. A
// read the caller's own context ended says nothing about coordination, so it
// changes nothing remembered. A NIL WATCH remembers nothing, so it judges
// exactly this read: an unread log raises nothing.
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
				// A LOG WITH NO WINDOW has no parts: their failures reset.
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
	partRetired    = "retirements"
)

var windowParts = []string{partAcks, partAdmissions, partRetired}

func partKey(stream, part string) string { return stream + " " + part }

// part notes one read of a part of an open window and logs the reason when its
// reads start failing and when they stop: once per change, not per report,
// which the alarm heartbeat assembles every ten seconds. A NIL WATCH remembers
// nothing, so it logs every failure.
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
