package engine

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"

	"github.com/crewlet/crewlet/internal/setup"
)

// integrationDutyName is the fleet singleton the reconcile loop claims.
const integrationDutyName = "integration-reconcile"

// integrationDutyTTL is how long the duty survives without a re-claim.
//
// DERIVED FROM THE LONGEST PASS IT ADMITS, plus a tick to renew in. It was
// three ticks — 45 seconds — chosen to match the retention sweep's ratio, and
// that number cannot be right beside a pass allowed [setup.PassDeadline]: one
// surface's pass blows a 45-second TTL five times over, so the duty lapses
// mid-sweep, a peer claims it, and both nodes sweep the surfaces the other has
// not reached. Nothing UNSAFE follows from that — the surface's own lease is
// what stops two writers at one third-party app — but the sweep stops being
// deterministic and `integration_duty_lost` becomes routine noise on a healthy
// fleet.
//
// The cost is on the other side and is worth naming: a node that dies holding
// the duty leaves it unclaimable for this long instead of 45 seconds. Against a
// settled cadence of ten minutes that delays a reconcile by less than half an
// interval, which is the cheaper of the two.
//
// # The NAME does not change with it, and a rolling upgrade is why
//
// A lease two builds share is a peer contract, so raising a TTL under an
// unchanged name looks like exactly the kind of change that needs a new key.
// It is the opposite here, in both directions.
//
// A claim WRITES ITS EXPIRY: [coord.Lease.ExpiresAt] is the store's own
// deadline, and every reader honours the record rather than recomputing from
// its own constant — a heartbeat takes its next tick from it. So a 45-second
// claim by an old node and a four-and-a-half-minute claim by a new one are two
// hold durations and never two opinions about who holds it. Neither build can
// see a lease the other holds as free.
//
// And a SECOND NAME is the failure this is accused of. Two names are two
// locks: the old build would claim the old one, the new build the new one,
// both would hold, and both would sweep every surface for the whole length of
// the upgrade — which is the overlap, arrived at deliberately, rather than
// the one the shared name is supposed to cause.
//
// What actually bounds the overlap is neither number. [integration.Worker]
// re-claims before every surface it visits, and a claim by the owner that
// already holds it doubles as a renew, so the TTL only ever has to cover ONE
// pass rather than a whole sweep. An old node's 45 seconds is short for that
// and its sweep can lapse mid-pass — which is the bug this constant fixes, and
// it is the old build's to fix by being replaced, not something a rename
// reaches.
const integrationDutyTTL = setup.PassDeadline + 2*integration.Interval

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
		// AND THE NAME A REGISTRATION IS HELD UNDER, for the one surface
		// where the address cannot find it again. Read fresh on every
		// pass for the same reason the endpoint is: it is a field of the
		// applied revision and an apply can change it — which is exactly
		// the change this exists to notice.
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
		// AND HOW LONG A CONVERGED SURFACE IS TRUSTED, which is the only
		// thing that ever finds access somebody revoked by hand at the
		// third-party app. A company's own field, read fresh on every pass
		// for the reason the endpoint is: it is edited live, and a value
		// captured here would make an operator who shortened the interval
		// wait out the one they had just replaced.
		SettledInterval: func() time.Duration {
			company := e.Company()
			if company == nil {
				return 0
			}
			return company.Config.Integrations.CheckInterval()
		},
		ClaimDuty: integration.DutyFunc(
			e.workerDuty(integrationDutyName, integrationDutyTTL)),
		// THE ONE GUARD EVERY WRITER AT A SURFACE TAKES, held by the worker
		// across the row re-read, the pass and the status write alike. See
		// [Engine.holdSurface], and the note in `passConverger.Reconcile` for
		// why the pass no longer takes it itself.
		Guard: integration.Guard(e.holdSurface),
		// AND THE POSTURE GATE. Every reconciler reads the live company
		// document, so a node the fleet has already moved past would converge
		// third-party apps to a revision that has been replaced. Asked before
		// the duty is claimed, so a shedding node's lease lapses and a peer
		// holding the current revision takes the loop over.
		Admits: integration.AdmitsFunc(e.admits),
		// WHAT THE DOCUMENT STILL DECLARES, for the teardown-only surfaces
		// that have no pass to answer it. See [integration.Options].Configured.
		Configured: func(kind integration.Kind) bool {
			company := e.Company()
			if company == nil {
				// No active revision is not "the document declares
				// nothing" — it is a node that cannot say, and forgetting
				// every row on that would delete the whole fleet's status
				// during a boot.
				return true
			}
			return company.Config.DeclaresIntegration(kind.String())
		},
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
	apiBase, webBase := cfg.Bases(env.LookupOK)
	return github.NewClient(github.ClientOptions{
		APIBase: apiBase, WebBase: webBase, Token: token,
	})
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
	// A CANCELLED PASS IS A FAULT, AND THIS CHECK COMES FIRST FOR A REASON.
	//
	// Every arm below can answer without touching the network, and two of them
	// answer in ways a dead context makes actively destructive: an empty
	// finding list is read by the loop as "this integration is ready" and
	// trusted for a full settled interval, and ErrNotConfigured makes it
	// FORGET the surface's status row. A node draining during shutdown would
	// walk its surfaces and delete the fleet's whole integration status on the
	// way out.
	//
	// Guarded here rather than in each of the seven [setup.Pass]
	// implementations because this is the one frame every one of them reaches
	// the LOOP through. It is not the only frame they are reached through:
	// [setup.Runner.Execute] serves the dashboard's own pass and carries the
	// same guard for the same reason. Each third-party app's own Reconcile
	// checks too, which is what its conformance harness drives.
	//
	// The passes in setuppass.go deliberately carry none. Every one of them
	// reaches a vendor call within a few statements, and a check at each would
	// be seven copies of a rule that has two honest homes: the frame the loop
	// uses, and the frame the dashboard uses.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	// it finds, and reporting is most of what this loop is for.
	//
	// [provision.ReadOnly] RATHER THAN NIL. This passed nil and called it a
	// dry run, which no pass implemented: two of them refused a nil sink at
	// their entry point with ErrNoSink, so every tick reported those
	// integrations as a FAULT — "the last pass could not read this",
	// retried for ever — on an ordinary deployment that keeps its ${VAR}s
	// in the environment and has no secrets.keys at all.
	sink, err := c.engine.SetupSink(reconcileOperator)
	if err != nil {
		log.WarnContext(ctx, "integration_sink_unavailable",
			"integration", c.pass.Kind().String(), "error", err,
			"detail", "this pass reads and reports; it will mint nothing")
		sink = provision.ReadOnly()
	}
	// NO GUARD IS TAKEN HERE, and that is the fix rather than an omission.
	//
	// It used to be taken at this line and released the moment this function
	// returned — which put the write that RECORDS the pass outside it. The
	// loop then folded the outcome into a row it had read before the pass
	// began, and an operator's disconnect landing in that window was silently
	// overwritten: the card went from Disconnecting back to connected and they
	// pressed the button again. coord.Integrations says that cannot happen
	// because every writer takes the lease first; every writer did, and then
	// gave it back too early.
	//
	// So [integration.Worker] takes it around the whole visit — the row
	// re-read, this pass, and the status write — through the Guard wired in
	// `startIntegrations`. Taking it again here would not merely be redundant:
	// [setup.Runner]'s in-process claim is NOT reentrant, so this would answer
	// not-held on every tick, for ever, deterministically. The coord lease IS
	// reentrant for the same owner, which is exactly what makes that mistake
	// easy to reason your way into.
	//
	// ctx is already the guard's: bounded by [setup.PassDeadline], strictly
	// inside the lease the worker is holding.
	findings, err := c.pass.Run(ctx, setup.PassInput{
		Sink:        sink,
		WebhookBase: company.Config.Integrations.WebhookBase(c.engine.resolver().LookupOK),
	})
	if err != nil {
		return nil, err
	}
	// AND WHAT THIS NODE'S OWN WIRING COULD NOT RESOLVE, which no vendor
	// pass can see: a seat's tracker or code-host ACCOUNT is read with that
	// seat's own credential by the engine, not by the pass, and a lookup
	// that failed had nothing that would ever ask again. See
	// [Engine.resolveRouting] — this visit is the retry, and the finding is
	// what keeps the visits coming and stops the card reading ready over a
	// seat that receives nothing.
	return append(findings,
		c.engine.resolveRouting(ctx, c.pass.Kind(), reported(findings))...), nil
}

// reported is the set of seats the surface's own pass has already said
// something about.
//
// ONE CAUSE, ONE FINDING. Both halves resolve the SAME seats with the SAME
// credential against the SAME instance — the pass through its own
// resolveSeats, this node's wiring through its registry — so a seat whose
// lookup fails produces two findings about one fact, and they do not even
// agree about who has to act: the tracker's classifies as degraded and owed
// by an ADMIN, the wiring's as provisioning and owed by the ENGINE. The
// engine's outranks the admin's, so the card put "the engine is working on
// it" in the headline and "a person must act at Atlassian" underneath it,
// about one seat, for one transient reason.
//
// THE PASS'S ANSWER WINS because it is the one with the vendor's own words in
// it. What the wiring adds is the seats the pass said NOTHING about — every
// seat on a surface whose pass reports no per-seat identity at all, which is
// both code hosts — and that is exactly what is kept.
func reported(findings []integration.Finding) map[string]bool {
	seen := make(map[string]bool, len(findings))
	for _, f := range findings {
		if f.Subject != "" {
			seen[f.Subject] = true
		}
	}
	return seen
}

// reconcileOperator is who the loop's writes are attributed to, so an audit
// row says a timer did this rather than naming a person who did not.
const reconcileOperator = "reconcile loop"
