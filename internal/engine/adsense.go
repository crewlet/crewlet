package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/adsense"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// adSense is AdSense as this engine reaches it: the growth tools' product,
// and the pass's reads.
func (e *Engine) adSense() *adsense.Product {
	return adsense.New(e.runningConfig, e.resolver, consentGrant{engine: e, kind: integration.KindAdSense},
		e.GrowthAudit(), growthLeases{engine: e}, nil)
}

// adSensePass checks AdSense's company half: its consent grant, which every
// seat's call goes as; that it carries the write scope once a seat may
// change settings; and that it reaches every account the document names.
// Then it runs the agents' access through the access sync, which takes no
// step: a seat's level and accounts are the document alone. No Teardown:
// giving the grant up is the disconnect's (ADR-0178).
type adSensePass struct{ engine *Engine }

func (*adSensePass) Kind() integration.Kind { return integration.KindAdSense }

// Needs is nil: the consent requirement is the setup form's, and the client
// secret an ordinary secret field.
func (*adSensePass) Needs() *setup.Requirement { return nil }

func (p *adSensePass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *adSensePass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	if company == nil || !company.Config.Integrations.GrowthEnabled(string(integration.KindAdSense)) {
		return nil, integration.ErrNotConfigured
	}
	if f, err := p.engine.IntegrationGrantFinding(ctx, integration.KindAdSense); err != nil || f != nil {
		if f != nil {
			return []integration.Finding{*f}, nil
		}
		return nil, err
	}
	grant, _, err := p.engine.IntegrationGrant(ctx, integration.KindAdSense)
	if err != nil {
		return nil, err
	}
	findings, reached, err := p.engine.adSense().Check(ctx, grant.Scopes)
	if err != nil {
		return nil, err
	}
	return findings, p.engine.RecordIntegrationAccounts(ctx, integration.KindAdSense, reached)
}
