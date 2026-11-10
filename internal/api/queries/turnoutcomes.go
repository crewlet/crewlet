package queries

import (
	"cmp"
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/usage"
)

// TurnOutcomesAnswer is `turn_outcomes`: over a spend window, each agent's
// started turns by what woke them, how many changed nothing and what they spent.
type TurnOutcomesAnswer struct {
	From string           `json:"from"`
	To   string           `json:"to"`
	Rows []TurnOutcomeRow `json:"rows"`
}

// TurnOutcomeRow is one agent's started turns under one wake reason, as the turn
// ledger keys it ([types.WakeOf]): `schedule:<name>`, `work_item:<reason>` and
// the like.
type TurnOutcomeRow struct {
	Seat    string `json:"seat"`
	AgentID string `json:"agent_id"`
	Wake    string `json:"wake"`
	Turns   int64  `json:"turns"`
	Tokens  int64  `json:"tokens"`
	// Nothing is the turns that committed nothing, and NothingTokens their spend.
	Nothing       int64 `json:"nothing"`
	NothingTokens int64 `json:"nothing_tokens"`
}

// turnOutcomes answers `turn_outcomes` over the window `tokens` reads
// ([Sources.spendWindowOf]): every data node's days, summed.
func (s Sources) turnOutcomes(ctx context.Context, p Params) (any, error) {
	w, err := s.spendWindowOf(p)
	if err != nil {
		return nil, err
	}
	out := TurnOutcomesAnswer{From: w.Range.First.Label, To: w.Range.Last.Label, Rows: []TurnOutcomeRow{}}
	if w.personOnly() {
		return out, nil
	}
	rows, err := s.Usage.TurnLedger(ctx, usage.SpendQuery{From: out.From, To: out.To})
	if err != nil {
		return nil, err
	}
	out.Rows = foldTurnOutcomes(rows, s.chartHandles(), w.AgentID)
	return out, nil
}

// foldTurnOutcomes sums the ledger's started turns per agent and wake, named by
// the chart's handle while the chart holds the seat; only agent's, where set.
func foldTurnOutcomes(rows []usage.TurnLedgerRow, chart map[string]chartSeat, agent string) []TurnOutcomeRow {
	type key struct{ agent, wake string }
	at := map[key]int{}
	out := []TurnOutcomeRow{}
	for _, r := range rows {
		if types.WakeDisposition(r.Disposition) != types.DispositionStarted || agent != "" && r.AgentID != agent {
			continue
		}
		k := key{r.AgentID, r.Wake}
		i, seen := at[k]
		if !seen {
			seat := r.Handle
			if c, ok := chart[r.AgentID]; ok && c.handle != "" {
				seat = c.handle
			}
			at[k], i = len(out), len(out)
			out = append(out, TurnOutcomeRow{Seat: cmp.Or(seat, r.AgentID), AgentID: r.AgentID, Wake: r.Wake})
		}
		spent := r.Input + r.Output + r.CacheRead + r.CacheWrite + r.AuxInput + r.AuxOutput
		o := &out[i]
		o.Turns += r.Turns
		o.Tokens += spent
		if types.TurnOutcome(r.Outcome) == types.OutcomeNothing {
			o.Nothing += r.Turns
			o.NothingTokens += spent
		}
	}
	slices.SortFunc(out, func(a, b TurnOutcomeRow) int {
		return cmp.Or(cmp.Compare(a.Seat, b.Seat), cmp.Compare(a.Wake, b.Wake))
	})
	return out
}
