package toolsession_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcprelay"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// TestMain answers as the relay when the session under test starts this binary
// as one — exactly what a coding CLI does with the engine's binary.
func TestMain(m *testing.M) {
	mcprelay.DispatchIfRelay()
	os.Exit(m.Run())
}

// fakeDispatcher offers a fixed set, records every call, and can add a tool
// when one is called — an activation.
type fakeDispatcher struct {
	mu       sync.Mutex
	offered  []toolsession.Tool
	revision uint64
	calls    []toolsession.Call

	// activates names a tool whose call adds `adds` to the offered set.
	activates string
	adds      toolsession.Tool

	// hold, when set, is waited on inside a call to the tool it names.
	holdTool string
	hold     chan struct{}
	entered  chan struct{}
}

func tool(name string) toolsession.Tool {
	return toolsession.Tool{Def: llm.ToolDef{
		Name: name, Description: name + " things",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"id": map[string]any{"type": "integer"},
		}},
	}}
}

func (d *fakeDispatcher) Offered() []toolsession.Tool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]toolsession.Tool(nil), d.offered...)
}

func (d *fakeDispatcher) Revision() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.revision
}

func (d *fakeDispatcher) Call(call toolsession.Call) toolsession.Result {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	if call.Name == d.activates {
		d.offered = append(d.offered, d.adds)
		d.revision++
	}
	hold, entered := d.hold, d.entered
	d.mu.Unlock()
	if call.Name == d.holdTool && hold != nil {
		close(entered)
		<-hold
	}
	if call.ArgumentsError != "" {
		return toolsession.Result{Output: "not run: " + call.ArgumentsError, Failed: true}
	}
	raw, _ := json.Marshal(call.Arguments)
	return toolsession.Result{Output: call.Name + " got " + string(raw)}
}

func (d *fakeDispatcher) recorded() []toolsession.Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]toolsession.Call(nil), d.calls...)
}

// host is a session host for one test, removed with it.
func host(t *testing.T) *toolsession.Host {
	t.Helper()
	h, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// client connects an MCP client straight to a session's socket.
func client(t *testing.T, s *toolsession.Session) *mcp.ClientSession {
	t.Helper()
	conn, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "fake-cli", Version: "1"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func names(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var out []string
	for _, tl := range res.Tools {
		out = append(out, tl.Name)
	}
	return out
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

// A CLI STARTS THE RELAY IT IS HANDED AND REACHES THE SEAT'S TOOLS THROUGH IT:
// the command [toolsession.Host.Command] names, run as a CLI runs an MCP server,
// lists what the dispatcher offers and calls it.
//
// Through the real relay rather than a connection to the socket, because the
// command line is the contract a CLI is given and the one part a direct
// connection skips.
func TestACLIReachesTheSeatsToolsThroughTheRelay(t *testing.T) {
	t.Parallel()
	h := host(t)
	d := &fakeDispatcher{offered: []toolsession.Tool{tool("get_item")}}
	s, err := h.Open(t.Context(), "crewlet", "CTO (cto)", d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Retire()

	command, args := h.Command(s)
	if args[0] != mcprelay.Command || args[1] != s.Path() {
		t.Fatalf("the CLI is told to start %s %v, want the relay on the session's socket", command, args)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "fake-cli", Version: "1"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.CommandTransport{Command: exec.Command(command, args...)}, nil)
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	defer cs.Close()

	if got := names(t, cs); len(got) != 1 || got[0] != "get_item" {
		t.Fatalf("the CLI was offered %v, want the dispatcher's one tool", got)
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_item", Arguments: map[string]any{"id": 1},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := text(res); got != `get_item got {"id":1}` {
		t.Errorf("the call answered %q", got)
	}
	if !s.Connected() {
		t.Error("the session does not know a CLI listed its tools")
	}
}

// A LARGE INTEGER ID ARRIVES EXACT, through json.Number: decoded the default
// way, every number is a float64, and an issue id past 2^53 is silently a
// different issue.
func TestALargeIntegerArgumentArrivesExact(t *testing.T) {
	t.Parallel()
	h := host(t)
	d := &fakeDispatcher{offered: []toolsession.Tool{tool("get_item")}}
	s, err := h.Open(t.Context(), "crewlet", "", d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Retire()
	cs := client(t, s)
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_item", Arguments: json.RawMessage(`{"id":9007199254740993}`),
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := d.recorded()[0].Arguments["id"]
	if n, ok := got.(json.Number); !ok || n.String() != "9007199254740993" {
		t.Errorf("the id arrived as %#v, want the exact json.Number", got)
	}
}

// ARGUMENTS THAT DO NOT PARSE REACH THE DISPATCHER SAYING SO, and are not run
// on an empty object — a call the model never made. The dispatcher answers
// them, so what the CLI is told and what the phase records are one decision.
//
// Mutation: answer such a call in the server, or hand it on with no
// ArgumentsError, and the dispatcher's record here is wrong.
func TestArgumentsThatDoNotParseReachTheDispatcherAsSuch(t *testing.T) {
	t.Parallel()
	h := host(t)
	d := &fakeDispatcher{offered: []toolsession.Tool{tool("get_item")}}
	s, err := h.Open(t.Context(), "crewlet", "", d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Retire()
	cs := client(t, s)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_item", Arguments: json.RawMessage(`["a","b"]`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	calls := d.recorded()
	if len(calls) != 1 || calls[0].ArgumentsError == "" {
		t.Fatalf("the dispatcher saw %+v, want one call naming why its arguments could not be read", calls)
	}
	if !res.IsError || !strings.HasPrefix(text(res), "not run:") {
		t.Errorf("the CLI was answered %q (error %v), want the dispatcher's refusal", text(res), res.IsError)
	}
}

// THE CALL'S _meta REACHES THE DISPATCHER VERBATIM, because that is where a
// CLI that names its calls puts the name — the join between a call and the
// model request that asked for it.
func TestTheCallsMetaReachesTheDispatcher(t *testing.T) {
	t.Parallel()
	h := host(t)
	d := &fakeDispatcher{offered: []toolsession.Tool{tool("get_item")}}
	s, err := h.Open(t.Context(), "crewlet", "", d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Retire()
	cs := client(t, s)
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "get_item", Arguments: map[string]any{},
		Meta: mcp.Meta{"claudecode/toolUseId": "toolu_01"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := d.recorded()[0].Meta["claudecode/toolUseId"]; got != "toolu_01" {
		t.Errorf("the dispatcher saw _meta %v", d.recorded()[0].Meta)
	}
}

// AN ACTIVATION IS ADVERTISED ON THE SAME CONNECTION: the CLI that called
// `activate_tool` lists the new tool next, over the session it already holds.
//
// Mutation: drop the Sync after a call and the list stays as it opened.
func TestAnActivationIsAdvertisedOnTheSameConnection(t *testing.T) {
	t.Parallel()
	h := host(t)
	d := &fakeDispatcher{
		offered:   []toolsession.Tool{tool("activate_tool")},
		activates: "activate_tool", adds: tool("post_message"),
	}
	s, err := h.Open(t.Context(), "crewlet", "", d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Retire()
	cs := client(t, s)
	if got := names(t, cs); strings.Join(got, ",") != "activate_tool" {
		t.Fatalf("offered %v before the activation", got)
	}
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "activate_tool", Arguments: map[string]any{},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := names(t, cs)
	if len(got) != 2 {
		t.Fatalf("offered %v after the activation, want post_message beside activate_tool", got)
	}
}

// AN ACTIVATION'S ANSWER WAITS FOR THE CLIENT'S RE-LIST, where the client
// re-lists: answered at once, a CLI asked its model again before its re-list
// landed, and the activated tool reached the model one request late. A client
// that never re-lists is answered anyway, once the wait is over.
func TestAnActivationsAnswerWaitsForTheClientsRelist(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		relists bool
	}{{"a client that re-lists", true}, {"a client that never does", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := host(t)
			d := &fakeDispatcher{
				offered:   []toolsession.Tool{tool("activate_tool")},
				activates: "activate_tool", adds: tool("post_message"),
			}
			s, err := h.Open(t.Context(), "crewlet", "", d)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Retire()
			s.Server().HoldAnswersForRelist()
			const relistDelay = 300 * time.Millisecond
			var cs *mcp.ClientSession
			opts := &mcp.ClientOptions{}
			if tc.relists {
				opts.ToolListChangedHandler = func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
					// A CLI that re-reads its list a moment after the notice.
					time.Sleep(relistDelay)
					_, _ = cs.ListTools(ctx, nil)
				}
			}
			conn, err := net.Dial("unix", s.Path())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			c := mcp.NewClient(&mcp.Implementation{Name: "fake-cli", Version: "1"}, opts)
			cs, err = c.Connect(t.Context(), &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(func() { _ = cs.Close() })
			_ = names(t, cs)
			began := time.Now()
			if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "activate_tool", Arguments: map[string]any{},
			}); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			answered := time.Now()
			if !tc.relists {
				if took := answered.Sub(began); took < time.Second {
					t.Errorf("answered after %s: the hold let the answer go with no re-list", took)
				}
				return
			}
			// The client re-lists no sooner than relistDelay after the
			// notice, so an answer that waited for it took at least that —
			// and was let go once it came, well before the hold's own bound.
			if took := answered.Sub(began); took < relistDelay {
				t.Errorf("answered after %s, before the client could have re-listed", took)
			} else if took > 1500*time.Millisecond {
				t.Errorf("answered after %s: the hold outlasted the client's re-list", took)
			}
		})
	}
}

// THE HINTS TRAVEL WITH THE TOOL, because a CLI decides from them whether a
// call may run beside another and whether to ask first.
func TestAToolsHintsAreAdvertised(t *testing.T) {
	t.Parallel()
	h := host(t)
	readOnly := tool("get_item")
	readOnly.Annotations = crewletmcp.Annotations{ReadOnly: crewletmcp.Yes}
	d := &fakeDispatcher{offered: []toolsession.Tool{readOnly}}
	s, err := h.Open(t.Context(), "crewlet", "", d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Retire()
	res, err := client(t, s).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if a := res.Tools[0].Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("get_item was advertised with hints %+v, want read-only", a)
	}
}

// RETIRING WAITS FOR A CALL THAT IS RUNNING, then leaves nothing to connect
// to. A call is a tool that may be writing, so ending a CLI run does not cut it
// off; and a relay left behind by that run must find no socket, rather than the
// next run's.
//
// Mutation: close the connections without waiting and Retire returns while
// the call is still inside the dispatcher.
func TestRetiringWaitsForARunningCallAndLeavesNoSocket(t *testing.T) {
	t.Parallel()
	h := host(t)
	d := &fakeDispatcher{
		offered:  []toolsession.Tool{tool("slow")},
		holdTool: "slow", hold: make(chan struct{}), entered: make(chan struct{}),
	}
	s, err := h.Open(t.Context(), "crewlet", "", d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cs := client(t, s)
	// RELEASED ON EVERY EXIT, the failing ones included, and registered after
	// the client so it runs before the client's close: a case that stops with
	// the call still held leaves that close waiting on it.
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(d.hold) }) })
	go func() {
		_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}})
	}()
	<-d.entered

	retired := make(chan struct{})
	go func() {
		s.Retire()
		close(retired)
	}()
	select {
	case <-retired:
		t.Fatal("Retire returned while a call was still running")
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(func() { close(d.hold) })
	select {
	case <-retired:
	case <-time.After(5 * time.Second):
		t.Fatal("Retire never returned after the call did")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("the socket is still there after Retire: %v", err)
	}
	if _, err := net.Dial("unix", s.Path()); err == nil {
		t.Error("a retired session accepted a connection")
	}
	s.Retire() // twice is a no-op
}

// THE DIRECTORY IS THIS USER'S ALONE, and goes with the host. It is the whole
// of what admits a connection — a session's socket carries no token.
func TestTheHostsDirectoryIsThisUsersAloneAndGoesWithIt(t *testing.T) {
	t.Parallel()
	h, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	info, err := os.Lstat(h.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("the host's directory is %v, want a 0700 directory", info.Mode())
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(h.Dir()); !os.IsNotExist(err) {
		t.Errorf("the directory outlived the host: %v", err)
	}
}

// A TEMP ROOT TOO DEEP FOR A SOCKET IS REFUSED AT THE START, naming TMPDIR,
// rather than failing every seat's bind with an error that names no setting.
//
// Not parallel: it sets the process's TMPDIR.
func TestATempRootTooDeepForASocketIsRefusedNamingTMPDIR(t *testing.T) {
	deep := t.TempDir()
	for len(deep) < 140 {
		deep = filepath.Join(deep, strings.Repeat("d", 20))
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", deep)
	h, err := toolsession.NewHost()
	if err == nil {
		_ = h.Close()
		t.Fatal("a host was made under a temp root no socket path fits under")
	}
	if !strings.Contains(err.Error(), "TMPDIR") {
		t.Errorf("the refusal does not name TMPDIR: %v", err)
	}
	if entries, _ := os.ReadDir(deep); len(entries) != 0 {
		t.Errorf("a refused host left %d entries behind in the temp root", len(entries))
	}
}
