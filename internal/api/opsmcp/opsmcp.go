// Package opsmcp serves the company's own tracker and knowledge base to an
// operator's AI assistant, over MCP.
//
// # Why this exists
//
// The premise of running Crewlet is that an AI manages your company. The
// person doing that management is very often working through an AI of their
// own — a coding agent, an assistant, whatever they already have open — and
// that assistant needs to be able to ask what is on the board, file the thing
// they just decided, and read what the company has written down. Without this
// they are reduced to describing the dashboard to it.
//
// # It is the SAME TOOLS a seat holds, with a different writer
//
// Not a parallel implementation. Every tool here is the one from
// [internal/agent/builtin], constructed with [builtin.WorkDeps.Actor] set —
// so a schema, a default, a trimmed field and the wording of a refusal are
// each written once. Two copies of "file an item" drift on exactly the parts
// nobody looks at, and only one of the two is ever tested.
//
// What differs is WHO the write is attributed to. A seat's writes carry its
// handle and its turn; these carry the operator's token label and
// [tracker.AuthorOperator], so a person and the credential they used are two
// separate facts on the record and an audit can tell an operator's edit from
// an agent's.
//
// # It serves what the CURRENT revision runs
//
// Which halves are served — the engine's own tracker, its own knowledge base,
// search over whichever knowledge base the company runs — is the current
// revision's to say, and a live apply moves it. So nothing here is decided at
// boot: the catalogue is brought into line on every request and every call is
// answered against the surface as it is when the call runs. See [Server].
//
// # It is ALWAYS authenticated
//
// Unlike the sandbox bridge at [mcpbridge.PathPrefix], which authenticates
// with a signed per-run token in its own path because the box inside holds no
// API credential, this surface is reached by a person's own client and is
// guarded by the ordinary operator bearer token — the same one /config and
// /secrets take. It WRITES to the company, so `allow_anonymous_read` does not
// reach it: a write is a write whatever reads are open.
package opsmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/logging"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

var log = logging.Get("api.opsmcp")

// Path is the route this surface is mounted at.
//
// UNDER ITS OWN PREFIX rather than under /mcp/, which the auth package exempts
// wholesale for the sandbox bridge — mounting here would have put a writable
// company surface behind no credential at all, and the collision with the
// bridge's own `/mcp/{token}` pattern would have made which one answered a
// question of registration order.
const Path = "/operator/mcp"

// serverName is what an MCP client lists this server as.
const serverName = "crewlet-operator"

// Half is one of the three things this surface serves, each switched by a
// setting of its own — which is what a call to a tool whose half has closed
// is refused naming.
type Half string

// The halves.
const (
	// Work is the engine's own tracker, on `tracker.backend`.
	Work Half = "work"

	// Pages is the engine's own knowledge base, on `knowledge.backend`.
	Pages Half = "pages"

	// Knowledge is ranked search over whichever knowledge base the company
	// runs — on either backend, because an operator's assistant needs to
	// search a Confluence wiki exactly as much as the engine's own pages.
	Knowledge Half = "knowledge"
)

// Valid reports whether h is a half this surface serves.
func (h Half) Valid() bool {
	switch h {
	case Work, Pages, Knowledge:
		return true
	}
	return false
}

// Surface is what this surface serves at one moment.
type Surface struct {
	// Deps are the tools' dependencies for the halves served now. A half
	// that is not served has zero deps, and its tools are omitted on
	// [builtin.OperatorTools]' own rule.
	Deps builtin.OperatorDeps

	// Closed is, per half not served now, the sentence saying why — the
	// setting that closed it, named, so whoever reads the refusal knows
	// what to change. A half that is served has no entry.
	Closed map[Half]string
}

// Options configure the surface.
type Options struct {
	// Surface answers what is served NOW, and it is asked on every request
	// and every call. The company's backends are set by its current
	// revision, and a live apply moves them: a surface decided once would
	// go on writing native pages after a move to Confluence, where no
	// search of the company reads them, and would never offer search to a
	// company that gained a knowledge base after this process started.
	Surface func() Surface

	// Company names the company in the server's own title, so an operator
	// with two of these connected can tell which is which. Read once: an
	// MCP client reads the title when it initializes and not again.
	Company string
}

// Server is the operator's MCP surface.
//
// # The catalogue follows the current revision
//
// Every request first brings the advertised catalogue into line with what
// [Options.Surface] serves now — adding and removing tools, which the SDK
// announces to connected sessions as a changed tool list — and every call is
// answered against the surface as it is when the call runs, never against the
// catalogue a session listed. Between a revision closing a half and a client
// re-listing, a call can name a tool the surface no longer serves: that call
// is REFUSED naming the setting that closed it, rather than answered as a tool
// that never existed, because "unknown tool" sends an assistant looking for a
// typo when what changed is the company.
type Server struct {
	srv     *mcp.Server
	surface func() Surface

	mu sync.Mutex
	// listed is the catalogue the SDK advertises now.
	listed map[string]bool
	// halves is the half of every tool this surface has served, kept after
	// the half closes: it is what lets a call to a closed tool name the
	// setting rather than be told the tool does not exist.
	halves map[string]Half
}

// New builds the surface, or nil when there is no surface to ask.
func New(opts Options) *Server {
	if opts.Surface == nil {
		return nil
	}
	title := "Crewlet"
	if name := strings.TrimSpace(opts.Company); name != "" {
		title = name + " (Crewlet)"
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name: serverName, Title: title, Version: "1",
	}, &mcp.ServerOptions{
		// THE TOOLS CAPABILITY ALWAYS, with its list-changed flag. Left
		// to inference it is decided by whether any tool is registered
		// at the instant a session initializes — and a revision closing
		// every half between the request's own check and that instant
		// would tell the session this server has no tools, which a
		// client asks only once. Nothing else is advertised: this surface
		// sends no log messages, so the SDK's default logging capability
		// would promise a feature it never uses.
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{ListChanged: true},
		},
	})
	s := &Server{
		srv: srv, surface: opts.Surface,
		listed: map[string]bool{}, halves: map[string]Half{},
	}
	// EVERY CALL IS ANSWERED HERE, before the SDK looks the name up in
	// the catalogue it advertises — see [Server.answer].
	srv.AddReceivingMiddleware(s.dispatch)
	s.sync()
	return s
}

// Annotations is the behavioural hints this surface advertises for one tool,
// or the zero set for a name it does not serve.
//
// It exists because the hints were invisible from outside: the surface
// published a name, a description and a schema and nothing else, so an
// operator's own assistant saw `search_work_items` and `remove_work_item` as
// identically unannotated and had nothing to ask a person on before an
// irreversible call. Exposing what is advertised is what lets that be
// asserted rather than assumed.
func (s *Server) Annotations(name string) tools.Annotations {
	if s == nil {
		return tools.Annotations{}
	}
	return builtin.AnnotationsFor(name)
}

// Tools names what this surface serves now, in the catalogue's own order.
func (s *Server) Tools() []string {
	if s == nil {
		return nil
	}
	var names []string
	for _, tool := range builtin.OperatorTools(s.surface().Deps) {
		names = append(names, tool.Name())
	}
	return names
}

// sync makes the advertised catalogue the one served now, and reports how many
// tools that is.
//
// UNDER ONE LOCK FROM READ TO WRITE, so two requests racing an apply cannot
// leave the older surface advertised: whichever syncs last read the surface
// last.
func (s *Server) sync() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.surface()
	for name, half := range halvesOf(now.Deps) {
		s.halves[name] = half
	}
	catalogue := builtin.OperatorTools(now.Deps)
	served := make(map[string]bool, len(catalogue))
	for _, tool := range catalogue {
		served[tool.Name()] = true
	}
	var gone []string
	for name := range s.listed {
		if !served[name] {
			gone = append(gone, name)
			delete(s.listed, name)
		}
	}
	if len(gone) > 0 {
		s.srv.RemoveTools(gone...)
	}
	for _, tool := range catalogue {
		if s.listed[tool.Name()] {
			continue
		}
		s.listed[tool.Name()] = true
		s.srv.AddTool(&mcp.Tool{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: tool.Parameters(),
			// AND THE HINTS, which this surface published none of.
			// The engine decides each tool's read-only, destructive,
			// idempotent and open-world hints in one switch and the
			// registry has carried them the whole time — and the two
			// places that hand the catalogue to somebody else's client
			// dropped them, so an operator's assistant saw
			// `search_work_items` and `remove_work_item` as identically
			// unannotated. A client that asks before a destructive call
			// had nothing to ask on.
			Annotations: crewletmcp.SDKAnnotations(
				builtin.AnnotationsFor(tool.Name())),
		}, s.handle)
	}
	return len(catalogue)
}

// halvesOf is which half each tool these deps serve belongs to — read off the
// catalogue itself, one half at a time, so it cannot disagree with which deps
// a tool is offered on.
func halvesOf(deps builtin.OperatorDeps) map[string]Half {
	out := map[string]Half{}
	for half, only := range map[Half]builtin.OperatorDeps{
		Work:      {Work: deps.Work, Leads: deps.Leads, LeadsProject: deps.LeadsProject},
		Pages:     {Pages: deps.Pages},
		Knowledge: {Knowledge: deps.Knowledge, Org: deps.Org},
	} {
		for _, tool := range builtin.OperatorTools(only) {
			out[tool.Name()] = half
		}
	}
	return out
}

// dispatch is the receiving middleware every call passes through.
func (s *Server) dispatch(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if call, ok := req.(*mcp.CallToolRequest); ok && method == "tools/call" {
			if result, answered, err := s.answer(ctx, call.Params.Name,
				call.Params.Arguments); answered {

				return result, err
			}
		}
		return next(ctx, method, req)
	}
}

// handle is the handler every advertised tool is added with.
//
// THE SAME ANSWER AS [Server.dispatch]'s, which reaches every call first: the
// SDK requires a handler per tool, and a second path answering calls on its
// own terms is the drift this surface exists to prevent.
func (s *Server) handle(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result, answered, err := s.answer(ctx, req.Params.Name, req.Params.Arguments)
	if !answered {
		return nil, fmt.Errorf("opsmcp: unknown tool %q", req.Params.Name)
	}
	return result, err
}

// answer answers one call against the surface as it is NOW: the tool run where
// it is served, a refusal naming the setting where its half has closed since
// this surface served it, and answered=false for a name this surface has never
// served — which the SDK answers as the unknown tool it is.
func (s *Server) answer(ctx context.Context, name string,
	raw json.RawMessage) (*mcp.CallToolResult, bool, error) {

	now := s.surface()
	for _, tool := range builtin.OperatorTools(now.Deps) {
		if tool.Name() == name {
			result, err := call(ctx, tool, raw)
			return result, true, err
		}
	}
	s.mu.Lock()
	half, served := s.halves[name]
	s.mu.Unlock()
	why := now.Closed[half]
	if !served || why == "" {
		return nil, false, nil
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{
			Text: name + " is not served now: " + why + ".",
		}},
	}, true, nil
}

// call runs one engine tool on the MCP SDK's terms.
//
// THE OPERATOR ID COMES FROM THE REQUEST'S CREDENTIAL, carried on the
// context by the auth middleware and read by the deps' own Actor function —
// never from an argument. A caller that could name its own actor could file
// work as anybody, which is the same rule a seat's tools follow and the same
// reason.
func call(ctx context.Context, tool tools.Callable, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var args map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("opsmcp: %s: bad arguments: %w", tool.Name(), err)
		}
	}
	result, err := tool.Call(ctx, args)
	if err != nil {
		// A TRANSPORT ERROR, not a tool failure: the caller's context
		// ended. The distinction is the SDK's own — a failed tool is an
		// ordinary result with IsError, and reporting it as a protocol
		// error would make a client retry a refusal.
		return nil, err
	}
	return &mcp.CallToolResult{
		IsError: result.Failed,
		Content: []mcp.Content{&mcp.TextContent{Text: result.Output}},
	}, nil
}

// Handler serves the surface at [Path].
//
// EVERY METHOD, for the reason the sandbox bridge takes every method:
// streamable HTTP is a GET for the server-to-client stream and a DELETE to
// end a session, and a pattern naming one verb answers 405 to the others —
// which an MCP client reports as a transport that does not support streaming
// rather than as a route registered wrong.
func (s *Server) Handler() http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.srv }, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// THE GUARD IS THE APP'S, not a second one here: this path is in
		// [auth.GuardedPrefixes], so a request that reaches this handler
		// has already presented a valid operator token. Reading the id
		// off the context rather than re-checking it is what keeps one
		// decision about who may write.
		operator, ok := auth.OperatorFrom(r.Context())
		if !ok || operator == "" {
			// UNREACHABLE if the guard is mounted, and refused rather
			// than trusted if it somehow is not: this surface writes to
			// the company, and a write with no writer is the one thing
			// it must never record.
			log.WarnContext(r.Context(), "operator_mcp_unguarded",
				"detail", "a request reached the operator MCP surface with no "+
					"operator on its context; the auth guard is not in front of it")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// NOTHING SERVED NOW ANSWERS AS AN UNMOUNTED ROUTE DOES. An
		// endpoint that lists no tools reads to an operator as broken,
		// while one that is not there matches what the company's config
		// says — and the next revision that serves something answers
		// here with no restart.
		if s.sync() == 0 {
			http.NotFound(w, r)
			return
		}
		streamable.ServeHTTP(w, r)
	})
}

// ---- who a write is attributed to -------------------------------------- //

// WorkActor and PageActor read the operator off the request's context.
//
// # An unbound token identifies as itself
//
// A Tier A token has a NAME — the key in `api.auth.tokens` — and that name is
// what lands on the record: `founder`, `ci`, `ops-bot`. It is not a seat and
// must not look like one, so the actor KIND is [tracker.AuthorOperator] — the
// discriminator every renderer and every recipient rule already reads — and
// the credential is recorded again in `OperatorID` so an audit can ask what
// one token did without reasoning about kinds.
//
// The name goes in the author field rather than being left empty because the
// tracker requires one: a record carrying no author is a history row nobody
// can attribute, which is the single thing this surface exists to prevent.
//
// The alternative — asking the caller to name a seat to act as — was rejected:
// it lets anybody with the token write as anybody, and a tracker whose author
// field can be chosen by the writer is not an audit trail.
func WorkActor(ctx context.Context, _ *turnctx.Turn) (builtin.Actor, error) {
	id, ok := auth.OperatorFrom(ctx)
	if !ok || id == "" {
		return builtin.Actor{}, fmt.Errorf("opsmcp: no operator on this request")
	}
	// THE OPERATOR'S OWN NAME IS THE HANDLE, and the kind says it is not a
	// seat. A tracker whose author field is chosen by the writer is not an
	// audit trail, so there is deliberately no way for a caller to name a
	// seat to act as.
	return builtin.Actor{
		Handle: id, Kind: tracker.AuthorOperator, OperatorID: id,
	}, nil
}

// PageActor is [WorkActor] for the knowledge base, and records the SAME
// operator under the SAME name.
//
// THE HANDLE IS THE TOKEN'S OWN NAME, exactly as above. It used to be left
// empty here, and `pages.Actor.Name` falls back to `"operator:" + OperatorID`
// for an actor with no handle — so one person writing through one surface was
// recorded as `founder` on a work commit and `operator:founder` on a page
// commit. The kind is already on the row, in its own column, so the prefix was
// a second encoding of a fact the row carries; what it bought was that the
// audit feed, which is the one screen that reads both histories, showed the
// same person as two people three rows apart, and that a reader filtering on
// a name matched half of what they did.
func PageActor(ctx context.Context, _ *turnctx.Turn) (pages.Actor, error) {
	id, ok := auth.OperatorFrom(ctx)
	if !ok || id == "" {
		return pages.Actor{}, fmt.Errorf("opsmcp: no operator on this request")
	}
	return pages.Actor{Handle: id, Kind: pages.AuthorOperator, OperatorID: id}, nil
}
