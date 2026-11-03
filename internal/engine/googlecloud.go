package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// googleCloudPass adapts googlecloud.Reconcile to the pass contract: the
// provisioner the engine reaches by federation, the levels Google's roles
// leave grantable and the identity project's accounts, then each agent's own
// service account.
//
// NO TEARDOWN: the engine puts nothing company-wide at Google. Every agent's
// account is disabled or deleted by the disconnect's access sync
// ([Engine.tearDownSeatAccess]), from the ledger.
//
// NO SEAT PLAN: an agent's token is issued at use for an hour and never
// sealed, so no pass mints one.
type googleCloudPass struct{ engine *Engine }

func (*googleCloudPass) Kind() integration.Kind { return integration.KindGoogleCloud }

// Needs is nil: the provisioner and its key are held in the company document.
func (*googleCloudPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *googleCloudPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *googleCloudPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GoogleCloud
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	const subject = "integrations.google_cloud.provisioning"
	if cfg.Provisioning == nil {
		// Config validation refuses this shape; a document that reached
		// here anyway is reported as the finding it is.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject,
			Detail: subject + " is unset, so no agent's service account can be made",
		}}, nil
	}
	env := p.engine.resolver()
	prov := cfg.Provisioning
	switch {
	case strings.TrimSpace(env.Value(prov.FederationKey)) == "":
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject + ".federation_key",
			Detail: "the Google Cloud federation key did not resolve, so Crewlet cannot sign in as the provisioner",
		}}, nil
	case strings.TrimSpace(env.Value(prov.Provider)) == "":
		// THE PERSON'S STEP: the provider holding the key's public half
		// exists only once they run the connect form's block.
		return []integration.Finding{{
			Kind: integration.FindingApprovalRequired, Subject: subject + ".provider",
			Title:     "Run the Cloud Shell block",
			Detail:    "Run the connect form's block in Cloud Shell, then paste the provider name it prints",
			Remedy:    "Open the Google Cloud connection, run its block in Cloud Shell, and paste the last line it prints",
			ActionURL: googlecloud.CloudShellURL,
		}}, nil
	}
	client, err := googlecloud.ClientFor(company.Config, env, nil)
	if err != nil {
		// A VALUE THE CLIENT REFUSES, a fact about the document rather than
		// a pass that failed to look.
		//nolint:nilerr // Deliberate: reported as a finding on the document.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: subject,
			Detail: strings.TrimPrefix(err.Error(), "googlecloud: "),
		}}, nil
	}
	// THE AGENTS' ACCOUNTS, which the orphan advisory spares: the ledger's.
	held, err := p.engine.seatAccessAccounts(ctx, integration.KindGoogleCloud)
	if err != nil {
		return nil, fmt.Errorf("engine: google cloud pass: %w", err)
	}
	res, err := googlecloud.Reconcile(ctx, googlecloud.Options{
		Client: client, Company: company.Config.Name, Seats: held,
		InUse: seataccess.LevelsInUse(company.Config, seataccess.LaneGoogleCloud),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: google cloud pass: %w", err)
	}
	return res.Findings(), nil
}
