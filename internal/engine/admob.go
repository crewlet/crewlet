package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/admob"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// consentGrant is a consent product's company grant as its client sends with
// it (googlegrowth.Grant): this node's token source for the grant, read afresh
// at each call so a sign-in or a sign-out takes effect at once.
type consentGrant struct {
	engine *Engine
	kind   integration.Kind
}

// Token is the grant's access token now, or why there is none (an error
// wrapping ErrIntegrationSignInNeeded).
func (g consentGrant) Token(ctx context.Context) (string, error) {
	c, err := g.engine.IntegrationCredential(ctx, g.kind)
	if err != nil {
		return "", err
	}
	return c.source.Token(ctx)
}

// Renewed is a fresh token after Google refused refused: one renewal.
func (g consentGrant) Renewed(ctx context.Context, refused string) (string, error) {
	c, err := g.engine.IntegrationCredential(ctx, g.kind)
	if err != nil {
		return "", err
	}
	return c.source.Rejected(ctx, refused)
}

// adMob is AdMob as this engine reaches it: the growth tools' product, and
// the pass's reads.
func (e *Engine) adMob() *admob.Product {
	return admob.New(e.runningConfig, e.resolver, consentGrant{engine: e, kind: integration.KindAdMob},
		e.GrowthAudit(), growthLeases{engine: e}, nil)
}

// adMobPass checks AdMob's company half: its consent grant, which every seat's
// call goes as; that it carries the write scope once a seat may change
// settings; and that it reaches every account the document names. Then it
// runs the agents' access through the access sync, which takes no step: a
// seat's level and accounts are the document alone. No Teardown: giving the
// grant up is the disconnect's (ADR-0178).
type adMobPass struct{ engine *Engine }

func (*adMobPass) Kind() integration.Kind { return integration.KindAdMob }

// Needs is nil: the consent requirement is the setup form's, and the client
// secret an ordinary secret field.
func (*adMobPass) Needs() *setup.Requirement { return nil }

func (p *adMobPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *adMobPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	if company == nil || !company.Config.Integrations.GrowthEnabled(string(integration.KindAdMob)) {
		return nil, integration.ErrNotConfigured
	}
	if f, err := p.engine.IntegrationGrantFinding(ctx, integration.KindAdMob); err != nil || f != nil {
		if f != nil {
			return []integration.Finding{*f}, nil
		}
		return nil, err
	}
	grant, _, err := p.engine.IntegrationGrant(ctx, integration.KindAdMob)
	if err != nil {
		return nil, err
	}
	findings, reached, err := p.engine.adMob().Check(ctx, grant.Scopes)
	if err != nil {
		return nil, err
	}
	return findings, p.engine.RecordIntegrationAccounts(ctx, integration.KindAdMob, reached)
}
