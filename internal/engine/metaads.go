package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/metaads"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
)

// metaAdsPass proves the company's Meta app and system user's tokens and
// renews one near its end, then runs the agents' access, which is the
// document's alone.
type metaAdsPass struct{ engine *Engine }

func (*metaAdsPass) Kind() integration.Kind { return integration.KindMetaAds }

// Needs is nil: the tokens are held in the company document.
func (*metaAdsPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *metaAdsPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *metaAdsPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	for _, product := range p.engine.growthProducts() {
		if meta, ok := product.(*metaads.Product); ok {
			return meta.Prove(ctx)
		}
	}
	return nil, integration.ErrNotConfigured
}

// metaAdsRenewals keeps Meta Ads' renewed tokens in the estate, sealed under
// growth/meta_ads/token/<slot>, where no operator surface reads them.
type metaAdsRenewals struct{ engine *Engine }

func metaAdsRenewal(slot string) string { return "growth/" + metaads.Kind + "/token/" + slot }

// renewer is the engine's own name on a renewal it seals.
var renewer = secrets.Author{Name: "growth", Kind: string(iam.ActorSystem)}

func (r metaAdsRenewals) Held(ctx context.Context, slot string) (string, uint64, bool, error) {
	estate, err := r.engine.fleetReady()
	if err != nil {
		return "", 0, false, err
	}
	held, found, err := estate.Held(ctx, metaAdsRenewal(slot))
	return held.Value, held.Version, found, err
}

func (r metaAdsRenewals) Put(ctx context.Context, slot, value string, version uint64) (bool, error) {
	estate, err := r.engine.fleetReady()
	if err != nil {
		return false, err
	}
	if version == 0 {
		return estate.Create(ctx, metaAdsRenewal(slot), value, renewer, secrets.SourceProvision, r.engine.now())
	}
	return estate.Swap(ctx, metaAdsRenewal(slot), value, version, renewer, secrets.SourceProvision, r.engine.now())
}

func (r metaAdsRenewals) Delete(ctx context.Context, slot string) error {
	estate, err := r.engine.fleetReady()
	if err != nil {
		return err
	}
	_, err = estate.Delete(ctx, metaAdsRenewal(slot))
	return err
}
