package org

import (
	"maps"
	"slices"
	"strings"
	"sync"
)

// Who owns an integration scope.
//
// The lead map makes a work item that names nobody land somewhere: the worst
// tracker failure is a ticket filed into a project nobody watches, silently.
//
// A project identifier, project key and space key are one org-model question:
// which seat owns the place the work was filed. The walk that answers it
// (units before root seats, inherited leads, first declaration winning) is
// about the ORG, so it lives here, not in three integrations that could each
// answer differently.

// Scope is one integration's per-unit and per-seat identity, and how to
// read it.
type Scope struct {
	// OfUnit reads the scope a unit declares.
	OfUnit func(*Unit) string
	// OfRole reads the scope a seat declares.
	OfRole func(*Role) string
}

// LeadReport is what a walk found besides the mapping.
//
// RETURNED rather than logged, so the org model owes nothing to a logger and
// each integration reports the finding in its own vocabulary, so an operator
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
// Two sources, in order: a UNIT declaring the scope maps it to its effective
// lead (inherited included); a ROOT SEAT declaring it maps it to itself, having
// no unit.
//
// A seat INSIDE a unit declares where IT writes, not who owns the scope;
// letting a member override the unit's lead would hand the fallback to
// whichever teammate was walked first.
//
// Scopes are keyed UPPER, as every integration renders them.
func (o *Organization) LeadsBy(s Scope) (map[string]string, LeadReport) {
	var report LeadReport
	if o == nil || s.OfUnit == nil || s.OfRole == nil {
		return nil, report
	}
	byScope := map[string][]leadSource{}

	// One index for every scoped unit's lead, built at the first, for the
	// reason [Organization.autoManageByLead] gives.
	chart := sync.OnceValue(o.Index)
	for u := range o.AllUnits() {
		scope := NormalizeScope(s.OfUnit(u))
		if scope == "" {
			continue
		}
		lead := chart().EffectiveLead(u)
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
