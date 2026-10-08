package tokens_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tokens"
)

// auxRec is an auxiliary record: the `auxiliary` phase, a purpose in the
// worker field, a stage, and the provider calls it coalesced.
func auxRec(stage, purpose, turn, at string, calls, total int) tokens.Record {
	return tokens.Record{
		EventID: at + stage + purpose, Timestamp: at,
		AgentRole: "CEO", AgentID: "id-CEO",
		Phase: tokens.PhaseAuxiliary, Worker: purpose, Stage: stage,
		Model: "haiku", TurnID: turn, Calls: calls,
		InputTokens: total, TotalTokens: total,
	}
}

// ONE "CALLS" UNIT: PROVIDER CALLS, under one label on every row.
//
// A bucket counted RECORDS, so a forty-round executor was "1 call" beside a
// coalesced auxiliary record of seventy rewrites that would have been "1 call"
// too — two units under one word on the same card. A record states its calls,
// and a bucket sums them.
func TestABucketCountsProviderCallsNotRecords(t *testing.T) {
	t.Parallel()
	exec := rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:05Z", 90, 30)
	exec.Calls = 4
	review := rec("CEO", "review", "sonnet", "t1", "2026-06-14T12:00:08Z", 10, 5)
	got := tokens.Aggregate([]tokens.Record{
		exec, review,
		auxRec(tokens.StageTurn, "condense_produced", "t1", "2026-06-14T12:00:09Z", 70, 7000),
	}, tokens.Options{Since: since, Until: until})

	if got.Totals.Calls != 4+1+70 {
		t.Errorf("calls = %d, want 75: four rounds, the review's one call and "+
			"seventy coalesced rewrites", got.Totals.Calls)
	}
	if len(got.ByWorker) != 1 || got.ByWorker[0].Worker != "condense_produced" ||
		got.ByWorker[0].Calls != 70 {
		t.Errorf("by_worker = %+v, want the purpose with its seventy calls", got.ByWorker)
	}
}

// A TURN'S COST IS ITS PHASES AND ITS IN-TURN AUXILIARY SPEND, and never the
// reflection after it.
//
// The reflection's records carry the turn's id — the turn's page draws them —
// and are the seat's learning, not what the work cost: a per-turn row that
// counted them would make a turn dearer the more its seat had to remember.
// They are still spend, so every other dimension counts them.
func TestATurnsCostIsItsPhasesAndItsInTurnAuxiliarySpend(t *testing.T) {
	t.Parallel()
	got := tokens.Aggregate([]tokens.Record{
		rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:05Z", 90, 30),
		auxRec(tokens.StageTurn, "memory_filter", "t1", "2026-06-14T12:00:01Z", 1, 300),
		auxRec("reflection", "persist_decider", "t1", "2026-06-14T12:00:20Z", 1, 5000),
	}, tokens.Options{Since: since, Until: until})

	if len(got.ByTurn) != 1 {
		t.Fatalf("by_turn = %+v", got.ByTurn)
	}
	if turn := got.ByTurn[0]; turn.TotalTokens != 120+300 {
		t.Errorf("the turn costs %d, want 420: its phase and its in-turn auxiliary "+
			"call, without the 5000 its reflection spent", turn.TotalTokens)
	}
	if turn := got.ByTurn[0]; turn.EndedAt != "2026-06-14T12:00:05Z" {
		t.Errorf("the turn ends at %s, want its last own record — a reflection is "+
			"not the turn going on", turn.EndedAt)
	}
	if got.Totals.TotalTokens != 120+300+5000 || got.ByAgent[0].TotalTokens != 5420 {
		t.Errorf("totals = %d, seat = %d, want every record counted: the reflection is "+
			"still the seat's spend", got.Totals.TotalTokens, got.ByAgent[0].TotalTokens)
	}
}

// THE STAGE THIS PACKAGE COUNTS AS A TURN'S IS THE ONE THE CATALOGUE PUBLISHES.
// A copy, because this package is a leaf; a renamed stage on one side would
// drop every in-turn record from its turn without a sound.
func TestTheTurnStageIsTheCataloguesOwn(t *testing.T) {
	t.Parallel()
	if tokens.StageTurn != string(types.AuxStageTurn) {
		t.Fatalf("tokens.StageTurn = %q, the catalogue publishes %q",
			tokens.StageTurn, types.AuxStageTurn)
	}
}

// A PERSON IS NOBODY'S SEAT. A person's auxiliary spend — a question answered
// on the operator surface — names no agent and no agent role of its own, only
// the role of the person's seat; keyed as seats are, every person would share
// one empty id on a named window, and on the live one a person would be folded
// into whatever seat carried their role. Each person is their own row, marked
// as one, named by their handle, and — on a named window — with no turn counts,
// since a person takes no turns.
func TestAPersonsSpendIsTheirOwnRowOnBothWindows(t *testing.T) {
	t.Parallel()
	person := func(handle string, total int) tokens.Record {
		r := auxRec("operator", "answer_knowledge", "", "2026-06-14T12:00:09Z", 1, total)
		r.AgentID, r.AgentRole, r.Person = "", "Founder", handle
		r.EventID += handle
		return r
	}
	founderSeat := rec("Founder", "execute", "sonnet", "t1", "2026-06-14T12:00:05Z", 90, 30)
	live := tokens.Aggregate([]tokens.Record{founderSeat, person("maya", 70), person("ana", 20)},
		tokens.Options{Since: since, Until: until})
	byName := map[string]tokens.AgentRow{}
	for _, a := range live.ByAgent {
		byName[a.Handle+"/"+a.Role] = a
	}
	if len(live.ByAgent) != 3 {
		t.Fatalf("live by_agent = %+v, want the seat and each person apart", live.ByAgent)
	}
	if maya := byName["maya/Founder"]; !maya.Person || maya.TotalTokens != 70 {
		t.Errorf("live maya = %+v, want her own person row of 70", maya)
	}

	r := days(t, "2026-06-14", "2026-06-14", time.UTC)
	named := tokens.FoldDaily([]tokens.Cell{
		cell("2026-06-14", "ceo", "execute", "sonnet", 60),
		{Day: "2026-06-14", Handle: "maya", Role: "Founder", Person: true,
			Phase: tokens.PhaseAuxiliary, Worker: "answer_knowledge", Model: "haiku",
			Bucket: tokens.Bucket{TotalTokens: 70, Calls: 2}},
		{Day: "2026-06-14", Handle: "ana", Role: "Founder", Person: true,
			Phase: tokens.PhaseAuxiliary, Worker: "answer_knowledge", Model: "haiku",
			Bucket: tokens.Bucket{TotalTokens: 20, Calls: 1}},
	}, nil, tokens.DailyOptions{Range: r})
	people := 0
	for _, a := range named.ByAgent {
		if !a.Person {
			continue
		}
		people++
		if a.AgentID != "" || a.Turns != nil || a.Failed != nil || a.Role != "Founder" {
			t.Errorf("named person row %+v, want a person with no id and no turn counts", a)
		}
	}
	if people != 2 || named.Totals.TotalTokens != 150 {
		t.Fatalf("named by_agent = %+v, want two person rows beside the seat", named.ByAgent)
	}
}

// PEOPLE WITH EQUAL TOTALS COME BACK IN ONE ORDER, on both windows.
//
// A person's row carries no agent id, and two people may share a role, so a
// tie broken on the seat's key alone compared them equal and left them in Go's
// randomised map order: an export or a golden capture of the same window
// differed on every refresh. Each fold is asked many times, because a map's
// order is what the defect turns on.
func TestEqualPeopleAreOrderedByTheirOwnHandle(t *testing.T) {
	t.Parallel()
	handles := []string{"ana", "bo", "cy", "di", "ed", "flo"}
	var records []tokens.Record
	var cells []tokens.Cell
	for _, h := range handles {
		r := auxRec("operator", "answer_knowledge", "", "2026-06-14T12:00:09Z", 1, 40)
		r.AgentID, r.AgentRole, r.Person, r.EventID = "", "Founder", h, "q-"+h
		records = append(records, r)
		cells = append(cells, tokens.Cell{Day: "2026-06-14", Handle: h, Role: "Founder",
			Person: true, Phase: tokens.PhaseAuxiliary, Worker: "answer_knowledge",
			Model: "haiku", Bucket: tokens.Bucket{TotalTokens: 40, Calls: 1}})
	}
	r := days(t, "2026-06-14", "2026-06-14", time.UTC)
	order := func(rows []tokens.AgentRow) []string {
		out := make([]string, 0, len(rows))
		for _, a := range rows {
			out = append(out, a.Handle)
		}
		return out
	}
	for range 20 {
		live := tokens.Aggregate(records, tokens.Options{Since: since, Until: until})
		named := tokens.FoldDaily(cells, nil, tokens.DailyOptions{Range: r})
		for window, got := range map[string][]string{
			"live": order(live.ByAgent), "named": order(named.ByAgent),
		} {
			if !slices.Equal(got, handles) {
				t.Fatalf("%s by_agent = %v, want %v: equal totals ordered by handle",
					window, got, handles)
			}
		}
	}
}
