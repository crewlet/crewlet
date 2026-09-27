package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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

// A PHASE'S PRICE REACHES THE ROLLUP, from the payload member the writer reads
// it out of into its column.
//
// Only a subscription coding CLI quotes one, so it is zero on most records.
// What matters is that the read carries it at all: without it the rollup's
// currency total is zero for every window, which renders as a company that has
// never spent a cent rather than as one whose backend does not quote.
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

// A TAIL THAT STARTS INSIDE ONE INSTANT KEEPS THE RECORDS THE TABLE'S ORDER
// PUTS NEWEST. Records share an instant routinely, and the table orders them by
// (time, id) descending: a tail cut between two records of one instant keeps
// the one with the greater id, whichever spend type each is, and keeps each
// record once.
func TestATailCutInsideAnInstantKeepsTheTablesNewest(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	for _, rec := range []struct {
		id, eventType string
		at            time.Time
		tokens        int
	}{
		{"e-1", "agent_phase_completed", at, 1},
		{"e-2", "auxiliary_call_completed", at.Add(time.Second), 2},
		{"e-3", "agent_phase_completed", at.Add(time.Second), 4},
		{"e-4", "auxiliary_call_completed", at.Add(2 * time.Second), 8},
	} {
		payload, err := json.Marshal(map[string]any{
			"role": "PM", "phase": "execute", "model": "sonnet",
			"input_tokens": rec.tokens, "total_tokens": rec.tokens,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: rec.id, Type: rec.eventType, Time: rec.at,
			Category: "lifecycle", Actor: "PM", Payload: payload,
		}); err != nil {
			t.Fatalf("append %s: %v", rec.id, err)
		}
	}

	tail, more, err := log.PhaseTokenTail(t.Context(), store.PhaseTokenQuery{SinceDays: 1}, 2)
	if err != nil {
		t.Fatalf("phase token tail: %v", err)
	}
	ids := make([]string, len(tail))
	for i, r := range tail {
		ids[i] = r.EventID
	}
	if !slices.Equal(ids, []string{"e-4", "e-3"}) || !more {
		t.Errorf("tail of 2 = %v (more=%v), want e-4 and then e-3 — the greater id of the "+
			"instant e-2 shares — and more=true", ids, more)
	}
	tail, more, err = log.PhaseTokenTail(t.Context(), store.PhaseTokenQuery{SinceDays: 1}, 3)
	if err != nil {
		t.Fatalf("phase token tail: %v", err)
	}
	ids = ids[:0]
	for _, r := range tail {
		ids = append(ids, r.EventID)
	}
	if !slices.Equal(ids, []string{"e-4", "e-3", "e-2"}) || !more {
		t.Errorf("tail of 3 = %v (more=%v), want the whole instant once, newest first, and more=true", ids, more)
	}
}

// A RECORD'S MODEL SPLIT AND ITS UNREPORTED MARK REACH THE STORED ROLLUP. A
// read without them counts every phase under its first model and states a
// coding run's floor as its whole, while the live window, which reads both off
// the event, answers otherwise.
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

// ONE TYPE PAST THE LIMIT IS A WINDOW THAT HOLDS MORE, and the record read one
// past the limit is the only evidence of it. Taken `limit` deep instead, a
// window holding one record more than the limit, every one of a single type,
// answers exactly the limit's worth, says nothing was left behind, and the seed
// heads its rollup with the whole window over a tail that left the oldest
// record out. Each type on its own, since each is its own read and the merge
// takes from one only what it needs; and a window holding exactly the limit
// says it holds no more.
func TestATailOfOneTypeSaysWhenTheWindowHoldsMore(t *testing.T) {
	t.Parallel()
	const limit = 3
	for _, eventType := range []string{"agent_phase_completed", "auxiliary_call_completed"} {
		for _, c := range []struct {
			held int
			more bool
		}{{limit + 1, true}, {limit, false}} {
			log := open(t).Events()
			now := time.Now().UTC()
			for i := range c.held {
				if err := log.Append(t.Context(), store.EventRecord{
					ID: fmt.Sprintf("r-%d", i), Type: eventType,
					Time:     now.Add(-time.Duration(c.held-i) * time.Minute),
					Category: "lifecycle", Actor: "PM",
					Payload: []byte(`{"role":"PM","phase":"execute","total_tokens":1}`),
				}); err != nil {
					t.Fatalf("append: %v", err)
				}
			}
			tail, more, err := log.PhaseTokenTail(t.Context(), store.PhaseTokenQuery{SinceDays: 1}, limit)
			if err != nil {
				t.Fatalf("%s × %d: phase token tail: %v", eventType, c.held, err)
			}
			ids := make([]string, len(tail))
			for i, r := range tail {
				ids[i] = r.EventID
			}
			want := make([]string, 0, limit)
			for i := c.held - 1; i >= c.held-limit; i-- {
				want = append(want, fmt.Sprintf("r-%d", i))
			}
			if !slices.Equal(ids, want) || more != c.more {
				t.Errorf("%s × %d, limit %d: tail %v (more=%v), want %v (more=%v)",
					eventType, c.held, limit, ids, more, want, c.more)
			}
		}
	}
}

// THE SPEND READ TAKES ITS VALUES FROM THE COLUMNS, the price, the split and
// the unreported mark among them (schema/0032), and parses no payload. A record
// appended with a spend that disagrees with its own payload is how that shows:
// both reads answer what the columns hold. A read that went back to the payload
// for any of the three would parse every phase record's prompts again, on the
// one read with a time budget.
func TestTheSpendReadTakesItsValuesFromTheColumns(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "p1", Type: "agent_phase_completed", Time: time.Now().UTC().Add(-time.Hour),
		Category: "lifecycle", Actor: "PM", Tags: map[string]string{"agent_role": "PM"},
		Payload: []byte(`{"role":"PM","phase":"execute","model":"from-payload","total_tokens":1,
			"cost_usd":9,"models":[{"model":"from-payload","input_tokens":1}],"run_spend_unreported":false}`),
		Spend: &store.Spend{
			Phase: "execute", Model: "sonnet", InputTokens: 5, TotalTokens: 5, CostUSD: 0.25,
			Models: []tokens.ModelSpend{{Model: "haiku", InputTokens: 5}}, Unreported: true,
		},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	whole, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	tail, _, err := log.PhaseTokenTail(t.Context(), store.PhaseTokenQuery{SinceDays: 1}, 10)
	if err != nil {
		t.Fatalf("phase token tail: %v", err)
	}
	for name, got := range map[string][]tokens.Record{"PhaseTokens": whole, "PhaseTokenTail": tail} {
		if len(got) != 1 {
			t.Fatalf("%s: %d records, want the one appended", name, len(got))
		}
		r := got[0]
		if r.CostUSD != 0.25 || !r.Unreported || r.Model != "sonnet" ||
			!slices.Equal(r.Models, []tokens.ModelSpend{{Model: "haiku", InputTokens: 5}}) {
			t.Errorf("%s: record = %+v, want the columns' $0.25, haiku split and floor mark", name, r)
		}
	}
}

// THE SPEND READS NEED ONE CONNECTION. Every statement is read to its end
// before the next begins, the tail's merge of the two types included, so a
// pool of one serves them: at `store.max_open_conns: 1`, a read that opened
// its second statement while the first was still open would wait on the pool
// for a connection only it could give back.
func TestTheSpendReadsNeedOneConnection(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "one.db"), store.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := db.Events()
	now := time.Now().UTC()
	for i, eventType := range []string{"agent_phase_completed", "auxiliary_call_completed",
		"agent_phase_completed", "auxiliary_call_completed"} {
		if err := log.Append(t.Context(), store.EventRecord{
			ID: fmt.Sprintf("r-%d", i), Type: eventType, Time: now.Add(-time.Duration(4-i) * time.Minute),
			Category: "lifecycle", Actor: "PM", Payload: []byte(`{"role":"PM","total_tokens":1}`),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	whole, err := log.PhaseTokens(ctx, store.PhaseTokenQuery{SinceDays: 1})
	if err != nil || len(whole) != 4 {
		t.Fatalf("PhaseTokens on a pool of one = %d records, %v; want all four", len(whole), err)
	}
	tail, more, err := log.PhaseTokenTail(ctx, store.PhaseTokenQuery{SinceDays: 1}, 3)
	if err != nil || len(tail) != 3 || !more {
		t.Fatalf("PhaseTokenTail on a pool of one = %d records (more=%v), %v; want three and more", len(tail), more, err)
	}
}

// A TAIL THAT CANNOT READ SAYS THE WINDOW MAY HOLD MORE. Nothing it read says
// the window is empty, and the caller that seeds a rollup from it heads that
// rollup with what the records cover: answered "no more", an empty tail would
// head the live records with the whole window they do not cover.
func TestATailThatCannotReadSaysTheWindowMayHoldMore(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tail, more, err := log.PhaseTokenTail(ctx, store.PhaseTokenQuery{SinceDays: 1}, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a tail read on a cancelled context answered %v, want its cause", err)
	}
	if tail == nil || len(tail) != 0 || !more {
		t.Errorf("tail = %v (nil %v), more=%v; want an allocated empty tail and more", tail, tail == nil, more)
	}
}

// A TAIL LONGER THAN ONE STATEMENT KEEPS EVERY RECORD ONCE, in the table's
// order. The tail reads each type a statement at a time, each starting past
// the last record the one before it read, and records share an instant
// routinely: a statement that started at the instant rather than past the
// record would read one twice or step over its neighbours.
func TestATailLongerThanOneStatementKeepsEveryRecordOnce(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	// Three records to an instant, so a statement's boundary falls inside one
	// whichever record ends it; and an auxiliary call every fifth instant, so
	// the merge takes from both types across the boundaries.
	var want []string
	for i := range 1200 {
		eventType := "agent_phase_completed"
		if i%15 == 7 {
			eventType = "auxiliary_call_completed"
		}
		id := fmt.Sprintf("r-%04d", i)
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: eventType, Time: base.Add(time.Duration(i/3) * time.Millisecond),
			Category: "lifecycle", Actor: "PM", Payload: []byte(`{"role":"PM","total_tokens":1}`),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		want = append(want, id)
	}
	slices.Reverse(want)
	tail, more, err := log.PhaseTokenTail(t.Context(), store.PhaseTokenQuery{SinceDays: 1}, 1100)
	if err != nil {
		t.Fatalf("phase token tail: %v", err)
	}
	ids := make([]string, len(tail))
	for i, r := range tail {
		ids[i] = r.EventID
	}
	if !slices.Equal(ids, want[:1100]) || !more {
		t.Errorf("tail of 1100 over 1200 records differs from the table's newest 1100 (more=%v): "+
			"got %d records, first %v, last %v", more, len(ids), ids[:3], ids[len(ids)-3:])
	}
}
