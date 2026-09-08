package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"

	"github.com/crewlet/crewlet/internal/setup"
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
// # CONNECTING IS THE PERMISSION
//
// This loop used to run every pass with no sink and no webhook base, on the
// reasoning that a base is permission to register a hook and a sink is
// permission to mint a credential, and neither is a decision a timer gets to
// make. That reasoning had a hole in it: the operator HAS made the decision.
// They opened the connect form, pasted an organization credential and pressed
// Connect, which is a person asking for exactly this. What the withheld
// permissions actually bought was a screen full of buttons (Run setup,
// Recheck) asking them to say yes a second time, and an integration that sat
// unprovisioned until they found the right one.
//
// So a pass here runs with both. What still cannot happen unattended is
// anything a person has NOT asked for: a company with no block is not
// reconciled, a block with no credential reports a finding rather than
// acting, and nothing is ever DELETED by the loop except through a disconnect
// somebody pressed.
//
// # One implementation per surface
//
// The reconciler is the SAME [setup.Pass] the dashboard's own button used to
// run, wrapped by [passConverger]. There were two spellings of this before,
// a converger for the loop and a pass for the button, which is two chances
// to disagree about what an integration's state is depending on which of them
// last touched it.

func (e *Engine) startIntegrations(ctx context.Context) {
	// NO COORDINATION STORE, NO LOOP. A node without one has nowhere to
	// record what a pass finds, and a loop that ran anyway would spend a
	// third-party app's rate limit on an answer nothing could read.
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

	// EVERY SURFACE THIS BUILD CAN REMOVE, paired with the seam that
	// removes it, and every surface it can converge, from the one
	// implementation the dashboard's button also used to run.
	drop := e.disconnectors()
	regs := make([]integration.Registration, 0, len(integration.Kinds))
	converged := map[integration.Kind]bool{}
	for _, pass := range e.setupPasses() {
		converged[pass.Kind()] = true
		regs = append(regs, integration.Registration{
			Reconciler:   &passConverger{pass: pass, engine: e},
			Disconnector: drop[pass.Kind()],
		})
	}
	// TEARDOWN ONLY for a surface with no pass. Slack's apps are created
	// from the command line, so there is nothing here to converge, but a
	// disconnect for it still has a block to drop, and without a
	// registration that intent would sit on the fleet row for ever.
	for _, kind := range integration.Kinds {
		if converged[kind] {
			continue
		}
		regs = append(regs, integration.Registration{
			Only: kind, Disconnector: drop[kind],
		})
	}

	worker, err := integration.New(integration.Options{
		Registrations: regs,
		Store:         store,
		// WHERE THIRD-PARTY APPS REACH THIS DEPLOYMENT, read fresh on
		// every pass rather than captured here: it is a field of the
		// applied revision and an apply can change it.
		Endpoint: func() string {
			// EMPTY WHERE THIS NODE CANNOT SAY, which is what the worker
			// reads a missing endpoint as. A node with no active revision
			// has no public base to have registered anything against.
			company := e.Company()
			if company == nil {
				return ""
			}
			return company.Config.Integrations.WebhookBase(e.resolver().LookupOK)
		},
		ClaimDuty: integration.DutyFunc(
			e.workerDuty(integrationDutyName, integrationDutyTTL)),
	})
	if err != nil {
		// NOT FATAL. A company whose reconcile loop could not be built
		// still runs every seat, and taking the engine down over a status
		// surface would trade a working company for a dashboard field.
		log.ErrorContext(ctx, "integration_reconciler_unavailable", "error", err,
			"detail", "the company is running without integration status; "+
				"the integration subcommands still report the same findings")
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
	store, err := e.IntegrationStore()
	if err != nil || store == nil {
		return nil, err
	}
	return store.LoadIntegrations(ctx)
}

// IntegrationStore is the fleet row every pass writes its findings to.
//
// Exported because a pass an operator runs from the dashboard writes the SAME
// row the reconcile loop writes, and a second store built beside it would let
// the two disagree about an integration's state depending on which surface
// last touched it.
//
// Nil with no error is a node with no coordination store: there is nowhere
// for a status to live, which is a real posture rather than a failure.
func (e *Engine) IntegrationStore() (*integration.CoordStore, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, nil
	}
	return integration.NewCoordStore(e.backends.Fleet)
}

// stopIntegrations ends the loop, waiting for a pass in flight.
func (e *Engine) stopIntegrations() {
	if e.integrations != nil {
		e.integrations.Stop()
	}
}

// The helpers below outlived the convergers that used to sit here: the
// passes need them for exactly the same reason, which is what made the two
// implementations duplicates rather than neighbours.

// githubReconcileClient builds the org client the read-only pass uses.
//
// The org token is OPTIONAL on this host, and its absence is a documented
// degradation rather than a failure, so a nil client is a valid thing to hand
// the pass: it reports credential_missing and reads nothing, which is what an
// operator needs to see and not the fault this used to produce.
func githubReconcileClient(cfg *config.GitHub, env *config.Resolver) (*github.Client, error) {
	token := strings.TrimSpace(env.Value(cfg.Token))
	if token == "" {
		return nil, nil
	}
	apiBase, webBase := githubBases(cfg, env)
	return github.NewClient(github.ClientOptions{
		APIBase: apiBase, WebBase: webBase, Token: token,
	})
}

// githubBases is where GitHub is reached: the REST base and the browser base,
// both DERIVED from a RESOLVED url.
//
// RESOLVED FIRST, then asked for its bases. [config.GitHub.APIBase] and
// [config.GitHub.WebURL] are derived from URL, so building them off the
// unresolved block points an Enterprise Server deployment at github.com
// whenever the host is written as a ${VAR}.
//
// AND DERIVED RATHER THAN PASSED THROUGH. The REST base is `<url>/api/v3` on
// Enterprise Server and `api.github.com` when the field is empty; the raw url
// is neither. Handing it over as the API base is not inert on the seat-app
// half: every call 404s, and [github.ReconcileSeatApps] reads a 404 from the
// installation endpoints as an app GitHub no longer has, so it FORGETS the
// seat's app — clearing the id, the slug and the pointer to a key GitHub
// issues once and never reissues, for an app that exists and works.
//
// One helper because there are three callers and the empty-url case makes the
// wrong one look right: github.com works either way, so a copy that skipped
// this was only ever wrong on Enterprise Server.
func githubBases(cfg *config.GitHub, env *config.Resolver) (apiBase, webBase string) {
	if cfg == nil {
		return "", ""
	}
	resolved := *cfg
	resolved.URL = strings.TrimSpace(env.Value(cfg.URL))
	return resolved.APIBase(), resolved.WebURL()
}

// confluenceBaseURL is the REST base this node reads the wiki on.
func confluenceBaseURL(cfg *config.Confluence, env *config.Resolver) string {
	resolved := config.Confluence{
		URL:     strings.TrimSpace(env.Value(cfg.URL)),
		CloudID: strings.TrimSpace(env.Value(cfg.CloudID)),
	}
	return resolved.BaseURL()
}

// passConverger runs a [setup.Pass] as the loop's reconciler.
//
// ONE IMPLEMENTATION PER SURFACE, reached two ways. The pass is what the
// dashboard's own button used to run, and the loop ran a second thing beside
// it; two spellings of the same work are two chances to disagree about what
// an integration's state is depending on which of them touched it last.
//
// It supplies the sink and the webhook base the loop used to withhold. See
// this file's own doc for why: connecting is the operator asking for exactly
// this, and withholding them bought nothing but a screen of buttons asking
// them to say so twice.
type passConverger struct {
	pass   setup.Pass
	engine *Engine
}

func (c *passConverger) Kind() integration.Kind { return c.pass.Kind() }

func (c *passConverger) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	company := c.engine.Company()
	if company == nil {
		// NO COMPANY, NOTHING TO CONVERGE. A node runs with no active
		// revision at all — it is how one boots before a company is ever
		// imported, and how one keeps serving when the fleet's revision
		// cannot be read. Every surface is then UNCONFIGURED rather than
		// broken, which is exactly what the sentinel says and what the
		// loop already does the right thing with: it forgets the status
		// rather than recording a fault against a company that does not
		// exist yet.
		return nil, integration.ErrNotConfigured
	}
	// A SINK IS BEST EFFORT HERE. A node with no keyring cannot seal a
	// minted credential, but it can still read a surface and report what
	// it finds, and reporting is most of what this loop is for. The pass
	// treats a nil sink as a dry run, which is the honest posture for a
	// node that could not have recorded what it created.
	sink, err := c.engine.SetupSink(reconcileOperator)
	if err != nil {
		log.WarnContext(ctx, "integration_sink_unavailable",
			"integration", c.pass.Kind().String(), "error", err,
			"detail", "this pass reads and reports; it will mint nothing")
		sink = nil
	}
	// UNDER THE SURFACE'S OWN LEASE, the same one an operator's pass takes.
	//
	// This tick and that button run THE SAME [setup.Pass] with the same sink
	// and the same webhook base, so they create the same accounts, mint the
	// same tokens and register the same hooks. Two of them at once is the
	// collision the worker's own singleton exists to rule out — both read a
	// surface with no account for a seat, both create one — reachable here
	// between the loop and the dashboard rather than between two nodes.
	//
	// The loop's own `integration-reconcile` duty does not cover it: that
	// one answers "which node runs the loop", which is a different question
	// from "who is writing at this surface", and a lease keyed on a
	// different name excludes nobody.
	if duty := c.engine.setupDuty(c.pass.Kind()); duty != nil {
		release, held, err := duty(ctx)
		switch {
		case err != nil:
			// UNKNOWN IS NOT FREE. A coordination store that could not
			// answer has not said the surface is idle, and the whole
			// point of the lease is that acting on that guess is what
			// creates the duplicate.
			return nil, fmt.Errorf("%w: %w", integration.ErrReconcileUnavailable, err)
		case !held:
			// SOMEBODY IS ALREADY DOING THIS. Nothing is recorded and no
			// attempt is counted — see [integration.ErrReconcileUnavailable].
			return nil, integration.ErrReconcileUnavailable
		}
		defer release()
	}
	return c.pass.Run(ctx, setup.PassInput{
		Sink:        sink,
		WebhookBase: company.Config.Integrations.WebhookBase(c.engine.resolver().LookupOK),
	})
}

// reconcileOperator is who the loop's writes are attributed to, so an audit
// row says a timer did this rather than naming a person who did not.
const reconcileOperator = "reconcile loop"
