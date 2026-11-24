package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// The epoch and how a new one replaces it.
//
// A config revision is an immutable epoch, and applying one publishes a new
// epoch: nothing is mutated in place. Mutating the live objects lets a
// mid-turn reader see values from two revisions — a budget cap from one and a
// model chain from the next, a company that never existed — with nothing
// raised. A turn PINS the epoch once at its top and reads only that; a
// revision published mid-turn reaches the next turn.

// epoch holds the current company: an atomic pointer, so readers never block,
// and no lock because there is only one writer ([Engine.Apply]).
type epoch struct {
	current atomic.Pointer[Company]
}

// Company is the epoch this engine is running.
//
// A caller needing more than one fact from it takes it ONCE and reads that
// value: two calls can straddle a publish, and facts from two epochs make a
// company that never existed — a hazard removable only at the call site.
func (e *Engine) Company() *Company { return e.epoch.current.Load() }

// RecheckGitHub asks the reconcile loop to look at GitHub now rather than
// waiting out its cadence, for the moment a person finishes something there.
//
// Installing an agent's App is a click at GitHub that sends no webhook here
// (the App is private), and the admin-owed card asking for it backs off to ten
// minutes ([integration.Schedule]), so the wait is longest just after the
// install. The redirect supplies only the timing: nothing about it is believed
// and no installation id travels with it ([integration.Worker.Refresh]), so
// the pass that follows is the ordinary verified one.
func (e *Engine) RecheckGitHub() {
	e.integrations.Refresh(integration.KindGitHub)
}

// installEpoch publishes an epoch and tells the store what derives from it:
// ONE function for boot and apply, as state set on one path and forgotten on
// the other fails silently.
//
// Before the publish: the party registry, so no reader finds a company
// [Engine.Registry] cannot resolve (an apply's earlier index is kept), and
// what Gitlet reads off the current epoch alone, carried from the one being
// replaced ([Engine.codeInForce], [Engine.codeRoster]). After: a store learns
// a vector width only if it has none ([store.DB.LearnEmbeddingDim]); the
// shared MCP catalogue is refiled, as mid-build changes were filed on the old
// epoch ([Engine.sharedCatalogueChanged]), before the skill audit reads it.
// The activation is stamped at build ([stampActivation]).
func (e *Engine) installEpoch(ctx context.Context, c *Company) {
	if c != nil {
		// Gitlet's limits and retention, carried over the replaced epoch
		// where this one hosts no code of its own.
		c.codeInForce = carriedCode(c, e.epoch.current.Load())
		// Its chart as Gitlet judges a seat by, built at the first agent
		// read that asks rather than at every apply.
		c.roster = rosterOf(c)
	}
	if c != nil && !e.indexes(c) {
		e.refreshParties(ctx, c)
	}
	e.epoch.current.Store(c)
	e.applyIssueReports(ctx, c)
	if c != nil {
		e.mcpFileMu.Lock()
		e.fileSharedTools(ctx, c, false)
		e.mcpFileMu.Unlock()
	}
	if c != nil && c.Models == nil {
		// Once per epoch, as the only line reaching the operator of a company
		// nobody has messaged: with no traffic, no held delivery logs it.
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
	// A revision can declare or drop the catalogue, or change its image or
	// runtime: probe again rather than publish the last revision's answer.
	e.kickRunnerProbe()
	// Each login likewise: an entry may have started or stopped using one.
	e.kickHostLogins()
}

// stampActivation records on c the instant the fleet activated the revision it
// was built from: UTC, and zero for one no activation has named.
//
// Once, at build, on both paths ([New], [Engine.Apply]): later it would write
// to an epoch a reader may already hold, and at install it would be missing
// for what reads the company between build and publish ([Company.ActivatedAt]).
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
// or 0 for a company that configures none: "no declared width", against which
// the store applies no dimension check ([store.DB.LearnEmbeddingDim]).
func embeddingWidth(c *Company) int {
	if c == nil || c.Config == nil || c.Config.Providers.Embeddings == nil {
		return 0
	}
	return c.Config.Providers.Embeddings.Width()
}

// Apply publishes a new epoch, returning this node's outcome and the stages it
// got through in order: the trail ConfigRevisionApplied carries to the audit
// log, on a refusal what had already changed. Only the secret snapshot moves
// before a refusal is possible, so a refused node resolves rotated credentials
// ([config.Company.ValidateRunnable] refuses earlier); the build changes
// nothing, so error leaves the prior epoch current, correct and safe to route
// to. [configplane.StatusDegraded] is for a stage that cannot be un-applied:
// none exists yet, and one must run last. The reconciler's tick is the one
// caller (the API only activates); the lock excludes [Engine.Drain], after
// which an apply is refused with [errStopped]. activatedAt is the activation's
// instant ([coord.Activation.At]), which the chart apply stamps so a late older
// revision cannot walk a newer one back; zero applies none ([Engine.applyChart]).
func (e *Engine) Apply(ctx context.Context, cfg *config.Company,
	activatedAt time.Time) (configplane.ApplyStatus, []string, error) {
	e.applying.Lock()
	defer e.applying.Unlock()
	if e.stopped {
		return configplane.StatusError, nil, errStopped
	}
	var applied []string
	// THE SNAPSHOT FIRST: re-activating an unchanged revision is the
	// documented rotation gesture, and only a re-read makes it rotate. A
	// failed read builds on the previous snapshot, never on none, which would
	// resolve every ${VAR} from the environment alone; [New] always reads one
	// ([Engine.bootSecrets]), so this refuses only an engine built otherwise.
	if !e.refreshSecrets(ctx) && e.holdsSecretStore() && e.env.Load() == nil {
		err := errors.New("this node has never read its secret store, so the " +
			"revision's ${VAR} references would resolve from the environment alone")
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision was refused before anything changed")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "secrets")
	// THE BUILD, which changes nothing; [Buildable] runs the same one.
	next, sandboxManager, built, err := buildRevision(cfg, e.resolver(), e.plans, e.ledger, e.sandboxOtel, e.fakePlay)
	applied = append(applied, built...)
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision could not be built and was refused before "+
				"anything changed; this node still serves the previous epoch")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	stampActivation(next, activatedAt)
	// The windows this node kept before a restart, so an LLM fleet balances on
	// what its members last reported ([Engine.recallReadings]).
	e.recallReadings(ctx, next)
	// Spent plans stay spent with nothing carried: an entry on the same
	// account finds its predecessor's window, one on a new credential starts
	// clean ([harness.Plans]). The sweep drops a sign-in neither epoch names
	// and that sits nothing out.
	e.plans.Sweep(append(signIns(e.Company()), signIns(next)...), time.Now())
	// THE NATIVE HALVES on a node's first company, before the tools (registered
	// only where they exist) and the inbound edge (whose parsers include
	// theirs). The state log and identity estate run since boot, so this starts
	// only what depends on the company, and on a node without `data` only which
	// halves it reaches ([Engine.startNativeFor]).
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
	// THE SANDBOX RUNTIME, on a node that has none and whose revision reaches a
	// cell, before the tools: every executor launches into it ([Engine.startSandbox]).
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
	// Equipped before publishing, as at boot: a turn can start the instant the
	// pointer moves, and a revision missing its builtins looks like a model
	// that stopped using its tools.
	if err := e.equip(ctx, next); err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision built but could not be equipped with this "+
				"node's tools; the previous epoch is still current")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "tools")
	// The learning workers are rebuilt (they hold the epoch's org and model
	// registry); the dispatcher, its subscription and redelivery ring stay.
	// BUILT here, SWAPPED IN after the bundles below: live workers run the
	// revision's models at once, so they must follow the bundles and every
	// stage that can refuse. A failed swap keeps the previous workers (a stale
	// org is a smaller wrong than no reflection), except on a node's first
	// company, whose dispatcher is attached here ([Engine.prepareReflection]).
	// Named in the trail here because the swap cannot fail.
	installReflection, attachErr := e.prepareReflection(ctx, next)
	if attachErr != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", attachErr,
			"detail", "the reflect dispatcher could not be attached for this "+
				"node's first company; the revision is not served here yet")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", attachErr)
	}
	applied = append(applied, "learning")
	// Only the sandbox MANAGER is swapped: the coordinator and waiter hold this
	// process's busy set and poll loop, and rebuilding them would forget
	// mid-run seats and start a second loop ([sandbox.Coordinator.SetManager]).
	// Here, after every stage that can refuse a serving node, so a refusal
	// never leaves it launching through a catalogue its epoch lacks.
	if rt := e.sandbox.Load(); rt != nil {
		rt.coordinator.SetManager(sandboxManager)
		applied = append(applied, "sandbox")
	}

	// The party index is rebuilt BEFORE the publish: a removed seat stays
	// addressable an instant (a recorded skip) rather than an added seat being
	// unresolvable in the current epoch, which in a rollout is the one being
	// adopted. Not named in `applied`: [Engine.followCompany] names it for
	// both paths. A refusal after this costs only a rebuild.
	//
	// Every seat it indexes gets its mailbox first: its wakes go to its inbox,
	// which the stream discards while no consumer covers it, and the wake
	// queue ([Engine.wakes]) ensures only the served company's seats. A refusal
	// leaves mailboxes the retirement sweep removes.
	if e.node != nil {
		e.node.EnsureMailboxesFor(ctx, next.Seats())
	}
	e.refreshParties(ctx, next)
	if e.inboundStarted() {
		// Each vendor's parser derives its lead map from the org, so a kept
		// boot-time parser routes the revision's work by the old org chart
		// (TestEveryVendorReconcilerRunsOnApply).
		e.reconcileConfluence(next)
		e.reconcileDatadog(ctx, next)
		e.reconcileJira(ctx, next)
		e.reconcileGitLab(ctx, next)
		e.reconcileGitHub(ctx, next)
		// The chat surfaces too, or a surface connected after boot routes
		// every delivery to nobody until a restart. See [Engine.reconcileSlack]
		// for why one rebuilds unconditionally and the other does not.
		e.reconcileSlack(ctx, next)
		e.reconcileMattermost(ctx, next)
		// The engine's own chat's indicator: its mode is live founder policy
		// and a revision can switch its surface ([Engine.applyChatStatus]).
		e.applyChatStatus(ctx, next)
	} else if err := e.startInbound(ctx, next); err != nil {
		// A node that booted with no company has no edge for the reconcilers
		// above, so its first company STARTS it, and a failed start is
		// refused like a build: a company served with no edge looks healthy
		// and hears nothing, and the retry starts it again. A node in a
		// maintenance mode starts none ([Engine.startInbound]).
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the inbound edge could not be started for this node's "+
				"first company; the revision is not served here yet")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	// What the integration loop last concluded is stale: its cadence is for
	// re-asking third-party apps, and this document just changed
	// ([integration.Worker.MarkStale]).
	e.integrations.MarkStale()
	// The native backends' parsers are rebuilt likewise; their appliers,
	// index, stores and feeds follow a log a revision does not change
	// ([Engine.reconcileNative]).
	e.reconcileNative(ctx, next)
	// The tool skills' source after the knowledge base's reconcile, whose
	// wiring the Confluence source reads ([Engine.reconcileSkills]).
	e.reconcileSkills(next)
	applied = append(applied, "integrations")

	// THE CREDENTIAL BUNDLES, only once no stage can refuse: the workspace is
	// one per state directory, shared with the running seats, so a refused
	// revision's bundle would sign them in as an account no published
	// revision names, and survive every retry ([Company.restoreBundles]). And
	// before anything runs the revision's models (reflection swaps in next,
	// turns follow the publish), so the first call finds the login.
	next.restoreBundles(ctx)
	next.warnAncestorFiles(ctx)
	installReflection()
	previous := e.Company()
	e.installEpoch(ctx, next)
	applied = append(applied, "epoch")

	// THE SWEEP is rebuilt when this apply started the native runtime, whose
	// arrival changes its job list; horizons need nothing, as every job reads
	// the current epoch's at each sweep ([Engine.rebuildMaintenance]).
	if startedNative {
		e.rebuildMaintenance(ctx)
		applied = append(applied, "maintenance")
	}

	// THE BACKGROUND PASSES follow after the swap, as their loops walk the
	// current epoch's roster; the loops keep running and keep their clocks
	// ([Engine.reconfigureLearningPasses]).
	e.reconfigureLearningPasses(ctx, next)
	applied = append(applied, "learning_passes")

	// EVERYTHING DERIVED FROM THE COMPANY, AFTER the pointer moves, so no
	// refusal rebuilds the derived state of an epoch that never became
	// current: the same list a boot runs ([Engine.followCompany]).
	applied = append(applied, e.followCompany(ctx, next)...)

	log.InfoContext(ctx, "config_applied",
		"company", next.Config.Name, "seats", len(next.Seats()),
		"previous_seats", seatCount(previous))
	// PLACEMENT AT ONCE when seats or where they may run changed, rather than a
	// sweep interval later with added seats unrun and removed ones held. At the
	// end, not at the swap: acquisition reads what [Engine.followCompany] just
	// installed (seat tools, mailbox, model holds, sandbox runtime), so an
	// earlier pass claims seats onto a half-applied node ([seat.Host.Resweep]).
	if e.node != nil && seatsMoved(previous, next) {
		e.node.Host().Resweep()
	}
	// The seats this node holds are measured for an access review once the
	// applies go quiet (accessreview.go): a run of saves is one review.
	e.kickAccessReviews()
	return configplane.StatusOK, applied, nil
}

// buildRevision builds what an apply builds from a revision BEFORE it changes
// anything on this node: the company, over env, and the sandbox catalogue it
// configures, nil when it reaches no sandbox cell. The catalogue is built on
// every node beside the company: one that cannot be built is a revision that
// cannot be served, as every executor would launch into boxes nobody can mint.
//
// ONE LIST for [Engine.Apply] and [Engine.Buildable], so the check never passes
// what every apply refuses. The third return is the stages as the apply's
// trail names them (`company` once built). It leaves a harness's working
// directory claimed for the harness an entry names (the harness backend's
// workspace registry): a later revision driving another harness from the same
// directory is refused until a restart.
func buildRevision(cfg *config.Company, env *config.Resolver, plans *harness.Plans, ledger *fleet.Ledger,
	otel *sandbox.OtelReceiver, play fakePlayer) (*Company, *sandbox.Manager, []string, error) {

	next, err := newCompany(cfg, env, plans, ledger)
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

// signIns is the sign-in of every harness entry of an epoch, for
// [harness.Plans.Sweep]: the sign-ins whose records an apply must keep.
func signIns(c *Company) []harness.SignInID {
	if c == nil || c.Models == nil {
		return nil
	}
	var ids []harness.SignInID
	for _, provider := range c.Models.All() {
		if cli, ok := provider.(*harness.Provider); ok {
			if id := cli.SignInID(); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// Buildable reports whether cfg builds on this node — the half of an apply
// that changes nothing ([buildRevision]), over this node's own ${VAR} chain —
// and discards what it built.
//
// It is for a write that must not land a revision this node cannot run:
// `crewlet run`'s import activates its Tier B file fleet-wide before any apply
// builds it, so without this an unbuildable company replaces the running one,
// every apply refuses it, and the node's next boot is refused. The resolver is
// the boot snapshot ([Engine.bootSecrets]), which an apply re-reads; a value
// rotated in between is the rotation gesture, not a different revision.
func (e *Engine) Buildable(cfg *config.Company) error {
	_, _, _, err := buildRevision(cfg, e.resolver(), nil, nil, nil, e.fakePlay)
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
