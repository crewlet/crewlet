package engine

import (
	"context"
	"errors"
	"strings"

	awsint "github.com/crewlet/crewlet/internal/aws"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// awsPass checks the company's half of AWS, the stack its agents' roles are
// made through, then runs each agent's role through the access sync. No
// Teardown: the stack is the owner's, and only the owner deletes it.
type awsPass struct{ engine *Engine }

func (*awsPass) Kind() integration.Kind { return integration.KindAWS }

// Needs is nil: the certificate authority is minted, never asked for.
func (*awsPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *awsPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

// run holds the account's stack to what this engine renders, as the Test
// button does, so a stack edited by hand or left at an older version is a
// finding before an agent's role depends on it.
func (p *awsPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company().Config
	cfg := company.Integrations.AWS
	// DISABLED IS NOT CONFIGURED: switching the block off is how an operator
	// stops the engine acting in AWS.
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	const subject = "integrations.aws.provisioning.connection"
	client, err := awsint.ClientFor(company, p.engine.resolver(), nil)
	switch {
	case errors.Is(err, awsint.ErrNotConnected):
		return []integration.Finding{{
			Kind: integration.FindingApprovalRequired, Subject: subject,
			Detail: "the Crewlet stack's output is not pasted yet: run the CloudShell block on the AWS connection, " +
				"then paste the last line it prints",
		}}, nil
	case err != nil:
		return nil, err
	}
	// THE ATLAS'S ROLE IS HELD TO THE IDENTITY SPLIT AND NOTHING ELSE HERE: no
	// agent depends on it, so what it lacks (report.Atlas) is said by the
	// read that could not sign in as it, on the atlas's own sources.
	report, err := client.CheckStack(ctx, cfg.Provisioning.RegionsOrDefault(), cfg.Provisioning.AtlasRead)
	if err != nil {
		return nil, integration.Reject(err, awsint.Status(err))
	}
	var problems []string
	if report.Outdated() {
		problems = append(problems, "the stack is not the version this engine renders")
	}
	problems = append(append(problems, report.Split...), report.Drift...)
	if len(problems) > 0 {
		return []integration.Finding{{
			Kind: integration.FindingApprovalRequired, Subject: subject,
			Detail: strings.Join(problems, " ") + " Run the CloudShell block on the AWS connection again to update " +
				"the stack.",
		}}, nil
	}
	return nil, nil
}
