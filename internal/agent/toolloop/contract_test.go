package toolloop_test

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession/toolsessiontest"
)

// THE TOOL LOOP'S DISPATCHER HOLDS THE GATES EVERY DISPATCHER HOLDS — the
// contract the bridge's runs as well (internal/api/mcpbridge). Each case is a
// phase run by toolloop.Run, whose CLI run the suite plays: it reaches the
// run's session over its unix socket, as the relay a CLI starts does.
func TestTheDispatcherKeepsTheToolSessionContract(t *testing.T) {
	t.Parallel()
	toolsessiontest.Run(t, func(t *testing.T, spec toolsessiontest.Spec) *toolsessiontest.Served {
		cli := &suiteCLI{served: make(chan suiteRun, 1), end: make(chan struct{})}
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			// The phase's error is the suite's to read off the record: a
			// fence or a panic cuts the phase, and Run says so.
			_, _ = toolloop.Run(spec.Context, toolloop.Config{
				Provider: cli, Surface: contractSurface(t, spec), Host: host(t),
				Messages: opening(), MaxRounds: 8, Fence: spec.Fence,
			})
		}()
		var r suiteRun
		select {
		case r = <-cli.served:
		case <-ended:
			t.Fatal("the phase ended before its CLI run was served")
		}
		var once sync.Once
		return &toolsessiontest.Served{
			Dial: func(t *testing.T) (mcp.Transport, func()) {
				conn, err := net.Dial("unix", r.socket)
				if err != nil {
					t.Fatalf("reach the run's session: %v", err)
				}
				return &mcp.IOTransport{Reader: conn, Writer: conn}, func() { _ = conn.Close() }
			},
			Dispatcher: toolloop.RunDispatcher(r.host),
			End: func(*testing.T) {
				once.Do(func() { close(cli.end) })
				<-ended
			},
			Recorded: func(*testing.T) []toolsessiontest.Recorded {
				var out []toolsessiontest.Recorded
				for _, c := range toolloop.Recorded(r.host) {
					out = append(out, toolsessiontest.Recorded{Seq: c.Seq, Name: c.Name,
						Output: c.Result.Output, Failed: c.Result.Failed, Class: c.Result.Refusal()})
				}
				return out
			},
			UnparsedArguments: toolloop.UnparsedArguments,
		}
	})
}

// suiteCLI is the CLI the suite plays. Its one run is served as a CLI whose
// record the host does not read — so every call is placed the moment it
// arrives, never held for a record that will not come — and that re-lists its
// tools when told they changed. It stays out until the suite ends it: a CLI the
// host decided to stop is one it has not stopped yet, and its calls still
// arrive. A later run of the phase calls nothing.
type suiteCLI struct {
	served chan suiteRun
	end    chan struct{}
	runs   atomic.Int32
}

// suiteRun is the run the suite drives: its host, and its session's socket.
type suiteRun struct {
	host   llm.ToolHost
	socket string
}

func (*suiteCLI) Model() string { return "toolsessiontest" }

func (c *suiteCLI) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	done := &llm.Completion{Model: c.Model(), Content: "done", StopReason: llm.StopEnd}
	if req.Tools == nil || c.runs.Add(1) > 1 {
		return done, nil
	}
	served, err := req.Tools.Serve(ctx, llm.ServeOptions{Relists: true, Concurrency: llm.CallsAtOnce})
	if err != nil {
		return nil, err
	}
	// RELEASED AS A BACKEND RELEASES A RUN: once its CLI is gone, which lets
	// every answer the host withheld go, and retires the session.
	defer served.Release()
	c.served <- suiteRun{host: req.Tools, socket: served.Args[len(served.Args)-1]}
	select {
	case <-c.end:
	case <-ctx.Done():
	}
	return done, nil
}

// contractSurface is the suite's tools on a seat's surface, as a phase's is:
// each a seat tool, called for the turn, on a turn whose call log counts its
// calls — the suite's suspending tool among them, through [parkingSurface].
func contractSurface(t *testing.T, spec toolsessiontest.Spec) toolloop.Surface {
	t.Helper()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "dev"}
	turn := &turnctx.Turn{RunID: "run-1", Seat: seat,
		Org:   &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}},
		Calls: turnctx.NewCallLog()}
	var surf *tools.Surface
	reg := tools.NewRegistry()
	suspends := map[string]bool{}
	for _, tool := range spec.Tools {
		if err := reg.Register(&suiteTool{tool: tool, surface: func() *tools.Surface { return surf }},
			tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.Name, err)
		}
		suspends[tool.Name] = tool.Suspends
	}
	surf = tools.NewSurface("execute", reg.Snapshot(), spec.Offered).ForTurn(turn)
	return parkingSurface{Surface: surf, suspends: suspends}
}

// parkingSurface is a seat's surface on which the suite's suspending tool asks
// the run to park, as a tool whose work outlives its call does: it runs alone
// ([toolloop.Detacher]) and its result carries [toolloop.ToolResult.Suspend].
// A seat's own tools never park a phase — only the Dashboard Assistant's do —
// so the gate a phase that may not suspend holds is proven against this
// stand-in.
type parkingSurface struct {
	*tools.Surface
	suspends map[string]bool
}

var _ toolloop.Detacher = parkingSurface{}

func (s parkingSurface) Detached(name string) bool { return s.suspends[name] }

func (s parkingSurface) Admit(name string, args map[string]any) toolloop.Admitted {
	admitted := s.Surface.Admit(name, args)
	if !s.suspends[name] {
		return admitted
	}
	return func(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
		res, err := admitted(ctx, call)
		res.Suspend = true
		return res, err
	}
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
