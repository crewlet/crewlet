package toolsession

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/logging"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
	"github.com/crewlet/crewlet/internal/tools/toolschema"
)

var log = logging.Get("toolsession")

// unlisted is each tool name already logged as left out for its schema, so
// the warning is once per tool per process.
var unlisted sync.Map

// listable reports whether tool's input schema is one a harness can take: one
// it would refuse would cost the whole server's list in Claude Code, so it is
// left out alone, and logged ([toolschema]).
func listable(tool Tool) bool {
	faults := toolschema.Faults(tool.Def.Parameters)
	if len(faults) == 0 {
		return true
	}
	if _, logged := unlisted.LoadOrStore(tool.Def.Name, struct{}{}); !logged {
		log.Warn("tool_schema_invalid", "tool", tool.Def.Name, "faults", faults)
	}
	return false
}

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
	// Offered is what the harness is offered now, in order.
	Offered() []Tool

	// Revision changes whenever Offered would answer differently, so a server
	// asks for the list only when there is something new in it — rendering
	// it deep-clones every active tool's schema.
	Revision() uint64

	// Call decides and runs one call and answers what the harness is handed back.
	// It never fails as a Go error: a call that could not run is a failed
	// result the model reads, and a session that can no longer serve is the
	// dispatcher's state, which it ends the harness run over itself.
	//
	// NO CONTEXT, deliberately: a call must never run under the MCP request's,
	// since a harness that cancels it or dies would stop a tool half-way
	// through a write. It runs under its run's context, and is handed only
	// [Call.Abandoned], [Call.Over], [Call.Running].
	// A DISPATCHER MAY ANSWER BEFORE ITS TOOL RETURNS, and run it on (the tool
	// loop, ahead of its harness's timeout; internal/api/mcpbridge, as the
	// opening ends), then finishing the call itself.
	Call(call Call) Result
}

// Tool is one offered tool: what a model is given, and the behavioural hints
// an MCP client is given beside it.
//
// The hints are for the harness, not the model. A harness decides from them whether a
// call may run beside another (measured: Claude Code and Qwen Code run two
// calls at once only when both are read-only) and whether to ask a person first
// (Codex runs a read-only tool its approval policy would otherwise refuse) — so
// a tool advertised with none is one the harness treats as an unknown write.
type Tool struct {
	Def         llm.ToolDef
	Annotations crewletmcp.Annotations
}

// Call is one call as the harness made it.
type Call struct {
	Name      string
	Arguments map[string]any

	// ArgumentsError is why the arguments could not be read, when they could
	// not: the dispatcher answers the call with that rather than running it on
	// an empty object, which is a call the model never made.
	ArgumentsError string

	// Header is the HTTP request's header where the call came over HTTP, nil
	// over the relay's stdio: what an HTTP front set on it for its dispatcher
	// to read (internal/api/mcpbridge marks the request a call came on).
	Header http.Header

	// Meta is the request's `_meta`, verbatim. Some harnesses put their own id for
	// the call there (Claude Code's `claudecode/toolUseId`), which is how a call
	// is joined to the model request that asked for it.
	Meta map[string]any

	// Abandoned is closed once NOBODY CAN READ THE ANSWER ANY LONGER and the
	// request shows it: the harness cancelled the request
	// (`notifications/cancelled`, sent by Claude Code and OpenCode at their
	// tool timeout), the connection failed (a relay's socket closed), or, on a
	// server that keeps answers going ([Server.KeepAnswersGoing]), a progress
	// could not be written: the harness hung up, and the streamable HTTP
	// transport keeps no stream to come back to. A harness that stops waiting
	// otherwise (a POST nothing is kept going on, or in the last cadence before
	// the call returns) leaves it open. Nil for a call no request carries.
	//
	// A SIGNAL, NEVER A CONTEXT, for the reason [Dispatcher.Call] takes none:
	// it says only, once the call has run, whether its answer had a reader.
	Abandoned <-chan struct{}

	// Over is closed once the request is OVER ON ITS TRANSPORT, nothing more
	// to be written for it: its answer's write has returned (written, refused
	// or failed), or the request was cancelled, or its connection failed. A
	// dispatcher that answers a call while its tool runs on waits on it before
	// it closes the call's session, since the SDK refuses every answer not yet
	// written once a session's close has begun. It is the request's context's
	// Done: NEVER CLOSED BEFORE THE ANSWER'S WRITE HAS RETURNED unless the
	// request was cancelled or its connection failed, when nobody reads the
	// answer. Why that holds, and why nothing here widens it as [Call.Abandoned]
	// is widened, is in the package doc ("An answer before the tool returns").
	// Nil, never closed, for a call no request carries.
	Over <-chan struct{}

	// Running is for the dispatcher to call once, the moment it starts
	// RUNNING the call; nil on a server that keeps no answer going
	// ([Server.KeepAnswersGoing]). The call's first progress is written before
	// Running returns, and another every cadence until the dispatcher returns.
	//
	// THE DISPATCHER'S TO CALL, because only it knows where a call stops being
	// one answered at once (refused for a run that is over, closed tools,
	// unparsed arguments) and becomes one whose tool may take minutes.
	Running func()
}

// Result is what the harness is handed back for one call.
type Result struct {
	Output string
	Failed bool

	// Images ride after Output as image content, held to [crewletmcp.CheckImages]
	// as they are written: an answer whose images break it is answered failed.
	Images []crewletmcp.Image
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

	// listed is closed the first time a client asks for the tool list: a harness
	// run that never did never had the seat's tools, which is a different
	// failure from a model that had them and called none.
	//
	// THE LIST, NOT THE HANDSHAKE'S LAST STEP. A client on a newer protocol
	// revision may never send `notifications/initialized` (the SDK's own
	// client did not, measured on v1.8.0), and every harness lists the tools
	// before it can offer one to its model.
	listed   chan struct{}
	listOnce sync.Once

	// lists counts the tool lists served, and relisted is closed and
	// replaced as each is — what a held answer waits on
	// ([Server.HoldAnswersForRelist]).
	lists     uint64
	relisted  chan struct{}
	holdLists bool

	// answerEvery is how often a running call's answer is kept going
	// ([Server.KeepAnswersGoing]); zero keeps nothing going.
	answerEvery time.Duration
}

// relistWait bounds how long a call that changed the tool list holds its
// answer for the client's re-list: a harness that re-reads its list when told it
// changed does so within a fraction of a second of the notice (OpenCode's
// served loop, the slowest measured, about 270 ms after it), and two seconds
// is far past that while still short against the model request the answer
// lets the harness make. A client that has not listed by then is answered anyway.
const relistWait = 2 * time.Second

// NewServer builds the server for one dispatcher, named as the harness will list
// it. title is shown in the handshake, for a harness's own logs. instructions,
// where there are any, are the server's own MCP instructions — what it is for —
// which a harness puts in front of its model beside the server's tools: the
// channel a server says what it is for on, where a prompt would be the
// engine's guess at where a harness reads it.
func NewServer(name, title, instructions string, d Dispatcher) *Server {
	s := &Server{d: d, listed: make(chan struct{}), relisted: make(chan struct{})}
	var opts *mcp.ServerOptions
	if instructions != "" {
		opts = &mcp.ServerOptions{Instructions: instructions}
	}
	s.srv = mcp.NewServer(&mcp.Implementation{Name: name, Title: title, Version: "1"}, opts)
	s.srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}
			s.listOnce.Do(func() { close(s.listed) })
			res, err := next(ctx, method, req)
			// SERVED, then counted: an answer held for the re-list goes
			// once the client has the new list, not while it is asking.
			s.mu.Lock()
			s.lists++
			close(s.relisted)
			s.relisted = make(chan struct{})
			s.mu.Unlock()
			return res, err
		}
	})
	s.Sync()
	return s
}

// MCP is the SDK server, for a transport to connect.
func (s *Server) MCP() *mcp.Server { return s.srv }

// Listed is closed once a client has asked for the tool list.
func (s *Server) Listed() <-chan struct{} { return s.listed }

// HoldAnswersForRelist is for a client that re-reads the tool list when told
// it changed: the answer to a call that changed the list waits until the
// client has listed again, up to [relistWait], so the request the answer lets
// the harness make already offers its model the new tools. Answered at once
// instead, the harness asked its model again before its re-list landed — Gemini
// harness and OpenCode were measured offering an activated tool one request
// late, a request spent on a model that could not yet call it.
func (s *Server) HoldAnswersForRelist() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdLists = true
}

// KeepAnswersGoing is for a transport whose harness gives up on a call whose
// response stays silent: every call the dispatcher runs ([Call.Running]) has
// a `notifications/progress` written on its own progress token as it starts,
// another every `every` while its tool runs, and none once it returns — see
// keepalive.go.
//
// A CALL WHOSE REQUEST CARRIES NO PROGRESS TOKEN GETS ITS ANSWER ALONE: a
// client that gave none has said it reads none.
//
// THE CALLER CHOOSES every, because the silence checks it must beat are the
// transport's (the bridge's, for harnesses that dial it over HTTP).
func (s *Server) KeepAnswersGoing(every time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answerEvery = every
}

// Sync brings the advertised list up to what the dispatcher offers.
//
// AFTER EVERY CALL, because a call is what moves it: a harness activates a tool by
// calling `activate_tool` through this same server. The SDK tells a connected
// client the list changed, which the harnesses that re-list mid-run act on at once;
// for the ones that do not, the dispatcher ends the run and the next one starts
// with the new list — see the tool loop.
func (s *Server) Sync() {
	s.sync()
}

// sync is [Server.Sync], answering whether the advertised list changed and how
// many lists had been served when it did.
func (s *Server) sync() (changed bool, listsBefore uint64) {
	rev := s.d.Revision()
	s.mu.Lock()
	defer s.mu.Unlock()
	listsBefore = s.lists
	if s.advertised != nil && s.synced == rev {
		return false, listsBefore
	}
	if s.advertised == nil {
		s.advertised = map[string]struct{}{}
	}
	s.synced = rev
	live := make(map[string]struct{})
	for _, tool := range s.d.Offered() {
		name := tool.Def.Name
		if _, have := s.advertised[name]; have {
			live[name] = struct{}{}
			continue
		}
		if !listable(tool) {
			continue
		}
		live[name] = struct{}{}
		s.srv.AddTool(&mcp.Tool{
			Name:        name,
			Description: tool.Def.Description,
			InputSchema: tool.Def.Parameters,
			Annotations: crewletmcp.SDKAnnotations(tool.Annotations),
		}, s.handler(name))
		s.advertised[name] = struct{}{}
		changed = true
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
		changed = true
	}
	return changed, listsBefore
}

// awaitRelist waits until the client has listed the tools more than after
// times, for at most [relistWait] — see [Server.HoldAnswersForRelist].
func (s *Server) awaitRelist(ctx context.Context, after uint64) {
	timer := time.NewTimer(relistWait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		if s.lists > after {
			s.mu.Unlock()
			return
		}
		next := s.relisted
		s.mu.Unlock()
		select {
		case <-next:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// handler hands one call to the dispatcher.
func (s *Server) handler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// WHETHER THE HARNESS STILL WAITS, and WHEN THE REQUEST IS OVER: the
		// request's context's Done, and nothing else of it ([Call.Abandoned],
		// [Call.Over]).
		call := Call{Name: name, Meta: req.Params.Meta, Abandoned: ctx.Done(), Over: ctx.Done()}
		if req.Extra != nil {
			call.Header = req.Extra.Header
		}
		s.mu.Lock()
		every := s.answerEvery
		s.mu.Unlock()
		var going *beat
		if every > 0 {
			// THE ANSWER KEPT GOING once the dispatcher runs the call
			// ([Server.KeepAnswersGoing]), and the request's end widened to
			// a progress that found the call's response gone.
			going = newBeat(ctx, req.Session, req.Params.GetProgressToken(), every)
			defer going.lose()
			call.Running, call.Abandoned = going.start, going.unread.Done()
		}
		args, err := toolargs.Decode(req.Params.Arguments)
		if err != nil {
			// HANDED ON, NOT ANSWERED HERE: a call whose arguments did not
			// parse is still a call the phase records, and the dispatcher is
			// what answers it — with why, as a failed result the model can
			// fix — so the record and the reply cannot disagree about it.
			call.ArgumentsError = err.Error()
		}
		call.Arguments = args
		res := s.d.Call(call)
		// NOTHING KEPT GOING PAST THE ANSWER: the beat is stopped, and waited
		// for, before the answer is handed back, so no progress races the
		// answer on its response or follows it.
		going.stop()
		// THE CALL MAY HAVE BEEN AN ACTIVATION — see [Server.Sync] — and
		// its answer then waits for the client's re-list where the client
		// re-lists ([Server.HoldAnswersForRelist]).
		changed, before := s.sync()
		s.mu.Lock()
		hold := s.holdLists
		s.mu.Unlock()
		if changed && hold {
			s.awaitRelist(ctx, before)
		}
		return answerOf(name, res), nil
	}
}

// answerOf is res as the harness reads it: its text first, so a client that
// reads no image still reads the answer, then each image. Images the rule
// refuses are never written: the call is answered failed, saying why.
func answerOf(name string, res Result) *mcp.CallToolResult {
	if err := crewletmcp.CheckImages(res.Output, res.Images); err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("%s could not be answered: %v. Nothing about the call was wrong.", name, err),
		}}}
	}
	content := make([]mcp.Content, 0, 1+len(res.Images))
	content = append(content, &mcp.TextContent{Text: res.Output})
	for _, img := range res.Images {
		content = append(content, &mcp.ImageContent{Data: img.Data, MIMEType: img.MIMEType})
	}
	return &mcp.CallToolResult{IsError: res.Failed, Content: content}
}
