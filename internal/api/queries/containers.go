package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
)

// WHO OWNS A KNOWLEDGE CONTAINER, served beside it.
//
// A unit's `space:` names the container its seats write pages into, and its
// `project:` the tracker project it files work in. Both are kept out of the
// ANONYMOUS org projection — a container key names where a company writes, a
// map of what to look at — and a screen that wanted "who files here" used to
// read the whole company document for it, which is an admin's (ADR-0031): a
// member's knowledge page could not say which team a space belongs to. So the
// `containers` answer, a member's, carries each container's owners the way
// `work_projects` carries each project's unit: resolved against the RUNNING
// chart through the same seam ([engine.ChartUnits]), so a space and a project
// name a team by one name and a lead by one handle.

// ContainerRow is one knowledge container as `containers` answers it.
type ContainerRow struct {
	pages.ContainerListing

	// Owners is every unit whose `space:` names this container, in the
	// chart's own order, each with the tracker project it files work in —
	// ALWAYS a list, because "no unit names this space" is a fact about the
	// company a screen states, and an absent key would read as unknown.
	Owners []ContainerOwner `json:"owners"`
}

// ContainerOwner is one unit that writes into a container.
type ContainerOwner struct {
	// Unit is the unit as the chart answers it — its durable key and the
	// name it goes by now — exactly as a project row's `unit` is.
	Unit tracker.UnitRef `json:"unit"`

	// Lead is who leads it, the EFFECTIVE lead — inherited from an ancestor
	// where the unit names none — exactly as a project row's `lead` is.
	Lead tracker.LeadRef `json:"lead"`

	// Project is the tracker project the unit files work in, in the one
	// spelling a key is compared under ([org.NormalizeScope]); absent for a
	// unit that names none.
	Project string `json:"project,omitempty"`
}

func (s Sources) containers(ctx context.Context, p Params) (any, error) {
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	list, err := s.Pages.Containers(ctx, fresh)
	if err != nil {
		return nil, err
	}
	owners := s.containerOwners()
	out := make([]ContainerRow, len(list))
	for i, c := range list {
		row := owners[org.NormalizeScope(c.Key)]
		if row == nil {
			row = []ContainerOwner{}
		}
		out[i] = ContainerRow{ContainerListing: c, Owners: row}
	}
	return map[string]any{"containers": out}, nil
}

// containerOwners is every unit of the running chart that names a container,
// keyed by the container in the spelling every container key is compared
// under. Empty where no chart is loaded: an unresolved chart owns nothing it
// could name.
func (s Sources) containerOwners() map[string][]ContainerOwner {
	organization := s.organization()
	out := map[string][]ContainerOwner{}
	if organization == nil {
		return out
	}
	// THE SAME CHART the walk reads: every read of it builds a new one, and
	// a unit resolved against another epoch's could miss.
	units := engine.ChartUnits(organization)
	for unit := range organization.AllUnits() {
		space := org.NormalizeScope(unit.Space)
		if space == "" {
			continue
		}
		// THROUGH THE CHART SEAM, never the struct's own fields: the seam
		// is what decides a unit's key and its effective lead, and a
		// second reading here would be the one place a team is named
		// differently from its projects.
		resolved, found := units.ResolveUnit(unit.Key())
		out[space] = append(out[space], ContainerOwner{
			Unit:    tracker.UnitRef{Key: resolved.Key, Name: resolved.Name, Resolved: found},
			Lead:    resolved.Lead,
			Project: org.NormalizeScope(unit.Project),
		})
	}
	return out
}
