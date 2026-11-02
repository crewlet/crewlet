package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
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

// restartErr is an engine refusal as the route answers it, the engine's
// sentence kept beside the route's sentinel.
func restartErr(err error) error {
	for engineErr, routeErr := range map[error]error{
		engine.ErrNoMCPServer:       mcpapi.ErrNoSuchServer,
		engine.ErrMCPServerOff:      mcpapi.ErrServerOff,
		engine.ErrMCPSeatOnShared:   mcpapi.ErrSeatOnShared,
		engine.ErrNoAgentSeat:       mcpapi.ErrNoSuchSeat,
		engine.ErrMCPSeatNotGranted: mcpapi.ErrSeatNotGranted,
	} {
		if errors.Is(err, engineErr) {
			return fmt.Errorf("%w: %w", routeErr, err)
		}
	}
	return err
}
