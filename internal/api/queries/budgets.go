package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events/types"
)

// The budgets answer: the CEILINGS the engine enforces against, paired with the
// DURABLE counters it enforces with, window by window.
//
// Every scope states ALL THREE calendar windows — the day, the ISO week and the
// month on the company's clock — with what it has spent in each, because what a
// seat spent this week is a fact whether or not a ceiling is written for the
// week. A window a ceiling caps also carries its `limit`, its refusal stamp and
// the engine's `state` (ok, near or refusing); one nothing caps carries no
// `limit` at all, never 0, which would state a range of nothing that is already
// full. The windows are built by [engine.BudgetWindows], the one implementation
// the live `budget_meters` frame is built from too, so the answer and the push
// never state one counter two ways; and `near_fraction` is served beside them,
// so a surface that draws a threshold mark draws the engine's.
//
// So `durable: false` means UNREADABLE, never zero: a company drawn at 0% of
// its budget when the truth is that nobody looked is the lie this shape exists
// to avoid, and every window list is empty rather than a list of zeroes.

// budgets answers the whole budget surface.
//
// Registered only beside a company source, so [Sources.Company] is never nil
// here; what can be nil is the epoch it returns.
func (s Sources) budgets(ctx context.Context, _ Params) (any, error) {
	out := map[string]any{
		"timezone":      "",
		"durable":       false,
		"near_fraction": engine.BudgetNearFraction,
		"org":           map[string]any{"windows": []types.BudgetWindow{}},
		"seats":         []any{},
	}
	company := s.Company()
	if company == nil {
		// No epoch: the ceilings and the clock are unknown, so nothing on
		// this screen can be stated.
		return out, nil
	}
	out["timezone"] = company.Location().String()
	organization, err := company.Organization()
	if err != nil {
		//nolint:nilerr // An unbuildable org means nothing on this screen can
		// be stated; empty windows with durable:false say exactly that.
		return out, nil
	}

	// The windows are cut on the company's own clock at this answer's
	// instant, which is what the gate cuts a charge on.
	windows := coord.WindowsAt(s.clock(), company.Location())

	// The durable half. A counter that cannot be read leaves `durable` false
	// and every figure absent.
	if s.Budget == nil {
		return out, nil
	}
	rows, err := s.Budget.Usage(ctx, windows)
	if err != nil {
		//nolint:nilerr // Deliberate: see the paragraph above.
		return out, nil
	}
	used := make(map[string]coord.Usage, len(rows))
	for _, u := range rows {
		used[u.Scope] = u
	}
	read := func(scope string) coord.Usage {
		if u, found := used[scope]; found {
			return u
		}
		return coord.Unspent(scope, windows)
	}
	out["durable"] = true
	out["org"] = map[string]any{
		"windows": engine.BudgetWindows(coord.Caps(organization.TokenBudget), read(coord.OrgScope)),
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
		seats = append(seats, map[string]any{
			"agent_id": id.String(),
			"role":     role.Name,
			"handle":   role.Handle(),
			"windows":  engine.BudgetWindows(coord.Caps(role.TokenBudget), read(coord.AgentScope(id.String()))),
		})
	}
	out["seats"] = seats
	return out, nil
}
