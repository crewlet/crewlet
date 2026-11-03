package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// digitaloceanPass checks the company's half of DigitalOcean, which is the
// team name alone, then runs each agent's token through the access sync. Every
// call to DigitalOcean is a seat's own, with its own token. No Teardown:
// nothing company-wide is registered at DigitalOcean, and the access sync
// revokes each agent's token itself.
type digitaloceanPass struct{ engine *Engine }

func (*digitaloceanPass) Kind() integration.Kind { return integration.KindDigitalOcean }

// Needs is nil: the company half holds no credential.
func (*digitaloceanPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *digitaloceanPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *digitaloceanPass) run(context.Context, setup.PassInput) ([]integration.Finding, error) {
	cfg := p.engine.Company().Config.Integrations.DigitalOcean
	// DISABLED IS NOT CONFIGURED: switching the block off is how an operator
	// stops the engine acting at DigitalOcean.
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	const subject = "integrations.digitalocean.provisioning.team"
	if cfg.Provisioning == nil || strings.TrimSpace(p.engine.resolver().Value(cfg.Provisioning.Team)) == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject,
			Detail: subject + " resolved to nothing, so no agent's token can be checked against a team",
		}}, nil
	}
	return nil, nil
}

// seatPlan is every agent that is on at DigitalOcean, whose pasted token the
// access sync seals as DIGITALOCEAN_TOKEN_<HANDLE>. A seat's `role.sandbox.env`
// naming it as DIGITALOCEAN_ACCESS_TOKEN waits on it rather than launching
// without.
func (p *digitaloceanPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindDigitalOcean)
}
