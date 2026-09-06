package mcpbridge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/tools"
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

// activator stands in for activate_tool: a bridged call that widens the very
// surface it was called on, which is the only way a coding agent's active set
// ever moves while its run is in a box.
type activator struct {
	surface func() *tools.Surface
	target  string
}

func (a *activator) Name() string               { return "activate" }
func (a *activator) Description() string        { return "activate " + a.target }
func (a *activator) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (a *activator) Call(context.Context, map[string]any) (tools.Result, error) {
	if a.surface().Activate(a.target) {
		return tools.Result{Output: a.target + " is active"}, nil
	}
	return tools.Result{Output: "no such tool", Failed: true}, nil
}

// ledger records what a resume would read back.
type ledger struct {
	mu    sync.Mutex
	calls []tools.Call
	err   error
}

func (l *ledger) Append(_ context.Context, _ string, c tools.Call) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.calls = append(l.calls, c)
	return nil
}

func (l *ledger) rows() []tools.Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]tools.Call(nil), l.calls...)
}

// denyGuard refuses one tool, standing in for the required-skill gate.
type denyGuard struct{ tool, reason string }

func (g denyGuard) Check(name, _ string) string {
	if name == g.tool {
		return g.reason
	}
	return ""
}

func (g denyGuard) Observe(string, map[string]any) {}

// --- fixture ---------------------------------------------------------------

type fixture struct {
	bridge  *mcpbridge.Bridge
	server  *httptest.Server
	session *mcpbridge.Session
	surface *tools.Surface
	ledger  *ledger
	byName  map[string]*stubTool
}

// newFixture wires a bridge over a two-tool surface and serves it.
func newFixture(t *testing.T, offer ...string) *fixture {
	t.Helper()
	reg := tools.NewRegistry()
	byName := map[string]*stubTool{}
	for _, name := range []string{"read_page", "post_message"} {
		tool := &stubTool{name: name}
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
		byName[name] = tool
	}
	f := &fixture{ledger: &ledger{}, byName: byName}
	// In the universe for every case, offered only where a case names it.
	widen := &activator{surface: func() *tools.Surface { return f.surface }, target: "post_message"}
	if err := reg.Register(widen, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(activate): %v", err)
	}
	if len(offer) == 0 {
		offer = []string{"read_page", "post_message"}
	}
	f.surface = tools.NewSurface("execute", reg.Snapshot(), offer)
	f.bridge = mcpbridge.New(mcpbridge.Options{Key: []byte("test-key")})
	f.session = &mcpbridge.Session{
		RunID: "run-1", Handle: "dev", Role: "Engineer",
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
	f.bridge = mcpbridge.New(mcpbridge.Options{
		Key: []byte("test-key"), BaseURL: f.server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", f.bridge.Handler())
	f.server.Config.Handler = mux
	return f.bridge.Open(f.session)
}

// dial connects an MCP client the way a coding agent's own client would.
func dial(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
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
			res, err := http.Post(f.server.URL+mcpbridge.PathPrefix+tc.token,
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
	res, err := http.Post(url, "application/json",
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

// A DEPLOYMENT WHOSE API IS NOT REACHABLE FROM A BOX cannot bridge, and the
// honest answer is an empty endpoint the caller can refuse agent mode on —
// not a run that starts and fails on its first tool call.
//
// AND NOTHING IS REGISTERED FOR IT. The minted token is the only way to reach
// a session, so one registered with no endpoint is a live surface nothing can
// dispatch to — and the caller, reading the empty answer as a refusal, never
// launches the run whose end would have closed it.
func TestNoBaseURLMintsNoEndpointAndHoldsNoSession(t *testing.T) {
	t.Parallel()
	b := mcpbridge.New(mcpbridge.Options{Key: []byte("k")})
	if url := b.Open(&mcpbridge.Session{
		RunID: "run-1", Surface: emptySurface(),
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
	f.bridge = mcpbridge.New(mcpbridge.Options{
		Key: []byte("test-key"), BaseURL: f.server.URL, TTL: time.Minute,
		Now: func() time.Time { return clock },
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", f.bridge.Handler())
	f.server.Config.Handler = mux
	url := f.bridge.Open(f.session)

	clock = clock.Add(2 * time.Minute)
	res, err := http.Post(url, "application/json",
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

// THE ADVERTISED SET IS THE SURFACE'S LIVE ONE. A server built once when the
// run opened would advertise the set it started with for the run's whole life,
// so a tool the agent activates mid-run would be invisible until it ended.
func TestTheAdvertisedToolsFollowTheLiveSurface(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "read_page", "activate")
	url := f.open(t)
	// ONE SESSION, HELD FOR THE RUN, the way a coding agent's client holds
	// it. A fresh session per list would rebuild the server and pass for a
	// bridge whose connected boxes never see an activation at all.
	sess := dial(t, url)

	names := func() []string {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		listed, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		var out []string
		for _, tool := range listed.Tools {
			out = append(out, tool.Name)
		}
		slices.Sort(out)
		return out
	}

	if got := names(); !slices.Equal(got, []string{"activate", "read_page"}) {
		t.Fatalf("tools = %v", got)
	}
	// Activated the only way a box can activate anything: over the bridge.
	if res := callTool(t, sess, "activate", nil); res.IsError {
		t.Fatalf("activate failed: %s", text(res))
	}
	if got := names(); !slices.Contains(got, "post_message") {
		t.Fatalf("a tool activated mid-run is not advertised on the box's own session: %v", got)
	}
	// Listed AND callable — the server's table is what dispatches, and a
	// name the surface offers but the server never learned is refused as
	// unknown before the surface is asked.
	if res := callTool(t, sess, "post_message", map[string]any{"text": "hi"}); res.IsError {
		t.Errorf("an activated tool is listed but not callable: %s", text(res))
	}
	if seen := f.byName["post_message"].seen(); len(seen) != 1 {
		t.Errorf("post_message ran %d times, want 1", len(seen))
	}
}

// CLOSING A RUN ENDS THE MCP SESSIONS ITS BOX HOLDS, not only the bridge's
// own map entry. The SDK keeps every session a box opened — transport,
// server, and through the server's handlers the seat's live surface — until
// the client sends a DELETE or an idle clock fires, and a box torn down
// mid-run sends nothing. Left there, each agent-mode run this process ever
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
		RunID: "run-1", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger,
	}
	url := f.bridge.Open(second)
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

	fresh := f.bridge.Open(&mcpbridge.Session{
		RunID: "run-1", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger,
	})
	if fresh == stale {
		t.Fatal("the relaunched run was handed the superseded box's own URL")
	}
	res, err := http.Post(stale, "application/json",
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
	b := mcpbridge.New(mcpbridge.Options{Key: []byte("k"), BaseURL: "http://x"})
	for _, tc := range []struct {
		name    string
		session *mcpbridge.Session
	}{
		{"no run id", &mcpbridge.Session{Handle: "dev", Surface: emptySurface()}},
		{"no surface", &mcpbridge.Session{RunID: "run-1", Handle: "dev"}},
		{"nothing at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if url := b.Open(tc.session); url != "" {
				t.Errorf("endpoint = %q, want empty", url)
			}
		})
	}
	if b.Live() != 0 {
		t.Errorf("%d incomplete sessions were registered", b.Live())
	}
}

func emptySurface() *tools.Surface {
	return tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil)
}

// A TOKEN THIS FLEET SIGNED THAT NAMES NO SESSION HERE IS DIAGNOSABLE.
//
// The response deliberately cannot say why — forged, expired and "that run is
// over" are three different facts, and naming one tells an attacker the same.
// But the operator needs the fourth: a bridge URL that resolves to a node
// other than the one holding the session answers 401 to every call of a LIVE
// run, forever, and without this it is indistinguishable from a forged token.
func TestAnUnresolvedTokenSaysWhyInTheLogNotTheResponse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// A session THIS node opened, then a PEER over the same fleet key —
	// which is exactly a load balancer in front of two nodes.
	endpoint := f.open(t)
	token := endpoint[strings.LastIndex(endpoint, "/")+1:]

	peer := mcpbridge.New(mcpbridge.Options{
		Key: []byte("test-key"), BaseURL: "https://engine.example.com",
	})
	runID, reason := peer.Miss(token)
	if runID != "run-1" {
		t.Fatalf("the peer could not even name the run: %q", runID)
	}
	if !strings.Contains(reason, mcpbridge.BaseURLVar) {
		t.Errorf("the reason does not name the setting that fixes it: %q", reason)
	}

	// A FORGED token is the other reason, and it names no run — telling
	// the two apart is the whole point.
	forgedID, forgedReason := peer.Miss("v1.run-1.9999999999.notasignature")
	if forgedID != "" {
		t.Errorf("a forged token resolved to run %q", forgedID)
	}
	if strings.Contains(forgedReason, mcpbridge.BaseURLVar) {
		t.Errorf("a forged token was blamed on the deployment: %q", forgedReason)
	}
}

// token mints the path token for a run the fixture's bridge holds a session
// for, so a case can hand the transport exactly what a box would.
func (f *fixture) token(t *testing.T, runID string) string {
	t.Helper()
	url := f.bridge.Open(&mcpbridge.Session{
		RunID: runID, Handle: "dev", Role: "Engineer", Surface: f.surface,
	})
	if url == "" {
		t.Fatal("no endpoint")
	}
	tok := strings.TrimPrefix(url, f.server.URL+mcpbridge.PathPrefix)
	f.bridge.Close(runID)
	return tok
}
