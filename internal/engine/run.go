package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/skillsync"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracing"
)

// Engine is one process running a company.
//
// It owns exactly three things — the epoch, the backends, and the node — and
// the wiring between them. Everything else it delegates: the guard order is
// the inbox package's, the turn's rules are the turn package's, the seat math
// is the placement package's. That is the whole reason this file is short.
type Engine struct {
	// epoch is the company this engine is running, replaced whole by an
	// apply and never mutated. See epoch.go.
	epoch    epoch
	backends *Backends
	node     *node.Node

	// configWriter is how a disconnect removes a block, installed by the
	// wiring that builds the config surface. Atomic because the loop
	// reads it from its own goroutine while the API installs it.
	configWriter atomic.Pointer[ConfigWriter]

	// setupRunner is the provisioning runner this node serves, built once.
	//
	// ONE INSTANCE, and that is the whole reason it is held here rather
	// than constructed where it is used: [setup.Runner.Hold] guards a
	// surface with an in-process claim as well as a fleet lease, and three
	// callers with three claim maps guard nothing against each other. The
	// operator's pass, the reconcile loop's tick and a disconnect's
	// teardown all take it from here.
	setupRunner func() *setup.Runner
	// metrics is the process's one recorder, from [Options.Metrics].
	metrics *metrics.Recorder

	// searching is how many knowledge scans this node is running right
	// now — the gauge behind `crewlet.tracker.search.concurrency`. On the
	// ENGINE rather than on the searcher because the searcher is rebuilt
	// per epoch and a counter rebuilt underneath an in-flight scan would
	// decrement a fresh zero to minus one.
	searching atomic.Int64

	// mode is what this node started for, and incarnation is this
	// PROCESS's identity — the two facts the capacity window's handshake
	// and its barrier are written in terms of.
	mode        statelog.MaintenanceMode
	incarnation string

	// id is this node's stable identity, resolved once at construction.
	// A PLACEMENT rather than a process: what registers under it must
	// survive a restart, which is the opposite property to incarnation's.
	id string

	// duties names every fleet duty this engine claims, so a graceful stop
	// can give each one back. See [Engine.releaseDuties].
	duties claimedDuties

	// startedAt is when THIS engine was built, which is when the node
	// started: the API this process serves runs inside it and reports this
	// same instant. Carried on the presence heartbeat so a peer can tell a
	// node that has been up for a week from one that restarted a minute ago.
	startedAt time.Time

	// posture reports this node's config lag, from whoever owns the
	// control plane. Nil publishes none — see [Engine.SetPosture].
	posture atomic.Pointer[func(context.Context) string]

	// onApplied is run after each apply publishes its epoch, for the
	// surfaces derived from configuration. Nil runs nothing — see
	// [Engine.SetOnApplied].
	onApplied atomic.Pointer[func(context.Context)]

	// ownsBackends says whether Stop closes them. Ownership is a separate
	// fact from use: a borrowed Backends is still the one this engine
	// requeues and pauses through, so holding "the ones I use" and "do I
	// close them" in one nilable field made the park path reach for a nil
	// queue on exactly the engines that were lent their backends.
	ownsBackends bool

	// dispatch turns one inbox partition into one turn. Held so a test can
	// substitute its own without standing up a broker.
	dispatch *Dispatcher

	// modelHolds is every seat inbox this node paused because its company
	// had no model, which the apply that brings one releases. See
	// nomodels.go.
	modelHolds modelHolds

	// applying serialises [Engine.Apply] against [Engine.Drain], and stopped,
	// which it guards, is what refuses an apply once a drain has begun. See
	// [Engine.Apply].
	applying sync.Mutex
	stopped  bool

	// watchdog ends this process when the seat host's heartbeat stops
	// turning past the lease TTL.
	//
	// On the ENGINE rather than inside the seat host, for the reason
	// [seat.Watchdog.Stop] gives: the engine is what knows a shutdown has
	// begun, and disarming has to happen BEFORE the drain. Teardown
	// legitimately blocks for a long time — reaping MCP process trees,
	// joining goroutines, tearing sandboxes down — and a watchdog still
	// armed through it would shoot a node in the middle of the seat
	// release that makes a drain graceful.
	//
	// It was never constructed at all: seat.NewWatchdog had no caller
	// outside its own test, so a node that wedged neither worked nor died,
	// held a peer's mail unacked, and was never restarted by an
	// orchestrator watching for liveness.
	watchdog *seat.Watchdog

	// shuttingDown is set at the first moment of [Engine.Drain] and never
	// cleared. See [Engine.ShuttingDown].
	shuttingDown atomic.Bool

	// drainOnce makes the drain one operation however many callers ask for
	// it: [Engine.Stop] drains for a caller that did not, and a second run
	// would announce a second stop for one shutdown.
	drainOnce sync.Once

	// batch is the inbox coalescing window and cap, shared with every seat
	// attachment on this node.
	//
	// ONE VALUE, held on the ENGINE and mutated in place, because that is
	// what [queue.BatchOptions] is built for: it guards its own fields so a
	// hot reload takes effect on the next batch with no re-subscription.
	// The alternative — a fresh value per attach — would leave a seat
	// claimed before an apply reading the old window forever, and only a
	// seat that happened to move nodes would ever pick up a change.
	//
	// It was nothing at all: node.Config.BatchOptions was never set, so
	// every seat took queue.DefaultBatchOptions and the two company knobs
	// that configure this — notification_coalesce_window_seconds and
	// notification_coalesce_max_batch — were declared, defaulted,
	// validated, documented, and read by nothing.
	batch *queue.BatchOptions

	// sandbox is this node's code-work machinery: the coordinator holding
	// the busy set, the waiter polling detached runs, and the durable row
	// store behind both. On the ENGINE rather than on an epoch, because
	// they are facts about this process — rebuilding them on an apply would
	// forget which seats are mid-run and start a second poll loop against
	// the same rows.
	sandboxCoordinator *sandbox.Coordinator
	sandboxWaiter      *sandbox.Waiter
	sandboxPending     sandbox.PendingStore

	// leaseTTL is the seat lease TTL, which is also the seat lease bucket's
	// own age, resolved once from Tier A.
	//
	// Held because it is a CEILING for every SEAT claim this engine makes
	// outside the seat host: the KV's expiry is bucket-wide, so it refuses a
	// seat lease asked to outlive it, and the mailbox retirement claims a
	// removed seat's lease with exactly this TTL. Duties are NOT bounded by
	// it: they live in a bucket of their own whose ceiling is
	// coord.MaxDutyTTL, and a duty clamped to this value lapsed between two
	// of its own ticks.
	leaseTTL time.Duration

	// memory carries a seat's memory between the nodes that run it.
	//
	// On the ENGINE rather than on an epoch, for the same reason the
	// sandbox machinery is: it holds per-seat watermarks that describe what
	// THIS PROCESS has already published, and rebuilding it on a config
	// apply would republish every seat's whole history.
	//
	// Nil on a node with no broker or no store, where there is nothing to
	// carry memory over and nothing to carry.
	memory *memsync.Syncer

	// memorySync is the loop that publishes it, and the handle Stop uses
	// to end it. Nil where memory is.
	memorySync *memorySync

	// sandboxOtel mints each coding run's telemetry endpoint. Nil exports
	// nothing from inside a box, which is the ordinary configuration.
	//
	// Held here and handed OUT to the API this process serves rather than
	// built twice: that API verifies the tokens this engine minted, and two
	// receivers would sign with two per-process keys unless a keyring
	// happens to be configured, which is exactly the case that must not
	// depend on happening to be configured. A peer verifies them with its
	// own receiver, which is why the key is derived from the fleet's
	// keyring rather than held.
	sandboxOtel *sandbox.OtelReceiver

	// bridge serves a running seat's tool surface to a coding agent over
	// MCP. Nil where no bridge URL is configured, which is every
	// deployment that runs no agent mode.
	bridge *mcpbridge.Bridge

	// reflector is the learning write side: one dispatcher for the life of
	// the process, whose org and workers an apply swaps. On the ENGINE
	// rather than on an epoch because its redelivery ring is process
	// state — see learning.Reflector.
	reflector *learning.Reflector

	// native is this node's copy of the company's own tracker and
	// knowledge base: the projectors, their index, and the read and write
	// sides over them. Nil on a company running the vendor backends, which
	// is the whole switch — see native.go.
	//
	// On the ENGINE rather than on an epoch, because a projector follows a
	// coordination FAMILY and a family does not change when a company
	// revision does. Rebuilding it on an apply would drop the projection
	// and re-run a boot reconcile on every configuration change, which for
	// a company that edits its org chart twice a day is a projection that
	// is never hydrated.
	native *native

	// env is this node's ${VAR} resolver: the secret store in front of the
	// process environment, refreshed on every apply. One per node rather
	// than one per call site — see secrets.go for why that matters.
	//
	// Pointer[Resolver], not Pointer[*Resolver]. The doubled indirection
	// bought a second nilable level with no meaning of its own — a stored
	// non-nil pointer to a nil resolver read the same as nothing stored —
	// so every reader had to check both, and one that checked only the
	// outer would have dereferenced nil.
	env atomic.Pointer[config.Resolver]

	// republish coalesces the re-activations a provisioning pass asks for
	// when it seals a credential. See republish.go.
	republish republisher

	// cipher is the keyring this node seals and opens secret rows with,
	// nil on a node that has none. Held rather than rebuilt because ONE
	// cipher per process is what keeps a row this node wrote a row it can
	// read back.
	cipher secrets.Cipher

	// sinkUnavailable makes the loop say "this node cannot seal a minted
	// credential" ONCE. It is a property of [Engine.cipher] — the same for
	// every integration and unchanged until the process restarts — and the
	// reconcile loop asks per surface per tick. See integrations.go.
	sinkUnavailable sync.Once

	// profile is what this node declared it does: whether it claims
	// seats, serves inbound traffic, and runs the fleet's singleton
	// duties. Held because the duty gate reads it on every claim — see
	// duty.go for why the lease alone is not the whole answer.
	profile placement.NodeProfile

	// learning is the loops of the background passes no turn drives:
	// episode compaction, skill ageing, clustered synthesis and promotion.
	// On the ENGINE for the same reason the sandbox waiter is: they are
	// loops this PROCESS runs, and rebuilding them on an apply would restart
	// their clocks. What they run is the applied revision's, handed over by
	// every apply. See [Engine.startLearningBackground].
	learning *learning.Background

	// notify is this node's inbound edge: the party registry, the
	// notification service and the third-party app transports. On the ENGINE
	// rather than on an epoch, because a transport holds live sockets and
	// server-resolved identities — rebuilding it on every apply would
	// drop every connection whenever an unrelated field changed. The
	// registry inside it IS swapped per epoch; see notifications.go.
	notify notifications

	// skills is this node's tool-skill registry, built once and OUTLIVING
	// every epoch: its content comes from the knowledge base rather than
	// from config, so an apply that changed a seat's model has nothing to
	// say about it. Rebuilding it per epoch would empty it on every apply
	// and leave seats running without their company's guidance until the
	// next sync walk — which on a webhook-driven sync could be never.
	skills *skills.Registry

	// skillSync is the one loop that keeps [Engine.skills] current: the
	// source each apply names, the page changes this node and its peers
	// heard, a periodic walk and a bounded retry. On the ENGINE for the
	// registry's own reason, and because it is a loop this process runs.
	// See skillsync.go.
	skillSync *skillsync.Syncer

	// mcp supervises every MCP child this NODE runs — the company's shared
	// servers and the per-role children of the seats it holds. One per
	// engine rather than one per epoch: the children are processes, and an
	// apply that replaced the bridge would orphan every one of them.
	mcp *mcp.Bridge

	// seatMCP is one bridge per seat this node holds, for that seat's
	// per-role children. Separate from the shared bridge above because a
	// bridge's catalogue is keyed by tool name across its servers, and two
	// seats' children of one template publish the same names.
	//
	// seatTools is that seat's registry — the epoch's surface cloned, with
	// the bridge's catalogue filed into it. It lives HERE, beside the
	// bridge whose lifetime it shares, rather than on the [Company]: both
	// are per-SEAT-LEASE, and a [Company] is per-EPOCH. Held on the epoch
	// it was silently dropped by every apply, because an apply publishes a
	// new [Company] and nothing carried the map across — so every seat
	// this node already held fell back to the shared surface and lost its
	// entire per-role tool set, permanently, until the seat was released
	// and re-claimed. Both maps are guarded by mcpMu because they must
	// never disagree: a registry naming tools of a bridge that is gone
	// offers the model entries that can only fail.
	mcpMu     sync.Mutex
	seatMCP   map[string]*mcp.Bridge
	seatTools map[string]*tools.Registry

	// embeddings is the company's vector backend, swapped on apply. An
	// atomic pointer rather than a mutex because it is read on the Plan
	// phase's hot path and written only by an apply: the read must not
	// queue behind anything, and there is nothing else to hold a lock
	// across.
	embeddings atomic.Pointer[embeddings.Embedder]

	// maintenance is the retention sweep for the short-horizon tables. On
	// the engine for the same reason the sandbox machinery is: it is a
	// loop this process runs, and rebuilding it on an apply would start a
	// second one against the same rows.
	maintenance  *maintenance.Worker
	integrations *integration.Worker

	// mailboxes registers each seat's mailbox with the fleet and retires
	// the mailbox of a seat that has left the company. Built BEFORE the
	// node, which registers through it, and swept by the maintenance duty.
	// Nil on an engine with no fleet store, where nothing is registered
	// and nothing retired.
	mailboxes *maintenance.Mailboxes

	// reconciler is the loop converging this engine on the activation
	// pointer, recorded by [Engine.NewReconciler]. The mailbox sweep reads
	// which activation this engine's epoch came from through it, because
	// nothing else knows: an apply is handed a document, not an epoch.
	reconciler atomic.Pointer[Reconciler]

	// rewired remembers what the last seat-identity retry resolved on each
	// surface, so the recovery is logged on the TRANSITION rather than on
	// every pass of every surface for the life of the deployment. See
	// [rewireLog].
	rewired rewireLog

	// retention is the state log's trim: the fleet singleton that decides
	// how far each domain's log may be purged and publishes what it
	// concluded. On the ENGINE for the reason maintenance is — it is a
	// loop this process runs, and rebuilding it on an apply would leave
	// two loops publishing one fleet's floor.
	retention *retention

	// budgetReports is the live token-meter loop. Every node runs one —
	// the counters are shared, so this is a frame rather than a duty.
	budgetReports *budgetReporter

	// embedding is the vector domain's one writer: the fleet singleton
	// that turns sources whose text has moved into vector records. On the
	// ENGINE for the reason the trim is — it is a loop this process runs,
	// and rebuilding it on an apply would leave two loops holding one
	// company's provider budget. It reads the current epoch's embedder per
	// tick instead, so a model change lands without a restart.
	embedding *embedDuty

	// scheduler is the role/unit cron tick. On the ENGINE rather than on an
	// epoch for the same reason maintenance is: it is a loop this process
	// runs, and rebuilding it on an apply would leave two loops racing for
	// one company's fires. reconcileScheduler arms and disarms it instead.
	scheduler schedulerLoop

	// cooldowns is the loop that pulls the fleet's credential bench into
	// this node's pools. On the ENGINE rather than on an epoch because it
	// is a loop this process runs — see cooldowns.go.
	cooldowns *cooldowns

	// onboarded remembers which seats this PROCESS has seen onboarded.
	//
	// On the engine rather than on an epoch, because it is a fact about
	// what this process has observed and not about the company: an apply
	// publishes a new epoch, and a latch that came with it would forget
	// every seat and re-run a pass for agents already marked. It is keyed
	// by chain hash, so a live restructure still re-onboards by design.
	onboarded *runner.Latch
}

// Options configure an engine.
type Options struct {
	Bootstrap *config.Bootstrap
	Company   *config.Company

	// Mode is what this node starts for: normal, or one of the two
	// maintenance modes a capacity change restarts the fleet into.
	//
	// A RUN'S PROPERTY rather than the deployment's, so it is never read
	// from Tier A: every node enters and leaves it three times over one
	// change, and a file carrying it would leave a node that came back
	// after an unrelated restart still refusing to publish, with the
	// reason in a file nobody re-read. Empty is normal.
	Mode statelog.MaintenanceMode

	// Metrics is where this node's measurements are written.
	//
	// ONE RECORDER FOR THE PROCESS, built by the caller and shared with
	// [tracing.Configure], whose MeterProvider is a READER of it — two
	// recorders would make a collector's dashboard and `crewlet retention
	// status` disagree about the same event, which is the drift the single
	// catalogue exists to prevent.
	//
	// Nil records nothing, which is a legal deployment (a test, an
	// embedded engine) and NOT the normal one: every instrument in
	// [metrics.Catalogue] is declared either way, so a nil recorder is a
	// documented catalogue nothing fills.
	Metrics *metrics.Recorder

	// OtelReceiver mints the per-run OTLP endpoints a sandbox exports to.
	// Nil builds one from the environment; see
	// [sandbox.BuildOtelReceiver].
	OtelReceiver *sandbox.OtelReceiver

	// Bridge serves a seat's tools to a coding agent. Nil builds one from
	// the environment; see [mcpbridge.Build].
	Bridge *mcpbridge.Bridge

	// Backends may be supplied by a caller that already opened them and
	// keeps their lifetime. Nil opens them from the bootstrap config, and
	// the engine then owns their lifetime.
	//
	// `crewlet run` always leaves this nil: the engine opens its own, and
	// the API the same process serves reads them back through
	// [Engine.Backends]. The callers that supply a set are the ones that
	// need the estate before or after the engine that runs on it: a peer's
	// record written before this node exists, or a failed boot whose
	// leftovers are inspected. A supplied set must be complete; see
	// [Backends.Complete].
	Backends *Backends

	// Dispatch overrides the default dispatcher.
	//
	// Fields left nil on it are FILLED IN from this engine, rather than the
	// whole dispatcher being replaced or the whole thing being taken as-is.
	// A test that wants to pin ownership answers supplies Conditions and
	// still gets the real ledgers; one that wants to observe parks supplies
	// Park and still gets the real screening.
	Dispatch *Dispatcher

	// AwaitingSandbox reports whether a seat is parked on a detached coding
	// run, whose job outlasts any broker ack window. Nil answers no, which
	// is correct for a build with no sandbox provider wired: a seat that
	// cannot start a detached run is never waiting on one.
	AwaitingSandbox func(handle string) bool

	// SandboxPollInterval overrides the completion poll's cadence. Zero
	// takes the production value, which is sized against coding jobs that
	// run for minutes; a test shrinks it so a run settles in a second
	// rather than waiting out a real tick.
	SandboxPollInterval time.Duration
}

// New assembles an engine.
//
// Config errors surface HERE, before anything is dialled or claimed. A node
// that boots on a bad config and discovers it at the first turn has already
// told its peers it owns seats.
func New(ctx context.Context, opts Options) (*Engine, error) {
	if opts.Bootstrap == nil {
		return nil, fmt.Errorf("engine: no bootstrap config")
	}
	// THE CROSS-TIER RULES, before anything is opened. Each tier validated
	// alone on its way in; what neither could see is the other, and the one
	// rule that needs both (a native tracker or knowledge base whose log
	// would live on an in-memory stream) produces a node that refuses to
	// serve permanently rather than one that fails on the way up.
	if err := config.CheckTiers(opts.Bootstrap, opts.Company); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}

	// EVERYTHING THAT CAN FAIL WITH NOTHING OPEN COMES FIRST, which is why
	// the telemetry receiver, the bridge and this node's identity are
	// resolved above the backends rather than below them. It is the same
	// ordering [Engine.startNative] uses on its own node-id read, for the
	// same reason: a failure that has opened nothing has nothing to unwind.
	// Placed below the backends, each of these returned past an ALREADY
	// OPEN store and broker and closed neither — and the store is
	// exclusive to one process, so the next attempt in the same process
	// contended with the corpse of this one.
	//
	// BUILT BEFORE THE SANDBOX RUNTIME, because the manager takes it: a
	// receiver constructed after the first apply would leave every run
	// launched in between exporting nowhere, silently.
	otel := opts.OtelReceiver
	if otel == nil {
		built, err := sandbox.BuildOtelReceiver(os.Getenv,
			keyMaterial(opts.Bootstrap))
		if err != nil {
			// A receiver URL that is set and unusable is a deployment
			// that asked for in-box telemetry and would get none, so it
			// is refused rather than dropped: the alternative is a
			// config that looks complete and exports nothing.
			return nil, fmt.Errorf("engine: sandbox telemetry: %w", err)
		}
		otel = built
	}
	// SAME KEY MATERIAL, DIFFERENT DOMAIN, and for the same reason the
	// receiver above is built here: a split deployment mints in this
	// process and verifies in another, so both derive their key from the
	// fleet's keyring rather than from a per-process random.
	bridge := opts.Bridge
	if bridge == nil {
		bridge = mcpbridge.Build(os.Getenv, keyMaterial(opts.Bootstrap))
	}

	// THE MODE AND THE INCARNATION, resolved once. An unset mode is
	// normal, which is what every caller that does not know about
	// maintenance gets; the incarnation is minted here rather than at the
	// lease, because the capacity barrier compares it and a second mint
	// would be a second identity for one process.
	//
	// AND THE NODE ID ONCE TOO. It was resolved here and then AGAIN a
	// hundred lines below, from the same bootstrap through the same
	// resolver, with the second copy handed to the node while the first
	// stayed on the engine — two constructions of the one value the
	// comment beside the second forbids, and two chances to disagree about
	// what this node is called if either ever learns a new source.
	mode := opts.Mode
	if mode == "" {
		mode = statelog.ModeNormal
	}
	nodeID, err := config.ResolveNodeID(opts.Bootstrap, nil)
	if err != nil {
		return nil, err
	}
	incarnation := config.NewIncarnation(nodeID)

	// Only what this engine OPENED does it close. A caller that supplied
	// backends keeps their lifetime, and must supply all four: see
	// [Backends.Complete] for why a partial set is refused rather than run.
	backends := opts.Backends
	ownsBackends := false
	if backends != nil {
		if err = backends.Complete(); err != nil {
			return nil, err
		}
	} else {
		// ASSIGNED, not declared through a temporary: `opened, err :=`
		// shadows the err the node-id read above already declared, which is
		// the one shape govet cannot tell from the bug that check is on for.
		backends, err = OpenBackends(ctx, opts.Bootstrap, opts.Company)
		if err != nil {
			return nil, err
		}
		ownsBackends = true
	}

	e := &Engine{
		backends: backends, ownsBackends: ownsBackends,
		onboarded: runner.NewLatch(), skills: skills.NewRegistry(),
		mcp:         mcp.NewBridge(nil),
		sandboxOtel: otel,
		bridge:      bridge,
		metrics:     opts.Metrics,
		mode:        mode,
		incarnation: incarnation,
		id:          nodeID,
		startedAt:   time.Now().UTC(),
		// Built before equip, which is what writes the company's own
		// numbers into it, and before node.New, which hands the same
		// value to every seat attachment.
		batch: queue.DefaultBatchOptions(),
	}
	// ONE RUNNER PER ENGINE, because its in-process guard is half of what
	// keeps two writers off one third-party app — see [setup.Runner.Hold].
	// A runner per caller would give the loop, the API and a disconnect a
	// claim map each, which guards nothing.
	e.setupRunner = sync.OnceValue(e.newSetupRunner)

	// ONE FAILURE PATH FOR EVERYTHING BELOW, armed before the first thing
	// that outlives this call and stood down only once the engine is the
	// caller's.
	//
	// Every return below this line used to unwind by closing the backends
	// and nothing else. By the last of them this node had brought up a
	// state log, spawned the company's shared MCP process trees, told the
	// fleet it was about to publish, and armed a dozen duty loops — none
	// of which the discarded engine could ever be asked to stop again,
	// because nothing else holds a reference to it. Closing the store and
	// the broker underneath them is exactly the mid-batch write
	// [Engine.Stop] orders itself to avoid; and where the CALLER supplied
	// the backends, nothing was closed at all and the loops simply ran on,
	// in a process whose boot had failed.
	//
	// Deferred rather than written at each return, for the reason
	// [Engine.startNative]'s own guard is: the list of things to unwind
	// grows down the function, and a per-return list stops matching it at
	// the first loop somebody adds.
	//
	// [context.WithoutCancel] because this is a teardown. The failure
	// being unwound is routinely the caller's own cancellation, and a
	// cleanup that inherited a dead context does nothing at all.
	booted := false
	defer func() {
		if booted {
			return
		}
		e.teardown(context.WithoutCancel(ctx))
		log.InfoContext(ctx, "engine_boot_abandoned", "node", nodeID,
			"detail", "this boot left nothing running: the state log, the "+
				"shared MCP children, every duty loop and this node's "+
				"admission are stopped, and any backends this engine opened "+
				"itself are closed")
	}()

	// THE SKILL SYNC IS BUILT BEFORE THE NATIVE BACKENDS, whose page
	// projection nudges it from a post-commit hook: a nudge with nothing to
	// land on would be lost. Building it starts nothing.
	if err = e.newSkillSync(nodeID); err != nil {
		return nil, fmt.Errorf("engine: tool skills: %w", err)
	}

	// THE KEYRING AND THE SNAPSHOT BEFORE THE FIRST EPOCH, because the
	// epoch resolves every ${VAR} it holds as it is built — the provider
	// keys, the integration tokens, the per-role MCP env. Loading the
	// snapshot afterwards would give the first epoch environment-only
	// resolution and every later one the store, so a rotated secret would
	// work on the second apply and not on boot.
	//
	// A node whose keyring is CONFIGURED but broken fails here rather than
	// resolving everything from the environment and looking healthy.
	cipher, err := openCipher(opts.Bootstrap)
	if err != nil {
		return nil, err
	}
	e.cipher = cipher

	// THE ADMISSION HANDSHAKE, BEFORE ANY PUBLISHER — and this is the
	// earliest point at which it can run, because it needs the
	// coordination backend and nothing else. Everything below it starts
	// something that writes.
	//
	// A NODE IN A MAINTENANCE MODE SKIPS IT AND ACKNOWLEDGES INSTEAD: it
	// is not admitting itself to publish, it is offering the evidence the
	// capacity barrier is established from. See maintenance_mode.go.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.admit(ctx, maintenanceStreams()); err != nil {
		return nil, err
	}
	e.acknowledge(ctx, maintenanceStreams())

	// MIGRATED BEFORE THE SNAPSHOT, so a value set on this node while the
	// engine was stopped is on the fleet before anything resolves it —
	// and, once it is, so are this node's peers. See [Engine.migrateSecrets].
	e.migrateSecrets(ctx)
	e.refreshSecrets(ctx)

	// AND THE FOLLOWS, on the same reasoning and in the same window: before
	// the epoch below builds the chat transports, so nothing is matching an
	// inbound message against the bucket while it is being filled. See
	// [Engine.migrateFollows].
	e.migrateFollows(ctx)

	// A NIL COMPANY IS THE UNCONFIGURED NODE, not a caller's mistake.
	//
	// The store is authoritative at runtime and a fleet's first revision
	// may arrive over the API, so a node has to be able to run with no
	// company at all: serving its HTTP surface, reporting itself
	// unconfigured, and holding no seats until the control plane hands it
	// an epoch. Everything below is written against that — a nil epoch
	// means "nothing to equip, nothing to place", never a panic.
	var company *Company
	if opts.Company != nil {
		company, err = NewCompanyWith(opts.Company, e.resolver())
		if err != nil {
			return nil, err
		}
	}
	// SKIPPED ENTIRELY WHEN THERE IS NO EPOCH. Both of these are derived
	// from a company — the sandbox backends it configures, the tools its
	// seats are given — and an unconfigured node has neither to build. The
	// apply that brings it its first revision runs both then, on the same
	// path every later apply takes.
	if company != nil {
		// BEFORE equip, because equip registers run_sandbox and only a
		// node with a coordinator can offer it: a tool whose dependency is
		// absent is OMITTED rather than registered-and-broken, so an
		// engine that equipped first would build a code-enabled company
		// whose seats have no code tool and plan around one anyway.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.buildSandboxRuntime(company); err != nil {
			return nil, fmt.Errorf("engine: sandbox: %w", err)
		}
		// BEFORE equip too, and for exactly the same reason: the ten
		// native tracker and knowledge tools are registered only where
		// their halves exist, so a node that equipped first would run a
		// company on the native backends whose seats have no way to read
		// or write them.
		//
		// It does NOT wait for hydration — the reconcile is O(keys), and
		// a node that blocked here would serve no dashboard, answer no
		// probe and run no duty until it finished. Seat acquisition is
		// what waits; see [Engine.NativeHydrated].
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.startNative(ctx, opts.Bootstrap, company); err != nil {
			return nil, err
		}
		// EQUIPPED BEFORE PUBLISHED. A turn can start the instant the
		// epoch is current, and one that found an empty registry would run
		// a seat with no tools at all — a company that boots cleanly and
		// can do nothing.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.equip(ctx, company); err != nil {
			return nil, err
		}
	}
	e.installEpoch(company)

	// SET BEFORE the node, because the node is handed this exact value —
	// two constructions of it would be two places to disagree about what
	// this node does.
	// THROUGH THE BOOTSTRAP'S OWN ACCESSOR, not a second construction of
	// the same thing: it parses the roles with the validator that already
	// refused an unknown one at load, and it is what the fleet view reads
	// a peer's presence row back through.
	e.profile = opts.Bootstrap.Node.Profile(nodeID)
	e.leaseTTL = effectiveLeaseTTL(opts.Bootstrap, backends.Coord)
	// BEFORE the node, which registers every seat's mailbox through it on
	// its first walk, and AFTER the lease TTL, which a retirement claims a
	// seat for.
	if e.mailboxes, err = e.buildMailboxes(backends, nodeID); err != nil {
		return nil, fmt.Errorf("engine: seat mailboxes: %w", err)
	}
	n, err := node.New(node.Config{
		Queue:     backends.Queue,
		Coord:     backends.Coord,
		Mailboxes: e.mailboxRegistry(),
		// The node ID is STABLE across restarts; the owner is this
		// INCARNATION. A restarted process that reused its owner id would
		// be indistinguishable from the one that died, and would inherit
		// leases it never renewed.
		NodeID: nodeID,
		Owner:  config.NewIncarnation(nodeID),
		// Read FRESH through the epoch, never bound to the company this
		// engine started on: an apply replaces the seat set, and a
		// method value captured here would keep claiming seats a
		// deleted role no longer has and never claim a new one. The
		// host's own doc asks for exactly this and the binding did the
		// opposite.
		Seats: func() []placement.Seat { return e.Company().Seats() },
		// A seat whose mailbox attached before this node's projection had
		// caught up would answer "there is no such item" to its own
		// tools — and that is an answer a seat ACTS on: it files the
		// duplicate, it tells a person their link is dead. So a node
		// mid-hydration keeps every seat it holds and claims nothing new
		// until its copy of the company's records is current.
		//
		// Trivially true on a company running the vendor backends, which
		// have no projection to wait for.
		SeatsAdmitted: e.NativeHydrated,
		// AND THE OTHER DIRECTION. Admission withholds new work from a
		// node that is merely behind; this gives back work already held
		// by a node whose rows are wrong. Two gates because the remedies
		// differ: one is waiting, the other is a peer taking over.
		SeatsServiceable: e.SeatsServiceable,
		Profile:          e.profile,
		// WHAT THIS NODE IS DOING, advertised to peers on every
		// heartbeat. Only the node running a seat knows its in-flight
		// count and its drain state, and /health answers about whichever
		// node served the request — so behind a load balancer a refresh
		// tells a different story each time.
		Status: e.nodeStatus,
		Turn:   e.Dispatch,
		// Before the mailbox opens and after it closes — see node.Config.
		// A seat mid-detached-run must have its runs recovered and its mail
		// parked before anything can deliver to it.
		SeatReady: func(ctx context.Context, handle string, lease coord.Lease) error {
			return e.prepareSeat(ctx, handle, lease.Epoch, lease.Owner)
		},
		SeatDone: e.releaseSeat,
		LeaseTTL: e.leaseTTL,
		// The host's own ceiling, from Tier A. Per NODE, so a fleet's is
		// N times this. Passed through unresolved: zero is the shape of an
		// absent key and node.New is what turns it into the default, so
		// the number lives in one place — see node.DefaultMaxConcurrent.
		MaxConcurrent: opts.Bootstrap.Node.MaxConcurrent,
		// The company's own coalescing knobs, live: this is the value an
		// apply writes through, and every seat attachment reads it.
		BatchOptions: e.batch,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: node: %w", err)
	}
	// BEFORE THE NODE RUNS, because the first seat it acquires hydrates
	// through this. A nil syncer is the honest shape for a node with no
	// broker or no store: there is nowhere to carry memory to, and
	// prepareSeat then skips the step rather than pretending it happened.
	if e.memory, err = memsync.New(backends.Store, backends.Conn(),
		func(handle string) string {
			c := e.Company()
			if c == nil {
				// Unconfigured: no seat has an identity yet, and
				// nothing has memory to carry.
				return ""
			}
			role := c.Org.AgentSeatByHandle(handle)
			id, ok := c.Org.AgentIDFor(role)
			if !ok {
				return ""
			}
			return id.String()
		}); err != nil {
		return nil, fmt.Errorf("engine: seat memory: %w", err)
	}
	// ARMED HERE, STARTED IN Start. The watchdog stands down permanently
	// the first time no watched duty is live, and the host is not live
	// until node.Start runs it — so building it here and starting it there
	// is the only ordering that watches anything at all.
	//
	// AT THE HOST'S OWN TTL, which is the lease the seats it guards are
	// actually held under — see [seat.Watchdog], whose whole argument is
	// that the two are one number. Asked of the host rather than passed
	// e.leaseTTL, because the host is what resolved it.
	e.watchdog = seat.NewWatchdog(n.Host().TTL())
	e.watchdog.Watch("seat-host", n.Host())
	e.node = n
	e.dispatch = e.buildDispatcher(opts, backends)

	// EVERYTHING BELOW THIS LINE PUBLISHES, and a maintenance-mode node
	// starts none of it: the seat host and its mailboxes, every duty, the
	// scheduler, the change feeds, the notification spine, the retention
	// trim and the reflection pass.
	//
	// ONE GATE RATHER THAN A CONDITION PER LOOP. Each of these is started
	// by its own call and a per-call check is a list somebody maintains —
	// which is the shape that lets one publisher be forgotten and the
	// whole exclusion be a claim. What makes this correct is that the
	// gate is the LAST thing in the constructor: a loop added after it
	// cannot be started by accident, because there is no code after it to
	// add one to.
	if !e.mode.Publishes() {
		log.InfoContext(ctx, "maintenance_mode_started",
			"mode", e.mode, "node", nodeID, "incarnation", e.incarnation,
			"detail", "the broker and the coordination estate are up and no "+
				"publisher is: no seats, no duties, no scheduler, no change "+
				"feed and no write routes")
		// THE ENGINE IS THE CALLER'S FROM HERE, so the guard above stands
		// down and [Engine.Stop] — the same teardown — is what ends it.
		// This return is a success like the last one, and a maintenance
		// node that unwound itself on the way out would take down the one
		// posture the capacity barrier is waiting to read.
		booted = true
		return e, nil
	}

	// LAST, because its fleet-singleton duty is claimed under the node's
	// own incarnation.
	if err := e.startSandboxWaiter(ctx, opts.SandboxPollInterval); err != nil {
		return nil, fmt.Errorf("engine: sandbox waiter: %w", err)
	}
	e.startMaintenance(ctx)
	// THE LOG'S OWN TRIM, beside the sweep and after the node exists for
	// the same reason: its duty is claimed under the node's incarnation,
	// and a trim that ran before the lease existed would run on every node
	// at once. Without it a domain's log only ever grows — to its ceiling,
	// where appends are refused.
	// THE LIVE TOKEN METERS, which the dashboard's header pushes from and
	// which nothing published — so every header carried zeroes. Armed
	// before the native backends, because it needs neither: the counters
	// are coordination's and the company's caps are the epoch's.
	e.startBudgetReports(ctx)
	if e.native != nil {
		e.startRetention(ctx, opts.Bootstrap, e.native.log)
		// AND THE VECTOR DOMAIN'S ONE WRITER. Without it every other
		// half of semantic search is present and correct over an empty
		// corpus — which reports as a healthy domain rather than as a
		// missing one.
		e.startEmbedding(ctx, e.native.log)
	}
	// Beside the sweep, and a fleet singleton on the same terms: two nodes
	// reconciling one third-party app at the same moment can each create an identity
	// for one seat, and no later pass can detect or repair that.
	e.startIntegrations(ctx)
	e.startMemorySync(ctx)
	e.startScheduler(ctx)
	// The credential pools were attached to the fleet's ledger by equip,
	// above, so a bench already publishes. This arms the other half: the
	// pull that tells this node what its peers have already benched.
	e.startCooldownRefresh(ctx)
	// AFTER the epoch is current, because the dispatcher resolves a turn's
	// seat against it — and before notifications, so a turn woken by the
	// first inbound message already has somewhere to write what it learns.
	// A failure here is fatal: the subscription is the only path to the
	// write side, and a company that boots without it learns nothing at
	// all while looking entirely healthy.
	if err := e.startReflection(ctx); err != nil {
		return nil, err
	}
	// The background passes, after the node exists: each is a fleet
	// singleton claimed under its own incarnation.
	e.startLearningBackground(ctx)
	// BEFORE notifications, because a feed publishes onto the same inbound
	// edge the service consumes: started after, the changes committed in
	// the window between would reach the record and wake nobody.
	e.startNativeFeeds(ctx)
	// BEFORE notifications, because the Confluence parser hands it every
	// page change it hears; see [Engine.startSkillSync].
	e.startSkillSync(ctx)
	if err := e.startNotifications(ctx, e.Company()); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	// AND THE SOURCE AFTER THEM, because the Confluence source is read off
	// the wiring startNotifications just built. This is the boot walk: an
	// apply calls the same thing for every later epoch.
	e.reconcileSkills(e.Company())
	booted = true
	return e, nil
}

// buildDispatcher wires the dispatcher ONCE.
//
// Once, at construction, and never again — not per delivery. The node runs one
// consume loop per seat, so every attached seat can be inside Dispatch at the
// same moment; assigning to the shared dispatcher's fields on the way in is a
// write to memory a peer goroutine is reading, which is a data race whatever
// the values happen to be. The fields it would have written are the same on
// every call, which is exactly what makes the bug survive testing.
func (e *Engine) buildDispatcher(opts Options, backends *Backends) *Dispatcher {
	d := opts.Dispatch
	if d == nil {
		d = &Dispatcher{}
	}
	if d.Conditions == nil {
		awaiting := opts.AwaitingSandbox
		if awaiting == nil && e.sandboxCoordinator != nil {
			awaiting = e.sandboxCoordinator.AwaitingSandbox
		}
		d.Conditions = e.conditionsFor(awaiting)
	}
	if d.Ledgered == nil {
		d.Ledgered = inbox.Ledgered
	}
	if d.Turn == nil {
		d.Turn = e.runTurn
	}
	if d.Prompts == nil {
		// Read off the LIVE epoch on each dispatch, like the conversation
		// policy below: buildDispatcher runs BEFORE startNotifications, so
		// a captured registry would be the empty one this node booted with
		// and every merge would supersede nothing for the life of the
		// process.
		d.Prompts = e.notifyPrompts
	}
	if d.Conversation == nil {
		// Read off the LIVE company on each dispatch, not captured here:
		// buildDispatcher runs once and a config apply replaces the company.
		d.Conversation = func() config.ConversationSession {
			return e.Company().Config.TurnEngine.ConversationSession
		}
	}
	if d.Park == nil {
		d.Park = e.park
	}
	if d.Pause == nil {
		d.Pause = e.pause
	}
	if d.Answer == nil && e.sandboxCoordinator != nil {
		// THE CALLER THIS METHOD NEVER HAD. TryResumeFromAnswer has been
		// exported and tested since the clarification path was written, and
		// nothing in the engine called it — so a coding run that asked a
		// person a question waited out its pause TTL however promptly they
		// replied.
		d.Answer = e.sandboxCoordinator.TryResumeFromAnswer
	}
	if d.NoteDeferred == nil {
		d.NoteDeferred = e.node.Host().NoteDeliveryDeferred
	}
	if d.Observe == nil {
		d.Observe = e.observe
	}
	// THE TWO LEDGERS COME FROM DIFFERENT PLACES, and the split is the
	// point. Completions must be agreed across the FLEET — a redelivery
	// that lands on a peer has to find the record, or the turn runs twice
	// — so it lives on the coordination store. Conversations are a seat's
	// own history, read only by the node running that seat, so they stay
	// on the node's local database where a long thread costs nothing to
	// replicate.
	//
	// Neither is nil-checked: [New] refuses a Backends without a store or a
	// fleet, so both are always here by the time this runs.
	if d.Completions == nil {
		d.Completions = ledgerstore.NewFleetCompletions(backends.Fleet)
	}
	if d.Conversations == nil {
		d.Conversations = ledgerstore.NewConversations(backends.Store)
	}
	return d
}

// Start brings the engine up.
//
// The node's long-lived loops are started on a DETACHED context, and that is
// the whole substance of this method. The seat host derives its heartbeat and
// sweep loops from whatever it is started with, so an engine started on a
// signal context stops renewing its leases the instant SIGTERM arrives —
// before the drain has begun. The drain then waits for in-flight turns, which
// can take minutes, while every lease lapses at the TTL and peers claim seats
// this node is still running turns on. Two nodes, one seat, arrived at through
// the graceful path.
//
// STOP is what ends an engine. A caller's context bounds how long Start itself
// may take, not how long the engine runs.
func (e *Engine) Start(ctx context.Context) error {
	if err := e.node.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("engine: start: %w", err)
	}
	// AFTER the host is running, and detached from the caller's context
	// like the host itself. Before it, every watched duty reads as not
	// live and the watchdog stands down for the life of the process —
	// which is the one failure mode a suicide timer must not have: it
	// looks exactly like a healthy armed one. [Engine.Stop] disarms it.
	//
	// The precondition is CHECKED rather than commented, because getting
	// it wrong is silent by construction: a watchdog that stood down and
	// one that is watching are indistinguishable from outside the package,
	// and the difference only shows up as a node that wedges and is never
	// restarted.
	if e.watchdog != nil {
		if _, live := e.node.Host().Beat(); !live {
			log.WarnContext(ctx, "watchdog_not_armed",
				"detail", "the seat host is not running yet, so the watchdog would "+
					"stand down permanently; this node will not end itself if it wedges",
				"hint", "start the watchdog after node.Start, not before")
		} else {
			e.watchdog.Start(context.WithoutCancel(ctx))
		}
	}
	// The started line has to be printable BEFORE a company exists: an
	// unconfigured node genuinely started, and a log that only says so once
	// a revision lands would leave the one state an operator most needs to
	// recognise looking like a boot that hung.
	company := e.Company()
	name := "(unconfigured)"
	if company != nil {
		name = company.Config.Name
	}
	log.InfoContext(ctx, "engine_started", "company", name,
		"seats", len(company.Seats()), "configured", company != nil)
	// AND INTO THE AUDIT LOG, which had no line for it: the event store is
	// what an operator reads days later, and "when did this node last come
	// up" was answerable only from whatever kept the process's stdout.
	//
	// NOT ON AN UNCONFIGURED NODE. The event names the company, and a node
	// waiting for its first revision is not serving one — a line naming
	// "(unconfigured)" would be a company by that name in every listing
	// that groups on it.
	if company != nil {
		e.publishLifecycle(ctx, events.New(
			types.OrgStarted{OrgName: company.Config.Name}, tracing.TraceOf(ctx)))
	}
	return nil
}

// publishLifecycle puts one node's start or stop into the audit event log.
//
// BEST EFFORT AND UNATTRIBUTED TO ANY SEAT: no seat is running at either
// moment, so the envelope's source is this node — which is also what tells one
// member's line from another's on a fleet, since every member publishes its
// own pair.
func (e *Engine) publishLifecycle(ctx context.Context, ev *events.Event) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	if e.node != nil {
		ev.Source = e.node.ID()
	}
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "lifecycle_event_not_published", "type", ev.Type,
			"error", err.Error(),
			"detail", "the audit log has no line for this node's start or stop; "+
				"the engine log does")
	}
}

// Drain is the first half of a graceful stop: this node stops taking work,
// lets the turns already running finish, and hands its seats back. Every
// backend stays open; [Engine.Stop] is the half that closes them.
//
// SPLIT FROM STOP FOR THE HTTP SURFACE, which has to come down between the two
// halves and nowhere else. Not before the drain: /health and /ready are what an
// orchestrator reads while the turns finish, and a node that closed its
// listener first answered neither, so its liveness probe failed while it was
// doing exactly what it should. Not after the teardown: both probes read the
// broker and the coordination store the teardown closes.
//
// ONCE. A second call, and the drain [Engine.Stop] runs for a caller that did
// not drain first, both wait for the first to finish and then do nothing more,
// so one shutdown is announced once and no seat is handed back twice.
//
// Bounded only by ctx, for the reason [node.Node.Drain] gives.
func (e *Engine) Drain(ctx context.Context) {
	e.drainOnce.Do(func() {
		// BEFORE ANYTHING THAT CAN BLOCK, so every surface that asks
		// refuses new work from the moment the drain was decided rather
		// than from whenever the seat host reaches it: the announcement
		// below is a broker round trip, and the seat host's own flag is
		// not set until the drain gets there.
		e.shuttingDown.Store(true)
		// FIRST of what does block. The drain below and the teardown
		// after it reap MCP process trees, join goroutines and wait on
		// in-flight turns indefinitely: all legitimate, all slow, and all
		// of it would look to an armed watchdog exactly like the wedge it
		// exists to end. Exiting through the middle of a drain abandons
		// the seat release that makes it graceful, and costs every peer a
		// full TTL of dark seats.
		if e.watchdog != nil {
			e.watchdog.Stop()
		}
		// NO APPLY RUNS ON A NODE THAT IS STOPPING. The reconcile loop
		// can be mid-tick when the process is told to stop, and an apply
		// that went on past this point would start again what the drain
		// and the teardown after it end: the scheduler re-armed after its
		// loop was stopped and its duty given back, the background passes
		// handed to loops that are gone, and on a node's first company
		// the inbound edge started on a node that is leaving. So the
		// drain waits out an apply already running, which returns quickly
		// on the cancelled context that asked for the stop, and every
		// later one is refused.
		//
		// ON THE DRAIN rather than on the teardown, because the drain is
		// where a stop is decided: an apply admitted during it would
		// re-arm on a node that is already handing its seats back. It is
		// also the one gate that sees an apply arriving over the STREAM
		// from a peer, which the API's own drain gate never does.
		//
		// BEFORE the line below, so the company that line names is the
		// one this node actually stops on: past the gate no apply can
		// install another.
		e.applying.Lock()
		e.stopped = true
		e.applying.Unlock()
		// WHERE THE STOP IS DECIDED, rather than where it completes. The
		// broker stays open until the teardown, so the drain's far end
		// would carry this line just as well; what that end does not
		// survive is a second interrupt or a supervisor's kill grace
		// running out mid-drain, and the one thing the audit log must not
		// lose is that this node was told to stop.
		if company := e.Company(); company != nil {
			e.publishLifecycle(ctx, events.New(
				types.OrgStopped{OrgName: company.Config.Name}, tracing.TraceOf(ctx)))
		}
		e.node.Drain(ctx)
	})
}

// ShuttingDown reports whether a drain has begun.
//
// True from the first moment of [Engine.Drain] and never false again. It is
// what the HTTP surface refuses new work on and what /health and /ready
// report, and it is deliberately not the seat host's own draining flag, which
// is set later, once the drain has reached the seat host.
func (e *Engine) ShuttingDown() bool { return e.shuttingDown.Load() }

// Stop drains, unless [Engine.Drain] already has, and shuts down.
//
// The DRAIN comes first and is the difference between a restart that resumes
// cleanly and one that redelivers half-finished turns: it stops claiming, hands
// back every seat, and waits for in-flight handlers before anything closes.
func (e *Engine) Stop(ctx context.Context) {
	e.Drain(ctx)
	e.teardown(ctx)
	log.InfoContext(ctx, "engine_stopped")
}

// teardown stops everything a node started, in the one order that is correct.
//
// ONE IMPLEMENTATION FOR TWO CALLERS — [Engine.Stop] and [New]'s failure path
// — for the reason [native.shutdown] gives about its own two: a half-built
// engine leaks precisely what a built one does. The apply loops do not know
// their node never finished booting, a shared MCP server is a process tree
// holding the company's credentials whether or not a seat ever used it, and an
// admission left behind says "this process may be publishing" to a coordinator
// that is about to take a capacity exclusion. A second copy of this order is
// how one of them stops matching the other.
//
// What is NOT here is what only a RUNNING engine has to do: disarm the
// watchdog, announce its stop into the audit log, and drain. A boot that
// failed has no seats to hand back, published no start to pair a stop with,
// and never armed the timer.
//
// Nil-safe and never-started-safe throughout, because the boot path reaches it
// with any suffix of this list undone — each stop is a no-op where the thing
// it stops was never started, and the node is absent entirely where the
// failure came before [node.New].
func (e *Engine) teardown(ctx context.Context) {
	// After the drain: the waiter's keepalive is what stops a running box
	// being reaped, so stopping it first would start the orphan clock on
	// every in-flight run while turns are still finishing.
	e.stopSandbox()
	e.stopNotifications(ctx)
	e.stopMaintenance()
	e.stopRetention()
	e.stopBudgetReports()
	e.stopEmbedding()
	// AFTER THE DRAIN AND AFTER EVERY LOOP, which is what the admission
	// says: the key means "this process may be publishing", so withdrawing
	// it while a seat was still finishing a turn would tell a coordinator
	// the opposite of the truth.
	if err := e.withdraw(ctx); err != nil {
		log.WarnContext(ctx, "admission_not_withdrawn", "err", err,
			"detail", "a capacity operation will see this node as a possible "+
				"publisher until an operator excludes it")
	}
	e.stopIntegrations()
	// AFTER the integration loop, which is what runs the passes that ask
	// for a re-activation: disarming first would leave a window in which a
	// pass could arm one more on a node that is shutting down.
	e.republish.stop()
	// AFTER the drain, which released every seat and flushed each one's
	// memory on the way out. Stopping it before the drain would leave the
	// releases to the flush alone, which is the bounded path rather than
	// the whole one.
	e.stopMemorySync()
	// BEFORE backends.Close below, which closes the store the four passes
	// query. They tick on a detached context on purpose — like the node's
	// loops, they must not stop at SIGTERM — so nothing else ends them,
	// and a tick landing after the close would run compaction queries and
	// paid summarisation against a closed database.
	e.stopLearning()
	e.stopScheduler()
	// AFTER every duty loop above has stopped and waited out its tick, so no
	// tick of this node runs once a peer can take the duty.
	e.releaseDuties(ctx)
	e.stopCooldownRefresh()
	// BEFORE the native backends, whose page projection its walk reads, and
	// before backends.Close, which closes the broker its nudge listens on.
	e.stopSkillSync(ctx)
	// BEFORE backends.Close, for the same reason and with more at stake:
	// the projectors and the indexer both write, and an apply landing
	// after the close would fail its transaction mid-batch and leave the
	// cursor ahead of the rows it claims to describe.
	e.stopNative(ctx)
	if e.node != nil {
		e.node.Stop(ctx)
	}
	// AFTER the seats are released, so a per-role child is normally
	// already gone with its seat. This is the backstop for the ones that
	// were not: a stdio server is a process TREE holding a seat's
	// credentials, and one left behind outlives the engine that vouched
	// for it.
	e.stopSharedServers(ctx)
	if e.ownsBackends {
		e.backends.Close(ctx)
	}
}

// StallLag is how far behind the worst live watched duty is.
//
// The early-warning half of the watchdog: it fires at the lease TTL, and
// this is the number climbing towards it. Zero means nothing to report —
// either the duty is turning normally or nothing is being watched — which is
// the same reading a health surface wants for both.
func (e *Engine) StallLag() time.Duration {
	if e.watchdog == nil {
		return 0
	}
	return e.watchdog.Lag()
}

// Node exposes this process's participation in the fleet, for the operator
// surfaces that report which seats it holds.
func (e *Engine) Node() *node.Node { return e.node }

// Backends exposes the infrastructure this engine runs on.
//
// For the API `crewlet run` serves beside the engine, in the same process: the
// two share one broker and one store, and the API did not open them, so it
// needs a handle. A second set would be worse than inconvenient: two
// connections to one broker fail independently, and the store is exclusive to
// one process, so a second open is contention with itself.
func (e *Engine) Backends() *Backends { return e.backends }

// Recorder is the process's one metrics recorder, or nil.
//
// EXPOSED for the same reason [Engine.Backends] is: a subsystem the CLI
// composes beside the engine — the backup service is the one — measures
// something the catalogue declares, and a second recorder would be a second
// set of series for one process. Nil is legal and records nothing.
func (e *Engine) Recorder() *metrics.Recorder { return e.metrics }

// Dispatch delivers one inbox partition to a seat.
//
// This is the node's TurnFunc, exported because it is also the entry point for
// a delivery that did not come from the seat's own subscription — a sandbox
// run completing, which resumes a suspended Execute loop rather than waiting
// for the broker to hand the seat something.
func (e *Engine) Dispatch(ctx context.Context, handle string, evs []*events.Event) queue.Result {
	return e.dispatch.Dispatch(ctx, handle, evs)
}

// conditionsFor answers the ownership and posture questions for one seat.
func (e *Engine) conditionsFor(awaiting func(string) bool) func(string) inbox.Conditions {
	return func(handle string) inbox.Conditions {
		_, owned := e.node.Host().MayStart(handle)
		company := e.Company()
		return inbox.Conditions{
			// FRESHNESS, not membership: a renew at t proves exclusivity
			// through t+ttl, and a membership snapshot can be a full TTL
			// stale — which is exactly the window this check exists to
			// close.
			Owned: owned,
			// Read off the EPOCH rather than asserted true: a company
			// with no providers.llm is applied with no model registry,
			// and every delivery to its seats is held on the inbox until
			// an apply brings one. See nomodels.go.
			TurnEngineReady: company != nil && company.Models != nil,
			AwaitingSandbox: awaiting != nil && awaiting(handle),
			// The SAME gate the inbound edge and the scheduler read, so
			// a shedding node refuses at every trigger admission rather
			// than only at the one that happened to be wired. It was a
			// hardcoded true, which made a seat's own inbox the one
			// path a shed could never reach.
			AdmitsTriggers: e.admits(),
		}
	}
}

// seatFence is the per-round ownership check a turn on this seat runs under,
// or nil when this process holds no grant to check against.
//
// THE MISSING HALF OF THE ADMISSION. [Engine.conditionsFor] above asks
// [seat.Host.MayStart] once, when the delivery arrives, and a turn then runs
// for minutes past that answer — calling models, firing tools, posting to
// chat and writing to the tracker as a seat the placement sweep may have
// handed to a peer two sweeps ago. The tool loop has checked a fence at the
// top of every round since it was written; nothing ever supplied one, so the
// mechanism was inert and all three places that documented the protection
// described something that did not run. See [seat.Host.Fence] for why the
// check is on the EPOCH rather than on membership, and for the two states —
// a store blip and an unproven teardown — that deliberately do not close it.
func (e *Engine) seatFence(handle string) func() error {
	if e.node == nil {
		return nil
	}
	return e.node.Host().Fence(handle)
}

// park requeues a partition onto the seat's own inbox.
//
// Republished one at a time, and a failure part way through is reported: the
// dispatcher acks only on success, so a partial requeue comes back as a NAK
// and the whole partition is redelivered. Same-id copies left behind by the
// half that landed are collapsed by the dedupe at the top of the next
// screening, which is why that stage runs before any parking branch.
func (e *Engine) park(ctx context.Context, handle string, evs []*events.Event) error {
	subject := topics.AgentInbox(handle)
	if subject == "" {
		return fmt.Errorf("engine: seat %q has no inbox subject", handle)
	}
	for _, ev := range evs {
		if err := e.backends.Queue.Publish(ctx, subject, ev); err != nil {
			return fmt.Errorf("engine: requeue %s onto %s: %w", ev.Type, subject, err)
		}
	}
	return nil
}

// turnInputFor renders a dispatched [Request] as the loop's own [turn.Input].
//
// A NAMED MAPPING rather than a literal at the call site, because every field
// here is one the loop cannot derive for itself and degrades SILENTLY without:
// a missing Depth checks the delegation cap against a constant zero, and a
// missing Reply skips every delivery gate the turn has. Both were omitted from
// that literal at different times, and neither omission had a symptom — the
// cap bounded nothing, and a turn closed `done` having answered nobody on the
// surface it was asked from. A literal drops a field quietly; a function with
// a test does not.
func turnInputFor(req Request, reply turn.Reply) turn.Input {
	return turn.Input{TurnID: req.WorkKey, Depth: req.Depth, Reply: reply}
}

// runTurn is the default turn: build the seat's runner and drive the loop.
func (e *Engine) runTurn(ctx context.Context, req Request) (turn.Result, error) {
	// THE TURN'S OWN SPAN, opened before anything else so every phase, LLM
	// round and tool call below is a child of it and every event the turn
	// publishes carries its id. The dispatcher has already restored the
	// trigger's trace onto ctx, so this is a child of the wake rather than
	// a root.
	ctx, span := tracing.Start(ctx, "engine", "agent.turn",
		attribute.String("crewlet.seat", req.Handle),
		attribute.String("crewlet.work_key", req.WorkKey),
		attribute.Bool("crewlet.coalesced", req.Coalesce),
		attribute.Int("crewlet.delegation_depth", req.Depth))
	defer span.End()

	// PINNED ONCE. Two reads of the epoch can straddle an apply, and a turn
	// that built its runner from one revision and took its round caps from
	// the next is running a company that never existed — the exact failure
	// publishing-instead-of-mutating exists to remove.
	company := e.Company()
	// Assembled BEFORE the runner, because the runner needs it: every phase
	// event carries the turn's identity, and a runner built without it
	// publishes phases attributed to nobody.
	tel := e.describeTurn(ctx, company, req)
	// THE ASK, not the partition: a coalesced conversation reaches the model
	// as ONE merged digest rather than as its constituents concatenated —
	// see [Request.Trigger] and internal/engine/coalesce.go.
	task := DescribeTrigger(req.Ask())
	// RENDERED BEFORE THE RUNNER, which is what freezes it: the runner
	// receives strings and has nowhere to re-fetch from, so a self_iterate
	// loop cannot move the system prompt underneath the executor. Nothing
	// is re-fetched mid-turn any more: the one case that needed it — a
	// thin trigger whose turn-start search was skipped — is served by the
	// executor calling search_knowledge over the same seam, on a query it
	// writes once it knows what the task needs.
	blocks := e.prefetchFor(ctx, company, req, task)
	// The skills OFFERED to this turn, carried onto its completion so the
	// curator ages a skill on when it was last put in front of a model
	// rather than archiving the ones a seat reads every turn.
	tel.skills = blocks.SkillIDs

	reply := ReplyFor(req.Events)
	turnIdentity := tel.runnerTurn(company, req.WorkKey, req.Depth, req.DelegationChain,
		task, reply)
	r, err := company.RunnerFor(req.Handle, e.seatRegistry(company, req.Handle), RunnerInput{
		Task:    task,
		Context: blocks,
		Skills:  e.skills,
		Reply:   reply,
		// BOUNDED AT RENDER, never at write. The stored row is the only copy
		// of the turn; what a prompt shows is a display decision, and this
		// one drops whole entries oldest-first and says how many.
		Conversation: ledger.RenderHistory(req.History, ledger.HistoryOptions{
			MaxChars: ledger.InjectedMaxChars,
		}),
		Publisher: e.backends.Queue,
		Turn:      turnIdentity,
		// The executor's runtime, from the seat's own provider chain —
		// see [Engine.agentRunFor].
		AgentRun: e.agentRunFor(company, req.Handle, turnIdentity.Context),
		Markers:  e.markers(),
		Latch:    e.onboarded,
		// Read off the PINNED epoch, so a revision that raises a ceiling
		// mid-turn cannot move the limit a round is judged against.
		Budget: e.meterFor(company, req.Handle),
		// The round-cap extension judge, from the same pinned epoch. It
		// was never supplied, so every exhaustion rescued with "no_judge"
		// and the extension mechanism was inert.
		Judge: e.judgeFor(company, req.Handle),
		// The seat's headroom, for a sub-agent spawn. Three-valued on
		// purpose: nil is "no ceiling configured", and a FAILED read
		// refuses the spawn rather than granting it no ceiling.
		Remaining: e.remainingFor(company, req.Handle),
		// Built HERE, at turn start, because the epoch it closes on is
		// the grant THIS turn was admitted under — a fence built later
		// would compare a re-claim against itself.
		Fence: e.seatFence(req.Handle),
	})
	if err != nil {
		// No turn-completed event: nothing started, so nothing ended. A
		// seat whose runner could not be built never published a started
		// phase either, so there is no live row to close — and publishing a
		// completion for a turn that never ran would put a failed turn in
		// the record of a seat that did not take one.
		return turn.Result{}, err
	}

	// BEFORE Plan, on its own budget. A seat's first ever turn used to run
	// onboarding inside Plan and could spend the whole plan budget reading
	// pages — the turn most likely to produce no plan at all was the one
	// where a seat had never planned before.
	//
	// A failure here does NOT fail the turn: the seat is un-onboarded, which
	// is the state it was already in, and refusing to work over it would
	// make a knowledge base that is briefly unreachable stop the company.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if ran, err := r.Onboard(ctx); err != nil {
		log.WarnContext(ctx, "onboarding_pass_failed", "handle", req.Handle,
			"error", err, "detail", "the seat stays un-onboarded and retries "+
				"next turn; the turn continues")
	} else if ran {
		log.InfoContext(ctx, "onboarding_pass_ran", "handle", req.Handle)
	}

	res, err := turn.Run(ctx, r, company.TurnSettings(req.TimeoutSeconds),
		turnInputFor(req, reply))
	// The moment the turn returns, and before its frame unwinds: the runner
	// holds the suspended conversation only until then, and a row without
	// one is a detached run nothing can ever resume.
	if res.Suspended {
		e.persistSuspension(ctx, r, req.WorkKey)
	}
	// Published on BOTH paths. An error here means a phase broke, which is
	// precisely when a dashboard most needs the turn closed: the phase
	// events already put the seat into `working`, and returning without this
	// leaves it there until the seat happens to take another turn.
	e.publishTurnCompleted(ctx, tel, req.WorkKey, r.Spend(), res, err)
	// AND, if a colleague asked for this turn, the answer they are waiting
	// for. Here because this is the one frame holding both the result and
	// the trigger; after the completion event because the reply wakes
	// another seat and the record of this turn should exist first.
	e.answerColleague(ctx, company, req, res)
	return res, err
}

// nodeStatus is what this node advertises about itself on every heartbeat.
//
// Read LIVE, per beat, rather than cached: a cached in-flight count is a
// node reporting work it finished a minute ago, and the whole value of
// carrying this fleet-wide is that an operator can see where the work
// actually is right now.
func (e *Engine) nodeStatus(ctx context.Context) coord.NodeStatus {
	status := coord.NodeStatus{StartedAt: e.startedAt}
	if b := e.backends; b != nil && b.Queue != nil {
		status.InFlight = b.Queue.InFlightCount()
	}
	if n := e.node; n != nil {
		if host := n.Host(); host != nil {
			status.Draining = host.Draining()
		}
	}
	// The native replication loops — the wiki's projector and every
	// state-log domain's applier — so an operator asking why a fresh node
	// holds no seats can see the answer in the fleet view rather than
	// inferring it from an empty claim list. See
	// [coord.NodeStatus.ProjectionsReady] for why this is not a readiness
	// signal.
	for _, loop := range e.NativeStatus(ctx) {
		status.ProjectionsTotal++
		if loop.Ready {
			status.ProjectionsReady++
		}
	}
	if fn := e.posture.Load(); fn != nil {
		posture := (*fn)(ctx)
		// A posture read that ran out of the beat's budget did not
		// answer — what came back is the reporter's fail-open default,
		// which is "serve". Publishing that would put a confident
		// healthy posture on a node whose control plane is unreadable,
		// so an unanswered read says nothing at all.
		if ctx.Err() == nil {
			status.Posture = posture
		}
	}
	return status
}

// StartedAt is when this engine was built, which is when the node started.
//
// ONE INSTANT FOR EVERY SURFACE. The API served in this process reports it as
// the node's `started_at`, and the presence heartbeat carries it to every peer's
// fleet view, so the node's own health and the fleet's picture of it cannot
// disagree about how long it has been up.
func (e *Engine) StartedAt() time.Time { return e.startedAt }

// SetPosture supplies the config-lag reporter the presence heartbeat
// advertises.
//
// # A setter rather than an option
//
// The posture belongs to the reconciler, which is built AFTER the engine —
// it needs the engine to converge against. So there is no moment at
// construction when a real wiring could pass one, and an option nobody could
// fill would be a second way in that only tests use.
//
// Safe to call while the engine is running: the heartbeat reads it per beat,
// so a posture supplied late starts appearing within one interval rather
// than at the next restart.
//
// The context is the beat's, already bounded — see [seat.Config].Status.
func (e *Engine) SetPosture(fn func(context.Context) string) { e.posture.Store(&fn) }

// SetAdmits supplies the gate that decides whether this node takes new
// inbound work.
//
// # Why this is a setter, and why it is not the same wire as SetPosture
//
// A setter for the reason SetPosture is one: the posture belongs to the
// reconciler, which is built AFTER the engine because it needs an engine to
// converge against. It was an OPTION, which meant nothing could ever fill it
// — the one production call site of [New] runs before the reconciler exists —
// so the gate was nil on every node, [notify.Service] admitted every
// delivery, the scheduler fired on every tick, and a node that had decided it
// was shedding kept taking work. [configplane.Posture.ServesTraffic] was
// written for this gate and had no caller at all.
//
// Separate from the posture reporter because the two are read on different
// paths at different rates: the reporter is called per heartbeat and pays a
// live control-plane read, while this is called per delivery and must not.
// See [Reconciler.Admits] for which value it answers from.
//
// Nil admits everything, which is the single-node case and the case before a
// control plane exists. A shedding node DEFERS a delivery — back to the
// broker, consumer quiesced — rather than routing it against a company it is
// not sure of.
func (e *Engine) SetAdmits(fn func() bool) {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	e.notify.admits = fn
}

// SetOnApplied registers a hook run after every config apply publishes its
// epoch, for surfaces that render the COMPANY rather than its activity.
//
// The dashboard's roster, org tree and tool catalogue are all derived from
// configuration, so nothing that happens afterwards will correct them: a
// revision that adds a role, renames one, or removes one changes all three
// and produces no event a projection could learn from. Without this, an open
// dashboard kept rendering the company it connected to until someone
// reloaded the page — and the client cannot paper over a deletion either,
// because an overlay merge has no way to express a row going away.
//
// A SETTER for the same reason SetPosture is one: the API half is built after
// the engine, so there is no moment at construction when a real wiring could
// pass one.
//
// Safe to call while the engine is running, and safe to leave unset — an
// engine with no hook applies exactly as before.
func (e *Engine) SetOnApplied(fn func(context.Context)) { e.onApplied.Store(&fn) }

// notifyApplied runs the hook, if one is set.
func (e *Engine) notifyApplied(ctx context.Context) {
	if fn := e.onApplied.Load(); fn != nil && *fn != nil {
		(*fn)(ctx)
	}
}

// OtelReceiver is this node's sandbox telemetry receiver, or nil when
// CREWLET_SANDBOX_OTEL_RECEIVER_URL is unset.
//
// Handed to the API this process serves, so the route verifies with the
// receiver the engine mints against rather than with a second one built from a
// key nobody shares.
func (e *Engine) OtelReceiver() *sandbox.OtelReceiver { return e.sandboxOtel }

// Bridge is this node's MCP tool bridge, or nil.
//
// Exposed for the same reason the receiver is: the API this process serves
// mounts the route, and it is handed this engine's own: one object, so a
// session opened by a run is the session the route resolves.
func (e *Engine) Bridge() *mcpbridge.Bridge { return e.bridge }

// keyMaterial is the Tier A keyring, as the OTLP token key is derived from.
//
// THE REFERENCES ARE NOT RESOLVED HERE, and must not be: this runs before the
// secret store is open, and the store's own key is what would resolve them.
// Two processes reading the same document derive the same key either way —
// what matters is that they agree, not that the material is the plaintext.
func keyMaterial(boot *config.Bootstrap) []string {
	if boot == nil {
		return nil
	}
	out := make([]string, 0, len(boot.Secrets.Keys))
	for _, key := range boot.Secrets.Keys {
		out = append(out, key.ID+":"+key.Material)
	}
	return out
}

// observe publishes an engine-side observability event.
//
// BEST EFFORT, and loudly so: the caller is doing real work and this is the
// feed's record of it. A publish that fails is logged and swallowed, because
// failing the work to keep a row would be the wrong trade in every case this
// is used for.
func (e *Engine) observe(ctx context.Context, ev *events.Event) {
	if e.backends == nil || e.backends.Queue == nil || ev == nil {
		return
	}
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "engine_observation_dropped", "event", ev.Type,
			"error", err.Error(),
			"detail", "the work itself is unaffected; the feed will not show it")
	}
}
