package queries_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// seedSpend writes one completed phase, with the role and price it carried.
func seedSpend(t *testing.T, log *store.EventLog, id string, at time.Time,
	role, phase string, total int, usd float64) {

	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"role": role, "phase": phase, "model": "sonnet",
		"input_tokens": total, "output_tokens": 0, "total_tokens": total,
		"cost_usd": usd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "agent_phase_completed", Time: at,
		Category: "lifecycle", Actor: role,
		Tags:    map[string]string{"agent_role": role, "phase": phase},
		Payload: payload,
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

func seriesOver(t *testing.T, r *queries.Registry, params map[string]any) tokens.Series {
	t.Helper()
	got := askRaw(t, r, "token_series", params)
	out, ok := got.(tokens.Series)
	if !ok {
		t.Fatalf("token_series answered %T, want a series", got)
	}
	return out
}

// THE SPEND WITH A TIME AXIS, which the breakdown has no dimension for.
func TestTheSeriesBucketsTheWindowItWasAsked(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	seedSpend(t, log, "a", base.Add(10*time.Minute), "PM", "plan", 100, 0)
	seedSpend(t, log, "b", base.Add(2*time.Hour+5*time.Minute), "PM", "execute", 40, 0.5)

	r := registryOver(t, queries.Sources{Spend: log})
	got := seriesOver(t, r, map[string]any{
		"group":  "phase",
		"bucket": "hour",
		"since":  base.Format(time.RFC3339),
		"until":  base.Add(3 * time.Hour).Format(time.RFC3339),
	})

	if len(got.Points) != 3 {
		t.Fatalf("points = %d, want one per hour", len(got.Points))
	}
	if got.Points[0].TotalTokens != 100 || got.Points[1].Calls != 0 ||
		got.Points[2].TotalTokens != 40 {
		t.Errorf("buckets = %+v", got.Points)
	}
	if got.Totals.CostUSD != 0.5 || got.Totals.PricedCalls != 1 {
		t.Errorf("totals = %+v, want the one price the payload carried", got.Totals)
	}
	if got.Group != tokens.GroupPhase || got.Interval != tokens.IntervalHour {
		t.Errorf("the answer does not echo what it was built with: %+v", got)
	}
}

// AN UNKNOWN GROUP OR BUCKET IS REFUSED NAMING WHAT IS ACCEPTED.
//
// A chart legended by one dimension over another's bands is worse than an
// error, and falling through to whichever branch the switch ended on is
// exactly how that happens.
func TestAnUnknownDimensionIsRefusedRatherThanDefaulted(t *testing.T) {
	t.Parallel()
	r := registryOver(t, queries.Sources{Spend: openStore(t).Events()})
	for _, params := range []map[string]any{
		{"group": "project"},
		{"bucket": "minute"},
	} {
		if _, err := r.Answer(t.Context(), "token_series", params, ""); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%v answered err = %v, want ErrBadParams", params, err)
		}
	}
	// AND AN ABSENT ONE IS THE DEFAULT, because a chart with no controls
	// touched still has to draw something.
	got := seriesOver(t, r, nil)
	if got.Group != tokens.GroupPhase || got.Interval != tokens.IntervalHour {
		t.Errorf("defaults = %s/%s", got.Group, got.Interval)
	}
}

// COMPARE-TO-PREVIOUS IS A SHIFT OF THE WINDOW, computed here.
//
// A client subtracting in local time produces two windows of different lengths
// across a DST boundary, and the chart then reports a change nobody made.
func TestPreviousShiftsTheWindowByItsOwnLength(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	seedSpend(t, log, "old", base.Add(-30*time.Minute), "PM", "plan", 7, 0)
	seedSpend(t, log, "new", base.Add(30*time.Minute), "PM", "plan", 11, 0)

	r := registryOver(t, queries.Sources{Spend: log})
	params := map[string]any{
		"since": base.Format(time.RFC3339),
		"until": base.Add(time.Hour).Format(time.RFC3339),
	}
	now := seriesOver(t, r, params)
	if now.Totals.TotalTokens != 11 {
		t.Errorf("the window itself = %d, want 11", now.Totals.TotalTokens)
	}

	params["previous"] = true
	prev := seriesOver(t, r, params)
	if prev.Totals.TotalTokens != 7 {
		t.Errorf("the previous window = %d, want 7", prev.Totals.TotalTokens)
	}
	if prev.Until != base.Format(time.RFC3339) {
		t.Errorf("the previous window ends at %s, want the current one's start %s",
			prev.Until, base.Format(time.RFC3339))
	}
}

// AND IT NEEDS BOTH EDGES: the window before an open-ended one has no length.
func TestPreviousWithoutAWindowIsRefused(t *testing.T) {
	t.Parallel()
	r := registryOver(t, queries.Sources{Spend: openStore(t).Events()})
	_, err := r.Answer(t.Context(), "token_series", map[string]any{
		"previous": true, "since": time.Now().UTC().Format(time.RFC3339),
	}, "")
	if !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("err = %v, want ErrBadParams", err)
	}
}

// AN UNBOUNDED TOP EDGE RUNS TO NOW, not to the newest record.
//
// A company that has been quiet for six hours has six empty buckets on the
// right of its chart. An axis that stopped where the spending did would read
// as a live company, which is the one thing this screen must not do.
func TestAnOpenWindowDrawsTheQuietHoursAtTheEnd(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	now := time.Now().UTC().Truncate(time.Hour)
	seedSpend(t, log, "quiet", now.Add(-6*time.Hour).Add(time.Minute), "PM", "plan", 3, 0)

	r := registryOver(t, queries.Sources{
		Spend: log,
		Now:   func() time.Time { return now.Add(30 * time.Minute) },
	})
	got := seriesOver(t, r, map[string]any{
		"since": now.Add(-6 * time.Hour).Format(time.RFC3339),
	})
	if len(got.Points) != 7 {
		t.Fatalf("points = %d, want the six quiet hours and the one with spend",
			len(got.Points))
	}
	if got.Points[0].TotalTokens != 3 {
		t.Errorf("the first bucket = %+v", got.Points[0])
	}
	if last := got.Points[6]; last.Calls != 0 {
		t.Errorf("the newest bucket = %+v, want empty", last)
	}
}

// THE AXIS IS LABELLED WITH THE WINDOW SERVED, not the one asked for.
//
// The store floors a request at its own retention, so a chart headed with the
// year it was asked for over a month of bars states a span its bars do not
// cover — and the comparison a reader makes across two captures is then
// between two different windows both called the same thing.
func TestAWindowBelowTheStoresFloorIsDrawnAndLabelledAtTheFloor(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Hour)
	r := registryOver(t, queries.Sources{
		Spend: openStore(t).Events(),
		Now:   func() time.Time { return now },
	})
	asked := now.AddDate(-3, 0, 0)
	got := seriesOver(t, r, map[string]any{
		"bucket": "day",
		"since":  asked.Format(time.RFC3339),
		"until":  now.Format(time.RFC3339),
	})
	if got.Since == asked.Format(time.RFC3339) {
		t.Fatalf("since = %s, which is what was asked rather than what the "+
			"store can serve", got.Since)
	}
	floor := now.Add(-time.Duration(store.MaxPhaseTokenDays) * 24 * time.Hour)
	if want := tokens.IntervalDay.Start(floor).Format(time.RFC3339); got.Since != want {
		t.Errorf("since = %s, want the floor's own day at %s", got.Since, want)
	}
}
