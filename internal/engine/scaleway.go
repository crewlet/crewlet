package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
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
// ONE THING IS COMPANY-WIDE AT SCALEWAY: the atlas application, made where
// the company switched `integrations.scaleway.atlas` on (ADR-0144). The pass
// keeps it and its teardown deletes it. Every agent's application is deleted
// by the disconnect's access sync ([Engine.tearDownSeatAccess]), from the
// ledger.
type scalewayPass struct{ engine *Engine }

func (*scalewayPass) Kind() integration.Kind { return integration.KindScaleway }

// Needs is nil: the provisioner key is held in the company document.
func (*scalewayPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *scalewayPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *scalewayPass) run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
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
		InUse:              seataccess.LevelsInUse(company.Config, seataccess.LaneScaleway),
		AtlasApplicationID: sealedValue(env, scaleway.AtlasApplicationName),
	})
	if err != nil {
		return nil, fmt.Errorf("engine: scaleway pass: %w", err)
	}
	found, err := p.atlas(ctx, in, client, company.Config.Name, cfg.Atlas)
	if err != nil {
		return nil, err
	}
	return append(res.Findings(), found...), nil
}

// sealedValue is the value sealed under name on this node, "" where none is.
func sealedValue(env *config.Resolver, name string) string {
	return strings.TrimSpace(env.Value("${" + name + "}"))
}

// atlas brings the atlas application to the company's setting.
//
// A FAILURE HERE IS A FINDING, NEVER THE PASS'S: the atlas is optional
// reading, and a pass that failed over it would report the whole connection
// broken while every agent's application works. Only a pass that was
// cancelled answers an error.
func (p *scalewayPass) atlas(
	ctx context.Context, in setup.PassInput, client *scaleway.Client, company string, want bool,
) ([]integration.Finding, error) {
	env := p.engine.resolver()
	found, err := scaleway.ReconcileAtlas(ctx, scaleway.AtlasOptions{
		Client: client, Company: company, Want: want, Sink: in.Sink,
		ApplicationID: sealedValue(env, scaleway.AtlasApplicationName),
		AccessKey:     sealedValue(env, scaleway.AtlasAccessName),
		SecretHeld:    sealedValue(env, scaleway.AtlasSecretName) != "",
	})
	switch {
	case err == nil:
		return found, nil
	case ctx.Err() != nil:
		return nil, fmt.Errorf("engine: scaleway pass: %w", ctx.Err())
	}
	log.WarnContext(ctx, "scaleway_atlas_application_unconverged", "error", err)
	title, detail := "Atlas application not up to date", "Crewlet could not bring the read-only IAM application "+
		"it keeps for Atlas up to date, so Atlas may read less at Scaleway. The next check tries again"
	if !want {
		title, detail = "Atlas application not deleted", "Crewlet could not delete the read-only IAM application "+
			"it made for Atlas. The next check tries again"
	}
	return append(found, integration.Finding{
		Kind: integration.FindingCoveragePartial, Subject: "integrations.scaleway.atlas",
		Title: title, Detail: detail, ActionURL: scaleway.ApplicationsURL,
		Remedy: "If it stays, check the provisioner application still holds IAMApplicationManager and " +
			"IAMPolicyManager on the Organization",
	}), nil
}

// Teardown deletes the atlas application with its policy and its key, and
// forgets what was sealed for it. One never made, or already gone, is the
// state asked for.
func (p *scalewayPass) Teardown(ctx context.Context, in setup.TeardownInput) (provision.Removed, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Scaleway
	env := p.engine.resolver()
	appID := sealedValue(env, scaleway.AtlasApplicationName)
	if appID == "" && sealedValue(env, scaleway.AtlasAccessName) == "" && sealedValue(env, scaleway.AtlasSecretName) == "" {
		return provision.Removed{}, nil
	}
	if appID != "" {
		if cfg == nil || cfg.Provisioning == nil {
			return provision.Removed{}, errors.New("engine: scaleway teardown: the atlas application cannot be " +
				"deleted without integrations.scaleway.provisioning")
		}
		client, err := scaleway.NewClient(scaleway.ClientOptions{
			OrganizationID: env.Value(cfg.Provisioning.OrganizationID),
			SecretKey:      env.Value(cfg.Provisioning.SecretKey),
		})
		if err != nil {
			return provision.Removed{}, fmt.Errorf("engine: scaleway teardown: %w", err)
		}
		if err := scaleway.RemoveAtlas(ctx, client, appID); err != nil {
			return provision.Removed{}, fmt.Errorf("engine: scaleway teardown: %w", err)
		}
	}
	// FORGOTTEN HERE, not through the seats' removal list: these names are no
	// seat's, and the key they held died with the application.
	sink, err := p.engine.SetupSink(in.By)
	if err != nil {
		return provision.Removed{}, fmt.Errorf("engine: scaleway teardown: %w", err)
	}
	if err := sink.Forget(ctx, scaleway.AtlasSealedNames()...); err != nil {
		return provision.Removed{}, fmt.Errorf("engine: scaleway teardown: %w", err)
	}
	return provision.Removed{}, nil
}

// seatPlan is every agent that is on at Scaleway, whose secret key and access
// key the access sync seals as SCALEWAY_SECRET_KEY_<HANDLE> and
// SCALEWAY_ACCESS_KEY_<HANDLE>. A seat's tool naming either waits on them
// rather than launching without.
func (p *scalewayPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindScaleway)
}
