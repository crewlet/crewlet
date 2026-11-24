package engine

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"

	"github.com/crewlet/crewlet/internal/setup"
)

// integrationDutyName is the fleet singleton the reconcile loop claims.
const integrationDutyName = "integration-reconcile"

// integrationDutyTTL is how long the duty survives without a re-claim: the
// longest pass it admits ([setup.PassDeadline]) plus a tick to renew in.
// [integration.Worker] re-claims before every surface, so it covers ONE pass,
// not a sweep. Shorter, the duty lapses mid-pass and a peer sweeps beside the
// holder: not unsafe (each surface's lease stops two writers), but the sweep
// turns nondeterministic and `integration_duty_lost` routine. The cost, a dead
// holder's duty unclaimable this long, is under half a settled interval.
//
// The NAME stays across a TTL change: a claim writes its own expiry
// ([coord.Lease.ExpiresAt]) and readers honour it, so builds with two TTLs
// agree on the holder, while a second name would be two locks both held,
// sweeping every surface twice through a rolling upgrade.
const integrationDutyTTL = setup.PassDeadline + 2*integration.Interval

// startIntegrations arms the reconcile loop.
//
// Every reconciler is registered once and reads [Engine.Company] per pass:
// rebuilding the worker on each apply would drop the duty lease on changes
// unrelated to integrations. A surface whose block has gone answers
// [integration.ErrNotConfigured] and the loop forgets its status.
//
// CONNECTING IS THE PERMISSION: a pass runs with a sink and a webhook base,
// because the operator who pressed Connect asked for exactly this. Nothing a
// person has NOT asked for happens: no block, no reconcile; no credential, a
// finding; nothing is deleted except through a disconnect somebody pressed.
// The reconciler is the dashboard's own [setup.Pass] ([passConverger]).

func (e *Engine) startIntegrations(ctx context.Context) {
	// NO COORDINATION STORE, NO LOOP: nowhere to record a pass's findings, so
	// running anyway would spend a vendor's rate limit on an answer nobody reads.
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

	// Each surface's pass, paired with the seam that removes the surface.
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
	// TEARDOWN ONLY for a kind with no pass (none in this build): its block can
	// still be disconnected, and with no registration that intent would sit on
	// the fleet row for ever.
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
		// Endpoint, Registration, SettledInterval and Configured read the
		// applied revision per pass, never a value captured now: an apply can
		// change it.
		Endpoint: func() string {
			// Empty where this node cannot say, which the worker reads as
			// missing. The VENDORS' address, never the one a person reaches:
			// behind a public listener they differ ([config.API.WebhookBase]).
			return e.webhookBase
		},
		// The name a registration is held under, for the one surface whose
		// address cannot find it again; a change to it is what this notices.
		Registration: func(kind integration.Kind) string {
			company := e.Company()
			if company == nil || kind != integration.KindDatadog {
				return ""
			}
			cfg := company.Config.Integrations.Datadog
			if cfg == nil {
				return ""
			}
			return datadog.WebhookNameOf(cfg)
		},
		// How long a converged surface is trusted: the only thing that finds
		// access revoked by hand at the vendor, so a shortened interval must
		// take effect at once.
		SettledInterval: func() time.Duration {
			company := e.Company()
			if company == nil {
				return 0
			}
			return company.Config.Integrations.CheckInterval()
		},
		ClaimDuty: integration.DutyFunc(
			e.workerDuty(integrationDutyName, integrationDutyTTL)),
		// THE ONE GUARD every writer at a surface takes, held by the worker
		// across the row re-read, the pass and the status write
		// ([Engine.holdSurface]; `passConverger.Reconcile` says why the pass
		// takes none).
		Guard: integration.Guard(e.holdSurface),
		// The posture gate, asked before the duty is claimed: a node the fleet
		// has moved past would converge vendors to a replaced revision, so its
		// lease lapses to a peer holding the current one.
		Admits: integration.AdmitsFunc(e.admits),
		// What the document still declares, for the teardown-only surfaces
		// that have no pass to answer it. See [integration.Options].Configured.
		Configured: func(kind integration.Kind) bool {
			company := e.Company()
			if company == nil {
				// No active revision is a node that cannot say, not "declares
				// nothing": forgetting every row on it would delete the
				// fleet's status during a boot.
				return true
			}
			return company.Config.DeclaresIntegration(kind.String())
		},
	})
	if err != nil {
		// NOT FATAL: the company still runs every seat, and the loop is only
		// a status surface.
		log.ErrorContext(ctx, "integration_reconciler_unavailable", "error", err,
			"detail", "the company is running without integration status; "+
				"the integration subcommands still report the same findings")
		return
	}
	e.integrations = worker
	// Detached like the sweep's loop, so its lifetime matches every other
	// loop's rather than ending at SIGTERM.
	worker.Start(context.WithoutCancel(ctx))
}

// Converges reports whether a reconcile pass converges this surface in this
// build: a property of the build, read from [Engine.setupPasses] rather than
// the runner, which is nil on a node with no coordination store. It tells a
// surface awaiting its first report from one that will never get one.
func (e *Engine) Converges(kind integration.Kind) bool {
	for _, pass := range e.setupPasses() {
		if pass.Kind() == kind {
			return true
		}
	}
	return false
}

// Integrations exposes the reconcile loop, exported as [Engine.Maintenance]
// is: whether anything checks the integrations, and what it found, is an
// operator's question to ask.
func (e *Engine) Integrations() *integration.Worker { return e.integrations }

// IntegrationStates reads what the fleet last found, each surface's status
// with its findings, for the API, from coordination rather than the worker's
// memory: the node answering is usually not the duty holder (`-roles ingress`
// splits the API from the seats).
func (e *Engine) IntegrationStates(ctx context.Context) ([]integration.State, error) {
	store, err := e.IntegrationStore()
	if err != nil {
		return nil, err
	}
	return store.LoadIntegrationsWithFindings(ctx)
}

// IntegrationStatuses reads each surface's status alone, for a reader that
// shows no findings (the Advisor duty's signature names them by digest).
func (e *Engine) IntegrationStatuses(ctx context.Context) ([]integration.State, error) {
	store, err := e.IntegrationStore()
	if err != nil {
		return nil, err
	}
	return store.LoadIntegrations(ctx)
}

// IntegrationStore is the fleet row every pass writes its findings to,
// exported so the dashboard's pass writes the SAME row as the loop rather
// than a second store that could disagree. Every engine holds a fleet store
// ([New] refuses backends without one).
func (e *Engine) IntegrationStore() (*integration.CoordStore, error) {
	return integration.NewCoordStore(e.backends.Fleet)
}

// stopIntegrations ends the loop, waiting for a pass in flight.
func (e *Engine) stopIntegrations() {
	if e.integrations != nil {
		e.integrations.Stop()
	}
}

// githubReconcileClient builds the org client the read-only pass uses. The
// org token is OPTIONAL here, so no token is a nil client and no error: the
// pass then reports credential_missing and reads nothing.
func githubReconcileClient(cfg *config.GitHub, env *config.Resolver) (*github.Client, error) {
	token := strings.TrimSpace(env.Value(cfg.Token))
	if token == "" {
		return nil, nil
	}
	apiBase, webBase := cfg.Bases(env.LookupOK)
	return github.NewClient(github.ClientOptions{
		APIBase: apiBase, WebBase: webBase, Token: token,
	})
}

// confluenceBaseURL is the REST base this node reads the wiki on, resolved
// ([config.Confluence.ResolvedBaseURL]).
func confluenceBaseURL(cfg *config.Confluence, env *config.Resolver) string {
	return cfg.ResolvedBaseURL(env.Value)
}

// passConverger runs a [setup.Pass] as the loop's reconciler, so the loop and
// the dashboard's button run ONE implementation per surface and cannot
// disagree about its state. It supplies the sink and the webhook base
// (startIntegrations: connecting is the permission).
type passConverger struct {
	pass   setup.Pass
	engine *Engine
}

func (c *passConverger) Kind() integration.Kind { return c.pass.Kind() }

// DueBy is when the kind's next pass is due at the latest: the instant its
// earliest live grant leaves an agent's credential ([seataccess.NextExpiry]),
// so a grant never outlives its expiry waiting for the settled interval.
func (c *passConverger) DueBy(now time.Time) time.Time {
	company := c.engine.Company()
	if company == nil {
		return time.Time{}
	}
	return seataccess.NextExpiry(company.Config, c.pass.Kind(), now)
}

func (c *passConverger) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	// A CANCELLED PASS IS A FAULT, checked first: an empty finding list reads
	// as "ready" for a settled interval, and ErrNotConfigured forgets the
	// status row, so a node draining at shutdown would delete the fleet's
	// integration status on its way out. Checked here, the one frame the loop
	// reaches every [setup.Pass] through, and in [setup.Runner.Execute] for
	// the dashboard's; each vendor's own Reconcile checks too (its conformance
	// harness drives it). The passes in setuppass.go carry none: seven copies
	// of a rule with two homes.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	company := c.engine.Company()
	if company == nil {
		// NO COMPANY, NOTHING TO CONVERGE: a node runs with no active revision
		// before any import and while the fleet's cannot be read. Every
		// surface is then unconfigured rather than broken, so the loop forgets
		// its status rather than record a fault.
		return nil, integration.ErrNotConfigured
	}
	// The sink the dashboard's pass is handed, so a credential the loop mints
	// lands where an operator's does. Every node holds the keyring, so a sink
	// that cannot be built is a fault, never a posture to carry on in.
	sink, err := c.engine.SetupSink(loopActor())
	if err != nil {
		return nil, err
	}
	// NO GUARD IS TAKEN HERE: [integration.Worker] holds it around the whole
	// visit (row re-read, this pass, status write) through the Guard wired in
	// `startIntegrations`. Released when this returns, it would leave the
	// status write outside it, and a disconnect landing there is overwritten.
	// Taking it again is not redundant but fatal: [setup.Runner]'s in-process
	// claim is NOT reentrant, so it would answer not-held on every tick, while
	// the coord lease IS reentrant for its owner, which makes the mistake easy.
	// ctx is the guard's, bounded by [setup.PassDeadline] inside the lease.
	findings, err := c.pass.Run(ctx, setup.PassInput{
		Sink:           sink,
		By:             loopActor(),
		WebhookBase:    c.engine.webhookBase,
		WebhookBaseKey: c.engine.webhookBaseKey,
	})
	if err != nil {
		return nil, err
	}
	// AND WHAT THIS NODE'S WIRING COULD NOT RESOLVE, which no vendor pass
	// sees: a seat's tracker or code-host account, read with the seat's own
	// credential by the engine. This visit is the retry
	// ([Engine.resolveRouting]), and the finding keeps the visits coming and
	// the card from reading ready over a seat that receives nothing.
	return append(findings,
		c.engine.resolveRouting(ctx, c.pass.Kind(), reported(findings))...), nil
}

// reported is the set of seats the surface's own pass already said something
// about. ONE CAUSE, ONE FINDING: the pass and this node's wiring resolve the
// same seats with the same credential, so one failed lookup would be two
// findings disagreeing about who acts (an admin, or the engine). The pass's
// wins, carrying the vendor's words; the wiring adds only the seats it said
// nothing about (every seat on both code hosts, whose passes report no
// per-seat identity).
func reported(findings []integration.Finding) map[string]bool {
	seen := make(map[string]bool, len(findings))
	for _, f := range findings {
		if f.Subject != "" {
			seen[f.Subject] = true
		}
	}
	return seen
}

// loopActor is who the loop's writes are attributed to, so an audit row says a
// timer did this rather than naming a person who did not: the ENGINE's kind,
// and no credential, because none made the write.
func loopActor() iam.Actor {
	return iam.Actor{Name: "reconcile loop", Kind: iam.ActorSystem}
}
