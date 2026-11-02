package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
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
	r, err := m.e.CheckMCPServer(ctx, engine.MCPCheckRequest{Server: req.Server, Entry: req.Entry, Draft: req.Draft,
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

// mcpSignIns is the engine as the MCP sign-in routes start, read and end a
// server's OAuth sign-in through, its refusals mapped onto mcpapi's.
type mcpSignIns struct{ e *engine.Engine }

func (m mcpSignIns) StartSignIn(ctx context.Context, server string, by iam.Actor) (mcpapi.SignInAnswer, error) {
	started, err := m.e.StartMCPSignIn(ctx, server, by)
	if err != nil {
		return mcpapi.SignInAnswer{}, signInErr(err)
	}
	out := signInAnswer(started.SignIn)
	out.AuthorizeURL = started.AuthorizeURL
	return out, nil
}

func (m mcpSignIns) SignIn(ctx context.Context, id uuid.UUID) (mcpapi.SignInAnswer, error) {
	s, err := m.e.MCPSignIn(ctx, id)
	if err != nil {
		return mcpapi.SignInAnswer{}, signInErr(err)
	}
	return signInAnswer(s), nil
}

func (m mcpSignIns) SignOut(ctx context.Context, server string, by iam.Actor) (mcpapi.SignOutAnswer, error) {
	out, err := m.e.SignOutMCPServer(ctx, server, by)
	if err != nil {
		return mcpapi.SignOutAnswer{}, signInErr(err)
	}
	return mcpapi.SignOutAnswer{Server: server, Revocation: string(out.Revocation), Said: out.Said}, nil
}

// signInAnswer is a sign-in's record as the routes answer it: never its sealed
// half.
func signInAnswer(s coord.MCPSignIn) mcpapi.SignInAnswer {
	return mcpapi.SignInAnswer{ID: s.ID, Server: s.Server, Status: string(s.Status), Detail: s.Detail,
		StartedAt: s.StartedAt, ExpiresAt: s.ExpiresAt}
}

// signInErr is an engine refusal of a sign-in gesture as the route answers it.
func signInErr(err error) error {
	var refused *engine.MCPSignInRefusal
	if errors.As(err, &refused) {
		return &mcpapi.SignInRefusal{Status: refused.Status, Detail: refused.Reason + ". " + refused.Remedy}
	}
	for engineErr, routeErr := range map[error]error{
		engine.ErrNoMCPServer:        mcpapi.ErrNoSuchServer,
		engine.ErrMCPNoOAuth:         mcpapi.ErrNoOAuth,
		engine.ErrNoCallbackAddress:  mcpapi.ErrNoCallback,
		engine.ErrNoMCPSignIn:        mcpapi.ErrNoSignIn,
		engine.ErrMCPNotSignedIn:     mcpapi.ErrNotSignedIn,
		engine.ErrMCPServiceIdentity: mcpapi.ErrServiceIdentity,
	} {
		if errors.Is(err, engineErr) {
			return fmt.Errorf("%w: %w", routeErr, err)
		}
	}
	return err
}

// mcpLanding is the engine as the sign-in callback finishes a sign-in through.
type mcpLanding struct{ e *engine.Engine }

func (m mcpLanding) CompleteSignIn(ctx context.Context, l webhooks.MCPLanding) (string, error) {
	server, err := m.e.CompleteMCPSignIn(ctx, engine.MCPSignInLanding{State: l.State, Code: l.Code,
		Issuer: l.Issuer, Error: l.Error, Description: l.Description})
	var refused *engine.MCPSignInRefusal
	if errors.As(err, &refused) {
		return server, &webhooks.MCPSignInRefusal{Status: refused.Status, Reason: refused.Reason,
			Remedy: refused.Remedy}
	}
	return server, err
}

func (m mcpLanding) ClientMetadata() (oauth.Metadata, bool) { return m.e.ClientMetadata() }
