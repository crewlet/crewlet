package jira

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/org"
)

// Which seats hold a tracker identity, and where that identity lives.
//
// # The engine names no variable of its own
//
// A seat's Jira credential lives in its `mcp_env.jira` block, under whichever
// key that seat's tool stack reads — the community Atlassian MCP servers want
// JIRA_API_TOKEN beside JIRA_USERNAME, an HTTP server takes an Authorization
// header. So the scan looks under the keys the tools already use rather than
// inventing CREWLET_JIRA_TOKEN_<seat>, which would be a variable nothing
// reads.
//
// ONE list, exported, read by both the engine's identity resolution and the
// reconcile's report. A second copy beside either of them is how the two come
// to look under different keys — and the failure is silent, because a seat
// whose credential was not found looks exactly like a seat that has none.

// A SEAT'S OWN TRACKER CREDENTIAL IS READ THROUGH [atlassian.CredentialAt].
//
// It used to be read here, by this package's own SeatEnvs / CredentialKeys /
// EmailKeys and a seatBlock scan — and by a second, differently-spelled set in
// internal/confluence, and a third in internal/atlassian. One Atlassian account
// authenticates both products, so that was one question answered three ways,
// and they disagreed: a seat holding `mcp_env.atlassian.JIRA_API_TOKEN`, which
// is the spelling atlassian.PlanFor's own note tells operators to write, read
// as ready to Jira and as having no credential at all to Confluence, for ever.
//
// The reader moved to internal/atlassian because that package owns the
// ACCOUNT — it mints the service account and records the address Atlassian
// assigns — and the products are consumers of an identity it creates.

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
