package mcpbridge_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession/toolsessiontest"
)

// THE BRIDGE'S DISPATCHER HOLDS THE GATES EVERY DISPATCHER HOLDS — the
// contract the tool loop's runs as well (internal/agent/toolloop). Each case
// is one box-mode run's session, opened on a bridge mounted on a live
// listener, which the suite reaches over the run's minted URL as the CLI in a
// box does.
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
		session := &mcpbridge.Session{
			RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
			Surface: contractSurface(t, spec), Ledger: calls, Fence: spec.Fence,
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
	gone context.Context
	cut  context.CancelFunc
}

func newBoxLine(t *testing.T) *boxLine {
	t.Helper()
	gone, cut := context.WithCancel(context.Background())
	l := &boxLine{base: &http.Transport{}, gone: gone, cut: cut}
	t.Cleanup(l.base.CloseIdleConnections)
	t.Cleanup(cut)
	return l
}

func (l *boxLine) RoundTrip(r *http.Request) (*http.Response, error) {
	if l.gone.Err() != nil {
		return nil, errors.New("the box is gone")
	}
	ctx, abort := context.WithCancel(r.Context())
	stop := context.AfterFunc(l.gone, abort)
	resp, err := l.base.RoundTrip(r.WithContext(ctx))
	if err != nil {
		stop()
		abort()
		return nil, err
	}
	resp.Body = &abortOnClose{ReadCloser: resp.Body, done: func() { stop(); abort() }}
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
func contractSurface(t *testing.T, spec toolsessiontest.Spec) *tools.Surface {
	t.Helper()
	turn := seatTurn()
	turn.Calls = turnctx.NewCallLog()
	var surf *tools.Surface
	reg := tools.NewRegistry()
	for _, tool := range spec.Tools {
		if err := reg.Register(&suiteTool{tool: tool, surface: func() *tools.Surface { return surf }},
			tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.Name, err)
		}
	}
	surf = tools.NewSurface("execute", reg.Snapshot(), spec.Offered).ForTurn(turn)
	return surf
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
