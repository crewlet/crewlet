package confluence

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/org"
)

// SpacesOf is every space the company document names: each unit's and each
// seat's `space`, in key order. It is the reach a seat's Confluence scope is
// held within, the counterpart of jira.ProjectsOf.
func SpacesOf(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	seen := map[string]bool{}
	for u := range o.AllUnits() {
		if key := org.NormalizeScope(u.Space); key != "" {
			seen[key] = true
		}
	}
	for seat := range o.AllRoles() {
		if key := org.NormalizeScope(seat.Space); key != "" {
			seen[key] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
