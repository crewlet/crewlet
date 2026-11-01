package config

import (
	"encoding/json"
	"slices"
	"testing"
)

// referencesDoc names the server `tracker` from every kind of field, at every
// depth, beside a second server and the `github` vendor block, which the
// engine reads by name whatever servers exist.
const referencesDoc = `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false}
  - {name: search, command: search-mcp}
  - {name: github, transport: http, url: "https://api.example.com/mcp", shared: false}
roles:
  - name: CEO
    handle: ceo
    mcp_env:
      tracker: {TOKEN: "${CEO_TRACKER}"}
      search: {KEY: "${SEARCH_KEY}"}
    sandbox:
      mcp: {servers: [search, tracker]}
units:
  - name: Eng
    id: eng
    mcp_env:
      tracker: {REGION: "${TRACKER_REGION}"}
    roles:
      - name: Dev
        handle: dev
        mcp_env:
          github: {Authorization: "Bearer ${DEV_GH}"}
        sandbox:
          max_turns: 40
          mcp: {servers: [tracker, github]}
    children:
      - name: Core
        id: core
        mcp_env:
          tracker: {TOKEN: "${CORE_TRACKER}"}
        roles:
          - name: Ops
            handle: ops
            sandbox: {mcp: {servers: [tracker]}}
`

// referenceRow is a reference as a test compares it.
type referenceRow struct {
	Field      MCPReferenceField
	Seat, Unit string
	Segments   string
	EngineRead bool
}

func rowsOf(t *testing.T, refs []MCPServerReference) []referenceRow {
	t.Helper()
	out := make([]referenceRow, 0, len(refs))
	for _, ref := range refs {
		segments, err := json.Marshal(ref.Segments)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, referenceRow{Field: ref.Field, Seat: ref.Seat, Unit: ref.Unit,
			Segments: string(segments), EngineRead: ref.EngineRead})
	}
	return out
}

// EVERY FIELD THAT NAMES A SERVER IS ONE OF ITS REFERENCES, wherever it is
// written, in document order, and only that server's.
//
// The walk is what a removal clears and what the reference rule refuses, so a
// field it misses is one a removal leaves behind naming nothing. A block the
// engine reads by its name besides the server's is marked, since a removal
// must leave it.
//
// Mutation: drop the sandbox walk, or the units walk, and this fails.
func TestEveryFieldThatNamesAServerIsOneOfItsReferences(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, referencesDoc)

	got := rowsOf(t, cfg.MCPServerReferences("tracker"))
	want := []referenceRow{
		{MCPReferenceEnv, "ceo", "", `["roles",0,"mcp_env","tracker"]`, false},
		{MCPReferenceSandbox, "ceo", "", `["roles",0,"sandbox","mcp","servers",1]`, false},
		{MCPReferenceSandbox, "dev", "", `["units",0,"roles",0,"sandbox","mcp","servers",0]`, false},
		{MCPReferenceSandbox, "ops", "",
			`["units",0,"children",0,"roles",0,"sandbox","mcp","servers",0]`, false},
		{MCPReferenceEnv, "", "eng", `["units",0,"mcp_env","tracker"]`, false},
		{MCPReferenceEnv, "", "core", `["units",0,"children",0,"mcp_env","tracker"]`, false},
	}
	if !slices.Equal(got, want) {
		t.Errorf("tracker's references:\n got %+v\nwant %+v", got, want)
	}

	github := rowsOf(t, cfg.MCPServerReferences("github"))
	if len(github) != 2 || !github[0].EngineRead || github[1].EngineRead {
		t.Errorf("github's references = %+v, want dev's block marked as one the engine "+
			"reads and dev's box entry not", github)
	}
	if refs := cfg.MCPServerReferences("absent"); len(refs) != 0 {
		t.Errorf("a name nothing writes has references: %+v", refs)
	}
}

// A REMOVAL CLEARS EVERY FIELD THAT NAMES THE SERVER ALONE, AND NOTHING ELSE.
//
// The server leaves `mcp_servers`; every seat's and unit's block for it and
// every box entry listing it go; a mapping left empty is dropped rather than
// written as `{}`; another server's fields stay; and what remains validates,
// because nothing names what is gone.
//
// Mutation: skip the units, or keep an emptied sandbox, and this fails.
func TestRemovingAServerClearsEveryFieldThatNamesItAlone(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, referencesDoc)
	want := rowsOf(t, cfg.MCPServerDependants("tracker"))

	cleared, found := cfg.RemoveMCPServer("tracker")
	if !found {
		t.Fatal("a declared server was not found")
	}
	if got := rowsOf(t, cleared); !slices.Equal(got, want) || len(got) != 6 {
		t.Errorf("cleared:\n got %+v\nwant every one of tracker's six dependants %+v", got, want)
	}
	if len(cfg.MCPServerReferences("tracker")) != 0 {
		t.Errorf("tracker is still named: %+v", cfg.MCPServerReferences("tracker"))
	}
	if slices.ContainsFunc(cfg.MCPServers, func(s MCPServer) bool { return s.Name == "tracker" }) {
		t.Error("the entry is still declared")
	}
	ceo, dev, ops := &cfg.Roles[0], &cfg.Units[0].Roles[0], &cfg.Units[0].Children[0].Roles[0]
	if ceo.MCPEnv["search"]["KEY"] != "${SEARCH_KEY}" ||
		!slices.Equal(ceo.Sandbox.MCP.Servers, []string{"search"}) {
		t.Errorf("another server's fields moved: %+v %+v", ceo.MCPEnv, ceo.Sandbox)
	}
	if dev.Sandbox == nil || dev.Sandbox.MaxTurns == nil ||
		!slices.Equal(dev.Sandbox.MCP.Servers, []string{"github"}) {
		t.Errorf("dev's sandbox = %+v, want its own settings and github kept", dev.Sandbox)
	}
	if ops.Sandbox != nil {
		t.Errorf("ops's sandbox held only tracker and is %+v, want it dropped", ops.Sandbox)
	}
	if cfg.Units[0].MCPEnv != nil || cfg.Units[0].Children[0].MCPEnv != nil {
		t.Errorf("an emptied unit block stays: %v %v", cfg.Units[0].MCPEnv,
			cfg.Units[0].Children[0].MCPEnv)
	}
	assertStillValid(t, cfg)

	if cleared, found := cfg.RemoveMCPServer("tracker"); found || cleared != nil {
		t.Errorf("a second removal found %v and cleared %+v", found, cleared)
	}
}

// A BLOCK THE ENGINE READS BY ITS NAME OUTLIVES THE SERVER OF THAT NAME.
//
// A seat's `mcp_env.github` is its GitHub App token as much as the `github`
// server's credentials, so removing the server leaves the block for the
// engine, while the box entry, which only the server answered, goes.
//
// Mutation: clear every block named after the server, and dev's token goes.
func TestRemovingAVendorNamedServerKeepsTheBlockTheEngineReads(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, referencesDoc)

	cleared, found := cfg.RemoveMCPServer("github")
	if !found {
		t.Fatal("a declared server was not found")
	}
	if got := rowsOf(t, cleared); len(got) != 1 || got[0].Field != MCPReferenceSandbox {
		t.Errorf("cleared = %+v, want dev's box entry alone", got)
	}
	dev := &cfg.Units[0].Roles[0]
	if dev.MCPEnv["github"]["Authorization"] != "Bearer ${DEV_GH}" {
		t.Errorf("dev's github block = %v, want it kept for the engine", dev.MCPEnv)
	}
	if !slices.Equal(dev.Sandbox.MCP.Servers, []string{"tracker"}) {
		t.Errorf("dev's box = %v, want github out of it", dev.Sandbox.MCP.Servers)
	}
	assertStillValid(t, cfg)
}

// assertStillValid holds c, written back out, to every rule a submitted
// document answers to.
func assertStillValid(t *testing.T, c *Company) {
	t.Helper()
	document, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCompany(document); err != nil {
		t.Errorf("the document a removal leaves does not validate: %v", err)
	}
}
