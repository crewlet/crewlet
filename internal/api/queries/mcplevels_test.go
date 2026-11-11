package queries_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// A SERVER'S DETAIL SAYS THE LEVEL EACH TOOL RUNS AT, blocked ones listed. The
// level is the entry's decision by the server's own name for the tool (its
// level, else the default), `custom` whether the entry names it, `baseline`
// the default, and `unlisted_levels` every level no live listing names: here a
// level keyed by the prefixed name, which decides nothing. The `tools` push
// every reader shares lists only what an agent can be offered, so a blocked
// tool is not in it, and its name is left free.
//
// Mutation: answer the level from the listing's flag where the entry decides,
// judge unlisted levels against the prefixed names, or let ListedRows take a
// blocked tool, and this is red.
func TestADetailSaysTheLevelEachToolRunsAt(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	live := epochs(t, backend)
	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	github := slices.IndexFunc(cfg.MCPServers, func(m config.MCPServer) bool { return m.Name == "github" })
	cfg.MCPServers[github].Tools = config.MCPToolPolicy{Default: config.MCPToolBlocked,
		Levels: map[string]config.MCPToolLevel{"get_issue": config.MCPToolAllow,
			"gh_create_issue": config.MCPToolBlocked}}
	getIssue := listed("gh_get_issue", "Read one issue", readOnly, 1)
	deleteRepo := listed("gh_delete_repo", "Delete a repository", unannotated, 1)
	deleteRepo.Blocked = true
	listings := []coord.MCPListing{{Server: "github", Node: "node-a", Epoch: live["node-a"],
		At: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), Instances: 1,
		Tools: []coord.MCPListedTool{deleteRepo, getIssue}}}
	surface, _ := configSurface(t, mcpCompany)
	s := mcpSources(t, backend)
	s.Company = companySource(t, cfg)
	s.Config, s.MCPListings = surface, fakeListings{listings: listings}
	s, _ = withHistory(t, s)

	got, err := s.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %+v, want the blocked tool listed beside the allowed one", got.Tools)
	}
	for _, want := range []struct {
		name   string
		level  config.MCPToolLevel
		custom bool
	}{
		{"gh_delete_repo", config.MCPToolBlocked, false},
		{"gh_get_issue", config.MCPToolAllow, true},
	} {
		i := slices.IndexFunc(got.Tools, func(v queries.MCPToolView) bool { return v.Name == want.name })
		if i < 0 || got.Tools[i].Level != want.level || got.Tools[i].Custom != want.custom {
			t.Errorf("%s = %+v, want level %s, custom %v", want.name, got.Tools, want.level, want.custom)
		}
	}
	if got.Baseline == nil || *got.Baseline != config.MCPToolBlocked ||
		!slices.Equal(got.UnlistedLevels, []string{"gh_create_issue"}) {
		t.Errorf("baseline %v, unlisted %v; want blocked and the prefixed name", got.Baseline, got.UnlistedLevels)
	}
	if member := queries.MemberMCPServer(got); member.Tools[0].Level != config.MCPToolBlocked {
		t.Errorf("a member reads %+v, want the level that tells them why no agent is offered it", member.Tools[0])
	}

	rows := queries.ListedRows(nil, listings, nil)
	if len(rows) != 1 || rows[0].Name != "gh_get_issue" {
		t.Errorf("the tools push lists %+v, want only the tool an agent can be offered", rows)
	}
}

// UNLISTED LEVELS ARE JUDGED ONLY WHERE THE LISTING CAN SAY: with no live
// listing (a server off or not started), a level the listing does not name may
// name a tool the server lists, so none is named.
func TestUnlistedLevelsAreJudgedOnlyWhereTheListingCanSay(t *testing.T) {
	t.Parallel()
	policy := config.MCPToolPolicy{Levels: map[string]config.MCPToolLevel{"zeta": config.MCPToolBlocked,
		"alpha": config.MCPToolBlocked, "search": config.MCPToolAllow}}
	whole := queries.MCPServerAnswer{Listed: []queries.MCPServerListing{{Node: "node-a", Tools: 1}},
		Tools: []queries.MCPToolView{{Name: "x_search", Raw: "search"}}}
	if got := queries.UnlistedLevels(policy, whole); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Errorf("unlisted = %v, want alpha and zeta, sorted", got)
	}
	if got := queries.UnlistedLevels(policy, queries.MCPServerAnswer{}); got == nil || len(got) != 0 {
		t.Errorf("with no listing, unlisted = %v, want an empty list", got)
	}
}
