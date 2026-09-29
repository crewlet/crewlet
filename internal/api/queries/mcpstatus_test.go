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
  - name: linear
    transport: http
    url: https://mcp.example.com
    headers:
      Authorization: Bearer literal-linear-secret
`

// presentNode puts one node on the lease table with the heartbeat a node
// running this build publishes, or — with no status — an older build's.
func presentNode(t *testing.T, backend coord.Backend, id string, status *coord.NodeStatus) {
	t.Helper()
	meta := map[string]any{"roles": []string{"seats"}}
	if status != nil {
		meta[coord.StatusKey] = status.Meta()
	}
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
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
	return queries.Sources{Coord: backend, Company: func() *config.Company { return cfg }}
}

// mcpFleet is two upgraded nodes and one older one: github started on both
// upgraded nodes and failed for one seat on node-b; docs started on node-a
// alone; linear started nowhere.
func mcpFleet(t *testing.T) coord.Backend {
	t.Helper()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		Features: []coord.Feature{coord.FeatureMCPStatus},
		MCP: []coord.MCPServerStatus{
			{Server: "docs", Shared: true, Started: 1, Tools: 4},
			{Server: "github", Started: 2, Tools: 26},
		},
	})
	presentNode(t, backend, "node-b", &coord.NodeStatus{
		Features: []coord.Feature{coord.FeatureMCPStatus},
		MCP: []coord.MCPServerStatus{
			{Server: "github", Started: 1, Failed: 1, Tools: 25, Error: "401 Bad credentials", ErrorSeat: "backend-dev"},
		},
	})
	presentNode(t, backend, "node-c", &coord.NodeStatus{InFlight: 1})
	return backend
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

// AN OLDER NODE'S CELLS ARE UNKNOWN, NEVER ZERO. It publishes no report, and a
// row of zeros under its name would read as "started nothing here" — the
// confident zero the fleet answer is written to avoid.
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
	presentNode(t, backend, "node-c", &coord.NodeStatus{InFlight: 1})
	presentNode(t, backend, "node-d", nil)
	for _, row := range askMCPStatus(t, mcpSources(t, backend)).Servers {
		if row.State != queries.MCPUnreported {
			t.Errorf("%s = %s in a fleet with no report, want unreported", row.Name, row.State)
		}
	}
}

// A FAILURE WITH NOTHING STARTED IS FAILING, and it is told apart from one
// seat's bad credentials on an otherwise working server.
func TestAServerThatStartedNowhereIsFailing(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		Features: []coord.Feature{coord.FeatureMCPStatus},
		MCP:      []coord.MCPServerStatus{{Server: "docs", Shared: true, Failed: 1, Error: "exec: docs-mcp: not found"}},
	})
	if docs := serverRow(t, askMCPStatus(t, mcpSources(t, backend)), "docs"); docs.State != queries.MCPFailing {
		t.Errorf("docs = %s, want failing", docs.State)
	}
}

// A SERVER ONLY A NODE KNOWS STAYS VISIBLE: a node on an older revision still
// runs a server this node's configuration removed, and that is the one row an
// operator who just removed it is looking for.
func TestAServerOnlyANodeReportsIsListedAsUnconfigured(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{
		Features: []coord.Feature{coord.FeatureMCPStatus},
		MCP:      []coord.MCPServerStatus{{Server: "retired", Shared: true, Started: 1, Tools: 3}},
	})
	row := serverRow(t, askMCPStatus(t, mcpSources(t, backend)), "retired")
	if row.Configured || row.State != queries.MCPRunning || !row.Shared {
		t.Errorf("retired = %+v, want a running server the configuration does not carry", row)
	}
}

// NO CREDENTIAL REACHES THE ANSWER: `env`, `headers` and a seat's `mcp_env`
// are what a server authenticates with, and the launch is shown without them.
func TestTheServerStatusCarriesNoCredential(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, mcpSources(t, mcpFleet(t)))
	raw, err := r.Answer(t.Context(), "mcp_servers_status", nil, "founder")
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

// OPERATOR-ONLY: node ids, launch commands and each failure's first line are
// the shape of the deployment. And an unreadable lease table is an error, not
// a fleet with no nodes.
func TestTheServerStatusIsOperatorOnlyAndNeverGuesses(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, mcpSources(t, mcpFleet(t)))
	if !r.RequiresOperator("mcp_servers_status") {
		t.Fatal("mcp_servers_status is served to any caller")
	}
	if _, err := r.Answer(t.Context(), "mcp_servers_status", nil, ""); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("anonymous mcp_servers_status answered %v, want an authorization refusal", err)
	}

	broken := queries.NewRegistry()
	queries.Register(broken, queries.Sources{Coord: unreadableLeases{coordmemory.New()}})
	if _, err := broken.Answer(t.Context(), "mcp_servers_status", nil, "founder"); err == nil {
		t.Error("an unreadable lease table was answered as a fleet")
	}
}

// THE TOOLS SCREEN READS WHAT THIS ANSWER SENDS, every shape held both ways,
// and knows exactly the states the engine sends.
func TestTheToolsScreenReadsWhatTheServerStatusSends(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, mcpSources(t, mcpFleet(t)))
	raw, err := r.Answer(t.Context(), "mcp_servers_status", nil, "founder")
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
