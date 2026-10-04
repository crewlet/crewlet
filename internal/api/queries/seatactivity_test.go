package queries_test

import (
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
)

// seatDay publishes one node's whole seat-day — every turn statistic, not only
// the counts [spendFixture.day] writes — through the shipped applier.
func (f *spendFixture) seatDay(node, day, handle string, turns usage.Turns, cells ...usage.Tokens) {
	f.t.Helper()
	f.seq++
	r := usage.Record{
		RecordEnvelope: usage.RecordEnvelope{Writer: node, Subject: usage.Subject{
			Kind: usage.KindSeat, Node: node, Day: day, Seat: f.agentID(handle)}},
		Handle: handle, Role: strings.ToUpper(handle),
		Turns:  &turns,
		Tokens: cells,
	}
	body, err := r.Encode()
	if err != nil {
		f.t.Fatalf("encode: %v", err)
	}
	env, err := usage.Domain{}.Envelope(body)
	if err != nil {
		f.t.Fatalf("envelope: %v", err)
	}
	rec := statelog.Record{Envelope: env, Payload: body, Position: statelog.Position{
		Stream: usage.Domain{}.Stream().Name, Generation: 1, Seq: f.seq}}
	if err := f.db.Replicated().Tx(f.t.Context(), func(tx *sql.Tx) error {
		_, err := usage.NewApplier().Apply(f.t.Context(), tx, rec, statelog.ApplyOptions{
			MaxVariables: f.db.Replicated().Caps().MaxVariables})
		return err
	}); err != nil {
		f.t.Fatalf("apply %s %s %s: %v", node, day, handle, err)
	}
}

func activityOf(t *testing.T, f *spendFixture, params map[string]any) queries.SeatActivityAnswer {
	t.Helper()
	got, ok := askRaw(t, registryOver(t, f.sources()), "seat_activity", params).(queries.SeatActivityAnswer)
	if !ok {
		t.Fatalf("seat_activity answered %T", got)
	}
	return got
}

func seatRow(t *testing.T, a queries.SeatActivityAnswer, handle string) queries.SeatActivity {
	t.Helper()
	for _, s := range a.Seats {
		if s.Handle == handle {
			return s
		}
	}
	t.Fatalf("no row for %q in %+v", handle, a.Seats)
	return queries.SeatActivity{}
}

// hist is a histogram of the given durations.
func hist(ds ...time.Duration) usage.Hist {
	var h usage.Hist
	for _, d := range ds {
		h.Add(d)
	}
	return h
}

// EVERY NODE'S DAYS, SUMMED EXACTLY. A seat's page counted the rows of the
// phase history it had loaded — at most fifty, from the answering node's own
// log — so the number stopped at fifty and described one node. Two nodes'
// days for one seat, one of them outside the window, must come back as the
// exact sum of the ones inside it, day by day.
func TestSeatActivityCountsEveryNodesDaysExactly(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	f.seatDay("node-a", "2026-09-24", "lead", usage.Turns{Count: 3, Failed: 1,
		LastEndedAt: time.Date(2026, 9, 24, 5, 0, 0, 0, time.UTC)}, spent("execute", "m", 100))
	f.seatDay("node-b", "2026-09-24", "lead", usage.Turns{Count: 2,
		LastEndedAt: time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)}, spent("review", "m", 40))
	f.seatDay("node-b", "2026-09-25", "lead", usage.Turns{Count: 1}, spent("execute", "m", 7))
	// Outside a seven-day window ending the 25th.
	f.seatDay("node-a", "2026-09-10", "lead", usage.Turns{Count: 50}, spent("execute", "m", 999))

	got := activityOf(t, f, map[string]any{"days": 7})
	if got.Since != "2026-09-19" || got.Until != "2026-09-25" || got.Days != 7 {
		t.Errorf("window = %s..%s (%d), want the seven company days ending Tokyo's today",
			got.Since, got.Until, got.Days)
	}
	lead := seatRow(t, got, "lead")
	if lead.Turns != 6 || lead.Failed != 1 || lead.Tokens != 147 {
		t.Errorf("lead = %d turns, %d failed, %d tokens; want both nodes' days in the "+
			"window summed: 6, 1, 147", lead.Turns, lead.Failed, lead.Tokens)
	}
	if len(lead.PerDay) != 7 {
		t.Fatalf("per_day has %d days, want every day of the window", len(lead.PerDay))
	}
	if d := lead.PerDay[5]; d.Day != "2026-09-24" || d.Turns != 5 || d.Failed != 1 || d.Tokens != 140 {
		t.Errorf("the 24th = %+v, want both nodes' rows for that day summed", d)
	}
	if d := lead.PerDay[0]; d.Day != "2026-09-19" || d.Turns != 0 {
		t.Errorf("the first day = %+v, want a quiet day drawn as zeros", d)
	}
	if lead.LastTurnAt == nil || !lead.LastTurnAt.Equal(time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("last_turn_at = %v, want the newest ended turn across nodes", lead.LastTurnAt)
	}
	// A QUIET AGENT SEAT OF THE CHART HAS A ROW, with zeros: "took no turns" is
	// a measurement, and a roster that dropped the seat would read as a seat
	// that does not exist.
	sre := seatRow(t, got, "sre")
	if sre.Turns != 0 || !sre.InChart || len(sre.PerDay) != 7 {
		t.Errorf("sre = %+v, want an in-chart row of zeros over seven days", sre)
	}
}

// THE FIRST-PASS RATE IS OVER REVIEWED TURNS, and absent when none were. Over
// every turn it would read each unreviewed turn as a rejection — 3 approved of
// 4 reviewed out of 10 turns is 75%, not 30% — and a seat nobody reviewed has
// no rate at all, not 0%.
func TestSeatActivityFirstPassIsOverReviewedTurnsOnly(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	f.seatDay("node-a", "2026-09-23", "lead", usage.Turns{Count: 6, Reviewed: 3, FirstPass: 2, SentBack: 1})
	f.seatDay("node-b", "2026-09-24", "lead", usage.Turns{Count: 4, Reviewed: 1, FirstPass: 1})
	f.seatDay("node-a", "2026-09-24", "sre", usage.Turns{Count: 5})

	got := activityOf(t, f, map[string]any{"days": 7})
	lead := seatRow(t, got, "lead")
	if lead.Reviewed != 4 || lead.FirstPass != 3 || lead.SentBack != 1 {
		t.Errorf("lead reviewed/first_pass/sent_back = %d/%d/%d, want 4/3/1",
			lead.Reviewed, lead.FirstPass, lead.SentBack)
	}
	if lead.FirstPassPct == nil || *lead.FirstPassPct != 75 {
		pct := "absent"
		if lead.FirstPassPct != nil {
			pct = strconv.FormatFloat(*lead.FirstPassPct, 'f', -1, 64)
		}
		t.Errorf("first_pass_pct = %s, want 75 — three approved of four REVIEWED", pct)
	}
	if sre := seatRow(t, got, "sre"); sre.FirstPassPct != nil {
		t.Errorf("sre first_pass_pct = %v with nothing reviewed, want absent", *sre.FirstPassPct)
	}
}

// THE QUANTILES ARE THE MERGED HISTOGRAM'S, within the stated resolution. A
// median of each node's median is not the fleet's median; adding the counts
// is, to the bin.
func TestSeatActivityQuantilesAreWithinTheResolution(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	var a, b []time.Duration
	for i := 1; i <= 50; i++ {
		a = append(a, time.Duration(i)*10*time.Second) // 10s..500s
	}
	for i := 51; i <= 100; i++ {
		b = append(b, time.Duration(i)*10*time.Second) // 510s..1000s
	}
	f.seatDay("node-a", "2026-09-24", "lead", usage.Turns{Count: 50, Durations: hist(a...)})
	f.seatDay("node-b", "2026-09-25", "lead", usage.Turns{Count: 50, Durations: hist(b...)})

	got := activityOf(t, f, map[string]any{"days": 7})
	if got.QuantileResolution != usage.QuantileResolution || got.QuantileResolution > 0.06 {
		t.Errorf("quantile_resolution = %v, want the histogram's own %v", got.QuantileResolution,
			usage.QuantileResolution)
	}
	lead := seatRow(t, got, "lead")
	for _, c := range []struct {
		name string
		got  *int64
		want time.Duration
	}{{"p50", lead.P50Ms, 500 * time.Second}, {"p90", lead.P90Ms, 900 * time.Second}} {
		if c.got == nil {
			t.Fatalf("%s absent over 100 turns", c.name)
		}
		rel := math.Abs(float64(*c.got)-float64(c.want.Milliseconds())) / float64(c.want.Milliseconds())
		if rel > usage.QuantileResolution {
			t.Errorf("%s = %dms, want %v within %.3f (off by %.3f)", c.name, *c.got, c.want,
				usage.QuantileResolution, rel)
		}
	}
	if sre := seatRow(t, got, "sre"); sre.P50Ms != nil {
		t.Errorf("sre p50 = %v with no turn ended, want absent", *sre.P50Ms)
	}
}

// THE PREVIOUS WINDOW IS THE SAME NUMBER OF DAYS BEFORE, as totals, and only
// when asked for — "+n vs last week" needs last week, and nothing else does.
func TestSeatActivityCarriesThePreviousWindowsTotals(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	f.seatDay("node-a", "2026-09-24", "lead", usage.Turns{Count: 4})
	f.seatDay("node-a", "2026-09-18", "lead", usage.Turns{Count: 2, Failed: 1, Reviewed: 1},
		spent("execute", "m", 11))
	f.seatDay("node-b", "2026-09-12", "lead", usage.Turns{Count: 3}, spent("execute", "m", 5))
	// The day before the previous window: in neither.
	f.seatDay("node-b", "2026-09-11", "lead", usage.Turns{Count: 100})

	got := activityOf(t, f, map[string]any{"days": 7, "previous": true})
	if got.PreviousSince != "2026-09-12" || got.PreviousUntil != "2026-09-18" {
		t.Errorf("previous window = %s..%s, want the seven days before", got.PreviousSince,
			got.PreviousUntil)
	}
	lead := seatRow(t, got, "lead")
	if lead.Turns != 4 {
		t.Errorf("turns = %d, want this window's 4 only", lead.Turns)
	}
	if p := lead.Previous; p == nil || p.Turns != 5 || p.Failed != 1 || p.Reviewed != 1 || p.Tokens != 16 {
		t.Errorf("previous = %+v, want 5 turns, 1 failed, 1 reviewed, 16 tokens", lead.Previous)
	}
	// AND ITS DAYS, every one of the previous window's seven, oldest first —
	// so the fortnight a week-on-week figure is made over is one answer. The
	// window's own days stay the window's: the 24th is in `per_day`, never in
	// the previous one's.
	if p := lead.Previous; p != nil {
		if len(p.PerDay) != 7 || p.PerDay[0].Day != "2026-09-12" || p.PerDay[6].Day != "2026-09-18" {
			t.Fatalf("previous per_day = %+v, want the seven days 12th..18th", p.PerDay)
		}
		if p.PerDay[0].Turns != 3 || p.PerDay[6].Turns != 2 || p.PerDay[6].Failed != 1 {
			t.Errorf("previous per_day = %+v, want 3 turns on the 12th and 2 (1 failed) on the 18th",
				p.PerDay)
		}
		for _, d := range p.PerDay {
			if d.Day == "2026-09-24" || d.Turns == 100 {
				t.Errorf("a day outside the previous window reached it: %+v", d)
			}
		}
	}
	for _, d := range lead.PerDay {
		if d.Day < "2026-09-19" {
			t.Errorf("the window's per_day carries a previous day: %+v", d)
		}
	}
	if plain := activityOf(t, f, map[string]any{"days": 7}); seatRow(t, plain, "lead").Previous != nil {
		t.Error("previous totals came back unasked")
	}
}

// A SEAT WITH NO DAYS ANSWERS ONE ROW OF ZEROS. An empty list reads as "no
// such seat", and a seat that exists and did nothing is the opposite fact.
func TestSeatActivityAnUnknownSeatGetsOneEmptyRow(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	f.seatDay("node-a", "2026-09-24", "lead", usage.Turns{Count: 4})

	for _, handle := range []string{"sre", "nobody-here"} {
		got := activityOf(t, f, map[string]any{"seat": handle})
		if len(got.Seats) != 1 {
			t.Fatalf("seat=%s answered %d rows, want exactly one", handle, len(got.Seats))
		}
		row := got.Seats[0]
		if row.Handle != handle || row.Turns != 0 || len(row.PerDay) != queries.DefaultSeatActivityDays {
			t.Errorf("seat=%s row = %+v, want its handle with zeros over the default window",
				handle, row)
		}
	}
	if got := activityOf(t, f, map[string]any{"seat": "lead"}); len(got.Seats) != 1 ||
		got.Seats[0].Turns != 4 {
		t.Errorf("seat=lead = %+v, want its own row alone", got.Seats)
	}
}

// THE WINDOW IS 1..90 COMPANY DAYS, refused by name outside it.
func TestSeatActivityRefusesAWindowItCannotHold(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	r := registryOver(t, f.sources())
	for _, days := range []int{0, tokens.MaxSpendRangeDays + 1} {
		_, err := r.Answer(t.Context(), "seat_activity", map[string]any{"days": days}, "")
		if !errors.Is(err, queries.ErrBadParams) || !errors.Is(err, tokens.ErrWindowLength) ||
			!strings.Contains(queries.RefusalDetail(err), "days is") {
			t.Errorf("days=%d: %v, want a bad-params refusal naming days", days, err)
		}
	}
}

// A PERSON IS REFUSED BY NAME, NEVER ANSWERED WITH ZEROS. The engine runs no
// turn for a human seat, so a zero row would say a person "took no turns",
// and its `in_chart: false` would deny a seat the chart holds.
func TestSeatActivityRefusesAHumanSeat(t *testing.T) {
	t.Parallel()
	company, err := config.ParseCompany([]byte(spendCompany + `
  - name: Founders
    roles:
      - name: Jane Founder
        handle: jane
        kind: human
        contact:
          crewlet_operator_id: jane
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := &spendFixture{t: t, db: openStore(t), company: company}
	r := registryOver(t, f.sources())
	_, err = r.Answer(t.Context(), "seat_activity", map[string]any{"seat": "jane"}, "")
	if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(err.Error(), "Jane Founder") ||
		!strings.Contains(err.Error(), "human seat") {
		t.Errorf("seat=jane: %v, want a bad-params refusal naming the person", err)
	}
	// The unnamed answer still leaves the person out and lists the agents.
	got := activityOf(t, f, map[string]any{})
	for _, row := range got.Seats {
		if row.Handle == "jane" {
			t.Errorf("the company-wide answer carries the human seat: %+v", row)
		}
	}
	if len(got.Seats) != 2 {
		t.Errorf("company-wide answer = %d rows, want the two agent seats", len(got.Seats))
	}
}
