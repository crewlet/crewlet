package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// seatTable is a [session.Chart] answering from a table, counting the seat
// reads it serves.
type seatTable struct {
	position uint64
	lag      time.Duration
	posErr   error
	seats    map[string]session.Seat
	seatErr  error
	reads    *atomic.Int64
}

func (c seatTable) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
	if c.reads != nil {
		c.reads.Add(1)
	}
	if c.seatErr != nil {
		return session.Seat{}, false, c.seatErr
	}
	seat, found := c.seats[ref]
	return seat, found, nil
}

func (c seatTable) Position(context.Context) (uint64, time.Duration, error) {
	return c.position, c.lag, c.posErr
}

// companyChart is a chart at position 1 000 holding a human seat, an agent
// seat and a tombstone.
func companyChart() seatTable {
	return seatTable{position: 1000, reads: &atomic.Int64{}, seats: map[string]session.Seat{
		"platform-lead": {Handle: "platform-lead", Kind: session.SeatKindHuman},
		"triage-bot":    {Handle: "triage-bot", Kind: "agent"},
		"old-lead":      {Handle: "old-lead", Tombstoned: true},
	}}
}

// bound is a person bound to a seat, decided at a chart position.
func bound(id, seat string, at uint64) iamdomain.SeatBinding {
	return iamdomain.SeatBinding{
		Person: id, Login: id + ".person", Stage: iam.StageActive,
		Seat: seat, SeatAt: at,
	}
}

// A BINDING DANGLES EXACTLY WHEN THE REQUEST PATH WOULD REFUSE OR HOLD THE
// PERSON FOR WANT OF THE SEAT — both residues, and nothing else.
//
// The predicate this replaced asked whether the chart held a row by that
// handle, so a person bound to an AGENT seat — refused 403 on every request
// they made — was one the report and the alarm said nothing about. The rule is
// now the seat table itself, and this walks every row of it.
func TestABindingDanglesExactlyWhenTheSeatTableRefusesOrHoldsItsPerson(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		chart    seatTable
		row      iamdomain.SeatBinding
		dangling bool
		settled  bool
		unknown  bool
		says     string
	}{
		"a human seat the chart holds": {
			chart: companyChart(), row: bound("p1", "platform-lead", 900),
		},
		"no binding at all": {
			chart: companyChart(), row: bound("p1", "", 0),
		},
		"an agent seat, which the old predicate called held": {
			chart: companyChart(), row: bound("p1", "triage-bot", 900),
			dangling: true, settled: true, says: `"agent" seat`,
		},
		"a tombstoned seat": {
			chart: companyChart(), row: bound("p1", "old-lead", 900),
			dangling: true, settled: true, says: "tombstoned",
		},
		"an absent seat on a chart that has seen the bind": {
			chart: companyChart(), row: bound("p1", "gone-lead", 900),
			dangling: true, settled: true, says: "covers the binding",
		},
		"an absent seat on a chart that has not applied the hire yet": {
			chart: companyChart(), row: bound("p1", "new-hire", 1200),
			dangling: true, settled: false, says: "clears when",
		},
		"a chart applier past the stall grace": {
			chart: seatTable{position: 1000, lag: 2 * statelog.StallGrace},
			row:   bound("p1", "new-hire", 1200), unknown: true,
		},
		"a chart view that cannot be read": {
			chart: seatTable{position: 1000, seatErr: errors.New("estate closed")},
			row:   bound("p1", "platform-lead", 900), unknown: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			residue, dangling, err := danglingBinding(t.Context(), tc.chart, tc.row)
			if tc.unknown {
				if err == nil {
					t.Fatalf("a chart that cannot answer gave dangling=%v with no "+
						"error — a guess either way sends somebody to the wrong "+
						"remedy", dangling)
				}
				return
			}
			if err != nil {
				t.Fatalf("danglingBinding: %v", err)
			}
			if dangling != tc.dangling {
				t.Fatalf("dangling = %v, want %v (%s)", dangling, tc.dangling,
					residue.Detail)
			}
			if !dangling {
				return
			}
			if residue.Settled != tc.settled {
				t.Errorf("settled = %v, want %v — the two residues have two "+
					"remedies", residue.Settled, tc.settled)
			}
			if !strings.Contains(residue.Detail, tc.says) ||
				!strings.Contains(residue.Detail, tc.row.Seat) {
				t.Errorf("detail %q does not say %q about %q", residue.Detail,
					tc.says, tc.row.Seat)
			}
		})
	}
}

// bindingsDir is a directory's bindings at a position a case moves by hand —
// which is what a record landing does to the real one.
type bindingsDir struct {
	mu       sync.Mutex
	bindings []iamdomain.SeatBinding
	at       statelog.Position
	fail     error
	reads    int
}

func dirOf(bindings ...iamdomain.SeatBinding) *bindingsDir {
	return &bindingsDir{bindings: bindings, at: statelog.Position{Seq: 1}}
}

// set replaces the bindings, as a committed record would, and moves the
// position with them.
func (d *bindingsDir) set(bindings ...iamdomain.SeatBinding) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bindings = bindings
	d.at.Seq++
}

// touch moves the position with nothing about a binding changing — a sign-in.
func (d *bindingsDir) touch() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at.Seq++
}

func (d *bindingsDir) SeatBindings(context.Context) ([]iamdomain.SeatBinding, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	if d.fail != nil {
		return nil, d.fail
	}
	return append([]iamdomain.SeatBinding(nil), d.bindings...), nil
}

func (d *bindingsDir) At() statelog.Position {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.at
}

func (d *bindingsDir) readCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

// firing reports whether a reading built from the watch fires the alarm.
func firing(w *bindingWatch) (statelog.Reading, bool) {
	var reading statelog.Reading
	w.fill(&reading)
	for _, a := range statelog.Evaluate(reading) {
		if a.Kind == statelog.KindBindingDangling {
			return reading, true
		}
	}
	return reading, false
}

// A RESIDUE YOUNGER THAN THE GRACE DOES NOT FIRE — the control the alarm is
// built around — and one past it fires within a beat.
//
// A bind racing a removal, or a hire this node's chart applier reaches a few
// seconds late, is a dangling binding for moments. The alarm fires once a
// residue has been FOUND by this node's own observations for longer than the
// stall grace, measured from the first that found it to the latest — and not a
// moment before, so nobody is paged for a state that was already clearing.
// The observations are the heartbeat's, so "past the grace" is one
// [statelog.AlarmInterval] past it, not a quarter of an hour.
func TestAResidueYoungerThanTheGraceDoesNotFire(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	w := newWatchOver(dirOf(bound("p1", "triage-bot", 900)), companyChart())
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)

	var fired bool
	var reading statelog.Reading
	beats := 0
	for at := t0; !fired; at = at.Add(statelog.AlarmInterval) {
		w.observe(ctx, at)
		reading, fired = firing(w)
		if !fired && reading.DanglingBindings != 1 {
			t.Fatalf("beat %d counted %d residue(s), want the one", beats,
				reading.DanglingBindings)
		}
		if fired && reading.DanglingBindingFor <= statelog.StallGrace {
			t.Fatalf("fired at %s, which is not past the %s grace",
				reading.DanglingBindingFor, statelog.StallGrace)
		}
		beats++
		if beats > 10 {
			t.Fatal("ten beats in, a residue past the grace has not fired")
		}
	}
	if want := statelog.StallGrace + statelog.AlarmInterval; reading.DanglingBindingFor != want {
		t.Errorf("fired at an age of %s, want %s — the first beat past the grace",
			reading.DanglingBindingFor, want)
	}
	if reading.DanglingBindingSeat != "triage-bot" {
		t.Errorf("the reading names %q, want triage-bot", reading.DanglingBindingSeat)
	}
}

// A RESIDUE THAT CLEARS STARTS FROM NOTHING when it comes back.
//
// A seat removed, restored and removed again is a new residue the second
// time. Carrying the first one's age across the gap would fire the alarm on
// its first sighting for a state nobody has seen persist.
func TestAResidueThatClearedStartsItsClockAgain(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := dirOf(bound("p1", "triage-bot", 900))
	w := newWatchOver(dir, companyChart())
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	w.observe(ctx, t0)

	dir.set(bound("p1", "platform-lead", 900))
	w.observe(ctx, t0.Add(time.Hour))
	if reading, fired := firing(w); fired || reading.DanglingBindings != 0 {
		t.Fatalf("a repaired binding still reads %d dangling (fired %v)",
			reading.DanglingBindings, fired)
	}

	dir.set(bound("p1", "triage-bot", 900))
	w.observe(ctx, t0.Add(2*time.Hour))
	if _, fired := firing(w); fired {
		t.Error("a residue seen once, after a repair, fired on the age of the " +
			"one before it")
	}
}

// A FIRING ALARM HOLDS THROUGH A READ THAT FAILED AND A CHART THAT STALLED.
//
// Neither says anything about the binding. A failed read of the bindings is
// not an observation at all — the reading stays what the last one found, and
// its age does not grow through an outage nobody saw it through — and a chart
// past the stall grace leaves a residue it cannot judge exactly where it was.
// Reading either as "clear" dropped the alarm on the blip and raised it again a
// beat after it ended: two transitions on every surface, and an `alarm_cleared`
// line for a person still refused on every request.
func TestAFiringAlarmHoldsThroughAFailedReadAndAStalledChart(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := dirOf(bound("p1", "triage-bot", 900))
	chart := companyChart()
	w := newWatchOver(dir, chart)
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	w.observe(ctx, t0)
	w.observe(ctx, t0.Add(2*time.Minute))
	if _, fired := firing(w); !fired {
		t.Fatal("precondition: a two-minute residue does not fire")
	}

	dir.mu.Lock()
	dir.fail = errors.New("estate closed for an adoption")
	dir.at.Seq++
	dir.mu.Unlock()
	w.observe(ctx, t0.Add(3*time.Minute))
	reading, fired := firing(w)
	if !fired {
		t.Fatal("a read that failed cleared a firing alarm")
	}
	if reading.DanglingBindingFor != 2*time.Minute {
		t.Errorf("the residue aged to %s through a read that failed, want the "+
			"two minutes it was last seen at", reading.DanglingBindingFor)
	}

	// THE CHART STALLS for the next beat: the row is unknown, not clear.
	dir.mu.Lock()
	dir.fail = nil
	dir.mu.Unlock()
	w.chart = seatTable{position: 1001, lag: 2 * statelog.StallGrace}
	w.observe(ctx, t0.Add(4*time.Minute))
	if reading, fired := firing(w); !fired || reading.DanglingBindings != 1 {
		t.Fatalf("a chart that could not judge the row cleared it (%d, fired %v)",
			reading.DanglingBindings, fired)
	}

	// AND WHEN BOTH RECOVER, the residue is as old as it always was.
	w.chart = companyChart()
	w.observe(ctx, t0.Add(5*time.Minute))
	reading, fired = firing(w)
	if !fired || reading.DanglingBindingFor != 5*time.Minute {
		t.Errorf("after the outage the residue reads %s (fired %v), want the "+
			"five minutes since it was first found", reading.DanglingBindingFor,
			fired)
	}
}

// A BEAT READS NOTHING THAT HAS NOT MOVED.
//
// The alarm is evaluated every [statelog.AlarmInterval] on every node, so what a
// beat costs has to be bounded by what CHANGED: a beat on which neither the
// identity applier nor the chart applier committed reads nothing at all and
// extends the residues to now; a sign-in — the bulk of the identity log —
// re-reads the bindings and asks the chart about none of them; and only a
// chart that moved, or a binding that did, costs a seat read.
func TestABeatReadsNothingThatHasNotMoved(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := dirOf(bound("p1", "triage-bot", 900), bound("p2", "platform-lead", 900))
	chart := companyChart()
	w := newWatchOver(dir, chart)
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)

	w.observe(ctx, t0)
	if dir.readCount() != 1 || chart.reads.Load() != 2 {
		t.Fatalf("the first beat read the bindings %d time(s) and the chart %d, "+
			"want one read and a seat per binding", dir.readCount(), chart.reads.Load())
	}

	for i := 1; i <= 4; i++ {
		w.observe(ctx, t0.Add(time.Duration(i)*statelog.AlarmInterval))
	}
	if dir.readCount() != 1 || chart.reads.Load() != 2 {
		t.Errorf("four quiet beats read the bindings %d time(s) and the chart %d "+
			"times in all, want nothing past the first beat", dir.readCount(),
			chart.reads.Load())
	}
	if reading, _ := firing(w); reading.DanglingBindingFor != 4*statelog.AlarmInterval {
		t.Errorf("the quiet beats aged the residue to %s, want the four intervals "+
			"it was seen through", reading.DanglingBindingFor)
	}

	dir.touch()
	w.observe(ctx, t0.Add(5*statelog.AlarmInterval))
	if dir.readCount() != 2 || chart.reads.Load() != 2 {
		t.Errorf("a sign-in's beat read the bindings %d time(s) and the chart %d, "+
			"want the bindings re-read and no seat asked about again",
			dir.readCount(), chart.reads.Load())
	}

	w.chart = seatTable{position: 1001, seats: chart.seats, reads: chart.reads}
	w.observe(ctx, t0.Add(6*statelog.AlarmInterval))
	if chart.reads.Load() != 4 {
		t.Errorf("a chart that moved was asked about %d seat(s), want both "+
			"bindings re-classified", chart.reads.Load()-2)
	}
	if reading, fired := firing(w); !fired || reading.DanglingBindings != 1 {
		t.Errorf("after the re-classification the reading is %+v (fired %v)",
			reading, fired)
	}
}

// THE BEAT'S OBSERVATION REACHES THE TABLE THROUGH THE READING EVERY SURFACE
// READS.
//
// One evaluation feeds the gauge, the log line and the screen, so the watch is
// only useful if it lands on [statelog.Reading] where [retention.reading]
// assembles it — a clock kept beside the table and consulted by nobody is the
// silence the capacity alarms had.
func TestTheBindingWatchFeedsTheReadingTheTableEvaluates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	w := newWatchOver(dirOf(bound("p1", "old-lead", 900), bound("p2", "triage-bot", 900)),
		companyChart())
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	w.observe(ctx, t0)
	w.observe(ctx, t0.Add(2*time.Minute))

	r := &retention{state: &stateLog{}, bindings: w}
	reading := r.reading(ctx, t0.Add(2*time.Minute), coord.BackupPoint{}, false)
	if reading.DanglingBindings != 2 {
		t.Fatalf("the reading carries %d dangling binding(s), want 2",
			reading.DanglingBindings)
	}
	var alarm statelog.Alarm
	for _, a := range statelog.Evaluate(reading) {
		if a.Kind == statelog.KindBindingDangling {
			alarm = a
		}
	}
	if alarm.Kind == "" {
		t.Fatalf("two residues two minutes old did not fire: %+v", reading)
	}
	if !strings.Contains(alarm.Detail, "2 person(s)") {
		t.Errorf("the alarm reads %q and does not count both", alarm.Detail)
	}

	// AND A NODE RUNNING NO IDENTITY DOMAIN has no watch, and says nothing
	// rather than reading an empty directory as a clean one.
	none := &retention{state: &stateLog{}}
	if got := none.reading(ctx, t0, coord.BackupPoint{}, false); got.DanglingBindings != 0 {
		t.Errorf("a node with no directory read %d dangling", got.DanglingBindings)
	}
}
