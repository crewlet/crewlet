package jira

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// Which seats hold a tracker identity, and where that identity lives.
//
// # The engine names no variable of its own
//
// A seat's Jira credential lives in its `mcp_env.jira` block, under whichever
// key its tool stack reads (JIRA_API_TOKEN beside JIRA_USERNAME, or an
// Authorization header), not a CREWLET_JIRA_TOKEN_<seat> nothing reads.
//
// ONE exported list, read by both the engine's identity resolution and the
// reconcile's report: a second copy makes the two look under different keys,
// silently, since an unfound credential looks like none.

// A SEAT'S OWN TRACKER CREDENTIAL IS READ THROUGH [atlassian.CredentialAt].
//
// One Atlassian account authenticates both products, so one reader serves
// both: separate spellings per product disagree, a seat holding
// `mcp_env.atlassian.JIRA_API_TOKEN` reading as ready to Jira and credential-
// less to Confluence. The reader lives in internal/atlassian because that
// package owns the ACCOUNT (it mints the service account and records the
// address); the products consume the identity.

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
		if key := config.JiraProjectKey(u.Project); key != "" {
			seen[key] = true
		}
	}
	for seat := range o.AllRoles() {
		if key := config.JiraProjectKey(seat.Project); key != "" {
			seen[key] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
