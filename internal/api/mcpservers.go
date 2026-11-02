package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
)

// mcpRestarter is the engine as the MCP server routes restart a server
// through, its refusals mapped onto mcpapi's, as the model routes' engine is
// (models.go): the routes declare what they need, and this is the one place
// that adapts the engine to it.
type mcpRestarter struct{ e *engine.Engine }

func (m mcpRestarter) RestartServer(ctx context.Context, name, seat string, by iam.Actor) (mcpapi.Restart, error) {
	r, err := m.e.RestartMCPServer(ctx, name, seat, by)
	if err != nil {
		return mcpapi.Restart{}, restartErr(err)
	}
	return mcpapi.Restart{Server: r.Server, Seat: r.Seat, At: r.At, Node: r.Node}, nil
}

// mcpChecker is the engine as the MCP test routes test a server and check an
// address through, its refusals mapped onto mcpapi's as a restart's are.
type mcpChecker struct{ e *engine.Engine }

func (m mcpChecker) CheckServer(ctx context.Context, req mcpapi.CheckRequest) (mcpapi.Check, error) {
	tried := make([]engine.MCPValueTry, 0, len(req.Values))
	for _, v := range req.Values {
		tried = append(tried, engine.MCPValueTry{Key: v.Key, Variable: v.Variable, Value: v.Value})
	}
	r, err := m.e.CheckMCPServer(ctx, engine.MCPCheckRequest{Server: req.Server, Draft: req.Draft,
		Seat: req.Seat, Values: tried, By: req.By})
	if err != nil {
		return mcpapi.Check{}, restartErr(err)
	}
	out := mcpapi.Check{Server: r.Server, Seat: r.Seat, Draft: r.Draft, At: r.At, By: r.By, Node: r.Node,
		OK: r.OK, Nodes: make([]mcpapi.CheckedNode, 0, len(r.Nodes))}
	for _, n := range r.Nodes {
		out.Nodes = append(out.Nodes, mcpapi.CheckedNode{Node: n.Node, Answered: n.Answered, Reason: n.Reason,
			Report: n.Report})
	}
	return out, nil
}

func (m mcpChecker) CheckAddress(ctx context.Context, address string) mcp.AddressCheck {
	return m.e.CheckMCPAddress(ctx, address)
}

// restartErr is an engine refusal of a server gesture as the route answers
// it, the engine's sentence kept beside the route's sentinel.
func restartErr(err error) error {
	for engineErr, routeErr := range map[error]error{
		engine.ErrNoMCPServer:       mcpapi.ErrNoSuchServer,
		engine.ErrMCPServerOff:      mcpapi.ErrServerOff,
		engine.ErrMCPSeatOnShared:   mcpapi.ErrSeatOnShared,
		engine.ErrNoAgentSeat:       mcpapi.ErrNoSuchSeat,
		engine.ErrMCPSeatNotGranted: mcpapi.ErrSeatNotGranted,
		engine.ErrMCPSeatRequired:   mcpapi.ErrSeatRequired,
	} {
		if errors.Is(err, engineErr) {
			return fmt.Errorf("%w: %w", routeErr, err)
		}
	}
	return err
}
