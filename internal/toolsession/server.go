package toolsession

import (
	"context"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
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
	// NO CONTEXT, deliberately. The only one a session could hand over is
	// the MCP request's, and a call must never run under it: a harness that
	// gives up on a call can cancel its request, one that dies can end the
	// connection it came on, and a tool half-way through a write would stop
	// there. So a call runs under the context of the run the dispatcher
	// serves, which it holds, and the request's is not even on offer to be
	// used by mistake: all it is handed of the request is whether anybody can
	// still read the answer ([Call.Abandoned]), when the request is over on its
	// transport ([Call.Over]), and a way to say the call has started running
	// ([Call.Running]).
	//
	// A DISPATCHER MAY ANSWER A CALL BEFORE ITS TOOL RETURNS, and run the
	// tool on: the tool loop answers a call "still running" ahead of its
	// harness's own tool timeout, and the bridge answers every call still
	// running as the opening it was made on ends (internal/api/mcpbridge). The
	// call is then the dispatcher's to finish and to record, and the server
	// writes the answer it was handed as it writes any.
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

	// Meta is the request's `_meta`, verbatim. Some harnesses put their own id for
	// the call there (Claude Code's `claudecode/toolUseId`), which is how a call
	// is joined to the model request that asked for it.
	Meta map[string]any

	// Abandoned is closed once NOBODY CAN READ THE ANSWER ANY LONGER and the
	// request shows it: the harness cancelled the request
	// (`notifications/cancelled`, which Claude Code and OpenCode send at
	// their tool timeout, discarding a late answer), the connection the
	// request came on failed (a relay's socket closed), or, on a server that
	// keeps a running call's answer going ([Server.KeepAnswersGoing]), a
	// progress could not be written on the call's response: its harness hung
	// up on it, and the streamable HTTP transport keeps no stream a harness
	// could come back to, so the answer has no reader. A harness that stops
	// waiting with none of those leaves it open: one that hangs up on a POST
	// nothing is kept going on, or in the last cadence before its call
	// returns. Nil, never closed, for a call no request carries.
	//
	// A SIGNAL, NEVER A CONTEXT, for the reason [Dispatcher.Call] takes none:
	// the call runs under its run's context whatever becomes of the request,
	// and this says only, once it has run, whether its answer still had a
	// reader — which a dispatcher that records the call records beside it.
	Abandoned <-chan struct{}

	// Over is closed once the request is OVER ON ITS TRANSPORT, nothing more
	// to be written for it: its answer's write has returned — written,
	// refused, or failed — or the request was cancelled, or the connection it
	// came on failed. For a request neither cancelled nor on a connection
	// that failed, NEVER BEFORE THE ANSWER'S WRITE HAS RETURNED, which is
	// what a dispatcher that answers a call while its tool runs on waits for
	// before it closes the session the call came on: the SDK refuses every
	// answer not yet written once a session's close has begun. It is the
	// request's context's Done, which the SDK ends only after the answer's
	// write (go-sdk v1.8.0; held by this package's
	// TestACallsRequestIsOverOnlyOnceItsAnswerIsWritten, since it is the SDK's
	// behaviour and not its promise), unless it cancels the request first: on
	// the harness's `notifications/cancelled`, while the answer is still to
	// come, and on the connection's failure. Then nobody reads the answer, so a
	// wait on Over that lets go before its write loses nothing. And unlike
	// [Call.Abandoned] nothing here widens it: a progress that finds the call's
	// response gone, and the handler's own return, close Abandoned and leave
	// Over to the transport. Nil, never closed, for a call no request carries.
	Over <-chan struct{}

	// Running is for the dispatcher to call once, the moment it starts
	// RUNNING the call, past every answer it gives without running it; nil
	// on a server that keeps no running call's answer going
	// ([Server.KeepAnswersGoing]). The answer starts there: the call's first
	// progress is written before Running returns, and another every cadence
	// after it until the dispatcher returns.
	//
	// THE DISPATCHER'S TO CALL, because only it knows where a call stops
	// being one it answers at once — refused for a run that is over, for
	// tools that have closed, for arguments that did not parse — and becomes
	// one whose tool may take minutes. A call answered without running is
	// answered before its harness could give up on it, and needs nothing
	// kept going.
	Running func()
}

// Result is what the harness is handed back for one call.
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
// harness, Copilot and OpenCode were measured offering an activated tool one
// request late, a request spent on a model that could not yet call it.
func (s *Server) HoldAnswersForRelist() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdLists = true
}

// KeepAnswersGoing is for a transport whose harness gives up on a call whose
// response stays silent: every call the dispatcher runs ([Call.Running]) has
// a `notifications/progress` written on its own progress token the moment it
// starts running, another every `every` while its tool runs, and none once
// its tool has returned. So the response's first byte leaves at once, and the
// response is never silent for longer than every — see keepalive.go.
//
// A CALL WHOSE REQUEST CARRIES NO PROGRESS TOKEN GETS ITS ANSWER ALONE: the
// protocol ties a progress notification to a token the requester gave, and a
// client that gave none has said it reads none.
//
// THE CALLER CHOOSES every, because the harnesses' silence checks it has to
// answer are the transport's, not this package's (the bridge's, for the
// harnesses that dial it over HTTP).
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
		return &mcp.CallToolResult{
			IsError: res.Failed,
			Content: []mcp.Content{&mcp.TextContent{Text: res.Output}},
		}, nil
	}
}
