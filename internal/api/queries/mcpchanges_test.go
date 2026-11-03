package queries_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcpreview"
)

// defined is github's tool name (prefixed) under a definition whose
// description is description: its parts named so that two definitions of one
// tool differ in the description's part alone.
func defined(name, description string, hints coord.MCPToolHints) coord.MCPListedTool {
	t := listed(name, description, hints, 1)
	t.Declared = hints
	t.Parts = coord.MCPToolParts{Title: digestFor(name + "#title")[:coord.MCPToolPartLength],
		Description: digestFor(description)[:coord.MCPToolPartLength],
		Schema:      digestFor(name + "#schema")[:coord.MCPToolPartLength],
		Hints:       digestFor(name + "#" + hints.ReadOnly + hints.Destructive)[:coord.MCPToolPartLength]}
	return t
}

// A SERVER'S DETAIL LISTS EACH CHANGE AND HOLDS THE TOOL IT CHANGED (ADR-0078):
// a tool listed under another definition than the one accepted says which part
// moved, beside the definition accepted and dated by when the fleet first
// stored the new one; a tool added since the first listing is a change too;
// both, being tools their starting level would run unasked, answer `approve`
// and `held`; a tool the server no longer lists keeps its row, marked removed;
// and a member reads the held and removed marks and none of the changes.
//
// Mutation: answer a held tool's level as its decided allow, drop the removed
// row, or carry `changes` into the member's reading, and this is red.
func TestADetailListsEachChangeAndHoldsTheToolItChanged(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	live := epochs(t, backend)
	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	was := defined("gh_get_issue", "Read one issue", readOnly)
	now := defined("gh_get_issue", "Read one issue, and close it if stale", readOnly)
	deleteRepo := defined("gh_delete_repo", "Delete a repository", unannotated)
	added := defined("gh_list_secrets", "List the repository's secrets", readOnly)
	gone := defined("gh_old_search", "Search, the old way", readOnly)
	accepted := mcpreview.Baseline(coord.MCPListing{Server: "github",
		Tools: []coord.MCPListedTool{was, deleteRepo, gone}}, at.Add(-time.Hour))
	listings := []coord.MCPListing{{Server: "github", Node: "node-a", Epoch: live["node-a"], At: at,
		Instances: 1, Tools: []coord.MCPListedTool{deleteRepo, now, added}, ToolCount: 3}}
	seen := at.Add(-10 * time.Minute)
	surface, _ := configSurface(t, mcpCompany)
	s := mcpSources(t, backend)
	s.Company = companySource(t, cfg)
	s.Config = surface
	s.MCPListings = fakeListings{listings: listings, accepted: accepted, schemas: map[string]coord.MCPToolSchema{
		now.Digest: {Digest: now.Digest, Input: []byte(`{"type":"object"}`), At: seen},
		was.Digest: {Digest: was.Digest, Input: []byte(`{"type":"object","properties":{}}`), At: at},
	}}
	s, _ = withHistory(t, s)

	got, err := s.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if len(got.Changes) != 3 {
		t.Fatalf("changes = %+v, want get_issue changed, list_secrets added, old_search removed", got.Changes)
	}
	changed, newTool, removed := got.Changes[0], got.Changes[1], got.Changes[2]
	if changed.Tool != "get_issue" || !slices.Equal(changed.Kinds, []string{"description"}) || !changed.Held ||
		changed.Loosened || changed.Before == nil || changed.Before.Description != "Read one issue" ||
		!changed.Before.FirstListing || changed.Before.AcceptedBy != nil || changed.After == nil ||
		changed.After.Digest != now.Digest || changed.SeenAt == nil || !changed.SeenAt.Equal(seen) ||
		changed.Before.InputSchema == nil {
		t.Errorf("get_issue's change = %+v", changed)
	}
	if newTool.Tool != "list_secrets" || !slices.Equal(newTool.Kinds, []string{"added"}) || !newTool.Held ||
		newTool.Before != nil {
		t.Errorf("list_secrets' change = %+v", newTool)
	}
	if removed.Tool != "old_search" || !slices.Equal(removed.Kinds, []string{"removed"}) || removed.Held ||
		removed.After != nil || removed.SeenAt != nil {
		t.Errorf("old_search's change = %+v", removed)
	}
	byName := map[string]queries.MCPToolView{}
	for _, v := range got.Tools {
		byName[v.Name] = v
	}
	for name, want := range map[string]struct {
		level         config.MCPToolLevel
		held, removed bool
	}{
		"gh_get_issue":    {config.MCPToolApprove, true, false},
		"gh_list_secrets": {config.MCPToolApprove, true, false},
		"gh_delete_repo":  {config.MCPToolApprove, false, false},
		"gh_old_search":   {config.MCPToolAllow, false, true},
	} {
		v, ok := byName[name]
		if !ok || v.Level != want.level || v.Held != want.held || v.Removed != want.removed {
			t.Errorf("%s = %+v (%v), want level %s, held %v, removed %v", name, v, ok, want.level,
				want.held, want.removed)
		}
	}
	member := queries.MemberMCPServer(got)
	for _, v := range member.Tools {
		if v.Name == "gh_get_issue" && (!v.Held || v.Level != config.MCPToolApprove) {
			t.Errorf("a member reads get_issue as %+v, want it held", v)
		}
	}
}

// A SERVER WHOSE FIRST LISTING NOBODY HAS ACCEPTED SHOWS NO CHANGE: the node
// listing it first accepts it as it is, and until then nothing it lists waits.
func TestAServerNotYetBaselinedShowsNoChange(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	live := epochs(t, backend)
	surface, _ := configSurface(t, mcpCompany)
	s := mcpSources(t, backend)
	s.Config = surface
	s.MCPListings = fakeListings{listings: []coord.MCPListing{{Server: "github", Node: "node-a",
		Epoch: live["node-a"], At: time.Now(), Instances: 1, ToolCount: 1,
		Tools: []coord.MCPListedTool{defined("gh_get_issue", "Read one issue", readOnly)}}}}
	s, _ = withHistory(t, s)
	got, err := s.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if len(got.Changes) != 0 || got.Changes == nil || got.Tools[0].Held {
		t.Errorf("a server not yet baselined answers changes %+v, tools %+v", got.Changes, got.Tools)
	}
}
