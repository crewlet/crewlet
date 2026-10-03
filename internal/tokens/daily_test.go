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
		seats[a.AgentID] = a
	}
	ceo := seats["id-ceo"]
	if ceo.TotalTokens != 100 || ceo.Turns == nil || *ceo.Turns != 5 || *ceo.Failed != 1 {
		t.Errorf("ceo = %+v (turns %v), want 100 tokens over 5 turns, 1 failed", ceo, ceo.Turns)
	}
	if ops := seats["id-ops"]; ops.Turns == nil || *ops.Turns != 1 || ops.Role != "OPS" {
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
// derived agent id, and — where the chart does not name it — by the newest row:
// a role renamed mid-window is one seat under its current name, not two rows
// splitting its spend, and in whichever order the rows arrive.
func TestASeatRenamedMidWindowIsOneRowUnderItsNewestName(t *testing.T) {
	t.Parallel()
	older := cell("2026-06-14", "lead", "execute", "sonnet", 10)
	older.Role = "Lead"
	newer := cell("2026-06-15", "lead", "execute", "sonnet", 20)
	newer.Role = "Tech Lead"
	for _, order := range [][]tokens.Cell{{older, newer}, {newer, older}} {
		got := tokens.FoldDaily(order, nil,
			tokens.DailyOptions{Range: days(t, "2026-06-14", "2026-06-15", time.UTC)})
		if len(got.ByAgent) != 1 || got.ByAgent[0].Role != "Tech Lead" || got.ByAgent[0].TotalTokens != 30 {
			t.Errorf("by_agent = %+v, want one row named Tech Lead", got.ByAgent)
		}
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

	chart := tokens.Seats{}
	for _, handle := range []string{"a", "b", "c", "d", "e"} {
		chart["id-"+handle] = tokens.Seat{Handle: handle}
	}
	got := tokens.FoldDaily(cells, nil,
		tokens.DailyOptions{Range: days(t, "2026-06-14", "2026-06-14", time.UTC), Seats: chart})
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
		Seats: tokens.Seats{"id-CEO": {Handle: "ceo", Name: "CEO"}}, Since: since, Until: until,
	})
	if len(got.ByProvider) != 1 {
		t.Fatalf("by_provider = %+v", got.ByProvider)
	}
	p := got.ByProvider[0]
	if !slices.Equal(p.Seats, []string{"ceo", "Dev"}) || !slices.Equal(p.Models, []string{"sonnet", "haiku"}) {
		t.Errorf("provider = %+v, want the handle where the chart holds the seat and its name otherwise", p)
	}
	// The live window cannot count ended turns, and says so by absence.
	for _, row := range got.ByAgent {
		if row.Turns != nil || row.Failed != nil {
			t.Errorf("a live row claims %v turns: it holds phase records, not endings", *row.Turns)
		}
	}
}

// "USED BY" COUNTS A SEAT ONCE, whatever it was called: ranked by the name
// each row carried, one seat renamed inside the window was two entries
// splitting its spend — and the smaller half could fall off the top three —
// while two seats sharing a name were one. The seat is its id; the words come
// last.
func TestAProviderCountsASeatOnceWhateverItWasCalled(t *testing.T) {
	t.Parallel()
	before := cell("2026-06-14", "sam", "execute", "sonnet", 40)
	after := cell("2026-06-15", "samantha", "execute", "sonnet", 40)
	after.AgentID = before.AgentID
	twinA := cell("2026-06-14", "eng-1", "execute", "sonnet", 50)
	twinB := cell("2026-06-14", "eng-2", "execute", "sonnet", 45)
	twinA.Role, twinB.Role = "Engineer", "Engineer"
	for _, c := range []*tokens.Cell{&before, &after, &twinA, &twinB} {
		c.ProviderKey = "anthropic"
	}
	got := tokens.FoldDaily([]tokens.Cell{before, after, twinA, twinB}, nil, tokens.DailyOptions{
		Range: days(t, "2026-06-14", "2026-06-15", time.UTC),
		Seats: tokens.Seats{before.AgentID: {Handle: "samantha", Name: "Samantha"}},
	})
	p := got.ByProvider[0]
	if p.SeatsTotal != 3 || !slices.Equal(p.Seats, []string{"samantha", "Engineer", "Engineer"}) {
		t.Errorf("used by = %v of %d, want the renamed seat once at its 80 and the "+
			"two Engineers as two seats", p.Seats, p.SeatsTotal)
	}
}

// A RENAME ON ONE DAY IS NAMED THE SAME WHATEVER THE ORDER. Two rows of one
// company day can carry two names — each node published the name it ran the
// seat under — and a fold that kept the first to arrive would name the seat
// differently on two nodes asked the same question.
func TestTwoNamesOnOneDayAreSettledTheSameWayInEveryOrder(t *testing.T) {
	t.Parallel()
	one := cell("2026-06-14", "lead", "execute", "sonnet", 10)
	one.Role = "Lead"
	two := cell("2026-06-14", "lead", "review", "sonnet", 5)
	two.Role = "Tech Lead"
	r := days(t, "2026-06-14", "2026-06-14", time.UTC)
	var names []string
	for _, order := range [][]tokens.Cell{{one, two}, {two, one}} {
		got := tokens.FoldDaily(order, nil, tokens.DailyOptions{Range: r})
		names = append(names, got.ByAgent[0].Role)
		series := tokens.BucketDaily(order, tokens.SeriesOptions{Group: tokens.GroupSeat, Range: r})
		names = append(names, series.ByGroup[0].Label)
	}
	if names[0] == "" || slices.ContainsFunc(names, func(n string) bool { return n != names[0] }) {
		t.Errorf("one seat's day was named %v, want one name in every order", names)
	}
}
