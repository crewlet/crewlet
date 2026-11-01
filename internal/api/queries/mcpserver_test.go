package queries_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/mcp"
)

// fakeListings is the fleet's listings as a node's watched copy holds them.
type fakeListings struct {
	listings []coord.MCPListing
	schemas  map[string]coord.MCPToolSchema
	err      error
}

func (f fakeListings) MCPListings(context.Context) ([]coord.MCPListing, error) {
	return f.listings, f.err
}

func (f fakeListings) MCPToolSchemas(_ context.Context, digests []string) (map[string]coord.MCPToolSchema, error) {
	out := map[string]coord.MCPToolSchema{}
	for _, d := range digests {
		if s, ok := f.schemas[d]; ok {
			out[d] = s
		}
	}
	return out, nil
}

func digestFor(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// epochs is each live node's presence epoch.
func epochs(t *testing.T, backend coord.Backend) map[string]int64 {
	t.Helper()
	leases, err := backend.ListLive(t.Context(), coord.ClassNode)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, l := range leases {
		node, _ := coord.NodeID(l.Resource)
		out[node] = l.Epoch
	}
	return out
}

// listed is one tool as a listing names it.
func listed(name, description string, hints coord.MCPToolHints, instances int) coord.MCPListedTool {
	return coord.MCPListedTool{Name: name, Raw: name[len("gh_"):], Description: description, Hints: hints,
		Digest: digestFor(name + description), SchemaKept: true, Instances: instances}
}

var (
	readOnly = coord.MCPToolHints{ReadOnly: coord.HintYes, Destructive: coord.HintNo,
		Idempotent: coord.HintYes, OpenWorld: coord.HintNo}
	unannotated = coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
		Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown}
)

// TWO NODES' LISTINGS OF A SERVER MERGE INTO ITS DETAIL, and only live ones:
// a per-agent server's children on node-a and node-b list what their seats
// reach, and the detail names every tool once, its instances and nodes summed
// across the fleet, the definition most instances list shown, its risk read
// once by the engine, its schemas attached; a node that drained and a node's
// previous incarnation are not counted; and what removing the server takes
// with it is the dry run's own answer.
//
// Mutation: count a listing whatever its node's presence, show the first
// definition met rather than the most listed, or leave the dependants out,
// and this is red.
func TestTwoNodesListingsOfAServerMergeIntoItsDetail(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	live := epochs(t, backend)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	getIssue := listed("gh_get_issue", "Read one issue", readOnly, 2)
	createIssue := listed("gh_create_issue", "Open an issue", unannotated, 1)
	createIssueNew := listed("gh_create_issue", "Open an issue, with labels", unannotated, 3)
	listings := []coord.MCPListing{
		{Server: "github", Node: "node-a", Epoch: live["node-a"], At: at, Instances: 2,
			Info:         &coord.MCPServerInfo{Name: "github-mcp", Version: "1.3.0", Protocol: "2026-07-28"},
			Instructions: "old words",
			Tools:        []coord.MCPListedTool{createIssue, getIssue}, ToolCount: 2},
		{Server: "github", Node: "node-b", Epoch: live["node-b"], At: at.Add(time.Minute), Instances: 3,
			Info:         &coord.MCPServerInfo{Name: "github-mcp", Title: "GitHub", Version: "1.4.0", Protocol: "2026-07-28"},
			Instructions: "Search before you create.",
			Tools:        []coord.MCPListedTool{createIssueNew}, ToolCount: 1},
		// A NODE THAT DRAINED, and node-a's previous incarnation: neither
		// counts, though each record is still there.
		{Server: "github", Node: "node-gone", Epoch: 4, At: at, Instances: 1,
			Tools: []coord.MCPListedTool{listed("gh_delete_repo", "Delete a repository", unannotated, 1)}, ToolCount: 1},
		{Server: "docs", Node: "node-a", Epoch: live["node-a"] + 1, At: at, Instances: 1,
			Tools: []coord.MCPListedTool{listed("gh_search", "Search", readOnly, 1)}, ToolCount: 1},
	}
	schemas := map[string]coord.MCPToolSchema{
		getIssue.Digest: {Digest: getIssue.Digest, Input: json.RawMessage(`{"type":"object","required":["number"]}`),
			Output: json.RawMessage(`{"type":"object"}`)},
		createIssueNew.Digest: {Digest: createIssueNew.Digest, Input: json.RawMessage(`{"type":"object"}`)},
	}
	surface, _ := configSurface(t, mcpCompany)
	s := mcpSources(t, backend)
	s.Config, s.MCPListings = surface, fakeListings{listings: listings, schemas: schemas}

	got, err := s.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if got.Server.Name != "github" || !got.Server.Configured || got.Server.Started != 3 {
		t.Errorf("the server's row = %+v, want github's row from mcp_servers_status", got.Server)
	}
	status := answer(t, s, "mcp_servers_status", nil).(queries.MCPStatusAnswer)
	if i := slices.IndexFunc(status.Servers, func(r queries.MCPServerRow) bool { return r.Name == "github" }); i < 0 ||
		!reflect.DeepEqual(got.Server, status.Servers[i]) || !reflect.DeepEqual(got.Nodes, status.Nodes) {
		t.Errorf("the detail's row is not the status answer's")
	}
	if want := []queries.MCPServerListing{
		{Node: "node-a", At: at, Instances: 2, Tools: 2},
		{Node: "node-b", At: at.Add(time.Minute), Instances: 3, Tools: 1},
	}; !reflect.DeepEqual(got.Listed, want) {
		t.Errorf("listed = %+v,\nwant %+v", got.Listed, want)
	}
	if got.ServerInfo == nil || got.ServerInfo.Version != "1.4.0" || got.Instructions != "Search before you create." {
		t.Errorf("identity = %+v, %q, want the listing changed last", got.ServerInfo, got.Instructions)
	}
	if len(got.Tools) != 2 || got.ToolCount != 2 {
		t.Fatalf("tools = %+v (count %d), want create_issue and get_issue, the drained node's left out",
			got.Tools, got.ToolCount)
	}
	create, read := got.Tools[0], got.Tools[1]
	if create.Name != "gh_create_issue" || create.Digest != createIssueNew.Digest ||
		create.Description != "Open an issue, with labels" || create.Definitions != 2 ||
		create.Instances != 4 || !slices.Equal(create.Nodes, []string{"node-a", "node-b"}) {
		t.Errorf("create_issue = %+v, want node-b's definition, listed by more instances, of two", create)
	}
	if create.Risk != mcp.RiskDestructive || create.RiskDeclared || !create.External ||
		create.Annotations.ReadOnly != "unknown" {
		t.Errorf("an unannotated tool reads %s (declared %v, external %v), want undeclared destructive",
			create.Risk, create.RiskDeclared, create.External)
	}
	if create.InputSchema["type"] != "object" || create.OutputSchema != nil {
		t.Errorf("create_issue's schemas = %v, %v", create.InputSchema, create.OutputSchema)
	}
	if read.Name != "gh_get_issue" || read.Raw != "get_issue" || read.Risk != mcp.RiskReadOnly ||
		!read.RiskDeclared || read.External || read.Instances != 2 || read.Definitions != 1 ||
		read.OutputSchema["type"] != "object" {
		t.Errorf("get_issue = %+v", read)
	}
	dependants, found, err := surface.ServerDependants(t.Context(), "github")
	if err != nil || !found {
		t.Fatalf("ServerDependants: %v %v", found, err)
	}
	if !reflect.DeepEqual(got.Dependants, dependants) {
		t.Errorf("dependants = %+v, want the dry run's %+v", got.Dependants, dependants)
	}
}

// THE DETAIL NEVER CARRIES A CREDENTIAL: the launch's literals, the seat's
// mcp_env values and a header's value stay out of every byte of it, whatever
// the listings say.
func TestAServersDetailNamesNoCredential(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	surface, _ := configSurface(t, mcpCompany)
	s := mcpSources(t, backend)
	s.Config, s.MCPListings = surface, fakeListings{}
	for _, server := range []string{"github", "docs", "linear"} {
		detail, err := s.MCPServer(t.Context(), server)
		if err != nil {
			t.Fatalf("MCPServer(%s): %v", server, err)
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"ghp_literal_never_sent", "literal-docs-secret", "literal-linear-secret"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s's detail carries %q", server, secret)
			}
		}
		var shape map[string]any
		if err := json.Unmarshal(raw, &shape); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"server", "nodes", "listed", "tools", "tool_count",
			"server_info", "instructions", "dependants"} {
			if _, ok := shape[key]; !ok {
				t.Errorf("%s's detail has no %q", server, key)
			}
		}
		if shape["listed"] == nil || shape["tools"] == nil {
			t.Errorf("%s's detail sends null for a list: %s", server, raw)
		}
	}
}

// A SERVER NOBODY CONFIGURES, REPORTS OR LISTS IS NOT FOUND, a name that is
// empty is the caller's to change, and a copy of the listings not yet read, or
// none at all, is an error rather than a server that lists nothing.
func TestAServersDetailRefusesWhatItCannotAnswer(t *testing.T) {
	t.Parallel()
	backend := mcpFleet(t)
	surface, _ := configSurface(t, mcpCompany)
	s := mcpSources(t, backend)
	s.Config, s.MCPListings = surface, fakeListings{}
	if _, err := s.MCPServer(t.Context(), "nothing"); !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("an unknown server answered %v, want not found", err)
	}
	if _, err := s.MCPServer(t.Context(), " "); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("an empty name answered %v, want bad params", err)
	}
	unread := s
	unread.MCPListings = fakeListings{err: errors.New("not yet read")}
	if _, err := unread.MCPServer(t.Context(), "github"); err == nil {
		t.Error("a copy not yet read answered as a server that lists nothing")
	}
	none := s
	none.MCPListings = nil
	if _, err := none.MCPServer(t.Context(), "github"); !errors.Is(err, queries.ErrMCPDetailUnavailable) {
		t.Errorf("a node with no copy answered %v", err)
	}
}

// WHAT REMOVING A SERVER TAKES WITH IT IS ON ITS DETAIL, as the dry run of the
// removal answers it: a seat's mcp_env block and a box entry naming it.
//
// Mutation: answer the detail's dependants from a walk of its own, or leave
// them out, and this is red.
func TestAServersDetailNamesWhatItsRemovalTakes(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
roles:
  - name: Backend Dev
    handle: backend-dev
    mcp_env:
      tracker:
        TRACKER_TOKEN: literal-tracker-token
mcp_servers:
  - name: tracker
    shared: false
    command: tracker-mcp
`
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	surface, _ := configSurface(t, doc)
	s := queries.Sources{Coord: coordmemory.New(), Company: companySource(t, cfg), Config: surface,
		MCPListings: fakeListings{}}
	got, err := s.MCPServer(t.Context(), "tracker")
	if err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if len(got.Dependants.MCPEnv) != 1 || got.Dependants.MCPEnv[0].Key != "backend-dev" ||
		got.Dependants.MCPEnv[0].Kind != configapi.MCPHolderSeat {
		t.Errorf("dependants = %+v, want backend-dev's mcp_env block", got.Dependants)
	}
	want, found, err := surface.ServerDependants(t.Context(), "tracker")
	if err != nil || !found || !reflect.DeepEqual(got.Dependants, want) {
		t.Errorf("the detail's dependants %+v are not the dry run's %+v (%v, %v)", got.Dependants, want, found, err)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "literal-tracker-token") {
		t.Error("the detail carries the seat's credential")
	}
}

// A LISTED TOOL JOINS THE CATALOGUE UNDER A NAME NO ROW HOLDS, ONCE: a
// per-agent server's tools, which no registry on a node running none of its
// children holds, are in that node's catalogue in the registry's own row
// shape; a name a registry row already holds (a built-in, or another server's
// tool that wins it) is what a seat is offered, and the listed tool under it
// is not added.
//
// Mutation: key the merge on the source as well as the name, or add a name
// twice, and this is red.
func TestAListedToolJoinsTheCatalogueUnderANameNoRowHolds(t *testing.T) {
	t.Parallel()
	getIssue := listed("gh_get_issue", "Read one issue", readOnly, 2)
	taken := []queries.ToolRow{{Name: "gh_create_issue", Source: "mcp:jira"}}
	listings := []coord.MCPListing{
		{Server: "github", Node: "node-a", Tools: []coord.MCPListedTool{
			listed("gh_create_issue", "Open an issue", unannotated, 1), getIssue}},
		{Server: "github", Node: "node-b", Tools: []coord.MCPListedTool{getIssue}},
	}
	schemas := map[string]coord.MCPToolSchema{
		getIssue.Digest: {Digest: getIssue.Digest, Input: json.RawMessage(`{"type":"object"}`)},
	}
	rows := queries.ListedRows(taken, listings, schemas)
	if len(rows) != 1 {
		t.Fatalf("listed rows = %+v, want get_issue alone", rows)
	}
	want := queries.ToolRow{
		Name: "gh_get_issue", Description: "Read one issue", Source: "mcp:github",
		Annotations: queries.ToolHints{ReadOnly: "yes", Destructive: "no", Idempotent: "yes", OpenWorld: "no"},
		InputSchema: map[string]any{"type": "object"},
	}
	if !reflect.DeepEqual(rows[0], want) {
		t.Errorf("row = %+v,\nwant %+v", rows[0], want)
	}
	// A WRITE DELIVERS TO ITS SERVER, by the registry's own predicate.
	write := queries.ListedRows(nil, []coord.MCPListing{{Server: "github", Tools: []coord.MCPListedTool{
		listed("gh_create_issue", "Open an issue", unannotated, 1)}}}, nil)
	if len(write) != 1 || write[0].Delivers != "github" || write[0].InputSchema != nil {
		t.Errorf("a write listed as %+v, want it delivering to github, with no schema read", write)
	}
}
