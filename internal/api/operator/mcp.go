package operator

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
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

// MCPPattern is the route the MCP transport is mounted at, on [MCPPath]: a
// POST, because a transport that keeps no session has nothing for the other
// two verbs of streamable HTTP to do (see [Server.MCPHandler]). A GET, which asks for
// a server-to-client stream, and a DELETE, which ends a session, are answered
// the mux's own 405 naming POST — the answer MCP's streamable-HTTP transport
// gives a client for "this server offers no stream", and one carrying a code
// like every other refusal this engine writes. A client of the 2026-07-28
// protocol opens its stream as a POST instead — `subscriptions/listen`, held
// open until the client or the node ends it — and that is the one way this
// transport reaches a client between its requests ([Listeners]).
const MCPPattern = "POST " + MCPPath

// serverName is what an MCP client lists this server as.
const serverName = "crewlet-operator"

// mcpServer is one call's catalogue as an MCP server, for an operator's own AI
// assistant.
//
// BUILT FOR THE REQUEST, from the catalogue the request found: the transport
// is stateless ([Server.MCPHandler]), so there is no session a server built
// once would serve and nothing a later one strands — save the stream a
// listening client holds, which the node's one registry keeps ([Listeners]).
func (s *Server) mcpServer(cat catalogue) *mcp.Server {
	title := "Crewlet"
	if name := strings.TrimSpace(cat.company); name != "" {
		title = name + " (Crewlet)"
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name: serverName, Title: title, Version: "1",
	}, nil)
	// EVERY CALL'S SERVER KEEPS ITS LISTEN STREAMS IN THE NODE'S ONE
	// REGISTRY, so an announcement that the node's tools changed reaches
	// a stream held open on a catalogue no later call reads ([Listeners]).
	srv.AddReceivingMiddleware(s.listeners.receiving)
	srv.AddSendingMiddleware(s.listeners.sending)
	for _, tool := range cat.tools {
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
		}, s.handlerFor(cat, tool.Name()))
	}
	return srv
}

// handlerFor adapts one catalogue tool to the MCP SDK's own signature.
//
// BY NAME, THROUGH THE SERVER'S DISPATCH, rather than closing over the tool
// value, so MCP reaches a tool by the path every transport does, audit
// included, with no second handle to fall out of step. An MCP call names no
// operation: a tracker write mints one and answers it as `op_id`, which the
// assistant brings back as an argument to finish the same write.
//
// WHO IS CALLING COMES FROM THE REQUEST'S CREDENTIAL, on the context from the
// request guard, never from an argument: a caller naming its own actor could
// file work as anybody.
func (s *Server) handlerFor(cat catalogue, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// THE ONE DECODE every transport into a tool shares
		// ([toolargs.Decode]): through json.Number, so a number past
		// 2^53 reaches the tool as the caller sent it.
		args, err := toolargs.Decode(req.Params.Arguments)
		if err != nil {
			// THE ARGUMENTS' OWN WORDS ([toolargs.Decode]) after the
			// tool's name, which is what the assistant called: an
			// "operator:" in front named this package to it.
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		result, err := s.run(ctx, cat, Call{
			Transport: types.TransportMCP, Tool: name, Args: args,
		})
		if err != nil {
			// A TRANSPORT ERROR, not a tool failure: the caller's context
			// ended — or, unreachably, a name the SDK dispatched that the
			// catalogue does not hold. The distinction is the SDK's own:
			// a failed tool is an ordinary result with IsError, and
			// reporting it as a protocol error would make a client retry
			// a refusal.
			return nil, err
		}
		return mcpAnswer(result), nil
	}
}

// mcpAnswer is a tool's answer as an assistant reads it: its text with any note
// on the arguments after it ([tools.Rendered]), then each image the answer
// carries, which tools.Invoke held to the rule as the tool returned.
func mcpAnswer(result tools.Result) *mcp.CallToolResult {
	content := make([]mcp.Content, 0, 1+len(result.Images))
	content = append(content, &mcp.TextContent{Text: tools.Rendered(result)})
	for _, img := range result.Images {
		content = append(content, &mcp.ImageContent{Data: img.Data, MIMEType: img.MIMEType})
	}
	return &mcp.CallToolResult{IsError: result.Failed, Content: content}
}

// MCPHandler serves the MCP transport at [MCPPattern], a POST: GET and DELETE
// get the mux's 405 naming POST before anything here runs.
//
// STATELESS, so every call is decided by the request that carries it. A
// stateful session serves requests through the initialize request's context,
// so a call's principal would be whoever OPENED it: a withdrawn grant would
// keep working, and any credential echoing the same Mcp-Session-Id (a handle,
// not a secret) would act as the opener. Each POST instead gets a temporary
// session from THAT request's context. Tools-changed notices go down the
// `subscriptions/listen` POST, ended by the client, its connection or
// [Listeners.End]. Stateless also serves a FLEET: another ingress node would
// refuse a session opened elsewhere. No company: `503 no_active_revision`.
func (s *Server) MCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// THE GUARD IS THE APP'S, not a second one here: this path is not
		// on the guard's exemption list, so a request that reaches this
		// handler carries the guard's answer. Read through [auth.Caller]
		// so its two failures stay apart — a caller who presented nothing
		// is a 401, and one this node could not CHECK is a 503 — and a
		// request with no answer at all, which only a route mounted around
		// the guard could deliver, is refused rather than trusted: this
		// surface writes to the company, and a write with no writer is the
		// one thing it must never record.
		if _, ok := auth.Caller(w, r); !ok {
			return
		}
		cat, up := s.catalogueFor()
		if !up {
			httpjson.NoActiveRevision(w, httpjson.Detail{
				"detail": httpjson.NativeHalvesNotUp})
			return
		}
		srv := s.mcpServer(cat)
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
			&mcp.StreamableHTTPOptions{Stateless: true, MaxRequestBodyBytes: MCPMaxBody}).
			ServeHTTP(w, r)
	})
}

// MCPMaxBody bounds one MCP request's body.
//
// The transport reads a request's body whole before it dispatches it, so this
// is what one request may hold in memory. FOUR MEBIBYTES — the SDK's own
// default, stated here rather than inherited, since it is a promise this
// surface makes — because every catalogue call fits far below it: the largest
// is a page's body, at most twice [doclet.MaxBody] once escaped as a JSON
// string, which [MaxActBody] derives. The one argument it does bound is
// `write_project_file`'s content, the whole file in one string; a file that
// does not fit goes up through `PUT /worklet/files/{project}/{path...}`, which
// streams its body to the object store instead of holding it.
const MCPMaxBody = 4 << 20
