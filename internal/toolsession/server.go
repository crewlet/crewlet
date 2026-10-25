package toolsession

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Dispatcher is the phase's side of a session: what is offered, and how a call
// is decided and run.
//
// Declared here, by the package that serves it, and implemented by the frame
// that holds the phase — the tool loop. Everything a call is subject to lives
// behind Call: the seat's grant, the skill guard, the fence, the budget, the
// round cap, the terminator and the record. This package decides none of them,
// for the reason the bridge it grew out of states as a prohibition: a second
// idea of what is granted or what gets recorded would drift from the first, and
// it would drift on the security half.
type Dispatcher interface {
	// Offered is what the CLI is offered now, in order.
	Offered() []Tool

	// Revision changes whenever Offered would answer differently, so a server
	// asks for the list only when there is something new in it — rendering
	// it deep-clones every active tool's schema.
	Revision() uint64

	// Call decides and runs one call and answers what the CLI is handed back.
	// It never fails as a Go error: a call that could not run is a failed
	// result the model reads, and a session that can no longer serve is the
	// dispatcher's state, which it ends the CLI run over itself.
	Call(ctx context.Context, call Call) Result
}

// Tool is one offered tool: what a model is given, and the behavioural hints
// an MCP client is given beside it.
//
// The hints are for the CLI, not the model. A CLI decides from them whether a
// call may run beside another (measured: Claude Code and Qwen Code run two
// calls at once only when both are read-only) and whether to ask a person first
// (Codex runs a read-only tool its approval policy would otherwise refuse) — so
// a tool advertised with none is one the CLI treats as an unknown write.
type Tool struct {
	Def         llm.ToolDef
	Annotations crewletmcp.Annotations
}

// Call is one call as the CLI made it.
type Call struct {
	Name      string
	Arguments map[string]any

	// ArgumentsError is why the arguments could not be read, when they could
	// not: the dispatcher answers the call with that rather than running it on
	// an empty object, which is a call the model never made.
	ArgumentsError string

	// Meta is the request's `_meta`, verbatim. Some CLIs put their own id for
	// the call there (Claude Code's `claudecode/toolUseId`), which is how a call
	// is joined to the model request that asked for it.
	Meta map[string]any
}

// Result is what the CLI is handed back for one call.
type Result struct {
	Output string
	Failed bool
}

// Server is one MCP server over a dispatcher: the tool list kept in step with
// what the dispatcher offers, and every call handed to it.
type Server struct {
	srv *mcp.Server
	d   Dispatcher

	mu sync.Mutex
	// synced is the revision the list was last rendered from, and advertised
	// what it carries by name — so a call that changed nothing re-renders
	// nothing, and a client is told the list changed only when it did.
	synced     uint64
	advertised map[string]struct{}

	// listed is closed the first time a client asks for the tool list: a CLI
	// run that never did never had the seat's tools, which is a different
	// failure from a model that had them and called none.
	//
	// THE LIST, NOT THE HANDSHAKE'S LAST STEP. A client on a newer protocol
	// revision may never send `notifications/initialized` (the SDK's own
	// client did not, measured on v1.8.0), and every CLI lists the tools
	// before it can offer one to its model.
	listed   chan struct{}
	listOnce sync.Once
}

// NewServer builds the server for one dispatcher, named as the CLI will list
// it. title is shown in the handshake, for a CLI's own logs.
func NewServer(name, title string, d Dispatcher) *Server {
	s := &Server{d: d, listed: make(chan struct{})}
	s.srv = mcp.NewServer(&mcp.Implementation{Name: name, Title: title, Version: "1"}, nil)
	s.srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				s.listOnce.Do(func() { close(s.listed) })
			}
			return next(ctx, method, req)
		}
	})
	s.Sync()
	return s
}

// MCP is the SDK server, for a transport to connect.
func (s *Server) MCP() *mcp.Server { return s.srv }

// Listed is closed once a client has asked for the tool list.
func (s *Server) Listed() <-chan struct{} { return s.listed }

// Sync brings the advertised list up to what the dispatcher offers.
//
// AFTER EVERY CALL, because a call is what moves it: a CLI activates a tool by
// calling `activate_tool` through this same server. The SDK tells a connected
// client the list changed, which the CLIs that re-list mid-run act on at once;
// for the ones that do not, the dispatcher ends the run and the next one starts
// with the new list — see the tool loop.
func (s *Server) Sync() {
	rev := s.d.Revision()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.advertised != nil && s.synced == rev {
		return
	}
	if s.advertised == nil {
		s.advertised = map[string]struct{}{}
	}
	s.synced = rev
	live := make(map[string]struct{})
	for _, tool := range s.d.Offered() {
		name := tool.Def.Name
		live[name] = struct{}{}
		if _, have := s.advertised[name]; have {
			continue
		}
		s.srv.AddTool(&mcp.Tool{
			Name:        name,
			Description: tool.Def.Description,
			InputSchema: tool.Def.Parameters,
			Annotations: crewletmcp.SDKAnnotations(tool.Annotations),
		}, s.handler(name))
		s.advertised[name] = struct{}{}
	}
	var gone []string
	for name := range s.advertised {
		if _, still := live[name]; !still {
			gone = append(gone, name)
		}
	}
	if len(gone) > 0 {
		s.srv.RemoveTools(gone...)
		for _, name := range gone {
			delete(s.advertised, name)
		}
	}
}

// handler hands one call to the dispatcher.
func (s *Server) handler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		call := Call{Name: name, Meta: req.Params.Meta}
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			// HANDED ON, NOT ANSWERED HERE: a call whose arguments did not
			// parse is still a call the phase records, and the dispatcher is
			// what answers it — with why, as a failed result the model can
			// fix — so the record and the reply cannot disagree about it.
			call.ArgumentsError = err.Error()
		}
		call.Arguments = args
		res := s.d.Call(ctx, call)
		// THE CALL MAY HAVE BEEN AN ACTIVATION — see [Server.Sync].
		s.Sync()
		return &mcp.CallToolResult{
			IsError: res.Failed,
			Content: []mcp.Content{&mcp.TextContent{Text: res.Output}},
		}, nil
	}
}

// decodeArgs turns a call's raw arguments into the map a tool takes.
//
// THROUGH json.Number, so a large id survives: Go's default decodes every JSON
// number to float64, which silently rounds an issue id past 2^53 and sends the
// wrong one to a tracker.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	// A tool with no arguments arrives as an absent or empty message, which is
	// a call with an empty object rather than a malformed one.
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("the arguments are not a JSON object: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
