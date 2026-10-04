// Naming a schedule's scope for a person.

package queries

import (
	"github.com/crewlet/crewlet/internal/events/types"
)

// scopeNames resolves a ledger row's scope back to what a person calls it.
//
// A FIRE IS KEYED ON AN IDENTITY (see [schedule.Entry.ScopeID]) — a seat's
// agent id, a unit's key. A unit's key is legible as it stands; a role row's
// id is a uuid, so the seat's handle is resolved AT READ TIME from the company
// this node is running: the ledger has no column for it, and adding one would
// be a migration for a value nothing keys on.
//
// A row this node cannot name falls back to its id, which is the honest
// answer for the two ways that happens: a seat the org no longer has, and a
// node with no company of its own.
type scopeNames struct {
	// seats maps a role scope's id to the seat's handle.
	seats map[string]string
}

// scopeNames reads the running company once, for a whole answer.
func (s Sources) scopeNames() scopeNames {
	organization := s.organization()
	if organization == nil {
		return scopeNames{}
	}
	out := scopeNames{seats: map[string]string{}}
	for role := range organization.AllRoles() {
		if id, ok := organization.AgentIDFor(role); ok {
			out.seats[id.String()] = role.Handle()
		}
	}
	return out
}

// of names one scope: a role's by its seat's handle, and a unit's by its key,
// which is its id.
func (n scopeNames) of(scope types.ScheduleScope, id string) string {
	if scope == types.ScheduleScopeRole {
		if handle, named := n.seats[id]; named {
			return handle
		}
	}
	return id
}
