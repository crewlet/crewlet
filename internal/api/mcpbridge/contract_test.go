package mcpbridge_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession/toolsessiontest"
)

// THE BRIDGE'S DISPATCHER HOLDS THE GATES EVERY DISPATCHER HOLDS — the
// contract the tool loop's runs as well (internal/agent/toolloop). Each case
// is one detached run's session, opened on a bridge mounted on a live
// listener, which the suite reaches over the run's minted URL as the CLI in a
// box does. The session names its caller, the seat its surface is bound to,
// because the suite's surface is one that parks ([parking]) rather than the
// seat's own; a seat's own session is held to the same gates by gates_test.
func TestTheBridgeKeepsTheToolSessionContract(t *testing.T) {
	t.Parallel()
	toolsessiontest.Run(t, func(t *testing.T, spec toolsessiontest.Spec) *toolsessiontest.Served {
		mux := http.NewServeMux()
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		bridge := bridgeFor(t, mcpbridge.Options{
			Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL,
		})
		mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
		calls := &ledger{}
		surface := contractSurface(t, spec)
		session := &mcpbridge.Session{
			RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
			Surface: surface, Caller: actingSeat{surface.Surface},
			Ledger: calls, Fence: spec.Fence,
		}
		url := bridge.Open(spec.Context, session)
		if url == "" {
			t.Fatal("no endpoint was minted")
		}
		return &toolsessiontest.Served{
			Dial: func(t *testing.T) (mcp.Transport, func()) {
				box := newBoxLine(t)
				return &mcp.StreamableClientTransport{Endpoint: url,
					HTTPClient: &http.Client{Transport: box}}, box.cut
			},
			Dispatcher: mcpbridge.Dispatcher(session),
			End:        func(*testing.T) { bridge.Close(session.RunID) },
			Recorded: func(*testing.T) []toolsessiontest.Recorded {
				var out []toolsessiontest.Recorded
				for _, row := range calls.rows() {
					out = append(out, toolsessiontest.Recorded{Seq: row.Seq, Name: row.Name,
						Output: row.Output, Failed: row.Failed, Class: row.Refusal()})
				}
				return out
			},
			UnparsedArguments: toolloop.UnparsedArguments,
		}
	})
}

// boxLine is a box's network to the engine, with a transport of its own: every
// request it has in flight can be cut at once, as a box that dies cuts them —
// with no DELETE, which a dead box never sends, and which the SDK's own close
// holds until the calls in flight are answered.
type boxLine struct {
	base *http.Transport
	gone chan struct{}
	once sync.Once
}

func newBoxLine(t *testing.T) *boxLine {
	t.Helper()
	l := &boxLine{base: &http.Transport{}, gone: make(chan struct{})}
	t.Cleanup(l.base.CloseIdleConnections)
	t.Cleanup(l.cut)
	return l
}

// cut aborts every request in flight, and refuses every one after.
func (l *boxLine) cut() { l.once.Do(func() { close(l.gone) }) }

func (l *boxLine) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-l.gone:
		return nil, errors.New("the box is gone")
	default:
	}
	ctx, abort := context.WithCancel(r.Context())
	go func() {
		select {
		case <-l.gone:
			abort()
		case <-ctx.Done():
		}
	}()
	resp, err := l.base.RoundTrip(r.WithContext(ctx))
	if err != nil {
		abort()
		return nil, err
	}
	resp.Body = &abortOnClose{ReadCloser: resp.Body, done: abort}
	return resp, nil
}

// abortOnClose lets a request's context go once its body is read.
type abortOnClose struct {
	io.ReadCloser
	done func()
}

func (b *abortOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.done()
	return err
}

// contractSurface is the suite's tools on the run's seat's surface: each a
// seat tool, called for the run's turn, whose call log counts its calls.
func contractSurface(t *testing.T, spec toolsessiontest.Spec) parking {
	t.Helper()
	turn := seatTurn()
	turn.Calls = turnctx.NewCallLog()
	var surf *tools.Surface
	reg := tools.NewRegistry()
	parks := map[string]bool{}
	for _, tool := range spec.Tools {
		seat := &suiteTool{tool: tool, surface: func() *tools.Surface { return surf }}
		if err := reg.Register(seat, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.Name, err)
		}
		parks[tool.Name] = tool.Suspends
	}
	surf = tools.NewSurface("execute", reg.Snapshot(), spec.Offered).ForTurn(turn)
	return parking{Surface: surf, parks: parks}
}

// parking is the suite's surface with its suspending tools' results asking
// the run to park, as a tool whose work outlives its call answers — what the
// Dashboard Assistant's quick-mode surface does with a proposal. No seat tool
// asks for it, so the suite's are marked here rather than registered as one.
type parking struct {
	*tools.Surface
	parks map[string]bool
}

func (p parking) Admit(name string, args map[string]any) toolloop.Admitted {
	admitted := p.Surface.Admit(name, args)
	if !p.parks[name] {
		return admitted
	}
	return func(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
		res, err := admitted(ctx, call)
		res.Suspend = true
		return res, err
	}
}

// actingSeat decides a call as the seat the suite's surface is bound to: a
// surface that is not the seat's own [*tools.Surface] names its caller.
type actingSeat struct{ surface *tools.Surface }

func (c actingSeat) Bound() bool { return c.surface.Turn().Handle() != "" }

func (c actingSeat) Acting(ctx context.Context) context.Context {
	return iam.WithPrincipal(ctx, turnctx.Principal(c.surface.Turn()))
}

// suiteTool is one of the suite's tools as a seat tool.
type suiteTool struct {
	tool    toolsessiontest.Tool
	surface func() *tools.Surface
}

func (s *suiteTool) Name() string        { return s.tool.Name }
func (s *suiteTool) Description() string { return s.tool.Name + " does a thing" }
func (s *suiteTool) Parameters() map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}}}
}

func (s *suiteTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "unreachable: a seat tool is called for its turn", Failed: true,
		Cause: crewletmcp.RefusalInternalError}, nil
}

func (s *suiteTool) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {
	out := s.tool.Run(ctx, toolsessiontest.ToolCall{
		Args: args, Ordinal: turn.CallLog().Ordinal(s.tool.Name, args), Activate: s.surface().Activate,
	})
	return tools.Result{Output: out}, nil
}
