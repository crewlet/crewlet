// Package orgchart answers [authz.Chart] over one organization: who leads
// whom, a project, a unit and a page container, who sits in a unit, and which
// grants a seat's role carries, read off the tree a company document builds.
//
// # Pure over one tree, and why it is its own package
//
// The relations are arithmetic over an [org.Organization] and nothing else, so
// they live where every caller holding a tree can ask them. Two do. The engine
// asks about the company it is RUNNING, and a /config write asks about the
// two documents it compares — the revision it replaces and the one it
// proposes — because a lead may change only what sits inside a unit they lead
// on BOTH sides of the write, and the running company is neither of those
// (internal/api/configapi). Kept inside the engine, the second caller had no
// way to ask without a running engine holding the very document it is still
// deciding whether to store.
//
// The walks used to sit inside the engine's own methods, where they could only
// be exercised through a running engine against whatever hierarchy that
// fixture happened to have — and measured, deleting both of [Leads]' loops
// left that package's suite green.
//
// A NIL TREE IS AN ERROR, never a `false`: "this node holds no company" and
// "you lead nothing" are opposite facts, and the second refuses a lead their
// own team while the node reports itself healthy. See [authz.Chart].
package orgchart

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Of is the chart over o. A nil o answers every relation [authz.ErrNoChart].
func Of(o *org.Organization) authz.Chart { return chart{o: o} }

// chart is [authz.Chart] over one tree.
type chart struct{ o *org.Organization }

// tree is the organization, or why there is none.
func (c chart) tree() (*org.Organization, error) {
	if c.o == nil {
		return nil, fmt.Errorf("orgchart: no organization to read: %w", authz.ErrNoChart)
	}
	return c.o, nil
}

// Leads reports whether actor is above subject.
//
// TWO RELATIONS, EITHER OF WHICH IS ENOUGH, because a company expresses the
// same authority two ways: `manages:` on a seat, and `lead:` on a unit. A
// check that read one would refuse half the leads in any company that uses
// the other, and which half depends on how the founder wrote their chart.
//
// THE UNIT WALK IS UPWARD. A division's lead leads every team under it, which
// is the same inheritance [org.Organization.EffectiveLead] already applies to
// a unit that names nobody.
func (c chart) Leads(_ context.Context, actor, subject string) (bool, error) {
	o, err := c.tree()
	if err != nil {
		return false, err
	}
	return leads(o, actor, subject), nil
}

func leads(o *org.Organization, actor, subject string) bool {
	if actor == "" || subject == "" || actor == subject {
		// SELF IS NOT A LEAD RELATION. The own-or-lead class checks
		// self first and reaches here only for somebody else, so
		// answering true would make the two indistinguishable in the
		// reason a refusal reports.
		return false
	}
	seat := o.Role(subject)
	if seat == nil {
		// A SUBJECT THE TREE DOES NOT HOLD IS NOT AN ERROR. It is a
		// handle somebody typed for a person who is not in this
		// company, and "you do not lead them" is exactly true.
		return false
	}
	for _, above := range o.Ancestors(seat) {
		if above.Handle() == actor {
			return true
		}
	}
	// THE WHOLE CHAIN, innermost unit first: asking only the seat's own
	// unit would refuse a division lead their own teams.
	for _, unit := range o.UnitChainFor(seat) {
		if lead := o.EffectiveLead(unit); lead != nil && lead.Handle() == actor {
			return true
		}
	}
	return false
}

// LeadsAnyone reports whether actor leads any seat at all — the question a
// decision asks about a record before anybody knows whose it is (see
// [authz.Object.Unresolved]).
//
// THE SAME RELATION [chart.Leads] ASKS, over every seat rather than one, so
// the two can never disagree about somebody.
func (c chart) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	o, err := c.tree()
	if err != nil {
		return false, err
	}
	return leadsAnyone(o, actor), nil
}

// leadsAnyone asks [leads] of every seat rather than restating the relation,
// and answers the common case without that walk: a seat that manages nobody
// and is no unit's effective lead can be nobody's ancestor and nobody's unit
// lead, the only two ways [leads] answers yes.
func leadsAnyone(o *org.Organization, actor string) bool {
	role := o.Role(actor)
	if actor == "" || role == nil {
		return false
	}
	if len(role.Manages) == 0 && !leadsAUnit(o, role) {
		return false
	}
	for seat := range o.AllRoles() {
		if seat != role && leads(o, actor, seat.Handle()) {
			return true
		}
	}
	return false
}

// leadsAUnit reports whether role is some unit's effective lead, BY THE SEAT
// [org.Organization.EffectiveLead] resolves, exactly as [leads] reads it.
func leadsAUnit(o *org.Organization, role *org.Role) bool {
	for unit := range o.AllUnits() {
		if o.EffectiveLead(unit) == role {
			return true
		}
	}
	return false
}

// LeadsProject reports whether actor leads the unit that owns a project, or is
// the seat whose own project it is.
//
// A PROJECT NO UNIT AND NO SEAT DECLARES ANSWERS FALSE rather than erroring:
// the tree was read and holds no owner for it, which is a fact about the
// company rather than about this node.
func (c chart) LeadsProject(_ context.Context, actor, project string) (bool, error) {
	o, err := c.tree()
	if err != nil {
		return false, err
	}
	key := worklet.ProjectKey(project)
	if actor == "" || key == "" {
		return false, nil
	}
	for unit := range o.AllUnits() {
		if worklet.ProjectKey(unit.Project) != key {
			continue
		}
		if lead := o.EffectiveLead(unit); lead != nil && lead.Handle() == actor {
			return true, nil
		}
	}
	// A ROLE'S OWN PROJECT IS LED BY THAT ROLE: a seat that names its own
	// project decides how it is filed, which is the only reading of "the
	// lead" a one-seat project has.
	for role := range o.AllRoles() {
		if worklet.ProjectKey(role.Project) == key && role.Handle() == actor {
			return true, nil
		}
	}
	return false, nil
}

// LeadsContainer reports whether actor leads the unit that owns a page
// container, or is the seat whose own container it is.
//
// [chart.LeadsProject]'s walk over a different field, which is the whole of
// why [authz.Chart] states a separate method: a unit declares its tracker
// project in `project:` and its page container in `space:`, and the two keys
// are unrelated strings.
func (c chart) LeadsContainer(_ context.Context, actor, container string) (bool, error) {
	o, err := c.tree()
	if err != nil {
		return false, err
	}
	key := doclet.ContainerKey(container)
	if actor == "" || key == "" {
		return false, nil
	}
	for unit := range o.AllUnits() {
		if doclet.ContainerKey(unit.Space) != key {
			continue
		}
		if lead := o.EffectiveLead(unit); lead != nil && lead.Handle() == actor {
			return true, nil
		}
	}
	for role := range o.AllRoles() {
		if doclet.ContainerKey(role.Space) == key && role.Handle() == actor {
			return true, nil
		}
	}
	return false, nil
}

// LeadsUnit reports whether actor leads the unit key names, directly or from
// anywhere above it — whether that unit is in actor's SUBTREE.
//
// THE WHOLE CHAIN, itself included: a division lead leads the teams inside
// their division, and the inheritance [org.Organization.EffectiveLead] applies
// within one unit is the same relation one level up.
//
// A KEY NAMING NO UNIT ANSWERS FALSE rather than erroring: the tree was read
// and holds no such unit.
func (c chart) LeadsUnit(_ context.Context, actor, unitKey string) (bool, error) {
	o, err := c.tree()
	if err != nil {
		return false, err
	}
	if actor == "" || unitKey == "" {
		return false, nil
	}
	for _, unit := range o.UnitChainTo(unitKey) {
		if lead := o.EffectiveLead(unit); lead != nil && lead.Handle() == actor {
			return true, nil
		}
	}
	return false, nil
}

// SeatGranted reports whether actor's seat's role carries grant: whether it is
// an AGENT seat of the tree whose role states the grant ([carries]).
//
// A seat the tree does not hold, a human seat and a grant this build does not
// know answer false rather than erroring: the tree was read and grants nothing
// of the kind — see [authz.Chart.SeatGranted].
func (c chart) SeatGranted(_ context.Context, actor string, grant authz.SeatGrant) (bool, error) {
	o, err := c.tree()
	if err != nil {
		return false, err
	}
	return carries(o.AgentSeatByHandle(actor), grant), nil
}

// GrantedSeats is the handle of every agent seat of o whose role carries grant,
// in the tree's walk order: who a wake about what the grant opens is for.
//
// THE SAME RULE [chart.SeatGranted] ASKS ([carries]), over every seat rather
// than one, so the seats a grant admits and the seats it reaches can never
// differ. A nil tree grants nobody.
func GrantedSeats(o *org.Organization, grant authz.SeatGrant) []string {
	if o == nil {
		return nil
	}
	var out []string
	for role := range o.AllRoles() {
		if carries(role, grant) {
			out = append(out, role.Handle())
		}
	}
	return out
}

// carries reports whether role is an agent seat whose role states grant.
//
// AN AGENT'S ALONE: a person is decided by their own permissions, and a grant
// written on a human seat (which the config layer refuses on admission) opens
// nothing. A grant this build does not know opens nothing either, as an
// unknown permission does not.
func carries(role *org.Role, grant authz.SeatGrant) bool {
	if role == nil || role.IsHuman() {
		return false
	}
	switch grant {
	case authz.SeatGrantHealth:
		return role.HealthTools
	}
	return false
}

// MemberOf reports whether actor's seat sits in the unit key names or in any
// unit below it: who a share with that team admits.
//
// THE SEAT'S OWN CHAIN, outermost unit down to the one holding it, compared BY
// POINTER against the unit the key resolves to, for the reason
// [org.Organization.UnitChainFor] finds a seat by pointer: two units carrying
// one name must not answer for each other. A unit's lead seated in the unit
// above it is NOT a member, because they are not in the team; a share that
// should reach them names their seat or their own unit.
//
// A KEY NAMING NO UNIT, or an actor the tree does not seat, answers false
// rather than erroring: the tree was read and holds no such membership.
func (c chart) MemberOf(_ context.Context, actor, unitKey string) (bool, error) {
	o, err := c.tree()
	if err != nil {
		return false, err
	}
	if actor == "" || unitKey == "" {
		return false, nil
	}
	seat, unit := o.Role(actor), o.Unit(unitKey)
	if seat == nil || unit == nil {
		return false, nil
	}
	for _, in := range o.UnitChainFor(seat) {
		if in == unit {
			return true, nil
		}
	}
	return false, nil
}
