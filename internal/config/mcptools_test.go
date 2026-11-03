package config

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// A TOOL'S LEVEL IS DECIDED BY THE SERVER'S OWN NAME FOR IT: its own level
// where `levels` names it, else the default, else the level its hints start it
// at, which the caller reads off the server's listing (D1: allow for a
// read-only tool, approve for every other). A server allows every tool only
// while its default says allow and no level holds one back, which is what an
// executor box is held to: an unset default allows only what a server marks
// read-only, which no document can know.
//
// Mutation: make Level ignore the default or the starting level, or let
// AllowsEveryTool read an unset default as allow, and this is red.
func TestAToolsLevelIsDecidedByTheServersOwnName(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
mcp_servers:
  - {name: open, command: open-mcp}
  - {name: allows, command: allows-mcp, tools: {default: allow, levels: {read: allow}}}
  - {name: tracker, command: tracker-mcp, tool_prefix: tr_, tools: {levels: {delete_project: blocked}}}
  - {name: billing, command: billing-mcp, tools: {default: blocked, levels: {list_invoices: allow}}}
  - {name: asks, command: asks-mcp, tools: {default: approve, levels: {search: allow}}}
`)
	type decision struct {
		level  MCPToolLevel
		custom bool
	}
	// EVERY TOOL ASKED ABOUT STARTS AT APPROVE BY ITS HINTS, so a decision
	// that reads allow came from the entry, never the start.
	for i, want := range []struct {
		every bool
		tools map[string]decision
	}{
		{false, map[string]decision{"anything": {MCPToolApprove, false}}},
		{true, map[string]decision{"read": {MCPToolAllow, true}, "write": {MCPToolAllow, false}}},
		{false, map[string]decision{"delete_project": {MCPToolBlocked, true},
			"tr_delete_project": {MCPToolApprove, false}, "list_projects": {MCPToolApprove, false}}},
		{false, map[string]decision{"list_invoices": {MCPToolAllow, true}, "refund": {MCPToolBlocked, false}}},
		{false, map[string]decision{"search": {MCPToolAllow, true}, "delete": {MCPToolApprove, false}}},
	} {
		server := &cfg.MCPServers[i]
		if got := server.Tools.AllowsEveryTool(); got != want.every {
			t.Errorf("%s allows every tool: %v, want %v", server.Name, got, want.every)
		}
		for tool, d := range want.tools {
			if level, custom := server.Tools.Level(tool, MCPToolApprove); level != d.level || custom != d.custom {
				t.Errorf("%s's %s runs at %s (custom %v), want %s (custom %v)",
					server.Name, tool, level, custom, d.level, d.custom)
			}
		}
	}
}

// A LEVEL IS ALLOW, APPROVE OR BLOCKED, AND NAMES A TOOL: a default or a level
// outside the three is refused at its field, and so is a key no tool name could
// be (empty, or holding a space), each at its entry. Approve is a level the
// engine enforces (ADR-0077), so it is accepted.
//
// Mutation: drop the level check from MCPToolPolicy.validate, and this is red.
func TestALevelIsAllowApproveOrBlockedAndNamesATool(t *testing.T) {
	t.Parallel()
	mustCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, tools: {default: approve, levels: {list_projects: allow, merge: approve}}}
`)
	for _, c := range []struct {
		name, tools, path string
		kind              error
	}{
		{"a default outside the set", `{default: ask}`, "mcp_servers[0].tools.default", ErrUnknownValue},
		{"a level outside the set", `{levels: {delete_project: deny}}`,
			"mcp_servers[0].tools.levels.delete_project", ErrUnknownValue},
		{"an empty key", `{levels: {"": blocked}}`, "mcp_servers[0].tools.levels", ErrMissing},
		{"a key holding a space", `{levels: {"delete project": blocked}}`,
			"mcp_servers[0].tools.levels", ErrUnknownValue},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, tools: `+c.tools+`}
`, c.path)
			if !errors.Is(err, c.kind) {
				t.Errorf("want %v, got %v", c.kind, err)
			}
		})
	}
}

// AN EXECUTOR BOX IS REFUSED A SERVER THAT HOLDS A TOOL BACK: the harness in a
// box dials a server itself, past the engine, so a level could not hold there,
// and a tool at approve would run without anybody asked. A listing of a server
// with a tool blocked or at approve, by name or by its default, or with no
// default at all (whose tools start by their hints, most at approve), is
// refused at the box's entry, whichever of the two was written last; a server
// whose default and levels all allow is admitted. A stored revision carrying
// one runs and warns, its box given nothing for it (internal/engine).
//
// Mutation: drop validateBoxToolLevels from validateAdmission, or read an unset
// default as allow, and this is red.
func TestAnExecutorBoxIsRefusedAServerThatHoldsAToolBack(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, tools string }{
		{"a tool blocked by name", `{levels: {delete_project: blocked}}`},
		{"every tool blocked by the default", `{default: blocked, levels: {list_projects: allow}}`},
		{"a tool at approve by name", `{default: allow, levels: {merge: approve}}`},
		{"no default, so each tool starts by its hints", `{levels: {list_projects: allow}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			doc := `
name: Acme
providers: {sandbox: {fake: true}}
mcp_servers:
  - {name: notes, command: notes-mcp, tools: {default: allow}}
  - {name: tracker, command: tracker-mcp, tools: ` + c.tools + `}
roles:
  - {name: Dev, handle: dev, sandbox: {mcp: {servers: [notes, tracker]}}}
`
			const path = "roles[0].sandbox.mcp.servers[1]"
			err := rejects(t, doc, path)
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "box reaches a server whole") {
				t.Errorf("want the box refusal, got %v", err)
			}
			if strings.Contains(err.Error(), "servers[0]") {
				t.Errorf("a server whose every tool is allowed is refused too: %v", err)
			}
			stored, err := ParseCompanyDocument([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			if err := stored.ValidateRunnable(); err != nil {
				t.Fatalf("a stored revision carrying it must still run: %v", err)
			}
			if !slices.ContainsFunc(stored.AdmissionWarnings(), func(w Warning) bool { return w.Path == path }) {
				t.Errorf("a stored revision carrying it does not warn at %s: %+v", path, stored.AdmissionWarnings())
			}
		})
	}
	mustCompany(t, `
name: Acme
providers: {sandbox: {fake: true}}
mcp_servers:
  - {name: tracker, command: tracker-mcp, tools: {default: allow, levels: {list_projects: allow}}}
roles:
  - {name: Dev, handle: dev, sandbox: {mcp: {servers: [tracker]}}}
`)
}

// A SERVER THAT SETS NO LEVEL WRITES NO `tools`, and one that does keeps what
// it set through the document's own encoding, which every write of the
// document goes through.
func TestToolLevelsRoundTripThroughTheDocument(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
mcp_servers:
  - {name: open, command: open-mcp, tools: {levels: {}}}
  - {name: billing, command: billing-mcp, tools: {default: blocked, levels: {list_invoices: allow}}}
`)
	raw, err := json.Marshal(cfg.MCPServers[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"tools"`) {
		t.Errorf("a server setting no level writes %s", raw)
	}
	document, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeCompany(document)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.MCPServers[1].Tools; got.Default != MCPToolBlocked ||
		len(got.Levels) != 1 || got.Levels["list_invoices"] != MCPToolAllow {
		t.Errorf("billing's levels came back as %+v", got)
	}
}
