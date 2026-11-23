package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// appStoreConnectPass reads again every team's apps with an agent's own key,
// then runs the agents' access, which is the document's alone, its app chooser
// offering what this pass read. The company holds no key to prove.
type appStoreConnectPass struct{ engine *Engine }

func (*appStoreConnectPass) Kind() integration.Kind { return integration.KindAppStoreConnect }

// Needs is nil: each agent's own key is checked by its identity's pass.
func (*appStoreConnectPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccessReads]).
func (p *appStoreConnectPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccessReads(ctx, p.Kind(), in, p.run)
}

// run reads every team's apps fresh: a pass and Check now are when a new app
// joins the chooser at once.
func (p *appStoreConnectPass) run(ctx context.Context, _ setup.PassInput) (
	[]integration.Finding, seataccess.PassReads, error) {
	asc := p.engine.AppStoreConnect()
	if asc == nil {
		return nil, nil, integration.ErrNotConfigured
	}
	return nil, asc.SeatReads(ctx, true), nil
}
