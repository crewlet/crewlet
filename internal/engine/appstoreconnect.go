package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// appStoreConnectPass proves the company's App Store Connect keys and reads
// again the apps they reach, then runs the agents' access, which is the
// document's alone, its app chooser offering what this pass read.
type appStoreConnectPass struct{ engine *Engine }

func (*appStoreConnectPass) Kind() integration.Kind { return integration.KindAppStoreConnect }

// Needs is nil: the keys are held in the company document.
func (*appStoreConnectPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccessReads]).
func (p *appStoreConnectPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccessReads(ctx, p.Kind(), in, p.run)
}

// run proves the keys, then reads every team's apps fresh: a pass and Check
// now are when a new app joins the chooser at once.
func (p *appStoreConnectPass) run(ctx context.Context, _ setup.PassInput) (
	[]integration.Finding, seataccess.PassReads, error) {
	asc := p.engine.AppStoreAnalytics()
	if asc == nil {
		return nil, nil, integration.ErrNotConfigured
	}
	findings, err := asc.Prove(ctx)
	if err != nil {
		return findings, nil, err
	}
	return findings, asc.SeatReads(ctx, true), nil
}
