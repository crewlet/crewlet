package jira

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/org"
)

// The tracker's half of the org walk: which projects the chart names.
//
// A seat's Atlassian CREDENTIAL is not here. One account authenticates both
// products, so the grammar for finding it lives in [atlassian] and is read by
// the tracker, the knowledge base and the provisioner alike — it was written
// twice before, and the two copies had already come to look under different
// keys.

// ProjectsOf lists every Jira project key the org declares, sorted and
// upper-cased.
//
// Units and seats both, because both can name one: a unit says which project
// it owns, and a root seat says where it files. The reconcile checks all of
// them, because a project key with a typo in it is a routing gap that
// produces no error anywhere — the webhook arrives, the key matches nothing,
// and the issue reaches nobody.
func ProjectsOf(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	seen := map[string]bool{}
	for u := range o.AllUnits() {
		if key := org.NormalizeScope(u.JiraProject); key != "" {
			seen[key] = true
		}
	}
	for seat := range o.AllRoles() {
		if key := org.NormalizeScope(seat.JiraProject); key != "" {
			seen[key] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
