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
	//
	// A FACT ABOUT A DOCUMENT, and therefore ABSENT rather than false for
	// a company that has none. It says where a seat was WRITTEN versus
	// where it ended up, and a company composed from the org chart's own
	// rows was written nowhere: each row states its unit directly, so
	// nothing was moved by a reference at all.
	//
	// A POINTER for exactly that reason. "This seat was not moved" and
	// "there is no document to have moved it in" are different answers,
	// and a plain bool collapses the second into the first — which is the
	// shape that lets a reader act on a claim nothing made. See
	// [DeriveFrom], which sets it on nothing, and [Derived.WithoutPaths].
	PlacedByRef *bool `json:"placed_by_ref,omitempty"`
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

	// ID is the unit's KEY — its `id`, or its name where it declares none
	// — and it is what every reference in the document resolves: a
	// `manages:` entry, a seat's `unit:`, and the unit column on every
	// stored row.
	//
	// IT IS ON THE PROJECTION BECAUSE NOTHING ELSE IN IT CARRIES ONE. A
	// client reading a `manages` entry sees a key, and with only display
	// names beside it there is nothing in the same response to resolve
	// that key against — so it either matched on the name, which is a
	// different value the moment a unit declares an id, or rendered the
	// raw string and called the reference broken.
	//
	// SPELLED `id` ON THE WIRE, as the authored field and the public
	// projection both spell it: this hierarchy travels in the same
	// response as those, and one value under two names in one payload is
	// a client picking whichever it happened to read first.
	ID string `json:"id"`

	// Name is what a person reads. It is display only: nothing in the
	// document resolves a unit by it.
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
	return deriveFrom(o, x)
}

// DeriveFrom is [Derive] over an organization that is already built — the
// company VIEW a node composes from its chart rows.
//
// # Why the derivation has two entry points
//
// [Derive] answers about a DOCUMENT and can therefore say where each seat was
// written; that is what `crewlet validate` and the guarded config reads use
// the paths for. A running company's seats are rows on the chart's own log and
// were written at no path at all, so this is the same derivation with the one
// thing it cannot honestly answer left out — exactly what [Derived.WithoutPaths]
// produces, and it is the form every live reader already asked for.
//
// It exists because the projection the dashboard renders went through the
// document, and a stored revision carries no seats: every open screen was
// shown a company with no org chart in it, derived cleanly from bytes that
// were correct.
func DeriveFrom(o *org.Organization) Derived {
	if o == nil {
		return Derived{}
	}
	return deriveFrom(o, nil)
}

// deriveFrom is the one derivation. `paths` is the document index, or nil for
// an organization nobody authored a path for.
func deriveFrom(o *org.Organization, paths *identityIndex) Derived {
	var out Derived
	at := func(seat *org.Role) Path {
		if paths == nil {
			return nil
		}
		return paths.seats[seat]
	}
	unitAt := func(u *org.Unit) Path {
		if paths == nil {
			return nil
		}
		return paths.units[u]
	}

	homes := make(map[*org.Role]*org.Unit)
	for u := range o.AllUnits() {
		for _, r := range u.Roles {
			homes[r] = u
		}
	}

	for r := range o.AllRoles() {
		seat := DerivedSeat{
			Path:   at(r).String(),
			Handle: r.Handle(),
			Name:   r.Name,
			Kind:   string(org.KindAgent),
		}
		if r.IsHuman() {
			seat.Kind = string(org.KindHuman)
		}
		home := homes[r]
		if home != nil {
			seat.UnitPath = unitAt(home).String()
		}
		if paths != nil {
			// Written at the root and sitting in a unit: only a unit
			// reference moves a seat. STATED FOR EVERY SEAT, including
			// a genuine "no" for one that was not moved — and only
			// where there is a document to have written it in, because
			// absent is the answer for a company that has none.
			moved := home != nil && len(at(r)) > 0 && at(r)[0] == "roles"
			seat.PlacedByRef = &moved
		}
		if manager := o.Manager(r); manager != nil {
			seat.Manager = manager.Handle()
		}
		for m := range o.AllRoles() {
			// BY HANDLE, which is what a normalized manages list holds: an
			// authored entry names a seat by handle, and a unit key has been
			// expanded into the handles in it.
			if slices.Contains(m.Manages, r.Handle()) {
				seat.Managers = append(seat.Managers, m.Handle())
			}
		}
		for _, report := range o.Reports(r) {
			seat.Reports = append(seat.Reports, report.Handle())
		}
		for _, handle := range r.AutoManaged {
			if report := o.Role(handle); report != nil {
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
			Path:             unitAt(u).String(),
			ID:               u.Key(),
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
