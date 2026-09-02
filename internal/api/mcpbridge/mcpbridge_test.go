package mcpbridge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	if len(offer) == 0 {
		offer = []string{"read_page", "post_message"}
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), offer)

	f := &fixture{surface: surface, ledger: &ledger{}, byName: byName}
	f.bridge = mcpbridge.New(mcpbridge.Options{Key: []byte("test-key")})
	f.session = &mcpbridge.Session{
		RunID: "run-1", Handle: "dev", Role: "Engineer",
		Surface: surface, Ledger: f.ledger,
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
func TestNoBaseURLMintsNoEndpoint(t *testing.T) {
	t.Parallel()
	b := mcpbridge.New(mcpbridge.Options{Key: []byte("k")})
	if url := b.Open(&mcpbridge.Session{
		RunID: "run-1", Surface: emptySurface(),
	}); url != "" {
		t.Errorf("endpoint = %q, want empty", url)
	}
	// The session is still open, so a caller that has its own way to reach
	// the engine is not shut out.
	if b.Live() != 1 {
		t.Errorf("%d sessions live", b.Live())
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
	f := newFixture(t, "read_page")
	url := f.open(t)

	names := func() []string {
		sess := dial(t, url)
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
		return out
	}

	if got := names(); len(got) != 1 || got[0] != "read_page" {
		t.Fatalf("tools = %v", got)
	}
	f.surface.Activate("post_message")
	got := names()
	if len(got) != 2 {
		t.Errorf("a tool activated mid-run is not advertised: %v", got)
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
