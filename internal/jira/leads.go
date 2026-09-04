package jira

import (
	"github.com/crewlet/crewlet/internal/org"
)

// LeadsFrom maps each Jira project key to the handle that owns it.
//
// The walk itself is [org.Organization.LeadsBy] — which seat owns a scope is
// a question about the org chart, and the tracker's only contribution is
// naming the field and reporting what the walk found in Jira's own
// vocabulary.
func LeadsFrom(o *org.Organization) map[string]string {
	leads, report := o.LeadsBy(org.Scope{
		OfUnit: func(u *org.Unit) string { return u.Project },
		OfRole: func(r *org.Role) string { return r.Project },
	})
	for _, unled := range report.Unled {
		// A unit that owns a project and has nobody leading it. LOUD,
		// because the consequence is invisible: every issue in that
		// project that names nobody here routes to nobody, and that looks
		// exactly like an issue nobody filed.
		log.Warn("jira_project_has_no_lead", "unit", unled.Unit,
			"project", unled.Scope)
	}
	for _, conflict := range report.Ambiguous {
		log.Warn("jira_project_lead_ambiguous", "project", conflict.Scope,
			"declared_by", conflict.DeclaredBy, "chose", conflict.Chose,
			"candidates", conflict.Candidates)
	}
	return leads
}
