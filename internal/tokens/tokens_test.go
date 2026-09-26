package tokens_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tokens"
)

func rec(role, phase, model, turn, at string, in, out int) tokens.Record {
	return tokens.Record{
		EventID: at + role + phase, Timestamp: at,
		AgentRole: role, AgentID: "id-" + role,
		Phase: phase, Model: model, TurnID: turn,
		InputTokens: in, OutputTokens: out, TotalTokens: in + out,
	}
}

func TestOneTurnFoldsIntoEveryDimension(t *testing.T) {
	t.Parallel()
	got := tokens.Aggregate([]tokens.Record{
		rec("CEO", "plan", "sonnet", "t1", "2026-06-14T12:00:00Z", 60, 20),
		rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:05Z", 90, 30),
		rec("CEO", "review", "haiku", "t1", "2026-06-14T12:00:09Z", 40, 10),
	}, tokens.Options{Handles: map[string]string{"CEO": "ceo"}, Since: since, Until: until})

	if got.Totals.TotalTokens != 250 || got.Totals.Calls != 3 {
		t.Errorf("totals = %+v", got.Totals)
	}
	if len(got.ByPhase) != 3 || got.ByPhase[0].Phase != "execute" {
		t.Errorf("by_phase = %+v, want biggest first", got.ByPhase)
	}
	if len(got.ByModel) != 2 || got.ByModel[0].Model != "sonnet" {
		t.Errorf("by_model = %+v", got.ByModel)
	}
	if len(got.ByAgent) != 1 || got.ByAgent[0].Handle != "ceo" {
		t.Errorf("by_agent = %+v", got.ByAgent)
	}
	if n := got.ByAgent[0].ByPhase["plan"].TotalTokens; n != 80 {
		t.Errorf("the agent's plan bucket = %d, want 80", n)
	}
	if len(got.ByTurn) != 1 {
		t.Fatalf("by_turn = %+v", got.ByTurn)
	}
	turn := got.ByTurn[0]
	if turn.StartedAt != "2026-06-14T12:00:00Z" || turn.EndedAt != "2026-06-14T12:00:09Z" {
		t.Errorf("turn bounds = %s..%s, want the earliest and latest phase",
			turn.StartedAt, turn.EndedAt)
	}
	if got.AggregatedThrough != "2026-06-14T12:00:09Z" {
		t.Errorf("aggregated_through = %q, want the latest record", got.AggregatedThrough)
	}
}

func TestOrderOfArrivalDoesNotChangeTheAnswer(t *testing.T) {
	t.Parallel()
	// The live window is append-ordered by arrival and the store's is by
	// (time, id) DESCENDING, so the same records reach this in opposite
	// orders — and a rollup that depended on order would make the live
	// number and the queried one disagree for no visible reason.
	forward := []tokens.Record{
		rec("CEO", "plan", "sonnet", "t1", "2026-06-14T12:00:00Z", 60, 20),
		rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:05Z", 90, 30),
		rec("CTO", "plan", "haiku", "t2", "2026-06-14T12:00:07Z", 10, 5),
	}
	backward := []tokens.Record{forward[2], forward[1], forward[0]}

	a, _ := json.Marshal(tokens.Aggregate(forward, tokens.Options{}))
	b, _ := json.Marshal(tokens.Aggregate(backward, tokens.Options{}))
	if string(a) != string(b) {
		t.Errorf("the rollup depends on arrival order:\n%s\n%s", a, b)
	}
}

func TestTiesBreakOnANameRatherThanOnMapOrder(t *testing.T) {
	t.Parallel()
	// Go randomises map iteration, so rows with equal tokens would order
	// differently on every call — which makes a diff of two captures
	// unreadable and a golden test impossible.
	records := []tokens.Record{
		rec("A", "plan", "m", "t1", "2026-06-14T12:00:00Z", 5, 5),
		rec("B", "execute", "m", "t2", "2026-06-14T12:00:00Z", 5, 5),
		rec("C", "review", "m", "t3", "2026-06-14T12:00:00Z", 5, 5),
	}
	first, _ := json.Marshal(tokens.Aggregate(records, tokens.Options{}))
	for range 20 {
		next, _ := json.Marshal(tokens.Aggregate(records, tokens.Options{}))
		if string(next) != string(first) {
			t.Fatalf("unstable ordering:\n%s\n%s", first, next)
		}
	}
}

// A WORKER COUNTS ON THE TWO PHASES THAT NAME ONE, AND ON NO OTHER. A
// delegated task's record names the `workers:` template it ran and a learning
// worker's names that worker; both are a worker's spend. A Worker on any other
// phase is a stray value, and keying on a non-empty name alone would fold it
// into a worker's total as spend that worker never made.
func TestAWorkerCountsOnlyOnThePhasesThatNameOne(t *testing.T) {
	t.Parallel()
	aux := rec("CEO", "auxiliary", "haiku", "t1", "2026-06-14T12:00:00Z", 10, 5)
	aux.Worker = "reflect"
	delegated := rec("CEO", "subagent", "haiku", "t1", "2026-06-14T12:00:01Z", 200, 100)
	delegated.Worker = "researcher"
	stray := rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:02Z", 100, 50)
	stray.Worker = "reflect"
	inline := rec("CEO", "subagent", "haiku", "t1", "2026-06-14T12:00:03Z", 7, 0)

	got := tokens.Aggregate([]tokens.Record{aux, delegated, stray, inline}, tokens.Options{})
	want := []tokens.WorkerRow{
		{Phase: "subagent", Worker: "researcher"},
		{Phase: "auxiliary", Worker: "reflect"},
	}
	if len(got.ByWorker) != len(want) {
		t.Fatalf("by_worker = %+v, want the template's row and the learning worker's", got.ByWorker)
	}
	for i, w := range want {
		if got.ByWorker[i].Phase != w.Phase || got.ByWorker[i].Worker != w.Worker {
			t.Errorf("by_worker[%d] = %s/%s, want %s/%s", i,
				got.ByWorker[i].Phase, got.ByWorker[i].Worker, w.Phase, w.Worker)
		}
	}
	if got.ByWorker[0].TotalTokens != 300 {
		t.Errorf("the template's total = %d, want its own delegated task's 300",
			got.ByWorker[0].TotalTokens)
	}
	if got.ByWorker[1].TotalTokens != 15 {
		t.Errorf("the learning worker's total = %d, want only the auxiliary phase's 15",
			got.ByWorker[1].TotalTokens)
	}
}

// A TEMPLATE AND A LEARNING WORKER SHARING A NAME ARE TWO ROWS. Nothing
// reserves a learning worker's name from the `workers:` grammar, so a founder
// may call a template `persist_decider`; one row per name would sum the
// template's delegated spend and the learning worker's into one figure that
// belongs to neither.
func TestATemplateAndALearningWorkerWithOneNameAreTwoRows(t *testing.T) {
	t.Parallel()
	aux := rec("CEO", "auxiliary", "haiku", "t1", "2026-06-14T12:00:00Z", 10, 0)
	aux.Worker = "persist_decider"
	delegated := rec("CEO", "subagent", "haiku", "t1", "2026-06-14T12:00:01Z", 30, 0)
	delegated.Worker = "persist_decider"

	got := tokens.Aggregate([]tokens.Record{aux, delegated}, tokens.Options{})
	if len(got.ByWorker) != 2 {
		t.Fatalf("by_worker = %+v, want one row per kind of worker", got.ByWorker)
	}
	for _, row := range got.ByWorker {
		want := map[string]int{"auxiliary": 10, "subagent": 30}[row.Phase]
		if row.TotalTokens != want {
			t.Errorf("the %s row = %d tokens, want %d", row.Phase, row.TotalTokens, want)
		}
	}
}

// THE WORKER PHASES ARE THE EVENT CATALOGUE'S. This package spells them itself
// to stay a leaf, and a spelling that drifted from the catalogue's would leave
// every worker's spend out of the worker rows with nothing failing.
func TestTheWorkerPhasesAreTheCataloguesOwn(t *testing.T) {
	t.Parallel()
	if tokens.PhaseAuxiliary != string(types.PhaseAuxiliary) {
		t.Errorf("PhaseAuxiliary = %q, the catalogue says %q", tokens.PhaseAuxiliary, types.PhaseAuxiliary)
	}
	if tokens.PhaseSubagent != string(types.PhaseSubagent) {
		t.Errorf("PhaseSubagent = %q, the catalogue says %q", tokens.PhaseSubagent, types.PhaseSubagent)
	}
}

func TestARecordWithNoTurnStillCountsTowardEverythingElse(t *testing.T) {
	t.Parallel()
	// It is real spend. Dropping it would understate the totals; inventing
	// a turn key for it would make one row per phase in the turn table.
	got := tokens.Aggregate([]tokens.Record{
		rec("CEO", "plan", "sonnet", "", "2026-06-14T12:00:00Z", 60, 20),
	}, tokens.Options{})

	if got.Totals.TotalTokens != 80 {
		t.Errorf("totals = %+v", got.Totals)
	}
	if len(got.ByTurn) != 0 {
		t.Errorf("by_turn = %+v, want nothing attributable", got.ByTurn)
	}
}

func TestAnUnnamedDimensionBecomesUnknownRatherThanBlank(t *testing.T) {
	t.Parallel()
	// A blank key renders as a row a reader cannot tell from a rendering
	// bug, and dropping the record would lose real spend from the totals.
	got := tokens.Aggregate([]tokens.Record{
		{Timestamp: "2026-06-14T12:00:00Z", TotalTokens: 5, InputTokens: 5},
	}, tokens.Options{})

	if len(got.ByPhase) != 1 || got.ByPhase[0].Phase != "unknown" {
		t.Errorf("by_phase = %+v", got.ByPhase)
	}
	if len(got.ByModel) != 1 || got.ByModel[0].Model != "unknown" {
		t.Errorf("by_model = %+v", got.ByModel)
	}
	if got.Totals.TotalTokens != 5 {
		t.Errorf("the record was dropped from the totals: %+v", got.Totals)
	}
}

func TestTurnsAreNewestFirstAndCapped(t *testing.T) {
	t.Parallel()
	// The table is a TAIL of recent activity. Ordering by size would pin
	// one expensive turn to the top for as long as it stayed in the window.
	var records []tokens.Record
	for i, at := range []string{
		"2026-06-14T12:00:01Z", "2026-06-14T12:00:02Z", "2026-06-14T12:00:03Z",
	} {
		records = append(records, rec("CEO", "plan", "m", string(rune('a'+i)), at, 100-i*10, 0))
	}
	got := tokens.Aggregate(records, tokens.Options{RecentTurns: 2})
	if len(got.ByTurn) != 2 {
		t.Fatalf("by_turn = %d rows, want the cap", len(got.ByTurn))
	}
	if got.ByTurn[0].EndedAt != "2026-06-14T12:00:03Z" {
		t.Errorf("first turn ended %s, want the newest", got.ByTurn[0].EndedAt)
	}
}

// TestTurnOrderIsByInstantNotByBytes is the case whole seconds cannot express.
//
// RFC3339Nano TRIMS trailing zeros, so a stamp that lands on a round second
// carries no fractional part and ends in 'Z' (0x5A), where every fractional
// stamp in that same second ends in a digit after a '.' (0x2E). Compared as
// bytes, ...:05Z therefore sorts AFTER ...:05.9Z — so the newest-first table
// led with a turn that ended 900ms earlier, and the cap then dropped the
// genuinely newest one.
//
// The three turns below are one second apart in instants but reversed under a
// byte compare, which is what makes this fail on the old comparison rather
// than merely pass on the new one.
func TestTurnOrderIsByInstantNotByBytes(t *testing.T) {
	t.Parallel()
	var records []tokens.Record
	for i, at := range []string{
		"2026-06-14T12:00:05.9Z", // the newest instant, the lowest bytes
		"2026-06-14T12:00:05.5Z",
		"2026-06-14T12:00:05Z", // the oldest instant, the highest bytes
	} {
		records = append(records, rec("CEO", "plan", "m", string(rune('a'+i)), at, 10, 0))
	}

	got := tokens.Aggregate(records, tokens.Options{RecentTurns: 3})
	want := []string{
		"2026-06-14T12:00:05.9Z", "2026-06-14T12:00:05.5Z", "2026-06-14T12:00:05Z",
	}
	for i, w := range want {
		if got.ByTurn[i].EndedAt != w {
			t.Errorf("by_turn[%d] ended %s, want %s — ordered by bytes, not by instant",
				i, got.ByTurn[i].EndedAt, w)
		}
	}

	// And the cap keeps the newest, which is the consequence an operator
	// sees: the row they came to the panel for is the one that fell off.
	capped := tokens.Aggregate(records, tokens.Options{RecentTurns: 1})
	if len(capped.ByTurn) != 1 || capped.ByTurn[0].EndedAt != want[0] {
		t.Errorf("the cap kept %+v, want only the newest (%s)", capped.ByTurn, want[0])
	}

	// The watermark is the same comparison, one call site over.
	if got.AggregatedThrough != want[0] {
		t.Errorf("aggregated_through = %s, want the newest instant %s",
			got.AggregatedThrough, want[0])
	}
}

// The window every case here reports, fixed so a rollup's label is a value a
// test can compare rather than whatever the clock said.
var (
	since = time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	until = time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
)

func TestAnEmptyRollupMarshalsToArraysNotNulls(t *testing.T) {
	t.Parallel()
	// The client does `d.by_phase.length`, so a null throws in the browser
	// rather than rendering an empty table.
	raw, err := json.Marshal(tokens.Aggregate(nil, tokens.Options{Since: since, Until: until}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"by_phase", "by_model", "by_worker", "by_agent", "by_turn"} {
		if _, ok := body[key].([]any); !ok {
			t.Errorf("%s marshalled as %T, want an array", key, body[key])
		}
	}
	// THE WINDOW IS TWO INSTANTS, so an empty rollup still says what it is
	// empty OF — a figure with no window beside it is unreadable, and "0
	// days" was what the day count could say about a window that ended
	// yesterday.
	if body["since"] != "2026-06-14T00:00:00Z" || body["until"] != "2026-06-15T00:00:00Z" {
		t.Errorf("window = %v .. %v", body["since"], body["until"])
	}
}

func TestTheWireKeysAreTheOnesTheClientReads(t *testing.T) {
	t.Parallel()
	// The dashboard is the compatibility reference.
	// These are the exact keys views/spend.js and store.js index by name —
	// a renamed field here is a blank panel there, with no error anywhere.
	raw, _ := json.Marshal(tokens.Aggregate([]tokens.Record{
		rec("CEO", "plan", "sonnet", "t1", "2026-06-14T12:00:00Z", 60, 20),
	}, tokens.Options{Handles: map[string]string{"CEO": "ceo"}, Since: since, Until: until}))

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{
		"since", "until", "agent_role", "totals", "by_phase", "by_model",
		"by_worker", "by_agent", "by_turn", "aggregated_through",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("the rollup has no %q", key)
		}
	}
	totals, _ := body["totals"].(map[string]any)
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "calls"} {
		if _, ok := totals[key]; !ok {
			t.Errorf("totals has no %q", key)
		}
	}
	// The bucket is SPREAD into each row, not nested under a key.
	phase, _ := body["by_phase"].([]any)
	row, _ := phase[0].(map[string]any)
	for _, key := range []string{"phase", "total_tokens", "calls"} {
		if _, ok := row[key]; !ok {
			t.Errorf("a by_phase row has no %q: %v", key, row)
		}
	}
	agent, _ := body["by_agent"].([]any)
	arow, _ := agent[0].(map[string]any)
	// by_phase on an agent is a MAP, indexed per matrix cell.
	if _, ok := arow["by_phase"].(map[string]any); !ok {
		t.Errorf("an agent's by_phase is %T, want an object the client can "+
			"index by phase name", arow["by_phase"])
	}
	for _, key := range []string{"role", "handle", "agent_id"} {
		if _, ok := arow[key]; !ok {
			t.Errorf("a by_agent row has no %q", key)
		}
	}
}

// A WHOLE-SECOND STAMP IS NOT "AFTER" A FRACTIONAL ONE IN THE SAME SECOND.
//
// These stamps are RFC3339Nano, which TRIMS trailing zeros — so a whole
// second has no fractional part and its 'Z' (0x5A) sorts after the '.'
// (0x2E) of every fractional stamp in that second. Compared as bytes,
// 03:04:05Z ordered after 03:04:05.9Z, which is backwards. The comparison's
// own comment asserted the opposite premise and used it to justify never
// parsing.
func TestTheWatermarkOrdersByInstantNotByBytes(t *testing.T) {
	t.Parallel()
	const (
		fractional = "2026-01-02T03:04:05.9Z"
		wholeSec   = "2026-01-02T03:04:05Z"
	)
	// The byte comparison this replaces would put the whole second last.
	if wholeSec <= fractional {
		t.Fatal("the fixture no longer reproduces the byte ordering")
	}

	got := tokens.Aggregate([]tokens.Record{
		{EventID: "a", Timestamp: wholeSec, TurnID: "t1", TotalTokens: 1},
		{EventID: "b", Timestamp: fractional, TurnID: "t1", TotalTokens: 1},
	}, tokens.Options{})

	if got.AggregatedThrough != fractional {
		t.Errorf("AggregatedThrough = %q, want the later instant %q",
			got.AggregatedThrough, fractional)
	}
	if len(got.ByTurn) != 1 {
		t.Fatalf("turns = %d, want 1", len(got.ByTurn))
	}
	turn := got.ByTurn[0]
	if turn.StartedAt != wholeSec {
		t.Errorf("StartedAt = %q, want the earlier instant %q", turn.StartedAt, wholeSec)
	}
	if turn.EndedAt != fractional {
		t.Errorf("EndedAt = %q, want the later instant %q", turn.EndedAt, fractional)
	}
}

// AND ORDINARY STAMPS STILL ORDER, so the fix is a correction rather than a
// change of basis.
func TestOrdinaryStampsStillOrder(t *testing.T) {
	t.Parallel()
	got := tokens.Aggregate([]tokens.Record{
		{EventID: "a", Timestamp: "2026-01-02T03:04:05Z", TurnID: "t1"},
		{EventID: "b", Timestamp: "2026-01-02T09:00:00Z", TurnID: "t1"},
	}, tokens.Options{})
	if got.AggregatedThrough != "2026-01-02T09:00:00Z" {
		t.Errorf("AggregatedThrough = %q", got.AggregatedThrough)
	}
	if got.ByTurn[0].StartedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("StartedAt = %q", got.ByTurn[0].StartedAt)
	}
}

// TWO RUNS OF ONE TRIGGER ARE TWO COST ROWS, EACH LINKABLE TO THE OTHER.
//
// A turn id names one RUN (ADR-0017), so a trigger that fails without
// reaching outside the engine and is redelivered spends twice — and each
// attempt really did spend what it spent, so folding them into one row would
// charge a turn with another's tokens. What the rows need instead is the work
// key, which is the only thing that says they are attempts at one trigger:
// without it an expensive-looking pair reads as the company having paid for
// the work twice.
func TestTwoRunsOfOneTriggerAreTwoLinkableRows(t *testing.T) {
	t.Parallel()
	first := rec("CEO", "execute", "sonnet", "run-1", "2026-06-14T12:00:00Z", 10, 0)
	first.WorkKey = "wk-1"
	second := rec("CEO", "execute", "sonnet", "run-2", "2026-06-14T12:02:00Z", 300, 27)
	second.WorkKey = "wk-1"

	got := tokens.Aggregate([]tokens.Record{first, second},
		tokens.Options{Handles: map[string]string{"CEO": "ceo"}, Since: since, Until: until})

	if len(got.ByTurn) != 2 {
		t.Fatalf("by_turn = %d rows, want one per run: %+v", len(got.ByTurn), got.ByTurn)
	}
	byID := map[string]tokens.TurnRow{}
	for _, row := range got.ByTurn {
		byID[row.TurnID] = row
	}
	if byID["run-1"].TotalTokens != 10 || byID["run-2"].TotalTokens != 327 {
		t.Errorf("run-1 = %d, run-2 = %d — a sum across attempts charges one "+
			"turn with another's spend",
			byID["run-1"].TotalTokens, byID["run-2"].TotalTokens)
	}
	if byID["run-1"].WorkKey != "wk-1" || byID["run-2"].WorkKey != "wk-1" {
		t.Errorf("work keys = %q and %q, want both to name the one trigger",
			byID["run-1"].WorkKey, byID["run-2"].WorkKey)
	}
}

// A CAPPED TURN TABLE SAYS HOW MANY TURNS THERE WERE.
//
// `by_turn` is the newest N of the window and `totals` is summed over every
// record in it, so the table and the figure above it describe different sets.
// Without a count a reader who asked for fifty and got fifty could not tell
// fifty-one turns from five thousand — and the sibling cut in this package
// already refuses exactly that, folding the groups past its limit into a
// residual that names how many it stands for.
func TestACappedTurnTableSaysHowManyTurnsThereWere(t *testing.T) {
	t.Parallel()
	const want = 5
	var records []tokens.Record
	for i := range want * 2 {
		records = append(records, rec("CEO", "execute", "sonnet",
			fmt.Sprintf("t%02d", i),
			time.Date(2026, 6, 14, 12, 0, i, 0, time.UTC).Format(time.RFC3339),
			10, 5))
	}
	got := tokens.Aggregate(records, tokens.Options{
		Handles: map[string]string{"CEO": "ceo"},
		Since:   since, Until: until, RecentTurns: want,
	})

	if len(got.ByTurn) != want {
		t.Fatalf("by_turn holds %d rows, want the page of %d", len(got.ByTurn), want)
	}
	// COUNTED BEFORE THE CUT. A total taken after it would be the page's
	// own length, which is the bug a total exists to remove.
	if got.TurnsTotal != want*2 {
		t.Errorf("turns_total = %d, want %d — the table is a page and the "+
			"totals above it cover every turn in the window",
			got.TurnsTotal, want*2)
	}
	// AND THE TOTALS DESCRIBE THE WHOLE WINDOW, which is what makes the
	// count necessary rather than decorative.
	if got.Totals.Calls != want*2 {
		t.Errorf("totals cover %d calls, want %d", got.Totals.Calls, want*2)
	}

	// A WINDOW THAT FITS REPORTS ITS OWN SIZE, so the count is not simply
	// the record count and a reader can compare it with len(by_turn).
	small := tokens.Aggregate(records[:3], tokens.Options{
		Handles: map[string]string{"CEO": "ceo"},
		Since:   since, Until: until, RecentTurns: want,
	})
	if small.TurnsTotal != 3 || len(small.ByTurn) != 3 {
		t.Errorf("a three-turn window reports %d of %d",
			len(small.ByTurn), small.TurnsTotal)
	}
}

// A PHASE TWO MODELS SERVED IS COUNTED UNDER EACH, BY WHAT EACH BILLED.
//
// A fallback chain can move a phase between models round by round, and the
// record's own `model` names only the first. Keyed on that alone, every round
// the second model served is billed to the first — the per-model breakdown
// names a model that did not do the work.
func TestAPhaseTwoModelsServedCountsUnderEach(t *testing.T) {
	t.Parallel()
	phase := rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:00Z", 150, 15)
	phase.Models = []tokens.ModelSpend{
		{Model: "sonnet", InputTokens: 100, OutputTokens: 10},
		{Model: "haiku", InputTokens: 50, OutputTokens: 5},
	}

	got := tokens.Aggregate([]tokens.Record{phase}, tokens.Options{})
	byModel := map[string]tokens.ModelRow{}
	for _, row := range got.ByModel {
		byModel[row.Model] = row
	}
	if len(got.ByModel) != 2 || byModel["sonnet"].TotalTokens != 110 || byModel["haiku"].TotalTokens != 55 {
		t.Fatalf("by_model = %+v, want sonnet 110 and haiku 55 — each model's own rounds", got.ByModel)
	}
	if byModel["sonnet"].Calls != 1 || byModel["haiku"].Calls != 1 {
		t.Errorf("calls = %d and %d, want the record counted once under each model",
			byModel["sonnet"].Calls, byModel["haiku"].Calls)
	}
	// THE TOTALS ARE THE RECORD'S, once: the split reapportions, it never adds.
	if got.Totals.TotalTokens != 165 || got.Totals.Calls != 1 {
		t.Errorf("totals = %+v, want the one record's 165 over one call", got.Totals)
	}
}

// A RECORD WITHOUT A SPLIT COUNTS UNDER ITS ONE MODEL — which is every record
// a build that wrote no split published, and so the rule a mixed fleet's
// rollup needs.
func TestARecordWithoutASplitCountsUnderItsOneModel(t *testing.T) {
	t.Parallel()
	got := tokens.Aggregate([]tokens.Record{
		rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:00Z", 90, 30),
	}, tokens.Options{})
	if len(got.ByModel) != 1 || got.ByModel[0].Model != "sonnet" || got.ByModel[0].TotalTokens != 120 {
		t.Errorf("by_model = %+v, want the whole record under its model", got.ByModel)
	}
}

// WHAT A SPLIT DOES NOT COVER COUNTS UNDER THE RECORD'S MODEL, so a record
// whose split names part of its spend — rounds carried across a suspension by
// a build that kept no split for them — loses none of the rest, and `by_model`
// still sums to the totals.
func TestWhatASplitDoesNotCoverCountsUnderTheRecordsModel(t *testing.T) {
	t.Parallel()
	phase := rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:00Z", 300, 40)
	phase.Models = []tokens.ModelSpend{{Model: "haiku", InputTokens: 100, OutputTokens: 10}}

	got := tokens.Aggregate([]tokens.Record{phase}, tokens.Options{})
	byModel := map[string]int{}
	sum := 0
	for _, row := range got.ByModel {
		byModel[row.Model] = row.TotalTokens
		sum += row.TotalTokens
	}
	if byModel["haiku"] != 110 || byModel["sonnet"] != 230 {
		t.Errorf("by_model = %v, want haiku's 110 and the uncovered 230 under sonnet", byModel)
	}
	if sum != got.Totals.TotalTokens {
		t.Errorf("by_model sums to %d, the totals say %d", sum, got.Totals.TotalTokens)
	}
}

// A SPLIT THAT CLAIMS MORE THAN ITS RECORD IS NOT A SPLIT. Counted, it would
// make `by_model` sum past the totals beside it; the record counts whole under
// its model instead, as one with no split does.
func TestASplitThatExceedsItsRecordIsIgnored(t *testing.T) {
	t.Parallel()
	for name, split := range map[string][]tokens.ModelSpend{
		"more tokens than the record": {{Model: "haiku", InputTokens: 500, OutputTokens: 0}},
		"a negative entry":            {{Model: "haiku", InputTokens: -5, OutputTokens: 1}},
		"a price past the record's":   {{Model: "haiku", InputTokens: 1, OutputTokens: 1, CostUSD: 3}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			phase := priced(rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:00Z", 90, 30), 1)
			phase.Models = split
			got := tokens.Aggregate([]tokens.Record{phase}, tokens.Options{})
			if len(got.ByModel) != 1 || got.ByModel[0].Model != "sonnet" || got.ByModel[0].TotalTokens != 120 {
				t.Errorf("by_model = %+v, want the whole record under its own model", got.ByModel)
			}
		})
	}
}

// A PRICE SPLIT BY MODEL FOLLOWS ITS MODEL. A coding run that quotes each
// model's cost separately is priced per model, and the part of a record's price
// no entry claims stays with the record's own model rather than vanishing.
func TestAPriceSplitByModelFollowsItsModel(t *testing.T) {
	t.Parallel()
	phase := priced(rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:00Z", 1000, 100), 0.75)
	phase.Models = []tokens.ModelSpend{
		{Model: "opus", InputTokens: 600, OutputTokens: 60, CostUSD: 0.5},
		{Model: "haiku", InputTokens: 100, OutputTokens: 10, CostUSD: 0.05},
	}
	got := tokens.Aggregate([]tokens.Record{phase}, tokens.Options{})
	cost := map[string]float64{}
	for _, row := range got.ByModel {
		cost[row.Model] = row.CostUSD
	}
	if cost["opus"] != 0.5 || cost["haiku"] != 0.05 {
		t.Errorf("costs = %v, want each entry's own price", cost)
	}
	if d := cost["sonnet"] - 0.2; d > 1e-9 || d < -1e-9 {
		t.Errorf("sonnet's cost = %v, want the unclaimed 0.20", cost["sonnet"])
	}
	if got.Totals.CostUSD != 0.75 || got.Totals.PricedCalls != 1 {
		t.Errorf("totals = %+v, want the record's own price once", got.Totals)
	}
}

// AN UNREPORTED RUN IS A FLOOR, NEVER A ZERO. A coding run whose agent gave no
// account of its own spend reads, in tokens, as nothing — and a rollup that
// added it as nothing would state "this cost 40 tokens" about work that cost an
// unknown amount more. Every bucket the record reaches counts it as a call
// whose spend went unreported, and the per-model breakdown files the unmeasured
// part under no model's name rather than inside the executor model's measured
// figures.
func TestAnUnreportedRunIsCountedAsAFloorNotAZero(t *testing.T) {
	t.Parallel()
	phase := rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:00Z", 30, 10)
	phase.Unreported = true
	measured := rec("CEO", "review", "sonnet", "t1", "2026-06-14T12:00:05Z", 20, 5)

	got := tokens.Aggregate([]tokens.Record{phase, measured}, tokens.Options{})
	if got.Totals.UnreportedCalls != 1 || got.Totals.TotalTokens != 65 {
		t.Errorf("totals = %+v, want 65 tokens with one call unreported", got.Totals)
	}
	for _, row := range got.ByPhase {
		want := map[string]int{"execute": 1, "review": 0}[row.Phase]
		if row.UnreportedCalls != want {
			t.Errorf("the %s row counts %d unreported calls, want %d", row.Phase, row.UnreportedCalls, want)
		}
	}
	if got.ByTurn[0].UnreportedCalls != 1 || got.ByAgent[0].UnreportedCalls != 1 {
		t.Errorf("turn %+v and agent %+v must both say part of their spend is unknown",
			got.ByTurn[0].Bucket, got.ByAgent[0].Bucket)
	}
	byModel := map[string]tokens.ModelRow{}
	for _, row := range got.ByModel {
		byModel[row.Model] = row
	}
	if byModel["sonnet"].UnreportedCalls != 0 || byModel["sonnet"].TotalTokens != 65 {
		t.Errorf("sonnet = %+v, want its measured 65 and no unreported call", byModel["sonnet"].Bucket)
	}
	if u := byModel["unknown"]; u.UnreportedCalls != 1 || u.TotalTokens != 0 {
		t.Errorf("unknown = %+v, want the unmeasured part as its own call of no tokens", u.Bucket)
	}
}

// ONE ENCODING ON BOTH SIDES OF THE LEAF. The event catalogue writes a phase
// record's split and this package reads it back with a type of its own; a tag
// that drifted between them would decode every split as empty, and every phase
// would count under its first model again with nothing failing.
func TestTheSplitDecodesAsTheCatalogueWritesIt(t *testing.T) {
	t.Parallel()
	written := types.AgentPhaseCompleted{Models: []types.ModelSpend{
		{Model: "sonnet", InputTokens: 100, OutputTokens: 10, CostUSD: 0.25},
		{Model: "haiku", InputTokens: 50, OutputTokens: 5},
	}}
	raw, err := json.Marshal(written)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []tokens.ModelSpend{
		{Model: "sonnet", InputTokens: 100, OutputTokens: 10, CostUSD: 0.25},
		{Model: "haiku", InputTokens: 50, OutputTokens: 5},
	}
	if got := tokens.DecodeModels(payload.Models); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("DecodeModels = %+v, want %+v", got, want)
	}

	// And the live projection's route, from a payload already decoded into
	// Go values, reaches the same list.
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := tokens.ModelsOf(decoded["models"]); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ModelsOf = %+v, want %+v", got, want)
	}

	// A list that does not decode is no split, never a partial one.
	if got := tokens.DecodeModels([]byte(`{"not":"a list"}`)); got != nil {
		t.Errorf("a malformed list decoded as %+v", got)
	}
}

// recordOf reads a phase record's payload the way a producer hands it to the
// fold: the token columns, the split through DecodeModels, and the flag.
func recordOf(t *testing.T, ev types.AgentPhaseCompleted) tokens.Record {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload struct {
		Model        string          `json:"model"`
		Phase        string          `json:"phase"`
		InputTokens  int             `json:"input_tokens"`
		OutputTokens int             `json:"output_tokens"`
		TotalTokens  int             `json:"total_tokens"`
		CostUSD      float64         `json:"cost_usd"`
		Models       json.RawMessage `json:"models"`
		Unreported   bool            `json:"run_spend_unreported"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return tokens.Record{
		EventID: "e1", Timestamp: "2026-06-14T12:00:00Z", AgentRole: "Dev", TurnID: "t1",
		Phase: payload.Phase, Model: payload.Model,
		InputTokens: payload.InputTokens, OutputTokens: payload.OutputTokens, TotalTokens: payload.TotalTokens,
		CostUSD: payload.CostUSD, Models: tokens.DecodeModels(payload.Models), Unreported: payload.Unreported,
	}
}

// A CODING RUN'S OWN SPEND REACHES THE ROLLUP THROUGH ITS PHASE'S RECORD. The
// agent in the box spends on models the engine never called; the record that
// collects the run takes its tokens in beside the rounds this process ran, and
// every model the run named gets its own row with its own price.
func TestACollectedRunsSpendReachesTheRollup(t *testing.T) {
	t.Parallel()
	ev := types.AgentPhaseCompleted{
		Phase: types.PhaseExecute, Model: "exec-model", CostUSD: 0.4,
		InputTokens: 100, OutputTokens: 10, TotalTokens: 110,
		Models: []types.ModelSpend{{Model: "exec-model", InputTokens: 100, OutputTokens: 10}},
	}
	ev.AddRun(types.RunSpend{Collected: true, Whole: true, Models: []types.ModelSpend{
		{Model: "claude-sonnet", InputTokens: 600, OutputTokens: 60, CostUSD: 0.3},
		{Model: "claude-haiku", InputTokens: 300, OutputTokens: 40, CostUSD: 0.1},
	}})
	got := tokens.Aggregate([]tokens.Record{recordOf(t, ev)}, tokens.Options{})
	if got.Totals.TotalTokens != 1110 || got.Totals.UnreportedCalls != 0 {
		t.Errorf("totals = %+v, want the rounds' 110 and the run's 1000, all of it reported", got.Totals)
	}
	byModel := map[string]tokens.ModelRow{}
	for _, row := range got.ByModel {
		byModel[row.Model] = row
	}
	if byModel["exec-model"].TotalTokens != 110 || byModel["claude-sonnet"].TotalTokens != 660 ||
		byModel["claude-haiku"].TotalTokens != 340 {
		t.Errorf("by_model = %+v, want each model's own part", got.ByModel)
	}
	if byModel["claude-sonnet"].CostUSD != 0.3 || byModel["exec-model"].PricedCalls != 0 {
		t.Errorf("sonnet $%v, exec-model priced %d: the run's price belongs to the models it named",
			byModel["claude-sonnet"].CostUSD, byModel["exec-model"].PricedCalls)
	}
}

// A RUN WHOSE AGENT GAVE NO WHOLE ACCOUNT MARKS ITS RECORD. Whatever part of its
// spend was reported is counted, and the record says the rest is not known, so
// the rollup reads the figure as a floor.
func TestARunWithoutAWholeAccountMarksItsRecordAFloor(t *testing.T) {
	t.Parallel()
	ev := types.AgentPhaseCompleted{
		Phase: types.PhaseExecute, Model: "exec-model", InputTokens: 100, OutputTokens: 10, TotalTokens: 110,
	}
	ev.AddRun(types.RunSpend{Collected: true, Models: []types.ModelSpend{{InputTokens: 30, OutputTokens: 5}}})
	if !ev.RunSpendUnreported || ev.TotalTokens != 145 {
		t.Fatalf("record = %d tokens, unreported %v; want the floor's 145 marked unreported",
			ev.TotalTokens, ev.RunSpendUnreported)
	}
	got := tokens.Aggregate([]tokens.Record{recordOf(t, ev)}, tokens.Options{})
	if got.Totals.UnreportedCalls != 1 || got.ByPhase[0].UnreportedCalls != 1 {
		t.Errorf("totals %+v, by_phase %+v: the floor must say it is one", got.Totals, got.ByPhase)
	}

	// AND A RECORD THAT COLLECTED NO RUN SAYS NOTHING ABOUT ONE.
	quiet := types.AgentPhaseCompleted{Phase: types.PhaseExecute, InputTokens: 5, TotalTokens: 5}
	quiet.AddRun(types.RunSpend{})
	if quiet.RunSpendUnreported || quiet.TotalTokens != 5 || quiet.Models != nil {
		t.Errorf("a record that collected no run = %+v, want it unchanged", quiet)
	}
}
