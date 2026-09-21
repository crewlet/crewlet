package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// The budgets answer: the CAP the engine enforces against, paired with the
// DURABLE counter it enforces with.
//
// TWO SPANS, and they are the two that belong together. The cap is config; the
// counter is every node's spend since the last deliberate reset, which is what
// the gate actually refuses against, and the refusal stamp beside it is the
// gate's own record of saying no. The live meter a dashboard is pushed is this
// same counter, so it is not a third figure here.
//
// There used to be a third, `live_used`, labelled as "this process": the
// per-seat token totals the live projection summed from the turns it happened
// to have seen since it started. It shared a span with nothing an operator
// could name, and its org half summed an empty list and was null on every
// node. What a seat spent over a stated window is the spend rollup's per-agent
// row, which is one aggregation for every screen that shows spend.
//
// So `durable: false` means UNREADABLE, never zero: a company drawn at 0% of
// its budget when the truth is that nobody looked is the lie this shape exists
// to avoid.

// Budgets answers the whole budget surface.
//
// Registered only beside a company source, so [Sources.Company] is never nil
// here; what can be nil is the epoch it returns.
func (s Sources) budgets(ctx context.Context, _ Params) (any, error) {
	out := map[string]any{
		"org":     map[string]any{"durable_used": 0, "max_tokens": 0},
		"seats":   []any{},
		"durable": false,
	}
	company, roster := s.Company()
	if company == nil {
		// No epoch: the caps are unknown, so nothing on this screen can be
		// stated. Returning zeros with `durable: false` says exactly that.
		return out, nil
	}
	// THE COMPANY'S OWN ORG, derived from this node's chart rows rather
	// than re-resolved from the document: a stored revision carries no
	// seats at all, so the derivation this replaced answered an EMPTY
	// organization for every running company.
	if roster == nil {
		//nolint:nilerr // An unbuildable org means nothing on this screen can
		// be stated; zeros with durable:false say exactly that.
		return out, nil
	}

	// The durable half. A counter that cannot be read leaves `durable` false
	// and every figure absent — the alternative is drawing a company at 0%
	// of its budget when the truth is that nobody looked.
	used := map[string]coord.Usage{}
	if s.Budget != nil {
		rows, err := s.Budget.Usage(ctx)
		if err != nil {
			//nolint:nilerr // Deliberate: see the paragraph above.
			return out, nil
		}
		for _, u := range rows {
			used[u.Scope] = u
		}
		out["durable"] = true
	}

	orgRow := used[coord.OrgScope]
	out["org"] = map[string]any{
		"max_tokens":         company.TokenBudget,
		"durable_used":       orgRow.Used,
		"durable_updated_at": isoOrEmpty(orgRow.UpdatedAt),
		"refused_at":         isoOrEmpty(orgRow.RefusedAt),
	}

	seats := []any{}
	for role := range roster.AllRoles() {
		id, ok := roster.AgentIDFor(role)
		if !ok {
			// A human seat spends nothing: it is addressable and never
			// spawned, so a row for it would be a permanent zero a reader
			// has to learn to ignore.
			continue
		}
		row := used[coord.AgentScope(id.String())]
		seats = append(seats, map[string]any{
			"agent_id":           id.String(),
			"role":               role.Name,
			"handle":             role.Handle(),
			"max_tokens":         role.TokenBudget,
			"durable_used":       row.Used,
			"durable_updated_at": isoOrEmpty(row.UpdatedAt),
			"refused_at":         isoOrEmpty(row.RefusedAt),
		})
	}
	out["seats"] = seats
	return out, nil
}
