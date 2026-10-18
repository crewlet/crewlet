package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
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

// epoch holds the current company.
//
// An atomic pointer and nothing else: readers never block, and there is no
// lock because there is only ever one writer — see [Engine.Apply].
type epoch struct {
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
// Today that is four things.
//
// THE PARTY REGISTRY, indexed BEFORE the epoch is stored, so no reader can
// find a company through [Engine.Company] whose parties are not in
// [Engine.Registry]. An apply indexes earlier still, before it rebuilds the
// vendor wiring that registers into the new registry, and that index is kept
// rather than rebuilt here. Boot has nothing to rebuild in between, and indexed
// only at the end of construction, after every fleet duty was already armed;
// see [Engine.Registry] for what that window did.
//
// And a store opened with NO width, which is a node that booted on no
// revision: one whose store activated none, which holds no rows, or one
// started with `-import-company`, which reads none and may hold the stored
// revision's rows (see [OpenBackends] for why it learns the imported
// file's). Its first epoch is what tells it how wide its vectors will be. A
// store that already has a width keeps it
// ([store.DB.LearnEmbeddingDim] only ever raises from 0), because the width
// belongs to the rows in the file rather than to the current config, and
// [Engine.buildEmbedder] has already refused any revision that would change
// it.
//
// And THE SHARED MCP SERVERS' CATALOGUE AS IT STANDS NOW, refiled into the
// epoch's registry once the epoch is current. The registry was filled while
// the epoch was being built, and a shared server the bridge brought back in
// between — a retried start, a restart with different tools — told a refile
// that could only see the epoch being replaced; see
// [Engine.sharedCatalogueChanged]. Before the audit below, which reads it.
//
// And the TOOL-SKILL TRIGGER AUDIT, which reads the epoch that is current
// and so runs once this one is; see [Engine.auditSkills] for why it takes
// no epoch.
//
// It also tells the OPERATOR one thing: that an epoch with no model is now
// current. See nomodels.go.
//
// IT DOES NOT STAMP THE ACTIVATION: the company arrives here stamped, by
// [stampActivation] where it was built, because the native halves an apply
// starts and the reconcile it runs read the company before it is installed.
func (e *Engine) installEpoch(ctx context.Context, c *Company) {
	if c != nil && !e.indexes(c) {
		e.refreshParties(ctx, c)
	}
	e.epoch.current.Store(c)
	if c != nil {
		e.mcpFileMu.Lock()
		e.fileSharedTools(ctx, c, false)
		e.mcpFileMu.Unlock()
	}
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
}

// stampActivation records on c the instant the fleet activated the revision it
// was built from — UTC, and zero for one no activation has named.
//
// ONCE, AS THE COMPANY IS BUILT, on both paths that build one — [New] and
// [Engine.Apply] — and never again: a [Company] is published rather than
// mutated, so a stamp written after construction would be a write to an epoch
// a reader may already hold, and a stamp written at install would be missing
// from everything that reads the company between the build and the publish.
// See [Company.ActivatedAt] for what reads it.
func stampActivation(c *Company, activatedAt time.Time) {
	if c == nil {
		return
	}
	if !activatedAt.IsZero() {
		activatedAt = activatedAt.UTC()
	}
	c.ActivatedAt = activatedAt
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
// The secret snapshot is re-read first, and it is the one thing an apply
// changes before it can refuse: the store's newest values, which every live
// ${VAR} lookup on the serving epoch reads too, so a refused revision leaves
// the node resolving rotated credentials rather than broken ones. Then the
// build, which touches nothing: [NewCompany] validates, resolves the org and
// constructs the providers without reaching the network. So a revision that
// cannot be built is refused with the previous epoch still current and still
// correct — there is no rollback path because nothing it built was published,
// which is the point of publishing rather than mutating. A caller that must
// refuse a revision before even the snapshot moves validates it first, as the
// reconciler does ([config.Company.ValidateRunnable]).
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
// activatedAt is the instant the fleet activated cfg — the activation
// pointer's own ([coord.Activation.At]) — and it is what every clause that
// must agree across nodes about WHEN this configuration took effect reads: the
// chart apply stamps each project and each knowledge container with it, so an
// older configuration arriving late on another node cannot walk a newer one
// back. Zero is a configuration no activation named, whose chart is not
// applied; see [Engine.applyChart].
func (e *Engine) Apply(ctx context.Context, cfg *config.Company,
	activatedAt time.Time) (configplane.ApplyStatus, []string, error) {
	e.applying.Lock()
	defer e.applying.Unlock()
	if e.stopped {
		return configplane.StatusError, nil, errStopped
	}
	var applied []string
	// THE SNAPSHOT FIRST, because re-activating an unchanged revision is
	// the documented rotation gesture: the payload has not moved, so the
	// only thing that can have is what its ${VAR} references resolve to.
	// Rebuilding the epoch without re-reading the store would make that
	// gesture a no-op and rotation impossible without a restart.
	//
	// A FAILED READ BUILDS ON THE PREVIOUS SNAPSHOT, and never on none: an
	// engine holding a store that has never read it would resolve every
	// ${VAR} from the environment alone and record that epoch as applied.
	// [New] does not return without a snapshot ([Engine.bootSecrets]), so
	// this refuses only an engine built some other way, and refuses it
	// before anything changed.
	if !e.refreshSecrets(ctx) && e.holdsSecretStore() && e.env.Load() == nil {
		err := errors.New("this node has never read its secret store, so the " +
			"revision's ${VAR} references would resolve from the environment alone")
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision was refused before anything changed")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "secrets")
	// THE BUILD, which changes nothing: the company and the sandbox catalogue
	// it configures. See [buildRevision], which is also what a write that
	// must not land a revision this node cannot build asks ([Buildable]).
	next, sandboxManager, built, err := buildRevision(cfg, e.resolver(), e.sandboxOtel, e.fakePlay)
	applied = append(applied, built...)
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision could not be built and was refused before "+
				"anything changed; this node still serves the previous epoch")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	stampActivation(next, activatedAt)
	// THE SPENT PLANS STAY SPENT: every entry the revision left as it was
	// keeps the sit-out the entry it replaces was serving — see
	// [carrySitOuts].
	carrySitOuts(e.Company(), next)
	// THE NATIVE HALVES, on a node whose FIRST company this is — before the
	// tools, which are registered only where they exist, and before the
	// inbound edge, whose parsers include their own. The state log and the
	// identity estate they ride are the CORE's, running since boot on every
	// data node, so this starts only what depends on the company — and on a
	// node without `data`, only which halves it reaches through a data node.
	// See [Engine.startNativeFor], and the bug it fixes.
	startedNative, err := e.startNativeFor(ctx, next)
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "Worklet, Doclet and Chatlet could not be started for "+
				"this node's first company; the previous epoch is still current")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	if startedNative {
		applied = append(applied, "native")
	}
	// THE SANDBOX RUNTIME, on a node that has never run one and whose
	// revision reaches a sandbox cell — before the tools for the reason the
	// native halves are: every executor launches into it, so a seat's turn
	// has nowhere to run until it exists. See [Engine.startSandbox], and the bug it fixes.
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
	if err := e.equip(ctx, next); err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision built but could not be equipped with this "+
				"node's tools; the previous epoch is still current")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "tools")
	// The learning workers are rebuilt for the new epoch — they hold its
	// org and its model registry — while the dispatcher, its subscription
	// and its redelivery ring stay put. BUILT here and SWAPPED IN below,
	// with the bundles: the dispatcher's live workers serve the running
	// seats' reflections the moment they are swapped, so a swap here ran
	// the revision's models before its bundles were restored, and ahead of
	// stages that can still refuse it. A failed swap leaves the previous
	// epoch's workers serving rather than failing the apply: reflecting
	// against a stale org is a far smaller wrong than not reflecting. The
	// one refusal is a node's FIRST company, whose dispatcher is attached
	// here and would otherwise not exist at all. See
	// [Engine.prepareReflection].
	//
	// NAMED here, where it is built, and not at the swap: the trail says how
	// far an apply got before a refusal, and the swap below cannot fail, so
	// it is part of publishing rather than a stage a refusal can stop short
	// of.
	installReflection, attachErr := e.prepareReflection(ctx, next)
	if attachErr != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", attachErr,
			"detail", "the reflect dispatcher could not be attached for this "+
				"node's first company; the revision is not served here yet")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", attachErr)
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
	// IT IS NOT NAMED IN `applied` HERE, because [Engine.followCompany]
	// names it once for both paths — and it is that function's guarantee
	// that the registry answers for the published company, not this
	// call's. What this call buys is only which of the two windows the
	// apply spends, and an apply refused between here and the publish has
	// indexed a company nobody can reach, which costs a rebuild and
	// nothing else.
	e.refreshParties(ctx, next)
	if e.inboundStarted() {
		// The TRACKER is rebuilt on the same edge and for the same
		// reason: its lead map is derived from the org, so a node that
		// kept its boot-time parser would route the new revision's work
		// items by the old company's org chart.
		e.reconcileConfluence(next)
		e.reconcileDatadog(ctx, next)
		e.reconcileJira(ctx, next)
		e.reconcileGitLab(ctx, next)
		e.reconcileGitHub(ctx, next)
		// AND THE TWO CHAT SURFACES, which had no reconciler at all:
		// their parsers were assembled once at boot, so a company that
		// connected either one after starting had every delivery
		// verified at the edge and routed to nobody until the process
		// was restarted. See [Engine.reconcileSlack] for why one rebuilds
		// unconditionally and the other does not.
		e.reconcileSlack(ctx, next)
		e.reconcileMattermost(ctx, next)
		// AND THE ENGINE'S OWN CHAT'S INDICATOR, whose mode is founder
		// policy edited live and whose surface a revision can switch
		// on or off. See [Engine.applyChatStatus].
		e.applyChatStatus(ctx, next)
	} else if err := e.startInbound(ctx, next); err != nil {
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
	e.reconcileNative(ctx, next)
	// AND THE TOOL SKILLS' SOURCE, after the knowledge base's own reconcile
	// above, because the Confluence source is read off the wiring it left
	// running. See [Engine.reconcileSkills].
	e.reconcileSkills(next)
	applied = append(applied, "integrations")

	// THE CONFIGURED CREDENTIAL BUNDLES, written HERE and nowhere earlier:
	// every stage that can refuse the revision has run, so only a revision
	// about to be published writes its bundle. A workspace is one per state
	// directory and shared with the running epoch's seats, so a bundle a
	// refused revision wrote would sign those seats in as an account no
	// published revision names, and outlive every retry: the next revision's
	// restore finds a login present and keeps it. See [Company.restoreBundles].
	//
	// AND BEFORE ANYTHING RUNS THE REVISION'S MODELS: its reflection workers
	// go live only after it (built above, swapped in here), and its seats'
	// turns only once the epoch is current — so the revision's first call,
	// a turn's or a reflection pass's, finds the login.
	next.restoreBundles(ctx)
	installReflection()
	previous := e.Company()
	e.installEpoch(ctx, next)
	applied = append(applied, "epoch")

	// THE SWEEP, rebuilt when this apply started the native runtime — its
	// job list is the one thing built once at boot that the runtime's
	// arrival changes. NOT for the horizons: every job asks the current
	// epoch for its horizon at every sweep, so a revision that moves one
	// needs no rebuild. See [Engine.rebuildMaintenance].
	if startedNative {
		e.rebuildMaintenance(ctx)
		applied = append(applied, "maintenance")
	}

	// THE BACKGROUND PASSES follow the revision too, and after the swap:
	// their loops walk the CURRENT epoch's roster, and the passes handed to
	// them hold this revision's models and knobs. The loops keep running
	// and keep their clocks. See [Engine.reconfigureLearningPasses].
	//
	e.reconfigureLearningPasses(ctx, next)
	applied = append(applied, "learning_passes")

	// AND EVERYTHING DERIVED FROM THE COMPANY ITSELF — the parties, the
	// seat tool surfaces, the tracker's projects, the knowledge
	// containers, the mailboxes, the scheduler and the open sockets. All
	// of it AFTER the pointer moves, each for its own reason and all for
	// one: an apply refused later must not have rebuilt the derived state
	// of an epoch that never became current.
	//
	// THE SAME LIST A BOOT RUNS, through the same function. See
	// [Engine.followCompany].
	applied = append(applied, e.followCompany(ctx, next)...)

	log.InfoContext(ctx, "config_applied",
		"company", next.Config.Name, "seats", len(next.Seats()),
		"previous_seats", seatCount(previous))
	// AND PLACEMENT, AT ONCE, when this revision changed which seats exist
	// or where they may run: the sweep's next tick is the first thing that
	// would otherwise notice, up to its interval later with every seat the
	// revision added — a node's whole first company among them — run by
	// nobody, and every one it removed still held. HERE, at the end, and
	// not beside the swap: a seat's acquisition reads what the stages after
	// the swap install — the refiled seat tools, its mailbox, the released
	// model holds, the sandbox runtime — which [Engine.followCompany] has
	// just run, so a pass asked for at the swap would claim seats onto a node
	// still half applied. See [seat.Host.Resweep] for what bounds the pass.
	if e.node != nil && seatsMoved(previous, next) {
		e.node.Host().Resweep()
	}
	return configplane.StatusOK, applied, nil
}

// buildRevision builds what an apply builds from a revision BEFORE it changes
// anything on this node: the company, over env, and the sandbox catalogue it
// configures — nil when it reaches no sandbox cell, which is not a failure.
//
// THE CATALOGUE BESIDE THE COMPANY it is a part of, because one that cannot be
// built is a revision that cannot be served: every seat's executor would
// launch into boxes nobody can mint. It was built only where a coordinator already
// existed, so on every other node a broken block was published rather than
// refused.
//
// ONE LIST for [Engine.Apply] and [Engine.Buildable]: a stage added to an
// apply's build and not to the check is a revision the check passes and every
// apply refuses. The third return is the stages it got through as the apply's
// trail names them: `company` once the company is built, so a catalogue
// refused after it is reported past the company, as it always was.
//
// What it leaves behind is what an apply's build leaves: a coding CLI's
// working directory is claimed in this process for the CLI an entry names
// (the cli-agent backend's workspace registry), so a later revision driving
// another CLI from the same directory is refused until a restart — as it is
// after an apply that built and was refused.
func buildRevision(cfg *config.Company, env *config.Resolver,
	otel *sandbox.OtelReceiver, play fakePlayer) (*Company, *sandbox.Manager, []string, error) {

	next, err := NewCompanyWith(cfg, env)
	if err != nil {
		return nil, nil, nil, err
	}
	built := []string{"company"}
	manager, err := buildSandbox(next.Config, env, otel, play)
	if err != nil {
		return nil, nil, built, err
	}
	// THE EPOCH'S OWN CATALOGUE, pinned on it — see [Company.catalogue].
	next.catalogue = manager
	return next, manager, built, nil
}

// carrySitOuts hands every coding-CLI entry of next whose spec is unchanged
// from previous the sit-out its predecessor is serving
// ([cliagent.Provider.CarrySitOut]).
//
// AN APPLY REBUILDS EVERY ENTRY, and a rebuilt provider sits nothing out — so
// the first apply after an entry's plan was spent, an edit to an unrelated
// seat, handed every seat on the node the spent entry back, and each of their
// next calls and launches started a process to learn the window again. Only an
// UNCHANGED spec is the same login and the same plan: an entry whose spec moved
// — another login, another model, another CLI — is a new question and starts
// clean, as one that is new to the revision does.
func carrySitOuts(previous, next *Company) {
	if previous == nil || next == nil || previous.Models == nil || next.Models == nil ||
		previous.Config == nil || next.Config == nil {
		return
	}
	for key, spec := range next.Config.Providers.LLM {
		was, ok := previous.Config.Providers.LLM[key]
		if !ok || !reflect.DeepEqual(was, spec) {
			continue
		}
		now, okNow := next.Models.Provider(key)
		then, okThen := previous.Models.Provider(key)
		if !okNow || !okThen {
			continue
		}
		rebuilt, isCLI := now.(*cliagent.Provider)
		served, wasCLI := then.(*cliagent.Provider)
		if isCLI && wasCLI {
			rebuilt.CarrySitOut(served)
		}
	}
}

// Buildable reports whether cfg builds on this node — the half of an apply
// that changes nothing ([buildRevision]), over this node's own ${VAR} chain —
// and discards what it built.
//
// It is for a write that must not land a revision this node cannot run.
// `crewlet run`'s import of its Tier B file activates that file for the whole
// fleet before any apply builds it — the engine never boots on a file — so
// without this a company the node could not build was stored and activated
// first, an override over a company the fleet was running included, and then
// refused by every apply: the node served no company, and its next boot was
// refused on the revision it had just written.
//
// The resolver is the snapshot this node read at boot ([Engine.bootSecrets]),
// which an apply moments later re-reads: a value rotated in between is the
// only difference, and it is the rotation gesture rather than a revision.
func (e *Engine) Buildable(cfg *config.Company) error {
	_, _, _, err := buildRevision(cfg, e.resolver(), nil, e.fakePlay)
	return err
}

// seatsMoved reports whether two epochs differ in what placement reads of
// them: which seats exist, or where any of them may run. Either epoch may be
// absent — a node's first company moves every seat it has.
//
// BY HANDLE, never by position: [Company.Seats] walks the org, and the same
// seats in another order are not a change anybody has to claim.
func seatsMoved(previous, next *Company) bool {
	was, now := previous.Seats(), next.Seats()
	if len(was) != len(now) {
		return true
	}
	where := make(map[string]placement.SeatPlacement, len(was))
	for _, s := range was {
		where[s.Handle] = s.Placement
	}
	for _, s := range now {
		old, ok := where[s.Handle]
		if !ok || old.Node != s.Placement.Node || !maps.Equal(old.Labels, s.Placement.Labels) {
			return true
		}
	}
	return false
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
