package queries_test

import (
	"slices"
	"strings"
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
	s.MCPListings = fakeListings{listings: listings, accepted: accepted, definitions: map[string]coord.MCPToolDefinition{
		now.Digest: {Digest: now.Digest, Description: now.Description, Input: []byte(`{"type":"object"}`), At: seen},
		was.Digest: {Digest: was.Digest, Description: was.Description, Input: []byte(`{"type":"object","properties":{}}`),
			At: at},
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

// lateChange is get_issue under two definitions whose descriptions share their
// first 2 KiB and differ only after it, past the cut a listing and an accepted
// record make of a description, each as the store bounds it.
func lateChange(t *testing.T, at time.Time) (was, now coord.MCPListedTool, accepted []coord.MCPAcceptedTool) {
	t.Helper()
	opening := strings.Repeat("Reads one issue by its number. ", 2*coord.MaxMCPToolDescriptionBytes/31)
	was = defined("gh_get_issue", opening+"Never closes it.", readOnly)
	now = defined("gh_get_issue", opening+"Closes it when it is stale.", readOnly)
	accepted = mcpreview.Baseline(coord.MCPListing{Server: "github", Tools: []coord.MCPListedTool{was}},
		at.Add(-time.Hour))
	for i := range accepted {
		accepted[i] = accepted[i].Bounded()
	}
	return was, now, accepted
}

// A DESCRIPTION CHANGED PAST THE LISTING'S CUT IS SERVED WHOLE AND SHOWS AS
// CHANGED: a listing and an accepted record keep a description's first KiB, so
// the two definitions read alike there, and the change is served from each
// definition's record, whole, so the description a person reviews differs
// where the server changed it, and no part is said to be unshown.
//
// Mutation: serve the listing's or the accepted record's description, or read
// the listed definition's record only where its listing kept it, and this is
// red.
func TestADescriptionChangedPastTheListingsCutIsServedWhole(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	live := epochs(t, backend)
	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	was, now, accepted := lateChange(t, at)
	listing := coord.MCPListing{Server: "github", Node: "node-a", Epoch: live["node-a"], At: at,
		Instances: 1, Tools: []coord.MCPListedTool{now}, ToolCount: 1}.Bounded()
	// THE PREMISE: what a listing and an accepted record keep reads alike.
	listing.Tools[0].DefinitionKept = false
	if accepted[0].Description != listing.Tools[0].Description {
		t.Fatalf("the cut descriptions differ, so this case tests nothing:\n%q\n%q",
			accepted[0].Description, listing.Tools[0].Description)
	}
	s := mcpSources(t, backend)
	s.Company = companySource(t, cfg)
	s.Config, _ = configSurface(t, mcpCompany)
	s.MCPListings = fakeListings{listings: []coord.MCPListing{listing}, accepted: accepted,
		definitions: map[string]coord.MCPToolDefinition{
			was.Digest: {Digest: was.Digest, Description: was.Description, Input: []byte(`{}`), At: at},
			now.Digest: {Digest: now.Digest, Description: now.Description, Input: []byte(`{}`), At: at},
		}}
	s, _ = withHistory(t, s)

	got, err := s.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if len(got.Changes) != 1 {
		t.Fatalf("changes = %+v, want get_issue's alone", got.Changes)
	}
	c := got.Changes[0]
	if !slices.Equal(c.Kinds, []string{"description"}) || len(c.Unshown) != 0 || c.Unshown == nil {
		t.Errorf("get_issue's change moved %v with %v unshown, want the description, shown", c.Kinds, c.Unshown)
	}
	if c.Before == nil || c.After == nil || !c.Before.Whole || !c.After.Whole ||
		c.Before.Description != was.Description || c.After.Description != now.Description {
		t.Fatalf("get_issue's change serves before %+v and after %+v, want both descriptions whole",
			c.Before, c.After)
	}
	if c.Before.Description == c.After.Description {
		t.Error("the two descriptions served read alike, so the review would show nothing changed")
	}
}

// A PART NO RECORD CAN SHOW IS SAID TO BE UNSHOWN, NEVER SHOWN AS UNCHANGED:
// where the fleet keeps no record of a definition, only a listing's cut words
// are known, which read alike for a change past the cut; the change names the
// part as unshown so a reader says it changed and is not shown. And a part
// whose two records read alike although its digest moved is unshown too, the
// guard against a view showing two equal texts for a change.
//
// Mutation: leave Unshown empty without the records, or compare the cut words
// rather than the records, and this is red.
func TestAPartNoRecordCanShowIsSaidUnshown(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	live := epochs(t, backend)
	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	was, now, accepted := lateChange(t, at)
	listing := coord.MCPListing{Server: "github", Node: "node-a", Epoch: live["node-a"], At: at,
		Instances: 1, Tools: []coord.MCPListedTool{now}, ToolCount: 1}.Bounded()
	for name, tc := range map[string]struct {
		definitions map[string]coord.MCPToolDefinition
		whole       bool
	}{
		"no record kept": {definitions: nil, whole: false},
		"records alike": {whole: true, definitions: map[string]coord.MCPToolDefinition{
			was.Digest: {Digest: was.Digest, Description: "Reads one issue.", Input: []byte(`{}`), At: at},
			now.Digest: {Digest: now.Digest, Description: "Reads one issue.", Input: []byte(`{}`), At: at},
		}},
	} {
		s := mcpSources(t, backend)
		s.Company = companySource(t, cfg)
		s.Config, _ = configSurface(t, mcpCompany)
		s.MCPListings = fakeListings{listings: []coord.MCPListing{listing}, accepted: accepted,
			definitions: tc.definitions}
		s, _ = withHistory(t, s)
		got, err := s.MCPServer(t.Context(), "github")
		if err != nil {
			t.Fatalf("%s: MCPServer: %v", name, err)
		}
		if len(got.Changes) != 1 {
			t.Fatalf("%s: changes = %+v, want get_issue's alone", name, got.Changes)
		}
		c := got.Changes[0]
		if !slices.Equal(c.Kinds, []string{"description"}) || !slices.Equal(c.Unshown, []string{"description"}) {
			t.Errorf("%s: get_issue's change moved %v with %v unshown, want the description, unshown",
				name, c.Kinds, c.Unshown)
		}
		if c.Before.Whole != tc.whole || c.After.Whole != tc.whole {
			t.Errorf("%s: before whole %v, after whole %v, want %v", name, c.Before.Whole, c.After.Whole,
				tc.whole)
		}
	}
}
