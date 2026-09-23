package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// The budgets answer: the CAP the engine enforces against, paired with the
// DURABLE counter it enforces with.
//
// TWO SPANS, and they are the two that belong together. The cap is config; the
// counter is every node's spend in the same calendar window the cap is
// written for, which is what the gate actually refuses against, and the
// refusal stamp beside it is the gate's own record of saying no. The live
// meter a dashboard is pushed is this same counter, so it is not a third
// figure here.
//
// ONE WINDOW PER SCOPE, because the answer carries one figure per scope: each
// scope's binding window ([coord.Usage.Binding]) — a window that is refusing
// first, else the capped window with the least room left — with that window's
// ceiling as `max_tokens` and its refusal as `refused_at`, so a cap is never
// paired with another window's spend. A scope that caps nothing states its
// month, the widest spend the counter keeps, under a `max_tokens` of 0.
//
// There used to be a third figure, `live_used`, labelled as "this process":
// the per-seat token totals the live projection summed from the turns it
// happened to have seen since it started. It shared a span with nothing an
// operator could name, and its org half summed an empty list and was null on
// every node. What a seat spent over a stated window is the spend rollup's
// per-agent row, which is one aggregation for every screen that shows spend.
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
	company := s.Company()
	if company == nil {
		// No epoch: the caps are unknown, so nothing on this screen can be
		// stated. Returning zeros with `durable: false` says exactly that.
		return out, nil
	}
	organization, err := company.Organization()
	if err != nil {
		//nolint:nilerr // An unbuildable org means nothing on this screen can
		// be stated; zeros with durable:false say exactly that.
		return out, nil
	}

	// The windows are cut on the company's own clock at this answer's
	// instant, which is what the gate cuts a charge on.
	windows := coord.WindowsAt(s.clock(), company.Location())

	// The durable half. A counter that cannot be read leaves `durable` false
	// and every figure absent — the alternative is drawing a company at 0%
	// of its budget when the truth is that nobody looked.
	used := map[string]coord.Usage{}
	if s.Budget != nil {
		rows, err := s.Budget.Usage(ctx, windows)
		if err != nil {
			//nolint:nilerr // Deliberate: see the paragraph above.
			return out, nil
		}
		for _, u := range rows {
			used[u.Scope] = u
		}
		out["durable"] = true
	}
	read := func(scope string) coord.Usage {
		if u, found := used[scope]; found {
			return u
		}
		return coord.Unspent(scope, windows)
	}

	orgRow := read(coord.OrgScope)
	orgSlot, orgCap, _ := orgRow.Binding(coord.Caps(organization.TokenBudget))
	out["org"] = map[string]any{
		"max_tokens":         orgCap,
		"durable_used":       orgSlot.Used,
		"durable_updated_at": isoOrEmpty(orgRow.UpdatedAt),
		"refused_at":         isoOrEmpty(orgSlot.RefusedAt),
	}

	seats := []any{}
	for role := range organization.AllRoles() {
		id, ok := organization.AgentIDFor(role)
		if !ok {
			// A human seat spends nothing: it is addressable and never
			// spawned, so a row for it would be a permanent zero a reader
			// has to learn to ignore.
			continue
		}
		row := read(coord.AgentScope(id.String()))
		slot, seatCap, _ := row.Binding(coord.Caps(role.TokenBudget))
		seats = append(seats, map[string]any{
			"agent_id":           id.String(),
			"role":               role.Name,
			"handle":             role.Handle(),
			"max_tokens":         seatCap,
			"durable_used":       slot.Used,
			"durable_updated_at": isoOrEmpty(row.UpdatedAt),
			"refused_at":         isoOrEmpty(slot.RefusedAt),
		})
	}
	out["seats"] = seats
	return out, nil
}
