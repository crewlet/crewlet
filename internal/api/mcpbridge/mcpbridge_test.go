package mcpbridge_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// The bridge is the one place a sandbox can reach a live seat's tools, so
// every case here is about a boundary: which credential admits a call, whose
// dispatch runs it, what survives a restart, and what a box keeps when its run
// is over.

// --- doubles ---------------------------------------------------------------

// stubTool records the arguments it was called with, so a case can assert what
// crossed the wire rather than only that something did.
type stubTool struct {
	name string
	out  string
	fail bool
	// err is a call that could not run at all: the turn torn down.
	err error

	mu   sync.Mutex
	args []map[string]any
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return s.name + " does a thing" }
func (s *stubTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":   map[string]any{"type": "integer"},
			"text": map[string]any{"type": "string"},
		},
	}
}

func (s *stubTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	s.mu.Lock()
	s.args = append(s.args, args)
	s.mu.Unlock()
	if s.err != nil {
		return tools.Result{}, s.err
	}
	out := s.out
	if out == "" {
		out = s.name + " ok"
	}
	return tools.Result{Output: out, Failed: s.fail}, nil
}

func (s *stubTool) seen() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.args...)
}

// ledger records what a resume would read back.
type ledger struct {
	mu    sync.Mutex
	calls []mcpbridge.Call
	err   error
}

func (l *ledger) Append(_ context.Context, _ string, c mcpbridge.Call) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.calls = append(l.calls, c)
	return nil
}

func (l *ledger) rows() []mcpbridge.Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]mcpbridge.Call(nil), l.calls...)
}

// denyGuard refuses one tool, standing in for the required-skill gate.
type denyGuard struct{ tool, reason string }

func (g denyGuard) Check(name, _ string) string {
	if name == g.tool {
		return g.reason
	}
	return ""
}

func (g denyGuard) Observe(string, toolargs.Args) {}

// --- fixture ---------------------------------------------------------------

type fixture struct {
	bridge  *mcpbridge.Bridge
	server  *httptest.Server
	session *mcpbridge.Session
	surface *tools.Surface
	ledger  *ledger
	byName  map[string]*stubTool
}

// bridgeFor is [mcpbridge.New] for a keyring a case expects to be usable.
func bridgeFor(t *testing.T, opts mcpbridge.Options) *mcpbridge.Bridge {
	t.Helper()
	bridge, err := mcpbridge.New(opts)
	if err != nil {
		t.Fatalf("mcpbridge.New: %v", err)
	}
	return bridge
}

// newFixture wires a bridge over a two-tool surface and serves it.
func newFixture(t *testing.T, offer ...string) *fixture {
	t.Helper()
	reg := tools.NewRegistry()
	byName := map[string]*stubTool{}
	// REGISTERED WITH THEIR HINTS, because that is what the bridge now
	// advertises — and with two different shapes, so a case can tell a
	// read from a write on the wire rather than only in the registry.
	hints := map[string]tools.Annotations{
		"read_page": {ReadOnly: crewletmcp.Yes, Idempotent: crewletmcp.Yes},
		"post_message": {
			ReadOnly: crewletmcp.No, Destructive: crewletmcp.No,
			OpenWorld: crewletmcp.Yes,
		},
	}
	for _, name := range []string{"read_page", "post_message"} {
		tool := &stubTool{name: name}
		if err := reg.RegisterWith(tool, tools.OriginBuiltin,
			hints[name]); err != nil {

			t.Fatalf("Register(%s): %v", name, err)
		}
		byName[name] = tool
	}
	if err := reg.Register(whoami{}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(whoami): %v", err)
	}
	f := &fixture{ledger: &ledger{}, byName: byName}
	if len(offer) == 0 {
		offer = []string{"read_page", "post_message"}
	}
	f.surface = tools.NewSurface("execute", reg.Snapshot(), offer).ForTurn(seatTurn())
	f.bridge = bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key")})
	f.session = &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger,
	}

	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", f.bridge.Handler())
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// open registers the session and returns the URL a box would dial.
func (f *fixture) open(t *testing.T) string {
	t.Helper()
	f.bridge = bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: f.server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", f.bridge.Handler())
	f.server.Config.Handler = mux
	return f.bridge.Open(t.Context(), f.session)
}

// dial connects an MCP client the way a coding agent's own client would —
// re-reading the tool list when the server says it changed, as the CLIs that
// run in a box do, so an activation's held answer is let go by the re-list.
func dial(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, req *mcp.ToolListChangedRequest) {
			_, _ = req.Session.ListTools(ctx, nil)
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// HTTPClient IS NOT OPTIONAL HERE. Left unset the SDK reaches for
	// http.DefaultClient, and every httptest.Server.Close in this binary
	// ends by sweeping that pool — see
	// [github.com/crewlet/crewlet/internal/httpx/httpxtest]. This helper
	// holds a URL rather than a server, so it owns a pool instead of
	// borrowing one.
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callTool(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// --- the credential --------------------------------------------------------

// A BOX HOLDS NO API TOKEN, and giving it one would hand a sandbox the
// credential that reads the whole company. What admits a call is the per-run
// token in the path, and nothing else.
func TestOnlyAMintedTokenReachesASession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	url := f.open(t)
	if url == "" {
		t.Fatal("no endpoint was minted")
	}
	if !strings.HasPrefix(url, f.server.URL+mcpbridge.PathPrefix) {
		t.Fatalf("endpoint = %q", url)
	}

	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"forged", "v1.run-1.99999999999.notasignature", http.StatusUnauthorized},
		{"malformed", "garbage", http.StatusUnauthorized},
		{"another run's shape", "v1.run-2.99999999999.x", http.StatusUnauthorized},
		// No token at all matches no route: the pattern requires a
		// segment. 404 rather than 401 is the honest answer — there is
		// nothing at that address to be unauthorized for.
		{"absent", "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := f.server.Client().Post(f.server.URL+mcpbridge.PathPrefix+tc.token,
				"application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}

// A CLOSED RUN'S TOKEN IS WORTH NOTHING. Two gates stand behind the endpoint —
// the signature says the token was minted by this fleet, the session map says
// the run is still going — and without the second a box that outlived its run
// keeps a working key to a live seat's tools.
func TestATokenForAFinishedRunIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	url := f.open(t)
	// It works while the run is live.
	sess := dial(t, url)
	if res := callTool(t, sess, "read_page", map[string]any{}); res.IsError {
		t.Fatalf("a live run's call failed: %s", text(res))
	}
	_ = sess.Close()

	f.bridge.Close("run-1")
	if f.bridge.Live() != 0 {
		t.Fatalf("%d sessions after a close", f.bridge.Live())
	}
	res, err := f.server.Client().Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a finished run's token still worked: %d", res.StatusCode)
	}
}

// CLOSING IS IDEMPOTENT, because the paths that call it are: the completion
// path, the failure path and the boot sweep each have to be safe to run over a
// run the others already finished.
func TestClosingTwiceIsSafe(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.open(t)
	f.bridge.Close("run-1")
	f.bridge.Close("run-1")
	f.bridge.Close("never-existed")
	if f.bridge.Live() != 0 {
		t.Errorf("%d sessions live", f.bridge.Live())
	}
}

// A KEYRING THAT CANNOT SIGN FOR THE FLEET BUILDS NO BRIDGE. It used to take a
// per-process key and log that it had, which kept a single node working and
// cost a fleet the one diagnosis the fleet's key buys: a peer a misrouted call
// reaches could no longer tell a token its fleet signed from a forged one.
// Every node holds a keyring now, so a caller arriving without one is a wiring
// mistake, refused by name at both entrances — and [mcpbridge.Build] with
// neither a base URL nor a host URL still builds nothing and refuses nothing,
// because a node with no address a box can dial is a node that binds no
// listener, and that is not a keyring question.
//
// Mutation: restore the per-process fallback in runtoken.New and every row
// builds a bridge.
func TestAKeyringThatCannotSignBuildsNoBridge(t *testing.T) {
	t.Parallel()
	env := func(name string) string {
		if name == mcpbridge.BaseURLVar {
			return "https://engine.example.com"
		}
		return ""
	}
	for name, material := range map[string]runtoken.Material{
		"no keys":             {},
		"an active id unheld": {ActiveID: "k9", Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "m"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if b, err := mcpbridge.New(mcpbridge.Options{Material: material}); !errors.Is(err, runtoken.ErrNoKeyring) || b != nil {
				t.Errorf("New = %v, %v, want ErrNoKeyring and no bridge", b, err)
			}
			if b, err := mcpbridge.Build(env, material, ""); !errors.Is(err, runtoken.ErrNoKeyring) || b != nil {
				t.Errorf("Build = %v, %v, want ErrNoKeyring and no bridge", b, err)
			}
			// THE CONTROL: with no address at all nothing is built,
			// whatever the keyring, and that is a configuration rather
			// than a refusal.
			if b, err := mcpbridge.Build(func(string) string { return "" }, material, ""); err != nil || b != nil {
				t.Errorf("Build with no base URL = %v, %v, want nothing and no error", b, err)
			}
		})
	}
	// And a usable keyring builds one, so the rows above are refused for the
	// keyring and not for something else Build dislikes.
	if b, err := mcpbridge.Build(env, runtoken.OneKey("k1", "m"), ""); err != nil || b == nil {
		t.Fatalf("Build with a usable keyring = %v, %v, want a bridge", b, err)
	}
}

// A BOX ON THE NODE'S OWN HOST DIALS THE NODE'S OWN LISTENER. The `direct`
// cell's run is a process of this host, so the listener the node bound is an
// address it can always dial, and the quickstart's company — one `direct` cell
// and no CREWLET_MCP_BRIDGE_URL — has to bridge on that alone. A box in any
// other cell cannot dial the host's loopback, so it is given the variable's
// address or nothing, never the loopback one.
//
// And preferred to the variable for a host box even where both are set:
// loopback never leaves the host, while the variable may route through a load
// balancer to any peer of the fleet.
//
// Mutation: drop hostURL from Build and the host-only row builds nothing; give
// Reaches the host URL for every box and the container row dials loopback;
// prefer the base URL for a host box and the both-set row does.
func TestABoxOnTheHostDialsTheNodesOwnListener(t *testing.T) {
	t.Parallel()
	const host, base = "http://127.0.0.1:8080", "https://engine.example.com"
	withBase := func(name string) string {
		if name == mcpbridge.BaseURLVar {
			return base
		}
		return ""
	}
	noEnv := func(string) string { return "" }
	for _, tc := range []struct {
		name          string
		env           func(string) string
		host          string
		onHost, other string
	}{
		{"the host URL alone", noEnv, host, host, ""},
		{"the variable alone", withBase, "", base, base},
		{"both", withBase, host, host, base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := mcpbridge.Build(tc.env, runtoken.OneKey("k", "k"), tc.host)
			if err != nil || b == nil {
				t.Fatalf("Build = %v, %v, want a bridge", b, err)
			}
			if got := b.Reaches(true); got != tc.onHost {
				t.Errorf("a box on the host dials %q, want %q", got, tc.onHost)
			}
			if got := b.Reaches(false); got != tc.other {
				t.Errorf("a box off the host dials %q, want %q", got, tc.other)
			}
			_ = b.Handler()
			for _, onHost := range []bool{true, false} {
				want := tc.other
				if onHost {
					want = tc.onHost
				}
				endpoint := b.Open(t.Context(), &mcpbridge.Session{
					RunID: fmt.Sprintf("run-%t", onHost), Launch: "job-1", Handle: "dev",
					Surface: emptySurface(), OnHost: onHost,
				})
				switch {
				case want == "" && endpoint != "":
					t.Errorf("on host %t: minted %q with no address that box can dial", onHost, endpoint)
				case want != "" && !strings.HasPrefix(endpoint, want+mcpbridge.PathPrefix):
					t.Errorf("on host %t: endpoint %q, want it under %q", onHost, endpoint, want)
				}
			}
		})
	}
}

// A DEPLOYMENT WHOSE API IS NOT REACHABLE FROM A BOX cannot bridge, and the
// honest answer is an empty endpoint the caller can refuse the launch on —
// not a run that starts and fails on its first tool call.
//
// AND NOTHING IS REGISTERED FOR IT. The minted token is the only way to reach
// a session, so one registered with no endpoint is a live surface nothing can
// dispatch to — and the caller, reading the empty answer as a refusal, never
// launches the run whose end would have closed it.
func TestNoBaseURLMintsNoEndpointAndHoldsNoSession(t *testing.T) {
	t.Parallel()
	b := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "k")})
	// Mounted, so the missing base URL is the only reason to refuse.
	_ = b.Handler()
	if url := b.Open(t.Context(), &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Surface: emptySurface(),
	}); url != "" {
		t.Errorf("endpoint = %q, want empty", url)
	}
	if b.Live() != 0 {
		t.Errorf("%d sessions live for a run that was refused and will never be closed", b.Live())
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clock := time.Now()
	f.bridge = bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: f.server.URL, TTL: time.Minute,
		Now: func() time.Time { return clock },
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", f.bridge.Handler())
	f.server.Config.Handler = mux
	url := f.bridge.Open(t.Context(), f.session)

	clock = clock.Add(2 * time.Minute)
	res, err := f.server.Client().Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an expired token still worked: %d", res.StatusCode)
	}
}

// --- the dispatch is the seat's own ----------------------------------------

func TestABridgedCallRunsTheSeatsTool(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sess := dial(t, f.open(t))

	res := callTool(t, sess, "read_page", map[string]any{"text": "hello"})
	if res.IsError {
		t.Fatalf("call failed: %s", text(res))
	}
	if got := text(res); got != "read_page ok" {
		t.Errorf("output = %q", got)
	}
	seen := f.byName["read_page"].seen()
	if len(seen) != 1 || seen[0]["text"] != "hello" {
		t.Errorf("the tool saw %+v", seen)
	}
}

// A LARGE ID SURVIVES. Go's default JSON decode turns every number into a
// float64, which silently rounds an issue id past 2^53 — and the tool then
// updates a different issue. The bridge decodes through json.Number.
func TestALargeIdIsNotRounded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sess := dial(t, f.open(t))

	const exact = "9007199254740993" // 2^53 + 1
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_page", Arguments: map[string]any{"id": mustNumber(t, exact)},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	seen := f.byName["read_page"].seen()
	if len(seen) != 1 {
		t.Fatalf("the tool ran %d times", len(seen))
	}
	if got := literal(seen[0]["id"]); got != exact {
		t.Errorf("id = %s, want %s — the value was rounded in transit", got, exact)
	}
}

// A TOOL NOT ON THE SURFACE IS A FAILED RESULT, not a protocol error: the
// caller asked for something it cannot have, which is a thing to tell it about.
func TestAToolTheSeatDoesNotOfferIsRefusedAsAResult(t *testing.T) {
	t.Parallel()
	// The surface offers one of the two registered tools, so the other is
	// reachable-but-not-offered — the case the model can act on.
	f := newFixture(t, "read_page")
	sess := dial(t, f.open(t))

	// It is not even advertised.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "post_message" {
			t.Fatal("a tool the surface does not offer was advertised")
		}
	}
	// And calling it anyway does not run it.
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "post_message"})
	if err == nil && !res.IsError {
		t.Fatal("an unoffered tool ran")
	}
	if len(f.byName["post_message"].seen()) != 0 {
		t.Error("the tool ran despite not being offered")
	}
}

// THE SKILL GUARD IS THE SEAT'S, and it holds over the bridge. A second
// implementation here would drift, and it would drift on the security half —
// so the guard is the one the turn installed on its own surface.
func TestTheSeatsSkillGuardHoldsOverTheBridge(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.session.Surface = f.surface.WithGuard(denyGuard{
		tool: "post_message", reason: "load the posting skill first",
	})
	sess := dial(t, f.open(t))

	res := callTool(t, sess, "post_message", map[string]any{})
	if !res.IsError || !strings.Contains(text(res), "posting skill") {
		t.Fatalf("the guard did not refuse the call: %+v", res)
	}
	if len(f.byName["post_message"].seen()) != 0 {
		t.Error("a guarded tool ran anyway")
	}
	// The refusal is RECORDED, so an operator sees the run spent a call
	// here rather than that the tool silently did nothing.
	rows := f.ledger.rows()
	if len(rows) != 1 || !rows[0].Failed {
		t.Errorf("the refusal is not in the ledger: %+v", rows)
	}
}

// A FAILING TOOL IS A FAILED RESULT WITH ITS OUTPUT, not a dropped call: the
// caller reads the reason and can act on it.
func TestAFailingToolComesBackWithItsReason(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.byName["post_message"].fail = true
	f.byName["post_message"].out = "the channel is archived"
	sess := dial(t, f.open(t))

	res := callTool(t, sess, "post_message", map[string]any{})
	if !res.IsError || text(res) != "the channel is archived" {
		t.Fatalf("result = %+v / %q", res.IsError, text(res))
	}
}

// A CALL THAT COULD NOT RUN AT ALL IS TOLD IN WORDS OF ITS OWN: the turn the
// run belongs to was torn down, and the bridge answered the coding agent in
// the box, a model, with the Go error's text whole.
//
// Mutation: put the error's text back into the refused answer and this fails.
func TestACallThatCouldNotRunIsToldInWordsNotItsError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.byName["post_message"].err = fmt.Errorf("toolloop: %w", context.Canceled)
	sess := dial(t, f.open(t))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "post_message",
		Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("a call that could not run failed the protocol: %v", err)
	}
	if said := text(res); !strings.Contains(said, "the turn this coding run belongs to has ended") ||
		strings.Contains(said, "context canceled") || strings.Contains(said, "toolloop") {
		t.Errorf("a call that could not run was told %q", said)
	}
}

// statusTool takes a closed set, so a case can tell the spelling a model sent
// from the one its tool reads.
type statusTool struct{ stubTool }

func (*statusTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"status": map[string]any{"type": "string", "enum": []any{"draft", "published"}},
		"note":   map[string]any{"type": "string"},
	}}
}

// AN EXECUTOR RUN'S CALL IS HELD TO ITS TOOL'S SCHEMA OVER THE BRIDGE
// (ADR-0059), by the surface's one path ([tools.Invoke]) and no check of the
// bridge's own: the tool reads a closed set's member as its schema spells it;
// a call answered outside its schema carries the note to the CLI and onto the
// run's durable record, which a collected run's record is read from; and a
// value the tool would misread is refused before it runs, the refusal on the
// record too. The record keeps the spelling the model sent, which a resume
// reads back canonically (internal/agent/runner's seedCalls).
//
// Mutation: answer a bridged call with the tool's own output rather than the
// surface's rendered one and the note reaches neither the CLI nor the record;
// call the tool directly from the surface and the misread runs.
func TestABridgedCallIsHeldToItsToolsSchema(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := tools.NewRegistry()
	status := &statusTool{stubTool{name: "set_status"}}
	if err := reg.RegisterWith(status, tools.OriginBuiltin,
		tools.Annotations{ReadOnly: crewletmcp.No}); err != nil {
		t.Fatal(err)
	}
	f.session.Surface = tools.NewSurface("execute", reg.Snapshot(), []string{"set_status"}).
		ForTurn(seatTurn())
	sess := dial(t, f.open(t))

	res := callTool(t, sess, "set_status", map[string]any{"status": " Draft", "tittle": "x"})
	if res.IsError {
		t.Fatalf("the call failed: %s", text(res))
	}
	if seen := status.seen(); len(seen) != 1 || seen[0]["status"] != "draft" {
		t.Errorf("the tool read %+v, want the status as its schema spells it", seen)
	}
	const note = "\n\nNote: set_status ran, but these arguments are outside its schema"
	if got := text(res); !strings.HasPrefix(got, "set_status ok"+note) ||
		!strings.Contains(got, "`tittle` is not an argument") {
		t.Errorf("the CLI was answered %q, want the output and the note on the unknown argument", got)
	}

	refused := callTool(t, sess, "set_status", map[string]any{"note": map[string]any{"x": 1}})
	if !refused.IsError || !strings.Contains(text(refused), "set_status did not run") {
		t.Errorf("a misread was answered %+v / %q", refused.IsError, text(refused))
	}
	if len(status.seen()) != 1 {
		t.Error("the tool ran on a value it would misread")
	}

	rows := f.ledger.rows()
	if len(rows) != 2 {
		t.Fatalf("%d rows, want both calls: %+v", len(rows), rows)
	}
	if !strings.HasPrefix(rows[0].Output, "set_status ok"+note) || rows[0].Args["status"] != " Draft" {
		t.Errorf("the record is %+v, want the noted answer and the model's own spelling", rows[0])
	}
	if !rows[1].Failed || !strings.Contains(rows[1].Output, "set_status did not run") {
		t.Errorf("the refusal's record is %+v", rows[1])
	}
}

// --- the durable ledger ----------------------------------------------------

// A NATIVE TOOL LOOP KEEPS ITS CALLS IN MEMORY and the turn writes them at the
// end. A bridged run outlives its launching turn and can outlive the process,
// so a restart mid-run would leave the reviewer judging a turn whose whole
// tool log is gone.
func TestEveryBridgedCallIsRecordedWhereAResumeCanReadIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sess := dial(t, f.open(t))

	callTool(t, sess, "read_page", map[string]any{"text": "one"})
	callTool(t, sess, "post_message", map[string]any{"text": "two"})

	rows := f.ledger.rows()
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].Name != "read_page" || rows[1].Name != "post_message" {
		t.Errorf("rows are not in call order: %+v", rows)
	}
	if rows[0].Args["text"] != "one" || rows[0].Output != "read_page ok" {
		t.Errorf("the row does not carry the call: %+v", rows[0])
	}
}

// A LOST LEDGER ROW COSTS ONE LINE OF EVIDENCE. Failing the call instead would
// tell a coding agent to retry work that has already happened — the
// duplicate-side-effect failure the whole delivery machinery exists to stop.
func TestALedgerFailureDoesNotFailTheCall(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.ledger.err = errors.New("the store is unreachable")
	sess := dial(t, f.open(t))

	res := callTool(t, sess, "read_page", map[string]any{})
	if res.IsError {
		t.Fatalf("a ledger failure failed the call: %s", text(res))
	}
	if len(f.byName["read_page"].seen()) != 1 {
		t.Error("the tool did not run")
	}
}

// A RUN WITH NO LEDGER still works: that is a run whose whole life is one
// process, which needs nothing durable to resume from.
func TestARunWithNoLedgerStillDispatches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.session.Ledger = nil
	sess := dial(t, f.open(t))
	if res := callTool(t, sess, "read_page", map[string]any{}); res.IsError {
		t.Fatalf("call failed: %s", text(res))
	}
}

// --- what the caller is offered --------------------------------------------

// CLOSING A RUN ENDS THE MCP SESSIONS ITS BOX HOLDS, not only the bridge's
// own map entry. The SDK keeps every session a box opened — transport,
// server, and through the server's handlers the seat's live surface — until
// the client sends a DELETE or an idle clock fires, and a box torn down
// mid-run sends nothing. Left there, each executor run this process ever
// served would pin its seat's surface for the life of the process.
func TestClosingARunEndsTheBoxesOpenSessions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	url := f.open(t)
	sess := dial(t, url)
	// AT LEAST one, not exactly one: the SDK client's capability probe
	// (SEP-2575 server/discover, sent before initialize) opens a server
	// session of its own that the SDK never retires — which is one more
	// reason the close below has to reap everything the server holds.
	if n := f.session.Connections(); n == 0 {
		t.Fatal("no MCP session on a run with a connected box")
	}

	f.bridge.Close("run-1")
	if n := f.session.Connections(); n != 0 {
		t.Errorf("%d MCP sessions survive the run's close", n)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := sess.ListTools(ctx, nil); err == nil {
		t.Error("a box's session outlived its run")
	}
}

// A RUN OPENED AGAIN UNDER ITS OWN ID is a relaunch — a resumed executor
// going back into a box under the turn id it already had — and the
// coordinator deliberately does not end a run its own turn relaunched. So
// the earlier session is closed HERE: a box still holding it must not keep a
// working key to the surface the new run now owns, and nothing else would
// ever release it.
func TestReopeningARunReplacesItsEarlierSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	first := f.session
	dial(t, f.open(t))
	if n := first.Connections(); n == 0 {
		t.Fatal("no MCP session on the first run")
	}

	second := &mcpbridge.Session{
		RunID: "run-1", Launch: "job-2", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger,
	}
	url := f.bridge.Open(t.Context(), second)
	if n := first.Connections(); n != 0 {
		t.Errorf("the relaunched run's earlier session keeps %d box connections", n)
	}
	if f.bridge.Live() != 1 {
		t.Errorf("%d sessions live after a relaunch, want 1", f.bridge.Live())
	}
	if res := callTool(t, dial(t, url), "read_page", map[string]any{}); res.IsError {
		t.Errorf("the relaunched run cannot dispatch: %s", text(res))
	}
}

// AND ITS TOKEN STOPS WORKING. Closing the transport only ends a connection
// the box would reopen: it still holds a signed, unexpired URL, and while the
// token's subject was the RUN id that URL resolved to whichever session the
// run now had. The superseded box could then drive the new round's surface
// and append to the durable call log the relaunch reset exists to clear —
// re-delivering a message from a box nobody is collecting.
func TestASupersededBoxsTokenNoLongerResolves(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	stale := f.open(t)
	if res := callTool(t, dial(t, stale), "read_page", map[string]any{}); res.IsError {
		t.Fatalf("the first run could not dispatch: %s", text(res))
	}

	fresh := f.bridge.Open(t.Context(), &mcpbridge.Session{
		RunID: "run-1", Launch: "job-2", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger,
	})
	if fresh == stale {
		t.Fatal("the relaunched run was handed the superseded box's own URL")
	}
	res, err := f.server.Client().Post(stale, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a superseded box's token still reached a session: %d", res.StatusCode)
	}
	// The log tells the two misses apart, because only one is an operator's
	// problem — and only the signed one may be warned about, on a route
	// anyone can reach without a credential.
	if _, reason := f.bridge.Miss(strings.TrimPrefix(stale, f.server.URL+mcpbridge.PathPrefix)); reason == "" {
		t.Error("a superseded token resolved")
	} else if !strings.Contains(reason, "relaunched") {
		t.Errorf("the miss does not say the run was relaunched: %s", reason)
	}
}

// A BOX'S TOKEN OUTLIVES THE PROCESS THAT MINTED IT. A detached run goes on
// calling for as long as its CLI runs, and the node it dials can restart in
// the meantime: the new process holds no session until it takes the run's
// seat back, and then it opens the run's JOB again — and the token the box
// already holds, signed under the fleet's keyring, resolves to it. Nothing the
// box was given names the process that gave it. And the run's calls go on in
// its own order: the reopened session numbers them after the last one the
// run's row records.
//
// Mutation: name an opening by an id the opening process draws for itself, and
// the restarted node refuses the box's every call for the rest of its run;
// number a reopened session's calls from one, and they sort among the calls
// made before the restart.
func TestABoxsTokenOutlivesTheProcessThatMintedIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	url := f.open(t)
	if res := callTool(t, dial(t, url), "read_page", map[string]any{}); res.IsError {
		t.Fatalf("the run could not dispatch before the restart: %s", text(res))
	}

	// THE RESTART: a fresh process at the same address, over the same fleet
	// keyring, holding nothing yet.
	restarted := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: f.server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", restarted.Handler())
	f.server.Config.Handler = mux
	token := strings.TrimPrefix(url, f.server.URL+mcpbridge.PathPrefix)
	if _, reason := restarted.Miss(token); !strings.Contains(reason, "taken the run's seat back") {
		t.Errorf("before the seat is recovered the miss does not say why: %q", reason)
	}

	// THE RECOVERY: the run's job opened again, on a surface rebuilt here.
	recovered := &ledger{}
	before := f.ledger.rows()
	if len(before) != 1 {
		t.Fatalf("%d calls recorded before the restart, want 1", len(before))
	}
	if again := restarted.Open(t.Context(), &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", LastSeq: before[0].Seq, Handle: "dev",
		Role: "Engineer", Surface: f.surface, Ledger: recovered,
	}); again == "" {
		t.Fatal("the recovered job's session was not opened")
	}
	if res := callTool(t, dial(t, url), "read_page", map[string]any{}); res.IsError {
		t.Fatalf("the box's own token does not reach the recovered session: %s", text(res))
	}
	rows := recovered.rows()
	if len(rows) != 1 || rows[0].Name != "read_page" {
		t.Fatalf("the recovered session recorded %+v, want the box's call", rows)
	}
	if rows[0].Seq != before[0].Seq+1 {
		t.Errorf("the first call after the recovery is #%d, want #%d: after the run's "+
			"last call before it", rows[0].Seq, before[0].Seq+1)
	}
}

// A TOKEN THIS FLEET NEVER SIGNED IS NOT AN OPERATOR'S PROBLEM, and this
// route is deliberately exempt from authentication — the box holds no API
// token — so a warning per bad token is a log line anyone who can reach the
// engine can write without limit. Only the half that takes the signing key to
// produce may be warned about.
func TestOnlyASignedMissIsWorthWarningAbout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.open(t)
	runID, reason := f.bridge.Miss("not-a-token")
	if runID != "" {
		t.Errorf("a forged token named run %q", runID)
	}
	if !strings.Contains(reason, "forged") {
		t.Errorf("reason = %q", reason)
	}
	// A signed token for a run this node does not hold keeps its run id,
	// which is what makes it the actionable half.
	f.bridge.Close("run-1")
	if id, _ := f.bridge.Miss(f.token(t, "run-1")); id != "run-1" {
		t.Errorf("a fleet-signed miss named run %q, want run-1", id)
	}
}

// The handshake says WHICH SEAT this is, because it reaches the coding
// agent's own logs and a box that cannot tell one bridge from another is a box
// whose logs cannot be read.
func TestTheHandshakeNamesTheSeat(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sess := dial(t, f.open(t))
	impl := sess.InitializeResult().ServerInfo
	if !strings.Contains(impl.Title, "dev") || !strings.Contains(impl.Title, "Engineer") {
		t.Errorf("the handshake does not name the seat: %+v", impl)
	}
}

// mustNumber turns a decimal string into the JSON number a client sends.
func mustNumber(t *testing.T, s string) any {
	t.Helper()
	return jsonNumber(s)
}

type jsonNumber string

func (n jsonNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// literal renders a decoded argument exactly, so a rounded float is visible
// as one rather than being re-formatted back into the value it should have
// been.
func literal(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case interface{ String() string }:
		return t.String()
	default:
		return ""
	}
}

// A SESSION THAT CANNOT DISPATCH IS WORSE THAN NONE. Registered, it would
// answer a box's handshake and then take the process down on its first
// tools/list — one seat's wiring mistake becoming a fleet node's crash.
func TestAnIncompleteSessionIsRefusedRatherThanRegistered(t *testing.T) {
	t.Parallel()
	b := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "k"), BaseURL: "http://x"})
	// Mounted, so the incomplete session is the only reason to refuse.
	_ = b.Handler()
	for _, tc := range []struct {
		name    string
		session *mcpbridge.Session
	}{
		{"no run id", &mcpbridge.Session{Launch: "job-1", Handle: "dev", Surface: emptySurface()}},
		// NO JOB, and a token is minted for one: without it a relaunch's
		// box could not be told from its predecessor's, and a recovery
		// would have nothing to open the same session under.
		{"no job", &mcpbridge.Session{RunID: "run-1", Handle: "dev", Surface: emptySurface()}},
		// A JOB A TOKEN CANNOT CARRY: "~" ends the job in a token's subject
		// and "." ends the subject, so either would mint a token that names
		// some other job.
		{"a job with the subject's separator", &mcpbridge.Session{RunID: "run-1",
			Launch: "job~1", Handle: "dev", Surface: emptySurface()}},
		{"a job with the token's separator", &mcpbridge.Session{RunID: "run-1",
			Launch: "job.1", Handle: "dev", Surface: emptySurface()}},
		{"no surface", &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev"}},
		// A SURFACE NO TURN WAS BOUND TO names no seat, and every bridged
		// call acts as the run's seat — so it would answer the handshake
		// and then decide nothing it was asked.
		{"no seat", &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev",
			Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil)}},
		{"nothing at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if url := b.Open(t.Context(), tc.session); url != "" {
				t.Errorf("endpoint = %q, want empty", url)
			}
		})
	}
	if b.Live() != 0 {
		t.Errorf("%d incomplete sessions were registered", b.Live())
	}
}

// A BRIDGE NOTHING REACHES OPENS NO SESSION. A node that serves no route
// (api.port 0) and has joined no fleet answers nothing a box could dial, so the
// box would launch with a tool server that refuses every connection. The same
// bridge opens the same session once a listener has taken its handler, or a
// bridge that always refused would pass.
func TestABridgeNothingReachesOpensNoSession(t *testing.T) {
	t.Parallel()
	b := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "k"), BaseURL: "http://x"})
	session := &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev", Surface: emptySurface()}
	if b.Reachable() {
		t.Fatal("a bridge nothing mounted or joined reports itself reachable")
	}
	if url := b.Open(t.Context(), session); url != "" {
		t.Errorf("endpoint = %q for a bridge nothing reaches, want empty", url)
	}
	if b.Live() != 0 {
		t.Errorf("%d sessions live on a bridge nothing reaches", b.Live())
	}

	_ = b.Handler()
	if !b.Reachable() {
		t.Fatal("building the handler did not make the bridge reachable")
	}
	if url := b.Open(t.Context(), session); url == "" {
		t.Error("a mounted bridge refused a complete session")
	}
	var unset *mcpbridge.Bridge
	if unset.Reachable() {
		t.Error("a nil bridge reports itself reachable")
	}
}

func emptySurface() *tools.Surface {
	return tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).
		ForTurn(seatTurn())
}

// seatTurn is the turn every fixture surface is bound to: a bridged run is one
// seat's executor, and the bridge acts as that seat on every call.
func seatTurn() *turnctx.Turn {
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "dev"}
	return &turnctx.Turn{
		RunID: "run-1", Seat: seat,
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}},
	}
}

// A TOKEN THIS FLEET SIGNED THAT NAMES NO SESSION HERE IS DIAGNOSABLE.
//
// The response deliberately cannot say why — forged, expired and "that run is
// over" are three different facts, and naming one tells an attacker the same.
// But the operator needs the difference in the log: a node that has joined no
// fleet holds the only sessions it can answer for, and a signed token it holds
// nothing for is a run that ended or one it has not taken back yet — never a
// forgery.
func TestAnUnresolvedTokenSaysWhyInTheLogNotTheResponse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// A session THIS node opened, then a PEER over the same fleet key that
	// has joined no fleet to find the session's holder in.
	endpoint := f.open(t)
	token := endpoint[strings.LastIndex(endpoint, "/")+1:]

	peer := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	})
	runID, reason := peer.Miss(token)
	if runID != "run-1" {
		t.Fatalf("the peer could not even name the run: %q", runID)
	}
	if !strings.Contains(reason, "no fleet") {
		t.Errorf("the reason does not say the node has no fleet to find the holder in: %q", reason)
	}

	// A FORGED token is the other reason, and it names no run — telling
	// the two apart is the whole point.
	forgedID, forgedReason := peer.Miss("v1.run-1.9999999999.notasignature")
	if forgedID != "" {
		t.Errorf("a forged token resolved to run %q", forgedID)
	}
	if !strings.Contains(forgedReason, "forged") || strings.Contains(forgedReason, "fleet") {
		t.Errorf("a forged token was blamed on anything but itself: %q", forgedReason)
	}
}

// token mints the path token for a run the fixture's bridge holds a session
// for, so a case can hand the transport exactly what a box would.
func (f *fixture) token(t *testing.T, runID string) string {
	t.Helper()
	url := f.bridge.Open(t.Context(), &mcpbridge.Session{
		RunID: runID, Launch: "job-of-" + runID, Handle: "dev", Role: "Engineer", Surface: f.surface,
	})
	if url == "" {
		t.Fatal("no endpoint")
	}
	tok := strings.TrimPrefix(url, f.server.URL+mcpbridge.PathPrefix)
	f.bridge.Close(runID)
	return tok
}

// TestTheBridgeAdvertisesTheHintsTheRegistryHolds is the finding. Every tool
// the bridge served carried a name, a description and a schema and nothing
// else — so a coding agent's own MCP client saw `read_page` and
// `post_message` as identically unannotated, with nothing to ask a person on
// before an irreversible call. The registry had carried both decisions the
// whole time; the edge that hands the catalogue out dropped them.
func TestTheBridgeAdvertisesTheHintsTheRegistryHolds(t *testing.T) {
	t.Parallel()
	sess := dial(t, newFixture(t).open(t))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	seen := map[string]*mcp.ToolAnnotations{}
	for _, tool := range listed.Tools {
		seen[tool.Name] = tool.Annotations
	}
	read, held := seen["read_page"]
	if !held {
		t.Fatal("read_page was not advertised")
	}
	if read == nil || !read.ReadOnlyHint {
		t.Fatalf("read_page advertises %+v — a client cannot tell it from a "+
			"write, which is the only thing it has to ask a person on", read)
	}
	write, held := seen["post_message"]
	if !held {
		t.Fatal("post_message was not advertised")
	}
	switch {
	case write == nil:
		t.Fatal("post_message advertises no hints at all")
	case write.ReadOnlyHint:
		t.Fatal("a write advertises read-only")
	case write.OpenWorldHint == nil || !*write.OpenWorldHint:
		t.Fatalf("post_message's open-world hint is %v — an absent one is "+
			"what makes a private write read as a shared one",
			write.OpenWorldHint)
	}
}
