package engine

import (
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// grantCompany grants a shared server to one seat and a per-agent server, for
// which nobody holds an mcp_env block, to one unit.
func grantCompany(t *testing.T) *Company {
	t.Helper()
	return specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - {name: search, command: search-server, access: {seats: [ceo]}, tools: {default: allow}}
  - {name: tracker, command: tracker-server, shared: false, access: {units: [eng]}, tools: {default: allow}}
  - {name: notes, command: notes-server, tools: {default: allow}}
roles:
  - {name: CEO, handle: ceo, llm: zulu}
units:
  - name: Eng
    id: eng
    roles:
      - {name: Dev, handle: dev, llm: zulu}
`)
}

// A SHARED SERVER'S TOOLS REACH ONLY THE SEATS IT IS GRANTED TO. Its one
// instance files them into the epoch's registry, and every surface a seat's
// turn is built against is cut from that registry for the seat: a held seat's
// and the fallback alike. A server with no access reaches every agent.
//
// Mutation: make grantedSurface return c.Tools.Clone() unfiltered, and this is
// red.
func TestASharedServersToolsReachOnlyTheSeatsItIsGrantedTo(t *testing.T) {
	t.Parallel()
	company := grantCompany(t)
	for _, tool := range []struct{ name, server string }{
		{"search_web", "search"}, {"notes_read", "notes"},
	} {
		if err := company.Tools.Register(namedTool(tool.name), tools.Origin(tool.server)); err != nil {
			t.Fatal(err)
		}
	}
	if err := company.Tools.Register(namedTool("lookup_colleague"), tools.OriginBuiltin); err != nil {
		t.Fatal(err)
	}
	e := &Engine{}
	e.epoch.current.Store(company)
	for handle, want := range map[string][]string{
		"ceo": {"lookup_colleague", "notes_read", "search_web"},
		"dev": {"lookup_colleague", "notes_read"},
	} {
		got := e.ToolsFor(handle).Names()
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s holds %v, want %v", handle, got, want)
		}
		held := seatSurface(t.Context(), company, &seatChildren{handle: handle, bridge: mcp.NewBridge(nil, mcp.Supervision{})})
		if got := held.Names(); !slices.Equal(slices.Sorted(slices.Values(got)), want) {
			t.Errorf("held %s's surface holds %v, want %v", handle, got, want)
		}
	}
	if _, ok := company.Tools.Lookup("search_web"); !ok {
		t.Error("the epoch's registry lost the shared tool: a seat's cut must be its own copy")
	}
}

// A PER-AGENT SERVER STARTS A CHILD FOR EACH SEAT ITS GRANT REACHES, and for
// nobody else, though no seat declares an mcp_env block for it.
func TestAPerAgentServerStartsForTheSeatsItsGrantReaches(t *testing.T) {
	t.Parallel()
	company := grantCompany(t)
	for handle, want := range map[string][]string{
		"dev": {mcp.InstanceName("tracker", "Dev")},
		"ceo": nil,
	} {
		specs, outcomes := seatSpecs(company, company.Org.AgentSeatByHandle(handle), config.EnvOnly(), nil)
		var got []string
		for _, s := range specs {
			got = append(got, s.Name)
		}
		if !slices.Equal(got, want) || len(outcomes) != 0 {
			t.Errorf("%s's children are %v (outcomes %+v), want %v", handle, got, outcomes, want)
		}
	}
}

// A BOX IS GIVEN ONLY A SERVER ITS SEAT IS GRANTED. Admission refuses the
// listing, so a stored revision is what carries one: the box goes without it.
func TestABoxIsGivenOnlyAServerItsSeatIsGranted(t *testing.T) {
	t.Parallel()
	company := grantCompany(t)
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"search", "notes"}}}
	for handle, want := range map[string][]string{
		"ceo": {"notes", "search"},
		"dev": {"notes"},
	} {
		got, withheld := sandboxMCP(config.EnvOnly(), company, company.Org.AgentSeatByHandle(handle), gate, nil)
		names := slices.Sorted(maps.Keys(got))
		if !slices.Equal(names, want) || len(withheld) != 0 {
			t.Errorf("%s's box is given %v (withheld %+v), want %v", handle, names, withheld, want)
		}
	}
}
