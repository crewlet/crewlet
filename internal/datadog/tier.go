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
// almost every organization uses and because a name is not a scope: an
// operator reading "Datadog Standard Role" against an agent learns less than
// one reading "Standard", and the roster has one line to say it in.

// The roles Datadog creates in every organization.
const (
	RoleReadOnly = "Datadog Read Only Role"
	RoleStandard = "Datadog Standard Role"
	RoleAdmin    = "Datadog Admin Role"
)

// TierOf maps a role onto the access vocabulary the rest of the engine
// speaks, so an agent's Datadog access reads beside its GitHub access rather
// than in a second grammar.
//
// EMPTY FOR A ROLE THIS ENGINE DOES NOT KNOW, which is a custom one an
// organization made, and the honest answer: its name says what it is and this
// engine has no basis for claiming how much it grants.
func TierOf(role string) string {
	switch strings.TrimSpace(role) {
	case RoleReadOnly:
		return "read_only"
	case RoleStandard:
		return "review"
	case RoleAdmin:
		return "full_access"
	default:
		return ""
	}
}

// RoleLabel is the role written for a person.
//
// Datadog prefixes its own with the product name and suffixes them with the
// word Role, which is noise on a row that has already said which app it is
// about. A custom role is passed through whole: it is what an admin called
// it, and shortening somebody's own name is how a reader stops recognising
// it.
func RoleLabel(role string) string {
	switch strings.TrimSpace(role) {
	case RoleReadOnly:
		return "Read-only"
	case RoleStandard:
		return "Standard"
	case RoleAdmin:
		return "Admin"
	default:
		return strings.TrimSpace(role)
	}
}

// RoleHint is the one line a form or a hover puts under the role.
func RoleHint(role string) string {
	switch strings.TrimSpace(role) {
	case RoleReadOnly:
		return "Reads monitors, dashboards and logs, changes nothing."
	case RoleStandard:
		return "Reads everything and writes monitors, dashboards and notebooks."
	case RoleAdmin:
		return "Everything a teammate could do, including billing and users."
	default:
		return "A role this organization defined, so what it grants is set at Datadog."
	}
}
