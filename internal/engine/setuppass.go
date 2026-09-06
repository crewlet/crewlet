package engine

import (
	"context"
	"fmt"
	"time"

	"strings"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// Running a vendor's provisioning from the API rather than from a shell.
//
// # This is where the loop's two deliberate absences are filled in
//
// The reconcile loop hands every vendor a pass with no sink and no webhook
// base, and states plainly why: a base is permission to register a hook and a
// sink is permission to mint a credential, and neither is a decision a timer
// gets to make. A pass here supplies both, because a person asked for it.
//
// The vendor function is the SAME one, in every case. Nothing about
// provisioning is reimplemented for the API: what an adapter does is resolve
// the config, build the client, and hand over the sink and the base the loop
// withholds.

// setupPasses are the vendors this build can provision over the API.
//
// The three whose passes neither create an account nor issue a credential AT
// the vendor. Each reads the instance, mints a webhook credential whose value
// is the engine's own on both ends, and registers a hook, and none of them
// produces anything a person then owns and has to be told about.
//
// GitLab is here too, and it is a different kind of act: its pass creates a
// service account per agent, mints a token on each and adds them to a group.
// Everything it makes outlives the run and is visible to the whole group, so
// it runs only with a transient administrator credential asked for on every
// pass and never stored, and it never deletes: decommissioning is left to the
// command line, because a company mid-edit looks exactly like one that
// removed a seat.
//
// Mattermost is the same shape as GitLab and joins on the same terms. It is
// the one vendor here that needs no public address at all: it holds an
// outbound websocket per seat and verifies no inbound delivery, so its pass
// creates accounts and registers nothing.
//
// Slack stays on the command line. Its apps are created through Slack's
// app-manifest API, which authenticates with a configuration token Slack
// issues only by hand and which an organisation may not permit at all, and
// the record of what was created lives in a local ledger file rather than in
// the fleet.
func (e *Engine) setupPasses() []setup.Pass {
	return []setup.Pass{
		&githubPass{engine: e},
		&jiraPass{engine: e},
		&confluencePass{engine: e},
		&gitlabPass{engine: e},
		&mattermostPass{engine: e},
	}
}

// SetupRunner is the pass runner this node serves, or nil when it has no
// secret store to mint into.
//
// Nil rather than a runner that refuses: a pass that cannot record what it
// mints must not run at all, and the surface above answers 503 naming the
// keyring rather than starting something it will have to unwind.
func (e *Engine) SetupRunner(now func() time.Time) *setup.Runner {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return setup.NewRunner(e.setupPasses(), e.setupDuty, now)
}

// setupDuty is the fleet lease one vendor's pass holds while it runs.
//
// The SAME mechanism the reconcile loop's singleton uses, under its own name,
// so a pass and a loop tick for one vendor never overlap either. The TTL is
// generous relative to a pass: a lease that expired mid-run would let a
// second node start minting while the first was still writing.
func (e *Engine) setupDuty(kind integration.Kind) setup.Duty {
	duty := e.workerDuty("setup-provision-"+string(kind), setupLeaseTTL)
	if duty == nil {
		return nil
	}
	return setup.Duty(duty)
}

// setupLeaseTTL bounds how long one pass may hold its vendor.
//
// Five minutes against passes measured in seconds: the value is a backstop
// for a node that died mid-run, not a deadline for the work. Shorter would
// risk a live pass losing its lease; much longer would leave a vendor locked
// out after a crash for no benefit.
const setupLeaseTTL = 5 * time.Minute

// SetupSink is the recorder a pass writes minted credentials through.
//
// The SAME type `crewlet <vendor> provision -secret-store` builds, so a
// credential minted from the dashboard and one minted from a shell land in
// the same place under the same envelope. Write-through, so a value is
// durable before the next one is minted.
func (e *Engine) SetupSink(operator string) (provision.TokenSink, error) {
	if e.cipher == nil {
		return nil, fmt.Errorf("engine: this node has no keyring, so a minted credential cannot be sealed")
	}
	return provision.NewSecretStoreSink(
		fleetsecrets.New(e.backends.Fleet, e.cipher), operator), nil
}

// jiraPass adapts jira.Reconcile to the pass contract.
type jiraPass struct{ engine *Engine }

func (*jiraPass) Kind() integration.Kind { return integration.KindJira }

// Needs is nil: the org account already in the config is what registers the
// hook, so there is no second administrator credential to ask for.
func (*jiraPass) Needs() *setup.Requirement { return nil }

func (p *jiraPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Jira
	if cfg == nil {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	base := jiraBaseURL(cfg, env)
	token := strings.TrimSpace(env.Value(cfg.Token))
	if base == "" || token == "" {
		// The same two facts the loop reports as findings, and reported
		// the same way here: a pass that cannot reach the instance has
		// observed nothing, and calling that a fault would send an
		// operator looking for an outage.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "the Jira site address or the org token did not resolve, so " +
				"this pass could not reach the instance",
		}}, nil
	}
	client, err := jira.NewClient(jira.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: jira pass: %w", err)
	}
	res, err := jira.Reconcile(ctx, jira.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		Sink: in.Sink, WebhookBase: in.WebhookBase,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: jira pass: %w", err)
	}
	return res.Findings(), nil
}

// confluencePass adapts confluence.Reconcile to the pass contract.
type confluencePass struct{ engine *Engine }

func (*confluencePass) Kind() integration.Kind { return integration.KindConfluence }

func (*confluencePass) Needs() *setup.Requirement { return nil }

func (p *confluencePass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Confluence
	if cfg == nil {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	base := confluenceBaseURL(cfg, env)
	token := strings.TrimSpace(env.Value(cfg.Token))
	if base == "" || token == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "the Confluence site address or the org token did not resolve, " +
				"so this pass could not reach the instance",
		}}, nil
	}
	client, err := confluence.NewClient(confluence.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: confluence pass: %w", err)
	}
	res, err := confluence.Reconcile(ctx, confluence.Options{
		Client: client, Config: cfg, Value: env.Value,
		Sink: in.Sink, WebhookBase: in.WebhookBase, Recreate: in.Recreate,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: confluence pass: %w", err)
	}
	return res.Findings(), nil
}

// gitlabPass adapts gitlab.Reconcile to the pass contract.
type gitlabPass struct{ engine *Engine }

func (*gitlabPass) Kind() integration.Kind { return integration.KindGitLab }

// Needs is the group Owner token. Asked on every run and never stored: it
// creates accounts and mints tokens on them, which is a standing power if it
// is kept and a grant with an end if it is not.
func (*gitlabPass) Needs() *setup.Requirement {
	req := gitlab.OperatorCredential()
	return &req
}

func (p *gitlabPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitLab
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	if strings.TrimSpace(in.Operator) == "" {
		// A FINDING, NOT A FAULT: the operator has not supplied the one
		// credential this pass cannot mint for itself, which is a fact
		// about what is missing rather than a failure to look.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "no group Owner token was supplied, and the seats' own tokens " +
				"are what this pass mints, so it cannot bootstrap itself from them",
		}}, nil
	}
	env := p.engine.resolver()
	plan, err := gitlab.PlanFor(company.Org, cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: gitlab pass: %w", err)
	}
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: env.Value(cfg.URL), Token: in.Operator,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: gitlab pass: %w", err)
	}
	signingVar, _ := provision.SoleVar(cfg.SigningSecret)
	res, err := gitlab.Reconcile(ctx, gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: in.Sink,
		WebhookBase:      in.WebhookBase,
		SigningSecret:    env.Value(cfg.SigningSecret),
		SigningSecretVar: signingVar,
		// ROTATE ONLY WHEN ASKED, and NEVER DECOMMISSION from here.
		// Rotating revokes the credential every agent is currently
		// authenticating with, so an operator adding a tenth seat would
		// take the other nine down; deleting an account because a seat
		// left the config cannot be told apart from a company mid-edit.
		// Both stay deliberate gestures on the command line.
		Rotate: in.Recreate,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: gitlab pass: %w", err)
	}
	return res.Findings(), nil
}

// mattermostPass adapts mattermost.Reconcile to the pass contract.
type mattermostPass struct{ engine *Engine }

func (*mattermostPass) Kind() integration.Kind { return integration.KindMattermost }

// Needs is the administrator token. Asked on every run and never stored, for
// the reason GitLab's is: it creates accounts and mints tokens on them.
func (*mattermostPass) Needs() *setup.Requirement {
	req := mattermost.OperatorCredential()
	return &req
}

func (p *mattermostPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.Mattermost
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	if strings.TrimSpace(in.Operator) == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: "no administrator token was supplied, and the bots' own tokens " +
				"are what this pass mints, so it cannot bootstrap itself from them",
		}}, nil
	}
	env := p.engine.resolver()
	plan, err := mattermost.PlanFor(company.Org, cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: env.Value(cfg.URL), Token: in.Operator,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	// NO WEBHOOK BASE IS PASSED because there is nowhere to pass it: this
	// vendor holds an outbound socket per seat and registers nothing. And
	// no rotation and no decommissioning, for the reasons GitLab's pass
	// gives: both take working agents down and both stay deliberate
	// command-line gestures.
	res, err := mattermost.Reconcile(ctx, mattermost.Options{
		Client: client, Config: cfg, Org: company.Org, Plan: plan, Sink: in.Sink,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: mattermost pass: %w", err)
	}
	return res.Findings(), nil
}

// githubPass adapts github.Reconcile to the pass contract.
type githubPass struct{ engine *Engine }

func (*githubPass) Kind() integration.Kind { return integration.KindGitHub }

// Needs is nil: GitHub issues nothing on a provisioner's behalf, so this pass
// asks for no transient administrator credential. What it writes at the
// vendor, it writes with the organization token already in the config.
func (*githubPass) Needs() *setup.Requirement { return nil }

func (p *githubPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitHub
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	client, err := githubReconcileClient(cfg, env)
	if err != nil {
		return nil, fmt.Errorf("engine: github pass: %w", err)
	}
	res, err := github.Reconcile(ctx, github.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		// THE TWO THE LOOP WITHHOLDS. A base is permission to register,
		// a sink is permission to mint, and a person asked for both.
		Sink:             in.Sink,
		WebhookBase:      in.WebhookBase,
		RecreateWebhooks: in.Recreate,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: github pass: %w", err)
	}
	return res.Findings(), nil
}
