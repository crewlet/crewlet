package tokens_test

import (
	"encoding/json"
	"testing"

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
	}, tokens.Options{Handles: map[string]string{"CEO": "ceo"}, SinceDays: 1})

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

func TestAWorkerCountsOnlyOnItsOwnPhase(t *testing.T) {
	t.Parallel()
	// Worker is set only on an auxiliary phase. Keying on a non-empty
	// worker alone would fold a stray value on some other phase into a
	// worker's total, which reads as a learning worker spending tokens it
	// never spent.
	aux := rec("CEO", "auxiliary", "haiku", "t1", "2026-06-14T12:00:00Z", 10, 5)
	aux.Worker = "reflect"
	stray := rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:01Z", 100, 50)
	stray.Worker = "reflect"

	got := tokens.Aggregate([]tokens.Record{aux, stray}, tokens.Options{})
	if len(got.ByWorker) != 1 {
		t.Fatalf("by_worker = %+v", got.ByWorker)
	}
	if got.ByWorker[0].TotalTokens != 15 {
		t.Errorf("worker total = %d, want only the auxiliary phase",
			got.ByWorker[0].TotalTokens)
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

func TestAnEmptyRollupMarshalsToArraysNotNulls(t *testing.T) {
	t.Parallel()
	// The client does `d.by_phase.length`, so a null throws in the browser
	// rather than rendering an empty table.
	raw, err := json.Marshal(tokens.Aggregate(nil, tokens.Options{SinceDays: 7}))
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
	if body["since_days"] != float64(7) {
		t.Errorf("since_days = %v", body["since_days"])
	}
}

func TestTheWireKeysAreTheOnesTheClientReads(t *testing.T) {
	t.Parallel()
	// The dashboard is the compatibility reference.
	// These are the exact keys views/spend.js and store.js index by name —
	// a renamed field here is a blank panel there, with no error anywhere.
	raw, _ := json.Marshal(tokens.Aggregate([]tokens.Record{
		rec("CEO", "plan", "sonnet", "t1", "2026-06-14T12:00:00Z", 60, 20),
	}, tokens.Options{Handles: map[string]string{"CEO": "ceo"}, SinceDays: 1}))

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{
		"since_days", "agent_role", "totals", "by_phase", "by_model",
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
	if !(wholeSec > fractional) {
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
