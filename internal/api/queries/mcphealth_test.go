package queries_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/mcp"
)

// fakeInstances answers every read with read, and records which nodes each
// server's read asked.
type fakeInstances struct {
	read coord.MCPInstanceRead

	mu    sync.Mutex
	asked map[string][]string
}

func (f *fakeInstances) MCPInstances(_ context.Context, server string, nodes []string) (coord.MCPInstanceRead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.asked == nil {
		f.asked = map[string][]string{}
	}
	f.asked[server] = slices.Clone(nodes)
	return f.read, nil
}

func (f *fakeInstances) nodesAsked(server string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[server]
}

// INSTANCE DETAIL FANS OUT TO EVERY NODE HOLDING AN INSTANCE: a server's
// detail asks each node whose last beat reports an instance of it, and every
// node whose beat carried no report (which may hold one nobody has heard of),
// and no other. Each instance comes back named by its seat's handle, a class
// this build does not name reads as none, and the server's last answer and last
// failure are the latest across its instances.
//
// Mutation: ask every live node whatever it reports, skip the node that did not
// report, or take the first instance's failure as the server's, and this is red.
func TestInstanceDetailFansOutToEveryNodeHoldingAnInstance(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	surface, _ := configSurface(t, mcpCompany)
	cfg, err := config.ParseCompany([]byte(mcpCompany))
	if err != nil {
		t.Fatal(err)
	}
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	backendDev, _ := organization.AgentIDFor(organization.AgentSeatByHandle("backend-dev"))
	early := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	instances := &fakeInstances{read: coord.MCPInstanceRead{
		Instances: []coord.MCPInstance{
			{Server: "github", Seat: backendDev, Node: "node-a", State: coord.MCPInstanceRunning, Since: early,
				Restarts: 2, Tools: 26, LastAnswer: late,
				LastFailure: &coord.MCPInstanceFailure{Class: string(mcp.FailExited), Said: "exited", At: early}},
			{Server: "github", Seat: backendDev, Node: "node-b", State: coord.MCPInstanceFailed, Since: late,
				Failure:     &coord.MCPInstanceFailure{Class: "quantum", Said: "a class from a newer build", At: late},
				LastFailure: &coord.MCPInstanceFailure{Class: "quantum", Said: "a class from a newer build", At: late},
				LastAnswer:  early},
		},
		Nodes: []coord.MCPInstanceNode{
			{Node: "node-a", Answered: true, Count: 1},
			{Node: "node-b", Answered: true, Count: 1},
			{Node: "node-c", Reason: "did not answer within 2s"},
		},
	}}
	s := mcpSources(t, backend)
	s.Config, s.MCPListings, s.MCPInstances = surface, fakeListings{}, instances

	for server, want := range map[string][]string{
		"github": {"node-a", "node-b", "node-c"},
		"docs":   {"node-a", "node-c"},
		"linear": {"node-c"},
	} {
		if _, err := s.MCPServer(t.Context(), server); err != nil {
			t.Fatalf("MCPServer(%s): %v", server, err)
		}
		if got := instances.nodesAsked(server); !slices.Equal(got, want) {
			t.Errorf("%s's detail asked %v, want %v: the nodes reporting an instance and the "+
				"node that did not report", server, got, want)
		}
	}

	got, err := s.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Instances) != 2 || len(got.InstanceNodes) != 3 {
		t.Fatalf("instances %+v, nodes %+v", got.Instances, got.InstanceNodes)
	}
	running, failed := got.Instances[0], got.Instances[1]
	if running.Seat != "backend-dev" || running.Node != "node-a" || running.Restarts != 2 ||
		running.Failure != nil || running.LastFailure == nil || running.LastFailure.Class != mcp.FailExited ||
		running.LastFailure.Seat != "backend-dev" || running.LastFailure.Node != "node-a" {
		t.Errorf("the running instance = %+v", running)
	}
	if failed.Failure == nil || failed.Failure.Class != "" || failed.Failure.Said != "a class from a newer build" {
		t.Errorf("a failure classed by a newer build = %+v, want its sentence and no class", failed.Failure)
	}
	if c := got.InstanceNodes[2]; c.Node != "node-c" || c.Answered || c.Reason == "" {
		t.Errorf("the node that did not answer = %+v", c)
	}
	if got.LastSuccess == nil || !got.LastSuccess.Equal(late) {
		t.Errorf("last success = %v, want %s, the latest answer of any instance", got.LastSuccess, late)
	}
	if got.LastFailure == nil || !got.LastFailure.At.Equal(late) || got.LastFailure.Node != "node-b" {
		t.Errorf("last failure = %+v, want node-b's, the latest", got.LastFailure)
	}

	none := s
	none.MCPInstances = nil
	quiet, err := none.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(quiet)
	if !strings.Contains(string(raw), `"instances":[]`) || !strings.Contains(string(raw), `"instance_nodes":[]`) ||
		!strings.Contains(string(raw), `"last_success":null`) {
		t.Errorf("a detail with no instance reader sends %s; want empty lists and nulls", raw)
	}
}

// THE STATUS ROW'S NODE CELL SAYS SINCE WHEN THE SERVER HAS BEEN DOWN THERE, WHY
// IN A CLASS, AND HOW OFTEN IT CAME BACK, off the node's beat: a class this
// build does not name reads as none, and a node that has no outage reads null.
func TestTheStatusCellCarriesTheOutageItsClassAndRestarts(t *testing.T) {
	t.Parallel()
	down := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "docs", Shared: true, Failed: 1, Error: "would not start", ErrorClass: "dns",
			DownSince: down, Restarts: 3},
	}})
	presentNode(t, backend, "node-b", &coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "docs", Shared: true, Failed: 1, Error: "would not start", ErrorClass: "quantum"},
	}})
	presentNode(t, backend, "node-c", &coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "docs", Shared: true, Started: 1, Tools: 4},
	}})
	status := answer(t, mcpSources(t, backend), "mcp_servers_status", nil).(queries.MCPStatusAnswer)
	i := slices.IndexFunc(status.Servers, func(r queries.MCPServerRow) bool { return r.Name == "docs" })
	if i < 0 {
		t.Fatal("no docs row")
	}
	cells := status.Servers[i].Nodes
	if a := cells[0]; a.ErrorClass != mcp.FailDNS || a.DownSince == nil || !a.DownSince.Equal(down) || a.Restarts != 3 {
		t.Errorf("node-a's cell = %+v, want dns, down since %s, three restarts", a, down)
	}
	if b := cells[1]; b.ErrorClass != "" {
		t.Errorf("a class this build does not name reads %q, want none", b.ErrorClass)
	}
	if c := cells[2]; c.DownSince != nil || c.ErrorClass != "" || c.Restarts != 0 {
		t.Errorf("a serving node's cell = %+v, want no outage", c)
	}
}

// A SERVER'S ADDRESS IS ANSWERED WITH ITS CREDENTIALS WITHHELD, in the status
// row and so in the detail that answers it: a vendor that takes a key in its
// query string has it shown to nobody.
func TestAServersAddressIsAnsweredWithItsCredentialsWithheld(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
mcp_servers:
  - name: search
    transport: http
    url: https://mcp.example.com/mcp?api_key=literal-key-never-sent&region=eu
`
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	surface, _ := configSurface(t, doc)
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "search", Shared: true, Started: 1, Tools: 2},
	}})
	s := queries.Sources{Coord: backend, Company: companySource(t, cfg), Config: surface,
		MCPListings: fakeListings{}}
	detail, err := s.MCPServer(t.Context(), "search")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(detail)
	if strings.Contains(string(raw), "literal-key-never-sent") {
		t.Errorf("the detail carries the key: %s", raw)
	}
	if detail.Server.URL != "https://mcp.example.com/mcp?api_key=redacted&region=eu" {
		t.Errorf("the address reads %q", detail.Server.URL)
	}
}

// THE FACTS THE ENGINE RESOLVES ABOUT A SERVER ARE ON ITS DETAIL, decided once
// so a screen never re-derives them: how it signs in (an integration's
// per-agent credential, configured values, or nothing), where a local one
// runs, the timeouts in force with the defaults applied, the boxes it is given
// with each box's cell, and the placements expected to fail: a local server in
// a box that runs its own image, and a server on a private network in a cloud
// box. A server the configuration does not declare has nothing resolved.
//
// Mutation: judge a private address with a rule of its own, leave the
// default timeouts unapplied, or warn about a box on the engine host, and
// this is red.
func TestTheEngineResolvesAServersFactsOnItsDetail(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  sandbox:
    local: {image: "ghcr.io/example/box:1"}
    e2b: {api_key: "${E2B_API_KEY}"}
    default_run_in: container
  llm:
    main: {model: sonnet}
    cloud: {model: sonnet, run_in: e2b}
    host: {model: sonnet, run_in: direct}
mcp_servers:
  - name: docs
    command: docs-mcp
    startup_timeout_seconds: 30
    env:
      DOCS_TOKEN: "${DOCS_TOKEN}"
  - name: intranet
    transport: http
    url: http://10.0.0.5:8080/mcp
  - name: github
    shared: false
    command: github-mcp-server
    args: ["stdio"]
roles:
  - name: SWE
    handle: swe
    llm: main
    sandbox: {mcp: {servers: [docs]}}
  - name: Researcher
    handle: researcher
    llm: cloud
    sandbox: {mcp: {servers: [intranet, docs]}}
  - name: Operator
    handle: operator
    llm: host
    sandbox: {mcp: {servers: [docs, intranet]}}
`
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	surface, _ := configSurface(t, doc)
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{MCP: []coord.MCPServerStatus{}})
	s := queries.Sources{Coord: backend, Company: companySource(t, cfg), Config: surface,
		MCPListings: fakeListings{}}
	read := func(name string) *queries.MCPResolved {
		t.Helper()
		got, err := s.MCPServer(t.Context(), name)
		if err != nil {
			t.Fatalf("MCPServer(%s): %v", name, err)
		}
		if got.Resolved == nil {
			t.Fatalf("%s has nothing resolved", name)
		}
		return got.Resolved
	}

	docs := read("docs")
	if docs.Auth != queries.MCPAuthHeaders || docs.RunsOn != queries.MCPRunsOnEngineHost || docs.Binding != nil {
		t.Errorf("docs = %+v, want configured values, on the engine host, no binding", docs)
	}
	if docs.Timeouts != (queries.MCPTimeouts{StartupSeconds: 30, RequestSeconds: mcp.DefaultRequestTimeout.Seconds(),
		RequestDefault: true}) {
		t.Errorf("docs' timeouts = %+v, want its own startup and the default per call", docs.Timeouts)
	}
	wantBoxes := []queries.MCPBoxSeat{{Seat: "swe", Cell: "container"}, {Seat: "researcher", Cell: "e2b"},
		{Seat: "operator", Cell: "direct"}}
	if !slices.Equal(docs.BoxSeats, wantBoxes) {
		t.Errorf("docs' boxes = %+v, want %+v", docs.BoxSeats, wantBoxes)
	}
	var kinds []string
	for _, w := range docs.Warnings {
		kinds = append(kinds, string(w.Kind)+"@"+w.Seat)
		if !strings.Contains(w.Message, "docs-mcp") {
			t.Errorf("a command warning that names no command: %q", w.Message)
		}
	}
	if want := []string{"command_in_box@swe", "command_in_box@researcher"}; !slices.Equal(kinds, want) {
		t.Errorf("docs' warnings = %v, want %v: a box on the engine host runs the host's command", kinds, want)
	}

	intranet := read("intranet")
	if intranet.Auth != queries.MCPAuthNone || intranet.RunsOn != "" ||
		intranet.Timeouts != (queries.MCPTimeouts{StartupSeconds: mcp.DefaultStartupTimeout.Seconds(),
			RequestSeconds: mcp.DefaultRequestTimeout.Seconds(), StartupDefault: true, RequestDefault: true}) {
		t.Errorf("intranet = %+v, want no credential, no host, the defaults", intranet)
	}
	if len(intranet.Warnings) != 1 || intranet.Warnings[0].Kind != queries.MCPWarnPrivateAddressInCloudBox ||
		intranet.Warnings[0].Seat != "researcher" || intranet.Warnings[0].Cell != "e2b" {
		t.Errorf("intranet's warnings = %+v, want the researcher's cloud box alone", intranet.Warnings)
	}

	github := read("github")
	if github.Auth != queries.MCPAuthIntegration || github.Binding == nil ||
		github.Binding.Vendor != "github" || github.Binding.By != queries.MCPBindingRecognised ||
		len(github.Binding.Keys) == 0 || github.Binding.Keys[0].Key == "" {
		t.Errorf("github = %+v (binding %+v), want github's recognised binding", github, github.Binding)
	}
	if len(github.BoxSeats) != 0 || len(github.Warnings) != 0 {
		t.Errorf("github is given to boxes nobody named: %+v", github)
	}
}
