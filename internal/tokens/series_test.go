package tokens_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tokens"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func priced(r tokens.Record, usd float64) tokens.Record { r.CostUSD = usd; return r }

func TestEveryBucketInTheWindowIsDrawn(t *testing.T) {
	t.Parallel()
	// A HOLE IN A TIME SERIES IS NOT AN EMPTY HOUR, it is a chart the
	// client has to repair — and repairing it in the browser is the copy of
	// this bucketing that this package exists to have stopped.
	got := tokens.Bucketed([]tokens.Record{
		rec("CEO", "plan", "sonnet", "t1", "2026-06-14T12:10:00Z", 60, 20),
		rec("CEO", "plan", "sonnet", "t2", "2026-06-14T15:40:00Z", 10, 10),
	}, tokens.SeriesOptions{
		Group:    tokens.GroupPhase,
		Interval: tokens.IntervalHour,
		Since:    at("2026-06-14T12:00:00Z"),
		Until:    at("2026-06-14T16:00:00Z"),
	})

	if len(got.Points) != 4 {
		t.Fatalf("points = %d, want one per hour of the window", len(got.Points))
	}
	if got.Points[0].At != "2026-06-14T12:00:00Z" {
		t.Errorf("first bucket at %q, want the window's own start", got.Points[0].At)
	}
	if got.Points[0].TotalTokens != 80 || got.Points[3].TotalTokens != 20 {
		t.Errorf("edges = %d and %d", got.Points[0].TotalTokens, got.Points[3].TotalTokens)
	}
	if got.Points[1].Calls != 0 || got.Points[2].Calls != 0 {
		t.Errorf("the quiet hours are not empty: %+v %+v", got.Points[1], got.Points[2])
	}
	if got.Until != "2026-06-14T16:00:00Z" {
		t.Errorf("until = %q, want the end of the last bucket", got.Until)
	}
}

func TestABucketIsUTCMidnightAndNotAnAbsoluteMultiple(t *testing.T) {
	t.Parallel()
	// The obvious spelling is time.Truncate(24*time.Hour), which truncates
	// the duration since year 1 and lands on a UTC midnight only because
	// year 1 happened to begin at one. It also silently uses whatever
	// Location the value carries. Both are asserted here so the calendar
	// arithmetic cannot be "simplified" back.
	east := time.FixedZone("east", 9*3600)
	start := tokens.IntervalDay.Start(time.Date(2026, 6, 15, 3, 0, 0, 0, east))
	if want := "2026-06-14T00:00:00Z"; start.Format(time.RFC3339) != want {
		t.Errorf("day start = %s, want %s — the UTC day, not the local one",
			start.Format(time.RFC3339), want)
	}
	hour := tokens.IntervalHour.Start(time.Date(2026, 6, 15, 3, 59, 59, 0, east))
	if want := "2026-06-14T18:00:00Z"; hour.Format(time.RFC3339) != want {
		t.Errorf("hour start = %s, want %s", hour.Format(time.RFC3339), want)
	}
}

func TestTheBandsPastTheCapFoldIntoOneResidual(t *testing.T) {
	t.Parallel()
	// Grouping by turn over a busy window is thousands of bands. The chart
	// carries five and the rest are ONE row — and that row is marked rather
	// than named, so a phase or a model genuinely called "other" cannot be
	// mistaken for it.
	var records []tokens.Record
	for i, size := range []int{700, 600, 500, 400, 300, 200, 100} {
		records = append(records, tokens.Record{
			EventID:   string(rune('a' + i)),
			Timestamp: "2026-06-14T12:00:00Z",
			AgentRole: "CEO", Phase: "plan", Model: string(rune('a' + i)),
			TotalTokens: size, InputTokens: size,
		})
	}
	got := tokens.Bucketed(records, tokens.SeriesOptions{
		Group: tokens.GroupModel, Interval: tokens.IntervalHour,
		Since: at("2026-06-14T12:00:00Z"), Until: at("2026-06-14T13:00:00Z"),
	})

	if len(got.ByGroup) != 6 {
		t.Fatalf("by_group = %d rows, want five bands and one residual", len(got.ByGroup))
	}
	last := got.ByGroup[5]
	if !last.Other || last.Folded != 2 {
		t.Errorf("residual = %+v, want other with two groups folded", last)
	}
	if last.TotalTokens != 300 {
		t.Errorf("residual tokens = %d, want 300", last.TotalTokens)
	}
	if last.Group != "" {
		t.Errorf("residual group = %q, want empty so no real name can collide", last.Group)
	}
	if got.ByGroup[0].Group != "a" {
		t.Errorf("first band = %q, want the biggest", got.ByGroup[0].Group)
	}
	point := got.Points[0]
	if point.Residual.TotalTokens != 300 {
		t.Errorf("the bucket's residual = %d, want 300", point.Residual.TotalTokens)
	}
	if _, ok := point.Groups["f"]; ok {
		t.Error("a folded band is still its own key in the bucket")
	}
	if point.TotalTokens != 2800 {
		t.Errorf("the bucket's own total = %d, want every band", point.TotalTokens)
	}
}

func TestWhichBandsSurviveIsDecidedOverTheWholeWindow(t *testing.T) {
	t.Parallel()
	// A per-bucket decision would put a model in the chart for the hours it
	// happened to lead and in the residual for the rest — one band that
	// appears and disappears, which reads as spend that stopped.
	var records []tokens.Record
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		records = append(records, tokens.Record{
			EventID: "big" + name, Timestamp: "2026-06-14T12:00:00Z",
			Phase: "plan", Model: name, TotalTokens: 1000 - i,
		})
	}
	// `small` leads the SECOND hour outright and is still last overall.
	records = append(records, tokens.Record{
		EventID: "small", Timestamp: "2026-06-14T13:30:00Z",
		Phase: "plan", Model: "small", TotalTokens: 5,
	})
	got := tokens.Bucketed(records, tokens.SeriesOptions{
		Group: tokens.GroupModel, Interval: tokens.IntervalHour,
		Since: at("2026-06-14T12:00:00Z"), Until: at("2026-06-14T14:00:00Z"),
	})
	if _, ok := got.Points[1].Groups["small"]; ok {
		t.Error("a band that leads one bucket got its own key there, " +
			"so the legend's five are not the chart's five")
	}
	if got.Points[1].Residual.TotalTokens != 5 {
		t.Errorf("the second hour's residual = %d, want the folded band's 5",
			got.Points[1].Residual.TotalTokens)
	}
}

func TestARecordThisGroupingPlacesNowhereStillCounts(t *testing.T) {
	t.Parallel()
	// Grouping by worker leaves out every phase that is not a worker's.
	// Those are real spend, and a chart whose bands sum to less than the
	// company's total has to SAY so rather than let a reader discover it by
	// comparing two screens.
	got := tokens.Bucketed([]tokens.Record{
		{EventID: "w", Timestamp: "2026-06-14T12:00:00Z",
			Phase: "auxiliary", Worker: "reflect", TotalTokens: 30},
		{EventID: "d", Timestamp: "2026-06-14T12:00:00Z",
			Phase: "subagent", Worker: "researcher", TotalTokens: 20},
		{EventID: "p", Timestamp: "2026-06-14T12:00:00Z",
			Phase: "plan", TotalTokens: 70},
		// Worker set on a phase that names none: the phase is part of
		// what keys the band, so this one counts nowhere either.
		{EventID: "stray", Timestamp: "2026-06-14T12:00:00Z",
			Phase: "plan", Worker: "reflect", TotalTokens: 1},
	}, tokens.SeriesOptions{
		Group: tokens.GroupWorker, Interval: tokens.IntervalHour,
		Since: at("2026-06-14T12:00:00Z"), Until: at("2026-06-14T13:00:00Z"),
	})
	if got.Totals.TotalTokens != 121 {
		t.Errorf("totals = %d, want every record in the window", got.Totals.TotalTokens)
	}
	if got.Grouped.TotalTokens != 50 {
		t.Errorf("grouped = %d, want the learning worker's and the template's", got.Grouped.TotalTokens)
	}
	if len(got.ByGroup) != 2 || got.ByGroup[0].Group != "auxiliary/reflect" ||
		got.ByGroup[1].Group != "subagent/researcher" {
		t.Errorf("by_group = %+v, want one band per worker, keyed with its phase", got.ByGroup)
	}
}

// A TEMPLATE AND A LEARNING WORKER SHARING A NAME ARE TWO BANDS, for the
// reason they are two rows of the rollup: nothing reserves a learning worker's
// name from the `workers:` grammar, and one band would chart two workers' spend
// as one.
func TestATemplateAndALearningWorkerWithOneNameAreTwoBands(t *testing.T) {
	t.Parallel()
	got := tokens.Bucketed([]tokens.Record{
		{EventID: "w", Timestamp: "2026-06-14T12:00:00Z",
			Phase: "auxiliary", Worker: "persist_decider", TotalTokens: 10},
		{EventID: "d", Timestamp: "2026-06-14T12:00:00Z",
			Phase: "subagent", Worker: "persist_decider", TotalTokens: 30},
	}, tokens.SeriesOptions{
		Group: tokens.GroupWorker, Interval: tokens.IntervalHour,
		Since: at("2026-06-14T12:00:00Z"), Until: at("2026-06-14T13:00:00Z"),
	})
	bands := map[string]int{}
	for _, row := range got.ByGroup {
		bands[row.Group] = row.TotalTokens
	}
	if bands["subagent/persist_decider"] != 30 || bands["auxiliary/persist_decider"] != 10 || len(bands) != 2 {
		t.Errorf("bands = %v, want the template's 30 and the learning worker's 10 apart", bands)
	}
}

func TestASeatWithNoUnitIsPlacedRatherThanPooledWithTheUnknown(t *testing.T) {
	t.Parallel()
	got := tokens.Bucketed([]tokens.Record{
		rec("CEO", "plan", "sonnet", "t1", "2026-06-14T12:00:00Z", 10, 0),
		rec("ENG", "plan", "sonnet", "t2", "2026-06-14T12:00:00Z", 20, 0),
	}, tokens.SeriesOptions{
		Group: tokens.GroupUnit, Interval: tokens.IntervalHour,
		Since: at("2026-06-14T12:00:00Z"), Until: at("2026-06-14T13:00:00Z"),
		Units: map[string]string{"ENG": "Engineering"},
	})
	names := map[string]int{}
	for _, row := range got.ByGroup {
		names[row.Group] = row.TotalTokens
	}
	if names["Engineering"] != 20 {
		t.Errorf("by_group = %+v, want the mapped unit", got.ByGroup)
	}
	if names["no unit"] != 10 {
		t.Errorf("by_group = %+v, want the root seat in its own band", got.ByGroup)
	}
	if _, ok := names["unknown"]; ok {
		t.Error("a root seat was pooled with the records that carried no role at all")
	}
}

func TestOnlyAPositivePriceCountsAndTheCountIsCarriedBesideIt(t *testing.T) {
	t.Parallel()
	// A price is reported by ONE backend. Zero dollars over zero priced
	// calls means nobody said; zero over two means two runs were billed
	// nothing — and a negative is a bad payload, never a rebate.
	got := tokens.Bucketed([]tokens.Record{
		priced(rec("CEO", "execute", "cli", "t1", "2026-06-14T12:00:00Z", 10, 0), 0.25),
		priced(rec("CEO", "execute", "cli", "t2", "2026-06-14T12:10:00Z", 10, 0), -1),
		rec("CEO", "plan", "sonnet", "t3", "2026-06-14T12:20:00Z", 10, 0),
	}, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Interval: tokens.IntervalHour,
		Since: at("2026-06-14T12:00:00Z"), Until: at("2026-06-14T13:00:00Z"),
	})
	if got.Totals.CostUSD != 0.25 || got.Totals.PricedCalls != 1 {
		t.Errorf("totals = %+v, want one priced call at 0.25", got.Totals)
	}
	if b := got.Points[0].Groups["plan"]; b.PricedCalls != 0 || b.CostUSD != 0 {
		t.Errorf("the unpriced band = %+v, want nothing quoted", b)
	}
}

func TestOrderOfArrivalDoesNotChangeTheSeries(t *testing.T) {
	t.Parallel()
	forward := []tokens.Record{
		rec("CEO", "plan", "sonnet", "t1", "2026-06-14T12:00:00Z", 60, 20),
		rec("ENG", "execute", "opus", "t2", "2026-06-14T13:30:00Z", 10, 40),
		rec("CEO", "review", "haiku", "t1", "2026-06-14T14:59:59Z", 5, 5),
	}
	backward := []tokens.Record{forward[2], forward[1], forward[0]}
	opts := tokens.SeriesOptions{
		Group: tokens.GroupSeat, Interval: tokens.IntervalHour,
		Since: at("2026-06-14T12:00:00Z"), Until: at("2026-06-14T15:00:00Z"),
		Handles: map[string]string{"CEO": "ceo"},
	}
	a, _ := json.Marshal(tokens.Bucketed(forward, opts))
	b, _ := json.Marshal(tokens.Bucketed(backward, opts))
	if string(a) != string(b) {
		t.Errorf("the same records in two orders gave two series:\n%s\n%s", a, b)
	}
	got := tokens.Bucketed(forward, opts)
	if got.ByGroup[0].Group != "CEO" || got.ByGroup[0].Handle != "ceo" {
		t.Errorf("by_group[0] = %+v, want the seat and its handle", got.ByGroup[0])
	}
	if got.ByGroup[1].Handle != "" {
		t.Errorf("an unmapped role got the handle %q rather than none",
			got.ByGroup[1].Handle)
	}
}

func TestTheWindowIsHalfOpenSoTwoAdjacentOnesNeitherLoseNorDouble(t *testing.T) {
	t.Parallel()
	// Compare-to-previous asks for [since-span, since) and [since, until).
	// A record exactly on the boundary belongs to the later window and to
	// exactly one of them.
	edge := []tokens.Record{
		rec("CEO", "plan", "sonnet", "t1", "2026-06-14T12:00:00Z", 10, 0),
	}
	since, until := at("2026-06-14T12:00:00Z"), at("2026-06-14T13:00:00Z")
	prevSince, prevUntil := tokens.PreviousWindow(since, until)
	if prevSince != at("2026-06-14T11:00:00Z") || prevUntil != since {
		t.Fatalf("previous window = %s..%s", prevSince, prevUntil)
	}
	now := tokens.Bucketed(edge, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Since: since, Until: until,
	})
	prev := tokens.Bucketed(edge, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Since: prevSince, Until: prevUntil,
	})
	if now.Totals.Calls != 1 {
		t.Errorf("the later window missed the record on its own start")
	}
	if prev.Totals.Calls != 0 {
		t.Errorf("the earlier window claimed the record on its exclusive end")
	}
}

func TestAWindowTooLongForTheAxisKeepsTheNewestAndSaysSo(t *testing.T) {
	t.Parallel()
	// Silently dropping the oldest buckets would put a year's heading over
	// forty days of bars. The window is REPORTED as what was covered.
	since := at("2020-01-01T00:00:00Z")
	until := at("2026-06-14T12:30:00Z")
	got := tokens.Bucketed(nil, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Interval: tokens.IntervalHour,
		Since: since, Until: until,
	})
	if len(got.Points) != tokens.MaxSeriesPoints {
		t.Fatalf("points = %d, want the cap", len(got.Points))
	}
	if got.Since == since.Format(time.RFC3339) {
		t.Error("the answer claims the window it was asked for, not the one it drew")
	}
	last := got.Points[len(got.Points)-1].At
	if last != "2026-06-14T12:00:00Z" {
		t.Errorf("last bucket at %q, want the one holding the window's end", last)
	}
}

func TestAnUnknownIntervalOrGroupIsAValueTheCallerCanRefuse(t *testing.T) {
	t.Parallel()
	for _, i := range tokens.Intervals {
		if !i.Valid() {
			t.Errorf("%q is in the closed set and reports invalid", i)
		}
	}
	for _, g := range tokens.Groups {
		if !g.Valid() {
			t.Errorf("%q is in the closed set and reports invalid", g)
		}
	}
	if tokens.Interval("minute").Valid() || tokens.Group("project").Valid() {
		t.Error("an unknown value reports valid, so a caller cannot refuse it")
	}
}

func TestNoWindowAndNoRecordsIsAnEmptyAxisNotTheYearOne(t *testing.T) {
	t.Parallel()
	got := tokens.Bucketed(nil, tokens.SeriesOptions{Group: tokens.GroupPhase})
	if len(got.Points) != 0 {
		t.Errorf("points = %+v, want none", got.Points)
	}
	if got.Since != "" || got.Until != "" {
		t.Errorf("window = %q..%q, want none claimed", got.Since, got.Until)
	}
	if body, _ := json.Marshal(got); string(body) == "" {
		t.Fatal("unmarshalable")
	} else if !jsonHas(body, `"series":[]`) || !jsonHas(body, `"by_group":[]`) {
		t.Errorf("empty lists marshalled as null, which throws in the browser: %s", body)
	}
}

func jsonHas(body []byte, want string) bool {
	return len(body) > 0 && contains(string(body), want)
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
