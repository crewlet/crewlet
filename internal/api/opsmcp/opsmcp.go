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
// [work.AuthorOperator], so a person and the credential they used are two
// separate facts on the record and an audit can tell an operator's edit from
// an agent's.
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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/work"
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

// Options configure the surface.
type Options struct {
	// Work and Pages are the native backends. Both nil serves nothing and
	// the route is ABSENT — which is the honest shape for a company on
	// Jira and Confluence: there is nothing here it could manage.
	Work  builtin.WorkDeps
	Pages builtin.PageDeps

	// Knowledge is the company's ranked search, or nil.
	Knowledge builtin.KnowledgeSearcher

	// Company names the company in the server's own title, so an operator
	// with two of these connected can tell which is which.
	Company string
}

// Server is the operator's MCP surface.
type Server struct {
	srv   *mcp.Server
	names []string
}

// New builds the surface, or nil when there is nothing to serve.
//
// NIL RATHER THAN AN EMPTY SERVER, so the route is not mounted at all: an
// endpoint that exists and lists no tools reads to an operator as broken,
// while one that is not there matches what their config says.
func New(opts Options) *Server {
	catalogue := builtin.OperatorTools(builtin.OperatorDeps{
		Work: opts.Work, Pages: opts.Pages, Knowledge: opts.Knowledge,
	})
	if len(catalogue) == 0 {
		return nil
	}

	title := "Crewlet"
	if name := strings.TrimSpace(opts.Company); name != "" {
		title = name + " (Crewlet)"
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name: serverName, Title: title, Version: "1",
	}, nil)

	s := &Server{srv: srv}
	for _, tool := range catalogue {
		s.names = append(s.names, tool.Name())
		srv.AddTool(&mcp.Tool{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: tool.Parameters(),
		}, handlerFor(tool))
	}
	return s
}

// Tools names what this surface serves, for the operator log line.
func (s *Server) Tools() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.names...)
}

// handlerFor adapts one engine tool to the MCP SDK's own signature.
//
// THE OPERATOR ID COMES FROM THE REQUEST'S CREDENTIAL, carried on the
// context by the auth middleware and read by the deps' own Actor function —
// never from an argument. A caller that could name its own actor could file
// work as anybody, which is the same rule a seat's tools follow and the same
// reason.
func handlerFor(tool tools.Callable) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, fmt.Errorf("opsmcp: %s: bad arguments: %w", tool.Name(), err)
			}
		}
		result, err := tool.Call(ctx, args)
		if err != nil {
			// A TRANSPORT ERROR, not a tool failure: the caller's context
			// ended. The distinction is the SDK's own — a failed tool is
			// an ordinary result with IsError, and reporting it as a
			// protocol error would make a client retry a refusal.
			return nil, err
		}
		return &mcp.CallToolResult{
			IsError: result.Failed,
			Content: []mcp.Content{&mcp.TextContent{Text: result.Output}},
		}, nil
	}
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
// must not look like one, so the actor kind is [work.AuthorOperator] and the
// handle is left empty rather than filled with a name that would render as a
// colleague in every thread it appears in.
//
// The alternative — asking the caller to name a seat to act as — was rejected:
// it lets anybody with the token write as anybody, and a tracker whose author
// field can be chosen by the writer is not an audit trail.
func WorkActor(ctx context.Context, _ *turnctx.Turn) (work.Actor, error) {
	id, ok := auth.OperatorFrom(ctx)
	if !ok || id == "" {
		return work.Actor{}, fmt.Errorf("opsmcp: no operator on this request")
	}
	return work.Actor{Kind: work.AuthorOperator, OperatorID: id}, nil
}

// PageActor is [WorkActor] for the knowledge base.
func PageActor(ctx context.Context, _ *turnctx.Turn) (pages.Actor, error) {
	id, ok := auth.OperatorFrom(ctx)
	if !ok || id == "" {
		return pages.Actor{}, fmt.Errorf("opsmcp: no operator on this request")
	}
	return pages.Actor{Kind: pages.AuthorOperator, OperatorID: id}, nil
}
