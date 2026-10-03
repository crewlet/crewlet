// Naming a schedule's scope for a person.

package queries

import (
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
)

// scopeNames resolves a ledger row's scope back to what a person calls it.
//
// A FIRE IS KEYED ON AN IDENTITY (see [schedule.Entry.ScopeID]) — a seat's
// agent id, a unit's origin key — because a schedule's history and its
// at-most-once dedupe must survive a rename. Neither is legible as it stands:
// a role row's id is a uuid, and a unit's origin key is the key it was created
// under, which a rename has since moved on from.
//
// So the name is resolved AT READ TIME from the company this node is running,
// rather than stored beside the row. Two reasons, and the second is the one
// that decides it: the ledger has no column for it and adding one would be a
// migration for a value nothing keys on, and a stored name would be the name
// the scope had WHEN IT FIRED — so a renamed seat's history would show two
// different people, neither of whom is in the company now.
//
// A row this node cannot name falls back to its id, which is the honest
// answer for the two ways that happens: a scope the chart no longer has, and
// a node with no company of its own.
type scopeNames struct {
	// organization is the ONE chart reading every row of an answer is named
	// from. Nil is not an error — it is a node with nothing to resolve
	// against.
	organization *org.Organization

	// seats maps a role scope's id to the seat's handle now.
	seats map[string]string
}

// scopeNames reads the running company once, for a whole answer.
func (s Sources) scopeNames() scopeNames {
	organization := s.organization()
	if organization == nil {
		return scopeNames{}
	}
	out := scopeNames{organization: organization, seats: map[string]string{}}
	for role := range organization.AllRoles() {
		if id, ok := organization.AgentIDFor(role); ok {
			out.seats[id.String()] = role.Handle()
		}
	}
	return out
}

// of names one scope: a role's by the handle its seat answers to now, and a
// unit's by the key it answers to now — through [org.Organization.Unit],
// which resolves the key a unit was created under as well as its live one.
func (n scopeNames) of(scope types.ScheduleScope, id string) string {
	switch scope {
	case types.ScheduleScopeRole:
		if handle, named := n.seats[id]; named {
			return handle
		}
	case types.ScheduleScopeUnit:
		if n.organization != nil {
			if unit := n.organization.Unit(id); unit != nil {
				return unit.Key()
			}
		}
	}
	return id
}
