package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// mcpCompany declares a shared server, a per-seat template and a server no
// node has started, with credentials on the template that must never reach
// the answer.
const mcpCompany = `
name: Acme
roles:
  - name: Backend Dev
    handle: backend-dev
    mcp_env:
      github:
        GITHUB_TOKEN: ghp_literal_never_sent
mcp_servers:
  - name: docs
    command: docs-mcp
    args: ["--read-only"]
    env:
      DOCS_TOKEN: literal-docs-secret
  - name: github
    shared: false
    command: npx
    args: ["-y", "@modelcontextprotocol/server-github"]
    tool_prefix: gh_
  - name: linear
    transport: http
    description: Reads and files Linear issues
    url: https://mcp.example.com
    headers:
      Authorization: Bearer literal-linear-secret
`

// presentNode puts one node on the lease table with the heartbeat it
// publishes, or — with no status — the placement half alone, which is what a
// beat whose status hook did not answer within its share of the beat carries.
func presentNode(t *testing.T, backend coord.Backend, id string, status *coord.NodeStatus) {
	t.Helper()
	meta := map[string]any{"roles": []string{"seats"}}
	if status != nil {
		meta[coord.StatusKey] = status.Meta()
	}
	if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
		Owner: id + ":1", TTL: time.Minute, Meta: meta,
	}); err != nil {
		t.Fatal(err)
	}
}

func mcpSources(t *testing.T, backend coord.Backend) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	return queries.Sources{
		Coord: backend, Company: companySource(t, cfg),
		Builtins: func() []queries.ToolRow { return catalogueBuiltins() },
	}
}

// mcpFleet is two reporting nodes and one whose last heartbeat carried no
// status: github started on both reporting nodes and failed for one seat on
// node-b; docs started on node-a alone; linear started nowhere.
//
// A NODE REPORTS THE FAILING SEAT BY ITS ID, and the answer names it by its
// handle.
func mcpFleet(t *testing.T) coord.Backend {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	backendDev, ok := organization.AgentIDFor(organization.AgentSeatByHandle("backend-dev"))
	if !ok {
		t.Fatal("the fixture has no backend-dev seat")
	}
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{
			{Server: "docs", Shared: true, Started: 1, Tools: 4,
				Shadowed: []coord.MCPToolShadow{{Tool: "search", By: "linear"}}, ShadowedCount: 1},
			{Server: "github", Started: 2, Tools: 26,
				Refused: []coord.MCPToolRefusal{
					{Tool: longGitHubTool, Reason: "too_long"},
					{Tool: "gh_read.file", Reason: "characters"},
				}, RefusedCount: 12},
		},
	})
	presentNode(t, backend, "node-b", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{
			{Server: "github", Started: 1, Failed: 1, Tools: 25, Error: "401 Bad credentials", ErrorSeat: backendDev,
				Refused: []coord.MCPToolRefusal{
					{Tool: "gh_lookup_colleague", Reason: "builtin"},
					{Tool: "gh_read.file", Reason: "characters"},
				}, RefusedCount: 3},
		},
	})
	presentNode(t, backend, "node-c", nil)
	return backend
}

// longGitHubTool is a name github's tool_prefix pushes one character over
// the longest a coding CLI can hand its model.
var longGitHubTool = "gh_" + strings.Repeat("x", cliprofile.ToolNameMax()-2)

// A SERVER'S TOOLS LOST TO THEIR NAMES ARE ONE LIST ACROSS THE FLEET, each
// tool once however many nodes name it, sorted, each with the engine's
// sentence worded for the server's own prefix, and counted as the node
// counting the most counts them: a node names at most eight. A server whose
// tools are all offered sends two empty lists and two zeros.
//
// Mutation: keep one node's list, sum the counts, or word a sentence without
// the configured prefix, and this is red.
func TestAServersToolsLostToTheirNamesAreOneListAcrossTheFleet(t *testing.T) {
	t.Parallel()
	got := askMCPStatus(t, mcpSources(t, mcpFleet(t)))

	github := serverRow(t, got, "github")
	refused := []queries.MCPRefusedTool{
		{Tool: longGitHubTool, Reason: mcp.NameTooLong},
		{Tool: "gh_lookup_colleague", Reason: mcp.NameBuiltin},
		{Tool: "gh_read.file", Reason: mcp.NameCharacters},
	}
	slices.SortFunc(refused, func(a, b queries.MCPRefusedTool) int { return strings.Compare(a.Tool, b.Tool) })
	for i := range refused {
		refused[i].Message = refused[i].Reason.Sentence(refused[i].Tool, "gh_")
	}
	if !slices.Equal(github.RefusedTools, refused) || github.RefusedCount != 12 {
		t.Errorf("github refused %+v of %d, want %+v of 12", github.RefusedTools, github.RefusedCount, refused)
	}
	if i := slices.IndexFunc(github.RefusedTools, func(r queries.MCPRefusedTool) bool { return r.Tool == longGitHubTool }); i < 0 ||
		!strings.Contains(github.RefusedTools[i].Message, `Shorten the tool_prefix "gh_" by 1`) {
		t.Errorf("the long name's sentence does not say how much to shorten the prefix: %+v", github.RefusedTools)
	}
	if len(github.ShadowedTools) != 0 || github.ShadowedCount != 0 {
		t.Errorf("github shadowed %+v of %d, want none", github.ShadowedTools, github.ShadowedCount)
	}

	docs := serverRow(t, got, "docs")
	shadowed := []queries.MCPShadowedTool{{Tool: "search", By: "linear", Message: mcp.ShadowSentence("search", "linear")}}
	if !slices.Equal(docs.ShadowedTools, shadowed) || docs.ShadowedCount != 1 {
		t.Errorf("docs shadowed %+v of %d, want %+v of 1", docs.ShadowedTools, docs.ShadowedCount, shadowed)
	}

	linear := serverRow(t, got, "linear")
	if linear.RefusedTools == nil || linear.ShadowedTools == nil || len(linear.RefusedTools)+len(linear.ShadowedTools) != 0 ||
		linear.RefusedCount+linear.ShadowedCount != 0 {
		t.Errorf("linear = %+v, want two empty lists and two zeros", linear)
	}
}

func askMCPStatus(t *testing.T, s queries.Sources) queries.MCPStatusAnswer {
	t.Helper()
	got, ok := answer(t, s, "mcp_servers_status", nil).(queries.MCPStatusAnswer)
	if !ok {
		t.Fatalf("mcp_servers_status answered %T", got)
	}
	return got
}

func serverRow(t *testing.T, a queries.MCPStatusAnswer, name string) queries.MCPServerRow {
	t.Helper()
	i := slices.IndexFunc(a.Servers, func(r queries.MCPServerRow) bool { return r.Name == name })
	if i < 0 {
		t.Fatalf("no row for %s in %+v", name, a.Servers)
	}
	return a.Servers[i]
}

// EVERY SERVER IS ONE ROW WITH ONE CELL PER LIVE NODE, and the state is the
// engine's reading of the sums — the screen draws it and derives nothing.
func TestEachServerIsJudgedAcrossEveryNodesReport(t *testing.T) {
	t.Parallel()
	got := askMCPStatus(t, mcpSources(t, mcpFleet(t)))

	if want := []queries.MCPStatusNode{
		{ID: "node-a", Reported: true}, {ID: "node-b", Reported: true}, {ID: "node-c", Reported: false},
	}; !slices.Equal(got.Nodes, want) {
		t.Fatalf("nodes = %+v, want %+v", got.Nodes, want)
	}

	github := serverRow(t, got, "github")
	if github.State != queries.MCPPartial || github.Started != 3 || github.Failed != 1 || github.Tools != 26 {
		t.Errorf("github = %+v, want partial, 3 started, 1 failed, 26 tools", github)
	}
	if github.Shared || !github.Configured || github.Command != "npx" {
		t.Errorf("github's launch = %+v, want the configured per-seat template", github)
	}
	if len(github.Nodes) != 3 {
		t.Fatalf("github has %d cells, want one per live node", len(github.Nodes))
	}
	b := github.Nodes[1]
	if b.Node != "node-b" || b.Error != "401 Bad credentials" || b.ErrorSeat != "backend-dev" {
		t.Errorf("node-b's cell = %+v, want its failure and the seat it was for", b)
	}

	if docs := serverRow(t, got, "docs"); docs.State != queries.MCPRunning || !docs.Shared {
		t.Errorf("docs = %+v, want a running shared server", docs)
	}
	if linear := serverRow(t, got, "linear"); linear.State != queries.MCPNotStarted || linear.Transport != "http" ||
		linear.URL != "https://mcp.example.com" {
		t.Errorf("linear = %+v, want a configured http server nothing started", linear)
	}
}

// A SILENT NODE'S CELLS ARE UNKNOWN, NEVER ZERO. Its last heartbeat carried no
// status (the status hook did not answer within the beat), and a row of zeros
// under its name would read as "started nothing here" — the confident zero the
// fleet answer is written to avoid. A node that published a status naming no
// server DID say: it started none.
func TestANodeThatDoesNotReportIsUnknownNotIdle(t *testing.T) {
	t.Parallel()
	got := askMCPStatus(t, mcpSources(t, mcpFleet(t)))
	for _, row := range got.Servers {
		c := row.Nodes[2]
		if c.Node != "node-c" || c.Reported {
			t.Errorf("%s: node-c's cell = %+v, want an unreported cell", row.Name, c)
		}
	}

	// And a fleet where NOBODY reports says so, rather than "not started".
	backend := coordmemory.New()
	presentNode(t, backend, "node-c", nil)
	presentNode(t, backend, "node-d", nil)
	for _, row := range askMCPStatus(t, mcpSources(t, backend)).Servers {
		if row.State != queries.MCPUnreported {
			t.Errorf("%s = %s in a fleet with no report, want unreported", row.Name, row.State)
		}
	}

	// THE CONTROL: a status naming no server is a node that started none.
	backend = coordmemory.New()
	presentNode(t, backend, "node-c", &coord.NodeStatus{InFlight: 1})
	for _, row := range askMCPStatus(t, mcpSources(t, backend)).Servers {
		if row.State != queries.MCPNotStarted {
			t.Errorf("%s = %s beside a node that reported starting nothing, want not_started",
				row.Name, row.State)
		}
	}
}

// A PUBLISHED STATUS WITH NO SERVERS IS "STARTED NONE", not "did not say": a
// node reports what it started beside every status it publishes, so a status
// carrying no rows is a node that started nothing — a reported column, and a
// server nobody started is `not_started` rather than unknown.
func TestAStatusWithNoServersStartedNone(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{InFlight: 1})
	got := askMCPStatus(t, mcpSources(t, backend))
	if want := []queries.MCPStatusNode{{ID: "node-a", Reported: true}}; !slices.Equal(got.Nodes, want) {
		t.Fatalf("nodes = %+v, want %+v", got.Nodes, want)
	}
	for _, row := range got.Servers {
		if row.State != queries.MCPNotStarted {
			t.Errorf("%s = %s on a node that started none, want not_started", row.Name, row.State)
		}
	}
}

// A FAILURE WITH NOTHING STARTED IS FAILING, and it is told apart from one
// seat's bad credentials on an otherwise working server.
func TestAServerThatStartedNowhereIsFailing(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{{Server: "docs", Shared: true, Failed: 1, Error: "exec: docs-mcp: not found"}},
	})
	if docs := serverRow(t, askMCPStatus(t, mcpSources(t, backend)), "docs"); docs.State != queries.MCPFailing {
		t.Errorf("docs = %s, want failing", docs.State)
	}
}

// A SEAT'S INSTANCE HELD BACK FOR ITS PROVISIONED CREDENTIAL IS WAITING, NOT
// FAILING, and says what it waits for.
//
// On a first boot under provisioning every seat's instance waits for the
// credential the pass mints, and an operator watching the tools screen while
// they connect an integration must not read that as a fault: nothing was
// launched and nothing failed. The waiting count is its own, its sentence and
// seat reach the cell as a failure's do, and it decides the state only where
// nothing else does — a server running for one seat is running, and one with
// a failure is failing whatever waits beside it.
func TestAServerWaitingForAProvisionedCredentialIsNotFailing(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	backendDev, _ := organization.AgentIDFor(organization.AgentSeatByHandle("backend-dev"))
	const wait = "not started: waiting for the gitlab integration to mint ${GITLAB_TOKEN_BACKEND_DEV}; " +
		"it starts once the seal reaches this node"

	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{{Server: "github", Waiting: 1, Wait: wait, WaitSeat: backendDev}},
	})
	github := serverRow(t, askMCPStatus(t, mcpSources(t, backend)), "github")
	if github.State != queries.MCPWaiting || github.Waiting != 1 || github.Failed != 0 {
		t.Errorf("github = %+v, want waiting, and not failing", github)
	}
	if cell := github.Nodes[0]; cell.Waiting != 1 || cell.Wait != wait || cell.WaitSeat != "backend-dev" ||
		cell.Error != "" {
		t.Errorf("node-a's cell = %+v, want the waiting instance, its sentence and its seat", cell)
	}

	for _, tc := range []struct {
		name   string
		status coord.MCPServerStatus
		want   queries.MCPServerState
	}{
		{"one seat served, another waiting", coord.MCPServerStatus{Server: "github", Started: 1, Waiting: 1}, queries.MCPRunning},
		{"one seat failed, another waiting", coord.MCPServerStatus{Server: "github", Failed: 1, Waiting: 1}, queries.MCPFailing},
	} {
		backend := coordmemory.New()
		presentNode(t, backend, "node-a", &coord.NodeStatus{MCP: []coord.MCPServerStatus{tc.status}})
		if got := serverRow(t, askMCPStatus(t, mcpSources(t, backend)), "github").State; got != tc.want {
			t.Errorf("%s: state = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// AN ANNOTATION KEY IS UNLISTED ONLY WHERE NO STARTED INSTANCE LISTS IT,
// FLEET-WIDE. Each node's row already names the keys none of its own
// instances lists; a key one node names and another's instances list is a
// key some seat's credentials reach — mcp-atlassian serves Confluence tools
// only to a seat holding a Confluence credential — so only the keys every
// node that started an instance names reach the answer. A node that started
// none listed nothing and does not judge, nor does one that did not report;
// a server started nowhere names nothing, never every key.
func TestAnAnnotationKeyIsUnlistedOnlyWhereNoStartedInstanceListsIt(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{
			{Server: "github", Started: 2, Tools: 26, Unlisted: []string{"create_pull_reqest", "merge_pull_request"}},
			{Server: "docs", Shared: true, Started: 1, Tools: 4, Unlisted: []string{"docs_search"}},
		},
	})
	presentNode(t, backend, "node-b", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{
			{Server: "github", Started: 1, Tools: 30, Unlisted: []string{"create_pull_reqest"}},
			{Server: "linear", Failed: 1, Error: "401", Unlisted: []string{"linear_typo"}},
		},
	})
	presentNode(t, backend, "node-c", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{
			{Server: "github", Failed: 2, Error: "401 Bad credentials"},
		},
	})
	presentNode(t, backend, "node-d", nil)
	got := askMCPStatus(t, mcpSources(t, backend))
	for name, want := range map[string][]string{
		"github": {"create_pull_reqest"},
		"docs":   {"docs_search"},
		"linear": {},
	} {
		if row := serverRow(t, got, name); row.UnlistedAnnotations == nil || !slices.Equal(row.UnlistedAnnotations, want) {
			t.Errorf("%s: unlisted_annotations = %#v, want %#v", name, row.UnlistedAnnotations, want)
		}
	}
}

// A SERVER ONLY A NODE KNOWS STAYS VISIBLE: a node on an older revision still
// runs a server this node's configuration removed, and that is the one row an
// operator who just removed it is looking for.
func TestAServerOnlyANodeReportsIsListedAsUnconfigured(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{{Server: "retired", Shared: true, Started: 1, Tools: 3}},
	})
	row := serverRow(t, askMCPStatus(t, mcpSources(t, backend)), "retired")
	if row.Configured || row.State != queries.MCPRunning || !row.Shared {
		t.Errorf("retired = %+v, want a running server the configuration does not carry", row)
	}
}

// A SERVER TURNED OFF IS DISABLED, decided before anything a node reports: a
// node still on the revision before it, running the server, shows that in its
// own cell, and the row reads as the configuration's decision rather than as a
// server that runs. One that runs says so, and each row carries the
// description its prompt line does.
func TestAServerTurnedOffIsDisabledWhateverANodeStillRuns(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(`
name: Acme
mcp_servers:
  - name: docs
    enabled: false
    description: Searches the engineering handbook
    command: docs-mcp
  - name: search
    command: search-mcp
`))
	if err != nil {
		t.Fatal(err)
	}
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		MCP: []coord.MCPServerStatus{
			{Server: "docs", Shared: true, Started: 1, Tools: 4},
			{Server: "search", Shared: true, Started: 1, Tools: 2},
		},
	})
	got := askMCPStatus(t, queries.Sources{Coord: backend, Company: companySource(t, cfg)})

	docs := serverRow(t, got, "docs")
	if docs.State != queries.MCPDisabled || docs.Enabled {
		t.Errorf("docs = %s, enabled %v; want disabled, whatever node-a still runs", docs.State, docs.Enabled)
	}
	if docs.Description != "Searches the engineering handbook" {
		t.Errorf("docs carries the description %q", docs.Description)
	}
	if cell := docs.Nodes[0]; cell.Started != 1 {
		t.Errorf("node-a's cell = %+v: the node still running it is not shown", cell)
	}
	if search := serverRow(t, got, "search"); search.State != queries.MCPRunning || !search.Enabled {
		t.Errorf("search = %s, enabled %v; want running and enabled", search.State, search.Enabled)
	}

	// ON A FLEET THAT HAS NOT SAID, the configuration still has: a server
	// turned off is disabled rather than unreported.
	silent := askMCPStatus(t, queries.Sources{Coord: coordmemory.New(), Company: companySource(t, cfg)})
	if docs := serverRow(t, silent, "docs"); docs.State != queries.MCPDisabled {
		t.Errorf("with no node reporting, docs = %s, want disabled", docs.State)
	}
}

// NO CREDENTIAL REACHES THE ANSWER: `env`, `headers` and a seat's `mcp_env`
// are what a server authenticates with, and the launch is shown without them.
func TestTheServerStatusCarriesNoCredential(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, mcpSources(t, mcpFleet(t)))
	raw, err := r.Answer(everyGrant(t), "mcp_servers_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := string(mustJSON(t, raw))
	for _, secret := range []string{"literal-docs-secret", "literal-linear-secret", "ghp_literal_never_sent", "DOCS_TOKEN", "Authorization"} {
		if strings.Contains(body, secret) {
			t.Errorf("the answer carries %q: %s", secret, body)
		}
	}
}

// ADMIN'S: launch commands and each failure's first line are the
// configuration and what became of it, which `/config` shows the same reader.
// And an unreadable lease table is an error, not a fleet with no nodes.
func TestTheServerStatusIsAdminsAndNeverGuesses(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, mcpSources(t, mcpFleet(t)))
	requiresExactly(t, r, "mcp_servers_status", iam.GrantAdmin)

	broken := queries.NewRegistry()
	queries.Register(broken, queries.Sources{Coord: unreadableLeases{coordmemory.New()}})
	if _, err := broken.Answer(everyGrant(t), "mcp_servers_status", nil); err == nil {
		t.Error("an unreadable lease table was answered as a fleet")
	}
}

// THE TOOLS SCREEN READS WHAT THIS ANSWER SENDS, every shape held both ways,
// and knows exactly the states the engine sends.
func TestTheToolsScreenReadsWhatTheServerStatusSends(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, mcpSources(t, mcpFleet(t)))
	raw, err := r.Answer(everyGrant(t), "mcp_servers_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := asMap(t, raw)
	holdShape(t, "McpServersStatusAnswer", []map[string]any{body}, false)
	holdShape(t, "McpStatusNode", rowsOf(t, body["nodes"]), false)
	servers := rowsOf(t, body["servers"])
	holdShape(t, "McpServerStatus", servers, false)
	var cells []map[string]any
	for _, s := range servers {
		cells = append(cells, rowsOf(t, s["nodes"])...)
	}
	holdShape(t, "McpServerNode", cells, false)
	holdShape(t, "BuiltinGroup", rowsOf(t, body["builtin"]), false)

	var refused, shadowed []map[string]any
	for _, s := range servers {
		refused = append(refused, rowsOf(t, s["refused_tools"])...)
		shadowed = append(shadowed, rowsOf(t, s["shadowed_tools"])...)
	}
	holdShape(t, "McpRefusedTool", refused, false)
	holdShape(t, "McpShadowedTool", shadowed, false)

	reasons, err := clientsource.Union(clientsource.Tree(t), "McpToolRefusal")
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the client's declaration", err)
	}
	slices.Sort(reasons)
	if want := slices.Sorted(slices.Values(stringsOf(mcp.NameRefusals))); !slices.Equal(reasons, want) {
		t.Errorf("the dashboard's McpToolRefusal is %v; the engine sends %v", reasons, want)
	}

	got, err := clientsource.Union(clientsource.Tree(t), "McpServerState")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the client's declaration", err)
	}
	slices.Sort(got)
	if want := slices.Sorted(slices.Values(stringsOf(queries.MCPServerStates))); !slices.Equal(got, want) {
		t.Errorf("the dashboard's McpServerState is %v; the engine sends %v", got, want)
	}
	for _, s := range queries.MCPServerStates {
		if !s.Valid() {
			t.Errorf("state %q is listed and not valid", s)
		}
	}
}

// unreadableLeases is a lease table whose listing fails, as a store blip
// makes it.
type unreadableLeases struct{ coord.Backend }

func (unreadableLeases) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
	return nil, errors.New("the coordination store did not answer")
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A SERVER IS DRAWN AS THE CATALOGUE ENTRY IT IS, by its address or its
// command, and by its own name with no mark where the catalogue knows it not.
func TestAServerRowNamesTheCatalogueEntryItIs(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(`
name: Acme
mcp_servers:
  - name: code
    transport: http
    shared: false
    url: https://api.githubcopilot.com/mcp/
  - name: db
    command: uvx
    args: ["postgres-mcp==0.3.0", "--access-mode=restricted"]
  - name: wiki
    transport: http
    url: https://wiki.example.com/mcp
`))
	if err != nil {
		t.Fatal(err)
	}
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", nil)
	answer := askMCPStatus(t, queries.Sources{Coord: backend, Company: companySource(t, cfg)})
	for _, want := range []queries.MCPServerRow{
		{Name: "code", Title: "GitHub", Catalogue: "github", Vendor: "github"},
		{Name: "db", Title: "PostgreSQL", Catalogue: "postgres", Vendor: "postgresql"},
		{Name: "wiki", Title: "wiki"},
	} {
		row := serverRow(t, answer, want.Name)
		if row.Title != want.Title || row.Catalogue != want.Catalogue || row.Vendor != want.Vendor {
			t.Errorf("%s is drawn as %q, %q, %q; want %q, %q, %q", want.Name, row.Title, row.Catalogue,
				row.Vendor, want.Title, want.Catalogue, want.Vendor)
		}
	}
}
