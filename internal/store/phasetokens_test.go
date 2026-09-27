package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// seedPhase writes one completed phase with a price and the promoted counts.
func seedPhase(t *testing.T, log *store.EventLog, id string, at time.Time,
	role string, tokens int, usd float64) {

	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"role": role, "phase": "execute", "model": "sonnet",
		"input_tokens": tokens, "output_tokens": 0, "total_tokens": tokens,
		"cost_usd": usd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "agent_phase_completed", Time: at,
		Category: "lifecycle", Actor: role,
		Tags: map[string]string{
			"agent_role": role, "phase": "execute", "model": "sonnet",
			"input_tokens": "0", "output_tokens": "0",
		},
		Payload: payload,
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// THE PRICE IS A COLUMN NOWHERE, and it is the one value here still read out
// of the payload.
//
// Only a subscription coding CLI reports one, so promoting it would be a
// migration and a column that is NULL on almost every row of the table. What
// matters is that the read carries it at all: without it the rollup's currency
// total is zero for every window, which renders as a company that has never
// spent a cent rather than as one whose backend does not quote.
func TestAPhasesPriceReachesTheRollup(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	seedPhase(t, log, "p1", base, "PM", 100, 0.25)
	seedPhase(t, log, "p2", base.Add(time.Minute), "PM", 50, 0)

	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("records = %d, want two", len(got))
	}
	var total float64
	for _, r := range got {
		total += r.CostUSD
	}
	if total != 0.25 {
		t.Errorf("summed price = %v, want 0.25 — the payload's own cost_usd", total)
	}
}

// A WINDOW IS TWO INSTANTS, which a day count cannot name.
//
// A cost explorer's range control produces two edges, and its
// compare-to-previous asks for the window immediately before the one on
// screen. Neither is "N days back from now", and a reader that could only be
// asked the second question answers the first one with the wrong rows.
func TestAnInstantWindowSelectsExactlyItsOwnRows(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	seedPhase(t, log, "before", base.Add(-time.Minute), "PM", 1, 0)
	seedPhase(t, log, "edge", base, "PM", 2, 0)
	seedPhase(t, log, "inside", base.Add(30*time.Minute), "PM", 4, 0)
	seedPhase(t, log, "after", base.Add(time.Hour), "PM", 8, 0)

	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{
		Since: base, Until: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	var sum int
	for _, r := range got {
		sum += r.TotalTokens
	}
	// 2 + 4: the lower edge is inclusive and the upper exclusive, so two
	// adjacent windows share their boundary instant without either losing
	// it or counting it twice.
	if sum != 6 {
		t.Errorf("tokens in [base, base+1h) = %d, want 6 — got %d rows", sum, len(got))
	}
}

// AND THE FLOOR IS REPORTED, not silently applied.
//
// A request further back than the table's retention cannot return more rows.
// Honouring it would make a scan of the whole table look like a supported
// query; applying it silently would put a decade's heading over a month of
// data, which is a lie about the numbers beside it.
func TestAWindowBelowTheFloorIsRaisedAndSaidSo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	q := store.PhaseTokenQuery{Since: now.AddDate(-3, 0, 0)}
	since, until := q.Window(now)
	want := now.Add(-time.Duration(store.MaxPhaseTokenDays) * 24 * time.Hour)
	if !since.Equal(want) {
		t.Errorf("since = %s, want the floor at %s", since, want)
	}
	// AN UNBOUNDED TOP EDGE IS REPORTED AS NOW, never as the zero time:
	// that is the instant the query actually covers through, and every
	// caller that got the zero wrote the same fixup back — which is how a
	// chart and the figures above it came to disagree about where a window
	// ends.
	if !until.Equal(now) {
		t.Errorf("until = %s, want now (%s)", until, now)
	}

	// A DAY COUNT STILL WORKS, and past the ceiling lands on the same floor.
	if since, _ := (store.PhaseTokenQuery{SinceDays: 9000}).Window(now); !since.Equal(want) {
		t.Errorf("since from a day count = %s, want the floor at %s", since, want)
	}
	// AND AN ABSENT ONE IS THE DEFAULT WINDOW, not the floor: a caller that
	// named nothing asked for a week, and answering with a month would make
	// every unparameterised read scan four times what it meant to.
	if since, _ := (store.PhaseTokenQuery{}).Window(now); !since.Equal(
		now.Add(-time.Duration(store.DefaultPhaseTokenDays) * 24 * time.Hour)) {
		t.Errorf("since with nothing named = %s, want the default window", since)
	}
}

// AN INVERTED WINDOW COLLAPSES rather than reading as a quiet company.
//
// until < since names no rows, and a reader that let it through would hand the
// SQL a range the store answers as empty — which is indistinguishable from a
// company that spent nothing.
func TestAnInvertedWindowCoversNothingRatherThanEverything(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	since, until := store.PhaseTokenQuery{
		Since: now.Add(-time.Hour), Until: now.Add(-2 * time.Hour),
	}.Window(now)
	if !since.Equal(until) {
		t.Errorf("window = %s..%s, want an empty one at the later edge", since, until)
	}
}

// storeEvent writes one event the way the engine's event writer does: through
// [store.RecordFor], which is where a spend record's columns are derived.
func storeEvent(t *testing.T, log *store.EventLog, ev *events.Event) {
	t.Helper()
	rec, ok, err := store.RecordFor(ev)
	if err != nil || !ok {
		t.Fatalf("RecordFor(%s) = %v, %v; want a row", ev.Type, ok, err)
	}
	if err := log.Append(t.Context(), rec); err != nil {
		t.Fatalf("append %s: %v", ev.Type, err)
	}
}

// stamped stamps an event with an instant of the test's choosing.
func stamped(ev *events.Event, at time.Time) *events.Event {
	ev.Timestamp = at
	return ev
}

// AN AUXILIARY COMPLETION IS SPEND EVERY STORED ROLLUP FOLDS. A learning
// worker's, a background pass's and the prefetch's completions are no phase's,
// so each publishes an auxiliary_call_completed of its own, and a read that
// selected phase records alone left that spend on the budget counter and off
// every breakdown, series and seeded live window. It reaches all of them here
// as its own phase, its own worker and the turn it served.
func TestAnAuxiliaryCompletionReachesTheBreakdownAndTheSeries(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	storeEvent(t, log, stamped(events.New(types.AgentPhaseCompleted{
		RoleName: "PM", TurnID: "run-1", WorkKey: "wk-1", Phase: types.PhaseExecute,
		Model: "sonnet", InputTokens: 90, OutputTokens: 10, TotalTokens: 100,
	}, events.TraceContext{}), base))
	storeEvent(t, log, stamped(events.New(types.AuxiliaryCallCompleted{
		RoleName: "PM", TurnID: "run-1", WorkKey: "wk-1", Phase: types.PhaseAuxiliary,
		Worker: "persist_decider", Model: "haiku", ProviderKey: "aux",
		InputTokens: 30, OutputTokens: 5, TotalTokens: 35,
	}, events.TraceContext{}), base.Add(time.Minute)))

	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	rollup := tokens.Aggregate(got, tokens.Options{})
	if rollup.Totals.TotalTokens != 135 || rollup.Totals.Calls != 2 {
		t.Fatalf("totals = %+v, want the phase's 100 and the auxiliary call's 35", rollup.Totals)
	}
	byPhase := map[string]int{}
	for _, row := range rollup.ByPhase {
		byPhase[row.Phase] = row.TotalTokens
	}
	if byPhase["auxiliary"] != 35 {
		t.Errorf("by_phase = %v, want the auxiliary call under its own phase", byPhase)
	}
	if len(rollup.ByWorker) != 1 || rollup.ByWorker[0].Phase != "auxiliary" ||
		rollup.ByWorker[0].Worker != "persist_decider" || rollup.ByWorker[0].TotalTokens != 35 {
		t.Errorf("by_worker = %+v, want the persist decider's 35", rollup.ByWorker)
	}
	if len(rollup.ByTurn) != 1 || rollup.ByTurn[0].TotalTokens != 135 || rollup.ByTurn[0].WorkKey != "wk-1" {
		t.Errorf("by_turn = %+v, want the call on the turn it served", rollup.ByTurn)
	}

	series := tokens.Bucketed(got, tokens.SeriesOptions{
		Group: tokens.GroupWorker, Since: base, Until: base.Add(time.Hour),
	})
	if len(series.ByGroup) != 1 || series.ByGroup[0].Group != "auxiliary/persist_decider" ||
		series.ByGroup[0].TotalTokens != 35 {
		t.Errorf("worker bands = %+v, want the persist decider's", series.ByGroup)
	}

	// AND THE LIVE WINDOW'S SEED, which reads the tail.
	tail, _, err := log.PhaseTokenTail(t.Context(), store.PhaseTokenQuery{SinceDays: 1}, 10)
	if err != nil {
		t.Fatalf("phase token tail: %v", err)
	}
	if len(tail) != 2 || tail[0].Phase != "auxiliary" {
		t.Errorf("tail = %+v, want both records, the auxiliary call newest", tail)
	}
}

// A TAIL ACROSS BOTH SPEND TYPES IS THE NEWEST OF BOTH. Each type is its own
// read, and the tail is their merge cut to the limit — so the records it keeps
// are the ones one ordered read of the two would have kept, and whether the
// window held more is still read rather than inferred.
func TestATailAcrossBothSpendTypesKeepsTheNewestOfBoth(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	now := time.Now().UTC()
	for i, ev := range []*events.Event{
		events.New(types.AgentPhaseCompleted{RoleName: "PM", Phase: types.PhaseExecute, TotalTokens: 1},
			events.TraceContext{}),
		events.New(types.AuxiliaryCallCompleted{RoleName: "PM", Phase: types.PhaseAuxiliary, TotalTokens: 2},
			events.TraceContext{}),
		events.New(types.AgentPhaseCompleted{RoleName: "PM", Phase: types.PhaseReview, TotalTokens: 4},
			events.TraceContext{}),
		events.New(types.AuxiliaryCallCompleted{RoleName: "PM", Phase: types.PhaseAuxiliary, TotalTokens: 8},
			events.TraceContext{}),
	} {
		storeEvent(t, log, stamped(ev, now.Add(-time.Duration(4-i)*time.Minute)))
	}
	sum := func(rs []tokens.Record) int {
		n := 0
		for _, r := range rs {
			n += r.TotalTokens
		}
		return n
	}
	tail, more, err := log.PhaseTokenTail(t.Context(), store.PhaseTokenQuery{SinceDays: 1}, 3)
	if err != nil {
		t.Fatalf("phase token tail: %v", err)
	}
	if sum(tail) != 14 || !more || tail[0].TotalTokens != 8 || tail[2].TotalTokens != 2 {
		t.Errorf("tail of 3 = %+v (more=%v), want the newest three, newest first, and more=true", tail, more)
	}
	if tail, more, _ := log.PhaseTokenTail(t.Context(), store.PhaseTokenQuery{SinceDays: 1}, 4); sum(tail) != 15 || more {
		t.Errorf("tail of exactly the window = %+v (more=%v), want all four and more=false", tail, more)
	}
}

// A RECORD'S MODEL SPLIT AND ITS UNREPORTED MARK REACH THE STORED ROLLUP. Both
// are payload members rather than columns, so a read of the columns alone
// counted every phase under its first model and stated a coding run's floor as
// its whole — the live window, which reads the payload, answered otherwise.
func TestAStoredRecordsSplitAndUnreportedMarkReachTheRollup(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	two := types.AgentPhaseCompleted{
		RoleName: "PM", TurnID: "run-1", Phase: types.PhaseExecute, Model: "sonnet",
		InputTokens: 150, OutputTokens: 15, TotalTokens: 165,
		Models: []types.ModelSpend{
			{Model: "sonnet", InputTokens: 100, OutputTokens: 10},
			{Model: "haiku", InputTokens: 50, OutputTokens: 5},
		},
	}
	storeEvent(t, log, stamped(events.New(two, events.TraceContext{}), at))
	floor := types.AgentPhaseCompleted{
		RoleName: "PM", TurnID: "run-2", Phase: types.PhaseExecute, Model: "sonnet",
		InputTokens: 10, TotalTokens: 10, CostUSD: 0.5,
	}
	floor.AddRun(types.RunSpend{Collected: true, Models: []types.ModelSpend{{InputTokens: 30, OutputTokens: 5}}})
	storeEvent(t, log, stamped(events.New(floor, events.TraceContext{}), at.Add(time.Minute)))

	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	rollup := tokens.Aggregate(got, tokens.Options{})
	byModel := map[string]tokens.ModelRow{}
	for _, row := range rollup.ByModel {
		byModel[row.Model] = row
	}
	if byModel["sonnet"].TotalTokens != 120 || byModel["haiku"].TotalTokens != 55 {
		t.Errorf("by_model = %+v, want sonnet's 110 and the floor's own 10, and haiku's 55", rollup.ByModel)
	}
	if byModel["unknown"].TotalTokens != 35 || byModel[tokens.UnmeasuredModel].Calls != 1 {
		t.Errorf("by_model = %+v, want the run's unnamed 35 and its unmeasured rest", rollup.ByModel)
	}
	if rollup.Totals.UnreportedCalls != 1 || rollup.Totals.CostUSD != 0.5 {
		t.Errorf("totals = %+v, want one unreported call and the run's price", rollup.Totals)
	}
}
