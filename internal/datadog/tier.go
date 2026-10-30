package datadog

import "strings"

// What an agent's Datadog account may do, and where the answer comes from.
//
// A ROLE, NOT A TIER, which is Datadog's own model: permission is granted by
// putting an account in a role, and an organization's roles are whatever its
// admins have made. So there is no closed set to offer, and the value on the
// config block is a role NAME that this engine passes through to the account
// it creates.
//
// The three Datadog ships with are named here anyway, because they are what
// almost every organization uses: the role picker groups them first, and the
// administration class is one of them.

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
