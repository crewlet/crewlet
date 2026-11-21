package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// appStoreConnectPass proves the company's App Store Connect keys, then runs
// the agents' access, which is the document's alone.
type appStoreConnectPass struct{ engine *Engine }

func (*appStoreConnectPass) Kind() integration.Kind { return integration.KindAppStoreConnect }

// Needs is nil: the keys are held in the company document.
func (*appStoreConnectPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *appStoreConnectPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *appStoreConnectPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	for _, product := range p.engine.growthProducts() {
		if asc, ok := product.(*appstoreconnect.Product); ok {
			return asc.Prove(ctx)
		}
	}
	return nil, integration.ErrNotConfigured
}
