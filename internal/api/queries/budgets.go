package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// The budgets answer: the CAP the engine enforces against, paired with the
// DURABLE counter it enforces with.
//
// THREE SPANS SIT ON THIS SCREEN and only two of them are comparable. The cap
// is config. The durable counter is every process since the last deliberate
// reset — what the engine actually refuses against — and those two go together.
// The LIVE meter beside them is this process's own run, and it shares a span
// with neither, which is why it is a separate field a reader can see is
// separate rather than a number silently mixed into the other two.
//
// So `durable: false` means UNREADABLE, never zero, and live_used is null
// rather than 0 on a node with no meter: a zero is a measurement, and printing
// one for "we could not look" is the lie this shape exists to avoid.

// Budgets answers the whole budget surface.
func (s Sources) budgets(ctx context.Context, _ Params) (any, error) {
	out := map[string]any{
		"org":     map[string]any{"durable_used": 0, "max_tokens": 0},
		"seats":   []any{},
		"durable": false,
	}
	if s.Company == nil {
		return out, nil
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

	// The durable half. A store that cannot be read leaves `durable` false
	// and every figure absent — the alternative is drawing a company at 0%
	// of its budget when the truth is that nobody looked.
	used := map[string]store.Usage{}
	if s.Budget != nil {
		rows, err := s.Budget.List(ctx)
		if err != nil {
			//nolint:nilerr // Deliberate: see the paragraph above.
			return out, nil
		}
		for _, u := range rows {
			used[u.Scope] = u
		}
		out["durable"] = true
	}

	orgRow := used[store.OrgScope]
	out["org"] = map[string]any{
		"max_tokens":         company.TokenBudget,
		"durable_used":       orgRow.Used,
		"durable_updated_at": isoOrEmpty(orgRow.UpdatedAt),
		"live_used":          s.liveOrgUsed(),
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
		row := used[store.AgentScope(id.String())]
		seats = append(seats, map[string]any{
			"agent_id":           id.String(),
			"role":               role.Name,
			"handle":             role.Handle(),
			"max_tokens":         role.TokenBudget,
			"durable_used":       row.Used,
			"durable_updated_at": isoOrEmpty(row.UpdatedAt),
			// NULL, not zero, when this node has no live meter for the
			// seat: "nothing spent this run" and "no meter here" are
			// different facts and the client renders them differently.
			"live_used": s.liveSeatUsed(role),
		})
	}
	out["seats"] = seats
	return out, nil
}

// liveOrgUsed is this process's own meter, or nil.
//
// Summed from the projection's per-seat overlays rather than held as its own
// counter: the overlay is what the engine reports and a second accumulator
// here would be a number that drifts from the one on the seat rows beside it.
func (s Sources) liveOrgUsed() any {
	if s.State == nil {
		return nil
	}
	total, any := 0, false
	for _, row := range s.State.MergeAgents(nil) {
		if n, ok := row["total_tokens"].(int); ok {
			total += n
			any = true
		}
	}
	if !any {
		return nil
	}
	return total
}

// liveSeatUsed is one seat's meter on this node, or nil.
func (s Sources) liveSeatUsed(role *org.Role) any {
	if s.State == nil || role == nil {
		return nil
	}
	overlay := s.State.AgentOverlay(role.Name)
	if overlay == nil {
		return nil
	}
	return overlay.TotalTokens
}
