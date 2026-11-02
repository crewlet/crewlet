package objectives

import (
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Recorder is where every observation also goes: the engine's one recorder,
// which an exporter and the operator's 24-hour window read.
type Recorder interface {
	Add(name string, n uint64, attrs metrics.Attrs)
	Observe(name string, d time.Duration, attrs metrics.Attrs)
}

// The outcomes an event is counted under, the closed set of the `outcome`
// attribute of [metrics.ObjectivesEvents].
const (
	OutcomeGood = "good"
	OutcomeBad  = "bad"
)

// BurnWindow is one pair of windows a burn rate is judged over, and the rate
// both must reach.
type BurnWindow struct {
	// Pace names the pair in an alarm's detail: `fast` or `slow`.
	Pace string

	// Long is the window that says the burn is real, Short the one that
	// says it is still happening.
	Long, Short time.Duration

	// Factor is the burn rate, in multiples of the rate that would spend
	// exactly the budget over [Period], that both windows must reach.
	Factor float64
}

// Spends is the fraction of a [Period]'s budget the long window spends at the
// factor: what an operator is told the pair means.
func (w BurnWindow) Spends() float64 { return w.Factor * float64(w.Long) / float64(Period) }

// LongestBurnWindow is the longest window any pair judges over, which is how
// far back the meter's ledger keeps minutes.
const LongestBurnWindow = 6 * time.Hour

// burnWindows are the pairs of the SRE workbook's multi-window alerting, the
// fastest first: an alarm names the fastest pair that fires.
//
// FOURTEEN POINT FOUR over an hour is a fiftieth of the month's budget spent in
// that hour, which leaves a person the rest of the day to act before the
// month is gone; SIX over six hours is a twentieth in six, a burn slow enough
// to read in a working day and fast enough to empty the budget in five. The
// short windows are a twelfth of their long ones, so an alarm clears within
// minutes of the fault ending rather than when the long window has forgotten
// it.
var burnWindows = []BurnWindow{
	{Pace: "fast", Long: time.Hour, Short: 5 * time.Minute, Factor: 14.4},
	{Pace: "slow", Long: LongestBurnWindow, Short: 30 * time.Minute, Factor: 6},
}

// BurnWindows is every pair, the fastest first.
func BurnWindows() []BurnWindow { return slices.Clone(burnWindows) }

// Burn is one objective's burn over one pair of windows.
type Burn struct {
	Objective Name
	Window    BurnWindow

	// LongRate and ShortRate are the budget's burn rate over each window:
	// the bad fraction of its events over the objective's budget. One is
	// spending the budget exactly as fast as the month allows.
	LongRate, ShortRate float64

	// LongBad and LongEvents are the long window's bad events and all of
	// its events.
	LongBad, LongEvents uint64
}

// Firing reports whether the burn alarms: both windows at or past the factor,
// and at least [MinBadEvents] bad events behind the long one.
func (b Burn) Firing() bool {
	return b.LongBad >= MinBadEvents && b.LongRate >= b.Window.Factor && b.ShortRate >= b.Window.Factor
}

// Meter is where every observation of an objective is recorded: classified
// once against its objective, counted into the engine's recorder and into a
// minute ledger the burn rates are read from.
//
// SAFE FOR CONCURRENT USE, and a NIL METER RECORDS NOTHING: a node built with
// no meter (a test, an embedded engine) has recording sites that call it all
// the same.
type Meter struct {
	rec Recorder
	now func() time.Time

	mu      sync.Mutex
	ledgers map[Name]*ledger
}

// NewMeter builds a meter over rec, which may be nil (the observations are
// then counted into the ledger alone), on the clock now (nil is UTC wall time).
func NewMeter(rec Recorder, now func() time.Time) *Meter {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Meter{rec: rec, now: now, ledgers: map[Name]*ledger{}}
}

// Record counts one observation of objective n: how long the event took and
// whether it was answered without fault on this engine's side
// ([Objective.Good]). A name this build does not hold is not counted, like a
// recorder's unknown instrument: an objective with no catalogue entry has no
// bound to judge it against and no documentation to read it by.
func (m *Meter) Record(n Name, elapsed time.Duration, answered bool) {
	if m == nil {
		return
	}
	o, ok := Of(n)
	if !ok {
		return
	}
	good := o.Good(elapsed, answered)
	outcome := OutcomeBad
	if good {
		outcome = OutcomeGood
	}
	if m.rec != nil {
		m.rec.Add(metrics.ObjectivesEvents, 1, metrics.Attrs{"objective": string(n), "outcome": outcome})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.ledgers[n]
	if l == nil {
		l = newLedger()
		m.ledgers[n] = l
	}
	l.add(m.now(), good)
}

// RecordSince counts one observation of objective n whose event began at
// stamp, ending now on the meter's own clock.
//
// FOR AN EVENT STAMPED BY ANOTHER CLOCK — the broker's own instant for a
// stored record, a peer's for a presence member's move — and so CLAMPED AT
// ZERO: a stamp a little after this node's now is the two clocks' skew, never
// an event answered before it began, and read as negative it would be a bad
// event ([Objective.Good]) the engine did nothing to cause. What skew the
// other way adds is inside the bound, which is why every bound measured
// across clocks is one no skew a healthy fleet runs with comes near.
func (m *Meter) RecordSince(n Name, stamp time.Time, answered bool) {
	if m == nil || stamp.IsZero() {
		return
	}
	m.Record(n, max(0, m.now().Sub(stamp)), answered)
}

// Burns is every budgeted objective's burn over every pair of windows, for
// each objective that has recorded an event inside the longest window, in
// catalogue order and the fastest pair first.
func (m *Meter) Burns() []Burn {
	if m == nil {
		return nil
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Burn
	for _, o := range catalogue {
		l := m.ledgers[o.Name]
		if l == nil || !o.Budgeted() {
			continue
		}
		for _, w := range burnWindows {
			longGood, longBad := l.over(now, w.Long)
			if longGood+longBad == 0 {
				continue
			}
			shortGood, shortBad := l.over(now, w.Short)
			out = append(out, Burn{
				Objective: o.Name, Window: w,
				LongRate:  rate(longGood, longBad, o.Budget()),
				ShortRate: rate(shortGood, shortBad, o.Budget()),
				LongBad:   longBad, LongEvents: longGood + longBad,
			})
		}
	}
	return out
}

// Burning is the burns that alarm: for each objective, the FASTEST pair that
// fires and no other, since a burn fast enough to fire the hour's pair is
// also firing the six hours' and is one cause rather than two.
func (m *Meter) Burning() []Burn {
	var out []Burn
	for _, b := range m.Burns() {
		if !b.Firing() {
			continue
		}
		if slices.ContainsFunc(out, func(seen Burn) bool { return seen.Objective == b.Objective }) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// rate is a window's burn rate: its bad fraction over the budget, zero for a
// window with no events.
func rate(good, bad uint64, budget float64) float64 {
	if good+bad == 0 || budget <= 0 {
		return 0
	}
	return float64(bad) / float64(good+bad) / budget
}

// ledgerSlot is the ledger's resolution. A MINUTE: the shortest window judged
// is five minutes, which a minute resolves to within a fifth, and the alarm
// table is evaluated every ten seconds, so a finer slot would re-read the same
// facts.
const ledgerSlot = time.Minute

// ledgerSlots is how many slots the ring keeps: the longest window, plus the
// current partial minute.
const ledgerSlots = int(LongestBurnWindow/ledgerSlot) + 1

// ledger is one objective's good and bad events per minute over the longest
// window, as a ring indexed by the minute's number: a slot is expired by
// comparing its minute rather than by sweeping, so nothing runs on a timer to
// keep it correct.
type ledger struct {
	minute    [ledgerSlots]int64
	good, bad [ledgerSlots]uint64
}

func newLedger() *ledger {
	l := &ledger{}
	for i := range l.minute {
		l.minute[i] = -1
	}
	return l
}

// add counts one event in the minute at.
func (l *ledger) add(at time.Time, good bool) {
	minute := at.Unix() / int64(ledgerSlot/time.Second)
	i := int(minute % int64(ledgerSlots))
	if l.minute[i] != minute {
		if l.minute[i] > minute {
			// THE SLOT HOLDS A LATER MINUTE, which only a clock that went
			// back produces: the later minute is the one inside the window.
			return
		}
		l.minute[i], l.good[i], l.bad[i] = minute, 0, 0
	}
	if good {
		l.good[i]++
	} else {
		l.bad[i]++
	}
}

// over sums the events of the last w, the current partial minute included:
// the w/[ledgerSlot] most recent minutes.
func (l *ledger) over(now time.Time, w time.Duration) (good, bad uint64) {
	current := now.Unix() / int64(ledgerSlot/time.Second)
	span := int64(w / ledgerSlot)
	for i, minute := range l.minute {
		if minute < 0 || minute > current || current-minute >= span {
			continue
		}
		good += l.good[i]
		bad += l.bad[i]
	}
	return good, bad
}
