// Naming a schedule's scope for a person.

package queries

import (
	"github.com/crewlet/crewlet/internal/events/types"
)

// scopeNames resolves a ledger row's scope back to what a person calls it.
//
// A FIRE IS KEYED ON AN IDENTITY (see [schedule.Entry.ScopeID]) — a seat's
// agent id, a unit's origin key — because a schedule's history and its
// at-most-once dedupe must survive a rename. None of that is legible: a role
// row's id is a uuid, and a table of them tells an operator nothing.
//
// So the name is resolved AT READ TIME from the company this node is running,
// rather than stored beside the row. Two reasons, and the second is the one
// that decides it: the ledger has no column for it and adding one would be a
// migration for a value nothing keys on, and a stored name would be the name
// the scope had WHEN IT FIRED — so a renamed seat's history would show two
// different people, neither of whom is in the company now.
//
// A row this node cannot name falls back to its id, which is the honest
// answer for the two ways that happens: a seat the active revision no longer
// has, and a node with no company of its own.
type scopeNames struct {
	// seats maps a role scope's id to the seat's handle. Nil is not an
	// error — it is a node with nothing to resolve against.
	seats map[string]string
}

// scopeNames reads the running company once, for a whole answer.
func (s Sources) scopeNames() scopeNames {
	if s.Company == nil {
		return scopeNames{}
	}
	_, roster := s.Company()
	if roster == nil {
		return scopeNames{}
	}
	out := scopeNames{seats: map[string]string{}}
	for role := range roster.AllRoles() {
		if id, ok := roster.AgentIDFor(role); ok {
			out.seats[id.String()] = role.Handle()
		}
	}
	return out
}

// of names one scope.
//
// A UNIT SCOPE NEEDS NO LOOKUP: its identity is its origin key, which is
// already an address somebody typed. Only a role's is derived, and only a
// role's is unreadable.
func (n scopeNames) of(scope types.ScheduleScope, id string) string {
	if scope != types.ScheduleScopeRole {
		return id
	}
	if handle, named := n.seats[id]; named {
		return handle
	}
	return id
}
