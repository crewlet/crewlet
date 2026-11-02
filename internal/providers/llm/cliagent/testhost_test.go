package cliagent

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// testHost is a phase's tool host for this package's suite: a REAL
// toolsession server, on a real unix socket the CLI's relay connects to, over
// a table of tools — and a record of everything the backend told it.
//
// It stands where the tool loop's dispatcher stands in production, and keeps
// to the contract as that does: each run gets a session of its own, a call
// runs under the values of the context the run was served under (which is how
// a model call made inside one finds the run's slot), and the backend is told
// when the first call of a stretch starts and the last one ends. Nothing here
// decides anything a phase would — no fence, no budget — because what is under
// test is the backend's half: that the CLI reaches the tools, and that what
// the CLI's record shows reaches the host.
type testHost struct {
	sessions *toolsession.Host

	mu       sync.Mutex
	tools    []llm.ToolDef
	handlers map[string]func(ctx context.Context, args map[string]any) (string, bool)
	calls    []hostCall
	turns    []llm.Turn
	observed chan struct{} // closed and replaced by each Observe
	spent    []llm.Usage
	busy     []bool
	inflight int
	opts     []llm.ServeOptions
	runCtx   context.Context
	pending  []llm.Message
	released []bool
	revision uint64

	done     chan struct{}
	doneOnce sync.Once
}

// hostCall is one call the CLI made, as the server handed it over.
type hostCall struct {
	Name    string
	Args    map[string]any
	Meta    map[string]any
	ArgsErr string
}

var (
	_ llm.ToolHost           = (*testHost)(nil)
	_ toolsession.Dispatcher = (*testHost)(nil)
)

func newTestHost(t *testing.T, names ...string) *testHost {
	t.Helper()
	sessions, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("toolsession.NewHost: %v", err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	h := &testHost{
		sessions: sessions,
		handlers: map[string]func(context.Context, map[string]any) (string, bool){},
		observed: make(chan struct{}),
		done:     make(chan struct{}),
	}
	for _, name := range names {
		h.tools = append(h.tools, llm.ToolDef{Name: name, Description: "the " + name + " tool",
			Parameters: map[string]any{"type": "object"}})
	}
	return h
}

// handle sets what a tool does when called.
func (h *testHost) handle(name string, fn func(ctx context.Context, args map[string]any) (string, bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[name] = fn
}

// end closes Done: the host needs the run to end.
func (h *testHost) end() { h.doneOnce.Do(func() { close(h.done) }) }

func (h *testHost) Serve(ctx context.Context, opts llm.ServeOptions) (llm.Served, error) {
	h.mu.Lock()
	h.opts = append(h.opts, opts)
	// THE RUN'S CONTEXT, values and all, is what its calls run under — see
	// the type's doc.
	h.runCtx = ctx
	h.mu.Unlock()
	session, err := h.sessions.Open(ctx, "crewlet", "cliagent test", h)
	if err != nil {
		return llm.Served{}, err
	}
	command, args := h.sessions.Command(session)
	var once sync.Once
	return llm.Served{Command: command, Args: args, Release: func() bool {
		once.Do(func() {
			session.Retire()
			h.mu.Lock()
			h.released = append(h.released, session.Connected())
			h.mu.Unlock()
		})
		return session.Connected()
	}}, nil
}

func (h *testHost) Observe(t llm.Turn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turns = append(h.turns, t)
	close(h.observed)
	h.observed = make(chan struct{})
}

func (h *testHost) Spent(u llm.Usage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spent = append(h.spent, u)
}

func (h *testHost) Done() <-chan struct{} { return h.done }

func (h *testHost) Halted() bool { return false }

func (h *testHost) Pending() []llm.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.pending)
}

func (h *testHost) Offered() []toolsession.Tool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]toolsession.Tool, 0, len(h.tools))
	for _, def := range h.tools {
		out = append(out, toolsession.Tool{Def: def, Annotations: mcp.Annotations{}})
	}
	return out
}

func (h *testHost) Revision() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.revision
}

func (h *testHost) Call(call toolsession.Call) toolsession.Result {
	h.mu.Lock()
	h.calls = append(h.calls, hostCall{Name: call.Name, Args: call.Arguments, Meta: call.Meta,
		ArgsErr: call.ArgumentsError})
	h.inflight++
	if h.inflight == 1 {
		h.tellBusyLocked(true)
	}
	fn := h.handlers[call.Name]
	// The run's values and none of its cancellation, as the tool loop runs
	// a call: a run that ends cancels nothing a tool is half-way through.
	ctx := context.WithoutCancel(h.runCtx)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.inflight--
		if h.inflight == 0 {
			h.tellBusyLocked(false)
		}
		h.mu.Unlock()
	}()
	if fn == nil {
		return toolsession.Result{Output: call.Name + " ok"}
	}
	out, failed := fn(ctx, call.Arguments)
	return toolsession.Result{Output: out, Failed: failed}
}

// tellBusyLocked is the dispatcher's own rule: the backend is told under the
// host's lock, so a backend that called back into the host from it would
// deadlock here as it would there.
func (h *testHost) tellBusyLocked(busy bool) {
	h.busy = append(h.busy, busy)
	if n := len(h.opts); n > 0 && h.opts[n-1].OnCall != nil {
		h.opts[n-1].OnCall(busy)
	}
}

// snapshot is what the backend told the host, and what the CLI called.
func (h *testHost) snapshot() (turns []llm.Turn, calls []hostCall, spent llm.Usage, spends []llm.Usage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, u := range h.spent {
		spent = addUsage(spent, u)
	}
	return slices.Clone(h.turns), slices.Clone(h.calls), spent, slices.Clone(h.spent)
}

// awaitRecorded waits, as the tool loop's host does before it runs a call,
// for the record to show a request asking for a call to name (the call and
// the record reach the engine by two ways, a socket and the CLI's output, and
// race), reporting whether it did within the host's two seconds.
func (h *testHost) awaitRecorded(ctx context.Context, name string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.waitRecorded(ctx, name)
}

// waitRecorded waits for the record to show a request asking for a call to
// name, woken by each turn the host is told of rather than by a clock, and
// reports false only when ctx ends first.
func (h *testHost) waitRecorded(ctx context.Context, name string) bool {
	for {
		h.mu.Lock()
		found := slices.ContainsFunc(h.latestTurnsLocked(), func(turn llm.Turn) bool {
			return slices.ContainsFunc(turn.Calls, func(c llm.TurnCall) bool { return c.Name == name })
		})
		observed := h.observed
		h.mu.Unlock()
		if found {
			return true
		}
		select {
		case <-observed:
		case <-ctx.Done():
			return false
		}
	}
}

// latestTurns is each request the host was told of, as last reported.
func (h *testHost) latestTurns() []llm.Turn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.latestTurnsLocked()
}

func (h *testHost) latestTurnsLocked() []llm.Turn {
	var order []string
	last := map[string]llm.Turn{}
	for _, t := range h.turns {
		if _, seen := last[t.ID]; !seen {
			order = append(order, t.ID)
		}
		last[t.ID] = t
	}
	out := make([]llm.Turn, 0, len(order))
	for _, id := range order {
		out = append(out, last[id])
	}
	return out
}
