package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ChartAuthority answers [authz.Chart] against the epoch this node is running.
//
// # Why it is three-valued, and what the two-valued one cost
//
// This replaces `LeadsProjectOf`, whose seam was `func(ctx, actor, project)
// bool` and whose body opened with:
//
//	c := e.Company()
//	if c == nil || c.Org == nil { return false }
//
// A node holds no company while it is booting, while it is installing a
// revision, and for as long as it is behind the chart log. Every one of those
// moments answered "you do not lead this project" about a person who leads it
// — so a lead was locked out of their own project's policy by a node that was
// merely lagging, the refusal named the project rather than the lag, and the
// node reported itself healthy throughout. The two facts are opposite and were
// wearing one value.
//
// So absent is an ERROR here. [authz.Decide] turns it into an UNKNOWN
// decision, which a surface answers 503 to rather than 403, and the admin
// path is checked before the chart is asked at all — an operator holding the
// deployment's own grant is never told "I cannot tell".
//
// PER CALL, which is the one thing the old seam got right: the writer outlives
// a revision, and a chart captured once would answer for a company that has
// since moved.
type ChartAuthority struct{ engine *Engine }

var _ authz.Chart = ChartAuthority{}

// ChartAuthorityOf is the seam over one engine.
func ChartAuthorityOf(e *Engine) ChartAuthority { return ChartAuthority{engine: e} }

// Leads reports whether actor is above subject in the chart.
//
// TWO RELATIONS, EITHER OF WHICH IS ENOUGH, because a company expresses the
// same authority two ways: `manages:` on a seat, and `lead:` on a unit. A
// check that read one would refuse half the leads in any company that uses
// the other, and which half depends on how the founder wrote their chart.
//
// THE UNIT WALK IS UPWARD. A division's lead leads every team under it, which
// is the same inheritance [org.Organization.EffectiveLead] already applies to
// a unit that names nobody — refusing there would send somebody to look for an
// authority nobody holds.
func (c ChartAuthority) Leads(_ context.Context, actor, subject string) (bool, error) {
	o, err := c.org()
	if err != nil {
		return false, err
	}
	return leadsInChart(o, actor, subject), nil
}

// leadsInChart is [ChartAuthority.Leads] over a chart that is already in hand.
//
// SPLIT OUT because the two halves fail differently and only one of them
// needs an engine: whether a node HAS a chart is a fact about the node, and
// who leads whom is arithmetic over a tree. Kept together, the walk could
// only ever be exercised through a running engine against whatever hierarchy
// that fixture happened to have — which is how a walk comes to be asserted by
// nothing. Measured: with the walk inside the method, deleting both loops
// left this package's suite green.
func leadsInChart(o *org.Organization, actor, subject string) bool {
	if actor == "" || subject == "" || actor == subject {
		// SELF IS NOT A LEAD RELATION. The own-or-lead class checks
		// self first and reaches here only for somebody else, so
		// answering true would make the two indistinguishable in the
		// reason a refusal reports.
		return false
	}
	seat := o.Role(subject)
	if seat == nil {
		// A SUBJECT THE CHART DOES NOT HOLD IS NOT AN ERROR. It is a
		// handle somebody typed for a person who is not in this
		// company, and "you do not lead them" is exactly true.
		return false
	}
	for _, above := range o.Ancestors(seat) {
		if above.Handle() == actor {
			return true
		}
	}
	// THE WHOLE CHAIN, innermost unit first, which is what
	// [org.Organization.UnitChainFor] already walks for the prompt
	// builder. Asking only the seat's own unit would refuse a division
	// lead their own teams — the inheritance EffectiveLead applies within
	// one unit is the same relation one level up.
	for _, unit := range o.UnitChainFor(seat) {
		if lead := o.EffectiveLead(unit); lead != nil && lead.Handle() == actor {
			return true
		}
	}
	return false
}

// LeadsProject reports whether actor leads the unit that owns a project, or is
// the seat whose own project it is.
//
// A PROJECT NO UNIT AND NO SEAT DECLARES ANSWERS FALSE rather than erroring:
// the chart was read and holds no owner for it, which is a fact about the
// company rather than about this node. Only an unreadable chart is unknown.
func (c ChartAuthority) LeadsProject(_ context.Context, actor, project string) (bool, error) {
	o, err := c.org()
	if err != nil {
		return false, err
	}
	return leadsProjectInChart(o, actor, project), nil
}

// leadsProjectInChart is the walk, split out for the reason [leadsInChart] is.
func leadsProjectInChart(o *org.Organization, actor, project string) bool {
	key := tracker.ProjectKey(project)
	if actor == "" || key == "" {
		return false
	}
	for unit := range o.AllUnits() {
		if tracker.ProjectKey(unit.Project) != key {
			continue
		}
		if lead := o.EffectiveLead(unit); lead != nil && lead.Handle() == actor {
			return true
		}
	}
	// A ROLE'S OWN PROJECT IS LED BY THAT ROLE. A seat that names its own
	// project decides how it is filed, which is the only reading of "the
	// lead" a one-seat project has.
	for role := range o.AllRoles() {
		if tracker.ProjectKey(role.Project) == key && role.Handle() == actor {
			return true
		}
	}
	return false
}

// LeadsUnit reports whether actor leads the unit key names, directly or from
// anywhere above it.
//
// THE WHOLE CHAIN, itself included, which is the same reading [Leads] takes of
// a seat's unit chain: a division lead leads the teams inside their division,
// and the inheritance [org.Organization.EffectiveLead] applies within one unit
// is the same relation one level up. A check that asked the unit alone would
// refuse a division lead editing a team they are plainly responsible for.
//
// A KEY NAMING NO UNIT ANSWERS FALSE rather than erroring: the chart was read
// and holds no such unit, which is a fact about the company rather than about
// this node. Only an unreadable chart is unknown.
func (c ChartAuthority) LeadsUnit(_ context.Context, actor, unitKey string) (bool, error) {
	o, err := c.org()
	if err != nil {
		return false, err
	}
	return leadsUnitInChart(o, actor, unitKey), nil
}

// leadsUnitInChart is the walk, split out for the reason [leadsInChart] is.
func leadsUnitInChart(o *org.Organization, actor, unitKey string) bool {
	if actor == "" || unitKey == "" {
		return false
	}
	for _, unit := range o.UnitChainTo(unitKey) {
		if lead := o.EffectiveLead(unit); lead != nil && lead.Handle() == actor {
			return true
		}
	}
	return false
}

// org is the running company's chart, or why this node cannot answer.
//
// ONE PLACE, because both methods need it and the whole point of this type is
// that "no chart" is never a `false` — written twice, one of them eventually
// returns the zero value and the collapse is back.
func (c ChartAuthority) org() (*org.Organization, error) {
	if c.engine == nil {
		return nil, fmt.Errorf("engine: %w", authz.ErrNoChart)
	}
	company := c.engine.Company()
	if company == nil || company.Org == nil {
		return nil, fmt.Errorf("engine: this node is not running a company yet, "+
			"so it cannot say who leads whom: %w", authz.ErrNoChart)
	}
	return company.Org, nil
}
