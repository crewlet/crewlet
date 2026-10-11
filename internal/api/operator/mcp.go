package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
)

// MCPPath is the path the MCP transport is served at — the address an
// assistant is configured with. [MCPPattern] is the route mounted at it.
//
// UNDER ITS OWN PREFIX rather than under /mcp/, which the auth package exempts
// wholesale for the sandbox bridge — mounting here would have put a writable
// company surface behind no credential at all, and the collision with the
// bridge's own `/mcp/{token}` pattern would have made which one answered a
// question of registration order.
const MCPPath = "/operator/mcp"

// MCPPattern is the route the MCP transport is mounted at: a POST, because a
// transport that keeps no session has nothing for the other two verbs of
// streamable HTTP to do (see [guardedMCP]). A GET, which asks for a
// server-to-client stream, and a DELETE, which ends a session, are answered
// the mux's own 405 naming POST — the answer MCP's streamable-HTTP transport
// gives a client for "this server offers no stream", and one carrying a code
// like every other refusal this engine writes.
const MCPPattern = "POST " + MCPPath

// serverName is what an MCP client lists this server as.
const serverName = "crewlet-operator"

// mcpTransport is the catalogue as an MCP server, for an operator's own AI
// assistant.
//
// IT ADMITS AN UNBOUND TOKEN, which is the difference between this transport
// and a person's: an assistant connected with a CI token or an ops-bot token
// is a credential acting as itself, and every write it makes names that
// credential as its author (see [WorkActor]).
type mcpTransport struct {
	srv *mcp.Server

	// handler is the streamable-HTTP handler over srv, the guard's check in
	// front of it — see [Server.MCPHandler] for why there is exactly one.
	handler http.Handler
}

// newMCPTransport registers every catalogue tool on an MCP server, and builds
// the one HTTP handler that serves it.
func newMCPTransport(s *Server, company string) mcpTransport {
	title := "Crewlet"
	if name := strings.TrimSpace(company); name != "" {
		title = name + " (Crewlet)"
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name: serverName, Title: title, Version: "1",
	}, nil)
	for _, tool := range s.catalogue.tools {
		srv.AddTool(&mcp.Tool{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: tool.Parameters(),
			// AND THE HINTS, which this surface once published none of.
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
		}, handlerFor(s, tool.Name()))
	}
	return mcpTransport{srv: srv, handler: guardedMCP(srv)}
}

// handlerFor adapts one catalogue tool to the MCP SDK's own signature.
//
// BY NAME, THROUGH THE SERVER'S DISPATCH, rather than closing over the tool
// value, so the MCP transport reaches a tool by exactly the path every other
// transport does — and is audited by it — and there is no second handle on it
// to fall out of step. An MCP call names no request, so its audit record
// carries none.
//
// THE OPERATOR ID COMES FROM THE REQUEST'S CREDENTIAL, carried on the
// context by the auth middleware and read by the deps' own Actor function —
// never from an argument. A caller that could name its own actor could file
// work as anybody, which is the same rule a seat's tools follow and the same
// reason.
func handlerFor(s *Server, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, fmt.Errorf("operator: %s: bad arguments: %w", name, err)
			}
		}
		result, served, err := s.dispatch(ctx, types.TransportMCP, "", name, args)
		if !served {
			// UNREACHABLE: the SDK dispatches only the names registered
			// above, which are the catalogue's. Refused by name rather
			// than trusted if it somehow is not.
			return nil, fmt.Errorf("operator: %s is not in this company's catalogue", name)
		}
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

// MCPHandler serves the MCP transport at [MCPPattern].
//
// THE SAME HANDLER ON EVERY CALL, built once with the server: the app asks its
// surface for the catalogue on every request (the api package's
// OperatorSurface) and nothing about a request changes how its transport is
// configured. Nothing the handler holds is a client's — see [guardedMCP] — so
// which handler answers a request, of which catalogue and on which node, is
// never something a client can tell.
func (s *Server) MCPHandler() http.Handler { return s.mcp.handler }

// MCPMaxBody bounds one MCP request's body.
//
// The transport reads a request's body whole before it dispatches it, so this
// is what one request may hold in memory. FOUR MEBIBYTES — the SDK's own
// default, stated here rather than inherited, since it is a promise this
// surface makes — because every catalogue call fits far below it: the largest
// is a page's body, at most twice [pages.MaxBody] once escaped as a JSON
// string, which [MaxActBody] derives. The one argument it does bound is
// `write_project_file`'s content, the whole file in one string; a file that
// does not fit goes up through `PUT /work/files/{project}/{path...}`, which
// streams its body to the object store instead of holding it.
const MCPMaxBody = 4 << 20

// guardedMCP is srv's one streamable-HTTP handler, behind a check that the
// app's guard has run — see [Server.MCPHandler].
//
// STATELESS: each request is served by a session made for it and closed when
// it ends, and nothing — no session, no goroutine, no Mcp-Session-Id — is kept
// between two of them. The transport's sessions were state this surface never
// used: a tool call reads its credential off its own request and its
// arguments off its own body, and nothing here sends a client progress, a log
// line, a sampling request or an elicitation. Kept, they cost three things.
// A FLEET'S INGRESS NODES behind a load balancer with no affinity — a shape
// the fleet guide draws — each answered a session another node had opened
// with a 404, so of N nodes (N−1)/N of an assistant's requests were refused,
// and every node kept the session it had opened for a client that would never
// reach it again. A CATALOGUE THE NODE BUILT AGAIN (the api package's
// EngineOperators: a first apply, a knowledge backend added or removed, a
// rename) ended every session opened on the one it replaced. And an ENDED
// SESSION is the end of the connection for the MCP client this repository
// ships, which reads that 404 as terminal where MCP's spec has a client open a
// new session — so each of those was a broken connection to an operator's
// assistant, as was the expiry any idle bound would have added, while with no
// bound a client that crashed held its session for the life of the process.
// Stateless, any node and any catalogue answers any request.
//
// What it costs is that per-request session, as the SDK builds one: the body
// read whole, bounded by [MCPMaxBody]; a JSON-RPC connection over it, which
// starts three goroutines — its reader, its handler queue and the one running
// the method — and ends them with the request; and the session registered on
// the catalogue's server and removed again, each under that server's one
// mutex, the removal a scan of every session the server holds — every request
// in flight on it and every stream held open on it. That is a linear term in
// the listeners parked on one catalogue, which at an operator's scale — a
// handful of assistants — is a few comparisons beside a tool call that crosses
// the estate router.
func guardedMCP(srv *mcp.Server) http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, MaxRequestBodyBytes: MCPMaxBody})
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
			// it must never record. Refused as JSON with the guard's own
			// code, like every refusal this engine writes: a client reads
			// an answer with no code as something in front of the node.
			log.WarnContext(r.Context(), "operator_mcp_unguarded",
				"detail", "a request reached the operator MCP surface with no "+
					"operator on its context; the auth guard is not in front of it")
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
			return
		}
		streamable.ServeHTTP(w, r)
	})
}
