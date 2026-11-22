package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/appleads"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// appleAdsPass proves the company's Apple Ads API users, then runs the agents'
// access, which is the document's alone.
type appleAdsPass struct{ engine *Engine }

func (*appleAdsPass) Kind() integration.Kind { return integration.KindAppleAds }

// Needs is nil: the API users' keys are held in the company document.
func (*appleAdsPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *appleAdsPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *appleAdsPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	for _, product := range p.engine.growthProducts() {
		if ads, ok := product.(*appleads.Product); ok {
			return ads.Prove(ctx)
		}
	}
	return nil, integration.ErrNotConfigured
}
