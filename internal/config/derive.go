package config

import (
	"slices"

	"github.com/crewlet/crewlet/internal/org"
)

// Derived is the hierarchy the engine derives from a company document, as
// data: every conclusion a reader of the document would otherwise have to
// reach again by re-implementing the org model.
//
// # Why the engine says it rather than a client working it out
//
// Because each rule here is one a second implementation gets wrong: a handle
// is a slug with Go's own case mapping, a root seat carrying `unit:` moves into
// that unit, a lead and a channel cascade to child units that set none, a
// `manages` entry naming a unit stands for its seats, a lead manages the
// members nobody else in the unit manages, and the primary manager is the
// first seat listing one in the engine's own seat order. A dashboard that
// derived these in TypeScript had already diverged from the engine on three of
// them and on the handle of a name with a dotted capital I. So the engine
// derives them once, from the same normalized organization it runs, and a
// client draws what it is told.
//
// Every list is null when it is empty, as Go marshals a nil slice.
type Derived struct {
	// Seats is every seat, in the engine's own order (root seats first, then
	// each unit's subtree), which is the order the primary manager is chosen
	// in.
	Seats []DerivedSeat `json:"seats"`
	// Units is every unit, depth first, parents before children.
	Units []DerivedUnit `json:"units"`
}

// DerivedSeat is one seat as the engine runs it.
type DerivedSeat struct {
	// Path is where the seat was written in the document (units[0].roles[1]),
	// even when its `unit:` reference moved it. Omitted where there is no
	// document to point into; see [Derived.WithoutPaths].
	Path string `json:"path,omitempty"`
	// Handle is the seat's identity: its declared handle, or the slug of its
	// name.
	Handle string `json:"handle"`
	Name   string `json:"name"`
	// Kind is agent or human, never empty: an unannotated seat is an agent.
	Kind string `json:"kind"`
	// UnitPath is the authored path of the unit the seat sits in once root
	// seats are placed, and empty for a seat at the root. Omitted with Path.
	UnitPath string `json:"unit_path,omitempty"`
	// PlacedByRef is a seat written at the root that its `unit:` reference
	// moved into a unit.
	PlacedByRef bool `json:"placed_by_ref"`
	// Manager is the handle of the primary manager, the first seat in
	// engine order whose manages lists this one ([org.Organization.Manager]),
	// and empty for a seat nobody manages.
	Manager string `json:"manager"`
	// Managers is every seat whose manages lists this one once unit
	// references are expanded, in engine order, the primary manager first.
	Managers []string `json:"managers"`
	// Reports is the handles of the seats this one manages, explicit and
	// automatic, in the order its normalized manages list holds them. An
	// entry naming nothing is not a report.
	Reports []string `json:"reports"`
	// AutoReports is the subset of Reports this seat manages because it leads
	// their unit, not because anyone wrote it.
	AutoReports []string `json:"auto_reports"`
	// OnboardingChain is the names of the units above the seat, outermost
	// first: with the company's name and the seat's own, what its onboarding
	// marker is hashed from, so a change to any of them makes the seat
	// onboard again.
	OnboardingChain []string `json:"onboarding_chain"`
}

// DerivedUnit is one unit as the engine runs it.
type DerivedUnit struct {
	// Path is where the unit was written. Omitted with a seat's.
	Path string `json:"path,omitempty"`
	Name string `json:"name"`
	// Type is the effective type: what the unit declares, or team.
	Type string `json:"type"`
	// Lead is the handle of the effective lead, declared or inherited, and
	// empty when the unit has none or names a seat that does not exist.
	Lead string `json:"lead"`
	// LeadInherited is a lead the unit took from an ancestor rather than
	// declaring itself.
	LeadInherited bool `json:"lead_inherited"`
	// Channel is the effective channel, declared or inherited.
	Channel string `json:"channel"`
	// ChannelInherited is a channel taken from an ancestor.
	ChannelInherited bool `json:"channel_inherited"`
	// Seats is the handles of the unit's direct members once root seats are
	// placed, in the order the engine holds them.
	Seats []string `json:"seats"`
}

// Derive is the hierarchy the engine derives from c.
//
// It reads the organization [Company.Organization] builds, normalized exactly
// as a running node normalizes it, and asks that organization's own readers
// every question a reader could get wrong: the manager rule, the reports, the
// effective lead, the unit chain. Nothing here decides a rule of its own. It
// does not validate: a document with problems still has a hierarchy, and a
// person fixing those problems needs to see it.
func Derive(c *Company) Derived {
	o, x := c.organization()
	var out Derived

	homes := make(map[*org.Role]*org.Unit)
	for u := range o.AllUnits() {
		for _, r := range u.Roles {
			homes[r] = u
		}
	}

	for r := range o.AllRoles() {
		seat := DerivedSeat{
			Path:   x.seats[r].String(),
			Handle: r.Handle(),
			Name:   r.Name,
			Kind:   string(org.KindAgent),
		}
		if r.IsHuman() {
			seat.Kind = string(org.KindHuman)
		}
		if home := homes[r]; home != nil {
			seat.UnitPath = x.units[home].String()
			// Written at the root, sitting in a unit: only a unit
			// reference moves a seat.
			seat.PlacedByRef = len(x.seats[r]) > 0 && x.seats[r][0] == "roles"
		}
		if manager := o.Manager(r); manager != nil {
			seat.Manager = manager.Handle()
		}
		for m := range o.AllRoles() {
			if slices.Contains(m.Manages, r.Name) {
				seat.Managers = append(seat.Managers, m.Handle())
			}
		}
		for _, report := range o.Reports(r) {
			seat.Reports = append(seat.Reports, report.Handle())
		}
		for _, name := range r.AutoManaged {
			if report := o.Role(name); report != nil {
				seat.AutoReports = append(seat.AutoReports, report.Handle())
			}
		}
		for _, u := range o.UnitChainFor(r) {
			seat.OnboardingChain = append(seat.OnboardingChain, u.Name)
		}
		out.Seats = append(out.Seats, seat)
	}

	for u := range o.AllUnits() {
		unit := DerivedUnit{
			Path:             x.units[u].String(),
			Name:             u.Name,
			Type:             string(u.Type),
			Channel:          u.Channel,
			ChannelInherited: u.DeclaredChannel == "" && u.Channel != "",
		}
		if lead := o.EffectiveLead(u); lead != nil {
			unit.Lead = lead.Handle()
			unit.LeadInherited = u.DeclaredLead == ""
		}
		for _, r := range u.Roles {
			unit.Seats = append(unit.Seats, r.Handle())
		}
		out.Units = append(out.Units, unit)
	}
	return out
}

// WithoutPaths is the hierarchy with every authored path removed: the form
// for a reader that is given no document to point into, such as the
// anonymous organization projection. Membership is still there, in each
// unit's seats.
func (d Derived) WithoutPaths() Derived {
	// Copied rather than written through, so the caller's value keeps its
	// paths; a nil list stays nil, which is what an empty one marshals as.
	var out Derived
	if d.Seats != nil {
		out.Seats = append([]DerivedSeat(nil), d.Seats...)
	}
	if d.Units != nil {
		out.Units = append([]DerivedUnit(nil), d.Units...)
	}
	for i := range out.Seats {
		out.Seats[i].Path, out.Seats[i].UnitPath = "", ""
	}
	for i := range out.Units {
		out.Units[i].Path = ""
	}
	return out
}
