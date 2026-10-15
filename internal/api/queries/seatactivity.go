package queries

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE SEAT ACTIVITY ANSWER, `seat_activity`: every seat's turn statistics over
// a window of company days, from the replicated `usage` domain (ADR-0020).
//
// ONE AGGREGATE rather than a count of whatever list a screen happened to load.
// A seat's page drew "Turns in the record" as the LENGTH of its phase history —
// a page of at most fifty, read from the answering node's own event log — so a
// busy seat's number stopped at fifty, a quiet one's described one node of the
// fleet, and neither said over what window. Here the numbers are every node's
// days, summed: the same on whichever node is asked, still counting a node
// that has left, and labelled with the days they cover.

// DefaultSeatActivityDays is the window a seat's activity covers when the
// question names none: a week, which is what a profile's KPIs compare against
// ("+n vs last week") and long enough to hold a seat that runs on a weekly
// schedule at least once.
const DefaultSeatActivityDays = 7

// SeatActivityAnswer is the `seat_activity` answer.
type SeatActivityAnswer struct {
	// Since and Until are the window's first and last company day, both
	// inclusive, and Days its length.
	Since string `json:"since"`
	Until string `json:"until"`
	Days  int    `json:"days"`

	// PreviousSince and PreviousUntil are the window before it, of the same
	// length, when `previous` was asked for.
	PreviousSince string `json:"previous_since,omitempty"`
	PreviousUntil string `json:"previous_until,omitempty"`

	// Seats is one row per seat: every agent seat of the current chart (a
	// quiet one included, with zeros — "took no turns" is a measurement),
	// plus any seat whose days are in the window and that has since left the
	// chart. Ordered by handle, never by a number that moves.
	Seats []SeatActivity `json:"seats"`

	// QuantileResolution is the worst relative error of p50_ms and p90_ms:
	// they are read from a merged duration histogram (see usage.Hist).
	QuantileResolution float64 `json:"quantile_resolution"`
}

// SeatActivity is one seat's turns over the window.
type SeatActivity struct {
	Handle  string `json:"handle"`
	Role    string `json:"role,omitempty"`
	AgentID string `json:"agent_id,omitempty"`

	// InChart is false for a seat whose days are in the window and that the
	// current chart no longer holds.
	InChart bool `json:"in_chart"`

	Turns  int64 `json:"turns"`
	Failed int64 `json:"failed"`

	// Reviewed is how many turns a reviewer judged, and FirstPass how many
	// of those it approved on the first review.
	Reviewed  int64 `json:"reviewed"`
	FirstPass int64 `json:"first_pass"`

	// FirstPassPct is FirstPass over REVIEWED turns, 0..100 — ABSENT when
	// none was reviewed. Over all turns it would read every unreviewed turn
	// as a failure, and a 0% for a seat nobody reviewed is a claim about its
	// work that nobody made.
	FirstPassPct *float64 `json:"first_pass_pct,omitempty"`

	// SentBack counts reviews that sent work back, not turns.
	SentBack int64 `json:"sent_back"`

	// P50Ms and P90Ms are turn-duration quantiles, within
	// QuantileResolution; absent when no turn ended.
	P50Ms *int64 `json:"p50_ms,omitempty"`
	P90Ms *int64 `json:"p90_ms,omitempty"`

	Tokens int64 `json:"tokens"`

	// PerDay is every day of the window, oldest first, a quiet one included.
	PerDay []SeatActivityDay `json:"per_day"`

	// LastTurnAt is when the newest turn in the window ended; absent when
	// none did.
	LastTurnAt *time.Time `json:"last_turn_at,omitempty"`

	// Previous is the same seat's totals over the window before, when
	// `previous` was asked for.
	Previous *SeatActivityTotals `json:"previous,omitempty"`
}

// SeatActivityDay is one company day of one seat, summed across nodes.
type SeatActivityDay struct {
	Day    string `json:"day"`
	Turns  int64  `json:"turns"`
	Failed int64  `json:"failed"`
	Tokens int64  `json:"tokens"`
}

// SeatActivityTotals is a seat's totals over the previous window.
type SeatActivityTotals struct {
	Turns     int64 `json:"turns"`
	Failed    int64 `json:"failed"`
	Reviewed  int64 `json:"reviewed"`
	FirstPass int64 `json:"first_pass"`
	SentBack  int64 `json:"sent_back"`
	Tokens    int64 `json:"tokens"`

	// PerDay is every day of the previous window, oldest first, a quiet one
	// included — so a profile draws the fortnight a week-on-week comparison
	// is made over from the ONE answer that makes the comparison, rather
	// than asking a second, overlapping one.
	PerDay []SeatActivityDay `json:"per_day"`
}

// seatFold is one seat's rows being summed.
type seatFold struct {
	row  SeatActivity
	hist usage.Hist
	days map[string]*SeatActivityDay
	prev SeatActivityTotals
}

// seatActivity answers `seat_activity{seat?, days, previous}`.
//
// `days` is 1..[tokens.MaxSpendRangeDays] company days ending today, 7 when
// unnamed; `previous` adds each seat's totals over the same number of days
// before; `seat` narrows to one handle, and a handle with no rows answers ONE
// row of zeros rather than none — an empty list reads as "no such seat", and
// the seat exists and did nothing. A human seat's handle is refused by name:
// the engine runs no turns for a person.
func (s Sources) seatActivity(ctx context.Context, p Params) (any, error) {
	days := DefaultSeatActivityDays
	if p.Has("days") {
		days = p.Int("days", 0)
		if days < 1 || days > tokens.MaxSpendRangeDays {
			return nil, refuseAs(tokens.ErrWindowLength, "days is %v, and a seat's "+
				"activity covers 1 to %d company days — ask for at most %d",
				p.Values()["days"], tokens.MaxSpendRangeDays, tokens.MaxSpendRangeDays)
		}
	}
	loc := s.zone()
	now := s.clock()
	window := tokens.LastDays(days, now, loc)
	horizon := tokens.NewHorizon(usage.HorizonDays, tokens.LastDays(1, now, loc).Last)
	previous := p.Bool("previous", false)
	prev := window.Previous()
	if previous && !horizon.Admits(prev) {
		return nil, refuseAs(tokens.ErrOutOfRange, "the window before these %d days "+
			"starts %s, before %s — the oldest day the %d-day usage history holds; "+
			"ask for fewer days", days, prev.First.Label, horizon.Floor, horizon.Days)
	}

	// ONE SEAT, by the handle a person reads — resolved through the chart
	// to the id its days are filed under ([Sources.seatAgentID]), which
	// refuses a PERSON's seat by name: the engine runs no turn for a human,
	// so a row of zeros would say "took no turns" about somebody no turn was
	// ever for.
	seat := strings.TrimSpace(p.String("seat"))
	agentID, err := s.seatParam(p)
	if err != nil {
		return nil, err
	}

	from := window.First.Label
	if previous {
		from = prev.First.Label
	}
	rows, err := usage.SeatDays(ctx, s.Usage, usage.SpendQuery{
		From: from, To: window.Last.Label, AgentID: agentID})
	if err != nil {
		return nil, usageErr(err)
	}

	answer := SeatActivityAnswer{
		Since: window.First.Label, Until: window.Last.Label, Days: window.Days(),
		QuantileResolution: usage.QuantileResolution,
	}
	if previous {
		answer.PreviousSince, answer.PreviousUntil = prev.First.Label, prev.Last.Label
	}
	answer.Seats = foldSeatActivity(rows, seatActivityFold{
		window: window, previous: previous, seat: seat, agentID: agentID,
		chart: chartSeats(s.organization()),
	})
	return answer, nil
}

// chartSeat is one agent seat of the current chart, as the fold keys it.
type chartSeat struct {
	handle, role, agentID string
}

// chartSeats is every AGENT seat of the running chart. A human seat is not run
// by the engine, so it has no turns to count and no row here — a zero beside a
// person would read as a person who did nothing.
//
// EACH BY THE ID ITS DAYS ARE FILED UNDER, derived from the seat's handle
// ([org.Organization.AgentIDFor]).
func chartSeats(organization *org.Organization) []chartSeat {
	if organization == nil {
		return nil
	}
	var out []chartSeat
	for role := range organization.AllRoles() {
		id, ok := organization.AgentIDFor(role)
		if !ok {
			continue
		}
		out = append(out, chartSeat{handle: role.Handle(), role: role.Name, agentID: id.String()})
	}
	return out
}

// seatActivityFold is what [foldSeatActivity] needs beside the rows.
type seatActivityFold struct {
	window   tokens.Range
	previous bool
	// agentID narrows to one seat, and seat is the handle it was asked by,
	// which names its row when the chart no longer holds it; empty is every
	// seat.
	seat, agentID string
	chart         []chartSeat
}

// foldSeatActivity sums every node's days into one row per seat.
//
// PURE over its inputs, so the arithmetic — the sums across nodes, the
// reviewed-only denominator, the merged quantiles — is testable without a
// database. A seat is keyed on its AGENT ID, which every node derives alike;
// the handle and role shown are the org's, or else the newest day's.
func foldSeatActivity(rows []usage.SeatDay, f seatActivityFold) []SeatActivity {
	inWindow := func(day string) bool {
		return day >= f.window.First.Label && day <= f.window.Last.Label
	}
	folds := map[string]*seatFold{}
	get := func(key string) *seatFold {
		if sf, ok := folds[key]; ok {
			return sf
		}
		sf := &seatFold{days: map[string]*SeatActivityDay{}}
		folds[key] = sf
		return sf
	}
	for _, c := range f.chart {
		if f.agentID != "" && c.agentID != f.agentID {
			continue
		}
		sf := get(c.agentID)
		sf.row.Handle, sf.row.Role, sf.row.AgentID, sf.row.InChart = c.handle, c.role, c.agentID, true
	}
	if f.agentID != "" && len(folds) == 0 {
		// THE SEAT ASKED FOR, WHETHER OR NOT THE CHART HOLDS IT: one row of
		// zeros, so "did nothing" and "no such seat" are not one answer.
		get(f.agentID).row.Handle, folds[f.agentID].row.AgentID = f.seat, f.agentID
	}

	// Rows arrive in (day, node, seat) order, so a later row's name is a
	// newer day's.
	for _, r := range rows {
		if f.agentID != "" && r.AgentID != f.agentID {
			continue
		}
		sf := get(r.AgentID)
		if !sf.row.InChart {
			if r.Handle != "" {
				sf.row.Handle = r.Handle
			}
			if r.Role != "" {
				sf.row.Role = r.Role
			}
			if sf.row.AgentID == "" {
				sf.row.AgentID = r.AgentID
			}
		}
		d := sf.days[r.Day]
		if d == nil {
			d = &SeatActivityDay{Day: r.Day}
			sf.days[r.Day] = d
		}
		d.Turns += r.Turns
		d.Failed += r.Failed
		d.Tokens += r.Tokens
		if !inWindow(r.Day) {
			sf.prev.Turns += r.Turns
			sf.prev.Failed += r.Failed
			sf.prev.Reviewed += r.Reviewed
			sf.prev.FirstPass += r.FirstPass
			sf.prev.SentBack += r.SentBack
			sf.prev.Tokens += r.Tokens
			continue
		}
		sf.row.Turns += r.Turns
		sf.row.Failed += r.Failed
		sf.row.Reviewed += r.Reviewed
		sf.row.FirstPass += r.FirstPass
		sf.row.SentBack += r.SentBack
		sf.row.Tokens += r.Tokens
		sf.hist.Merge(r.Durations)
		if !r.LastEndedAt.IsZero() && (sf.row.LastTurnAt == nil || r.LastEndedAt.After(*sf.row.LastTurnAt)) {
			at := r.LastEndedAt.UTC()
			sf.row.LastTurnAt = &at
		}
	}

	windows := f.window.Windows()
	out := make([]SeatActivity, 0, len(folds))
	for _, sf := range folds {
		row := sf.row
		if row.Reviewed > 0 {
			pct := float64(row.FirstPass) * 100 / float64(row.Reviewed)
			row.FirstPassPct = &pct
		}
		if d, ok := sf.hist.Quantile(0.5); ok {
			ms := d.Milliseconds()
			row.P50Ms = &ms
		}
		if d, ok := sf.hist.Quantile(0.9); ok {
			ms := d.Milliseconds()
			row.P90Ms = &ms
		}
		row.PerDay = everyDay(windows, sf.days)
		if f.previous {
			prev := sf.prev
			prev.PerDay = everyDay(f.window.Previous().Windows(), sf.days)
			row.Previous = &prev
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b SeatActivity) int {
		return cmp.Or(cmp.Compare(a.Handle, b.Handle), cmp.Compare(a.AgentID, b.AgentID))
	})
	return out
}

// everyDay is one row per day of windows, oldest first, a day with no record
// as zeros: "took no turns" is a measurement, and a gap in a chart's x-axis
// is not.
func everyDay(windows []period.Window, days map[string]*SeatActivityDay) []SeatActivityDay {
	out := make([]SeatActivityDay, 0, len(windows))
	for _, w := range windows {
		if d := days[w.Label]; d != nil {
			out = append(out, *d)
		} else {
			out = append(out, SeatActivityDay{Day: w.Label})
		}
	}
	return out
}
