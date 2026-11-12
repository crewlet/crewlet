package mcpbridge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
)

// serversFixture is a seat's surface holding the engine's own tool and the
// tools of two of its MCP servers ([serversSurface]), opened on a bridge that
// serves every route a box dials: the job's endpoint and each server beneath
// it.
type serversFixture struct {
	surface  *tools.Surface
	ledger   *ledger
	endpoint string
}

// serversSurface is a seat's surface, bound to its turn, holding the engine's
// own read_page and the tools of two of its MCP servers: github's two, which
// the company describes, and jira's one, which it does not.
func serversSurface(t *testing.T) *tools.Surface {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(&stubTool{name: "read_page"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(read_page): %v", err)
	}
	for name, server := range map[string]string{
		"create_issue": "github", "search_issues": "github", "create_ticket": "jira",
	} {
		if err := reg.Register(&stubTool{name: name}, tools.Origin(server)); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	reg.DescribeServers(map[string]string{"github": "The company's code host: its repositories, issues and pull requests."})
	return tools.NewSurface("execute", reg.Snapshot(),
		[]string{"read_page", "create_issue", "search_issues", "create_ticket"}).ForTurn(seatTurn())
}

func newServersFixture(t *testing.T) *serversFixture {
	t.Helper()
	return openServers(t, serversSurface(t))
}

// openServers opens a job's session over surface, its servers' endpoints
// beneath it, as [newServersFixture] does — each of configure readying the
// session before it opens.
func openServers(t *testing.T, surface *tools.Surface, configure ...func(*mcpbridge.Session)) *serversFixture {
	t.Helper()
	f := &serversFixture{ledger: &ledger{}, surface: surface}

	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.ServerPattern, bridge.Handler())
	server.Config.Handler = mux
	session := &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger,
	}
	for _, ready := range configure {
		ready(session)
	}
	f.endpoint = bridge.Open(t.Context(), session)
	if f.endpoint == "" {
		t.Fatal("the bridge opened no session")
	}
	return f
}

// listed is the names of the tools the server at url lists.
func listed(t *testing.T, url string) (names []string, instructions string) {
	t.Helper()
	sess := dial(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools(%s): %v", url, err)
	}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names, sess.InitializeResult().Instructions
}

// EACH OF A SEAT'S MCP SERVERS IS SERVED UNDER ITS OWN NAME, beneath the job's
// endpoint, listing its own tools alone and saying what it is for; the job's
// endpoint lists the engine's own tools alone. A harness keeps a server's tools
// out of its requests by the SERVER — Claude Code's tool search, OpenCode's
// Code Mode — so the engine's own tools, which a run needs on every request,
// and the seat's, which it needs a few of, are two kinds of server.
//
// Mutation: list every tool at every endpoint, or the MCP tools at the job's
// own, and this goes red; drop the server's description from its endpoint, and
// the harness is told nothing of what the server is for.
func TestEachServerIsServedUnderItsOwnNameBeneathTheJob(t *testing.T) {
	t.Parallel()
	f := newServersFixture(t)

	if got := mcpbridge.Servers(f.surface); !slices.Equal(got, []string{"github", "jira"}) {
		t.Fatalf("Servers = %v, want the seat's two MCP servers", got)
	}
	own, _ := listed(t, f.endpoint)
	if !slices.Equal(own, []string{"read_page"}) {
		t.Errorf("the job's endpoint lists %v, want the engine's own tools alone", own)
	}
	github, instructions := listed(t, mcpbridge.ServerURL(f.endpoint, "github"))
	if !slices.Equal(github, []string{"create_issue", "search_issues"}) {
		t.Errorf("github's endpoint lists %v, want its own two tools", github)
	}
	if instructions != "The company's code host: its repositories, issues and pull requests." {
		t.Errorf("github's endpoint says it is for %q, want the company's description", instructions)
	}
	jira, instructions := listed(t, mcpbridge.ServerURL(f.endpoint, "jira"))
	if !slices.Equal(jira, []string{"create_ticket"}) {
		t.Errorf("jira's endpoint lists %v, want its own tool", jira)
	}
	if instructions != "" {
		t.Errorf("jira's endpoint says it is for %q, where the company described it as nothing", instructions)
	}
}

// A CALL ON A SERVER'S ENDPOINT IS THE OPENING'S: decided by the same surface
// and written on the run's record as a call to the engine's own is, so the
// delivery check and the reviewer see what the run did with a seat's MCP
// server as they see everything else it did.
//
// Mutation: dispatch a server endpoint's calls anywhere but the opening's
// Call, and the run's record never holds them.
func TestACallOnAServersEndpointIsTheRunsCall(t *testing.T) {
	t.Parallel()
	f := newServersFixture(t)
	sess := dial(t, mcpbridge.ServerURL(f.endpoint, "github"))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "create_issue",
		Arguments: map[string]any{"text": "flaky test"}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool = %+v, %v", res, err)
	}
	rows := f.ledger.rows()
	if len(rows) != 1 || rows[0].Name != "create_issue" {
		t.Fatalf("the run's record holds %+v, want the one call made on github's endpoint", rows)
	}
}

// A SERVER THE OPENING DOES NOT SERVE IS NO ENDPOINT, never the engine's own
// under another name: a box handed a URL for one dials a route that serves no
// MCP server at all.
func TestAServerTheOpeningDoesNotServeIsNoEndpoint(t *testing.T) {
	t.Parallel()
	f := newServersFixture(t)
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: mcpbridge.ServerURL(f.endpoint, "slack"), HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err == nil {
		_ = sess.Close()
		t.Fatal("a server the opening does not serve answered as an MCP server")
	}
}

// holdingHolder holds every call it is handed, as an executor's holder records
// a call to a tool at Needs approval as its run's question.
type holdingHolder struct{}

func (holdingHolder) Hold(context.Context, *turnctx.Turn, tools.HeldCall) (tools.Result, bool) {
	return tools.Result{Output: "put to a person"}, true
}

// A HELD CALL IS RECORDED WITHOUT ITS ARGUMENTS OR THEIR DIGEST. A call to a
// tool at Needs approval is the run's question: its arguments ride the run's row
// sealed beside it (ADR-0077), and the call log is read in the clear by
// everybody who reads the seat's trail — so a digest of them there, beside the
// tool's name, would let any reader confirm a guess at what the seal hides. The
// record says the call was held, which every later reader counts by, and keeps
// neither. NOR DOES THE ENTRY WRITTEN AHEAD OF IT, before anything knows it will
// be held: a tool that may ask a person first gets an entry with no digest.
//
// Mutation: record a held call's digest, or its arguments, and both are on the
// run's record in the clear; write every entry with its digest (`false &&`
// before the surface's word on the tool), and the entry carries it.
func TestAHeldCallIsRecordedWithoutItsArgumentsOrTheirDigest(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if err := reg.RegisterWith(&stubTool{name: "close_issue"}, tools.Origin("tracker"), tools.Annotations{},
		func(e *tools.Entry) { e.NeedsApproval = true }); err != nil {
		t.Fatal(err)
	}
	f := openServers(t, tools.NewSurface("execute", reg.Snapshot(), []string{"close_issue"}).
		ForTurn(seatTurn()).WithHolder(holdingHolder{}))
	sess := dial(t, mcpbridge.ServerURL(f.endpoint, "tracker"))

	callTool(t, sess, "close_issue", map[string]any{"id": 7})
	rows := f.ledger.rows()
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1: %+v", len(rows), rows)
	}
	if row := rows[0]; !row.Held || row.Args != nil || row.Digest != "" {
		t.Errorf("the held call is recorded as held %v with arguments %v and identity %q, want it held "+
			"with neither", row.Held, row.Args, row.Digest)
	}
	if entries := f.ledger.entries(); len(entries) != 1 || entries[0].Name != "close_issue" ||
		entries[0].Digest != "" {
		t.Errorf("the call's entry, written ahead of it, is %+v, want its identity with no digest", entries)
	}
}
