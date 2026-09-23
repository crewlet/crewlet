package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT BINDING THAT DANGLES, and the one rule that says so.
//
// # Two logs, and the residue they leave
//
// A person's binding lives on the IDENTITY log and the seat it names on the
// CHART's. Two streams, two appliers, two arbitration anchors — so a bind and a
// seat's removal can each pass their own decide and both land, and a node can
// apply a bind before the hire it names. The design states the residue rather
// than claiming the boundary does the work, and it names two:
//
//   - SETTLED: the seat is tombstoned, is not a human seat, or is absent from a
//     chart that has seen everything the bind saw. Nothing clears it but a
//     record — an unbind, or a bind to another seat.
//   - NOT YET: the seat is absent from a chart that has not applied as far as
//     the binding. It clears when this node's chart applier catches up.
//
// Both are LEGAL — neither is corruption — and both are reported: by `crewlet
// iam check` and `GET /iam/check` (`binding_dangling`, with the seat and which
// residue it is), and, once one has outlived the race that makes it, by the
// `iam_binding_dangling` alarm.
//
// # Why the rule is the REQUEST PATH's own table
//
// [session.ResolveSeat] is what decides, per request, whether a signed-in
// person is served, refused 403 naming their seat, or held off 503 — and a
// binding is dangling exactly when that table would refuse or hold them for
// want of the seat. A second predicate written here ("does the chart hold this
// handle") is how the report came to miss a binding to an AGENT seat: it asked
// whether the row existed, the request path asks whether it is a human seat,
// and a person the request path refused on every call was one the report said
// nothing about. So both surfaces ask this function, and this function asks
// the table.
//
// # Why the alarm's age is observed, not read
//
// Nothing records when a binding began to dangle: it began when THIS node
// applied the later of a bind and a removal, which no record carries. So the
// age the alarm compares against the stall grace is how long this node's own
// observations have kept finding the residue — first sighting to latest — and
// never a persistence nobody saw. The observations are the alarm table's own
// heartbeat, every [statelog.AlarmInterval] (see [retention.heartbeat]) on every
// node that RUNS THE IDENTITY DOMAIN — one that runs none holds an empty copy
// of the directory, so it keeps no watch and observes nothing rather than
// reporting a company with no residue (see [newBindingWatch]). That is what
// lets the alarm fire at the stall grace the design gives it rather than at the
// trim's quarter-hour, and what keeps the three surfaces in step: the gauge,
// the log line and the screen all read the same observation.
//
// # Why a heartbeat can afford it
//
// An observation reads nothing that has not moved. A residue is a function of
// this node's identity rows and its chart rows and nothing else, and each has
// an applier position that moves whenever a row does — so a beat on which
// neither position moved re-reads nothing and extends what the last one found.
// When one did move, the bindings are one read over the bound people rather
// than a walk of the directory, and a binding is re-classified against the
// chart only when it, or the chart, changed since it was last classified.
// The chart moving is the rare case — an org-chart edit — and the identity
// log moving is the common one, since every sign-in is a record on it: that
// arm costs one indexed read and no chart read at all.

// BindingResidue is one person whose seat binding this node's chart view does
// not hold as a human seat.
type BindingResidue struct {
	// Person is the directory id, Login the name the dashboard prints and
	// Seat the seat their row names, by its IDENTITY — the handle it was
	// created under (ADR-0020).
	Person, Login, Seat string

	// Settled is the first residue — removed, tombstoned or not a human
	// seat, on a chart that has seen everything the bind saw — against the
	// second, a chart this node has not applied as far as the binding.
	// Two arms because they have two remedies: somebody has to write the
	// first one's record, and the second clears itself.
	Settled bool

	// Detail is the sentence every surface prints: which seat, why, and
	// what to do.
	Detail string
}

// key is what the alarm's clock follows a residue by: the person AND the seat,
// so a person rebound from one dangling seat to another starts a new clock
// rather than inheriting the first one's age.
func (r BindingResidue) key() string { return r.Person + "\x00" + r.Seat }

// bindingProbeBudget bounds one person's seat lookup.
//
// TWO SECONDS, and it is a per-ROW budget on a classification that may cover
// every binding in the company — so the number is what one local SQL read on a
// busy node costs at its worst rather than what a network call would. A lookup
// that cannot answer inside it is the unknown arm, which both surfaces skip.
const bindingProbeBudget = 2 * time.Second

// errSeatUnknown is the unknown arm when the resolver has no read failure of
// its own to name: a chart applier past the stall grace, or a node with no
// chart view at all.
var errSeatUnknown = errors.New("engine: this node's org chart cannot say " +
	"whether that seat exists")

// DanglingBinding classifies one person's seat binding against this node's
// own chart view.
//
// THREE-VALUED: dangling, not dangling, or an error when this node cannot tell
// — a chart applier past the stall grace, an unreadable view, a node running
// no chart domain. Every caller skips the third rather than guessing, because
// reporting a binding as dangling on a node that could not read the chart
// sends an administrator to unbind somebody whose seat is perfectly there.
func (e *Engine) DanglingBinding(ctx context.Context, row iamdomain.PersonRow) (
	BindingResidue, bool, error) {

	return danglingBinding(ctx, SeatViewOf(e), row.Binding())
}

// danglingBinding is the rule, over any chart view.
func danglingBinding(ctx context.Context, chart session.Chart, b iamdomain.SeatBinding) (
	BindingResidue, bool, error) {

	residue := BindingResidue{Person: b.Person, Login: b.Login, Seat: b.Seat}
	if b.Seat == "" {
		// NO BINDING. A removed person has no row to be asked about at
		// all: a removal deletes it and leaves the tombstone.
		return residue, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, bindingProbeBudget)
	defer cancel()
	binding := session.ResolveSeat(ctx, chart, session.PersonRow{
		Found: true, Stage: b.Stage, Seat: b.Seat, SeatAt: b.SeatAt,
	})
	switch binding.Row {
	case session.SeatRowGone:
		residue.Settled = true
		residue.Detail = binding.Detail + "; unbind them, or bind them to " +
			"another seat"
		return residue, true, nil
	case session.SeatRowBehind:
		residue.Detail = binding.Detail + "; it clears when this node's chart " +
			"applier reaches the binding, and if it does not, that applier " +
			"is what to look at"
		return residue, true, nil
	case session.SeatRowStalled:
		cause := binding.Err
		if cause == nil {
			cause = errSeatUnknown
		}
		return residue, false, fmt.Errorf("engine: resolve %q's seat %q: %s: %w",
			b.Person, b.Seat, binding.Detail, cause)
	}
	return residue, false, nil
}

// bindingSource is the directory's read side, as narrowly as the watch needs
// it: every binding in one read, and the position that read reflects.
type bindingSource interface {
	SeatBindings(ctx context.Context) ([]iamdomain.SeatBinding, error)
	At() statelog.Position
}

// bindingSighting is what one classification of the bindings found.
type bindingSighting struct {
	// residues are the bindings that dangle, in the order they were read.
	residues []BindingResidue

	// unknown are the residues' keys this classification could not settle.
	// They are neither dangling nor clear, and the clock treats them as
	// neither: a residue that was dangling before a chart stall is still
	// dangling after it unless something said otherwise.
	unknown map[string]bool
}

// classified is one binding's last classification, and what it was taken
// against — so an observation re-classifies a binding only when the binding or
// the chart moved.
type classified struct {
	binding  iamdomain.SeatBinding
	chartAt  uint64
	residue  BindingResidue
	dangling bool
}

// bindingWatch is how long each dangling binding has persisted on this node.
//
// PER NODE AND IN MEMORY, which is the honest answer to "who has to agree on
// it": the chart half of a residue is this node's own applier's position, so
// two nodes legitimately disagree about a hire one of them has not applied,
// and the age is this node's observation of its own state. Losing it at a
// restart costs one grace of re-observation, which is the price of never
// claiming a persistence this process did not see.
type bindingWatch struct {
	// dir and chart are this node's directory and chart view. A nil dir
	// is a node running no identity domain, which observes nothing.
	dir   bindingSource
	chart session.Chart

	mu sync.Mutex
	// first is when each residue was first found, carried across
	// observations that found it again or could not classify it.
	first map[string]time.Time
	// at, seen and known are the latest observation: when it was taken,
	// what dangled and whether any classification has ever succeeded.
	at    time.Time
	seen  []BindingResidue
	known bool

	// dirAt and chartAt are the two positions the latest classification
	// was taken at, and unsettled marks one that could not classify
	// every binding — which the next beat re-reads whether or not
	// anything moved.
	dirAt     statelog.Position
	chartAt   uint64
	unsettled bool
	// classes is each binding's last classification, by person.
	classes map[string]classified

	// failingSince is when the current run of walks that could not read
	// the bindings began, and failed how many beats it has lasted; both
	// zero while the last walk read them. See [bindingWatch.walkFailed]
	// for why a run is said twice rather than once a beat.
	failingSince time.Time
	failed       int

	// logger is where the walk speaks: the package's own outside a case.
	logger *slog.Logger
}

// newBindingWatch is the watch over one engine, or nil on a node that runs no
// identity domain — which has a legitimately empty copy of the directory, so a
// walk over it would report every node without one as having no residue at all
// rather than as not asking.
func newBindingWatch(e *Engine) *bindingWatch {
	dir := e.IAM()
	if dir == nil {
		return nil
	}
	return newWatchOver(dir, SeatViewOf(e))
}

// newWatchOver is a watch over any directory and chart view.
func newWatchOver(dir bindingSource, chart session.Chart) *bindingWatch {
	return &bindingWatch{dir: dir, chart: chart, first: map[string]time.Time{},
		classes: map[string]classified{}, logger: log}
}

// observe takes one observation of this node's bindings at now.
//
// NOTHING MOVED IS NOTHING TO READ: when neither the identity applier nor the
// chart applier has committed since the last classification, and that one
// settled every binding, the residues are exactly what they were, and the
// observation extends them to now. The positions are read BEFORE the bindings,
// so they are a floor under what the read saw: a record landing between the
// two is one the next beat re-reads for.
func (w *bindingWatch) observe(ctx context.Context, now time.Time) {
	if w == nil || w.dir == nil {
		return
	}
	dirAt := w.dir.At()
	chartAt, _, chartErr := w.chart.Position(ctx)
	w.mu.Lock()
	quiet := w.known && !w.unsettled && chartErr == nil &&
		dirAt == w.dirAt && chartAt == w.chartAt
	if quiet {
		w.at = now
		w.mu.Unlock()
		return
	}
	previous := w.classes
	w.mu.Unlock()

	bindings, err := w.dir.SeatBindings(ctx)
	if err != nil {
		// UNREADABLE IS NOT CLEAR, and it is not "still dangling" either:
		// the observation is not taken at all, so a firing alarm stays up
		// on the reading it fired on and a residue that has not fired
		// does not age through an outage it was not seen through. The
		// clocks are kept, so a residue that outlives the outage is not
		// made to wait out the grace again.
		if errors.Is(err, store.ErrNoEstate) || errors.Is(err, context.Canceled) {
			// A stop this process asked for — a shutdown, an
			// adoption's rename — is not an unreadable directory.
			return
		}
		w.walkFailed(ctx, now, err)
		return
	}
	w.walkRecovered(ctx, now)
	sighting, classes, settled := classify(ctx, w.chart, bindings, previous,
		chartAt, chartErr == nil)
	w.record(now, sighting, classes, dirAt, chartAt, !settled || chartErr != nil)
}

// classify settles every binding, re-using a classification whose binding and
// chart position have not moved since it was taken.
//
// A binding whose seat the chart cannot judge is left out of the cache, so the
// next observation asks again; settled reports whether there was none.
func classify(ctx context.Context, chart session.Chart, bindings []iamdomain.SeatBinding,
	previous map[string]classified, chartAt uint64, chartKnown bool) (
	bindingSighting, map[string]classified, bool) {

	out := bindingSighting{unknown: map[string]bool{}}
	classes := make(map[string]classified, len(bindings))
	settled := true
	for _, b := range bindings {
		if c, ok := previous[b.Person]; ok && chartKnown && c.chartAt == chartAt &&
			c.binding == b {
			classes[b.Person] = c
			if c.dangling {
				out.residues = append(out.residues, c.residue)
			}
			continue
		}
		residue, dangling, err := danglingBinding(ctx, chart, b)
		if err != nil {
			out.unknown[residue.key()] = true
			settled = false
			continue
		}
		if chartKnown {
			classes[b.Person] = classified{binding: b, chartAt: chartAt,
				residue: residue, dangling: dangling}
		}
		if dangling {
			out.residues = append(out.residues, residue)
		}
	}
	return out, classes, settled
}

// record folds one classification into the clocks.
//
// A RESIDUE THE CHART COULD NOT JUDGE THIS TIME STAYS A RESIDUE if it was one
// before: the chart stalling says nothing about the binding, and dropping it
// would clear a firing alarm on the stall and raise it again a beat after the
// applier recovered — two transitions on every surface for a state that never
// changed.
func (w *bindingWatch) record(now time.Time, s bindingSighting,
	classes map[string]classified, dirAt statelog.Position, chartAt uint64,
	unsettled bool) {

	w.mu.Lock()
	defer w.mu.Unlock()
	residues := slices.Clone(s.residues)
	for _, r := range w.seen {
		if s.unknown[r.key()] {
			residues = append(residues, r)
		}
	}
	next := make(map[string]time.Time, len(residues))
	for _, r := range residues {
		if since, seen := w.first[r.key()]; seen {
			next[r.key()] = since
			continue
		}
		next[r.key()] = now
	}
	for key := range s.unknown {
		if since, seen := w.first[key]; seen {
			next[key] = since
		}
	}
	// A RESIDUE THIS CLASSIFICATION FOUND CLEAR IS DROPPED, so the same
	// binding dangling again later — a seat removed, restored and removed —
	// starts a new clock rather than firing at once on an age it did not
	// have.
	w.first, w.at, w.seen, w.known = next, now, residues, true
	w.classes, w.dirAt, w.chartAt, w.unsettled = classes, dirAt, chartAt, unsettled
}

// walkFailed notes a beat whose read of the bindings failed, and says so only
// if it is the first of its run.
//
// # The rate is the alarm table's own
//
// The walk runs on the alarm heartbeat, so a directory that cannot be read
// fails it every [statelog.AlarmInterval] — four lines a minute for as long as
// the outage lasts, where the quarter-hourly evaluation it replaced wrote one.
// A failing walk is a STATE of the same shape as an alarm: it holds for minutes
// or days, and while it holds the `iam_binding_dangling` alarm is blind,
// standing on the reading it last took. So it is said at the rate
// [statelog.Tracker] says an alarm: once when it begins and once when it ends,
// the second carrying how long it lasted — because a level repeated every beat
// makes the log useless for the one thing an operator reads it for, when the
// state STARTED, and the end is what says how long the alarm's silence meant
// nothing. Both at WARN, as the tracker's pair is, so a filter that shows one
// end shows the other.
func (w *bindingWatch) walkFailed(ctx context.Context, now time.Time, err error) {
	w.mu.Lock()
	first := w.failingSince.IsZero()
	if first {
		w.failingSince = now
	}
	w.failed++
	w.mu.Unlock()
	if !first {
		return
	}
	w.logger.WarnContext(ctx, "iam_binding_walk_failed", "err", err,
		"detail", "the iam_binding_dangling alarm holds what it last "+
			"observed until a read of the bindings succeeds; "+
			"iam_binding_walk_recovered says when that is")
}

// walkRecovered notes a beat that read the bindings, and ends a run of failed
// ones by saying how long it lasted.
func (w *bindingWatch) walkRecovered(ctx context.Context, now time.Time) {
	w.mu.Lock()
	since, failed := w.failingSince, w.failed
	w.failingSince, w.failed = time.Time{}, 0
	w.mu.Unlock()
	if since.IsZero() {
		return
	}
	w.logger.WarnContext(ctx, "iam_binding_walk_recovered",
		"for", now.Sub(since).Round(time.Second).String(), "failed_beats", failed,
		"detail", "the bindings are readable again, and iam_binding_dangling "+
			"reflects them from this beat")
}

// fill writes the latest observation into a reading.
//
// THE AGE IS FIRST SIGHTING TO LATEST OBSERVATION, never to now: a report
// assembled between two beats knows the residue was there at the last one and
// nothing since, so measuring to now would call a residue old that may have
// been repaired a moment after the beat — and would let the screen fire an
// alarm the gauge beside it, set at the beat, does not.
func (w *bindingWatch) fill(out *statelog.Reading) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.known {
		return
	}
	for _, r := range w.seen {
		age := w.at.Sub(w.first[r.key()])
		out.DanglingBindings++
		if out.DanglingBindingSeat == "" || age > out.DanglingBindingFor {
			out.DanglingBindingFor, out.DanglingBindingSeat = age, r.Seat
		}
	}
}
