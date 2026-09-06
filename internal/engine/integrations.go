package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
)

// integrationDutyName is the fleet singleton the reconcile loop claims.
const integrationDutyName = "integration-reconcile"

// integrationDutyTTL is how long the duty survives without a re-claim.
//
// Three ticks, matching the retention sweep's and the sandbox waiter's ratio.
// The reason here is the sharper one: two nodes reconciling one surface at
// the same moment can both create an identity for one seat, so a single
// missed tick must not hand the duty to a peer.
const integrationDutyTTL = 3 * integration.Interval

// startIntegrations arms the reconcile loop.
//
// # Why registration is static and the config is read per pass
//
// A company document is edited live, so which surfaces are configured moves
// under a running loop. Rebuilding the worker on every apply would drop the
// duty lease on config changes that had nothing to do with integrations, so
// every reconciler here is registered once and reads [Engine.Company] on each
// pass instead. One whose block has gone answers
// [integration.ErrNotConfigured] and the loop forgets its status.
//
// # Which surfaces are registered, and why not all seven
//
// Only the ones whose existing pass is READ-ONLY. This loop runs unattended
// for the life of the deployment, so a pass that creates accounts or mints
// credentials on its own is a different and much larger decision than
// reporting what a vendor already has, and one an operator has to opt into
// rather than inherit from an upgrade.
//
// Jira and GitHub report rather than mint: neither vendor issues a credential
// on a provisioner's behalf, so their reconcile resolves each seat's identity
// and reads the instance. Run with no sink and no webhook base, which is
// exactly the posture `-dry-run` already uses on both subcommands, they write
// nothing at all.
//
// GitLab, Mattermost and Slack are deliberately absent. Each of their passes
// creates service accounts and mints tokens, and each refuses to run without
// a sink to record them in, so there is no read-only posture to put them in
// today. Wiring them here would mean the engine provisioning a vendor on its
// own schedule, which is a behaviour an operator must ask for.
func (e *Engine) startIntegrations(ctx context.Context) {
	// NO COORDINATION STORE, NO LOOP. A node without one has nowhere to
	// record what a pass finds, and a loop that ran anyway would spend a
	// vendor's rate limit on an answer nothing could read.
	if e.backends == nil || e.backends.Fleet == nil {
		log.InfoContext(ctx, "integration_reconciler_idle",
			"detail", "this node has no coordination store, so there is "+
				"nowhere to record what a pass finds")
		return
	}
	store, err := integration.NewCoordStore(e.backends.Fleet)
	if err != nil {
		log.ErrorContext(ctx, "integration_reconciler_unavailable", "error", err)
		return
	}

	regs := []integration.Registration{
		{Reconciler: &jiraConverger{engine: e}},
		{Reconciler: &githubConverger{engine: e}},
	}

	worker, err := integration.New(integration.Options{
		Registrations: regs,
		Store:         store,
		ClaimDuty: integration.DutyFunc(
			e.workerDuty(integrationDutyName, integrationDutyTTL)),
	})
	if err != nil {
		// NOT FATAL. A company whose reconcile loop could not be built
		// still runs every seat, and taking the engine down over a status
		// surface would trade a working company for a dashboard field.
		log.ErrorContext(ctx, "integration_reconciler_unavailable", "error", err,
			"detail", "the company is running without integration status; "+
				"the vendor subcommands still report the same findings")
		return
	}
	e.integrations = worker
	// Detached, for the reason the sweep's loop is: a loop bound to a
	// signal context stops at SIGTERM, which is harmless here but would
	// make its lifetime differ from every other loop's for no reason a
	// reader could find.
	worker.Start(context.WithoutCancel(ctx))
}

// Integrations exposes the reconcile loop.
//
// Exported for the same reason [Engine.Maintenance] is: "is anything checking
// my integrations, and what did it find" is a question an operator has to be
// able to ask, and the failure this whole subsystem removes was invisible
// precisely because nothing could answer it.
func (e *Engine) Integrations() *integration.Worker { return e.integrations }

// IntegrationStates reads what the fleet last found, for the API.
//
// Read from COORDINATION rather than from the worker's memory, and that is
// the point of storing it there: the node answering this request is usually
// not the node that holds the duty, because an operator running
// `-roles ingress` has put the API and the seats on separate hosts.
func (e *Engine) IntegrationStates(ctx context.Context) ([]integration.State, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, nil
	}
	store, err := integration.NewCoordStore(e.backends.Fleet)
	if err != nil {
		return nil, err
	}
	return store.LoadIntegrations(ctx)
}

// stopIntegrations ends the loop, waiting for a pass in flight.
func (e *Engine) stopIntegrations() {
	if e.integrations != nil {
		e.integrations.Stop()
	}
}

// jiraConverger reports what the tracker looks like now.
type jiraConverger struct{ engine *Engine }

func (jiraConverger) Kind() integration.Kind { return integration.KindJira }

func (c *jiraConverger) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	company := c.engine.Company()
	cfg := company.Config.Integrations.Jira
	if cfg == nil {
		return nil, integration.ErrNotConfigured
	}
	env := c.engine.resolver()

	base := jiraBaseURL(cfg, env)
	if base == "" {
		// A ${VAR} that resolved to nothing. Reported as a finding rather
		// than raised, because it is a statement about the operator's
		// configuration rather than a failure to read the world, and the
		// two get different cadences: this one is re-checked hourly
		// because nothing at Jira will ever change it.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: fmt.Sprintf(
				"neither integrations.jira.url (%q) nor cloud_id (%q) resolved "+
					"to anything, so there is nowhere to read the instance",
				cfg.URL, cfg.CloudID),
		}}, nil
	}
	token := strings.TrimSpace(env.Value(cfg.Token))
	if token == "" {
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing,
			Detail: fmt.Sprintf(
				"integrations.jira.token (%q) resolved to nothing, so the org "+
					"account cannot read the instance", cfg.Token),
		}}, nil
	}

	client, err := jira.NewClient(jira.ClientOptions{
		URL: base, Email: env.Value(cfg.Email), Token: token,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: jira reconcile: %w", err)
	}
	// NO SINK AND NO WEBHOOK BASE, which is what makes this read-only:
	// the pass resolves each seat's identity and reads the instance, and
	// registers nothing. It is the same posture `crewlet jira provision
	// -dry-run` runs in.
	res, err := jira.Reconcile(ctx, jira.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: jira reconcile: %w", err)
	}
	return res.Findings(), nil
}

// githubConverger reports what the code host looks like now.
type githubConverger struct{ engine *Engine }

func (githubConverger) Kind() integration.Kind { return integration.KindGitHub }

func (c *githubConverger) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	company := c.engine.Company()
	cfg := company.Config.Integrations.GitHub
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	env := c.engine.resolver()

	client, err := githubReconcileClient(cfg, env)
	if err != nil {
		return nil, fmt.Errorf("engine: github reconcile: %w", err)
	}
	res, err := github.Reconcile(ctx, github.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: github reconcile: %w", err)
	}
	return res.Findings(), nil
}

// githubReconcileClient builds the org client the read-only pass uses.
//
// The org token is OPTIONAL on this host, and its absence is a documented
// degradation rather than a failure, so a nil client is a valid thing to hand
// the pass: it then reports every seat's identity and no organization-level
// finding.
func githubReconcileClient(cfg *config.GitHub, env *config.Resolver) (*github.Client, error) {
	token := strings.TrimSpace(env.Value(cfg.Token))
	if token == "" {
		return nil, nil
	}
	// RESOLVED FIRST, then asked for its bases. APIBase and WebURL are
	// derived from URL, so building them off the unresolved block would
	// point an Enterprise Server deployment at github.com whenever the
	// host is written as a ${VAR}.
	resolved := *cfg
	resolved.URL = strings.TrimSpace(env.Value(cfg.URL))
	return github.NewClient(github.ClientOptions{
		APIBase: resolved.APIBase(), WebBase: resolved.WebURL(), Token: token,
	})
}
