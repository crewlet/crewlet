package configapi_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/iam"
)

// limitsLeadDoc is leadDoc's shape with MCP limits in it: the Platform lead's
// team holds tracker's close_items at approve, their Tooling team holds
// github's merge_pr at blocked, and their SRE holds github off. Data beside
// them, which is not theirs, turns tracker off. github's entry blocks
// delete_repo.
const limitsLeadDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - {name: github, command: github-mcp, tools: {default: allow, levels: {delete_repo: blocked}}}
  - {name: tracker, command: tracker-mcp, tools: {default: allow}}
roles:
  - {name: CEO, handle: ceo, llm: zulu}
units:
  - name: Engineering
    id: engineering
    lead: cto
    roles:
      - {name: CTO, handle: cto, llm: zulu}
    children:
      - name: Platform
        id: platform
        lead: platform-lead
        mcp_limits:
          tracker: {tools: {close_items: approve}}
        roles:
          - {name: Platform Lead, handle: platform-lead, kind: human}
          - name: SRE
            handle: sre
            llm: zulu
            mcp_limits:
              github: {enabled: false}
        children:
          - name: Tooling
            id: tooling
            mcp_limits:
              github: {tools: {merge_pr: blocked}}
            roles:
              - {name: Toolsmith, handle: toolsmith, llm: zulu}
      - name: Data
        id: data
        lead: data-lead
        mcp_limits:
          tracker: {enabled: false}
        roles:
          - {name: Data Lead, handle: data-lead, llm: zulu}
`

// limitsSurface is the surface over limitsLeadDoc.
func limitsSurface(t *testing.T) *surface {
	t.Helper()
	s := newSurface(t)
	s.seed(t, limitsLeadDoc)
	return s
}

// limitsOf is an entity's `mcp_limits`, made if it has none, for an edit.
func limitsOf(e map[string]any) map[string]any {
	limits, _ := e["mcp_limits"].(map[string]any)
	if limits == nil {
		limits = map[string]any{}
		e["mcp_limits"] = limits
	}
	return limits
}

// A TEAM'S LEAD NARROWS A SERVER FOR THEIR OWN TEAMS AND NEVER WIDENS ONE (the
// series' decision D4: administrators grant, leads only narrow).
//
// Every case is one write by the Platform lead, who holds no grant. They may
// turn a server off for their team, hold a tool back for a sub-team or a seat,
// further than it is held or as far as its entry already holds it. They may not
// turn a server back on, loosen a tool from blocked to approve, lift a tool's
// limit, or move a seat out from under their team's limits: each is refused
// as an administrator's, naming every seat it would widen. Nor may they touch
// Data's limits, which are not theirs, by narrowing or by lifting.
//
// Mutation: drop the Widened parts from admit, and every widening case is
// admitted.
func TestATeamsLeadNarrowsAServerAndNeverWidensOne(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		kind, id   string
		change     func(t *testing.T, entity map[string]any)
		admitted   bool
		refusedHas []string // kind/id/side/place/why/reason, for a refusal
	}{
		{"turning a server off for their team", configapi.EntityUnits, "platform",
			func(_ *testing.T, e map[string]any) {
				limitsOf(e)["github"] = map[string]any{"enabled": false}
			}, true, nil},
		{"holding a tool back for a sub-team", configapi.EntityUnits, "tooling",
			func(_ *testing.T, e map[string]any) {
				limitsOf(e)["tracker"] = map[string]any{"tools": map[string]any{"close_items": "blocked"}}
			}, true, nil},
		{"holding a tool back for a seat", configapi.EntityRoles, "toolsmith",
			func(_ *testing.T, e map[string]any) {
				limitsOf(e)["github"] = map[string]any{"tools": map[string]any{"open_pr": "approve"}}
			}, true, nil},
		{"holding back a tool the entry already blocks", configapi.EntityUnits, "tooling",
			func(_ *testing.T, e map[string]any) {
				limitsOf(e)["github"] = map[string]any{"tools": map[string]any{
					"merge_pr": "blocked", "delete_repo": "blocked"}}
			}, true, nil},

		{"turning a server back on for a seat", configapi.EntityRoles, "sre",
			func(_ *testing.T, e map[string]any) { delete(e, "mcp_limits") }, false,
			[]string{"seat/sre///widens/no_grant"}},
		{"loosening a tool from blocked to approve", configapi.EntityUnits, "tooling",
			func(_ *testing.T, e map[string]any) {
				limitsOf(e)["github"] = map[string]any{"tools": map[string]any{"merge_pr": "approve"}}
			}, false, []string{"seat/toolsmith///widens/no_grant"}},
		{"lifting their team's limit", configapi.EntityUnits, "platform",
			func(_ *testing.T, e map[string]any) { delete(e, "mcp_limits") }, false,
			[]string{"seat/sre///widens/no_grant", "seat/toolsmith///widens/no_grant"}},
		{"moving a seat out from under their sub-team's limits", configapi.EntityUnits, "platform",
			func(t *testing.T, e map[string]any) {
				tooling := childOf(t, e, "tooling")
				var smith any
				tooling["roles"], smith = takeSeat(t, rolesOf(tooling), "toolsmith")
				e["roles"] = append(rolesOf(e), smith)
			}, false, []string{"seat/toolsmith///widens/no_grant"}},
		{"narrowing a team that is not theirs", configapi.EntityUnits, "data",
			func(_ *testing.T, e map[string]any) {
				limitsOf(e)["github"] = map[string]any{"enabled": false}
			}, false, nil},
		{"lifting a team's limit that is not theirs", configapi.EntityUnits, "data",
			func(_ *testing.T, e map[string]any) { delete(e, "mcp_limits") }, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := limitsSurface(t)
			res := putEntityAs(t, s, platformLead(), c.kind, c.id,
				func(e map[string]any) { c.change(t, e) }, "")
			switch {
			case c.admitted:
				if res.Code != http.StatusCreated {
					t.Fatalf("a lead's narrowing answered %d: %s", res.Code, res.Body.String())
				}
			case c.refusedHas == nil:
				// ANOTHER TEAM'S is refused at its address, before anything
				// is built, as every write outside the subtree is.
				if res.Code != http.StatusForbidden {
					t.Fatalf("a lead's write to another team answered %d: %s", res.Code, res.Body.String())
				}
			default:
				parts := refusedParts(t, res)
				slices.Sort(parts)
				if !slices.Equal(parts, c.refusedHas) {
					t.Errorf("refused %v, want exactly %v", parts, c.refusedHas)
				}
			}
		})
	}
}

// AN ADMINISTRATOR LIFTS WHAT A LEAD SET: the grant is theirs, so turning a
// server back on for a team and loosening a tool are admitted, refused by no
// limit.
func TestAnAdministratorLiftsALeadsLimits(t *testing.T) {
	t.Parallel()
	s := limitsSurface(t)
	admin := iam.Principal{ID: uuid.New(), Login: "ops.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour)}
	for _, id := range []string{"platform", "data"} {
		if res := putEntityAs(t, s, admin, configapi.EntityUnits, id,
			func(e map[string]any) { delete(e, "mcp_limits") }, ""); res.Code != http.StatusCreated {
			t.Errorf("an administrator lifting %s's limits = %d: %s", id, res.Code, res.Body.String())
		}
	}
}
