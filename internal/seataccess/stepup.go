package seataccess

import "slices"

// NeedsStepUp reports whether moving one seat's lane from before to after is a
// raise to an administration grant, which needs a fresh step-up from the
// person saving it (D10): Datadog Admin Role, GitLab Maintainer or Owner, Jira
// Administrators, Confluence Admin, Mattermost System Admin, or a GitHub
// Read and write on Contents or Workflows.
//
// DERIVED FROM THE VENDOR'S OWN FACTS, never stored: opts is what the vendor
// offers now, each level marked by the vendor as administering or not
// ([Level.Administers]), and each GitHub permission whose write administers
// ([PermissionSpec.AdministersOnWrite]). Nothing in the document says "admin".
//
// A seat that already held the grant does not need a step-up to keep it, and
// lowering never does: the rule guards the moment power is handed out.
//
// IT FAILS CLOSED. A level the offer cannot vouch for (the vendor's list could
// not be read, or does not name the level) cannot be shown not to administer,
// so moving a seat to it needs the step-up. Judged open, an unreachable vendor
// would let a machine token hand out an administration role unchallenged; the
// sync still refuses a level the vendor does not offer.
func NeedsStepUp(opts Options, before, after Desired) bool {
	if !after.Enabled {
		return false
	}
	if after.Level != "" && (!before.Enabled || before.Level != after.Level) {
		levels, known := opts.LevelsOf(after.Level)
		if !known {
			return true
		}
		held, _ := opts.LevelsOf(before.Level)
		for _, level := range levels {
			// A SET RAISES where it adds an administering level the seat did
			// not hold; a single level where it is one.
			if level.Administers && (!before.Enabled || !slices.ContainsFunc(held,
				func(h Level) bool { return h.ID == level.ID })) {
				return true
			}
		}
	}
	for _, perm := range opts.Permissions {
		if !perm.AdministersOnWrite || after.Permissions[perm.Key] != AccessWrite {
			continue
		}
		if !before.Enabled || before.Permissions[perm.Key] != AccessWrite {
			return true
		}
	}
	return false
}

// Administers reports whether a seat holding d on a lane holds an
// administration grant, as the vendor's offer shows it: what the Agents tab
// marks on the seat. A DESCRIPTION, so it claims only what the offer shows:
// a level the offer cannot vouch for is not called an administration role
// here, whereas [NeedsStepUp], which guards the grant, treats it as one.
func Administers(opts Options, d Desired) bool {
	if !d.Enabled {
		return false
	}
	levels, _ := opts.LevelsOf(d.Level)
	if slices.ContainsFunc(levels, func(l Level) bool { return l.Administers }) {
		return true
	}
	return slices.ContainsFunc(opts.Permissions, func(p PermissionSpec) bool {
		return p.AdministersOnWrite && d.Permissions[p.Key] == AccessWrite
	})
}
