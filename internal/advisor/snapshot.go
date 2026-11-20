package advisor

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE COMPANY'S NUMBERS OVER A RANGE, as the Advisor's page draws them
// (ADR-0166): a strip of stats, each beside the range before it, and charts
// the dashboard draws from series, every one computed here from the records
// the detectors read, never estimated. A source that could not be read is
// named and its numbers left out, never drawn as nothing.

// SnapshotRanges are the ranges a snapshot is read over, in days.
var SnapshotRanges = []int{7, 14, 30}

// Snapshot is the company's numbers over one range.
type Snapshot struct {
	AsOf time.Time `json:"as_of"`
	// RunID is the run the snapshot was kept by, nil when computed on read.
	RunID  *string       `json:"run_id"`
	Range  SnapshotRange `json:"range"`
	Stats  []Stat        `json:"stats"`
	Charts []Chart       `json:"charts"`
	Unread []string      `json:"unread"`
}

// SnapshotRange is the span a snapshot covers: whole company days.
type SnapshotRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Days int       `json:"days"`
}

// Stat is one number of a range, beside the range before's where it was read.
type Stat struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Value float64  `json:"value"`
	Unit  Unit     `json:"unit"`
	Was   *float64 `json:"was"`
	// Better is the direction that is good, "up" or "down", nil where
	// neither is.
	Better *string `json:"better"`
}

// Chart is one chart the dashboard draws: series of values over the labels
// of one axis.
type Chart struct {
	ID     string        `json:"id"`
	Title  string        `json:"title"`
	Unit   Unit          `json:"unit"`
	Kind   ChartKind     `json:"kind"`
	X      ChartAxis     `json:"x"`
	Series []ChartSeries `json:"series"`
	// Note is one line under the chart, nil where it says nothing more.
	Note *string `json:"note"`
}

// ChartKind is how a chart is drawn: columns across, stacked columns, or bars
// down for long labels.
type ChartKind string

// The kinds.
const (
	ChartColumns        ChartKind = "columns"
	ChartStackedColumns ChartKind = "stacked_columns"
	ChartBars           ChartKind = "bars"
)

// ChartAxis is the axis a chart's series run along: what its points are, each
// one's stable key and its label.
type ChartAxis struct {
	Kind   AxisKind `json:"kind"`
	Keys   []string `json:"keys"`
	Labels []string `json:"labels"`
}

// AxisKind is what an axis's points are.
type AxisKind string

// The axis kinds.
const (
	AxisDay    AxisKind = "day"
	AxisSeat   AxisKind = "seat"
	AxisEntry  AxisKind = "entry"
	AxisReason AxisKind = "reason"
	AxisState  AxisKind = "state"
)

// ChartSeries is one series: a value per point of the axis, nil where none.
type ChartSeries struct {
	ID     string     `json:"id"`
	Label  string     `json:"label"`
	Values []*float64 `json:"values"`
}

// SnapshotInputs is what one snapshot is computed from: each source over the
// range and over the range before it, and whether it was read.
type SnapshotInputs struct {
	Range SnapshotRange
	// Ledger is the turn ledger over the range, and Before over the range
	// before; LedgerUnread where it could not be read.
	Ledger, LedgerBefore []usage.TurnLedgerRow
	LedgerUnread         bool
	// Flow and Pickups are the flow record's hours and pickup waits over the
	// range and the range before; FlowKept false for a company whose tracker
	// keeps no flow record, FlowUnread where it could not be read.
	Flow, FlowBefore       []worklet.FlowHours
	Pickups, PickupsBefore []time.Duration
	FlowKept, FlowUnread   bool
	// Open is every holder's open items now; WorkUnread where unread.
	Open       []worklet.HeldOpen
	WorkUnread bool
	// Names is each seat's name by handle.
	Names map[string]string
}

// betterDown is a stat that is better the lower it reads: a function, so the
// engine allocates nothing for it at init.
func betterDown() *string { return ptr("down") }

// ComputeSnapshot is the snapshot in, at asOf.
func ComputeSnapshot(in SnapshotInputs, asOf time.Time) Snapshot {
	out := Snapshot{AsOf: asOf, Range: in.Range, Stats: []Stat{}, Charts: []Chart{}, Unread: []string{}}
	name := func(handle string) string {
		if n := in.Names[handle]; n != "" {
			return n
		}
		return handle
	}
	if in.WorkUnread {
		out.Unread = append(out.Unread, UnreadWork)
	} else {
		open := 0
		for _, h := range in.Open {
			open += h.Open
		}
		out.Stats = append(out.Stats, Stat{Key: "open_items", Label: "Open items", Value: float64(open),
			Unit: UnitItems, Better: betterDown()})
		if c, ok := openWorkChart(in.Open, name); ok {
			out.Charts = append(out.Charts, c)
		}
	}
	switch {
	case in.FlowUnread:
		out.Unread = append(out.Unread, UnreadFlow)
	case in.FlowKept:
		now, before := flowTotals(in.Flow), flowTotals(in.FlowBefore)
		for _, s := range []struct {
			key, label string
			state      worklet.FlowState
		}{
			{"wait_hours", "Hours work waited for a seat", worklet.FlowDue},
			{"held_hours", "Hours work was held", worklet.FlowHeld},
			{"blocked_hours", "Hours work stood blocked", worklet.FlowBlocked},
		} {
			out.Stats = append(out.Stats, Stat{Key: s.key, Label: s.label, Value: hoursOf(now[s.state]),
				Unit: UnitHours, Was: ptr(hoursOf(before[s.state])), Better: betterDown()})
		}
		if len(in.Pickups) > 0 {
			stat := Stat{Key: "pickup_wait_minutes", Label: "Pickup wait, median",
				Value: minutesOf(percentile(in.Pickups, 50)), Unit: UnitMinutes, Better: betterDown()}
			if len(in.PickupsBefore) > 0 {
				stat.Was = ptr(minutesOf(percentile(in.PickupsBefore, 50)))
			}
			out.Stats = append(out.Stats, stat)
		}
		if c, ok := flowBySeatChart(in.Flow, name); ok {
			out.Charts = append(out.Charts, c)
		}
	}
	if in.LedgerUnread {
		out.Unread = append(out.Unread, UnreadLedger)
		return out
	}
	now, before := ledgerTotals(in.Ledger), ledgerTotals(in.LedgerBefore)
	out.Stats = append(out.Stats,
		Stat{Key: "turns", Label: "Turns", Value: float64(now.turns), Unit: UnitTurns,
			Was: ptr(float64(before.turns))},
		Stat{Key: "wasted_turns_percent", Label: "Turns that changed nothing", Value: now.wastedPercent(),
			Unit: UnitPercent, Was: before.wastedPercentOrNil(), Better: betterDown()},
		Stat{Key: "tokens_per_turn", Label: "Tokens per turn", Value: now.perTurn(), Unit: UnitTokens,
			Was: before.perTurnOrNil(), Better: betterDown()})
	if c, ok := wastedWakesChart(in.Ledger); ok {
		out.Charts = append(out.Charts, c)
	}
	if c, ok := tokensPerTurnChart(in.Ledger, name); ok {
		out.Charts = append(out.Charts, c)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// hoursOf is d in hours to one decimal, minutesOf in whole minutes.
func hoursOf(d time.Duration) float64   { return math.Round(d.Hours()*10) / 10 }
func minutesOf(d time.Duration) float64 { return math.Round(d.Minutes()) }

// flowTotals is the hours every seat's work stood in each state.
func flowTotals(hours []worklet.FlowHours) map[worklet.FlowState]time.Duration {
	out := map[worklet.FlowState]time.Duration{}
	for _, h := range hours {
		out[h.State] += h.Stood
	}
	return out
}

// ledgerSum is the started turns of a ledger, what they spent, and how many
// changed nothing.
type ledgerSum struct{ turns, nothing, tokens int64 }

func ledgerTotals(rows []usage.TurnLedgerRow) ledgerSum {
	var out ledgerSum
	for _, r := range rows {
		if types.WakeDisposition(r.Disposition) != types.DispositionStarted {
			continue
		}
		out.turns += r.Turns
		out.tokens += r.Tokens()
		if types.TurnOutcome(r.Outcome) == types.OutcomeNothing {
			out.nothing += r.Turns
		}
	}
	return out
}

func (s ledgerSum) wastedPercent() float64 {
	if s.turns == 0 {
		return 0
	}
	return math.Round(float64(s.nothing) * 100 / float64(s.turns))
}

func (s ledgerSum) wastedPercentOrNil() *float64 {
	if s.turns == 0 {
		return nil
	}
	return ptr(s.wastedPercent())
}

func (s ledgerSum) perTurn() float64 {
	if s.turns == 0 {
		return 0
	}
	return math.Round(float64(s.tokens) / float64(s.turns))
}

func (s ledgerSum) perTurnOrNil() *float64 {
	if s.turns == 0 {
		return nil
	}
	return ptr(s.perTurn())
}

// flowStates are the states a flow chart stacks, in the order drawn: those the
// flow record keeps (working is the turns' own record).
var flowStates = []struct {
	state worklet.FlowState
	label string
}{
	{worklet.FlowDue, "Waited for its seat"},
	{worklet.FlowHeld, "Held"},
	{worklet.FlowBlocked, "Blocked"},
	{worklet.FlowWaiting, "Waiting"},
	{worklet.FlowStuck, "Stuck"},
	{worklet.FlowMoving, "Moving"},
}

// flowBySeatChart is the hours each seat's work stood in each state, the
// seats with the most hours not moving first.
func flowBySeatChart(hours []worklet.FlowHours, name func(string) string) (Chart, bool) {
	by := map[string]map[worklet.FlowState]time.Duration{}
	for _, h := range hours {
		if h.Seat == "" {
			continue
		}
		if by[h.Seat] == nil {
			by[h.Seat] = map[worklet.FlowState]time.Duration{}
		}
		by[h.Seat][h.State] += h.Stood
	}
	if len(by) == 0 {
		return Chart{}, false
	}
	stalled := func(seat string) time.Duration {
		return by[seat][worklet.FlowDue] + by[seat][worklet.FlowHeld] + by[seat][worklet.FlowBlocked] +
			by[seat][worklet.FlowStuck]
	}
	seats := keysBy(by, func(a, b string) int { return cmp.Or(cmp.Compare(stalled(b), stalled(a)), cmp.Compare(a, b)) })
	c := Chart{ID: "flow_by_seat", Title: "Where each seat's work stood", Unit: UnitHours, Kind: ChartStackedColumns,
		X: seatAxis(seats, name)}
	for _, s := range flowStates {
		series := ChartSeries{ID: string(s.state), Label: s.label}
		drawn := false
		for _, seat := range seats {
			v := hoursOf(by[seat][s.state])
			drawn = drawn || v > 0
			series.Values = append(series.Values, ptr(v))
		}
		if drawn {
			c.Series = append(c.Series, series)
		}
	}
	return c, len(c.Series) > 0
}

// openWorkChart is each holder's open items, the pressing ones (urgent or high
// priority) apart, the most held first.
func openWorkChart(open []worklet.HeldOpen, name func(string) string) (Chart, bool) {
	held := slices.DeleteFunc(slices.Clone(open), func(h worklet.HeldOpen) bool { return h.Handle == "" || h.Open == 0 })
	if len(held) == 0 {
		return Chart{}, false
	}
	slices.SortStableFunc(held, func(a, b worklet.HeldOpen) int {
		return cmp.Or(cmp.Compare(b.Open, a.Open), cmp.Compare(a.Handle, b.Handle))
	})
	seats := make([]string, len(held))
	pressing := ChartSeries{ID: "pressing", Label: "Urgent or high priority"}
	other := ChartSeries{ID: "other", Label: "Other"}
	for i, h := range held {
		seats[i] = h.Handle
		pressing.Values = append(pressing.Values, ptr(float64(h.Pressing)))
		other.Values = append(other.Values, ptr(float64(h.Open-h.Pressing)))
	}
	return Chart{ID: "open_work", Title: "Open items by who holds them", Unit: UnitItems, Kind: ChartStackedColumns,
		X: seatAxis(seats, name), Series: []ChartSeries{pressing, other}}, true
}

// wastedWakesChart is the turns each wake reason started, by what they did,
// the reasons with the most turns that changed nothing first; its note names
// the reason that wasted the largest share.
func wastedWakesChart(rows []usage.TurnLedgerRow) (Chart, bool) {
	by := map[string]map[types.TurnOutcome]int64{}
	for _, r := range rows {
		if types.WakeDisposition(r.Disposition) != types.DispositionStarted || r.Wake == "" {
			continue
		}
		if by[r.Wake] == nil {
			by[r.Wake] = map[types.TurnOutcome]int64{}
		}
		by[r.Wake][types.TurnOutcome(r.Outcome)] += r.Turns
	}
	if len(by) == 0 {
		return Chart{}, false
	}
	total := func(w string) int64 {
		var n int64
		for _, v := range by[w] {
			n += v
		}
		return n
	}
	reasons := keysBy(by, func(a, b string) int {
		return cmp.Or(cmp.Compare(by[b][types.OutcomeNothing], by[a][types.OutcomeNothing]), cmp.Compare(a, b))
	})
	c := Chart{ID: "wasted_wakes", Title: "What each kind of wake led to", Unit: UnitTurns, Kind: ChartBars,
		X: ChartAxis{Kind: AxisReason, Keys: reasons, Labels: make([]string, len(reasons))}}
	for i, w := range reasons {
		c.X.Labels[i] = WakeLabel(w)
	}
	for _, o := range []struct {
		outcome types.TurnOutcome
		label   string
	}{{types.OutcomeNothing, "Changed nothing"}, {types.OutcomeWords, "Only words"}, {types.OutcomeMoved, "Moved work"}} {
		series := ChartSeries{ID: string(o.outcome), Label: o.label}
		for _, w := range reasons {
			series.Values = append(series.Values, ptr(float64(by[w][o.outcome])))
		}
		c.Series = append(c.Series, series)
	}
	worst, share := "", -1.0
	for _, w := range reasons {
		if n := total(w); n >= minNoteTurns {
			if s := float64(by[w][types.OutcomeNothing]) / float64(n); s > share {
				worst, share = w, s
			}
		}
	}
	if worst != "" {
		c.Note = ptr(fmt.Sprintf("%s wakes changed nothing in %d of %d turns (%d%%).", WakeLabel(worst),
			by[worst][types.OutcomeNothing], total(worst), int(math.Round(share*100))))
	}
	return c, true
}

// minNoteTurns is the fewest turns a wake reason must have started for a
// chart's note to name its share: a share of three turns says nothing.
const minNoteTurns = 20

// tokensPerTurnChart is each agent seat's tokens per started turn, the most
// first; its note gives the company's median.
func tokensPerTurnChart(rows []usage.TurnLedgerRow, name func(string) string) (Chart, bool) {
	by := map[string]ledgerSum{}
	for _, r := range rows {
		if r.Handle == "" {
			continue
		}
		s := by[r.Handle]
		one := ledgerTotals([]usage.TurnLedgerRow{r})
		s.turns, s.tokens, s.nothing = s.turns+one.turns, s.tokens+one.tokens, s.nothing+one.nothing
		by[r.Handle] = s
	}
	for seat, s := range by {
		if s.turns == 0 {
			delete(by, seat)
		}
	}
	if len(by) == 0 {
		return Chart{}, false
	}
	seats := keysBy(by, func(a, b string) int {
		return cmp.Or(cmp.Compare(by[b].perTurn(), by[a].perTurn()), cmp.Compare(a, b))
	})
	series := ChartSeries{ID: "tokens_per_turn", Label: "Tokens per turn"}
	per := make([]float64, len(seats))
	for i, seat := range seats {
		per[i] = by[seat].perTurn()
		series.Values = append(series.Values, ptr(per[i]))
	}
	slices.Sort(per)
	median := per[len(per)/2]
	if len(per)%2 == 0 {
		median = math.Round((per[len(per)/2-1] + per[len(per)/2]) / 2)
	}
	return Chart{ID: "tokens_per_turn", Title: "Tokens per turn by seat", Unit: UnitTokens, Kind: ChartColumns,
		X: seatAxis(seats, name), Series: []ChartSeries{series},
		Note: ptr(fmt.Sprintf("The company's median is %s tokens per turn.", groupedInt(int64(median))))}, true
}

// seatAxis is an axis of seats by handle, labelled by name.
func seatAxis(handles []string, name func(string) string) ChartAxis {
	a := ChartAxis{Kind: AxisSeat, Keys: handles, Labels: make([]string, len(handles))}
	for i, h := range handles {
		a.Labels[i] = name(h)
	}
	return a
}

func keysBy[V any](m map[string]V, order func(a, b string) int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, order)
	return out
}

// WakeLabel is a wake reason as a person reads it: "chat:message" is "Chat
// message", "lead_fallback" is "Lead fallback".
func WakeLabel(wake string) string {
	words := strings.NewReplacer(":", " ", "_", " ", "-", " ").Replace(wake)
	words = strings.Join(strings.Fields(words), " ")
	if words == "" {
		return wake
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

// groupedInt is n with thousands grouped by commas.
func groupedInt(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + groupedInt(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
