package seataccess

import (
	"errors"
	"maps"
	"strings"
)

// ErrSeveralRoles refuses keeping a Datadog account's roles when it holds more
// than one: a seat names one role, and keeping one of several would take the
// others away.
var ErrSeveralRoles = errors.New("the account holds several Datadog roles, and an agent " +
	"holds one: leave it one role at Datadog, or save the role it should hold here")

// KeptAccess is one seat's lane as the vendor holds it, in the document's
// terms: what "Keep the vendor's setting" (after drift, D2) and "Keep off"
// (after the account was turned off at the vendor, D3) write, so the next sync
// finds nothing to restore. want is the lane as the document holds it now, and
// have what the vendor shows.
//
// EACH VENDOR'S SETTING IN ITS OWN TERMS:
//   - an account turned off is kept off, and keeps the access the document
//     names, so turning it back on restores that rather than nothing;
//   - Slack holds no level, permissions or scope here (its scopes are its
//     app's), so only on or off is kept;
//   - GitHub keeps the app's permissions, every other vendor its level;
//   - a lane that narrows by scope keeps the vendor's scope.
func KeptAccess(lane Lane, want Desired, have Observed) (Desired, error) {
	kept := want.Normalized()
	kept.Enabled = have.Exists && have.Enabled
	if !kept.Enabled || lane == LaneSlack {
		return kept, nil
	}
	switch lane {
	case LaneGitHub:
		kept.Permissions = maps.Clone(have.Permissions)
	case LaneDatadog:
		if len(strings.Fields(have.Level)) > 1 {
			return Desired{}, ErrSeveralRoles
		}
		kept.Level = have.Level
	default:
		kept.Level = have.Level
	}
	if scoped(lane) {
		kept.Scope = have.Scope
	}
	return kept.Normalized(), nil
}

// scoped reports a lane that narrows by scope, by its vendor's catalog; a lane
// whose vendor this build does not register is taken as one, since the
// vendor's scope is what it shows.
func scoped(lane Lane) bool {
	vendor, ok := Lookup(lane.Kind())
	if !ok {
		return true
	}
	spec, ok := vendor.Catalog().Lane(lane)
	return !ok || spec.Scoped
}
