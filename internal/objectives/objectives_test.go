package objectives_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 18, 9, 0, 30, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recorded is a recorder that keeps what it was handed.
type recorded struct {
	mu       sync.Mutex
	adds     map[string]uint64
	observed map[string][]time.Duration
}

func newRecorded() *recorded {
	return &recorded{adds: map[string]uint64{}, observed: map[string][]time.Duration{}}
}

func key(name string, attrs metrics.Attrs) string {
	parts := []string{name}
	for _, k := range []string{"objective", "outcome"} {
		if v, ok := attrs[k]; ok {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

func (r *recorded) Add(name string, n uint64, attrs metrics.Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adds[key(name, attrs)] += n
}

func (r *recorded) Observe(name string, d time.Duration, attrs metrics.Attrs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed[key(name, attrs)] = append(r.observed[key(name, attrs)], d)
}

// EVERY OBJECTIVE IS ONE THE ALARM CAN JUDGE: a unique valid name, a bound, a
// reason, a target that is a fraction or best effort, and a budget no pair's
// factor overshoots (a factor times the budget past one is a bad fraction no
// window can reach, so that burn could never fire).
func TestEveryObjectiveIsOneTheAlarmCanJudge(t *testing.T) {
	t.Parallel()
	seen := map[objectives.Name]bool{}
	all := objectives.All()
	if len(all) == 0 {
		t.Fatal("the catalogue is empty, so this asserts nothing")
	}
	for _, o := range all {
		switch {
		case seen[o.Name]:
			t.Errorf("%s is catalogued twice", o.Name)
		case !o.Name.Valid():
			t.Errorf("%s does not report itself valid", o.Name)
		case o.Within <= 0:
			t.Errorf("%s has no bound", o.Name)
		case o.Reason == "" || o.Measures == "" || o.Where == "" || o.GoodEvent == "":
			t.Errorf("%s does not say what it measures, where, what is good and why", o.Name)
		case o.Target < 0 || o.Target >= 1:
			t.Errorf("%s targets %v, which is not a fraction below one", o.Name, o.Target)
		}
		seen[o.Name] = true
		for _, w := range objectives.BurnWindows() {
			if o.Budgeted() && w.Factor*o.Budget() > 1 {
				t.Errorf("%s's budget %v times the %s factor %v is past one: that burn can "+
					"never fire", o.Name, o.Budget(), w.Pace, w.Factor)
			}
		}
	}
	if objectives.Name("no_such_objective").Valid() {
		t.Error("an objective nobody catalogued reports itself valid")
	}
	for _, n := range []objectives.Name{objectives.PresenceJoin, objectives.PresenceCursor} {
		o, _ := objectives.Of(n)
		if o.Budgeted() || o.Budget() != 0 {
			t.Errorf("%s is best effort and carries a budget", n)
		}
	}
}

// THE BOUNDS ARE THE PLAN'S, each with its target, so a retune is a reviewed
// change here and not a drift somewhere a recording site spells its own.
func TestTheObjectivesAreTheDecidedOnes(t *testing.T) {
	t.Parallel()
	want := map[objectives.Name]struct {
		within time.Duration
		target float64
	}{
		objectives.PublishSuccess:   {30 * time.Second, 0.999},
		objectives.PublishApplied:   {2 * time.Second, 0.95},
		objectives.ViewFirstByte:    {300 * time.Millisecond, 0.999},
		objectives.ArtifactReady:    {3 * time.Second, 0.995},
		objectives.DataRoundTrip:    {250 * time.Millisecond, 0.95},
		objectives.DataResolved:     {5 * time.Second, 0.999},
		objectives.ChangeVisibility: {time.Second, 0.99},
		objectives.WakeLatency:      {10 * time.Second, 0.95},
		objectives.PresenceJoin:     {time.Second, 0},
		objectives.PresenceCursor:   {150 * time.Millisecond, 0},
	}
	if len(objectives.All()) != len(want) {
		t.Errorf("%d objectives, want %d", len(objectives.All()), len(want))
	}
	for n, w := range want {
		o, ok := objectives.Of(n)
		if !ok {
			t.Errorf("%s is not catalogued", n)
			continue
		}
		if o.Within != w.within || o.Target != w.target {
			t.Errorf("%s: within %s at %v, want %s at %v", n, o.Within, o.Target, w.within, w.target)
		}
	}
	if objectives.Period != 30*24*time.Hour {
		t.Errorf("the period is %s, want thirty days", objectives.Period)
	}
}

// A GOOD EVENT IS ANSWERED WITHOUT FAULT AND WITHIN ITS BOUND, both halves.
func TestAGoodEventIsAnsweredWithinItsBound(t *testing.T) {
	t.Parallel()
	o, _ := objectives.Of(objectives.ViewFirstByte)
	for _, c := range []struct {
		elapsed  time.Duration
		answered bool
		good     bool
	}{
		{0, true, true},
		{300 * time.Millisecond, true, true},
		{300*time.Millisecond + time.Nanosecond, true, false},
		{10 * time.Millisecond, false, false},
		{-time.Millisecond, true, false},
	} {
		if got := o.Good(c.elapsed, c.answered); got != c.good {
			t.Errorf("%s answered %v: good %v, want %v", c.elapsed, c.answered, got, c.good)
		}
	}
}

// THE PAIRS ARE THE WORKBOOK'S, and what each spends is what the alarm's
// remedy tells an operator: a fiftieth in an hour, a twentieth in six.
func TestTheBurnWindowsSpendWhatTheAlarmSays(t *testing.T) {
	t.Parallel()
	w := objectives.BurnWindows()
	if len(w) != 2 {
		t.Fatalf("%d pairs, want the fast and the slow", len(w))
	}
	for _, c := range []struct {
		pace        string
		long, short time.Duration
		factor      float64
		spends      float64
	}{
		{"fast", time.Hour, 5 * time.Minute, 14.4, 0.02},
		{"slow", 6 * time.Hour, 30 * time.Minute, 6, 0.05},
	} {
		var got objectives.BurnWindow
		for _, x := range w {
			if x.Pace == c.pace {
				got = x
			}
		}
		if got.Long != c.long || got.Short != c.short || got.Factor != c.factor {
			t.Errorf("%s: %+v, want %s and %s at %v", c.pace, got, c.long, c.short, c.factor)
		}
		if diff := got.Spends() - c.spends; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s spends %v of the month, want %v", c.pace, got.Spends(), c.spends)
		}
		if got.Long > objectives.LongestBurnWindow {
			t.Errorf("%s judges %s, past the %s the ledger keeps", c.pace, got.Long,
				objectives.LongestBurnWindow)
		}
	}
	if w[0].Pace != "fast" {
		t.Errorf("the first pair is %s: an alarm names the fastest pair that fires, so it "+
			"must come first", w[0].Pace)
	}
}

// record adds n events of objective name, each good or bad by its elapsed.
func record(m *objectives.Meter, name objectives.Name, n int, answered bool) {
	for range n {
		m.Record(name, time.Millisecond, answered)
	}
}

// A BURN FIRES ONLY WHEN BOTH WINDOWS BURN: the long window says it is real,
// the short one that it is still happening.
func TestABurnFiresOnlyWhenBothWindowsBurn(t *testing.T) {
	t.Parallel()

	t.Run("a burn in both windows fires the fast pair, once", func(t *testing.T) {
		t.Parallel()
		c := newClock()
		m := objectives.NewMeter(nil, c.now)
		// 5% bad on a 99.9% objective is a 50x burn in every window.
		record(m, objectives.PublishSuccess, 95, true)
		record(m, objectives.PublishSuccess, 5, false)
		got := m.Burning()
		if len(got) != 1 {
			t.Fatalf("burning %+v, want the one objective once", got)
		}
		b := got[0]
		if b.Objective != objectives.PublishSuccess || b.Window.Pace != "fast" {
			t.Errorf("burning %s over the %s pair, want publish_success over fast", b.Objective,
				b.Window.Pace)
		}
		if b.LongRate < 49.9 || b.LongRate > 50.1 || b.LongBad != 5 || b.LongEvents != 100 {
			t.Errorf("burn %+v, want a 50x rate over 5 bad of 100", b)
		}
		if len(m.Burns()) != 2 {
			t.Errorf("%d burns, want one per pair", len(m.Burns()))
		}
	})

	t.Run("a burn that stopped clears with the short window", func(t *testing.T) {
		t.Parallel()
		c := newClock()
		m := objectives.NewMeter(nil, c.now)
		record(m, objectives.ViewFirstByte, 90, true)
		record(m, objectives.ViewFirstByte, 10, false)
		if len(m.Burning()) != 1 {
			t.Fatal("the burn did not fire to begin with")
		}
		// Ten healthy minutes later the hour still holds the failures, and
		// the five minutes do not.
		c.advance(10 * time.Minute)
		record(m, objectives.ViewFirstByte, 100, true)
		for _, b := range m.Burns() {
			if b.Window.Pace == "fast" && b.LongRate < b.Window.Factor {
				t.Errorf("the hour forgot the failures: %+v", b)
			}
		}
		if got := m.Burning(); len(got) != 1 || got[0].Window.Pace != "slow" {
			t.Errorf("burning %+v, want only the slow pair, whose thirty minutes still hold "+
				"the failures", got)
		}
		c.advance(25 * time.Minute)
		record(m, objectives.ViewFirstByte, 100, true)
		if got := m.Burning(); len(got) != 0 {
			t.Errorf("burning %+v, want nothing once both short windows are healthy", got)
		}
	})

	t.Run("one failure is a fault report, not a rate", func(t *testing.T) {
		t.Parallel()
		c := newClock()
		m := objectives.NewMeter(nil, c.now)
		record(m, objectives.ArtifactReady, 9, true)
		record(m, objectives.ArtifactReady, 1, false)
		if got := m.Burning(); len(got) != 0 {
			t.Errorf("one bad event among ten fired %+v", got)
		}
		record(m, objectives.ArtifactReady, 1, false)
		if got := m.Burning(); len(got) != 1 {
			t.Errorf("two bad events among eleven fired %+v, want the burn", got)
		}
	})

	t.Run("a slow burn fires only the slow pair", func(t *testing.T) {
		t.Parallel()
		c := newClock()
		m := objectives.NewMeter(nil, c.now)
		// 1% bad on a 99.9% objective is a 10x burn: past six, short of 14.4.
		record(m, objectives.DataResolved, 990, true)
		record(m, objectives.DataResolved, 10, false)
		got := m.Burning()
		if len(got) != 1 || got[0].Window.Pace != "slow" {
			t.Errorf("burning %+v, want the slow pair alone", got)
		}
	})

	t.Run("a best-effort indicator never burns", func(t *testing.T) {
		t.Parallel()
		c := newClock()
		m := objectives.NewMeter(nil, c.now)
		record(m, objectives.PresenceCursor, 10, false)
		if got := m.Burns(); len(got) != 0 {
			t.Errorf("presence burned %+v", got)
		}
	})
}

// THE LEDGER FORGETS PAST THE LONGEST WINDOW, and an hour that has gone is not
// counted in the hour that follows.
func TestTheLedgerForgetsPastTheLongestWindow(t *testing.T) {
	t.Parallel()
	c := newClock()
	m := objectives.NewMeter(nil, c.now)
	record(m, objectives.PublishSuccess, 10, false)
	c.advance(objectives.LongestBurnWindow + time.Minute)
	if got := m.Burns(); len(got) != 0 {
		t.Errorf("failures older than the longest window are still counted: %+v", got)
	}
	record(m, objectives.PublishSuccess, 10, true)
	for _, b := range m.Burns() {
		if b.LongBad != 0 || b.LongEvents != 10 {
			t.Errorf("a reused slot kept its old minute's events: %+v", b)
		}
	}
}

// ONE CALL, TWO VIEWS: the recorder's counter by objective and outcome, and the
// ledger the burns read.
func TestARecordedEventReachesTheRecorder(t *testing.T) {
	t.Parallel()
	rec := newRecorded()
	m := objectives.NewMeter(rec, newClock().now)
	m.Record(objectives.DataRoundTrip, 100*time.Millisecond, true)
	m.Record(objectives.DataRoundTrip, 300*time.Millisecond, true)
	m.Record(objectives.DataRoundTrip, time.Millisecond, false)
	m.Record(objectives.Name("no_such_objective"), time.Millisecond, true)
	good := rec.adds[key(metrics.ObjectivesEvents, metrics.Attrs{"objective": "data_round_trip", "outcome": "good"})]
	bad := rec.adds[key(metrics.ObjectivesEvents, metrics.Attrs{"objective": "data_round_trip", "outcome": "bad"})]
	if good != 1 || bad != 2 {
		t.Errorf("good %d bad %d, want 1 and 2", good, bad)
	}
	if len(rec.adds) != 2 {
		t.Errorf("recorded %v: an uncatalogued objective was counted", rec.adds)
	}
	var nilMeter *objectives.Meter
	nilMeter.Record(objectives.DataRoundTrip, 0, true)
	if nilMeter.Burns() != nil || nilMeter.Burning() != nil {
		t.Error("a nil meter answered burns")
	}
}

// AN EVENT STAMPED BY ANOTHER CLOCK IS MEASURED ON THIS ONE, AND SKEW IS NOT A
// FAULT.
//
// The broker stamps a stored record and a peer stamps its member's move, and
// the meter ends the event on its own clock. A stamp a little ahead of that
// clock is the two clocks' skew: counted as negative it would be a bad event
// the engine did nothing to cause, so it is zero — good — while a stamp behind
// by more than the bound is late, whichever clock is to blame. A zero stamp
// names no event and counts none.
//
// Mutation: drop the clamp, and the skewed event is bad.
func TestAnEventStampedByAnotherClockIsMeasuredOnThisOne(t *testing.T) {
	t.Parallel()
	rec := newRecorded()
	c := newClock()
	m := objectives.NewMeter(rec, c.now)
	m.RecordSince(objectives.ChangeVisibility, c.now().Add(-400*time.Millisecond), true)
	m.RecordSince(objectives.ChangeVisibility, c.now().Add(50*time.Millisecond), true)
	m.RecordSince(objectives.ChangeVisibility, c.now().Add(-2*time.Second), true)
	m.RecordSince(objectives.ChangeVisibility, c.now(), false)
	m.RecordSince(objectives.ChangeVisibility, time.Time{}, true)
	good := rec.adds[key(metrics.ObjectivesEvents,
		metrics.Attrs{"objective": "change_visibility", "outcome": "good"})]
	bad := rec.adds[key(metrics.ObjectivesEvents,
		metrics.Attrs{"objective": "change_visibility", "outcome": "bad"})]
	if good != 2 || bad != 2 {
		t.Errorf("good %d bad %d, want the in-bound and the skewed event good and the late "+
			"and the faulted bad, with the unstamped one not counted", good, bad)
	}
	var nilMeter *objectives.Meter
	nilMeter.RecordSince(objectives.ChangeVisibility, c.now(), true)
}

// THE REFERENCE PAGE SAYS WHAT THE CODE JUDGES: the table in
// docs/reference/service-objectives.md is the catalogue's own rendering.
func TestTheReferencePageCarriesTheCatalogue(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "service-objectives.md"))
	if err != nil {
		t.Fatal(err)
	}
	table := objectives.Reference()
	if !strings.Contains(string(page), table) {
		t.Errorf("docs/reference/service-objectives.md does not carry the catalogue's table; "+
			"replace its table with:\n%s", table)
	}
	if !strings.Contains(table, "| `artifact_ready` |") || !strings.Contains(table, "| 99.5% |") {
		t.Errorf("control: the rendering is missing what it must hold:\n%s", table)
	}
}
