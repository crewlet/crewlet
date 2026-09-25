package tokens_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/tokens"
)

// cell is one company day's spend cell for a seat, with only the dimensions a
// case sets.
func cell(day, handle, phase, model string, total int) tokens.Cell {
	return tokens.Cell{
		Day: day, AgentID: "id-" + handle, Handle: handle, Role: strings.ToUpper(handle),
		Phase: phase, Model: model, ProviderKey: "p-" + model,
		Bucket: tokens.Bucket{InputTokens: total, TotalTokens: total, Calls: 1},
	}
}

// days is the range first..last on loc.
func days(t *testing.T, first, last string, loc *time.Location) tokens.Range {
	t.Helper()
	r, err := tokens.DaysBetween(first, last, loc)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEveryDayInTheWindowIsDrawn(t *testing.T) {
	t.Parallel()
	// A HOLE IN A TIME SERIES IS NOT AN EMPTY DAY, it is a chart the client
	// has to repair — and repairing it in the browser is the copy of this
	// bucketing that this package exists to have stopped.
	got := tokens.BucketDaily([]tokens.Cell{
		cell("2026-06-14", "ceo", "execute", "sonnet", 80),
		cell("2026-06-17", "ceo", "execute", "sonnet", 20),
	}, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Interval: tokens.IntervalDay,
		Range: days(t, "2026-06-14", "2026-06-17", time.UTC),
	})
	if len(got.Points) != 4 {
		t.Fatalf("points = %d, want one per day of the window", len(got.Points))
	}
	if got.Points[0].At != "2026-06-14T00:00:00Z" || got.Points[0].Window != "2026-06-14" {
		t.Errorf("first bucket = %s %s, want the window's own first day",
			got.Points[0].At, got.Points[0].Window)
	}
	if got.Points[0].TotalTokens != 80 || got.Points[3].TotalTokens != 20 {
		t.Errorf("edges = %d and %d", got.Points[0].TotalTokens, got.Points[3].TotalTokens)
	}
	if got.Points[1].Calls != 0 || got.Points[2].Calls != 0 {
		t.Errorf("the quiet days are not empty: %+v %+v", got.Points[1], got.Points[2])
	}
	if got.Until != "2026-06-18T00:00:00Z" || got.Days != 4 {
		t.Errorf("until = %q over %d days, want the end of the last day over 4",
			got.Until, got.Days)
	}
}

// A WEEK BUCKET IS THE COMPANY'S ISO WEEK, from its own Monday midnight — not
// seven UTC days from the window's first. Cut in Tokyo, a Monday begins at 15:00
// the Sunday before in UTC, and a week holding a clock change elsewhere is not
// 168 hours long; the axis is walked by label so neither can move a boundary.
func TestAWeekBucketIsTheCompanysISOWeek(t *testing.T) {
	t.Parallel()
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	// Wednesday 10 June 2026 to Tuesday 23 June: a partial week, a whole
	// one, and a partial one.
	got := tokens.BucketDaily([]tokens.Cell{
		cell("2026-06-10", "ceo", "execute", "sonnet", 1),
		cell("2026-06-14", "ceo", "execute", "sonnet", 2), // Sunday: still W24
		cell("2026-06-15", "ceo", "execute", "sonnet", 4), // Monday: W25
		cell("2026-06-23", "ceo", "execute", "sonnet", 8),
	}, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Interval: tokens.IntervalWeek,
		Range: days(t, "2026-06-10", "2026-06-23", tokyo),
	})
	want := []struct {
		window, at  string
		days, total int
	}{
		{"2026-W24", "2026-06-07T15:00:00Z", 5, 3},
		{"2026-W25", "2026-06-14T15:00:00Z", 7, 4},
		{"2026-W26", "2026-06-21T15:00:00Z", 2, 8},
	}
	if len(got.Points) != len(want) {
		t.Fatalf("points = %+v, want three weeks", got.Points)
	}
	for i, w := range want {
		p := got.Points[i]
		if p.Window != w.window || p.At != w.at || p.Days != w.days || p.TotalTokens != w.total {
			t.Errorf("week %d = {%s %s %d days %d}, want %+v",
				i, p.Window, p.At, p.Days, p.TotalTokens, w)
		}
	}
	if got.Interval != tokens.IntervalWeek || got.Since != "2026-06-09T15:00:00Z" {
		t.Errorf("series = %s from %s, want weeks from Tokyo's midnight on the 10th",
			got.Interval, got.Since)
	}
}

// SANDBOX IS EXECUTE. A detached coding run is the executor's own work done in
// a box, and drawn as a band of its own it would read as a fifth kind of
// spend the palette has no hue for.
func TestTheFourPhaseBandsFoldEveryPhase(t *testing.T) {
	t.Parallel()
	for phase, want := range map[string]tokens.Band{
		"execute": tokens.BandExecute, "sandbox": tokens.BandExecute, "plan": tokens.BandExecute,
		"review":    tokens.BandReview,
		"subagent":  tokens.BandWorkers,
		"auxiliary": tokens.BandAuxiliary, "judge": tokens.BandAuxiliary,
		"onboarding": tokens.BandAuxiliary, "": tokens.BandAuxiliary,
		"a-phase-from-a-newer-peer": tokens.BandAuxiliary,
	} {
		if got := tokens.PhaseBand(phase); got != want {
			t.Errorf("PhaseBand(%q) = %q, want %q", phase, got, want)
		}
	}
	got := tokens.BucketDaily([]tokens.Cell{
		cell("2026-06-14", "ceo", "execute", "sonnet", 10),
		cell("2026-06-14", "ceo", "sandbox", "cli", 30),
		cell("2026-06-14", "ceo", "subagent", "haiku", 5),
		cell("2026-06-14", "ceo", "judge", "haiku", 1),
	}, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Range: days(t, "2026-06-14", "2026-06-14", time.UTC),
	})
	bands := map[string]int{}
	for _, row := range got.ByGroup {
		bands[row.Group] = row.TotalTokens
	}
	if bands["execute"] != 40 || bands["workers"] != 5 || bands["auxiliary"] != 1 {
		t.Errorf("bands = %v, want the coding run inside execute", bands)
	}
	if _, ok := bands["sandbox"]; ok {
		t.Error("sandbox is a band of its own")
	}
}

// EVERY PHASE THE ENGINE WRITES IS FOLDED ON PURPOSE. A phase added to the
// event catalogue lands in Auxiliary by default — which is right for a value a
// NEWER peer wrote, and wrong for one this build produces and forgot to place.
func TestEveryPhaseThisBuildWritesHasABandChosenForIt(t *testing.T) {
	t.Parallel()
	chosen := map[types.Phase]tokens.Band{
		types.PhaseOnboarding: tokens.BandAuxiliary,
		types.PhaseExecute:    tokens.BandExecute,
		types.PhaseReview:     tokens.BandReview,
		types.PhaseSubagent:   tokens.BandWorkers,
		types.PhaseAuxiliary:  tokens.BandAuxiliary,
		types.PhaseJudge:      tokens.BandAuxiliary,
		types.PhaseSandbox:    tokens.BandExecute,
	}
	for phase, band := range chosen {
		if got := tokens.PhaseBand(string(phase)); got != band {
			t.Errorf("PhaseBand(%q) = %q, want %q", phase, got, band)
		}
	}
}

// THE PHASE LEGEND NEVER MOVES. By size, a week where review ran long would put
// Review first and hand it Execute's place — and with a positional palette its
// colour.
func TestThePhaseBandsAreInStackingOrderAndNeverFold(t *testing.T) {
	t.Parallel()
	got := tokens.BucketDaily([]tokens.Cell{
		cell("2026-06-14", "ceo", "review", "haiku", 900),
		cell("2026-06-14", "ceo", "auxiliary", "haiku", 500),
		cell("2026-06-14", "ceo", "subagent", "haiku", 50),
		cell("2026-06-14", "ceo", "execute", "sonnet", 10),
	}, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Groups: 1,
		Range: days(t, "2026-06-14", "2026-06-14", time.UTC),
	})
	var order []string
	for _, row := range got.ByGroup {
		order = append(order, row.Group)
	}
	if want := []string{"execute", "review", "workers", "auxiliary"}; !slices.Equal(order, want) {
		t.Errorf("phase bands = %v, want %v whatever their size or the cap", order, want)
	}
}

func TestTheBandsPastTheCapFoldIntoOneResidual(t *testing.T) {
	t.Parallel()
	// The chart carries four and the rest are ONE row — and that row is
	// marked rather than named, so a model genuinely called "other" cannot be
	// mistaken for it.
	var cells []tokens.Cell
	for i, size := range []int{700, 600, 500, 400, 300, 200} {
		c := cell("2026-06-14", "ceo", "execute", string(rune('a'+i)), size)
		c.CacheReadTokens = size / 10
		cells = append(cells, c)
	}
	got := tokens.BucketDaily(cells, tokens.SeriesOptions{
		Group: tokens.GroupModel, Range: days(t, "2026-06-14", "2026-06-14", time.UTC),
	})
	if len(got.ByGroup) != tokens.DefaultSeriesGroups+1 {
		t.Fatalf("by_group = %d rows, want four bands and one residual", len(got.ByGroup))
	}
	last := got.ByGroup[len(got.ByGroup)-1]
	if !last.Other || last.Folded != 2 || last.Group != "" {
		t.Errorf("residual = %+v, want an unnamed other with two groups folded", last)
	}
	// EVERY FIELD OF THE BUCKET, the cache counts included: the residual
	// was summed field by field and forgot the two added last, so "other"
	// claimed none of its input was ever served from cache.
	if last.TotalTokens != 500 || last.CacheReadTokens != 50 || last.Calls != 2 {
		t.Errorf("residual = %+v, want 500 tokens, 50 cached, 2 calls", last.Bucket)
	}
	point := got.Points[0]
	if point.Residual.TotalTokens != 500 {
		t.Errorf("the bucket's residual = %d, want 500", point.Residual.TotalTokens)
	}
	if _, ok := point.Groups["f"]; ok {
		t.Error("a folded band is still its own key in the bucket")
	}
	if point.TotalTokens != 2700 {
		t.Errorf("the bucket's own total = %d, want every band", point.TotalTokens)
	}
}

func TestWhichBandsSurviveIsDecidedOverTheWholeWindow(t *testing.T) {
	t.Parallel()
	// A per-bucket decision would put a model in the chart for the days it
	// happened to lead and in the residual for the rest — one band that
	// appears and disappears, which reads as spend that stopped.
	var cells []tokens.Cell
	for i, name := range []string{"a", "b", "c", "d"} {
		cells = append(cells, cell("2026-06-14", "ceo", "execute", name, 1000-i))
	}
	// `small` leads the SECOND day outright and is still last overall.
	cells = append(cells, cell("2026-06-15", "ceo", "execute", "small", 5))
	got := tokens.BucketDaily(cells, tokens.SeriesOptions{
		Group: tokens.GroupModel, Range: days(t, "2026-06-14", "2026-06-15", time.UTC),
	})
	if _, ok := got.Points[1].Groups["small"]; ok {
		t.Error("a band that leads one bucket got its own key there")
	}
	if got.Points[1].Residual.TotalTokens != 5 {
		t.Errorf("the second day's residual = %d, want the folded band's 5",
			got.Points[1].Residual.TotalTokens)
	}
}

func TestACellThisGroupingPlacesNowhereStillCounts(t *testing.T) {
	t.Parallel()
	w := cell("2026-06-14", "ceo", "auxiliary", "haiku", 30)
	w.Worker = "reflect"
	stray := cell("2026-06-14", "ceo", "execute", "haiku", 1)
	stray.Worker = "reflect"
	got := tokens.BucketDaily([]tokens.Cell{
		w, cell("2026-06-14", "ceo", "execute", "sonnet", 70), stray,
	}, tokens.SeriesOptions{
		Group: tokens.GroupWorker, Range: days(t, "2026-06-14", "2026-06-14", time.UTC),
	})
	if got.Totals.TotalTokens != 101 || got.Grouped.TotalTokens != 30 {
		t.Errorf("totals %d grouped %d, want 101 and only the worker's 30",
			got.Totals.TotalTokens, got.Grouped.TotalTokens)
	}
	if len(got.ByGroup) != 1 || got.ByGroup[0].Group != "reflect" {
		t.Errorf("by_group = %+v, want one band", got.ByGroup)
	}
}

func TestAUnitBandCountsItsSeatsAndARootSeatHasItsOwn(t *testing.T) {
	t.Parallel()
	got := tokens.BucketDaily([]tokens.Cell{
		cell("2026-06-14", "ceo", "execute", "sonnet", 10),
		cell("2026-06-14", "dev", "execute", "sonnet", 20),
		cell("2026-06-15", "dev", "review", "sonnet", 5),
		cell("2026-06-15", "ops", "execute", "sonnet", 1),
	}, tokens.SeriesOptions{
		Group: tokens.GroupUnit, Range: days(t, "2026-06-14", "2026-06-15", time.UTC),
		Units: map[string]string{"dev": "Engineering", "ops": "Engineering"},
	})
	rows := map[string]tokens.GroupRow{}
	for _, row := range got.ByGroup {
		rows[row.Group] = row
	}
	if eng := rows["Engineering"]; eng.TotalTokens != 26 || eng.Seats != 2 {
		t.Errorf("Engineering = %+v, want 26 tokens over two seats", eng)
	}
	if root := rows["no unit"]; root.TotalTokens != 10 || root.Seats != 1 {
		t.Errorf("the root seat's band = %+v, want its own", root)
	}
}

func TestProviderAndSeatAreBandsOfTheirOwn(t *testing.T) {
	t.Parallel()
	a := cell("2026-06-14", "ceo", "execute", "sonnet", 10)
	b := cell("2026-06-14", "dev", "execute", "opus", 30)
	b.ProviderKey = a.ProviderKey
	opts := tokens.SeriesOptions{Range: days(t, "2026-06-14", "2026-06-14", time.UTC)}

	opts.Group = tokens.GroupProvider
	byProvider := tokens.BucketDaily([]tokens.Cell{a, b}, opts)
	if len(byProvider.ByGroup) != 1 || byProvider.ByGroup[0].TotalTokens != 40 {
		t.Errorf("by provider = %+v, want one entry serving both models", byProvider.ByGroup)
	}
	opts.Group = tokens.GroupSeat
	bySeat := tokens.BucketDaily([]tokens.Cell{a, b}, opts)
	if bySeat.ByGroup[0].Group != "dev" || bySeat.ByGroup[0].Handle != "dev" {
		t.Errorf("by seat = %+v, want the handle as the band and its link", bySeat.ByGroup[0])
	}
}

func TestACellOutsideTheWindowCountsNowhere(t *testing.T) {
	t.Parallel()
	got := tokens.BucketDaily([]tokens.Cell{
		cell("2026-06-13", "ceo", "execute", "sonnet", 99),
		cell("2026-06-14", "ceo", "execute", "sonnet", 1),
	}, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Range: days(t, "2026-06-14", "2026-06-14", time.UTC),
	})
	if got.Totals.TotalTokens != 1 {
		t.Errorf("totals = %d, want only the window's own day", got.Totals.TotalTokens)
	}
}

func TestOrderOfArrivalDoesNotChangeTheSeries(t *testing.T) {
	t.Parallel()
	forward := []tokens.Cell{
		cell("2026-06-14", "ceo", "execute", "sonnet", 80),
		cell("2026-06-15", "dev", "review", "opus", 50),
		cell("2026-06-15", "ceo", "subagent", "haiku", 10),
	}
	backward := []tokens.Cell{forward[2], forward[1], forward[0]}
	opts := tokens.SeriesOptions{
		Group: tokens.GroupSeat, Range: days(t, "2026-06-14", "2026-06-15", time.UTC),
	}
	a, _ := json.Marshal(tokens.BucketDaily(forward, opts))
	b, _ := json.Marshal(tokens.BucketDaily(backward, opts))
	if string(a) != string(b) {
		t.Errorf("the same cells in two orders gave two series:\n%s\n%s", a, b)
	}
}

// THE PREVIOUS WINDOW IS THE SAME NUMBER OF COMPANY DAYS ending the day before,
// never the same number of hours: across a clock change the two would be cut
// on different midnights and the comparison would report a change nobody made.
func TestThePreviousWindowIsTheSameDaysEndingTheDayBefore(t *testing.T) {
	t.Parallel()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 8 March 2026 is the spring-forward Sunday in New York.
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, ny)
	for _, n := range []int{7, 30, 90} {
		r := tokens.LastDays(n, now, ny)
		if r.Days() != n || r.Last.Label != "2026-03-14" {
			t.Fatalf("LastDays(%d) = %s..%s (%d days)", n, r.First.Label, r.Last.Label, r.Days())
		}
		prev := r.Previous()
		if prev.Days() != n || prev.Last.Next().Label != r.First.Label {
			t.Errorf("previous of %d days = %s..%s (%d days), want %d days ending the day before %s",
				n, prev.First.Label, prev.Last.Label, prev.Days(), n, r.First.Label)
		}
		if !prev.Until().Equal(r.Since()) {
			t.Errorf("previous of %d ends %s, this window begins %s — the two share no edge",
				n, prev.Until(), r.Since())
		}
	}
}

// THE HORIZON ADMITS WHAT THE USAGE APPLIER KEEPS. Its own cutoff keeps the day
// exactly `days` before today, so a ninety-day window's previous half — which
// begins 179 days back — is inside it, and the day past the floor is not.
func TestTheHorizonAdmitsExactlyTheDaysTheApplierKeeps(t *testing.T) {
	t.Parallel()
	today := period.At(period.Day, time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC), time.UTC)
	h := tokens.NewHorizon(181, today)
	if h.Floor != today.Shift(-181).Label || h.Days != 181 {
		t.Fatalf("horizon = %+v", h)
	}
	ninety := tokens.LastDays(90, today.Start, time.UTC)
	if !h.Admits(ninety.Previous()) {
		t.Errorf("the previous ninety days (%s..) are refused under a floor of %s",
			ninety.Previous().First.Label, h.Floor)
	}
	past := tokens.Range{First: today.Shift(-182), Last: today}
	if h.Admits(past) {
		t.Errorf("a window from %s is admitted under a floor of %s", past.First.Label, h.Floor)
	}
}

func TestARangeRunsForwards(t *testing.T) {
	t.Parallel()
	if _, err := tokens.DaysBetween("2026-06-15", "2026-06-14", time.UTC); err == nil {
		t.Error("an inverted range was accepted")
	}
	if _, err := tokens.DaysBetween("2026-02-29", "2026-03-01", time.UTC); err == nil {
		t.Error("a date the calendar does not have was accepted")
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
	// THE TWO A DAILY ROW CANNOT ANSWER are gone from the closed sets, so a
	// caller asking for them is refused rather than handed a guess.
	for _, gone := range []string{"hour", "minute"} {
		if tokens.Interval(gone).Valid() {
			t.Errorf("the interval %q is still accepted", gone)
		}
	}
	for _, gone := range []string{"turn", "project"} {
		if tokens.Group(gone).Valid() {
			t.Errorf("the group %q is still accepted", gone)
		}
	}
}

func TestAnEmptyWindowIsAnAxisOfEmptyDaysNotNulls(t *testing.T) {
	t.Parallel()
	got := tokens.BucketDaily(nil, tokens.SeriesOptions{
		Group: tokens.GroupPhase, Range: days(t, "2026-06-14", "2026-06-16", time.UTC),
	})
	if len(got.Points) != 3 {
		t.Errorf("points = %d, want the window's three empty days", len(got.Points))
	}
	body, _ := json.Marshal(got)
	if !strings.Contains(string(body), `"by_group":[]`) || !strings.Contains(string(body), `"groups":{}`) {
		t.Errorf("empty lists marshalled as null, which throws in the browser: %s", body)
	}
}
