package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
)

// Engine is one process running a company.
//
// It owns exactly three things — the epoch, the backends, and the node — and
// the wiring between them. Everything else it delegates: the guard order is
// the inbox package's, the turn's rules are the turn package's, the seat math
// is the placement package's. That is the whole reason this file is short and
// the Python it replaces was seven and a half thousand lines.
type Engine struct {
	// epoch is the company this engine is running, replaced whole by an
	// apply and never mutated. See epoch.go.
	epoch    epoch
	backends *Backends
	node     *node.Node

	// startedAt is when THIS engine started, which on a split deployment
	// is a different process on a different clock from the API's own
	// start. Carried on the presence heartbeat so a peer can tell a node
	// that has been up for a week from one that restarted a minute ago.
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
	// queue on exactly the topology — merged API and engine — that shares
	// one broker.
	ownsBackends bool

	// dispatch turns one inbox partition into one turn. Held so a test can
	// substitute its own without standing up a broker.
	dispatch *Dispatcher

	// sandbox is this node's code-work machinery: the coordinator holding
	// the busy set, the waiter polling detached runs, and the durable row
	// store behind both. On the ENGINE rather than on an epoch, because
	// they are facts about this process — rebuilding them on an apply would
	// forget which seats are mid-run and start a second poll loop against
	// the same rows.
	sandboxCoordinator *sandbox.Coordinator
	sandboxWaiter      *sandbox.Waiter
	sandboxPending     sandbox.PendingStore

	// sandboxOtel mints each coding run's telemetry endpoint. Nil exports
	// nothing from inside a box, which is the ordinary configuration.
	//
	// Held here and handed OUT to the API rather than built twice: in a
	// split deployment the API verifies tokens this process minted, and
	// two receivers would sign with two per-process keys unless a keyring
	// happens to be configured — which is exactly the case that must not
	// depend on happening to be configured.
	sandboxOtel *sandbox.OtelReceiver

	// reflector is the learning write side: one dispatcher for the life of
	// the process, whose org and workers an apply swaps. On the ENGINE
	// rather than on an epoch because its redelivery ring is process
	// state — see learning.Reflector.
	reflector *learning.Reflector

	// env is this node's ${VAR} resolver: the secret store in front of the
	// process environment, refreshed on every apply. One per node rather
	// than one per call site — see secrets.go for why that matters.
	env atomic.Pointer[*config.Resolver]

	// cipher is the keyring this node seals and opens secret rows with,
	// nil on a node that has none. Held rather than rebuilt because ONE
	// cipher per process is what keeps a row this node wrote a row it can
	// read back.
	cipher secrets.Cipher

	// profile is what this node declared it does: whether it claims
	// seats, serves inbound traffic, and runs the fleet's singleton
	// duties. Held because the duty gate reads it on every claim — see
	// duty.go for why the lease alone is not the whole answer.
	profile placement.NodeProfile

	// learning is the two background passes no turn drives: episode
	// compaction and skill ageing. On the ENGINE for the same reason the
	// sandbox waiter is — they are loops this PROCESS runs, and rebuilding
	// them on an apply would start a second one against the same rows.
	learning *learning.Background

	// notify is this node's inbound edge: the party registry, the
	// notification service and the vendor transports. On the ENGINE
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

	// mcp supervises every MCP child this NODE runs — the company's shared
	// servers and the per-role children of the seats it holds. One per
	// engine rather than one per epoch: the children are processes, and an
	// apply that replaced the bridge would orphan every one of them.
	mcp *mcp.Bridge

	// seatMCP is one bridge per seat this node holds, for that seat's
	// per-role children. Separate from the shared bridge above because a
	// bridge's catalogue is keyed by tool name across its servers, and two
	// seats' children of one template publish the same names.
	mcpMu   sync.Mutex
	seatMCP map[string]*mcp.Bridge

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
	maintenance *maintenance.Worker

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

	// OtelReceiver mints the per-run OTLP endpoints a sandbox exports to.
	// Nil builds one from the environment; see
	// [sandbox.BuildOtelReceiver].
	OtelReceiver *sandbox.OtelReceiver

	// Backends may be supplied by a caller that already opened them — the
	// API process and the engine share one broker when they run merged.
	// Nil opens them from the bootstrap config, and the engine then owns
	// their lifetime.
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

	// Admits gates INBOUND work on the config posture, supplied by
	// whoever holds the control plane — the engine does not, because the
	// posture is a fleet question and the reconciler owns it.
	//
	// Nil admits everything, which is the single-node case and the case
	// before a control plane exists. A shedding node PARKS a delivery
	// rather than routing it against a company it is not sure of.
	Admits func() bool

	// SandboxPollInterval overrides the completion poll's cadence. Zero
	// takes the production value, which is sized against coding jobs that
	// run for minutes; a test shrinks it so a run settles in a second
	// rather than waiting out a real tick.
	SandboxPollInterval time.Duration
}

// pauseReasonNoTurnEngine is the hold name the no-turn-engine park takes on a
// seat's inbox.
//
// A STABLE KEY, not the screening's prose. Pause holds are keyed by reason so
// two subsystems gating one inbox cannot release each other's hold, which
// means the pause and the eventual resume must spell it identically. Deriving
// it from the human-readable reason would make an edit to a log message
// silently strand every seat that was parked under the old wording.
const pauseReasonNoTurnEngine = "no_turn_engine"

// New assembles an engine.
//
// Config errors surface HERE, before anything is dialled or claimed. A node
// that boots on a bad config and discovers it at the first turn has already
// told its peers it owns seats.
func New(ctx context.Context, opts Options) (*Engine, error) {
	if opts.Bootstrap == nil {
		return nil, fmt.Errorf("engine: no bootstrap config")
	}

	backends := opts.Backends
	ownsBackends := false
	if backends == nil {
		opened, err := OpenBackends(ctx, opts.Bootstrap, opts.Company)
		if err != nil {
			return nil, err
		}
		backends = opened
		ownsBackends = true
	}
	// Only what this engine OPENED does it close. A caller that supplied
	// backends keeps their lifetime — the merged API process outlives the
	// engine's own shutdown and still needs its broker.
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

	e := &Engine{
		backends: backends, ownsBackends: ownsBackends,
		onboarded: runner.NewLatch(), skills: skills.NewRegistry(),
		mcp:         mcp.NewBridge(nil),
		sandboxOtel: otel,
		startedAt:   time.Now().UTC(),
	}
	fail := func(err error) (*Engine, error) {
		if ownsBackends {
			backends.Close(ctx)
		}
		return nil, err
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
		return fail(err)
	}
	e.cipher = cipher
	// MIGRATED BEFORE THE SNAPSHOT, so a value set on this node while the
	// engine was stopped is on the fleet before anything resolves it —
	// and, once it is, so are this node's peers. See [Engine.migrateSecrets].
	e.migrateSecrets(ctx)
	e.refreshSecrets(ctx)

	company, err := NewCompanyWith(opts.Company, e.resolver())
	if err != nil {
		return fail(err)
	}
	// BEFORE equip, because equip registers run_sandbox and only a node
	// with a coordinator can offer it: a tool whose dependency is absent is
	// OMITTED rather than registered-and-broken, so an engine that equipped
	// first would build a code-enabled company whose seats have no code
	// tool and plan around one anyway.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.buildSandboxRuntime(company); err != nil {
		return fail(fmt.Errorf("engine: sandbox: %w", err))
	}
	// EQUIPPED BEFORE PUBLISHED. A turn can start the instant the epoch is
	// current, and one that found an empty registry would run a seat with
	// no tools at all — a company that boots cleanly and can do nothing.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.equip(ctx, company); err != nil {
		return fail(err)
	}
	e.epoch.current.Store(company)

	nodeID, err := config.ResolveNodeID(opts.Bootstrap, nil)
	if err != nil {
		return fail(fmt.Errorf("engine: node identity: %w", err))
	}
	// SET BEFORE the node, because the node is handed this exact value —
	// two constructions of it would be two places to disagree about what
	// this node does.
	// THROUGH THE BOOTSTRAP'S OWN ACCESSOR, not a second construction of
	// the same thing: it parses the roles with the validator that already
	// refused an unknown one at load, and it is what the fleet view reads
	// a peer's presence row back through.
	e.profile = opts.Bootstrap.Node.Profile(nodeID)
	n, err := node.New(node.Config{
		Queue: backends.Queue,
		Coord: backends.Coord,
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
		Seats:   func() []placement.Seat { return e.Company().Seats() },
		Profile: e.profile,
		// WHAT THIS NODE IS DOING, advertised to peers on every
		// heartbeat. Only the node running a seat knows its in-flight
		// count and its drain state, and /health answers about whichever
		// node served the request — so behind a load balancer a refresh
		// tells a different story each time. See
		// decisions/501-node-runtime.md.
		Status: e.nodeStatus,
		Turn:   e.Dispatch,
		// Before the mailbox opens and after it closes — see node.Config.
		// A seat mid-detached-run must have its runs recovered and its mail
		// parked before anything can deliver to it.
		SeatReady: func(ctx context.Context, handle string, lease coord.Lease) error {
			return e.prepareSeat(ctx, handle, lease.Epoch, lease.Owner)
		},
		SeatDone: e.releaseSeat,
		LeaseTTL: leaseTTL(opts.Bootstrap),
		// The host's own ceiling, from Tier A. Per NODE, so a fleet's is
		// N times this. Passed through unresolved: zero is the shape of an
		// absent key and node.New is what turns it into the default, so
		// the number lives in one place — see node.DefaultMaxConcurrent.
		MaxConcurrent: opts.Bootstrap.Node.MaxConcurrent,
	})
	if err != nil {
		return fail(fmt.Errorf("engine: node: %w", err))
	}
	e.node = n
	e.dispatch = e.buildDispatcher(opts, backends)
	// LAST, because its fleet-singleton duty is claimed under the node's
	// own incarnation.
	if err := e.startSandboxWaiter(ctx, opts.SandboxPollInterval); err != nil {
		return fail(fmt.Errorf("engine: sandbox waiter: %w", err))
	}
	e.startMaintenance(ctx)
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
		return fail(err)
	}
	// The two background passes, after the node exists: both are fleet
	// singletons claimed under its own incarnation.
	e.startLearningBackground(ctx)
	e.notify.admits = opts.Admits
	if err := e.startNotifications(ctx, e.Company()); err != nil {
		return fail(fmt.Errorf("engine: %w", err))
	}
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
	if d.Park == nil {
		d.Park = e.park
	}
	if d.Pause == nil {
		d.Pause = e.pause
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
	if backends.Fleet != nil && d.Completions == nil {
		d.Completions = ledgerstore.NewFleetCompletions(backends.Fleet)
	}
	// Nil-checked rather than assumed: a caller-supplied Backends may
	// carry no store, and the dispatcher already documents nil as the
	// single-node case where the seat lease is the whole mutual exclusion.
	if backends.Store != nil && d.Conversations == nil {
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
	company := e.Company()
	log.Info("engine_started", "company", company.Config.Name,
		"seats", len(company.Seats()))
	return nil
}

// Stop drains and shuts down.
//
// The DRAIN comes first and is the difference between a restart that resumes
// cleanly and one that redelivers half-finished turns: it stops claiming, hands
// back every seat, and waits for in-flight handlers before anything closes.
func (e *Engine) Stop(ctx context.Context) {
	e.node.Drain(ctx)
	// After the drain: the waiter's keepalive is what stops a running box
	// being reaped, so stopping it first would start the orphan clock on
	// every in-flight run while turns are still finishing.
	e.stopSandbox()
	e.stopNotifications(ctx)
	e.stopMaintenance()
	e.stopScheduler()
	e.stopCooldownRefresh()
	e.node.Stop(ctx)
	// AFTER the seats are released, so a per-role child is normally
	// already gone with its seat. This is the backstop for the ones that
	// were not: a stdio server is a process TREE holding a seat's
	// credentials, and one left behind outlives the engine that vouched
	// for it.
	e.stopSharedServers(ctx)
	if e.ownsBackends {
		e.backends.Close(ctx)
	}
	log.Info("engine_stopped")
}

// Node exposes this process's participation in the fleet, for the operator
// surfaces that report which seats it holds.
func (e *Engine) Node() *node.Node { return e.node }

// Backends exposes the infrastructure this engine runs on.
//
// For the MERGED topology, where one process is both engine and API: the two
// halves share one broker and one store, and the half that did not open them
// needs a handle. A second set would be worse than inconvenient — two
// connections to one broker fail independently, and the store is exclusive to
// one process, so a second open is contention with itself.
func (e *Engine) Backends() *Backends { return e.backends }

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
		return inbox.Conditions{
			// FRESHNESS, not membership: a renew at t proves exclusivity
			// through t+ttl, and a membership snapshot can be a full TTL
			// stale — which is exactly the window this check exists to
			// close.
			Owned: owned,
			// Read off the EPOCH rather than asserted true. NewCompany
			// refuses a company with no models today, so this cannot be
			// false yet; stating the actual rule means it stops being
			// true on its own when the config-apply path can hand a node
			// an epoch that has none, instead of a constant quietly
			// outliving the reason for it.
			TurnEngineReady: e.Company().Models != nil,
			AwaitingSandbox: awaiting != nil && awaiting(handle),
			AdmitsTriggers:  true,
		}
	}
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

// pause stops delivery on a seat's inbox before a park, so the requeued copies
// buffer on the queue rather than looping straight back.
func (e *Engine) pause(ctx context.Context, handle, reason string) error {
	subject, group := topics.AgentInbox(handle), topics.AgentInboxGroup(handle)
	if subject == "" || group == "" {
		return fmt.Errorf("engine: seat %q has no inbox subject", handle)
	}
	log.Info("seat_inbox_paused", "handle", handle, "reason", reason)
	return e.backends.Queue.PauseTopic(ctx, subject, group, pauseReasonNoTurnEngine)
}

// runTurn is the default turn: build the seat's runner and drive the loop.
func (e *Engine) runTurn(ctx context.Context, req Request) (turn.Result, error) {
	// PINNED ONCE. Two reads of the epoch can straddle an apply, and a turn
	// that built its runner from one revision and took its round caps from
	// the next is running a company that never existed — the exact failure
	// publishing-instead-of-mutating exists to remove (d-404).
	company := e.Company()
	// Assembled BEFORE the runner, because the runner needs it: every phase
	// event carries the turn's identity, and a runner built without it
	// publishes phases attributed to nobody.
	tel := e.describeTurn(company, req)
	task := DescribeTrigger(req.Events)
	// RENDERED BEFORE THE RUNNER, which is what freezes it: the runner
	// receives strings and has nowhere to re-fetch from, so a self_iterate
	// loop cannot move the system prompt underneath the planner. The one
	// fetch that cannot be frozen — the knowledge search a thin trigger's
	// gate skipped — rides the Recon seam below, keyed on a plan summary
	// that does not exist until Plan has run.
	prefetchReq, blocks := e.prefetchFor(ctx, company, req, task)
	// The skills OFFERED to this turn, carried onto its completion so the
	// curator ages a skill on when it was last put in front of a model
	// rather than archiving the ones a seat reads every turn.
	tel.skills = blocks.SkillIDs
	fetcher := e.prefetcher(company)

	r, err := company.RunnerFor(req.Handle, RunnerInput{
		Task:    task,
		Context: blocks,
		Skills:  e.skills,
		Recon: func(ctx context.Context, planSummary string) string {
			return fetcher.AfterPlan(ctx, prefetchReq, planSummary)
		},
		Conversation: ledger.RenderHistory(req.History, ledger.HistoryOptions{}),
		Publisher:    e.backends.Queue,
		Turn:         tel.runnerTurn(company, req.WorkKey, req.Depth),
		Markers:      e.markers(),
		Latch:        e.onboarded,
		// Read off the PINNED epoch, so a revision that raises a ceiling
		// mid-turn cannot move the limit a round is judged against.
		Budget: e.meterFor(company, req.Handle),
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

	res, err := turn.Run(ctx, r, company.TurnSettings(), turn.Input{TurnID: req.WorkKey})
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

// StartedAt is when this engine started.
//
// Its own accessor rather than a field on some larger snapshot: on a split
// deployment the API is a different process on a different clock, and a
// merged uptime would report one number for two windows.
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

// OtelReceiver is this node's sandbox telemetry receiver, or nil.
//
// Handed to the API so a merged process mounts the route the engine mints
// against, and so a SPLIT one is visibly missing it rather than answering 401
// with a key nobody shares.
func (e *Engine) OtelReceiver() *sandbox.OtelReceiver { return e.sandboxOtel }

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
		log.Warn("engine_observation_dropped", "event", ev.Type,
			"error", err.Error(),
			"detail", "the work itself is unaffected; the feed will not show it")
	}
}
