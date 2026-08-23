package plane

import (
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Who owns a project.
//
// The lead map is what makes a work item that names nobody land somewhere.
// A tracker's worst failure is not a misroute — it is a ticket filed into a
// project nobody watches, which produces no error anywhere and is discovered
// weeks later when somebody asks why it was never picked up.

// leadSource is one declaration of a project's owner, kept with the place it
// was declared so an ambiguity can be reported in terms an operator can find
// in their config file.
type leadSource struct{ where, handle string }

// LeadsFrom maps each Plane project identifier to the handle that owns it.
//
// Two sources, in this order:
//
//  1. A UNIT that declares the project maps it to the unit's effective lead,
//     which resolves through an inherited one — a lead set on a division owns
//     a team three levels down that named nobody itself.
//  2. A ROOT SEAT that declares the project maps it to that seat directly. A
//     root seat belongs to no unit, so there is no lead to resolve: the seat
//     IS the owner.
//
// Identifiers are keyed UPPER, because Plane accepts them in any case and an
// operator writes one however it was shown to them.
func LeadsFrom(o *org.Organization) map[string]string {
	if o == nil {
		return nil
	}
	byProject := map[string][]leadSource{}

	for u := range o.AllUnits() {
		project := normalizeIdentifier(u.PlaneProject)
		if project == "" {
			continue
		}
		lead := o.EffectiveLead(u)
		if lead == nil {
			// A unit that owns a project and has nobody leading it.
			// LOUD, because the consequence is invisible: every
			// unassigned item in that project routes to nobody, and
			// that looks exactly like an item nobody filed.
			log.Warn("plane_project_has_no_lead", "unit", u.Name, "project", project)
			continue
		}
		byProject[project] = append(byProject[project],
			leadSource{where: "unit:" + u.Name, handle: lead.Handle()})
	}
	// ROOT SEATS ONLY. A seat inside a unit that declares a project is
	// declaring where IT writes, not who owns the project — the unit's
	// lead already answers that, and letting a member override it would
	// hand the fallback to whichever teammate happened to be walked first.
	for _, r := range o.Roles {
		project := normalizeIdentifier(r.PlaneProject)
		if project == "" {
			continue
		}
		byProject[project] = append(byProject[project],
			leadSource{where: "role:" + r.Name, handle: r.Handle()})
	}

	leads := make(map[string]string, len(byProject))
	// SORTED for the WARNINGS below, not for the result: each project's
	// owner is decided independently, so this loop's order cannot change
	// what comes back. What it buys is that two applies of one config log
	// their conflicts in the same order and can be diffed. The determinism
	// that decides the OWNER is the walk order above — units before root
	// seats, parents before children — which is the org's own iteration
	// order and is pinned by its tests as well as these.
	for _, project := range slices.Sorted(maps.Keys(byProject)) {
		sources := byProject[project]
		// FIRST DECLARATION WINS. The warning fires only on genuine
		// ambiguity — several units sharing a project under ONE lead is
		// the documented shared-project pattern, and warning about it
		// would train every operator using it to ignore the channel that
		// also carries the real conflicts.
		leads[project] = sources[0].handle
		if candidates := distinctHandles(sources); len(candidates) > 1 {
			log.Warn("plane_project_lead_ambiguous", "project", project,
				"declared_by", places(sources), "chose", sources[0].handle,
				"candidates", candidates)
		}
	}
	return leads
}

func normalizeIdentifier(s string) string {
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
