package coord_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A SERVER'S REFUSED AND SHADOWED TOOLS TRAVEL BOUNDED, AND COUNTED WHOLE: a
// server lists as many tools as it likes, and a long prefix can refuse every
// one of them, so a row names at most MaxMCPToolNotes of each, every name cut
// to MaxMCPToolNameBytes, and carries how many there are in all. Through the
// JSON round trip a lease store makes, where a count becomes a float64 and a
// list of maps a list of anything.
//
// Mutation: drop the bound on either list, or the cut on a name, or read a
// count back as its list's length, and this is red.
func TestAServersRefusedAndShadowedToolsTravelBoundedAndCounted(t *testing.T) {
	t.Parallel()
	var refused []coord.MCPToolRefusal
	for i := range 12 {
		refused = append(refused, coord.MCPToolRefusal{Tool: fmt.Sprintf("docs_tool_%02d", i), Reason: "too_long"})
	}
	long := strings.Repeat("n", 3*coord.MaxMCPToolNameBytes)
	refused[0].Tool = long
	status := coord.NodeStatus{MCP: []coord.MCPServerStatus{{
		Server: "docs", Shared: true, Started: 1, Tools: 3,
		Refused: refused, RefusedCount: len(refused),
		Shadowed:      []coord.MCPToolShadow{{Tool: "search", By: "handbook"}},
		ShadowedCount: 1,
	}}}
	raw, err := json.Marshal(map[string]any{coord.StatusKey: status.Meta()})
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	got, ok := coord.StatusFromMeta(meta)
	if !ok || len(got.MCP) != 1 {
		t.Fatalf("decoded %+v", got)
	}
	row := got.MCP[0]
	if len(row.Refused) != coord.MaxMCPToolNotes || row.RefusedCount != 12 {
		t.Errorf("refused travelled as %d named of %d, want %d named of 12",
			len(row.Refused), row.RefusedCount, coord.MaxMCPToolNotes)
	}
	if n := len(row.Refused[0].Tool); n > coord.MaxMCPToolNameBytes || !strings.HasSuffix(row.Refused[0].Tool, "…") {
		t.Errorf("a %d-byte name travelled as %d bytes: %q", len(long), n, row.Refused[0].Tool)
	}
	if !reflect.DeepEqual(row.Refused[1:], refused[1:coord.MaxMCPToolNotes]) {
		t.Errorf("refused = %v, want the first %d as listed", row.Refused[1:], coord.MaxMCPToolNotes)
	}
	want := []coord.MCPToolShadow{{Tool: "search", By: "handbook"}}
	if !reflect.DeepEqual(row.Shadowed, want) || row.ShadowedCount != 1 {
		t.Errorf("shadowed = %v of %d, want %v of 1", row.Shadowed, row.ShadowedCount, want)
	}
}

// A ROW WITH NOTHING REFUSED OR SHADOWED CARRIES NEITHER KEY: the beat of a
// company whose names are in order is what it was before either existed.
func TestARowWithEveryToolOfferedCarriesNoNotes(t *testing.T) {
	t.Parallel()
	status := coord.NodeStatus{MCP: []coord.MCPServerStatus{{Server: "docs", Shared: true, Started: 1, Tools: 3}}}
	rows, _ := status.Meta()["mcp"].([]map[string]any)
	if len(rows) != 1 {
		t.Fatalf("mcp rows = %v", status.Meta()["mcp"])
	}
	for _, key := range []string{"refused", "refused_count", "shadowed", "shadowed_count"} {
		if _, ok := rows[0][key]; ok {
			t.Errorf("a row with every tool offered carries %q", key)
		}
	}
	// A count a peer wrote short of its own list is read as at least the list.
	got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: map[string]any{"mcp": []any{
		map[string]any{"server": "docs", "refused": []any{map[string]any{"tool": "a", "reason": "characters"}}},
	}}})
	if row := got.MCP[0]; row.RefusedCount != 1 {
		t.Errorf("a list of one with no count read as %d refused", row.RefusedCount)
	}
}
