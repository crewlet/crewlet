package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// seatTable is a [session.Chart] answering from a table.
type seatTable struct {
	position uint64
	lag      time.Duration
	posErr   error
	seats    map[string]session.Seat
	seatErr  error
}

func (c seatTable) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
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
	return seatTable{position: 1000, seats: map[string]session.Seat{
		"platform-lead": {Handle: "platform-lead", Kind: session.SeatKindHuman},
		"triage-bot":    {Handle: "triage-bot", Kind: "agent"},
		"old-lead":      {Handle: "old-lead", Tombstoned: true},
	}}
}

// bound is a person row bound to a seat, decided at a chart position.
func bound(id, seat string, at uint64) iamdomain.PersonRow {
	return iamdomain.PersonRow{
		ID: id, Login: id + ".person", Kind: iam.KindPerson,
		Stage: iam.StageActive, Seat: seat, SeatAt: at,
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
		row      iamdomain.PersonRow
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

// A SHREDDED PERSON'S SEAT BINDS NOBODY.
//
// A removal keeps the row so the audit trail resolves, and a person who can
// never act again is not somebody an administrator needs to unbind.
func TestAShreddedPersonsBindingIsNotAResidue(t *testing.T) {
	t.Parallel()
	row := bound("p1", "old-lead", 900)
	row.Shredded = true
	if _, dangling, err := danglingBinding(t.Context(), companyChart(), row); err != nil || dangling {
		t.Errorf("a shredded row's binding: dangling=%v err=%v", dangling, err)
	}
}

// pagedDirectory answers the directory a page at a time.
type pagedDirectory struct {
	pages [][]iamdomain.PersonRow
	fail  error
	asked int
}

func (d *pagedDirectory) People(_ context.Context, q iamdomain.PeopleQuery) (
	iamdomain.PeoplePage, error) {

	d.asked++
	if d.fail != nil {
		return iamdomain.PeoplePage{}, d.fail
	}
	index := 0
	if q.After != "" {
		for i, page := range d.pages {
			if len(page) > 0 && page[len(page)-1].ID == q.After {
				index = i + 1
			}
		}
	}
	out := iamdomain.PeoplePage{People: d.pages[index]}
	if index < len(d.pages)-1 {
		out.Next = d.pages[index][len(d.pages[index])-1].ID
	}
	return out, nil
}

// THE WALK READS EVERY PAGE, because a residue on the second page of the
// directory is as dangling as one on the first — and a walk that stopped at
// the first page would also CLEAR every residue after it, resetting clocks a
// flaky read never had any business touching.
func TestTheWalkClassifiesEveryPageOfTheDirectory(t *testing.T) {
	t.Parallel()
	dir := &pagedDirectory{pages: [][]iamdomain.PersonRow{
		{bound("p1", "platform-lead", 900), bound("p2", "triage-bot", 900)},
		{bound("p3", "old-lead", 900)},
	}}
	sighting, err := sightBindings(t.Context(), dir, companyChart())
	if err != nil {
		t.Fatalf("sightBindings: %v", err)
	}
	var seats []string
	for _, r := range sighting.residues {
		seats = append(seats, r.Seat)
	}
	if strings.Join(seats, ",") != "triage-bot,old-lead" {
		t.Errorf("the walk found %v, want both residues across both pages", seats)
	}
	if dir.asked != 2 {
		t.Errorf("the directory was asked %d time(s) for two pages", dir.asked)
	}
}

// watchFor is a watch over a directory whose one person is bound to a seat
// the chart does not hold as a human one.
func watchFor(dir peopleLister, chart session.Chart) *bindingWatch {
	return &bindingWatch{dir: dir, chart: chart, first: map[string]time.Time{}}
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
// built around.
//
// A bind racing a removal, or a hire this node's chart applier reaches a few
// seconds late, is a dangling binding for moments. The alarm fires once a
// residue has been FOUND by this node's own walks for longer than the stall
// grace, measured from the first walk that found it to the latest — and not a
// moment before, so nobody is paged for a state that was already clearing.
func TestAResidueYoungerThanTheGraceDoesNotFire(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := &pagedDirectory{pages: [][]iamdomain.PersonRow{
		{bound("p1", "triage-bot", 900)},
	}}
	w := watchFor(dir, companyChart())
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)

	w.observe(ctx, t0)
	if reading, fired := firing(w); fired || reading.DanglingBindings != 1 {
		t.Fatalf("first sighting: fired=%v count=%d, want counted and silent",
			fired, reading.DanglingBindings)
	}
	w.observe(ctx, t0.Add(statelog.StallGrace/2))
	if _, fired := firing(w); fired {
		t.Fatal("a residue half the grace old fired")
	}
	w.observe(ctx, t0.Add(statelog.StallGrace))
	if _, fired := firing(w); fired {
		t.Fatal("a residue exactly the grace old fired")
	}
	w.observe(ctx, t0.Add(statelog.StallGrace+time.Second))
	reading, fired := firing(w)
	if !fired {
		t.Fatalf("a residue past the grace did not fire: %+v", reading)
	}
	if reading.DanglingBindingSeat != "triage-bot" ||
		reading.DanglingBindingFor != statelog.StallGrace+time.Second {
		t.Errorf("the reading says %q for %s, want triage-bot for the time "+
			"between the first walk and the latest", reading.DanglingBindingSeat,
			reading.DanglingBindingFor)
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
	dir := &pagedDirectory{pages: [][]iamdomain.PersonRow{
		{bound("p1", "triage-bot", 900)},
	}}
	w := watchFor(dir, companyChart())
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	w.observe(ctx, t0)

	dir.pages[0][0].Seat = "platform-lead"
	w.observe(ctx, t0.Add(time.Hour))
	if reading, fired := firing(w); fired || reading.DanglingBindings != 0 {
		t.Fatalf("a repaired binding still reads %d dangling (fired %v)",
			reading.DanglingBindings, fired)
	}

	dir.pages[0][0].Seat = "triage-bot"
	w.observe(ctx, t0.Add(2*time.Hour))
	if _, fired := firing(w); fired {
		t.Error("a residue seen once, after a repair, fired on the age of the " +
			"one before it")
	}
}

// A WALK THAT CANNOT READ SAYS NOTHING AND FORGETS NOTHING.
//
// An unreadable directory is not a clean one, so the reading carries no
// residue while it lasts — and it is not a repaired one either, so the clocks
// are kept: a binding that has dangled for an hour does not have to wait out
// the grace again because the estate blinked. A chart stall is the same for
// one row: neither dangling nor clear.
func TestAnUnreadableWalkNeitherClearsNorAgesAResidue(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := &pagedDirectory{pages: [][]iamdomain.PersonRow{
		{bound("p1", "triage-bot", 900)},
	}}
	w := watchFor(dir, companyChart())
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	w.observe(ctx, t0)

	dir.fail = errors.New("estate closed for an adoption")
	w.observe(ctx, t0.Add(10*time.Minute))
	if reading, fired := firing(w); fired || reading.DanglingBindings != 0 {
		t.Fatalf("a walk that could not read reported %d dangling (fired %v)",
			reading.DanglingBindings, fired)
	}

	// THE CHART STALLS for the next walk: the row is unknown, not clear.
	dir.fail = nil
	w.chart = seatTable{position: 1000, lag: 2 * statelog.StallGrace}
	w.observe(ctx, t0.Add(20*time.Minute))
	if reading, fired := firing(w); fired || reading.DanglingBindings != 0 {
		t.Fatalf("a row the chart could not judge was counted (%d, fired %v)",
			reading.DanglingBindings, fired)
	}

	// AND WHEN BOTH RECOVER, the residue is as old as it always was.
	w.chart = companyChart()
	w.observe(ctx, t0.Add(30*time.Minute))
	reading, fired := firing(w)
	if !fired || reading.DanglingBindingFor != 30*time.Minute {
		t.Errorf("after the outage the residue reads %s (fired %v), want the "+
			"thirty minutes since it was first found", reading.DanglingBindingFor,
			fired)
	}
}

// THE TICK'S WALK REACHES THE TABLE THROUGH THE READING EVERY SURFACE READS.
//
// One evaluation feeds the gauge, the log line and the screen, so the walk is
// only useful if it lands on [statelog.Reading] where [retention.reading]
// assembles it — a clock kept beside the table and consulted by nobody is the
// silence the capacity alarms had.
func TestTheBindingWatchFeedsTheReadingTheTableEvaluates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := &pagedDirectory{pages: [][]iamdomain.PersonRow{
		{bound("p1", "old-lead", 900), bound("p2", "triage-bot", 900)},
	}}
	w := watchFor(dir, companyChart())
	t0 := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC)
	w.observe(ctx, t0)
	w.observe(ctx, t0.Add(RetentionInterval))

	r := &retention{state: &stateLog{}, bindings: w}
	reading := r.reading(ctx, t0.Add(RetentionInterval), coord.BackupPoint{}, false)
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
		t.Fatalf("two residues fifteen minutes old did not fire: %+v", reading)
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
