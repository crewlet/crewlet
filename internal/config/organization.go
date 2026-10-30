package config

import (
	"github.com/crewlet/crewlet/internal/org"
)

// Organization builds the runtime company from this revision: the seats,
// the hierarchy, and the org-wide facts every seat reads.
//
// It NORMALIZES the result: root seats carrying a unit reference move into
// that unit, leads and channels cascade, unit credentials layer under their
// agent members', a lead gains a manages entry for every direct member no
// direct member of its unit already manages, and a manages entry naming a
// unit expands to its seats ([org.Organization.Normalize] has the rules
// and their order). Doing that at the boundary is why nothing downstream
// has to know whether a seat was authored inside its unit or moved into it.
//
// The returned organization is a fresh tree: the config it came from is
// untouched, because a stored revision is read again on the next apply and
// a normalisation that mutated it would compound.
func (c *Company) Organization() (*org.Organization, error) {
	o, _ := c.organization()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// organization builds and normalises without validating, so [Company.Validate]
// can report the org's failures alongside its own rather than stopping at
// the first. It also returns where each seat and unit of the result was
// authored (see [identityIndex]), recorded before normalization moves any.
func (c *Company) organization() (*org.Organization, *identityIndex) {
	index := newIdentityIndex()
	o := &org.Organization{
		Name:           c.Name,
		Mission:        c.Mission,
		Vision:         c.Vision,
		Policies:       append([]string(nil), c.Policies...),
		TokenBudget:    c.TokenBudget.Ceilings(),
		KnowledgeScope: append([]string(nil), c.Knowledge.KnowledgeScope...),
	}
	for i := range c.Roles {
		seat := c.Roles[i].Seat()
		index.addSeat(seat, &c.Roles[i], idx(field("roles"), i))
		o.Roles = append(o.Roles, seat)
	}
	for i := range c.Units {
		unit := c.Units[i].Unit()
		index.addUnit(unit, &c.Units[i], idx(field("units"), i))
		o.Units = append(o.Units, unit)
	}
	o.Normalize()
	c.bindSeatTools(o)
	c.bindSeatCredentials(o)
	return o, index
}

// DanglingRefs reports every reference this revision resolves to nothing
// ([org.Organization.DanglingRefs]: a unit's lead, a root seat's unit, a
// manages entry).
//
// They are NOT validation failures, which is why they are a separate call
// rather than part of [Company.Validate]. Live config management bootstraps
// an org in pieces, so a unit can legitimately land before the seat that
// leads it, and every reader already treats a dangling lead as no lead.
// Refusing the revision would make the intermediate state unreachable;
// reporting it is what an operator needs.
func (c *Company) DanglingRefs() []org.DanglingRef {
	o, _ := c.organization()
	return o.DanglingRefs()
}
