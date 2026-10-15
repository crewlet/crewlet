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

// MCPPath is the route the MCP transport is mounted at.
//
// UNDER ITS OWN PREFIX rather than under /mcp/, which the auth package exempts
// wholesale for the sandbox bridge — mounting here would have put a writable
// company surface behind no credential at all, and the collision with the
// bridge's own `/mcp/{token}` pattern would have made which one answered a
// question of registration order.
const MCPPath = "/operator/mcp"

// serverName is what an MCP client lists this server as.
const serverName = "crewlet-operator"

// mcpServer is one call's catalogue as an MCP server, for an operator's own AI
// assistant.
//
// BUILT FOR THE REQUEST, from the catalogue the request found: the transport
// is stateless ([Server.MCPHandler]), so there is no session a server built
// once would serve and nothing a rebuilt one strands.
func (s *Server) mcpServer(cat catalogue) *mcp.Server {
	title := "Crewlet"
	if name := strings.TrimSpace(cat.company); name != "" {
		title = name + " (Crewlet)"
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name: serverName, Title: title, Version: "1",
	}, nil)
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
// value, so the MCP transport reaches a tool by exactly the path every other
// transport does — and is audited by it — and there is no second handle on it
// to fall out of step. An MCP call names no operation of its own: a tracker
// write mints one and answers it as `op_id`, which the assistant brings back
// as an argument to finish the same write.
//
// WHO IS CALLING COMES FROM THE REQUEST'S CREDENTIAL, carried on the context
// by the request guard and read by the catalogue's actor — never from an
// argument. A caller that could name its own actor could file work as
// anybody, which is the same rule a seat's tools follow and the same reason.
func (s *Server) handlerFor(cat catalogue, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, fmt.Errorf("operator: %s: bad arguments: %w", name, err)
			}
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
		return &mcp.CallToolResult{
			IsError: result.Failed,
			Content: []mcp.Content{&mcp.TextContent{Text: result.Output}},
		}, nil
	}
}

// MCPHandler serves the MCP transport at [MCPPath].
//
// # STATELESS, so every call is decided by the request that carries it
//
// The SDK's stateful handler opens a SESSION on the initialize request and
// serves every later request on that session through the context the
// initialize request had — so the principal a tool call reads off its context
// is whoever OPENED the session, not whoever sent the call. That made two
// things true that must not be: a grant withdrawn mid-session went on working
// for as long as the assistant stayed connected (the ceiling, a revoked
// session and a removed token included), and any other credential presenting
// the same Mcp-Session-Id header acted as the opener — a session id is an
// opaque handle a client echoes, not a secret anybody proved.
//
// Stateless mode gives each POST a temporary session built from THAT
// request's own context, so the guard's resolution of each request is the
// resolution its tool call is decided on, and there is no session for a second
// credential to ride. Nothing this surface serves needed the state: its
// catalogue is read for each request, so there is no list-changed
// notification to push, and no tool here asks the client anything back.
//
// # Three answers before any tool runs
//
// A node that has not been handed a company answers `503
// no_active_revision`; one whose catalogue is empty — no native half, no
// search, no seat controls — answers as the route's absence would, since an
// endpoint that lists no tools reads to an operator as broken; and anything
// else is served over the catalogue the request found.
//
// EVERY METHOD IS STILL ROUTED HERE, so a GET for the server-to-client stream
// and a DELETE to end a session are answered by the SDK's own 405 with
// `Allow: POST` — the transport's documented way of saying "this server offers
// no stream" — rather than by a mux 405 a client reports as a route
// registered wrong.
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
		switch {
		case !up:
			httpjson.NoActiveRevision(w, httpjson.Detail{
				"detail": httpjson.NativeHalvesNotUp})
			return
		case len(cat.tools) == 0:
			httpjson.NoRoute(w, r)
			return
		}
		srv := s.mcpServer(cat)
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
			&mcp.StreamableHTTPOptions{Stateless: true}).ServeHTTP(w, r)
	})
}
