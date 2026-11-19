package seataccess

import "slices"

// NeedsStepUp reports whether moving one seat's lane from before to after is a
// raise to an administration grant (Datadog Admin Role, GitLab Maintainer or
// Owner, Jira Administrators, Confluence Admin, Mattermost System Admin, GitHub
// write on Contents or Workflows), which needs a fresh step-up (D10). DERIVED
// FROM THE VENDOR'S OWN FACTS, never stored: opts marks each level
// ([Level.Administers]) and each permission ([PermissionSpec.AdministersOnWrite]).
// Keeping a held grant and lowering never need one; an extra write permission
// is a raise ([RaisesGrant]).
//
// IT FAILS CLOSED: a level the offer cannot vouch for cannot be shown not to
// administer, so moving a seat to it needs the step-up; judged open, an
// unreachable vendor would let a machine token hand out an administration role.
func NeedsStepUp(opts Options, before, after Desired) bool {
	if !after.Enabled {
		return false
	}
	if RaisesGrant(opts, before, after) {
		return true
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
	// A WRITE GRANT IS MARKED as the level it is a raise like; one the
	// catalogue cannot name is not called one here, as for a level.
	if slices.ContainsFunc(d.Grants, func(g Grant) bool {
		entry, ok := opts.Grantable(g.Permission)
		return ok && entry.Write
	}) {
		return true
	}
	return slices.ContainsFunc(opts.Permissions, func(p PermissionSpec) bool {
		return p.AdministersOnWrite && d.Permissions[p.Key] == AccessWrite
	})
}
