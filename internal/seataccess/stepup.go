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
// A level the vendor does not list is not judged here: the document's rules
// refuse it, and a step-up cannot make it valid.
func NeedsStepUp(opts Options, before, after Desired) bool {
	if !after.Enabled {
		return false
	}
	if after.Level != "" && (!before.Enabled || before.Level != after.Level) {
		levels, _ := opts.LevelsOf(after.Level)
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
