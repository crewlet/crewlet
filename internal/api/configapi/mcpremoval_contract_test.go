package configapi_test

import (
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE REMOVE DIALOG READS WHAT THE REMOVAL SENDS.
//
// Settings › MCP & tools removes a server through
// `DELETE /config/mcp-servers/{name}`: it draws a dry run's dependants (or a
// refusal's, which carries the same list), then a write's cleared fields and
// what became of each value it asked to delete, typed by `contract/mcp.ts`.
// Every object of the real answers is held to its declaration both ways, and
// the closed sets to the engine's own; the values' own shapes are
// TestTheRemoveDialogsReadWhatARemovalSaysOfItsValues'.
//
// Mutation: drop `segments` from McpBlockDependant, or `removed` from
// McpRemovalWritten, and this is red.
func TestTheRemoveDialogReadsWhatTheRemovalSends(t *testing.T) {
	t.Parallel()
	s := serverSurface(t)
	// SIGNED IN, so the written answer carries its McpGrantRevocation.
	s.oauth.held = map[string]bool{"linear": true}

	_, refused := removeServer(t, s, "linear", "?dry_run=true", "", nil)
	code, checked := removeServer(t, s, "linear", "?dry_run=true", `{"clear": true}`, nil)
	if code != http.StatusOK {
		t.Fatalf("the dry run = %d %v", code, checked)
	}
	code, written := removeServer(t, s, "linear", "",
		`{"clear": true, "delete_secrets": ["LINEAR_ORG"], "_summary": "remove linear"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("the removal = %d %v", code, written)
	}

	object := func(v any) map[string]any {
		t.Helper()
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("expected an object, the answer sent %T", v)
		}
		return m
	}
	rows := func(v any) []map[string]any {
		t.Helper()
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			t.Fatalf("expected a list with rows, the answer sent %v", v)
		}
		out := make([]map[string]any, 0, len(list))
		for _, row := range list {
			out = append(out, object(row))
		}
		return out
	}
	dependants := []map[string]any{object(refused["dependants"]), object(checked["dependants"])}
	var blocks, box []map[string]any
	for _, d := range dependants {
		blocks = append(blocks, rows(d["mcp_env"])...)
		blocks = append(blocks, rows(d["mcp_limits"])...)
		box = append(box, rows(d["sandbox"])...)
	}
	cleared := object(written["cleared"])
	for iface, sent := range map[string][]map[string]any{
		"McpRemovalCheck":   {checked},
		"McpRemovalWritten": {written},
		"McpDependants":     dependants,
		"McpCleared":        {cleared},
		"McpBlockDependant": slices.Concat(blocks, rows(cleared["mcp_env"]),
			rows(cleared["mcp_limits"])),
		"McpSandboxDependant": slices.Concat(box, rows(cleared["sandbox"])),
		"McpGrantRevocation":  {object(written["oauth"])},
	} {
		holdDeclared(t, iface, sent)
	}

	for union, want := range map[string][]string{
		"McpHolderKind": {string(configapi.MCPHolderSeat), string(configapi.MCPHolderUnit)},
	} {
		got, err := clientsource.Union(clientsource.Tree(t), union)
		if err != nil {
			t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
			t.Errorf("the dashboard's %s is %v; the engine sends %v", union, got, want)
		}
	}
}

// holdDeclared holds the rows of one answer to the dashboard's interface both
// ways: every member declared is sent (by every row, unless it is optional),
// and every key sent is declared.
func holdDeclared(t *testing.T, iface string, sent []map[string]any) {
	t.Helper()
	members, err := clientsource.Interface(clientsource.Tree(t), iface)
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration, and skipping "+
			"would certify nothing", err)
	}
	declared := map[string]bool{} // member → required
	for _, m := range members {
		declared[m.Name] = !m.Optional
	}
	if len(declared) == 0 || len(sent) == 0 {
		t.Fatalf("%s: %d members against %d rows proves nothing", iface, len(declared), len(sent))
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		by := 0
		for _, row := range sent {
			if _, ok := row[name]; ok {
				by++
			}
		}
		switch {
		case by == 0:
			t.Errorf("%s declares %s and no row the removal sent carries it", iface, name)
		case declared[name] && by != len(sent):
			t.Errorf("%s declares %s REQUIRED and %d of %d rows send it", iface, name, by, len(sent))
		}
	}
	for _, row := range sent {
		for _, key := range slices.Sorted(maps.Keys(row)) {
			if _, ok := declared[key]; !ok {
				t.Errorf("the removal sends %s on a %s and the dashboard does not declare it", key, iface)
			}
		}
	}
}
