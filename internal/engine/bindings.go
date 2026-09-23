package engine

import (
	"context"
	"errors"
	"fmt"
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
// iam check` and the Access screen, and, once one has outlived the race that
// makes it, by the `iam_binding_dangling` alarm.
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
// evaluations have kept finding the residue — first sighting to latest — and
// never a persistence nobody saw. The evaluations are the alarm table's own
// tick, on every node (see [retention.tick]), which is what keeps the three
// surfaces in step: the gauge, the log line and the screen all read the same
// observation, so none of them can call a residue old that the others have not
// seen persist.

// BindingResidue is one person whose seat binding this node's chart view does
// not hold as a human seat.
type BindingResidue struct {
	// Person is the directory id, Login the name the dashboard prints and
	// Seat the handle their row names.
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
// TWO SECONDS, and it is a per-ROW budget on a walk that may cover the whole
// directory — so the number is what one local SQL read on a busy node costs at
// its worst rather than what a network call would. A lookup that cannot answer
// inside it is the unknown arm, which both surfaces skip.
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

	return danglingBinding(ctx, SeatViewOf(e), row)
}

// danglingBinding is the rule, over any chart view.
func danglingBinding(ctx context.Context, chart session.Chart, row iamdomain.PersonRow) (
	BindingResidue, bool, error) {

	residue := BindingResidue{Person: row.ID, Login: row.Login, Seat: row.Seat}
	if row.Seat == "" || row.Shredded {
		// NO BINDING, or a person a removal has already shredded: the
		// directory keeps their row so the audit trail resolves, and a
		// seat named on a row that can never act again binds nobody.
		return residue, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, bindingProbeBudget)
	defer cancel()
	binding := session.ResolveSeat(ctx, chart, session.PersonRow{
		Found: true, Stage: row.Stage, Seat: row.Seat, SeatAt: row.SeatAt,
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
			row.ID, row.Seat, binding.Detail, cause)
	}
	return residue, false, nil
}

// peopleLister is the directory's read side, as narrowly as a walk needs it.
type peopleLister interface {
	People(ctx context.Context, q iamdomain.PeopleQuery) (iamdomain.PeoplePage, error)
}

// bindingSighting is what one walk of the directory found.
type bindingSighting struct {
	// residues are the bindings that dangle, in directory order.
	residues []BindingResidue

	// unknown are the residues' keys this walk could not classify. They
	// are neither dangling nor clear, and the clock treats them as
	// neither: a residue that was dangling before a chart stall is still
	// dangling after it unless something said otherwise.
	unknown map[string]bool
}

// sightBindings walks the whole directory once and classifies every binding.
//
// A FAILED PAGE FAILS THE WALK rather than yielding what the pages before it
// found: a walk that stopped halfway would clear every residue in the second
// half, and a clock reset by a read error is an alarm that can never age past
// a flaky minute.
func sightBindings(ctx context.Context, dir peopleLister, chart session.Chart) (
	bindingSighting, error) {

	out := bindingSighting{unknown: map[string]bool{}}
	after := ""
	for {
		page, err := dir.People(ctx, iamdomain.PeopleQuery{
			After: after, Limit: iamdomain.MaxPageSize,
		})
		if err != nil {
			return bindingSighting{}, fmt.Errorf("engine: walk the directory: %w", err)
		}
		for _, row := range page.People {
			residue, dangling, err := danglingBinding(ctx, chart, row)
			switch {
			case err != nil:
				out.unknown[residue.key()] = true
			case dangling:
				out.residues = append(out.residues, residue)
			}
		}
		if page.Next == "" {
			return out, nil
		}
		after = page.Next
	}
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
	dir   peopleLister
	chart session.Chart

	mu sync.Mutex
	// first is when each residue was first found, carried across walks
	// that found it again or could not classify it.
	first map[string]time.Time
	// at, seen and known are the latest walk: when it ran, what it found
	// and whether it could read the directory at all.
	at    time.Time
	seen  []BindingResidue
	known bool
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
	return &bindingWatch{dir: dir, chart: SeatViewOf(e), first: map[string]time.Time{}}
}

// observe walks the directory once and records what it found at now.
func (w *bindingWatch) observe(ctx context.Context, now time.Time) {
	if w == nil || w.dir == nil {
		return
	}
	sighting, err := sightBindings(ctx, w.dir, w.chart)
	if err != nil {
		w.mu.Lock()
		// UNREADABLE IS NOT CLEAR, and it is not "still dangling"
		// either: the reading says nothing until a walk succeeds, and
		// the clocks are KEPT, so a residue that outlives an outage is
		// not made to wait out the grace again.
		w.known = false
		w.mu.Unlock()
		if errors.Is(err, store.ErrNoEstate) || errors.Is(err, context.Canceled) {
			// A stop this process asked for — a shutdown, an
			// adoption's rename — is not an unreadable directory.
			return
		}
		log.WarnContext(ctx, "iam_binding_walk_failed", "err", err,
			"detail", "the iam_binding_dangling alarm says nothing until a "+
				"walk succeeds; the residues it was following keep their age")
		return
	}
	w.record(now, sighting)
}

// record folds one successful walk into the clocks.
func (w *bindingWatch) record(now time.Time, s bindingSighting) {
	w.mu.Lock()
	defer w.mu.Unlock()
	next := make(map[string]time.Time, len(s.residues))
	for _, r := range s.residues {
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
	// A RESIDUE THIS WALK FOUND CLEAR IS DROPPED, so the same binding
	// dangling again later — a seat removed, restored and removed — starts
	// a new clock rather than firing at once on an age it did not have.
	w.first, w.at, w.seen, w.known = next, now, s.residues, true
}

// fill writes the latest walk into a reading.
//
// THE AGE IS FIRST SIGHTING TO LATEST WALK, never to now: a report assembled
// between two walks knows the residue was there at the last one and nothing
// since, so measuring to now would call a residue old that may have been
// repaired a minute after the walk — and would let the screen fire an alarm
// the gauge beside it, set at the walk, does not.
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
