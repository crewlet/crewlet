package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/integration"
)

// The epoch and how a new one replaces it.
//
// A CONFIG REVISION IS AN IMMUTABLE EPOCH, AND APPLYING ONE PUBLISHES A NEW
// EPOCH — nothing is ever mutated in place. Applying a revision by
// mutating the live objects keeps their identity, so that anything holding a
// reference keeps working. That is
// precisely the problem: anything holding a reference kept working, and kept
// reading, mid-turn, values from two different revisions. A turn that read the
// budget cap before the swap and the model chain after it ran under a company
// that never existed, and nothing raised.
//
// Here a turn PINS the epoch once at the top and reads only that for the rest
// of the turn. A revision published mid-turn is simply not observed by that
// turn — the guarantee, not a limitation. The next turn gets it.

// epoch holds the current company: the two halves it is composed from, and
// the composition readers load.
//
// THE COMPOSED POINTER IS AN ATOMIC AND NOTHING MORE, so readers never block.
// The mutex covers the two INPUTS and the build between them — see view.go for
// why the composition happens on the write side rather than per read.
type epoch struct {
	chartView
	current atomic.Pointer[Company]
}

// Company is the epoch this engine is running.
//
// Every caller that needs more than one fact from it must take it ONCE and
// read that value, not call this twice: two calls can straddle a publish, and
// a caller that read the org from one epoch and the models from the next is
// running a company that never existed. That is the whole hazard this design
// exists to remove, and it is removable only at the call site.
func (e *Engine) Company() *Company { return e.epoch.current.Load() }

// RecheckGitHub asks the reconcile loop to look at GitHub now rather than
// waiting out its cadence.
//
// FOR THE MOMENT A PERSON FINISHES SOMETHING THERE. Installing an agent's App
// is a click at GitHub that this engine cannot perform and cannot be told
// about — an agent's App is private, so it sends no webhook here — so the
// card asking for it is owed to an admin, and an admin-owed surface backs off
// from fifteen seconds to ten minutes ([integration.Schedule]). The instant
// somebody DOES it is therefore the instant the wait is longest and least
// deserved: measured at an install completed in about eight seconds, followed
// by minutes of a card still asking for it.
//
// What the redirect supplies is only the timing. Nothing about it is
// believed and no installation id travels with the ask — see
// [integration.Worker.Refresh] — so the pass that follows is the ordinary
// verified one.
func (e *Engine) RecheckGitHub() {
	e.integrations.Refresh(integration.KindGitHub)
}

// installEpoch publishes an epoch and tells the store anything derived from it
// that the store cannot work out for itself.
//
// ONE FUNCTION for the two places an epoch becomes current — boot and apply —
// because state set in one and forgotten in the other fails silently and only
// on the path nobody exercised.
//
// Today that is three things.
//
// THE PARTY REGISTRY, indexed BEFORE the epoch is stored, so no reader can
// find a company through [Engine.Company] whose parties are not in
// [Engine.Registry]. An apply indexes earlier still, before it rebuilds the
// vendor wiring that registers into the new registry, and that index is kept
// rather than rebuilt here. Boot has nothing to rebuild in between, and indexed
// only at the end of construction, after every fleet duty was already armed;
// see [Engine.Registry] for what that window did.
//
// And a store opened with NO width, which is a node that booted with no active
// revision. It holds no rows, and its first epoch is what tells it how wide
// its vectors will be. A store that already has a width keeps it
// ([store.DB.LearnEmbeddingDim] only ever raises from 0), because the width
// belongs to the rows in the file rather than to the current config, and
// [Engine.buildEmbedder] has already refused any revision that would change
// it.
//
// And the TOOL-SKILL TRIGGER AUDIT, which reads the epoch that is current
// and so runs once this one is; see [Engine.auditSkills] for why it takes
// no epoch.
//
// It also tells the OPERATOR one thing: that an epoch with no model is now
// current. See nomodels.go.
//
// It takes the settings epoch and the COMPOSITION the caller already derived
// for it ([epoch.withView]), and returns the company a reader now loads —
// which is that same value whenever this node's rows have not moved in
// between. Two arguments because the composition has to exist BEFORE the
// publish: the registry is indexed for the exact value readers will find, and
// deriving a second one here would give that value a different identity from
// the one every stage above wired against.
func (e *Engine) installEpoch(ctx context.Context, c, view *Company) *Company {
	if view != nil && !e.indexes(view) {
		e.refreshParties(ctx, view)
	}
	// COMPOSED rather than stored: the company a reader loads is this
	// settings epoch and this node's chart view together, and whichever
	// half moves republishes the pair. See view.go.
	published := e.epoch.setSettings(c, view)
	if c != nil && c.Models == nil {
		// Said here, once per epoch, because it is the only line that
		// reaches the operator of a company nobody has messaged yet: with
		// no traffic there is no held delivery to log about.
		log.Warn("company_has_no_models", "company", c.Config.Name,
			"seats", len(c.Seats()),
			"detail", "the company configures no model provider: its seats are "+
				"placed and keep what arrives on their inboxes, but none takes a "+
				"turn until a revision adds one under providers.llm")
	}
	// Backends are always present on a running engine; a `crewlet validate`
	// engine applies to nothing and has no store to tell.
	if e.backends != nil && e.backends.Store != nil {
		e.backends.Store.LearnEmbeddingDim(embeddingWidth(c))
	}
	e.auditSkills()
	return published
}

// indexes reports whether the live party registry was built from exactly this
// company. By identity, because an epoch is published rather than mutated: a
// revision equal in every field is still a different epoch, with a registry of
// its own.
func (e *Engine) indexes(c *Company) bool {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.registry != nil && e.notify.registryFor == c
}

// embeddingWidth is the vector width an epoch's embeddings provider produces,
// or 0 for a company that configures none.
//
// Zero is "no declared width to check against", not a width of zero and not an
// unknown: the store applies no dimension check at all against it. See
// [store.DB.LearnEmbeddingDim] for why a store that has one keeps it.
func embeddingWidth(c *Company) int {
	if c == nil || c.Config == nil || c.Config.Providers.Embeddings == nil {
		return 0
	}
	return c.Config.Providers.Embeddings.Width()
}

// Apply publishes a new epoch, reporting what happened to this node.
//
// The build comes first and touches nothing: [NewCompany] validates, resolves
// the org and constructs the providers without reaching the network. So a
// revision that cannot be built is refused with the previous epoch still
// current and still correct — there is no rollback path because there was no
// mutation, which is the point of publishing rather than mutating.
//
// The three outcomes belong to the control plane, not to this function's
// convenience:
//
//   - ok      — published, and this node is serving it.
//   - error   — refused; this node still serves the PRIOR epoch correctly,
//     which is a legitimate degraded-but-correct state and safe to route to.
//   - degraded — the apply failed AFTER a restart-required subsystem was
//     mutated, so rollback could not restore it.
//
// DEGRADED IS NOT REACHABLE YET, and saying so is more useful than a hook with
// nothing behind it. It becomes reachable when the first subsystem that cannot
// be un-applied is wired: the per-role MCP children (Phase 6) and the
// notification transports (Phase 7). Both are applied LAST when they arrive,
// for exactly this reason — every step before them rolls back by publishing
// the previous epoch, so the window in which degraded is reachable is as small
// as the ordering can make it.
// ONE CALLER: the reconciler's tick, which is synchronous. The API's write
// path does NOT reach here: it activates a revision and lets the tick apply
// it, because activation also has to move the pointer, record the outcome and
// reset the attempt budget, none of which this function does. So there is no
// second apply to exclude, and the lock taken below is not for one. It is for
// [Engine.Drain], the one other writer of what an apply builds: an apply that
// overlapped the drain and the teardown after it restarted the scheduler, the
// background passes and a first company's inbound edge after they had ended
// them. The drain waits for an apply in flight, and an apply that starts after
// it is refused with [errStopped].
//
// The second return is the subsystems this apply GOT THROUGH, in the order it
// went through them. On a failure it is what was already mutated when the
// refusal happened, which is the whole of what makes a degraded apply
// diagnosable after the fact — it travels on ConfigRevisionApplied into the
// audit event log, where it outlives the fleet view's one-minute bucket.
//
// NO ACTIVATION INSTANT IS TAKEN. What must agree across nodes about a
// company's org chart — the tracker's projects, the knowledge containers — is
// stamped with the position on the chart's own log the published view was
// composed at ([Company.ChartAt]), which orders the chart without a clock; see
// [Engine.applyChart].
func (e *Engine) Apply(ctx context.Context, cfg *config.Company) (configplane.ApplyStatus, []string, error) {
	e.applying.Lock()
	defer e.applying.Unlock()
	if e.stopped {
		return configplane.StatusError, nil, errStopped
	}
	// THE RULES THAT NEED BOTH TIERS, before anything is touched — the same
	// check [New] makes of the company a node boots with. A boot was the
	// only place it ran, so a node that booted unconfigured accepted a first
	// company whose state log — which every company runs, for its org chart
	// if for nothing else — lives on an in-memory stream.
	if err := config.CheckTiers(e.boot, cfg); err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision was refused before anything changed; "+
				"this node still serves the previous epoch")
		return configplane.StatusError, nil, fmt.Errorf("engine: apply: %w", err)
	}
	var applied []string
	// THE SNAPSHOT FIRST, because re-activating an unchanged revision is
	// the documented rotation gesture: the payload has not moved, so the
	// only thing that can have is what its ${VAR} references resolve to.
	// Rebuilding the epoch without re-reading the store would make that
	// gesture a no-op and rotation impossible without a restart.
	e.refreshSecrets(ctx)
	applied = append(applied, "secrets")
	next, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision was refused before anything changed; "+
				"this node still serves the previous epoch")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "company")
	// THE REVISION'S SANDBOX MANAGER, built HERE — beside the company it is
	// a part of, before anything below mutates this node — because a
	// catalogue that cannot be built is a revision that cannot be served:
	// its code-enabled seats would plan around boxes nobody can mint. It was
	// built only where a coordinator already existed, so on every other
	// node a broken block was published rather than refused. Nil is a
	// company that reaches no sandbox cell, which is not a failure.
	sandboxManager, err := buildSandbox(next.Config, e.resolver(), e.sandboxOtel)
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision's providers.sandbox could not be built; "+
				"this node still serves the previous epoch")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	// THE NATIVE HALVES, on a node whose FIRST company this is — before the
	// tools, which are registered only where they exist, and before the
	// inbound edge, whose parsers include their own. The state log, the org
	// chart and the identity estate they ride are the CORE's, running since
	// boot on every node, so this starts only what depends on the company.
	// See [Engine.startNativeFor], and the bug it fixes.
	startedNative, err := e.startNativeFor(ctx, next)
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the native tracker and knowledge base could not be "+
				"started for this node's first company; the previous epoch is "+
				"still current")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	if startedNative {
		applied = append(applied, "native")
		// THE CHART THIS NODE'S BOOT WOULD HAVE SEEDED, now that there is
		// a company file to seed it from: the document's own chart, where
		// the chart is empty — in the boot's order and for its reasons
		// ([New]), because this IS that boot, one company later. A no-op
		// where there is nothing to publish (a stored revision carries no
		// chart, and a chart an offline import staged was redeemed at
		// boot, which the seed then finds), and it does not refuse the
		// apply: a seed that did not land is warned about exactly as at
		// boot.
		e.seedChartAtBoot(ctx, next.Config)
		// AND THE CHART VIEW OVER WHAT IT JUST WROTE, so the composition
		// below wires every stage against this node's own chart rather
		// than one read before the seed. Its derivation only: what follows
		// a published company runs once this one is, below. A failure is
		// not a refusal, for the boot's own reason — the view's triggers
		// are running and retry it.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if _, err := e.rebuildChart(ctx); err != nil {
			log.WarnContext(ctx, "chart_view_unbuilt_at_apply", "error", err,
				"detail", "this company is composed without this node's chart "+
					"rows until its chart view builds; the view's own triggers "+
					"retry it")
		}
	}
	// COMPOSED WITH THIS NODE'S CHART BEFORE ANY OTHER STAGE RUNS. The
	// revision carries the SETTINGS and the org chart is a log of its own,
	// so the company this apply just built has no seats in it at all — and
	// every stage below wires against a roster. See [epoch.withView].
	view := e.epoch.withView(next)
	// THE SANDBOX RUNTIME, on a node that has never run one and whose
	// revision reaches a sandbox cell — before the tools for the reason the
	// native halves are: run_sandbox and an agent-mode executor are offered
	// only where it exists. See [Engine.startSandbox], and the bug it fixes.
	startedSandbox, err := e.startSandbox(ctx, sandboxManager)
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the code sandbox could not be started for this revision; "+
				"the previous epoch is still current")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	if startedSandbox {
		applied = append(applied, "sandbox_runtime")
	}
	// Equipped before it is published, for the same reason as at boot: a
	// turn can start the instant the pointer moves, and a revision that
	// silently dropped every builtin would look like a model that stopped
	// using its tools.
	if err := e.equip(ctx, view); err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision built but could not be equipped with this "+
				"node's tools; the previous epoch is still current")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "tools")
	// The learning workers are rebuilt for the new epoch — they hold its
	// org and its model registry — while the dispatcher, its subscription
	// and its redelivery ring stay put. A failure leaves the previous
	// epoch's workers serving rather than failing the apply: reflecting
	// against a stale org is a far smaller wrong than not reflecting. The
	// one refusal is a node's FIRST company, whose dispatcher is attached
	// here and would otherwise not exist at all.
	if err := e.reconfigureReflection(ctx, view); err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the reflect dispatcher could not be attached for this "+
				"node's first company; the revision is not served here yet")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "learning")
	// The sandbox MANAGER is swapped, and only the manager: the coordinator
	// and the waiter hold this process's busy set and poll loop, so
	// rebuilding them would forget which seats are mid-run and start a
	// second loop against the same rows. The swap carries the backends of a
	// cell the revision dropped for the runs still on it, and a revision
	// with no catalogue changes nothing — see [sandbox.Coordinator.SetManager].
	//
	// HERE rather than beside the build, and it cannot fail: every stage
	// that can refuse a node already serving a company has run, so a
	// manager swapped in is one whose epoch is about to be published, and a
	// refusal never leaves a node launching through a catalogue its
	// current epoch does not have.
	if rt := e.sandbox.Load(); rt != nil {
		rt.coordinator.SetManager(sandboxManager)
		applied = append(applied, "sandbox")
	}

	// The party index is rebuilt BEFORE the epoch is published, and the
	// order is a choice between two brief windows. Refreshing first means
	// a seat the revision REMOVED stays addressable for an instant, which
	// costs a recorded skip. Refreshing after means a seat the revision
	// ADDED is unresolvable while the epoch that has it is already
	// current — and during a rollout the new company is the one being
	// adopted, so the window that favours it is the right one.
	//
	// IT IS NOT NAMED IN `applied` HERE, because the convergence below
	// names it once for both paths — and it is the convergence's guarantee
	// that the registry answers for the published company, not this
	// call's. What this call buys is only which of the two windows the
	// apply spends, and an apply refused between here and the publish has
	// indexed a company nobody can reach, which costs a rebuild and
	// nothing else.
	e.refreshParties(ctx, view)
	if e.inboundStarted() {
		// The TRACKER is rebuilt on the same edge and for the same
		// reason: its lead map is derived from the org, so a node that
		// kept its boot-time parser would route the new revision's work
		// items by the old company's org chart.
		e.reconcileConfluence(view)
		e.reconcileDatadog(ctx, view)
		e.reconcileJira(ctx, view)
		e.reconcileGitLab(ctx, view)
		e.reconcileGitHub(ctx, view)
		// AND THE TWO CHAT SURFACES, which had no reconciler at all:
		// their parsers were assembled once at boot, so a company that
		// connected either one after starting had every delivery
		// verified at the edge and routed to nobody until the process
		// was restarted. See [Engine.reconcileSlack] for why one rebuilds
		// unconditionally and the other does not.
		e.reconcileSlack(ctx, view)
		e.reconcileMattermost(ctx, view)
	} else if err := e.startInbound(ctx, view); err != nil {
		// A NODE THAT BOOTED WITH NO COMPANY has no inbound edge for
		// the reconcilers above to rebuild, and each of them returns
		// early without one. So its first company STARTS the edge, and
		// a start that fails is refused like a build: a company served
		// with no inbound edge looks healthy and hears nothing, and the
		// retry the refusal earns starts it again. See
		// [Engine.startInbound].
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the inbound edge could not be started for this node's "+
				"first company; the revision is not served here yet")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	// AND WHAT THE LOOP LAST CONCLUDED IS NOW OLD NEWS. Its cadence is for
	// asking a third-party app again, not for asking this document again,
	// and the answer just changed here. See [integration.Worker.MarkStale].
	e.integrations.MarkStale()
	// The NATIVE backends' parsers are on the same edge and rebuilt for
	// the same reason. Their appliers, index, stores and feeds are NOT:
	// those follow a log, which a company revision does not change — see
	// [Engine.reconcileNative].
	e.reconcileNative(ctx, view)
	// AND THE TOOL SKILLS' SOURCE, after the knowledge base's own reconcile
	// above, because the Confluence source is read off the wiring it left
	// running. See [Engine.reconcileSkills].
	e.reconcileSkills(view)
	applied = append(applied, "integrations")

	previous := e.Company()
	published := e.installEpoch(ctx, next, view)
	applied = append(applied, "epoch")

	// THE SWEEP, rebuilt for a node's FIRST company — after the epoch is
	// current, because it reads the conversation and inbox horizons off
	// it, and only then: its job list is the one thing built once at boot
	// that a first company changes. See [Engine.rebuildMaintenance].
	if previous == nil || startedNative {
		e.rebuildMaintenance(ctx)
		applied = append(applied, "maintenance")
	}

	// THE BACKGROUND PASSES follow the revision too, and after the swap:
	// their loops walk the CURRENT epoch's roster, and the passes handed to
	// them hold this revision's models and knobs. The loops keep running
	// and keep their clocks. See [Engine.reconfigureLearningPasses].
	//
	// IT IS NOT PART OF THE CONVERGENCE BELOW because nothing it builds is
	// derived from the org chart: a pass is the learning block's knobs and
	// this company's models, and the loops read the roster per tick off
	// whatever company is current. A chart write changes neither.
	e.reconfigureLearningPasses(ctx, published)
	applied = append(applied, "learning_passes")

	// AND EVERYTHING DERIVED FROM THE COMPANY ITSELF — the parties, the
	// seat tool surfaces, the tracker's projects, the knowledge
	// containers, the mailboxes, the scheduler and the open sockets. All
	// of it AFTER the pointer moves, each for its own reason and all for
	// one: an apply refused later must not have rebuilt the derived state
	// of an epoch that never became current.
	//
	// THE SAME LIST A CHART WRITE RUNS, through the same function, because
	// a chart write publishes a company too. See [Engine.convergeOn].
	applied = append(applied, e.convergeOn(ctx, published)...)

	log.InfoContext(ctx, "config_applied",
		"company", published.Config.Name, "seats", len(published.Seats()),
		"previous_seats", seatCount(previous))
	return configplane.StatusOK, applied, nil
}

// errStopped refuses an apply that reaches a node after [Engine.Drain] began.
// The node is leaving, and nothing it would build for the revision could run.
var errStopped = errors.New("engine: apply: this node is stopping and applies no revision")

// seatCount reports a possibly-absent epoch's seat count, for the log line
// that says what changed. The first apply on a node has no previous epoch.
func seatCount(c *Company) int {
	if c == nil {
		return 0
	}
	return len(c.Seats())
}
