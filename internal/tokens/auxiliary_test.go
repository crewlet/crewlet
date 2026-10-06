package tokens_test

import (
	"testing"

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
// and one that states none (an older peer's) is the one call that produced it.
func TestABucketCountsProviderCallsNotRecords(t *testing.T) {
	t.Parallel()
	exec := rec("CEO", "execute", "sonnet", "t1", "2026-06-14T12:00:05Z", 90, 30)
	exec.Calls = 4
	older := rec("CEO", "review", "sonnet", "t1", "2026-06-14T12:00:08Z", 10, 5)
	got := tokens.Aggregate([]tokens.Record{
		exec, older,
		auxRec(tokens.StageTurn, "condense_produced", "t1", "2026-06-14T12:00:09Z", 70, 7000),
	}, tokens.Options{Since: since, Until: until})

	if got.Totals.Calls != 4+1+70 {
		t.Errorf("calls = %d, want 75: four rounds, the one call an older record "+
			"stands for, and seventy coalesced rewrites", got.Totals.Calls)
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
