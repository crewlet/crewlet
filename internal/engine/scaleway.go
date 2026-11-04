package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// scalewayPass adapts scaleway.Reconcile to the pass contract: the provisioner
// key and the organization it provisions in, then each agent's own
// application, policy and key.
//
// NO TEARDOWN: the engine puts nothing company-wide at Scaleway. Every agent's
// application is deleted by the disconnect's access sync
// ([Engine.tearDownSeatAccess]), from the ledger.
type scalewayPass struct{ engine *Engine }

func (*scalewayPass) Kind() integration.Kind { return integration.KindScaleway }

// Needs is nil: the provisioner key is held in the company document.
func (*scalewayPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *scalewayPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *scalewayPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Scaleway
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	const subject = "integrations.scaleway.provisioning"
	if cfg.Provisioning == nil {
		// Config validation refuses this shape; a document that reached
		// here anyway is reported as the finding it is.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject,
			Detail: subject + " is unset, so no agent's application can be made",
		}}, nil
	}
	env := p.engine.resolver()
	secret := strings.TrimSpace(env.Value(cfg.Provisioning.SecretKey))
	access := strings.TrimSpace(env.Value(cfg.Provisioning.AccessKey))
	switch {
	case secret == "":
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject + ".secret_key",
			Detail: "the Scaleway provisioner secret key did not resolve, so no agent's application can be made",
		}}, nil
	case access == "":
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject + ".access_key",
			Detail: "the Scaleway provisioner access key did not resolve, so the key cannot be checked",
		}}, nil
	}
	client, err := scaleway.NewClient(scaleway.ClientOptions{
		OrganizationID: env.Value(cfg.Provisioning.OrganizationID), SecretKey: secret,
	})
	if err != nil {
		// THE ORGANIZATION ID, the one input left that NewClient refuses:
		// a fact about the document, not a pass that failed to look.
		//nolint:nilerr // Deliberate: reported as a finding on the document.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject + ".organization_id",
			Detail: subject + ".organization_id does not resolve to a Scaleway Organization ID",
		}}, nil
	}
	// THE AGENTS' APPLICATIONS, which the orphan advisory spares and whose
	// policies and keys are told from strangers': the ledger's.
	held, err := p.engine.seatAccessHeld(ctx, integration.KindScaleway)
	if err != nil {
		return nil, fmt.Errorf("engine: scaleway pass: %w", err)
	}
	seats := make(map[string]scaleway.Seat, len(held))
	for handle, rec := range held {
		seats[handle] = scaleway.Seat{
			ApplicationID: rec.Account.ID, Name: rec.Account.Name,
			AccessKey: strings.TrimSpace(env.Value("${" +
				seataccess.SinkName(integration.KindScaleway, scaleway.AccessKeyField, handle) + "}")),
		}
	}
	res, err := scaleway.Reconcile(ctx, scaleway.Options{
		Client: client, AccessKey: access, Company: company.Config.Name, Seats: seats,
		InUse: seataccess.LevelsInUse(company.Config, seataccess.LaneScaleway),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: scaleway pass: %w", err)
	}
	return res.Findings(), nil
}

// seatPlan is every agent that is on at Scaleway, whose secret key and access
// key the access sync seals as SCALEWAY_SECRET_KEY_<HANDLE> and
// SCALEWAY_ACCESS_KEY_<HANDLE>. A seat's tool naming either waits on them
// rather than launching without.
func (p *scalewayPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindScaleway)
}
