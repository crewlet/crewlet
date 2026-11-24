package mcpbridge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// serversFixture is a job opened over [serversSurface] on a bridge serving
// the job's endpoint and each server beneath it.
type serversFixture struct {
	surface  *tools.Surface
	ledger   *ledger
	bridge   *mcpbridge.Bridge
	endpoint string
}

// serversSurface holds the engine's read_page and two MCP servers' tools:
// github's two, which the company describes, and jira's one, which it does not.
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

// openServers opens a job's session over surface, each of configure readying
// the session first.
func openServers(t *testing.T, surface *tools.Surface, configure ...func(*mcpbridge.Session)) *serversFixture {
	t.Helper()
	f := &serversFixture{ledger: &ledger{}, surface: surface}

	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL,
	})
	f.bridge = bridge
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
// endpoint, listing its own tools and description; the job's endpoint lists the
// engine's own alone. A harness defers tools per server (Claude Code's tool
// search, OpenCode's Code Mode), so tools needed on every request and the
// seat's, needed a few at a time, must be different servers.
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

// THE ENGINE'S OWN SERVER LISTS NO NAME ANOTHER SERVER COULD SPELL. Qwen Code
// 0.28.4 named a user server `crewlet_`'s tool `x` and a repository server
// `crewlet__dup`'s tool `x` as the bridge's `_x` and `dup__x` (MEASURE-P3 N1,
// N2), and a harness joining with `-` spells another server `crewlet-a`'s
// tool `b` as a tool `a-b` of the bridge's. The job's endpoint lists none of
// those, whoever registered them, but each under an alias of the grammar; a
// seat's server lists its own as they are.
//
// Mutation: drop the job endpoint's aliasing, or apply it to every server.
func TestTheEnginesOwnServerListsNoNameAnotherServerCouldSpell(t *testing.T) {
	t.Parallel()
	odd := []string{"_x", "dup__x", "a-b", "x_"}
	reg := tools.NewRegistry()
	for _, name := range append([]string{"read_page"}, odd...) {
		if err := reg.Register(&stubTool{name: name}, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	if err := reg.Register(&stubTool{name: "get-issue"}, tools.Origin("github")); err != nil {
		t.Fatalf("Register(get-issue): %v", err)
	}
	f := openServers(t, tools.NewSurface("execute", reg.Snapshot(),
		[]string{"read_page", "_x", "dup__x", "a-b", "x_", "get-issue"}).ForTurn(seatTurn()))
	own, _ := listed(t, f.endpoint)
	if len(own) != 1+len(odd) || !slices.Contains(own, "read_page") {
		t.Errorf("the job's endpoint lists %v, want read_page and an alias of each of %v", own, odd)
	}
	for _, name := range own {
		if !harnessprofile.BridgeToolName(name) || slices.Contains(odd, name) {
			t.Errorf("the job's endpoint lists %q, a name another server could spell", name)
		}
	}
	if github, _ := listed(t, mcpbridge.ServerURL(f.endpoint, "github")); !slices.Equal(github, []string{"get-issue"}) {
		t.Errorf("github's endpoint lists %v, want its own tool as it is named", github)
	}
}

// unseated is a surface that is not a seat's [*tools.Surface], whose opening
// serves every tool as the engine's own ([mcpbridge.Servers]).
type unseated struct{ *tools.Surface }

// aliasSurface offers read_page and github's tools names, each tool's output
// naming it, on a surface whose opening serves them all as the engine's own.
func aliasSurface(t *testing.T, names ...string) mcpbridge.Surface {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(&stubTool{name: "read_page"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(read_page): %v", err)
	}
	for _, name := range names {
		if err := reg.Register(&stubTool{name: name}, tools.Origin("github")); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	return unseated{tools.NewSurface("execute", reg.Snapshot(),
		append([]string{"read_page"}, names...)).ForTurn(seatTurn())}
}

// openAliased opens a job over surface and answers the job's endpoint.
func openAliased(t *testing.T, surface mcpbridge.Surface) string {
	t.Helper()
	return openServers(t, nil, func(s *mcpbridge.Session) { s.Surface = surface }).endpoint
}

// A TOOL THE ENGINE'S OWN SERVER CANNOT LIST UNDER ITS NAME IS OFFERED UNDER AN
// ALIAS AND CALLABLE THROUGH IT, never dropped: a seat's MCP tool lands there
// when the opening does not serve its server, and its name is known only once
// that server lists, too late for config to refuse it. Two names that read
// alike (`get-issue`, `get__issue`, beside a `get_issue`) take two aliases;
// one too long is cut; a name the server can list is unchanged; and a tool's
// alias is its name's alone, the same in an opening without the others.
//
// Mutation: drop a tool whose name fails the grammar, list it under its
// sanitized name alone, or call the tool by its alias, and this goes red.
func TestAToolTheEnginesServerCannotNameIsOfferedAndCalledUnderAnAlias(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ab-", 20)
	odd := []string{"get-issue", "get__issue", "a__b", long}
	endpoint := openAliased(t, aliasSurface(t, append([]string{"get_issue"}, odd...)...))
	own, _ := listed(t, endpoint)
	if len(own) != 2+len(odd) || !slices.Contains(own, "read_page") || !slices.Contains(own, "get_issue") {
		t.Fatalf("the job's endpoint lists %v, want read_page, get_issue and an alias of each of %v", own, odd)
	}
	sess := dial(t, endpoint)
	reached := map[string]string{}
	for _, name := range own {
		if !harnessprofile.BridgeToolName(name) {
			t.Errorf("the job's endpoint lists %q, outside the grammar", name)
		}
		res := callTool(t, sess, name, map[string]any{"id": 1})
		tool, ok := strings.CutSuffix(text(res), " ok")
		if res.IsError || !ok {
			t.Errorf("a call on %q answered %q, want the tool's own answer", name, text(res))
			continue
		}
		if prev, twice := reached[tool]; twice {
			t.Errorf("%q and %q both reach %q", prev, name, tool)
		}
		reached[tool] = name
	}
	for _, tool := range append([]string{"read_page", "get_issue"}, odd...) {
		alias, ok := reached[tool]
		switch {
		case !ok:
			t.Errorf("no listed name reaches %q", tool)
		case harnessprofile.BridgeToolName(tool) && alias != tool:
			t.Errorf("%q is listed as %q, want its own name", tool, alias)
		case strings.HasPrefix(tool, "get") && tool != "get_issue" && !strings.HasPrefix(alias, "get_issue_"):
			t.Errorf("%q is listed as %q, want its words readable in front", tool, alias)
		}
	}
	alone, _ := listed(t, openAliased(t, aliasSurface(t, "get-issue")))
	if want := reached["get-issue"]; !slices.Contains(alone, want) {
		t.Errorf("get-issue is listed as %q beside the others and among %v alone, want one alias for the name", want,
			alone)
	}
}

// A LAUNCH IS HANDED THE TOOLS THE ENGINE'S OWN SERVER LISTS, exactly as its
// endpoint lists them as the run opens: a runner holds its harness's offer to
// them, so a name the endpoint does not list would end a run nothing narrowed,
// and one left out would let a setting take it away unseen; a tool it lists
// under an alias is handed under the alias. A harness loading its skills
// itself is not listed the bridge's skill loader.
//
// Mutation: hand every surface tool, the MCP servers' included, hand a tool
// under its own name where it is listed under an alias, or keep the skill
// loader for a harness loading its own, and this goes red.
func TestALaunchIsHandedTheToolsTheEnginesOwnServerLists(t *testing.T) {
	t.Parallel()
	for name, skills := range map[string]map[string]string{"skills of the bridge's": nil,
		"skills of its own": {"crewlet:deploy": "deploy"}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reg := tools.NewRegistry()
			for tool, origin := range map[string]string{"read_page": tools.OriginBuiltin,
				toolname.LoadToolSkill: tools.OriginBuiltin, "create_issue": tools.Origin("github"),
				"read-page": tools.OriginBuiltin} {
				if err := reg.Register(&stubTool{name: tool}, origin); err != nil {
					t.Fatalf("Register(%s): %v", tool, err)
				}
			}
			surface := tools.NewSurface("execute", reg.Snapshot(),
				[]string{"read_page", toolname.LoadToolSkill, "create_issue", "read-page"}).ForTurn(seatTurn())
			var session *mcpbridge.Session
			f := openServers(t, surface, func(s *mcpbridge.Session) { s.Skills = skills; session = s })
			own, _ := listed(t, f.endpoint)
			if got := mcpbridge.Listed(session); !slices.Equal(got, own) || slices.Contains(got, "create_issue") ||
				slices.Contains(got, "read-page") {
				t.Errorf("Listed = %v, the engine's own endpoint lists %v", got, own)
			}
		})
	}
}

// A CALL ON A SERVER'S ENDPOINT IS THE OPENING'S: decided by the same surface
// and on the run's record, so the delivery check and the reviewer see it.
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
// under another name.
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

// holdingHolder holds every call, as an executor's holder does a call to a
// tool at Needs approval.
type holdingHolder struct{}

func (holdingHolder) Hold(context.Context, *turnctx.Turn, tools.HeldCall) (tools.Result, bool) {
	return tools.Result{Output: "put to a person"}, true
}

// A HELD CALL IS RECORDED WITHOUT ITS ARGUMENTS OR THEIR DIGEST, nor is the
// entry written ahead of it: its arguments ride the run's row sealed
// (ADR-0077), and a digest in the clear call log would let any reader of the
// seat's trail confirm a guess at what the seal hides.
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
