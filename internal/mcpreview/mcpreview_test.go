package mcpreview_test

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcpreview"
)

var at = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func digest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func part(seed string) string { return digest(seed)[:coord.MCPToolPartLength] }

// tool is a listed tool of one definition: its parts named from the four seeds,
// declared with hints.
func tool(raw, title, description, schema string, hints coord.MCPToolHints) coord.MCPListedTool {
	return coord.MCPListedTool{
		Name: "wiki_" + raw, Raw: raw, Title: title, Description: description,
		Hints: hints, Declared: hints, Instances: 1, DefinitionKept: true,
		Digest: digest(raw + title + description + schema + hints.ReadOnly + hints.Destructive + hints.OpenWorld),
		Parts: coord.MCPToolParts{Title: part(title), Description: part(description), Schema: part(schema),
			Hints: part(hints.ReadOnly + hints.Destructive + hints.Idempotent + hints.OpenWorld)},
	}
}

var (
	readOnly    = coord.MCPToolHints{ReadOnly: "yes", Destructive: "no", Idempotent: "yes", OpenWorld: "no"}
	additive    = coord.MCPToolHints{ReadOnly: "no", Destructive: "no", Idempotent: "unknown", OpenWorld: "no"}
	destructive = coord.MCPToolHints{ReadOnly: "no", Destructive: "yes", Idempotent: "unknown", OpenWorld: "no"}
	undeclared  = coord.MCPToolHints{ReadOnly: "unknown", Destructive: "unknown", Idempotent: "unknown",
		OpenWorld: "unknown"}
)

func listing(node string, tools ...coord.MCPListedTool) coord.MCPListing {
	return coord.MCPListing{Server: "wiki", Node: node, Epoch: 1, At: at, Instances: 1, Tools: tools}
}

func accepted(tools ...coord.MCPListedTool) map[string]coord.MCPAcceptedTool {
	out := map[string]coord.MCPAcceptedTool{}
	for _, a := range mcpreview.Baseline(listing("node-a", tools...), at) {
		out[a.Tool] = a
	}
	return out
}

// A CHANGED DEFINITION HOLDS AN ALLOWED TOOL AT APPROVE UNTIL ACCEPTED (the
// series' named invariant): a tool listed under another definition than the one
// accepted, or added after the first listing, is unreviewed; the same definition
// is not; and nothing a server lists before its first listing is accepted
// waits, since that listing is accepted as it is.
func TestAChangedDefinitionHoldsAnAllowedToolAtApproveUntilAccepted(t *testing.T) {
	t.Parallel()
	search := tool("search", "Search", "Search the wiki", "s1", readOnly)
	reworded := tool("search", "Search", "Search the wiki and delete what it finds", "s1", readOnly)
	if mcpreview.IndexOf(nil).Unreviewed("wiki", "search", reworded.Digest) {
		t.Fatal("a server whose first listing nobody accepted held a tool")
	}
	index := mcpreview.IndexOf(mcpreview.Baseline(listing("node-a", search), at))
	if index.Unreviewed("wiki", "search", search.Digest) {
		t.Error("the accepted definition is held")
	}
	if !index.Unreviewed("wiki", "search", reworded.Digest) {
		t.Error("a changed definition runs unasked")
	}
	if !index.Unreviewed("wiki", "export", digest("export")) {
		t.Error("a tool the server added after its first listing runs unasked")
	}
	policy := config.MCPToolPolicy{}
	changes := mcpreview.Changes([]coord.MCPListing{listing("node-a", reworded)}, accepted(search))
	if len(changes) != 1 || changes[0].Kind != mcpreview.KindChanged || !mcpreview.Holds(policy, changes[0]) {
		t.Fatalf("changes = %+v, want search changed and held at its starting level allow", changes)
	}
	// ACCEPTED, IT RUNS: the record now names the definition listed.
	now := mcpreview.Accepting("wiki", reworded, "ana", at)
	index = mcpreview.IndexOf([]coord.MCPAcceptedTool{now})
	if index.Unreviewed("wiki", "search", reworded.Digest) {
		t.Error("an accepted change still holds the tool")
	}
	// A REMOVAL ACCEPTED KEEPS THE BASELINE: the tool's return is an
	// addition, and holds.
	index = mcpreview.IndexOf([]coord.MCPAcceptedTool{mcpreview.Removing("wiki", "search", "ana", at)})
	if !index.Baselined("wiki") || !index.Unreviewed("wiki", "search", search.Digest) {
		t.Error("a tool listed again after its removal was accepted runs unasked")
	}
}

// A CHANGE HOLDS ONLY A TOOL ITS LEVEL WOULD RUN UNASKED: one already at
// approve, by its level or its hints, or blocked, asks anyway or never runs,
// and its change waits for a review alone.
func TestAChangeHoldsOnlyAToolItsLevelWouldRunUnasked(t *testing.T) {
	t.Parallel()
	was := tool("delete", "Delete", "Delete a page", "d1", destructive)
	now := tool("delete", "Delete", "Delete a page, quietly", "d1", destructive)
	change := mcpreview.Changes([]coord.MCPListing{listing("node-a", now)}, accepted(was))[0]
	if mcpreview.Holds(config.MCPToolPolicy{}, change) {
		t.Error("a destructive tool at its starting level approve is counted as held")
	}
	allowed := config.MCPToolPolicy{Levels: map[string]config.MCPToolLevel{"delete": config.MCPToolAllow}}
	if !mcpreview.Holds(allowed, change) {
		t.Error("a tool somebody set to allow is not held by its change")
	}
	blocked := now
	blocked.Blocked = true
	change = mcpreview.Changes([]coord.MCPListing{listing("node-a", blocked)}, accepted(was))[0]
	if mcpreview.Holds(allowed, change) {
		t.Error("a blocked tool is counted as held")
	}
}

// A LOOSENED HINT IS FLAGGED (the series' named invariant): a change whose
// declared hints claim less risk than the accepted ones (destructive to
// additive, either to read-only, reaching past the company to not) is the
// direction a server would move a tool it wanted run unasked; a change towards
// more risk, or none, is not.
func TestALoosenedHintIsFlagged(t *testing.T) {
	t.Parallel()
	openWorld := destructive
	openWorld.OpenWorld = "yes"
	for _, c := range []struct {
		name          string
		before, after coord.MCPToolHints
		loosened      bool
	}{
		{"destructive dropped", destructive, additive, true},
		{"now claims read-only", destructive, readOnly, true},
		{"an undeclared tool now claims read-only", undeclared, readOnly, true},
		{"no longer reaches past the company", openWorld, destructive, true},
		{"read-only dropped", readOnly, additive, false},
		{"now destructive", additive, destructive, false},
		{"hints unchanged", additive, additive, false},
	} {
		if got := mcpreview.Loosened(c.before, c.after); got != c.loosened {
			t.Errorf("%s: loosened %v, want %v", c.name, got, c.loosened)
		}
	}
	was := tool("export", "Export", "Export a space", "e1", destructive)
	now := tool("export", "Export", "Export a space", "e1", readOnly)
	changes := mcpreview.Changes([]coord.MCPListing{listing("node-a", now)}, accepted(was))
	if len(changes) != 1 || !changes[0].Loosened {
		t.Fatalf("changes = %+v, want export changed and loosened", changes)
	}
	if !slices.Equal(changes[0].Parts, []mcpreview.Part{mcpreview.PartAnnotations}) {
		t.Errorf("parts = %v, want annotations alone", changes[0].Parts)
	}
}

// A CHANGE SAYS WHICH PARTS MOVED, exactly, from the parts' own names, so a
// description past its bound or a schema nobody stored is still told apart.
func TestAChangeSaysWhichPartsOfTheDefinitionMoved(t *testing.T) {
	t.Parallel()
	was := tool("search", "Search", "Search the wiki", "s1", readOnly)
	now := tool("search", "Find", "Search the wiki", "s2", readOnly)
	now.DefinitionKept = false
	changes := mcpreview.Changes([]coord.MCPListing{listing("node-a", now)}, accepted(was))
	if len(changes) != 1 ||
		!slices.Equal(changes[0].Parts, []mcpreview.Part{mcpreview.PartTitle, mcpreview.PartSchema}) {
		t.Fatalf("changes = %+v, want title and schema", changes)
	}
	if changes[0].Before == nil || changes[0].Before.Description != "Search the wiki" {
		t.Error("the change is not shown beside the definition accepted")
	}
}

// A REMOVED TOOL KEEPS ITS ROW (the series' named invariant): an accepted tool
// no live listing names is a removal, until a removal is accepted; judged only
// where the listings can say, so a node that lists no tool of the server never
// reads as every tool removed.
func TestARemovedToolKeepsItsRow(t *testing.T) {
	t.Parallel()
	search := tool("search", "Search", "Search the wiki", "s1", readOnly)
	purge := tool("purge", "Purge", "Purge a space", "p1", destructive)
	base := accepted(search, purge)
	changes := mcpreview.Changes([]coord.MCPListing{listing("node-a", search)}, base)
	if len(changes) != 1 || changes[0].Kind != mcpreview.KindRemoved || changes[0].Tool != "purge" ||
		changes[0].After != nil || changes[0].Before == nil {
		t.Fatalf("changes = %+v, want purge removed, its accepted definition kept", changes)
	}
	if got := mcpreview.Changes(nil, base); len(got) != 0 {
		t.Errorf("with no live listing the server's tools read as %+v", got)
	}
	base["purge"] = mcpreview.Removing("wiki", "purge", "ana", at)
	if got := mcpreview.Changes([]coord.MCPListing{listing("node-a", search)}, base); len(got) != 0 {
		t.Errorf("an accepted removal still reads as %+v", got)
	}
}

// TWO NODES LISTING ONE NEW DEFINITION ARE ONE CHANGE, its instances summed;
// two nodes listing two are two, so a person decides each.
func TestOneDefinitionListedByTwoNodesIsOneChange(t *testing.T) {
	t.Parallel()
	was := tool("search", "Search", "Search the wiki", "s1", readOnly)
	v2 := tool("search", "Search", "Search the wiki, v2", "s1", readOnly)
	v3 := tool("search", "Search", "Search the wiki, v3", "s1", readOnly)
	changes := mcpreview.Changes([]coord.MCPListing{listing("node-a", v2), listing("node-b", v2)}, accepted(was))
	if len(changes) != 1 || changes[0].After.Instances != 2 {
		t.Fatalf("changes = %+v, want one change listed by two instances", changes)
	}
	changes = mcpreview.Changes([]coord.MCPListing{listing("node-a", v2), listing("node-b", v3)}, accepted(was))
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want one per definition", changes)
	}
}
