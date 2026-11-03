package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// expoPass checks the company's half of Expo, which is the account name
// alone, then runs each agent's robot through the access sync. Every call to
// Expo is a seat's own, with its robot's token. No Teardown: nothing
// company-wide is registered at Expo.
type expoPass struct{ engine *Engine }

func (*expoPass) Kind() integration.Kind { return integration.KindExpo }

// Needs is nil: the company half holds no credential.
func (*expoPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *expoPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *expoPass) run(context.Context, setup.PassInput) ([]integration.Finding, error) {
	cfg := p.engine.Company().Config.Integrations.Expo
	// DISABLED IS NOT CONFIGURED: switching the block off is how an operator
	// stops the engine acting at Expo.
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	const subject = "integrations.expo.provisioning.account_name"
	if cfg.Provisioning == nil || strings.TrimSpace(p.engine.resolver().Value(cfg.Provisioning.AccountName)) == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject,
			Detail: subject + " resolved to nothing, so no agent's robot can be checked against an account",
		}}, nil
	}
	return nil, nil
}

// seatPlan is every agent that is on at Expo, whose robot's pasted token the
// access sync seals as EXPO_TOKEN_<HANDLE>. A seat's `role.sandbox.env` naming it as
// EXPO_TOKEN waits on it rather than launching without.
func (p *expoPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindExpo)
}
