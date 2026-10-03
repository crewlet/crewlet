package chart_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// A RUNTIME HALF IS HELD TO THE SEAT'S RULES, AND REFUSED BEFORE ANYTHING IS
// SEALED.
//
// A company file is validated whole before it is applied, but a seat written
// through the chart carries its runtime half as bytes this domain cannot read —
// so a `token_budget` of `{"day": 0}` reached every node, where the counters
// read it as a window nobody may spend in, and a `{"year": 5}` as a key no
// window answers to. The decide asks internal/org, against the kind the row
// holds, the very rules a company file's seat is held to, and asks before the
// seal: a write refused here must not have written a credential it carried to
// the company's secret store on its way to the refusal. Mutation: drop the
// check from the decide, or move it after the seal, and a case here fails.
func TestARuntimeHalfIsHeldToTheSeatsRulesBeforeAnythingIsSealed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind    chart.SeatKind
		runtime string
		says    string
	}{
		"a ceiling of 0": {chart.SeatAgent, `"token_budget": {"day": 0}`,
			"remove `token_budget.day` for no daily ceiling"},
		"a window that is no window": {chart.SeatAgent, `"token_budget": {"year": 5}`,
			"remove `token_budget.year`"},
		"a model chain on a human seat": {chart.SeatHuman, `"llm": "claude"`,
			"llm"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newWriteRig(t)
			r.batch("op-seat", seatOp(tc.kind, "sarah-chen", ""))
			before, err := r.log.End(t.Context())
			if err != nil {
				t.Fatalf("read the log's end: %v", err)
			}
			// A LITERAL CREDENTIAL BESIDE THE FAULT, which a decide that
			// sealed before it checked would have written to the store.
			_, err = r.writer.WriteSeat(t.Context(), "op-runtime", chart.SeatContent{
				Handle: "sarah-chen", Name: "Sarah Chen",
				Runtime: json.RawMessage(`{` + tc.runtime +
					`, "mcp_env": {"github": {"GITHUB_TOKEN": "ghp-LITERAL"}}}`),
			})
			if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the write answered %v, want a refusal saying %q", err, tc.says)
			}
			if sealed := r.storedSecrets(); len(sealed) > 0 {
				t.Errorf("a refused write sealed %v on its way to the refusal", sealed)
			}
			if after, err := r.log.End(t.Context()); err != nil || after != before {
				t.Errorf("a refused write reached the log: end %d -> %d (%v)", before, after, err)
			}
		})
	}

	// THE COUNTERFACTUAL, so the table cannot pass on a check that refuses
	// every half: one token a day is a ceiling, and the write lands.
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))
	if _, err := r.seat("op-runtime", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen",
		Runtime: json.RawMessage(`{"token_budget": {"day": 1}}`),
	}); err != nil {
		t.Errorf("a ceiling of one token a day was refused: %v", err)
	}
}

// storedSecrets is every name the rig's sealer has stored a value under.
func (r *writeRig) storedSecrets() []string {
	r.sealer.mu.Lock()
	defer r.sealer.mu.Unlock()
	var out []string
	for name := range r.sealer.sealed {
		out = append(out, name)
	}
	return out
}
