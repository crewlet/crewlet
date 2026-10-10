package org

import "slices"

// The hierarchy IS the execution graph. Who a seat reports to decides where
// a handoff goes, what a lead's roster contains, and where an escalation
// terminates — so these walks are read on prompt-building and routing
// paths, not just by the dashboard.
//
// They are methods on the org rather than free functions over (role, org)
// because every one of them needs the whole company to answer: management
// is expressed on the MANAGING seat, so finding a seat's manager is a
// search, not a field read.

// Manager returns the seat that lists r in its manages, or nil for a
// top-level seat.
//
// Management is stored on the manager, which is what lets one seat manage
// across units — a root-level CEO managing a VP three levels down needs no
// entry anywhere near that VP.
func (o *Organization) Manager(r *Role) *Role {
	for candidate := range o.AllRoles() {
		if slices.Contains(candidate.Manages, r.Name) {
			return candidate
		}
	}
	return nil
}

// Reports returns r's direct reports, in the order r lists them. A name
// that resolves to no seat is skipped — a chart can legitimately be
// half-wired mid-bootstrap.
func (o *Organization) Reports(r *Role) []*Role {
	var out []*Role
	for _, name := range r.Manages {
		if found := o.Role(name); found != nil {
			out = append(out, found)
		}
	}
	return out
}

// Ancestors returns the management chain above r, direct manager first and
// the topmost manager last.
//
// The walk stops on a repeat rather than assuming the chart is a tree: a
// config can express a cycle, and a prompt builder that looped forever on
// one would take the whole turn with it. A cycle simply ends the chain.
func (o *Organization) Ancestors(r *Role) []*Role {
	return chainAbove(r, o.Manager)
}

// chainAbove is the management chain above r as managerOf answers each step,
// under the one stopping rule [Organization.Ancestors] documents. Shared with
// [Organization.LeadsInLine] so that a seat's line and a seat's chain are one
// walk read in two directions rather than two walks that agree today.
func chainAbove(r *Role, managerOf func(*Role) *Role) []*Role {
	var out []*Role
	seen := map[string]struct{}{r.Name: {}}
	current := r
	for {
		manager := managerOf(current)
		if manager == nil {
			return out
		}
		if _, repeat := seen[manager.Name]; repeat {
			return out
		}
		out = append(out, manager)
		seen[manager.Name] = struct{}{}
		current = manager
	}
}

// LeadsInLine returns the handles of every seat in lead's LINE — every seat
// whose management chain ([Organization.Ancestors]) passes through lead: its
// direct reports, theirs, and so on to the bottom of the chart — in the
// engine's own seat order ([Organization.AllRoles]). Lead is never in its own
// line, and a nil lead has none.
//
// It is the ONE answer to "is this person somebody in that one's line", which
// is the authority a lead holds over a report's queue (`set_priorities`). It
// is defined once, here, because the copies of it disagreed: the dashboard
// walked every manager a seat has while the engine walked the primary chain,
// so a screen offered a lead a reorder the engine then refused.
//
// THE PRIMARY CHAIN, NOT EVERY MANAGER. A member reached both by an outside
// seat managing its unit by name and by its own lead's auto-management has two
// managers (see [Organization.autoManageByLead]), and it is in the line of the
// one [Organization.Manager] names and of everybody above that one — not in
// the other's. That is the line the org chart draws and the chain an identity
// prompt and an escalation follow; a lead relation over every manager would be
// a second chart, one the company never sees, granting writes nobody can
// trace to a line on it.
//
// THROUGH manages AND UNIT LEADERSHIP ALIKE, because after
// [Organization.Normalize] they are one list: a unit named in manages stands
// for its seats, and a unit's lead manages the members nobody in the unit
// already does. So a lead who wrote no manages at all still has the team they
// lead in their line.
//
// ANY DEPTH, not only direct reports: a founder leads everybody, and an
// authority that stopped one level down would make "somebody in their line"
// mean "somebody directly under them".
//
// A CYCLE ENDS where [Organization.Ancestors] ends it, at the first repeat, so
// every seat on a management loop is in every other's line and never in its
// own, and the walk terminates.
func (o *Organization) LeadsInLine(lead *Role) []string {
	if lead == nil {
		return nil
	}
	// [Organization.Manager] FOR EVERY SEAT AT ONCE: the first seat in
	// AllRoles order whose manages lists a name. Asked per step, the walk
	// below would rescan the company for every manager of every seat.
	primary := make(map[string]*Role)
	for candidate := range o.AllRoles() {
		for _, name := range candidate.Manages {
			if _, taken := primary[name]; !taken {
				primary[name] = candidate
			}
		}
	}
	managerOf := func(r *Role) *Role { return primary[r.Name] }

	// No seat is skipped as the lead itself: the chain above a seat never
	// holds the seat, which is the stopping rule rather than a special case.
	var out []string
	for r := range o.AllRoles() {
		if slices.Contains(chainAbove(r, managerOf), lead) {
			out = append(out, r.Handle())
		}
	}
	return out
}

// UnitFor returns the unit that holds r as a DIRECT member, or nil for a
// root-level seat.
//
// BY THE SEAT, NOT BY ITS NAME. A seat is used by pointer everywhere (see
// [Role]), and an applied revision can still hold two seats of one name (an
// admission rule an apply does not enforce): looked up by name, the second
// seat would be answered with the first one's unit, so its prompt would name
// a team it is not in.
func (o *Organization) UnitFor(r *Role) *Unit {
	for u := range o.AllUnits() {
		if slices.Contains(u.Roles, r) {
			return u
		}
	}
	return nil
}

// UnitChainFor returns the units from the outermost down to the one holding
// r — [division, department, team] — or nothing for a root-level seat.
//
// The chain is what onboarding walks (a seat reads the Onboarding page of
// every scope above it) and what the engine hashes to decide that a seat
// has MOVED and must onboard again. Found by the seat itself, as
// [Organization.UnitFor] is: by name, the second of two seats sharing one was
// onboarded for the first one's chain, and moving it re-onboarded nothing.
func (o *Organization) UnitChainFor(r *Role) []*Unit {
	for _, u := range o.Units {
		if chain := buildUnitChain(u, r); chain != nil {
			slices.Reverse(chain)
			return chain
		}
	}
	return nil
}

// buildUnitChain collects the units from the one holding r back up to u,
// innermost first. Reversing once at the end beats prepending at every level.
func buildUnitChain(u *Unit, r *Role) []*Unit {
	if slices.Contains(u.Roles, r) {
		return []*Unit{u}
	}
	for _, c := range u.Children {
		if chain := buildUnitChain(c, r); chain != nil {
			return append(chain, u)
		}
	}
	return nil
}

// EffectiveLead returns the seat leading u, including a lead inherited from
// an ancestor.
//
// [Unit.LeadRole] answers only for a lead inside u's own subtree; an
// inherited one lives outside it, so this falls back to an org-wide lookup.
// Nil means the unit has no lead, or names one that does not exist yet.
func (o *Organization) EffectiveLead(u *Unit) *Role {
	if u.Lead == "" {
		return nil
	}
	if lead := u.LeadRole(); lead != nil {
		return lead
	}
	return o.Role(u.Lead)
}

// IsUnitLead reports whether r leads any unit in the company.
//
// A seat can lead a unit it does not sit in — a department lead whose own
// seat is in one of its teams is the usual shape — so this asks about the
// units, not about where r lives.
func (o *Organization) IsUnitLead(r *Role) bool { return o.LeadDepth(r) >= 0 }

// LeadDepth returns how deep the unit r leads sits, or -1 when r leads
// none. Depth 0 is a top-level unit, 1 its child, and so on: a LOWER depth
// is higher authority, which is what makes it comparable between seats.
//
// When r leads a unit and one of its descendants — a division lead who also
// leads a team inside it — the shallower one wins, because the authority
// that matters is the widest one. Across separate top-level trees the first
// match wins; a seat leading units in two unrelated divisions has no
// meaningful single depth to report.
func (o *Organization) LeadDepth(r *Role) int {
	for _, u := range o.Units {
		if depth := leadDepth(u, r.Name, 0); depth >= 0 {
			return depth
		}
	}
	return -1
}

func leadDepth(u *Unit, roleName string, depth int) int {
	if u.Lead == roleName {
		return depth
	}
	for _, c := range u.Children {
		if found := leadDepth(c, roleName, depth+1); found >= 0 {
			return found
		}
	}
	return -1
}
