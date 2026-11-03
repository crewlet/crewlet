package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// expoPass checks the company's provisioner token and account at Expo, then
// runs each agent's robot user through the access sync.
//
// No Teardown: nothing company-wide is registered at Expo, and a disconnect's
// access sync deletes every agent's robot ([Engine.tearDownSeatAccess]).
type expoPass struct{ engine *Engine }

func (*expoPass) Kind() integration.Kind { return integration.KindExpo }

// Needs is nil: the provisioner token is held in the company document.
func (*expoPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *expoPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *expoPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	cfg := p.engine.Company().Config.Integrations.Expo
	// DISABLED IS NOT CONFIGURED: switching the block off is how an operator
	// stops the engine acting at Expo.
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	if cfg.Provisioning == nil {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: "integrations.expo.provisioning",
			Detail: "integrations.expo.provisioning is unset, so no agent's robot user can be created",
		}}, nil
	}
	env := p.engine.resolver()
	account := strings.TrimSpace(env.Value(cfg.Provisioning.AccountName))
	token := strings.TrimSpace(env.Value(cfg.Provisioning.AccessToken))
	var missing []integration.Finding
	for _, f := range []struct{ value, subject, what string }{
		{account, "integrations.expo.provisioning.account_name", "the Expo account name"},
		{token, "integrations.expo.provisioning.access_token", "the provisioner token"},
	} {
		if f.value == "" {
			missing = append(missing, integration.Finding{
				Kind: integration.FindingCredentialMissing, Subject: f.subject,
				Detail: f.subject + " resolved to nothing, so " + f.what + " is not known and no " +
					"agent's robot user can be created",
			})
		}
	}
	if len(missing) > 0 {
		return missing, nil
	}
	client, err := expo.NewClient(expo.ClientOptions{Token: token})
	if err != nil {
		return nil, fmt.Errorf("engine: expo pass: %w", err)
	}
	// NO SEATS: every agent's robot is the access sync's, which runs after
	// this ([Engine.withSeatAccess]); this checks the company's half.
	res, err := expo.Reconcile(ctx, expo.Options{Client: client, Account: account})
	if err != nil {
		return nil, fmt.Errorf("engine: expo pass: %w", err)
	}
	return res.Findings(), nil
}

// seatPlan is every agent that is on at Expo, whose robot's token the access
// sync seals as EXPO_TOKEN_<HANDLE>. A seat's `role.sandbox.env` naming it as
// EXPO_TOKEN waits on it rather than launching without.
func (p *expoPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindExpo)
}
