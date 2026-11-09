package advisor

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// place is what a warning's path names in the company document: the seat or
// the unit it sits under, and the schedule when it is one.
type place struct {
	role *config.Role
	unit *config.Unit
	// schedule is the index into the role's or the unit's schedules, -1
	// when the path names none.
	schedule int
}

// locate walks the company to the seat or unit segs sit under. Matched on
// the structured path ([config.Warning.Segments]) rather than its rendering,
// because a key may hold a dot.
func locate(c *config.Company, segs config.Path) place {
	p := place{schedule: -1}
	for role, path := range c.EachRole() {
		if hasPrefix(segs, path) {
			p.role = role
			p.schedule = scheduleIn(segs, path)
			return p
		}
	}
	var walk func(units []config.Unit, path config.Path) bool
	walk = func(units []config.Unit, path config.Path) bool {
		for i := range units {
			here := append(slices.Clone(path), i)
			if walk(units[i].Children, append(slices.Clone(here), "children")) {
				return true
			}
			if hasPrefix(segs, here) {
				p.unit = &units[i]
				p.schedule = scheduleIn(segs, here)
				return true
			}
		}
		return false
	}
	walk(c.Units, config.Path{"units"})
	return p
}

// hasPrefix reports whether segs begins with prefix.
func hasPrefix(segs, prefix config.Path) bool {
	return len(segs) >= len(prefix) && slices.Equal(segs[:len(prefix)], prefix)
}

// scheduleIn is the schedule index segs names right under owner, or -1.
func scheduleIn(segs, owner config.Path) int {
	rest := segs[len(owner):]
	if len(rest) < 2 || rest[0] != "schedules" {
		return -1
	}
	i, ok := rest[1].(int)
	if !ok {
		return -1
	}
	return i
}

// schedules is the schedule list the place's owner holds.
func (p place) schedules() []org.Schedule {
	switch {
	case p.role != nil:
		return p.role.Schedules
	case p.unit != nil:
		return p.unit.Schedules
	}
	return nil
}

// theSchedule is the schedule the place names, and false when it names none.
func (p place) theSchedule() (org.Schedule, bool) {
	list := p.schedules()
	if p.schedule < 0 || p.schedule >= len(list) {
		return org.Schedule{}, false
	}
	return list[p.schedule], true
}

// subject is the place as a finding names it.
func (p place) subject() Subject {
	if s, ok := p.theSchedule(); ok {
		out := Subject{Kind: SubjectSchedule, Schedule: s.Name, Name: s.Name}
		if p.role != nil {
			out.Seat = p.role.Seat().Handle()
		} else {
			out.Unit = p.unit.ID
		}
		return out
	}
	if p.role != nil {
		return Subject{Kind: SubjectSeat, Seat: p.role.Seat().Handle(), Name: p.role.Name}
	}
	return Subject{Kind: SubjectCompany}
}
