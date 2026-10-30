package datadog

import "strings"

// What an agent's Datadog account may do, in Datadog's own terms.
//
// A ROLE, NOT A TIER, which is Datadog's own model: permission is granted by
// putting an account in a role, and an organization's roles are whatever its
// admins have made. So there is no closed set: each seat's document stores the
// id of a role the organization has (D7), the Agents tab offers every role
// Datadog lists under Datadog's exact name, and nothing here grades a role
// into a cross-vendor word.

// The roles Datadog creates in every organization.
const (
	RoleReadOnly = "Datadog Read Only Role"
	RoleStandard = "Datadog Standard Role"
	RoleAdmin    = "Datadog Admin Role"
)

// ManagedRole reports whether a role is one Datadog creates in every
// organization, as opposed to a custom role an admin defined.
//
// BY NAME, because that is all Datadog's role listing says about it: the
// three managed roles carry the same names in every organization, and the
// picker groups them first under exactly those names (D7).
func ManagedRole(name string) bool {
	switch strings.TrimSpace(name) {
	case RoleReadOnly, RoleStandard, RoleAdmin:
		return true
	default:
		return false
	}
}

// AdministrationRole reports whether holding a role is the administration
// class (D10): Datadog Admin Role, which manages users, keys and billing.
//
// A raise to it carries a warning in the review and needs a fresh step-up.
// A custom role is not classed here: what it grants is set at Datadog, and
// this engine has no basis for claiming it administers anything.
func AdministrationRole(name string) bool { return strings.TrimSpace(name) == RoleAdmin }
