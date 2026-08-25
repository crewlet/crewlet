package org

import (
	"maps"
	"slices"
	"strings"
)

// Who owns a vendor scope.
//
// The lead map is what makes a work item that names nobody land somewhere. A
// tracker's worst failure is not a misroute — it is a ticket filed into a
// project nobody watches, which produces no error anywhere and is discovered
// weeks later when somebody asks why it was never picked up.
//
// # Why this is here rather than in each vendor
//
// A project identifier, a project key and a space key are different words
// for one org-model question: which seat owns the place this work was filed.
// The walk that answers it — units before root seats, an inherited lead
// resolving through the hierarchy, first declaration winning — is a rule
// about the ORG, not about any vendor, and three copies of it is three
// chances for one of them to answer differently from the others while each
// stays self-consistent.

// Scope is one vendor's per-unit and per-seat identity, and how to read it.
type Scope struct {
	// OfUnit reads the scope a unit declares.
	OfUnit func(*Unit) string
	// OfRole reads the scope a seat declares.
	OfRole func(*Role) string
}

// LeadReport is what a walk found besides the mapping.
//
// RETURNED rather than logged, so the org model owes nothing to a logger and
// each vendor reports the finding in its own vocabulary — an operator
// grepping for "jira" finds the Jira warning.
type LeadReport struct {
	// Unled names the units that declare a scope and have nobody leading
	// them. LOUD for the caller, because the consequence is invisible:
	// every unassigned item in that scope routes to nobody, which looks
	// exactly like an item nobody filed.
	Unled []UnledUnit

	// Ambiguous names the scopes more than one lead was declared for.
	Ambiguous []ScopeConflict
}

// UnledUnit is a unit that owns a scope and has no lead.
type UnledUnit struct{ Unit, Scope string }

// ScopeConflict is a scope several distinct leads were declared for.
type ScopeConflict struct {
	Scope string
	// DeclaredBy names each declaration's place — "unit:Engineering",
	// "role:CTO" — so an operator can find them in their config file.
	DeclaredBy []string
	// Chose is the handle that won.
	Chose string
	// Candidates are the distinct handles that were declared, in walk
	// order.
	Candidates []string
}

// leadSource is one declaration of a scope's owner, kept with the place it
// was declared.
type leadSource struct{ where, handle string }

// LeadsBy maps each declared scope to the handle that owns it.
//
// Two sources, in this order:
//
//  1. A UNIT that declares the scope maps it to the unit's effective lead,
//     which resolves through an inherited one — a lead set on a division
//     owns a team three levels down that named nobody itself.
//  2. A ROOT SEAT that declares the scope maps it to that seat directly. A
//     root seat belongs to no unit, so there is no lead to resolve: the seat
//     IS the owner.
//
// A seat INSIDE a unit that declares a scope is declaring where IT writes,
// not who owns the scope — the unit's lead already answers that, and letting
// a member override it would hand the fallback to whichever teammate
// happened to be walked first.
//
// Scopes are keyed UPPER, because every vendor this serves renders them
// upper and an operator writes one however it was shown to them.
func (o *Organization) LeadsBy(s Scope) (map[string]string, LeadReport) {
	var report LeadReport
	if o == nil || s.OfUnit == nil || s.OfRole == nil {
		return nil, report
	}
	byScope := map[string][]leadSource{}

	for u := range o.AllUnits() {
		scope := NormalizeScope(s.OfUnit(u))
		if scope == "" {
			continue
		}
		lead := o.EffectiveLead(u)
		if lead == nil {
			report.Unled = append(report.Unled, UnledUnit{Unit: u.Name, Scope: scope})
			continue
		}
		byScope[scope] = append(byScope[scope],
			leadSource{where: "unit:" + u.Name, handle: lead.Handle()})
	}
	for _, r := range o.Roles {
		scope := NormalizeScope(s.OfRole(r))
		if scope == "" {
			continue
		}
		byScope[scope] = append(byScope[scope],
			leadSource{where: "role:" + r.Name, handle: r.Handle()})
	}

	leads := make(map[string]string, len(byScope))
	// SORTED for the REPORT, not for the result: each scope's owner is
	// decided independently, so this loop's order cannot change what comes
	// back. What it buys is that two applies of one config report their
	// conflicts in the same order and can be diffed. The determinism that
	// decides the OWNER is the walk order above — units before root seats,
	// parents before children — which is the org's own iteration order.
	for _, scope := range slices.Sorted(maps.Keys(byScope)) {
		sources := byScope[scope]
		// FIRST DECLARATION WINS. A conflict is reported only on genuine
		// ambiguity — several units sharing a scope under ONE lead is the
		// documented shared-project pattern, and warning about it would
		// train every operator using it to ignore the channel that also
		// carries the real conflicts.
		leads[scope] = sources[0].handle
		if candidates := distinctHandles(sources); len(candidates) > 1 {
			report.Ambiguous = append(report.Ambiguous, ScopeConflict{
				Scope: scope, DeclaredBy: places(sources),
				Chose: sources[0].handle, Candidates: candidates,
			})
		}
	}
	return leads, report
}

// NormalizeScope is the one spelling a scope is compared under.
func NormalizeScope(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

func distinctHandles(sources []leadSource) []string {
	seen := make(map[string]bool, len(sources))
	var out []string
	for _, s := range sources {
		if !seen[s.handle] {
			seen[s.handle] = true
			out = append(out, s.handle)
		}
	}
	return out
}

func places(sources []leadSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.where)
	}
	return out
}
