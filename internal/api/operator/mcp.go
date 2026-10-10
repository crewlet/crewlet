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
// UNDER ITS OWN PREFIX rather than under /mcp/, which is open wholesale for
// the sandbox bridge — mounting there would have put a writable company
// surface behind no credential at all, and the collision with the
// bridge's own `/mcp/{token}` pattern would have made which one answered a
// question of registration order.
const MCPPath = "/operator/mcp"

// serverName is what an MCP client lists this server as.
const serverName = "crewlet-operator"

// mcpTransport is the catalogue as an MCP server, for an operator's own AI
// assistant.
//
// IT ADMITS AN UNLINKED ADMIN KEY, which is the difference between this
// transport and a person's: an assistant connected with a CI key or an ops-bot
// key is a credential acting as itself, and every write it makes names that
// credential as its author (see [WorkActor]). An unlinked MEMBER key it
// refuses, because a member acts only as the person the key is linked to
// (ADR-0031) — see [Server.MCPHandler].
type mcpTransport struct {
	srv *mcp.Server
}

// newMCPTransport registers every catalogue tool on an MCP server.
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
	return mcpTransport{srv: srv}
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

// MCPHandler serves the MCP transport at [MCPPath].
//
// EVERY METHOD, for the reason the sandbox bridge takes every method:
// streamable HTTP is a GET for the server-to-client stream and a DELETE to
// end a session, and a pattern naming one verb answers 405 to the others —
// which an MCP client reports as a transport that does not support streaming
// rather than as a route registered wrong.
func (s *Server) MCPHandler() http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcp.srv }, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// THE REACH IS THE APP'S, not a second check here: this path is
		// mounted at member reach, so a request that reaches this handler
		// has already presented an accepted key. Reading the principal off
		// the context rather than re-checking it is what keeps one
		// decision about who may reach the surface.
		caller := auth.PrincipalFrom(r.Context())
		if caller.ID == "" {
			// UNREACHABLE if the route table is in front, and refused
			// rather than trusted if it somehow is not: this surface
			// writes to the company, and a write with no writer is the
			// one thing it must never record. Refused as JSON with the
			// guard's own code, like every refusal this engine writes: a
			// client reads an answer with no code as something in front
			// of the node.
			log.WarnContext(r.Context(), "operator_mcp_unguarded",
				"detail", "a request reached the operator MCP surface with no "+
					"principal on its context; the route table is not in front of it")
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
			return
		}
		// A MEMBER ACTS ONLY AS THE PERSON THE KEY IS LINKED TO
		// (ADR-0031), so an unlinked member key is somebody's credential
		// acting as nobody: refused `unbound`, exactly as the act
		// transport refuses it. An admin key may act as itself — a CI or
		// ops-bot credential, attributed to its own name — and a linked
		// key of either role is the person.
		//
		// Resolved on EVERY REQUEST, session or not, because a config
		// apply can unlink the key between two calls of one session — and
		// the answer is PINNED on the request ([withSeat]), so the call
		// this admits is made, attributed and audited as the seat it was
		// admitted as rather than as whatever a later read of the chart
		// says.
		seat := seatFor(s.chart, s.env, caller.ID)
		if seat == "" && !caller.IsAdmin() {
			httpjson.FailWith(w, http.StatusForbidden, CodeUnbound, unboundMember(caller.ID))
			return
		}
		streamable.ServeHTTP(w, r.WithContext(withSeat(r.Context(), seat)))
	})
}
