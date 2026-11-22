package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// googleAnalyticsPass checks the company's half of Google Analytics: the
// Google identity it signs in through, and its two APIs on the identity
// project, which the Cloud Shell block turns on. Then it runs the agents'
// access through the access sync, which takes no step: a seat's level and
// properties are the document alone, its identity the Google identity pass's
// (googlegrowth.go). No Teardown: nothing is registered at Google.
type googleAnalyticsPass struct{ engine *Engine }

func (*googleAnalyticsPass) Kind() integration.Kind { return integration.KindGoogleAnalytics }

// Needs is nil: the integration holds no credential.
func (*googleAnalyticsPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *googleAnalyticsPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *googleAnalyticsPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	if company == nil || !company.Config.Integrations.GrowthEnabled(string(integration.KindGoogleAnalytics)) {
		return nil, integration.ErrNotConfigured
	}
	client, err := googlecloud.ClientFor(company.Config, p.engine.resolver(), nil)
	if err != nil {
		//nolint:nilerr // Deliberate: the Google identity's absence is the person's finding.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: "integrations.google_cloud.provisioning",
			Detail: "Google Analytics signs in through the Google Cloud connection's identity: " +
				googleauth.Sentence(err),
			Remedy: "Connect Google Cloud first; no project needs preparing",
		}}, nil
	}
	return missingAPIs(ctx, client, integration.KindGoogleAnalytics)
}
