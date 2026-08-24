package config

import (
	"github.com/crewlet/crewlet/internal/org"
)

// Organization builds the runtime company from this revision: the seats,
// the hierarchy, and the org-wide facts every seat reads.
//
// It NORMALIZES the result — root seats carrying a unit reference move into
// that unit, leads and channels cascade, unit credentials layer under their
// members', a lead gains a manages entry for every unmanaged direct member,
// and a manages entry naming a unit expands to its seats. Doing that at the
// boundary is why nothing downstream has to know whether a seat was
// authored inside its unit or moved into it.
//
// The returned organization is a fresh tree: the config it came from is
// untouched, because a stored revision is read again on the next apply and
// a normalisation that mutated it would compound.
func (c *Company) Organization() (*org.Organization, error) {
	o := c.organization()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// organization builds and normalises without validating, so [Company.Validate]
// can report the org's failures alongside its own rather than stopping at
// the first.
func (c *Company) organization() *org.Organization {
	o := &org.Organization{
		Name:             c.Name,
		Mission:          c.Mission,
		Vision:           c.Vision,
		Policies:         append([]string(nil), c.Policies...),
		TokenBudget:      c.TokenBudget,
		ConfluenceSpaces: append([]string(nil), c.Knowledge.ConfluenceSpaces...),
		PlaneProjects:    append([]string(nil), c.Knowledge.PlaneProjects...),
	}
	for i := range c.Roles {
		o.Roles = append(o.Roles, c.Roles[i].Seat())
	}
	for i := range c.Units {
		o.Units = append(o.Units, c.Units[i].OrgUnit())
	}
	o.Normalize()
	return o
}

// DanglingRefs reports references the org resolves to nothing — a unit
// naming a lead that does not exist, a seat naming a unit that does not.
//
// They are NOT validation failures, which is why they are a separate call
// rather than part of [Company.Validate]. Live config management bootstraps
// an org in pieces, so a unit can legitimately land before the seat that
// leads it, and every reader already treats a dangling lead as no lead.
// Refusing the revision would make the intermediate state unreachable;
// reporting it is what an operator needs.
func (c *Company) DanglingRefs() []org.DanglingRef {
	return c.organization().DanglingRefs()
}
