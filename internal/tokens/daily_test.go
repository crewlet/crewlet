package tokens_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tokens"
)

// A NAMED WINDOW'S ROLLUP IS THE LIVE ONE'S SHAPE, with the three differences
// its source forces stated rather than papered over: no per-turn tail, no
// watermark, and every seat's ended and failed turns counted.
func TestADailyFoldIsTheRollupWithTurnsAndWithoutATurnTail(t *testing.T) {
	t.Parallel()
	r := days(t, "2026-06-14", "2026-06-15", time.UTC)
	got := tokens.FoldDaily([]tokens.Cell{
		cell("2026-06-14", "ceo", "execute", "sonnet", 60),
		cell("2026-06-15", "ceo", "review", "haiku", 40),
		cell("2026-06-15", "dev", "execute", "sonnet", 30),
	}, []tokens.SeatDay{
		{Day: "2026-06-14", AgentID: "id-ceo", Handle: "ceo", Turns: 3, Failed: 1},
		{Day: "2026-06-15", AgentID: "id-ceo", Handle: "ceo", Turns: 2},
		// A seat that ended a turn and spent nothing still has a row.
		{Day: "2026-06-15", AgentID: "id-ops", Handle: "ops", Role: "OPS", Turns: 1},
	}, tokens.DailyOptions{Range: r, Horizon: tokens.Horizon{Days: 181, Floor: "2025-12-15"}})

	if got.Totals.TotalTokens != 130 || got.Totals.Calls != 3 {
		t.Errorf("totals = %+v", got.Totals)
	}
	if got.From != "2026-06-14" || got.To != "2026-06-15" || got.Days != 2 ||
		got.Since != "2026-06-14T00:00:00Z" || got.Until != "2026-06-16T00:00:00Z" {
		t.Errorf("window = %s..%s (%d days) %s..%s", got.From, got.To, got.Days, got.Since, got.Until)
	}
	seats := map[string]tokens.AgentRow{}
	for _, a := range got.ByAgent {
		seats[a.Handle] = a
	}
	ceo := seats["ceo"]
	if ceo.TotalTokens != 100 || ceo.Turns == nil || *ceo.Turns != 5 || *ceo.Failed != 1 {
		t.Errorf("ceo = %+v (turns %v), want 100 tokens over 5 turns, 1 failed", ceo, ceo.Turns)
	}
	if ops := seats["ops"]; ops.Turns == nil || *ops.Turns != 1 || ops.Role != "OPS" {
		t.Errorf("ops = %+v, want its turn counted though it spent nothing", ops)
	}
	body, _ := json.Marshal(got)
	var wire map[string]any
	_ = json.Unmarshal(body, &wire)
	for _, absent := range []string{"by_turn", "aggregated_through"} {
		if _, ok := wire[absent]; ok {
			t.Errorf("a named window carries %q, which a company day cannot answer", absent)
		}
	}
	if h, _ := wire["horizon"].(map[string]any); h["days"] != float64(181) || h["floor"] != "2025-12-15" {
		t.Errorf("horizon = %v, want it stated beside the answer", wire["horizon"])
	}
}

// ONE SEAT, ONE ROW, whatever it was called on each day. A seat is keyed on its
// derived agent id, and named by the newest row — a role renamed mid-window is
// one seat under its current name, not two rows splitting its spend.
func TestASeatRenamedMidWindowIsOneRowUnderItsNewestName(t *testing.T) {
	t.Parallel()
	older := cell("2026-06-14", "lead", "execute", "sonnet", 10)
	older.Role = "Lead"
	newer := cell("2026-06-15", "lead", "execute", "sonnet", 20)
	newer.Role = "Tech Lead"
	got := tokens.FoldDaily([]tokens.Cell{older, newer}, nil,
		tokens.DailyOptions{Range: days(t, "2026-06-14", "2026-06-15", time.UTC)})
	if len(got.ByAgent) != 1 || got.ByAgent[0].Role != "Tech Lead" || got.ByAgent[0].TotalTokens != 30 {
		t.Errorf("by_agent = %+v, want one row named Tech Lead", got.ByAgent)
	}
}

// WHICH ENTRY WE PAY FOR IS NOT WHICH MODEL ANSWERED. One provider entry
// serving two models is one row naming both, and "used by" names its three
// biggest seats and counts the rest.
func TestAProviderRowNamesItsModelsAndItsTopThreeSeats(t *testing.T) {
	t.Parallel()
	var cells []tokens.Cell
	for i, handle := range []string{"a", "b", "c", "d", "e"} {
		c := cell("2026-06-14", handle, "execute", "sonnet", 100-i*10)
		c.ProviderKey = "anthropic"
		cells = append(cells, c)
	}
	fallback := cell("2026-06-14", "e", "execute", "haiku", 500)
	fallback.ProviderKey = "anthropic"
	other := cell("2026-06-14", "a", "execute", "gpt", 1)
	other.ProviderKey = "openai"
	cells = append(cells, fallback, other)

	got := tokens.FoldDaily(cells, nil,
		tokens.DailyOptions{Range: days(t, "2026-06-14", "2026-06-14", time.UTC)})
	if len(got.ByProvider) != 2 {
		t.Fatalf("by_provider = %+v", got.ByProvider)
	}
	p := got.ByProvider[0]
	if p.ProviderKey != "anthropic" || p.TotalTokens != 900 {
		t.Errorf("first provider = %+v, want anthropic with every seat's tokens", p)
	}
	if !slices.Equal(p.Models, []string{"haiku", "sonnet"}) {
		t.Errorf("models = %v, want both, biggest first", p.Models)
	}
	if !slices.Equal(p.Seats, []string{"e", "a", "b"}) || p.SeatsTotal != 5 {
		t.Errorf("seats = %v of %d, want the three biggest of five", p.Seats, p.SeatsTotal)
	}
}

// THE LIVE WINDOW ANSWERS THE SAME PROVIDER ROWS, from its records, so the
// two sources of one screen rank "used by" alike.
func TestTheLiveRollupAnswersProvidersToo(t *testing.T) {
	t.Parallel()
	a := rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:00Z", 60, 20)
	a.ProviderKey = "anthropic"
	b := rec("Dev", "execute", "haiku", "t2", "2026-06-14T12:01:00Z", 5, 5)
	b.ProviderKey = "anthropic"
	got := tokens.Aggregate([]tokens.Record{a, b}, tokens.Options{
		Handles: map[string]string{"CEO": "ceo"}, Since: since, Until: until,
	})
	if len(got.ByProvider) != 1 {
		t.Fatalf("by_provider = %+v", got.ByProvider)
	}
	p := got.ByProvider[0]
	if !slices.Equal(p.Seats, []string{"ceo", "Dev"}) || !slices.Equal(p.Models, []string{"sonnet", "haiku"}) {
		t.Errorf("provider = %+v, want the handle where one exists and the role otherwise", p)
	}
	// The live window cannot count ended turns, and says so by absence.
	for _, row := range got.ByAgent {
		if row.Turns != nil || row.Failed != nil {
			t.Errorf("a live row claims %v turns: it holds phase records, not endings", *row.Turns)
		}
	}
}
