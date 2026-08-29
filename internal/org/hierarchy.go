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
	var out []*Role
	seen := map[string]struct{}{r.Name: {}}
	current := r
	for {
		manager := o.Manager(current)
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

// UnitFor returns the unit that holds r as a DIRECT member, or nil for a
// root-level seat.
func (o *Organization) UnitFor(r *Role) *Unit {
	for u := range o.AllUnits() {
		if u.Role(r.Name) != nil {
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
// has MOVED and must onboard again.
func (o *Organization) UnitChainFor(r *Role) []*Unit {
	for _, u := range o.Units {
		if chain := buildUnitChain(u, r.Name); chain != nil {
			slices.Reverse(chain)
			return chain
		}
	}
	return nil
}

// buildUnitChain collects the units from the one holding roleName back up
// to u, innermost first. Reversing once at the end beats prepending at
// every level.
func buildUnitChain(u *Unit, roleName string) []*Unit {
	if u.Role(roleName) != nil {
		return []*Unit{u}
	}
	for _, c := range u.Children {
		if chain := buildUnitChain(c, roleName); chain != nil {
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
