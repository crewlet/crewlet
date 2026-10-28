package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerfit"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/skillsync"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Engine is one process running a company.
//
// It owns exactly three things — the epoch, the backends, and the node — and
// the wiring between them. Everything else it delegates: the guard order is
// the inbox package's, the turn's rules are the turn package's, the seat math
// is the placement package's. That is the whole reason this file is short.
type Engine struct {
	// epoch is the company this engine is running, replaced whole by an
	// apply and never mutated — but for its tool registry's shared MCP
	// tools, which the shared bridge's own supervision refiles when a
	// server comes back with a different catalogue (mcpsupervise.go). See
	// epoch.go.
	epoch epoch

	// directoryNudge carries the identity applier's post-commit signal to
	// the party registry's rebuild loop. ONE SLOT, which is the coalescing
	// window — see [Engine.nudgeDirectory].
	directoryNudge chan struct{}

	backends *Backends
	node     *node.Node

	// githubIssuer holds the installation tokens this process has issued
	// its agents' own GitHub Apps, one per installation and scope, in
	// memory only. ONE for every reader of a seat's token (its tool
	// servers, its coding runs, the poll that keeps a long run's token
	// current and the inbound participant lookup), so an installation
	// costs one mint an hour however many ask, and a refusal one request
	// per wait of the seats' restart schedule ([Engine.seatRetryDelay],
	// set in [New]). See githubtoken.go.
	githubIssuer github.Issuer
	// tokenWaits is what this node's polls have said of the running jobs
	// whose seat's app issues no token, so each wait is said once per job
	// rather than on every pass. See githubtoken.go.
	tokenWaits tokenWaits

	// authEvents is this node's authentication audit trail, and
	// stopAuthTrail ends its loop. See authevents.go.
	authEvents    *authevents.Trail
	stopAuthTrail func()

	// externalBase is where a PERSON reaches this DEPLOYMENT:
	// `api.external_url`, without its trailing slash — the base every link
	// somebody clicks is composed on ([Engine.publicBase]). webhookBase is
	// where a VENDOR reaches it ([config.API.WebhookBase]): the public
	// listener's own address once `api.public` is set, since every webhook
	// route is served only there and the person's address answers 404 for
	// it, and `api.external_url` otherwise. Every registration, manifest
	// and teardown at a vendor is composed on the second; a link a person
	// follows never is. webhookBaseKey is the Tier A key the second is read
	// from ([config.API.WebhookBaseKey]), which a pass with no address names
	// as the setting to fill in — read beside it from the same block, so the
	// two cannot name different listeners.
	//
	// ON THE ENGINE AND NOT ON AN EPOCH, which is the whole of what
	// moving them to Tier A bought. They used to be Tier B pointers resolved
	// per epoch through the secret resolver, so every consumer had to
	// carry a resolver, handle the state where the reference read back as
	// its own seven characters, and re-derive the answer on each apply.
	// They are now facts about the process: fixed at boot, restart to
	// change, and the same on every path that composes a URL.
	externalBase   string
	webhookBase    string
	webhookBaseKey string

	// adminPosture is this node's own `api.auth.admin`, published on every
	// heartbeat — and empty on a node that serves no API, which has no
	// posture to state. Tier A, so it is read once at boot: the whole
	// reason a fleet compares these is that a rolling restart is how the
	// values come to differ, and a value that could change inside a
	// process would make the comparison meaningless. See
	// [coord.NodeStatus.AdminPosture].
	adminPosture iam.AdminPosture

	// tierATokens are the ids this node's `api.auth.tokens` declares — the
	// holders of every `token:<id>` login this node can authenticate — for
	// [Engine.HolderRecord], which answers a login no entry declares as
	// nobody's. Tier A, so read once at boot like the posture beside it.
	tierATokens map[string]struct{}

	// identity is the three identity duties' loops, or nil on a node that
	// arms none of them. See identityduties.go. ATOMIC because `/health`
	// reads the roster from the socket's health tick while a stop clears
	// it: a plain pointer there was a data race the race detector caught
	// on a node torn down under an open dashboard.
	identity atomic.Pointer[identityDuties]

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

	// objectives is the node's service objective meter, which every
	// artifacts recording site counts its events into and the alarm table
	// reads the burn rates from: ONE per process, so a burn is judged over
	// every event this node measured. Never nil on an engine [New] built.
	objectives *objectives.Meter

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

	// keyMint keeps two goroutines of this process from minting one company
	// key at once, and personBlinds is the identity estate's blinder once
	// resolved. See companykeys.go.
	keyMint      mintGate
	personBlinds personBlinds

	// startedAt is when THIS engine was built, which is when the node
	// started: the API this process serves runs inside it and reports this
	// same instant. Carried on the presence heartbeat so a peer can tell a
	// node that has been up for a week from one that restarted a minute ago.
	startedAt time.Time

	// posture reports this node's config lag, from whoever owns the
	// control plane. Nil publishes none — see [Engine.SetPosture].
	posture atomic.Pointer[func(context.Context) string]

	// onPublished is run after each published company is converged on, for
	// the surfaces derived from the company itself. Nil runs nothing — see
	// [Engine.SetOnCompanyPublished].
	onPublished atomic.Pointer[func(context.Context)]

	// onCatalogue is run after the shared bridge's own supervision refiled
	// the current epoch's catalogue. Nil runs nothing — see
	// [Engine.SetOnCatalogueChanged].
	onCatalogue atomic.Pointer[func(context.Context)]

	// onInbox is told whose inbox each committed tracker batch moved on
	// this node. Nil tells nobody — see [Engine.SetOnInboxMoved].
	onInbox atomic.Pointer[func([]worklet.InboxMovement)]

	// onInboxGap is told that the movements onInbox hears have a gap. Nil
	// tells nobody — see [Engine.SetOnInboxGap].
	onInboxGap atomic.Pointer[func()]

	// inboxRelay is this data node's inbox relay — the batches its applier
	// announced, for a peer whose copy is out of service, and the follower
	// that hears a peer's while this node's own copy is — and
	// stopInboxRelayFn ends both. Nil on a node without `data`. See
	// inboxrelay.go.
	inboxRelay       atomic.Pointer[inboxRelay]
	stopInboxRelayFn func(context.Context)

	// onIdentity is told whose credentials each committed identity batch
	// moved on this node. Nil tells nobody — see
	// [Engine.SetOnIdentityMoved].
	onIdentity atomic.Pointer[func(iamdomain.Moved)]

	// artifactsListeners is told what each committed artifacts batch moved
	// on this node; see [Engine.OnArtifactsMoved].
	artifactsListeners moveFanout[artifactsdomain.Moved]

	// artifactDataListeners is told what each committed datastore batch
	// moved on this node; see [Engine.OnArtifactDataMoved].
	artifactDataListeners moveFanout[artifactdata.Moved]

	// dataFootprint signals the datastore footprint sampler that a batch
	// committed ([Engine.runDataFootprint]). Made by [Engine.startCore]
	// before any applier runs; nil on a node without the estate, where
	// nothing sends.
	dataFootprint chan struct{}

	// assistantErasure signals the assistant's erasure loop that the
	// identity applier removed somebody ([Engine.runAssistantErasure]).
	// Made by [Engine.startCore], like dataFootprint.
	assistantErasure chan struct{}

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

	// budgetParks is every seat inbox this node holds because a capped
	// token window is refusing, each with the alarm that releases it when
	// the window turns over. See budgetpark.go.
	budgetParks budgetParks

	// clock is the instant every token-budget window this engine cuts is
	// read at, and nil is the wall clock — which is what every engine [New]
	// builds runs on. See [Engine.now] for why there is one and why it is
	// not an [Options] field.
	clock func() time.Time

	// pauses is this node's watched copy of every seat a person paused,
	// and the inbox holds it took because of them. See seatpause.go.
	pauses seatPauses

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

	// stopBudget is the allowance every lease this node's stop gives back
	// shares ([seat.StopBudget]) — the drain's and the teardown's — built by
	// whichever reaches it first ([Engine.stopping]). Written once, inside
	// stopBudgetOnce, and read only through it.
	stopBudget     *seat.StopBudget
	stopBudgetOnce sync.Once

	// teardownBegun is done from the moment the TEARDOWN begins
	// ([Engine.tearingDown]) — never the drain, whose wait for running turns
	// the integration loop runs straight through — and is what a give-back
	// no stop's context reaches watches to become a step of the stop
	// ([Engine.stepOnceTornDown]). Made on first use under teardownOnce, so
	// the zero engine a failed boot tears down has one too.
	teardownOnce  sync.Once
	teardownBegun context.Context
	beginTeardown context.CancelFunc

	// stopOnce does the same for the teardown [Engine.Stop] runs after the
	// drain. A second teardown is not a harmless repeat: every step in it
	// runs against what the first one closed, so a second Stop warned that
	// the admission could not be withdrawn — telling an operator to
	// exclude a node whose admission the first Stop had already removed —
	// and announced `engine_stopped` twice for one shutdown.
	stopOnce sync.Once

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
	// the busy set and the durable row store behind it. On the ENGINE
	// rather than on an epoch, because they are facts about this process —
	// rebuilding them on an apply would forget which seats are mid-run.
	//
	// AN ATOMIC POINTER, published whole and at most once — nil until the
	// first company that reaches a sandbox cell, at boot or at an apply,
	// never cleared — for the reason [Engine.native] is one: a node that
	// booted with no sandbox has a seat host, a dispatcher and a tool loop
	// reading it before an apply brings it up. Every reader loads it once.
	// See [Engine.startSandbox].
	sandbox atomic.Pointer[sandboxRuntime]

	// sandboxWaiter is the fleet's completion poll — the boxes every node
	// reaches, under the waiter duty — and sandboxHostWaiter this node's own,
	// for the boxes on its host ([sandbox.Drives]). Each started once, on a
	// node that publishes, the first time a runtime exists and the node does.
	sandboxWaiter     atomic.Pointer[sandbox.Waiter]
	sandboxHostWaiter atomic.Pointer[sandbox.Waiter]

	// sandboxPollInterval is Options.SandboxPollInterval, kept because an
	// apply can be what starts the poll.
	sandboxPollInterval time.Duration

	// fakePlay is Options.fakePlay: what a `providers.sandbox.fake`
	// company's runs do, nil for the double's own demonstration. Kept
	// because every apply builds the catalogue again.
	fakePlay fakePlayer

	// sandboxSeats are the seats whose sandbox control topic this node has
	// attached, so a seat is attached exactly once however it came to be
	// prepared — by its own acquisition, or by the apply that brought the
	// runtime up while it was already held. See [Engine.prepareHeldSeats].
	sandboxSeatsMu sync.Mutex
	sandboxSeats   map[string]bool

	// leaseTTL is the seat lease TTL, which is also the seat lease bucket's
	// own age, resolved once from Tier A.
	//
	// Held because it is a CEILING for every SEAT claim this engine makes
	// outside the seat host: the KV's expiry is bucket-wide, so it refuses a
	// seat lease asked to outlive it, and the mailbox retirement claims a
	// removed seat's lease with exactly this TTL. Nothing but a seat or a
	// presence lease is bounded by it (coord.HeldToSeatTTL): a duty and the
	// tracker's walk claims live in a bucket whose ceiling is
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

	// fill is the loop that fills the vectors the memory of the seats this
	// node holds is missing — see memoryfill.go — and the handle Stop uses
	// to end it. Nil on a node with no store or no seat host.
	fill *memoryFill

	// sandboxOtel mints each coding run's telemetry endpoint. Nil exports
	// nothing from inside a box, which is the ordinary configuration.
	//
	// Held here and handed OUT to the API this process serves rather than
	// built twice: that API verifies the tokens this engine minted, and one
	// object is the only arrangement that cannot drift from its twin. A
	// peer verifies them with its own receiver, which is why the key is
	// derived from the fleet's keyring rather than held.
	sandboxOtel *sandbox.OtelReceiver

	// bridge serves a running seat's tool surface to its executor's coding
	// agent over MCP. Nil where neither a bridge URL is configured nor this
	// node serves the bridge's route itself — which leaves every executor
	// launch on this node refused, naming the variable, so a node that runs
	// seats needs one. leaveBridgeFleet withdraws it from the fleet it
	// joined — see bridgefleet.go.
	bridge           *mcpbridge.Bridge
	leaveBridgeFleet queue.Unsubscribe

	// sessions is where every phase the engine drives serves its tools to
	// the CLI that runs it, on this host: this PROCESS's one
	// [toolsession.Host], a directory only the engine's user can open,
	// holding one socket per CLI run, which the CLI's `crewlet mcp-relay`
	// dials. Every turn's runner is handed it ([RunnerInput.Sessions]). The
	// executor's run is the one phase that does not use it: it is in a box,
	// and reaches the seat's tools over the bridge above.
	//
	// ON THE ENGINE, made once by [New] and removed by the teardown after
	// every turn has ended, because it is a fact about the process: a
	// directory per turn would be a directory per turn to clean up, and one
	// made on first use would have no owner to remove it. Never nil on an
	// engine [New] built. Nothing here binds a port — a node on `api.port:
	// 0` serves its phases' tools all the same.
	sessions *toolsession.Host

	// reflector is the learning write side: one dispatcher for the life of
	// the process, whose org and workers an apply swaps. On the ENGINE
	// rather than on an epoch because its redelivery ring is process
	// state — see learning.Reflector. ATOMIC because every seat this node
	// holds hands its reflection wakes to it from the queue's goroutines,
	// while the first apply on a node that booted with none builds it.
	reflector atomic.Pointer[learning.Reflector]

	// core is this node's always-on runtime: the state log for every
	// registered domain, the node gate, and the identity estate's two
	// sides. See core.go.
	//
	// PUBLISHED ONCE, by [New], on EVERY DATA NODE — with a company or
	// without, in every mode — and never cleared, not even by the teardown.
	// Nil for the life of a node without `data`, which holds no estate for
	// a log to run over and reaches it through [Engine.router]. On the
	// ENGINE rather than on an epoch, because the runtime follows the
	// fleet's LOGS and a log does not change when a company revision does.
	// Atomic because readers on every goroutine load it without a lock, and
	// the monotonic nil-to-runtime transition is what lets a caller that
	// reads it more than once rely on the later reads.
	core atomic.Pointer[core]

	// native is the half of this node's durable state only a company can
	// say anything about: the tracker's and the knowledge base's read and
	// write sides over the core's logs, where the company runs them
	// natively, the lexical index and the change feeds. Nil on a node that
	// has not yet been handed a company, and on a node without `data`,
	// which runs [Engine.remote] instead — see native.go.
	//
	// ATOMIC, AND WRITTEN AT MOST ONCE: nil until the node's first company
	// is met — at boot, or by the apply that brings a node that booted with
	// none its first ([Engine.startNativeFor]) — and never cleared after,
	// not even by the teardown. A plain field was correct only while boot
	// was the one writer, before any goroutine could read it; a node that
	// booted unconfigured and met its company later wrote it with the seat
	// host, the API and every tool already reading. The monotonic
	// nil-to-runtime transition is what lets a caller that reads it more
	// than once rely on the later reads.
	native atomic.Pointer[native]

	// remote is the same thing on a node that holds no data: which halves
	// its seats' tools reach through the router. At most one of the two is
	// ever set, and on the same terms — see remote.go.
	remote atomic.Pointer[remoteNative]

	// router is how every seat tool on this node reaches the estate — this
	// node's own copy while it is in service, another data node's otherwise
	// — built in [New] on EVERY node, before anything publishes; see
	// router.go. local is this data node's own copy, nil on a node that
	// holds no data.
	router *estate.Router
	local  *localEstate

	// envelopes sign every estate request this node sends and every reply
	// it answers a peer with, and verify both directions — one value for
	// the router and the server alike; see [estateEnvelopes].
	envelopes *estate.Envelopes

	// routedDirectory is the party registry's directory trigger on a node
	// without `data`, which reads the identity directory through a data
	// node — nil on a data node, whose core runs its own. See
	// [Engine.startRoutedDirectory].
	routedDirectory *directoryWatch

	// custody publishes a stateless node's events for a data node to
	// keep; nil on a node that holds data. keeper is a data node's half,
	// writing them into its own log; nil on every other node.
	custody *observe.Custody
	keeper  *observe.Keeper

	// stopEstate withdraws this data node as a server of the estate, nil
	// where it serves none. See [Engine.serveEstate].
	stopEstate queue.Unsubscribe

	// stopFleetBroker withdraws this member's broker-membership subject,
	// nil where it serves none. See [Engine.serveFleetBroker].
	stopFleetBroker queue.Unsubscribe

	// brokerWire signs every broker-membership request this node sends and
	// every reply it answers a peer with, and verifies both directions —
	// built in [New] on EVERY node, since any node asks; see
	// [newBrokerWire].
	brokerWire *brokerWire

	// dataView watches the fleet's presence leases, and it is what every
	// per-request question about data nodes is answered from — which one
	// a stateless node's tool call goes to, and which divide a search.
	// Nil where there is no coordination and so no fleet. Built in [New],
	// run from [Engine.startDataView] and stopped by [Engine.teardown];
	// see remote.go.
	dataView     *coord.LeaseView
	stopDataView func()

	// objects is this node's object store and, on a data node, the
	// collector's duty. See objects.go.
	objects *objectStore

	// uploads is this node's artifact upload and publish pipeline, built on
	// first use. See artifactupload.go.
	uploads artifactUploads

	// boot is the operator's Tier A configuration this engine was built
	// from. Immutable; kept because the duties an apply or a later loop
	// arms read Tier A — the trim's retention policy, the identity
	// estate's horizons, the backup owner.
	boot *config.Bootstrap

	// env is this node's ${VAR} resolver and the store snapshot it answers
	// from: the secret store in front of the process environment, refreshed
	// on every apply and after a provisioning pass seals a credential. One
	// per node rather than one per call site — see secrets.go for why that
	// matters.
	//
	// ONE POINTER TO BOTH, so a reader never takes a resolver from one
	// snapshot beside another's values.
	env atomic.Pointer[secretView]

	// secretsMu serialises every writer of env — an apply's refresh and a
	// provisioning pass's — so the store is read under the same lock the
	// snapshot is installed under: whichever runs later read the store
	// later, and neither installs a snapshot older than the other's.
	secretsMu sync.Mutex

	// environ is the environment every resolver this node builds ends in —
	// [Options.Environment], the process's own when that is nil. Read
	// through [Engine.environment], which answers the process's for an
	// engine built without one.
	environ config.Source

	// republish coalesces the re-activations a provisioning pass asks for
	// when it seals a credential. See republish.go.
	republish republisher

	// cipher is the keyring this node seals and opens secret rows with.
	// [New] refuses a node without one, so it is nil only on an Engine
	// built by hand in a test. Held rather than rebuilt because ONE cipher
	// per process is what keeps a row this node wrote a row it can read
	// back.
	cipher secrets.Cipher

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

	// mcpFileMu serialises filing the shared bridge's catalogue into an
	// epoch's registry outside an apply's own build: what the bridge's
	// supervision changed — a retried start that came up, a restart with
	// different tools — into the current epoch, and the bridge as it stands
	// into an epoch being published (mcpsupervise.go). Its own lock rather
	// than applying: the bridge calls that refile from its supervisor, which
	// an apply's Stop waits for.
	//
	// mcpRetryBase is the restart schedule's first wait for every bridge
	// this engine builds and for its seats' runners, zero for
	// [mcp.RestartBase]; [Options.mcpRetryBase].
	mcpFileMu    sync.Mutex
	mcpRetryBase time.Duration

	// seatMCP is one bridge per seat this node holds, for that seat's
	// per-role children, with what keeps them converged while the seat is
	// held (seatservers.go). Separate from the shared bridge above because a
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
	//
	// mcpShared and mcpSeats are what each start of a server concluded —
	// the shared servers per apply, the per-role children per pass over a
	// seat — for the heartbeat's [coord.NodeStatus.MCP]. Under mcpMu too,
	// because a seat's outcomes are replaced and forgotten with its bridge.
	mcpMu     sync.Mutex
	seatMCP   map[string]*seatChildren
	seatTools map[string]*tools.Registry
	mcpShared []mcpOutcome
	mcpSeats  map[string][]mcpOutcome

	// hooks caches the webhook verification material for the company it
	// was assembled from. See [Engine.WebhookSecrets] for why it is keyed
	// on the company's identity and why it is lazy.
	hooks struct {
		mu       sync.Mutex
		built    *Company
		material webhooks.Secrets
	}

	// embeddings is the company's vector backend, swapped on apply. An
	// atomic pointer rather than a mutex because it is read on the turn's
	// prefetch hot path and written only by an apply: the read must not
	// queue behind anything, and there is nothing else to hold a lock
	// across.
	embeddings atomic.Pointer[embeddings.Embedder]

	// maintenance is the retention sweep for the short-horizon tables. On
	// the engine for the same reason the sandbox machinery is: it is a
	// loop this process runs, and rebuilding it on every apply would churn
	// a duty that has nothing new to sweep.
	//
	// REBUILT ONCE MORE on a node's FIRST company, the one time its job
	// list changes after boot: a node that booted unconfigured built it
	// with no native halves and no company, so it swept no inbox and ran
	// none of the tracker's repairs. Its horizons need no rebuild — each is
	// asked at every sweep. The old worker is stopped — its in-flight tick
	// waited out — before the new one starts, so there is never a second
	// loop. Atomic because that write happens while the process runs. See
	// [Engine.rebuildMaintenance].
	maintenance  atomic.Pointer[maintenance.Worker]
	integrations *integration.Worker

	// mailboxes registers each seat's mailbox with the fleet and retires
	// the mailbox of a seat that has left the company. Built BEFORE the
	// node, which registers through it, and swept by the maintenance duty.
	// Nil on an engine with no fleet store, where nothing is registered
	// and nothing retired.
	mailboxes *maintenance.Mailboxes

	// following is held by a reconciler tick from its read of the pointer to
	// its record of what this node now serves ([Reconciler.Tick]), so one
	// engine follows the pointer one tick at a time.
	//
	// ON THE ENGINE, not the reconciler, because what it orders is the
	// engine's: two ticks interleaved, each reading the pointer and then
	// applying, let the one that read the OLDER epoch apply last, and the
	// boot row — moved only to what the engine just applied — walk back to
	// it. Production builds one reconciler per engine; a test harness that
	// builds a second ([Engine.NewReconciler] does not refuse one) is held
	// to the same order rather than trusted not to.
	following sync.Mutex

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
	//
	// ATOMIC because it is armed after the node exists, from the boot's
	// publishing tail — while the API may already be reading it for
	// `crewlet retention status` — and cleared by the teardown.
	retention atomic.Pointer[retention]

	// alarms is the retention loop's alarm tracker, published for the API's
	// health envelope ([Engine.Alarms]). ATOMIC and never cleared, because it
	// is read from every health request and push tick on goroutines this
	// engine does not own, while the loop that writes it starts and stops
	// on the engine's own: a plain field read against startRetention's write
	// is a data race, and nil-ing it on a stop would turn the last reading a
	// draining node took into "no evaluation has ever run".
	alarms atomic.Pointer[statelog.Tracker]

	// budgetReports is the live token-meter loop. Every node runs one —
	// the counters are shared, so this is a frame rather than a duty.
	budgetReports *budgetReporter

	// refusals is how this node's gates tell that loop a window has started
	// refusing ([refusalLatch]). Built with the engine rather than with the
	// loop, because the gates charge through it from the first apply and
	// the loop starts and stops beside them.
	refusals *refusalLatch

	// embedding is the vector domain's one writer: the fleet singleton
	// that turns sources whose text has moved into vector records. On the
	// ENGINE for the reason the trim is — it is a loop this process runs,
	// and rebuilding it on an apply would leave two loops holding one
	// company's provider budget. It reads the current epoch's embedder per
	// tick instead, so a model change lands without a restart.
	embedding *embedDuty

	// trashReaper is the trash reaper's duty loop (trashreap.go). On the
	// ENGINE for the embedding duty's reason: a loop this process runs
	// once, which an apply must not build a second of.
	trashReaper trashReaper

	// usage is this node's usage publisher: the loop that republishes the
	// days this node's own event log holds. On the ENGINE for the embedding
	// duty's reason — a second loop after an apply would be two writers on
	// this node's own subjects — and it reads the current epoch's clock and
	// chart per tick instead.
	usage *usageLoop

	// usageEvery is Options.usageEvery.
	usageEvery time.Duration

	// history is the fleet's turn-level history reader, and
	// stopHistoryServe withdraws this node as one of its answerers. See
	// history.go.
	history          *eventfan.Fleet
	stopHistoryServe queue.Unsubscribe

	// steers is this node's running turns' note boxes, and stopSteerServe
	// withdraws the node as their answerer. See steer.go.
	steers         *steerDesk
	stopSteerServe queue.Unsubscribe

	// presenceWire seals this node's presence frames and opens its peers',
	// and presence is the hub that holds its rooms. See presence.go.
	presenceWire *presence.Wire
	presence     *presence.Hub

	// answers seals a person's answer onto the parked run it answers, and
	// opens it before a resume reads it: the coordinator's, built here over
	// the fleet keyring like every other instruction channel
	// ([sandbox.CoordinatorOptions.Answers]).
	answers *sandbox.AnswerSeal

	// dataTurns is this node's tally of each turn segment's datastore ops,
	// shared by every registry it builds ([builtin.DataTurns]).
	dataTurns *builtin.DataTurns

	// folderTurns is this node's tally of the folder records each agent
	// turn segment writes, shared by every registry it builds
	// ([builtin.FolderTurns]).
	folderTurns *builtin.FolderTurns

	// onArtifactsServing is told whenever this node's artifact copies leave
	// or return to service, and stopArtifactWatchFn ends the watcher that
	// tells it. See presence.go.
	onArtifactsServing  atomic.Pointer[func(bool)]
	stopArtifactWatchFn func()

	// memoryReads answers a seat's memory from the node holding it, and
	// stopMemoryServe withdraws this node as one of its answerers. See
	// memoryread.go.
	memoryReads     *memread.Reader
	stopMemoryServe queue.Unsubscribe

	// sandboxTails answers a running coding run's live output from the node
	// that owns it, sandboxFeeds is this node's readings of the runs it owns,
	// and stopTailServe withdraws this node as one of its answerers. See
	// sandboxtail.go.
	sandboxTails  *sandbox.TailReader
	sandboxFeeds  *sandbox.LiveFeeds
	stopTailServe queue.Unsubscribe

	// boxReads asks a box's host to read it back and answers for the boxes
	// on this node's host; stopBoxReadServe and stopNudgeListen withdraw this
	// node as an answerer of those reads and a listener for the nudges its
	// waiters act on. Nil on a node with no broker. See sandboxhost.go.
	boxReads         *boxReads
	stopBoxReadServe queue.Unsubscribe
	stopNudgeListen  queue.Unsubscribe

	// scheduler is the role/unit cron tick. On the ENGINE rather than on an
	// epoch for the same reason maintenance is: it is a loop this process
	// runs, and rebuilding it on an apply would leave two loops racing for
	// one company's fires. reconcileScheduler arms and disarms it instead.
	scheduler schedulerLoop

	// onboarded remembers which seats this PROCESS has seen onboarded.
	//
	// On the engine rather than on an epoch, because it is a fact about
	// what this process has observed and not about the company: an apply
	// publishes a new epoch, and a latch that came with it would forget
	// every seat and re-run a pass for agents already marked. It is keyed
	// by chain hash, so a live restructure still re-onboards by design.
	onboarded *runner.Latch

	// rewrites is the cache every compaction this process makes is kept in
	// — see [Engine.compactorFor].
	//
	// On the engine rather than on an epoch for the latch's reason: a
	// rewrite is a function of the text it was made from, so it stays
	// right across an apply that did not touch it, and a turn running
	// across that apply re-renders its ledger against the same payloads.
	rewrites *compact.Cache

	// auxSpend is this node's auxiliary-spend ledger: every auxiliary
	// completion the seam sees, coalesced and published as
	// `auxiliary_spend` — see auxiliary.go and internal/auxspend.
	//
	// On the engine for the cache's reason, and more strongly: a bucket
	// is spend already made, and a ledger that came with an epoch would
	// drop whatever the apply that replaced it had not flushed.
	auxSpend *auxspend.Ledger
}

// Options configure an engine.
type Options struct {
	Bootstrap *config.Bootstrap
	Company   *config.Company

	// ActivatedAt is when Company was activated: the instant of the
	// activation this node last applied it from, which its store keeps on
	// the revision it boots on, when that is what Company is.
	//
	// ZERO FOR A COMPANY NO ACTIVATION HAS NAMED — one a caller built in
	// code, or an offline command's choice the fleet has not taken yet
	// (an offer: see [store.Offer]). `crewlet run` boots on the revision
	// its store says it boots on, or on nothing, and a Tier B file reaches
	// the engine through an activation, applied with the pointer's own
	// instant before a seat is claimed. An offer boots with none because no
	// activation has named it: stamped with an offline instant, the
	// replicated estate would carry a document the fleet may never run, at
	// an instant later than the fleet's, and refuse the fleet's own stamps.
	// The first apply of the activation that publishes it stamps it. It is
	// not the boot's clock, because what reads it
	// is the chart apply, which stamps every project with it so that an
	// older configuration cannot walk a newer one back — and a boot on a
	// stale revision stamped with NOW is exactly the older configuration
	// that would. See [Engine.applyChart].
	ActivatedAt time.Time

	// Mode is what this node starts for: normal, or one of the two
	// maintenance modes a capacity change restarts the fleet into.
	//
	// A RUN'S PROPERTY rather than the deployment's, so it is never read
	// from Tier A: every node enters and leaves it three times over one
	// change, and a file carrying it would leave a node that came back
	// after an unrelated restart still refusing to publish, with the
	// reason in a file nobody re-read. Empty is normal.
	Mode statelog.MaintenanceMode

	// Recovered is called once, from New, at the boundary between the two
	// halves of a boot — and never on a boot that fails before it.
	//
	// WHAT COMES BEFORE IT IS THE NODE'S OWN: its store and its migrations,
	// the broker (the embedded one's file-store recovery, a leaf's link, a
	// member's cluster), the streams and buckets it provisions, the custody
	// batches its own event log left unsettled, the replicated estate, the
	// state logs it provisions and a join's snapshot. All of it takes as
	// long as what the node holds is large, so the DEPLOYMENT bounds it — a
	// startup probe sized for the store it gave the node — and nothing in
	// this process does: a recovery cannot be cut short without losing what
	// it recovers. APPLYING those logs is not part of it: their appliers
	// catch up in the background from here on, and what waits for them is a
	// seat's acquisition ([Engine.StateLogHydrated]), never this call.
	//
	// WHAT COMES AFTER IT IS THE COMPANY'S: the snapshot of its secrets its
	// ${VAR} references resolve from ([Engine.bootSecrets]), the epoch built
	// from its document, the sandboxes it configures, the native halves it
	// runs, equipping its seats' tools — the shared MCP servers among them,
	// each bounded by its own `startup_timeout_seconds` — and restoring its
	// coding CLIs' credential bundles. None of that may hold a startup probe,
	// since a probe sized for a store's recovery knows nothing about a
	// company's secrets, tool servers or logins, so a caller answering probes
	// (internal/api's Boot) treats this call as the moment the node is alive
	// past anything only a dead disk could hold for ever.
	//
	// Called on every boot that gets that far, a node with no company and a
	// node started in a maintenance mode included, since each recovers what
	// it holds before deciding what else to start. Nil calls nothing.
	Recovered func()

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
	// the environment and BridgeHostURL; see [mcpbridge.Build].
	Bridge *mcpbridge.Bridge

	// BridgeHostURL is where a box on this node's own host — the `direct`
	// cell — reaches the bridge: the node's own listener that serves the
	// bridge's route, on loopback ([mcpbridge.Options.HostURL]). The caller
	// that bound that listener derives it, and only on a node that serves
	// the API: a node without the ingress role mounts no bridge route, and
	// its seats' boxes reach their tools through an ingress node's relay at
	// [mcpbridge.BaseURLVar]'s address. Empty there, and wherever the node
	// binds no listener.
	BridgeHostURL string

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

	// SandboxRuns reports what a seat's detached runs mean for its inbox:
	// whether one HOLDS the seat — a job outlasting any broker ack window,
	// so the seat's mail is parked — and whether one is waiting for a
	// person's answer, which any delivery might be. Nil reads this engine's
	// own coordinator, live — no to both until one exists, which is correct
	// for a node that has never run code: a seat that cannot start a
	// detached run is never in either state.
	//
	// ONE SEAM FOR BOTH, because the two move together: a run that parks on
	// a question stops holding its seat in the same moment it starts
	// awaiting an answer, and two seams could be read either side of that.
	SandboxRuns func(handle string) (held, awaitsAnswer bool)

	// SandboxPollInterval overrides the completion poll's cadence. Zero
	// takes the production value, which is sized against coding jobs that
	// run for minutes; a test shrinks it so a run settles in a second
	// rather than waiting out a real tick.
	SandboxPollInterval time.Duration

	// Environment is what a company's `${VAR}` references fall back to
	// behind the secret store, and what the per-run endpoints' settings are
	// read from ([sandbox.BuildOtelReceiver], [mcpbridge.Build]). Nil is
	// the process environment, which is what `crewlet run` leaves it.
	//
	// A SOURCE RATHER THAN THE PROCESS'S OWN for the reason the org model
	// takes a lookup ([org.HumanContact.ResolvedIdentities]): the
	// environment is the one genuinely ambient input, and an ambient input
	// serialises every test that sets it — Go forbids t.Setenv beside
	// t.Parallel — while two engines in one process can each be handed
	// their own. Tier A is not resolved through it: the bootstrap is the
	// root of trust, resolved from the process it was loaded in, by the
	// loader and [OpenBackends] alike.
	Environment config.Source

	// usageEvery is the usage publisher's cadence
	// ([usage.PublisherDeps.Every]), zero for the domain's own
	// [usage.FlushInterval]. UNEXPORTED, so nothing outside this package's
	// tests can set it (export_test.go): the cadence is the domain's
	// decision rather than a deployment's, and the knob exists only so a
	// test can watch a LIVE loop publish what arrives after boot without
	// waiting out fifteen seconds of it.
	usageEvery time.Duration

	// mcpRetryBase is the first wait of the MCP restart schedule
	// ([mcp.RestartWait]) — before a start that failed is tried again, a
	// shared server's by its bridge and a seat's per-role child's by the
	// seat's runner, and before a child that died soon after starting is
	// restarted — zero for [mcp.RestartBase], and the rest of the schedule
	// scales with it. UNEXPORTED for the reason usageEvery is: the schedule
	// is tied to the seat heartbeat rather than to a deployment, and the
	// knob exists only so a case can watch a retry bring a server up
	// without waiting fifteen seconds for it.
	mcpRetryBase time.Duration

	// fakePlay scripts what a `providers.sandbox.fake` company's runs do
	// ([sandbox.FakeRunner.Player]); nil plays each as the double's own
	// demonstration ([demoRun]). UNEXPORTED for the reason usageEvery is:
	// what a fake run does is the double's answer in a deployment, and the
	// knob exists only so a case can drive a whole turn — the run's bridged
	// calls included — through the engine it boots.
	fakePlay fakePlayer

	// sweepEvery is the seat host's placement cadence
	// ([node.Config.SweepInterval]), zero for [seat.SweepInterval].
	// UNEXPORTED for the reason usageEvery is: the cadence is the seat
	// host's decision, and the knob exists only so a case can put the next
	// tick out of reach and see what claims a seat without one.
	sweepEvery time.Duration
}

// New assembles an engine.
//
// Config errors surface HERE, before anything is dialled or claimed. A node
// that boots on a bad config and discovers it at the first turn has already
// told its peers it owns seats.
//
// The error is NAMED for the one deferred failure path below, which is what
// adds a lost broker connection's cause to whatever step failed.
func New(ctx context.Context, opts Options) (_ *Engine, err error) {
	if opts.Bootstrap == nil {
		return nil, fmt.Errorf("engine: no bootstrap config")
	}
	// TIER A'S OWN RULES, HELD HERE AND NOT ONLY AT THE LOADER. The loader
	// validates what it parses, which covers a file and nothing else: a
	// bootstrap built in code, or a loaded one edited afterwards (`crewlet
	// run`'s flag overrides), reaches this function having passed no rule
	// at all, and every rule it breaks fails later as something that looks
	// like a different problem. The e2e fleet harness is the measured case:
	// it ran its members on `coordination.type: local` over a clustered
	// stream — a shape [config.Bootstrap.Validate] refuses by name — so each
	// member claimed every seat and every duty for itself and saw no peer's
	// presence, and nothing said so: every member ran as a fleet of one, so
	// nothing it did looked wrong from inside it. The engine is the one
	// consumer that RUNS Tier A, so it is the one place that cannot assume
	// its input was checked.
	//
	// AMONG THEM A STREAM THAT SURVIVES A RESTART ([config.Stream.Durable]),
	// before anything is opened and whether or not this node has a company:
	// the core runtime puts every domain's log on a member's stream at boot,
	// so an embedded member held in memory is a node whose first restart
	// leaves it unable to serve for good. It used to be a cross-tier rule
	// asked only of a company, which let through exactly the node started
	// with none. And the keyring, which every node needs below.
	//
	// It NORMALIZES in place, which is the point rather than a side effect:
	// what the rules check is what this engine then runs. A bootstrap that is
	// already normal is written nowhere, so a caller holding the same pointer
	// sees no write.
	if err = opts.Bootstrap.Validate(); err != nil {
		return nil, fmt.Errorf("engine: the bootstrap config (Tier A) is invalid: %w", err)
	}
	// THE KEYRING FIRST, because everything below derives from it — the
	// per-run token issuers, the state logs' signatures, the secret store
	// and the company document a peer fetches — and because building it
	// opens nothing, so a node without one (or with a broken one) is
	// refused before there is anything to unwind. It used to be built
	// after the backends were open, and it used to answer nil for a node
	// with none.
	cipher, err := openCipher(opts.Bootstrap)
	if err != nil {
		return nil, err
	}
	// AND THE ESTATE'S ENVELOPES OVER IT, for the same reason and in the same
	// place: every request this node's router sends is signed with them and
	// every reply it is sent verified, and a data node answers its peers
	// under the same ones ([estateEnvelopes]).
	envelopes, err := estateEnvelopes(opts.Bootstrap)
	if err != nil {
		return nil, err
	}
	// AND EVERY OTHER CHANNEL A PEER CARRIES OUT A REQUEST ON, the same way:
	// the broker's membership, which a member changes through its own system
	// account, and a note to a running turn, which the running node offers
	// as the person who sent it (internal/signedask).
	wire, err := newBrokerWire(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	steers, err := newSteerDesk(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// AND THE PRESENCE CHANNEL, which carries no instruction and is sealed
	// all the same: a frame names a person (presence.go).
	presenceWire, err := newPresenceWire(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// AND A PERSON'S ANSWER TO A PARKED RUN, which waits on the run's row in
	// the coordination store and is handed to the resumed run as their
	// words: sealed there, like every other instruction (internal/sandbox,
	// answerseal.go).
	answers, err := sandbox.NewAnswerSeal(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// THE COMPLETION POLL'S CADENCE, whether or not this node will ever run a
	// sandbox: an apply can bring it its first, and the poll starts there.
	// See [checkSandboxPollInterval].
	if err = checkSandboxPollInterval(opts.SandboxPollInterval); err != nil {
		return nil, err
	}

	// EVERYTHING THAT CAN FAIL WITH NOTHING OPEN COMES FIRST, which is why
	// the telemetry receiver, the bridge and this node's identity are
	// resolved above the backends rather than below them. It is the same
	// ordering [Engine.startCore] uses on its own node-id read, for the
	// same reason: a failure that has opened nothing has nothing to unwind.
	// Placed below the backends, each of these returned past an ALREADY
	// OPEN store and broker and closed neither — and the store is
	// exclusive to one process, so the next attempt in the same process
	// contended with the corpse of this one.
	//
	// BUILT BEFORE THE SANDBOX RUNTIME, because the manager takes it: a
	// receiver constructed after the first apply would leave every run
	// launched in between exporting nowhere, silently.
	environ := opts.Environment
	if environ == nil {
		environ = config.EnvSource{}
	}
	otel := opts.OtelReceiver
	if otel == nil {
		otel, err = sandbox.BuildOtelReceiver(getenv(environ),
			tokenMaterial(opts.Bootstrap))
		if err != nil {
			// A receiver URL that is set and unusable is a deployment
			// that asked for in-box telemetry and would get none, so it
			// is refused rather than dropped: the alternative is a
			// config that looks complete and exports nothing.
			return nil, fmt.Errorf("engine: sandbox telemetry: %w", err)
		}
	}
	// SAME KEY MATERIAL, DIFFERENT DOMAIN, and for the same reason the
	// receiver above is built here: a fleet mints on one node and may
	// verify on another, so every node derives its key from the fleet's
	// keyring — which the keyring check above has already required.
	bridge := opts.Bridge
	if bridge == nil {
		if bridge, err = mcpbridge.Build(getenv(environ), tokenMaterial(opts.Bootstrap),
			opts.BridgeHostURL); err != nil {
			return nil, fmt.Errorf("engine: tool bridge: %w", err)
		}
	}

	// THE MODE AND THE INCARNATION, resolved once. An unset mode is
	// normal, which is what every caller that does not know about
	// maintenance gets; the incarnation is minted here rather than at the
	// lease, because the capacity barrier compares it and a second mint
	// would be a second identity for one process. It is the owner of every
	// lease this process holds for itself — the seat host's presence, seats
	// and duties (node.New below) alike.
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

	// THIS PROCESS'S TOOL SESSIONS, the last of what can fail with nothing
	// open: every phase that offers tools serves them to its CLI through a
	// socket in this directory, and a TMPDIR long enough to put that socket
	// past the platform's bound on a unix socket's path is refused HERE,
	// naming TMPDIR, rather than at every seat's first phase as a bind error
	// naming no setting.
	//
	// IT IS A DIRECTORY, so it is the one thing above that has something to
	// remove. Removed by this guard until the engine's own failure path is
	// armed below and takes it over — one deferred step rather than a close
	// at each return in between, for the reason that path gives.
	var sessions *toolsession.Host
	if sessions, err = toolsession.NewHost(); err != nil {
		return nil, fmt.Errorf("engine: the directory every seat's tools are served "+
			"to its CLI from: %w", err)
	}
	sessionsHanded := false
	defer func() {
		if !sessionsHanded {
			_ = sessions.Close()
		}
	}()

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
	// THE REPLICATED ESTATE THIS NODE HOLDS, opened before anything can
	// read or copy it — lent backends included, since holding is a fact
	// about the node rather than about whoever opened its store.
	if err = holdEstate(ctx, backends.Store, opts.Bootstrap); err != nil {
		if ownsBackends {
			backends.Close(ctx)
		}
		return nil, err
	}

	e := &Engine{
		boot:     opts.Bootstrap,
		environ:  environ,
		refusals: newRefusalLatch(),
		// SET HERE, BEFORE ANYTHING READS IT, and once: the admission
		// handshake below asks it whether this node publishes and whether
		// its broker is a member, and the node is handed this exact value
		// — two constructions of it would be two places to disagree about
		// what this node does. Through the bootstrap's own accessor, which
		// parses the roles with the validator that already refused an
		// unknown one at load, derives the broker kind from the stream
		// block, and is what the fleet view reads a peer's presence row
		// back through.
		profile:  opts.Bootstrap.Profile(nodeID),
		backends: backends, ownsBackends: ownsBackends, cipher: cipher,
		envelopes: envelopes, brokerWire: wire,
		onboarded: runner.NewLatch(), skills: skills.NewRegistry(),
		rewrites: compact.NewCache(),
		// ON THE NODE'S OWN QUEUE, which on a node without `data` is the
		// publish its custody hands to a data node — so a stateless node's
		// auxiliary spend reaches the history like its phases do.
		auxSpend:            auxspend.NewLedger(backends.Queue),
		steers:              steers,
		presenceWire:        presenceWire,
		answers:             answers,
		dataTurns:           builtin.NewDataTurns(nil),
		folderTurns:         builtin.NewFolderTurns(nil),
		mcpRetryBase:        opts.mcpRetryBase,
		sandboxOtel:         otel,
		sandboxPollInterval: opts.SandboxPollInterval,
		fakePlay:            opts.fakePlay,
		usageEvery:          opts.usageEvery,
		sandboxSeats:        map[string]bool{},
		bridge:              bridge,
		sessions:            sessions,
		metrics:             opts.Metrics,
		objectives:          newObjectives(opts.Metrics),
		mode:                mode,
		incarnation:         incarnation,
		id:                  nodeID,
		startedAt:           time.Now().UTC(),
		// Built before equip, which is what writes the company's own
		// numbers into it, and before node.New, which hands the same
		// value to every seat attachment.
		batch: queue.DefaultBatchOptions(),
		// BEFORE startCore, which threads the nudge into the identity
		// applier: a nil channel there would make every send block for
		// ever on the apply loop's own goroutine.
		directoryNudge: make(chan struct{}, 1),
	}
	// THE TIER A FACTS THE PROCESS HOLDS, read once and before anything below
	// reads them — the address people and vendors reach this deployment at,
	// this node's admin posture and the Tier A tokens it declares — because
	// each is fixed at boot and restart to change, and the first epoch built
	// below already composes links on the first and resolves logins on the
	// last.
	e.externalBase = opts.Bootstrap.API.ExternalBase()
	e.webhookBase = opts.Bootstrap.API.WebhookBase()
	e.webhookBaseKey = opts.Bootstrap.API.WebhookBaseKey()
	if opts.Bootstrap.ServesAPI() {
		e.adminPosture = opts.Bootstrap.API.Auth.Admin
	}
	e.tierATokens = make(map[string]struct{}, len(opts.Bootstrap.API.Auth.Tokens))
	for _, token := range opts.Bootstrap.API.Auth.Tokens {
		e.tierATokens[token.ID] = struct{}{}
	}
	// THE EVENT LOG FILES AN EVENT UNDER EVERY AGENT IT INVOLVES, by the
	// agent id the org chart derives for each seat a participant tag names
	// — which only a process holding the chart can say, so it is this
	// engine's, off the LIVE epoch at the moment each event is written.
	// Installed before anything below can publish; an event written with
	// no company active is filed under its own agent alone. See
	// [store.DB.SetEventSeats].
	backends.Store.SetEventSeats(func(handle string) (string, bool) {
		return seatAgentID(e.Company(), handle)
	})

	// ONE RUNNER PER ENGINE, because its in-process guard is half of what
	// keeps two writers off one third-party app — see [setup.Runner.Hold].
	// A runner per caller would give the loop, the API and a disconnect a
	// claim map each, which guards nothing.
	e.setupRunner = sync.OnceValue(e.newSetupRunner)
	// AFTER the engine exists, because what the bridge's supervision does
	// with a changed catalogue is refile this engine's registries; BEFORE
	// equip, which starts the company's shared servers on it.
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	// A MINT GITHUB REFUSED IS HELD on the restart schedule the seats' tool
	// servers retry that same refusal on, BEFORE anything reads a seat's
	// token: every reader of one installation (the seat's servers, a launch,
	// each running box's poll and its nudges, the inbound participant lookup)
	// meets the held refusal within the wait rather than asking GitHub again
	// on every read. See [github.Issuer].
	e.githubIssuer.RetryWait = e.seatRetryDelay

	// THE VIEW OF THE FLEET'S DATA NODES, built before the router that asks
	// it on every request. Building it lists nothing: it runs
	// once the boot's failure path is armed below, and answers unknown
	// until its first listing — see remote.go.
	if e.dataView, err = newDataView(opts.Bootstrap, backends.Coord); err != nil {
		if ownsBackends {
			backends.Close(ctx)
		}
		return nil, err
	}

	// EVERY NODE ROUTES THE ESTATE: a data node answers from its own copy
	// and asks a peer only while that copy cannot, and a node without
	// `data` asks for everything — see router.go. Built before anything
	// publishes, because a tool is handed its facades as soon as a seat is.
	if HoldsEstate(opts.Bootstrap) {
		e.local = newLocalEstate(e)
	}
	if e.router, err = e.newRouter(backends.Queue, nodeID, envelopes); err != nil {
		if ownsBackends {
			backends.Close(ctx)
		}
		return nil, err
	}
	// AND A READ OF A BOX ON ANOTHER NODE'S HOST, which that node carries
	// out — it pauses the box a collection reads — so it is signed like
	// every other such channel (sandboxhost.go). None without a broker: a
	// node with none is the whole fleet, and every box is its own.
	if backends.Queue != nil {
		if e.boxReads, err = newBoxReads(recordKeyring(opts.Bootstrap), nodeID, backends.Queue,
			e.sandboxCoordinator); err != nil {
			if ownsBackends {
				backends.Close(ctx)
			}
			return nil, err
		}
	}

	// A NODE WITHOUT `data` HANDS THE DATA NODES THE RECORD OF WHAT IT
	// DID — before anything publishes, because the custody listener must
	// be in place before the first turn a restarted node picks up off its
	// durable inbox.
	if !HoldsEstate(opts.Bootstrap) {
		e.custody = observe.NewCustody(backends.Queue)
		backends.Queue.AddPublishListener(e.custody.Listen())
		e.custody.Start(ctx)
	}

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
	// [Engine.startCore]'s own guard is: the list of things to unwind
	// grows down the function, and a per-return list stops matching it at
	// the first loop somebody adds.
	//
	// [context.WithoutCancel] because this is a teardown. The failure
	// being unwound is routinely the caller's own cancellation, and a
	// cleanup that inherited a dead context does nothing at all.
	//
	// AND THE ERROR IT RETURNS NAMES A LOST BROKER CONNECTION. A step that
	// failed because NATS closed a connection for good — credentials the
	// server stopped accepting, a max_payload lowered under the node, its
	// own embedded broker gone — answers with its own words, which say
	// "connection closed" and nothing an operator can act on, while the
	// sentence naming the cause and the setting to change sits recorded on
	// the queue. So the cause rides the error out beside the step's own
	// ([lostDuring]), read BEFORE the teardown: the client's closed handler
	// runs on its own goroutine after the failing call has been released,
	// so the read waits for it where a connection is already closed — and
	// once the teardown has closed the backends every connection reads as
	// closed, with no handler coming for this node's own close.
	booted := false
	defer func() {
		if booted {
			return
		}
		err = lostDuring(backends.Queue, err)
		e.teardown(context.WithoutCancel(ctx))
		log.InfoContext(ctx, "engine_boot_abandoned", "node", nodeID,
			"detail", "this boot left nothing running: the state log, the "+
				"shared MCP children, every duty loop and this node's "+
				"admission are stopped, and any backends this engine opened "+
				"itself are closed")
	}()
	// THE SESSIONS ARE THE ENGINE'S FROM HERE: the failure path above
	// removes them with everything else.
	sessionsHanded = true

	// THE FIRST THING THAT OUTLIVES THIS CALL, so the failure path above
	// is what stops it if the boot goes no further.
	e.startDataView(ctx)

	// THE AUDIT TRAIL BEFORE THE NATIVE BACKENDS, whose identity writer and
	// appliers announce through it.
	if err = e.startAuthEvents(ctx); err != nil {
		return nil, err
	}

	// THE SKILL SYNC IS BUILT BEFORE THE NATIVE BACKENDS, whose page
	// projection nudges it from a post-commit hook: a nudge with nothing to
	// land on would be lost. Building it starts nothing.
	if err = e.newSkillSync(nodeID); err != nil {
		return nil, fmt.Errorf("engine: tool skills: %w", err)
	}

	// THIS NODE ANSWERS FOR ITS OWN HISTORY from the moment it has a store
	// and a broker, whatever else it goes on to run — see history.go for
	// why a maintenance-mode node answers too. Like the core runtime below,
	// it is on EVERY node and in every mode: answering is not publishing.
	if err = e.armHistory(ctx); err != nil {
		return nil, fmt.Errorf("engine: serve the fleet's history: %w", err)
	}
	// AND FOR THE NOTES A PERSON SENDS ITS TURNS, which only this node can
	// hand to them — see steer.go.
	if err = e.armSteer(ctx); err != nil {
		return nil, fmt.Errorf("engine: serve notes to running turns: %w", err)
	}
	// AND FOR ITS INBOX MOVEMENTS, which a peer whose own copy is out of
	// service relays to its dashboards — and the follower that does the same
	// for this node's dashboards while its own copy is. See inboxrelay.go.
	if err = e.armInboxRelay(ctx); err != nil {
		return nil, err
	}
	// AND FOR WHO IS IN EACH ARTIFACT'S ROOM, which every node holds for its
	// own sockets and agents and tells its peers (see presence.go), and the
	// watch on its artifact copies' service that the rooms follow.
	if err = e.armPresence(ctx); err != nil {
		return nil, err
	}
	e.armArtifactWatch(ctx)

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

	// SERVING BEFORE ANY SEAT, and in every mode: a node restarted into a
	// maintenance mode still answers reads, and its writers are what
	// [Engine.estateBackend] withholds.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.serveEstate(ctx); err != nil {
		return nil, err
	}
	// AND KEEPING THE STATELESS NODES' EVENTS, in every mode too: what it
	// writes is this node's own event log, which no mode holds still.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.takeCustody(ctx); err != nil {
		return nil, err
	}
	// AND THE BROKER'S MEMBERSHIP, on a member, in every mode: the member
	// that answers a leaf's read of the metadata group, or carries an
	// operator's removal of a member that is gone, may be any of them —
	// and a fleet restarted into a maintenance mode is exactly when an
	// operator looks at who its broker still counts.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.serveFleetBroker(ctx); err != nil {
		return nil, err
	}
	// THE LEASE TTL IN FORCE, before anything claims a lease on the seat
	// heartbeat: the node's presence and seats and the mailbox retirement
	// all claim one below.
	e.leaseTTL = effectiveLeaseTTL(opts.Bootstrap, backends.Coord)
	// AND THE OBJECT STORE, over the backend the backends opened: every
	// node reads and writes files through it from boot, whatever mode it
	// started in.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.startObjects(); err != nil {
		return nil, err
	}

	// THE CORE RUNTIME, ON EVERY DATA NODE AND BEFORE ANYTHING DERIVED FROM A
	// COMPANY: the state log for every registered domain, the node gate and
	// the identity estate. A node with no company still signs people in — a
	// Tier A token's session included, which is what its company is created
	// through — and its identity log runs from boot; none of that waits for
	// the first company any more. Only its first PERSON does, since every
	// person holds a human seat the company declares (ADR-0032). See core.go.
	//
	// It does NOT wait for hydration — the reconcile is O(keys), and a node
	// that blocked here would serve no dashboard, hold its startup probe at
	// the recovery's 503 ([Options.Recovered] is called below) and run no
	// duty until it finished. Seat acquisition is what waits; see
	// [Engine.StateLogHydrated].
	//
	// A NODE WITHOUT `data` STARTS NONE OF IT: it holds no estate for a log to
	// run over (ADR-0025), and it signs nobody in, since the sign-in surface
	// runs only where `ingress` does and Tier A requires `data` beside it. What
	// it does need of the identity directory — whose record a login names, who
	// holds each seat — it asks a data node through the router, and its party
	// registry reads the directory there from now on ([Engine.startRoutedDirectory]).
	if HoldsEstate(opts.Bootstrap) {
		if err = e.startCore(ctx, opts.Bootstrap); err != nil {
			return nil, err
		}
	} else {
		e.startRoutedDirectory(ctx)
	}
	// THE BOUNDARY: everything above is what this node holds, recovered;
	// everything below is what its company configures. See
	// [Options.Recovered]. Logged, because it is the line a deployment sizes
	// its startup probe by, and the time from the process's first line to
	// this one is the recovery to measure.
	log.InfoContext(ctx, "engine_recovered", "node", nodeID,
		"detail", "what this node holds is open and recovered; what its company "+
			"configures (its secret snapshot, sandboxes, Worklet and Doclet, tool "+
			"servers, coding CLI logins) starts next")
	if opts.Recovered != nil {
		opts.Recovered()
	}
	// THE SNAPSHOT BEFORE THE FIRST EPOCH, because the epoch resolves every
	// ${VAR} it holds as it is built — the provider keys, the integration
	// tokens, the per-role MCP env. Loading the snapshot afterwards would
	// give the first epoch environment-only resolution and every later one
	// the store, so a rotated secret would work on the second apply and not
	// on boot.
	//
	// BELOW THE BOUNDARY, because nothing it reads is what the node holds:
	// it is the company's credentials, its cost grows with how many the
	// company keeps, and the only reader of what it installs is the company
	// built next. Above the boundary it held a startup probe sized for the
	// store, and that probe knows nothing of a company's secrets.
	//
	// AND ON EVERY NODE, a node with no company included, because the first
	// apply that brings one builds it from this snapshot whenever its own
	// refresh cannot read the store. See [Engine.bootSecrets] for why a node
	// never builds a company from the environment alone.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.bootSecrets(ctx); err != nil {
		return nil, err
	}
	// A NIL COMPANY IS THE UNCONFIGURED NODE, not a caller's mistake.
	//
	// The store is authoritative at runtime and a fleet's first revision
	// may arrive over the API, so a node has to be able to run with no
	// company at all: serving its HTTP surface, reporting itself
	// unconfigured, and holding no seats until the control plane hands it
	// an epoch. Everything below is written against that — a nil epoch
	// means "nothing to equip, nothing to place", never a panic.
	//
	// BUILT BELOW THE BOUNDARY with the rest of what the company
	// configures. A revision that cannot be built exits the boot here, past
	// the probe, as a bad configuration exits whatever the probe says.
	var company *Company
	if opts.Company != nil {
		company, err = NewCompanyWith(opts.Company, e.resolver())
		if err != nil {
			return nil, err
		}
		// STAMPED ONCE, as it is built, with the activation the caller
		// says this company came from — zero for one no activation has
		// named. See [stampActivation].
		stampActivation(company, opts.ActivatedAt)
	}
	// THE REST IS SKIPPED WHEN THERE IS NO EPOCH. Everything here is
	// derived from a company — the sandbox backends it configures, the
	// native halves its backends ask for, the tools its seats are given —
	// and an unconfigured node has none of them to build. The apply that
	// brings it its first revision builds all of it then: the native halves
	// ([Engine.startNativeFor]) and the sandbox runtime
	// ([Engine.startSandbox]), each before that apply equips.
	if company != nil {
		// BEFORE equip, because equip hands the artifact tools the boxes
		// of a turn's runs only on a node with the runtime
		// ([ArtifactToolDeps]): a dependency that is absent is OMITTED
		// rather than registered-and-broken, so an engine that equipped
		// first would build a company whose seats could never publish
		// what their executors' runs built.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.startSandboxFor(ctx, company); err != nil {
			return nil, fmt.Errorf("engine: sandbox: %w", err)
		}
		// BEFORE equip too, and for exactly the same reason: the ten
		// Worklet and Doclet tools are registered only where
		// their halves exist, so a node that equipped first would run a
		// company on the native backends whose seats have no way to read
		// or write them.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.startNative(ctx, opts.Bootstrap, company); err != nil {
			return nil, err
		}
		// NOT THE PROJECTS AND THE CONTAINERS HERE. They follow the
		// published company through [Engine.followCompany], which the
		// end of this constructor runs, stamped with the activation the
		// company was applied from.
		// EQUIPPED BEFORE PUBLISHED. A turn can start the instant the
		// epoch is current, and one that found an empty registry would run
		// a seat with no tools at all — a company that boots cleanly and
		// can do nothing.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.equip(ctx, company); err != nil {
			return nil, err
		}
		// THE CONFIGURED CREDENTIAL BUNDLES LAST, on Apply's rule: every
		// step above can still fail the boot, and a boot that fails must
		// leave no login behind in a state directory the next epoch's seats
		// share. And before the epoch is current, so its first model call
		// finds the login. See [Company.restoreBundles].
		//
		// BELOW THE BOUNDARY, as the rest of the company is: which entries
		// carry a bundle is the company's document, and a restore can wait
		// on the shared login's lock, which a startup probe sized for the
		// store knows nothing of.
		company.restoreBundles(ctx)
	}
	e.installEpoch(ctx, company)

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
		//
		// THE ENGINE'S OWN, minted once above — never a mint of its own.
		// This was `config.NewIncarnation(nodeID)` a second time, so the
		// coordination store held one process under two owners: its
		// presence, its seats, every fleet duty and the meter id its token
		// report is filed under on one, and its publish admission and its
		// capacity acknowledgements on the other. Two owners under one node
		// id is exactly what a second process running under that id looks
		// like, and a peer reading the store cannot tell the two apart.
		// Every lease
		// this process holds for ITSELF is under e.incarnation; the one
		// deliberate exception is the mailbox retirement's, which claims a
		// seat lease as a separate party and must lose to this host — see
		// [Engine.buildMailboxes].
		NodeID: nodeID,
		Owner:  e.incarnation,
		// Read FRESH through the epoch, never bound to the company this
		// engine started on: an apply replaces the seat set, and a
		// method value captured here would keep claiming seats a
		// deleted role no longer has and never claim a new one. The
		// host's own doc asks for exactly this and the binding did the
		// opposite.
		Seats: func() []placement.Seat { return e.Company().Seats() },
		// A seat whose mailbox attached before the copy of the company's
		// records its tools act on had caught up would answer "there is
		// no such item" to its own tools — and that is an answer a seat
		// ACTS on: it files the duplicate, it tells a person their link
		// is dead. So a node that is behind keeps every seat it holds and
		// claims nothing new until it has caught up
		// ([Engine.StateLogHydrated]): on a data node, every log whose
		// health gates admission established strictly, whatever backends
		// the company keeps or before it has one, because the core applies
		// them all from boot; and for each half the company runs natively,
		// a copy the estate's router says serves it — this node's own, or
		// a sound data node's.
		//
		// AND NEVER IN A MAINTENANCE MODE, where this same host still
		// runs for the presence it renews — see [Engine.seatsAdmitted].
		SeatsAdmitted: e.seatsAdmitted,
		// AND THE OTHER DIRECTION. Admission withholds new work from a
		// node that is merely behind; this gives back work already held
		// only by a node that can no longer ROUTE its seats' calls — no
		// sound copy of its own, and a presence view unknown and stale
		// past [statelog.FloorCacheStale]. A WRONG copy is not a reason
		// to shed: it goes out of service ([localEstate.For]) and the
		// seats stay, answered by a sound data node. Two gates because the
		// remedies differ: one is waiting, the other is a peer that can
		// route taking over. See [Engine.SeatsServiceable].
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
		SeatReady: func(ctx context.Context, s placement.Seat, lease coord.Lease) error {
			return e.prepareSeat(ctx, s, lease.Epoch, lease.Owner)
		},
		SeatDone: e.releaseSeat,
		// A seat that starts admitting turns re-checks at once what was
		// refused for want of one: an answer whose resume the seat's
		// preparation inherited waits on exactly this, not on the next
		// renew. See [Engine.mayResumeAnswer].
		//
		// BY HANDLE, because an owed answer is filed under the seat's
		// handle ([sandbox.Coordinator.Readmit]) and the hook is handed the
		// whole seat — see [node.Config.SeatAdmitted].
		SeatAdmitted: func(_ context.Context, s placement.Seat) {
			e.readmitAnswers(waitOwnership, s.Handle)
		},
		// A SEAT A PERSON PAUSED is attached already held, so placement
		// moving it here does not deliver the mail it is holding: the
		// release on the node it left dropped that node's hold with the
		// attachment. See seatpause.go.
		AttachHolds:   e.attachHolds,
		LeaseTTL:      e.leaseTTL,
		SweepInterval: opts.sweepEvery,
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
	//
	// OVER THE COORDINATION STORE'S CONNECTION (ADR-0001), which every
	// topology has: a seat's replay is an ordered consumer over its whole
	// history, and on the queue's own connection it would sit on the read
	// loop every mailbox consumes through. In the API the stores
	// themselves speak ([Backends.CoordinationJetStream]), so it reaches a
	// leaf's JetStream as well as a member's. [Backends.Conn] is the
	// embedded broker's second connection and is nil on purpose when this
	// node dialled an external cluster, so building on it switched memory
	// replication off on exactly the topology whose seats move between
	// machines: every seat forgot what it learned each time placement moved
	// it.
	//
	// THE SEAT'S DERIVED ID, never a roster lookup, so a seat a revision
	// removed still flushes on release ([memoryAgentID]).
	if e.memory, err = memsync.New(backends.Store, backends.CoordinationJetStream(),
		func(handle string) string { return memoryAgentID(e.Company(), handle) }); err != nil {
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
	// AND THE BROKER, whose death ends the process through the same exit —
	// as soon as there is a watchdog to end it, which is here. See
	// [Engine.watchBroker].
	e.watchBroker(ctx, backends.death)
	e.node = n
	e.dispatch = e.buildDispatcher(opts, backends)
	// THIS NODE ANSWERS FOR THE MEMORY OF THE SEATS IT HOLDS, in every mode
	// and whether or not it serves the API — see memoryread.go. After the
	// node, whose incarnation a seat's lease names.
	if err = e.armMemoryReads(ctx); err != nil {
		return nil, fmt.Errorf("engine: serve seats' memory: %w", err)
	}
	// AND THE LIVE OUTPUT OF THE CODING RUNS IT OWNS, for the same reason
	// and at the same point — see sandboxtail.go.
	if err = e.armSandboxTails(ctx); err != nil {
		return nil, fmt.Errorf("engine: serve coding runs' live output: %w", err)
	}
	// AND THE BOXES ON ITS HOST, which no other node can reach — see
	// sandboxhost.go.
	if err = e.armSandboxHost(ctx); err != nil {
		return nil, err
	}
	// AND THE TOOL BRIDGE OF THE EXECUTOR RUNS ITS SEATS LAUNCH, wherever a
	// box's call lands — see bridgefleet.go.
	if err = e.joinBridgeFleet(ctx, opts.Bootstrap); err != nil {
		return nil, fmt.Errorf("engine: serve the tool bridge to the fleet: %w", err)
	}

	// EVERYTHING BELOW THIS LINE PUBLISHES, and a maintenance-mode node
	// starts none of it: every duty, the scheduler, the change feeds, the
	// notification spine, the retention trim and the reflection pass.
	//
	// THE NODE ABOVE IS NOT ONE OF THEM, and [Engine.Start] starts it in
	// every mode: its seat host renews this node's PRESENCE, which is
	// membership rather than work — the capacity window's participant set,
	// the estate's read routing and the fleet views all read it while the
	// fleet is in a maintenance mode. What would publish through it is
	// withheld where it is decided rather than here: the host claims no
	// seat unless the mode publishes ([Engine.seatsAdmitted]), and no
	// surface is handed this node's writers ([Engine.writeSide]).
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
				"feed, no object repair or collection and no write routes")
		// THE ENGINE IS THE CALLER'S FROM HERE, so the guard above stands
		// down and [Engine.Stop] — the same teardown — is what ends it.
		// This return is a success like the last one, and a maintenance
		// node that unwound itself on the way out would take down the one
		// posture the capacity barrier is waiting to read.
		booted = true
		return e, nil
	}

	// WHICH SEATS A PERSON PAUSED, read before the seat host claims
	// anything — [Engine.Start] runs it — so the first mail this node is
	// handed meets a node that already knows. Bounded: past its budget the
	// watch keeps trying and the screening defers until it answers.
	e.startSeatPauses(ctx)
	// LAST, because its fleet-singleton duty is claimed under the node's
	// own incarnation.
	if err := e.startSandboxWaiter(ctx); err != nil {
		return nil, fmt.Errorf("engine: sandbox waiter: %w", err)
	}
	e.startMaintenance(ctx)
	// THE CORE'S DUTIES — the log's own trim and the identity estate's —
	// and this node's own usage publisher, on every data node that
	// publishes, company or not — a node without `data` runs no core and has
	// none: a fleet nobody has configured still writes its
	// identity log, and without the trim a domain's log only
	// ever grows, to its ceiling, where appends are refused. Beside the
	// sweep and after the node exists for the same reason: each duty is
	// claimed under the node's incarnation, and one that ran before the
	// lease existed would run on every node at once.
	e.startCoreDuties(ctx)
	// AND THE NATIVE HALF'S — the vector domain's one writer — where there
	// are native halves. An unconfigured node has none yet, and the apply
	// that brings its first company arms it then — see
	// [Engine.startNativeFor]. The live token meters are NOT armed here:
	// they are [Engine.Start]'s, after the host runs — see
	// [Engine.startBudgetReports].
	e.startNativeDuties(ctx)
	// AND THE OBJECT STORE'S COLLECTOR, behind the same gate: each pass pins
	// the estate with a barrier, which a capacity window refuses.
	e.startNativeCollector(ctx)
	// Beside the sweep, and a fleet singleton on the same terms: two nodes
	// reconciling one third-party app at the same moment can each create an identity
	// for one seat, and no later pass can detect or repair that.
	e.startIntegrations(ctx)
	e.startMemorySync(ctx)
	e.startMemoryFill(ctx)
	e.startScheduler(ctx)
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

	// AND WHAT FOLLOWS A PUBLISHED COMPANY, LAST, over the company this node
	// is now serving: the same list an apply runs, through the same
	// function. See [Engine.followCompany].
	//
	// # It is a safety net here rather than the mechanism, for most of it
	//
	// Boot brings most of these up by its own route and in its own order —
	// the registry with the epoch, the mailboxes with the seat host, the
	// scheduler with its loop — because each has a construction step this
	// does not. What running the list anyway buys is that the two paths
	// that publish a company cannot DISAGREE about what follows one: a step
	// added to the list is a step boot takes too, without anybody
	// remembering to add it here. The tracker's projects and the knowledge
	// containers have no other route at boot: this is where a company
	// booted on an activated revision is charted.
	e.followCompany(ctx, e.Company())
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
		runs := opts.SandboxRuns
		if runs == nil {
			// READ LIVE, never bound here: this runs once, in New, and on
			// a node whose first sandbox company arrives by apply there is
			// no coordinator yet to bind — so every delivery after that
			// apply was screened as though no run could exist, and a seat
			// with a job still running took new turns beside it.
			runs = e.sandboxSeatRuns
		}
		d.Conditions = e.conditionsFor(runs)
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
	if d.Rewriter == nil {
		d.Rewriter = func(handle string, use auxspend.Use) ledgerfit.Fitter {
			return e.seatCompactor(e.Company(), handle, use)
		}
	}
	if d.ReflectionRoom == nil {
		d.ReflectionRoom = func(ctx context.Context, handle string) (bool, error) {
			return e.reflectionRoom(ctx, e.Company(), handle)
		}
	}
	if d.FlushSpend == nil {
		d.FlushSpend = e.auxSpend.FlushTurn
	}
	if d.Park == nil {
		d.Park = e.park
	}
	if d.Pause == nil {
		d.Pause = e.holdInbox
	}
	if d.HoldSandbox == nil {
		// LIVE, for the reason Answer is below: the coordinator a seat's
		// sandbox hold belongs to may arrive by apply.
		d.HoldSandbox = e.holdSandboxSeat
	}
	if d.Budget == nil {
		d.Budget = e.budgetPark
	}
	if d.Answer == nil {
		// THE CALLER THIS METHOD NEVER HAD. TryResumeFromAnswer has been
		// exported and tested since the clarification path was written, and
		// nothing in the engine called it — so a coding run that asked a
		// person a question waited out its pause TTL however promptly they
		// replied.
		//
		// LIVE, for the reason the conditions above are: bound here, a
		// node whose sandbox arrived by apply offered no reply to any
		// parked run for the life of the process.
		d.Answer = e.answerParkedRun
	}
	if d.AnswerByTurn == nil {
		// The other way an answer reaches a parked run: named by its turn,
		// from any node, onto this seat's inbox. See [Dispatcher.routeAnswers].
		// LIVE for the reason Answer is — see [Engine.answerRunByTurn].
		d.AnswerByTurn = e.answerRunByTurn
	}
	if d.NoteDeferred == nil {
		d.NoteDeferred = e.node.Host().NoteDeliveryDeferred
	}
	if d.Observe == nil {
		d.Observe = e.observe
	}
	if d.Identify == nil {
		// Read off the LIVE epoch at the moment of the panic, like the
		// policies above: the dispatcher is built once and an apply can
		// remove the seat after it.
		d.Identify = func(handle string) (string, string) {
			return seatIdentity(e.Company(), handle)
		}
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
	// THE LIVE TOKEN METERS, which the dashboard's header pushes from and
	// which nothing published — so every header carried zeroes. AFTER the
	// host is running, because its first frame is published at once onto
	// the queue node.Start starts, and a backend that requires Start
	// refuses a publish before it: that first frame would be lost and every
	// dashboard would wait out a whole interval knowing nothing about the
	// company's budget. Detached, like everything else here — see
	// [Engine.startBudgetReports].
	e.startBudgetReports(ctx)
	// THE AUXILIARY-SPEND LEDGER'S TIMER, detached like the meters beside
	// it and on their cadence (auxspend.FlushInterval). Calls made before
	// it — an apply's first passes — wait in their buckets for its first
	// tick, and [Engine.teardown] flushes whatever is left.
	e.auxSpend.Run(context.WithoutCancel(ctx))
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
//
// On a bound of its own ([lifecycleContext]), never a step of the stop's
// allowance.
func (e *Engine) publishLifecycle(ctx context.Context, ev *events.Event) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	if e.node != nil {
		ev.Source = e.node.ID()
	}
	publishCtx, cancel := lifecycleContext(ctx)
	defer cancel()
	if err := e.backends.Queue.Publish(publishCtx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "lifecycle_event_not_published", "type", ev.Type,
			"error", err.Error(),
			"detail", "the audit log has no line for this node's start or stop; "+
				"the engine log does")
	}
}

// lifecyclePublishBudget bounds the publish of one lifecycle event: a node's
// start or stop, a seat's spawn or release.
//
// FIVE SECONDS, the JetStream client's own request timeout — what nats.go gives
// a publish whose context carries no deadline — and the bound the released
// seat's last memory publish has beside it ([memoryFlushTimeout]). A stream
// that has not acknowledged in that long will not inside any grace an
// orchestrator gives a stop, and what is lost is a line in the audit log and a
// live screen that ages the seat out instead.
//
// NEVER A STEP OF THE STOP'S ALLOWANCE ([seat.StopBudget]), which is the
// coordination store's: these travel on the event stream, which is replicated
// apart from the coordination buckets and can lose its quorum alone. A publish
// the stream never acknowledges waits out whatever deadline it is handed — and
// a deadline switches the client's own five seconds off — so on the allowance,
// a stream without quorum spent the time the presence, the seats, the
// admission and every duty needed, and a store that was answering was left
// holding all of them.
const lifecyclePublishBudget = 5 * time.Second

// lifecycleContext is what a lifecycle event is published on: ctx without its
// cancellation, bounded by [lifecyclePublishBudget] and nothing else.
//
// FREE OF ctx's CANCELLATION because the event records something that has
// already happened — the node started or was told to stop, the seat was taken
// or let go — and a caller that gave up on what comes after it has not taken
// that back. A drain's caller may pass a deadline for its wait, and a
// release's context is routinely one a shutdown has already ended.
func lifecycleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), lifecyclePublishBudget)
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
// Bounded by ctx for its wait, for the reason [node.Node.Drain] gives — and
// past it by at most [lifecyclePublishBudget], for the stop's announcement:
// the drain joins that publish before it returns, and it is free of ctx's
// cancellation for the reason [lifecycleContext] gives.
func (e *Engine) Drain(ctx context.Context) {
	// ONE ALLOWANCE FOR EVERY LEASE THE STOP GIVES BACK, from its first —
	// see [Engine.stopping].
	ctx = seat.WithStopBudget(ctx, e.stopping())
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
		//
		// AND BESIDE THE DRAIN rather than in front of it, joined before
		// the drain returns. The announcement travels on the event stream,
		// and a stream that will not acknowledge holds it for its whole
		// bound ([lifecyclePublishBudget]) — time the presence give-back the
		// drain begins with has no reason to wait out. On a member that has
		// lost both stores the two run out together, rather than the
		// announcement's five seconds coming first and the allowance after.
		var announced sync.WaitGroup
		if company := e.Company(); company != nil {
			ev := events.New(types.OrgStopped{OrgName: company.Config.Name},
				tracing.TraceOf(ctx))
			announced.Go(func() { e.publishLifecycle(ctx, ev) })
		}
		e.node.Drain(ctx)
		announced.Wait()
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
//
// ONCE, like the drain: a second call — the cleanup a failed start runs after
// the ordinary one has already been through — waits for the first to finish
// and then does nothing more. See [Engine.stopOnce].
func (e *Engine) Stop(ctx context.Context) {
	e.Drain(ctx)
	e.stopOnce.Do(func() {
		e.teardown(ctx)
		log.InfoContext(ctx, "engine_stopped")
	})
}

// stopping is the allowance this node's stop gives its leases back on — the
// presence and seat leases, the duties it releases, and the holds its loops
// give back as the teardown ends them ([Engine.tearingDown]) — built once, by
// the drain or by a failed boot's teardown, whichever comes first. See
// [seat.StopBudget] for why one allowance and not one per step. Its size is
// the stop allowance of this node's own lease TTL ([seat.StopAllowance], one
// heartbeat interval) LESS the share reserved for withdrawing its admission
// ([Engine.admissionShare]), so the stop's coordination is still one
// allowance in total.
//
// NOT the stop's lifecycle events, the custody flush, the last auxiliary
// spend or the usage publisher's last flush, which carry records on a stream
// rather than give a lease back: what they could not publish is lost, or
// waits for this node's next start, rather than lapsed, and a stream that will
// not acknowledge must not spend the time the coordination store is owed
// ([lifecyclePublishBudget]), so each keeps the bound of its own it states.
// Nor the admission, which has no lapse to fall back on.
func (e *Engine) stopping() *seat.StopBudget {
	e.stopBudgetOnce.Do(func() {
		e.stopBudget = seat.NewStopBudget(
			seat.StopAllowance(e.stopTTL()) - e.admissionShare())
	})
	return e.stopBudget
}

// tearingDown is the stop's allowance ([Engine.stopping]), and the moment the
// teardown begins, said to every give-back no stop's context reaches
// ([Engine.stepOnceTornDown]).
//
// THE TEARDOWN AND NOT THE DRAIN, because the drain's wait for running turns
// has no bound and nothing ends the loops that hold until the teardown does —
// a pass that finishes during that wait is an ordinary pass, and the time it
// spends belongs to it rather than to the seats the drain gives back at the
// end. A boot that failed reaches the teardown too, and it begins there the
// same way.
func (e *Engine) tearingDown() *seat.StopBudget {
	b := e.stopping()
	e.teardownSignal()
	e.beginTeardown()
	return b
}

// teardownSignal is done once the teardown has begun ([Engine.tearingDown]).
func (e *Engine) teardownSignal() context.Context {
	e.teardownOnce.Do(func() {
		e.teardownBegun, e.beginTeardown = context.WithCancel(context.Background())
	})
	return e.teardownBegun
}

// stepOnceTornDown is ctx for a give-back no stop's context reaches — a
// hold's, given back on the context it was taken on ([holdLeases]) — and the
// call that ends it, which the caller defers until the give-back returns.
//
// THE GIVE-BACK IS A STEP OF THE STOP'S ALLOWANCE FROM THE MOMENT THE TEARDOWN
// BEGINS: at once, if it already has, or partway through, if the teardown
// begins while the give-back is in flight — and cancelled when the allowance
// runs out. Before that it is no step of anything and keeps ctx's own bound.
// From the teardown's start rather than the give-back's, because the teardown
// is what waits for it: it stops the loop that holds and waits out the pass in
// flight, so a give-back begun during the drain and still running then is time
// the stop spends — against a store that has gone away, a client's whole
// request timeout beside the allowance — while one that finished during the
// drain's wait spent nothing the stop is owed.
func (e *Engine) stepOnceTornDown(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	begun := e.teardownSignal()
	finished, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-finished:
			return
		case <-begun.Done():
		}
		step, done := seat.StopStep(seat.WithStopBudget(context.Background(), e.stopping()))
		defer done()
		select {
		case <-finished:
		case <-step.Done():
			cancel()
		}
	}()
	return ctx, func() {
		close(finished)
		<-watched
		cancel()
	}
}

// stopTTL is the lease TTL a stop's bounds are fractions of: this node's own,
// or — for a boot that failed before it was resolved — the shipped one, which
// is what a lease it took would carry.
func (e *Engine) stopTTL() time.Duration {
	if e.leaseTTL > 0 {
		return e.leaseTTL
	}
	return seat.SeatLeaseTTL
}

// teardown stops everything a node started, in the one order that is correct.
//
// ONE IMPLEMENTATION FOR TWO CALLERS — [Engine.Stop] and [New]'s failure path
// — for the reason [core.shutdown] gives about its own two: a half-built
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
	// THE STOP'S ONE ALLOWANCE, the drain's if there was one — see
	// [Engine.stopping] — and from here on the one a hold given back by a
	// loop stopped below is charged to ([Engine.tearingDown]).
	ctx = seat.WithStopBudget(ctx, e.tearingDown())
	// FIRST, THE TRIGGER THAT RE-DERIVES WHAT A COMPANY ROUTES BY: the
	// party registry's. A rebuild over vendor wiring the stops below had
	// already ended would register identities nothing serves, so it is
	// ended before anything it reads from is.
	e.stopViewTriggers()
	// After the drain: the waiter's keepalive is what stops a running box
	// being reaped, so stopping it first would start the orphan clock on
	// every in-flight run while turns are still finishing.
	e.stopSandbox()
	e.stopNotifications(ctx)
	e.stopMaintenance()
	// THE NATIVE HALF'S DUTY BEFORE THE CORE'S, in the order the halves
	// come down below: the vector domain's writer publishes into a log the
	// core's trim is deciding how far to purge — and so does this node's
	// usage publisher, which is not a duty but writes the usage log the trim
	// counts.
	e.stopEmbedding()
	// EVERY WRITER OF A ROW THIS NODE'S USAGE DAY COUNTS, before the usage
	// publisher's last flush — or what they write after it is published only
	// by a next start, and a node that never starts again leaves it out of
	// the fleet's history: the learning passes, whose auxiliary calls spend;
	// the scheduler, whose late fire records a dispatch; the auxiliary-spend
	// ledger's last flush, once every producer of it has stopped — the turns
	// and reflections with the seats, the sandbox's condensations above, the
	// learning passes, a person's question with the HTTP surface before the
	// teardown began; and the custody keeper, whose settles make a stateless
	// node's rows count. The drain already ended the turns, whose phases and
	// completions are the rest.
	//
	// The memory fill stops beside them although it writes no such row — it
	// only embeds, and spends nothing a day counts — because it and the
	// learning passes query the store backends.Close closes, and tick on a
	// detached context on purpose — like the node's loops, they must not stop
	// at SIGTERM — so nothing else ends them, and a tick landing after the
	// close would run compaction queries and paid summarisation against a
	// closed database. Both also write memory the memory sync below carries.
	e.stopMemoryFill()
	e.stopLearning()
	e.stopScheduler()
	// BEFORE the custody flush below, which is what carries a node without
	// `data` its records.
	e.stopAuxSpend(ctx)
	// AND LEAVING THE GROUP, not only ending the pass: the group's
	// attachment would otherwise keep taking batches until the queue closes.
	// BEFORE backends.Close, which closes the store the keeper writes and the
	// broker it settles against.
	e.stopKeeper(ctx)
	e.stopUsage()
	e.stopRetention()
	e.stopIdentityDuties()
	e.stopBudgetReports()
	// Before the node's stop detaches the inboxes: an alarm firing into a
	// client that is closing would log a release it could not make.
	e.stopBudgetParks()
	// And the pause watch, for the same reason: a resume landing on a
	// client that is closing would lift a hold on nothing.
	e.stopSeatPauses()
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
	// the whole one. And after the memory fill and the learning passes
	// above, which write the memory it carries.
	e.stopMemorySync()
	// THE OBJECT STORE'S COLLECTOR with the other duty loops: the loop
	// claims its duty afresh every turn, so one still running past the
	// release below takes it straight back and the node exits holding it.
	// AND BEFORE the native runtime below, whose tracker reader it reads
	// the references through.
	e.stopObjectCollector()
	// THE TRASH REAPER for the collector's reason, and before the native
	// runtime below, whose knowledge store its passes purge through.
	e.stopTrashReaper()
	// AFTER every duty loop above has stopped and waited out its tick, so no
	// tick of this node runs once a peer can take the duty, and no turn of
	// this node can claim one again once it is given back.
	e.releaseDuties(ctx)
	// BEFORE the native backends, whose pages its walk reads, and before
	// backends.Close, which closes the broker its nudge listens on.
	e.stopSkillSync(ctx)
	e.stopServingEstate(ctx)
	e.stopServingFleetBroker(ctx)
	// BEFORE backends.Close, for the same reason and with more at stake:
	// the apply loops and the indexer both write, and an apply landing
	// after the close would fail its transaction mid-batch and leave the
	// cursor ahead of the rows it claims to describe.
	//
	// THE NATIVE HALF FIRST — its feeds, its index and its search
	// answerer, every one of which reads what the logs write — and then
	// the core, whose logs are the last thing standing: its view triggers
	// were ended at the top of this list.
	e.stopNative(ctx)
	// BEFORE backends.Close, which closes the store an answer reads: a
	// peer's question arriving mid-teardown is declined rather than read
	// from a closing file.
	e.stopHistory(ctx)
	// With it, and for the same reason: a note answered `accepted` by a
	// node that is tearing down is a note no round will read.
	e.stopSteer(ctx)
	// And presence, whose leaves must reach the fleet while the broker is
	// still there, and the watch the rooms follow.
	e.stopArtifactWatch()
	e.stopPresence(ctx)
	// And the inbox relay's follower and answerer, which ask and answer on
	// the same broker.
	e.stopInboxRelay(ctx)
	// And a seat's memory, read from the store backends.Close is about to
	// close.
	e.stopMemoryReads(ctx)
	// And a run's live output, read from boxes this node is about to stop
	// driving — and a read of a box on its host, asked by a peer.
	e.stopSandboxTails(ctx)
	e.stopSandboxHost(ctx)
	// And the bridge's relay, whose exchanges serve sessions this node is
	// about to stop holding.
	e.stopBridgeFleet(ctx)
	// AND THEN THE CORE, whose logs are the last thing standing — see the
	// comment above stopNative.
	e.stopCore()
	if e.node != nil {
		e.node.Stop(ctx)
	}
	// AFTER the seats are released, so a per-role child is normally
	// already gone with its seat. This is the backstop for the ones that
	// were not: a stdio server is a process TREE holding a seat's
	// credentials, and one left behind outlives the engine that vouched
	// for it.
	e.stopSharedServers(ctx)
	// AFTER everything that announces through it, and before the broker its
	// final flush publishes onto is closed — and before the custody flush
	// below, which carries a node without `data` its records.
	e.stopAuthEvents()
	// AFTER EVERY SEAT AND LOOP THAT PUBLISHES, so the last events they
	// published are in the buffer, and BEFORE the broker closes under the
	// last batch.
	e.stopCustody(ctx)
	e.stopWatchingDataNodes()
	if e.ownsBackends {
		e.backends.Close(ctx)
	}
	// LAST, once nothing can run a phase: the drain waited out every turn,
	// the sandbox's resumes stopped with it above, and the seats are gone
	// with the node. A phase still serving from the directory would find its
	// socket removed under its CLI.
	e.closeSessions(ctx)
}

// closeSessions removes this process's tool-session directory. Nil-safe: a
// boot that failed before making it has none.
func (e *Engine) closeSessions(ctx context.Context) {
	if e.sessions == nil {
		return
	}
	if err := e.sessions.Close(); err != nil {
		// Nothing to retry and nobody waiting: the directory is this
		// process's, under the temp root, and holds sockets no process
		// will dial again. Said so an operator who finds it knows why.
		log.WarnContext(ctx, "tool_sessions_not_removed", "dir", e.sessions.Dir(), "err", err)
	}
}

// custodyStopBudget is how long an orderly stop waits for the broker to take
// the events this node still holds.
//
// TEN SECONDS: many publishes to a broker that is up, and short enough that a
// stop with no member reachable is not held for long — what it could not
// publish is logged as lost.
const custodyStopBudget = 10 * time.Second

// stopCustody flushes and ends the event custody. Nil-safe.
func (e *Engine) stopCustody(ctx context.Context) {
	if e.custody == nil {
		return
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), custodyStopBudget)
	defer cancel()
	e.custody.Stop(bounded)
}

// StallLag is how far behind the worst live watched duty is: the time since
// its last beat, once it has missed one ([seat.Watchdog.Lag]).
//
// The early-warning half of the watchdog: it fires at the lease TTL, and
// this is the number climbing towards it. Zero means nothing to report:
// every watched duty is on its beat, or nothing is being watched, which is
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

// Cipher is this node's keyring: the ONE cipher of the process.
//
// EXPOSED for the reason [Engine.Backends] is. `crewlet run` composes the
// config surface, the secret surface and the sign-in surface beside the
// engine, and each seals or opens under the keyring the engine already built
// from Tier A — the reconciler authenticates a peer's revision under it, and a
// revision the config surface sealed under anything else would be one no node
// could open. Handing the surfaces this one rather than letting the command
// build its own is what makes "one keyring" a fact instead of a promise two
// constructors keep. Never nil on an engine [New] built.
func (e *Engine) Cipher() secrets.Cipher { return e.cipher }

// Recorder is the process's one metrics recorder, or nil.
//
// EXPOSED for the same reason [Engine.Backends] is: a subsystem the CLI
// composes beside the engine — the backup service is the one — measures
// something the catalogue declares, and a second recorder would be a second
// set of series for one process. Nil is legal and records nothing.
func (e *Engine) Recorder() *metrics.Recorder { return e.metrics }

// Objectives is the node's service objective meter: what the API's ready
// reports are counted into beside the engine's own recording sites. Never nil
// on an engine [New] built.
func (e *Engine) Objectives() *objectives.Meter { return e.objectives }

// newObjectives builds the node's objective meter over rec, which may be nil:
// the events are then counted into the meter's own ledger alone, and the burn
// alarm still judges them.
func newObjectives(rec *metrics.Recorder) *objectives.Meter {
	// A NIL RECORDER STAYS A NIL INTERFACE: one wrapped in it would be a
	// recorder the meter calls into.
	if rec == nil {
		return objectives.NewMeter(nil, nil)
	}
	return objectives.NewMeter(rec, nil)
}

// Dispatch delivers one inbox partition to a seat.
//
// This is the node's TurnFunc, exported because it is also the entry point for
// a delivery that did not come from the seat's own subscription — a sandbox
// run completing, which resumes the turn parked on it rather than waiting
// for the broker to hand the seat something.
func (e *Engine) Dispatch(ctx context.Context, handle string, evs []*events.Event) queue.Result {
	return e.dispatch.Dispatch(ctx, handle, evs)
}

// conditionsFor answers the ownership and posture questions for one seat.
func (e *Engine) conditionsFor(sandboxRuns func(string) (bool, bool)) func(string) inbox.Conditions {
	return func(handle string) inbox.Conditions {
		_, owned := e.node.Host().MayStart(handle)
		company := e.Company()
		// ONE CALL FOR BOTH SANDBOX ANSWERS: a park is a seat freed and a
		// question opened at once, and a reader that took them apart could
		// see the seat between the halves — free, with nothing waiting —
		// which is the delivery this whole seam exists to catch.
		var heldBySandbox, sandboxAwaitsAnswer bool
		if sandboxRuns != nil {
			heldBySandbox, sandboxAwaitsAnswer = sandboxRuns(handle)
		}
		paused, known := e.pauseOfHandle(handle)
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
			TurnEngineReady:     company != nil && company.Models != nil,
			SeatHeldBySandbox:   heldBySandbox,
			SandboxAwaitsAnswer: sandboxAwaitsAnswer,
			// The SAME gate the inbound edge and the scheduler read, so
			// a shedding node refuses at every trigger admission rather
			// than only at the one that happened to be wired. It was a
			// hardcoded true, which made a seat's own inbox the one
			// path a shed could never reach.
			AdmitsTriggers: e.admits(),
			// A PERSON'S PAUSE, off this node's watched copy — and
			// whether that copy has been read at all, which is a
			// different answer from "not paused". See seatpause.go.
			Paused:       paused,
			PauseUnknown: !known,
		}
	}
}

// seatFence is the per-round check a turn on this seat runs under: that this
// process still holds the grant the turn was admitted under, where it holds
// one at all, and that nobody has asked the turn to stop.
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
//
// AND A PERSON'S STOP. A pause that asked for the running turn to stop closes
// the same fence, at the same round boundary, with [turn.ErrStoppedByPerson]
// — ownership first, because a turn on a seat this node no longer holds is
// the successor's to be stopped or not, and handing the delivery on is what
// it is owed. The stop is read off this node's copy of the pauses on every
// round, so it reaches a turn that started before the pause did.
//
// THE SEAT'S ID IS RESOLVED ONCE, when the fence is built for the turn: a
// pause is kept on the id. A handle the running company resolves to no agent
// seat has no pause to read, and its turn is fenced on ownership alone.
func (e *Engine) seatFence(handle string) func() error {
	var owned func() error
	if e.node != nil {
		owned = e.node.Host().Fence(handle)
	}
	seat, seatErr := e.seatID(handle)
	resolved := seatErr == nil
	return func() error {
		if owned != nil {
			if err := owned(); err != nil {
				return err
			}
		}
		if !resolved {
			// No agent seat answers to the handle: there is no pause to
			// read, and the turn is fenced on ownership alone.
			return nil
		}
		return e.stopFor(seat, handle)
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
	id, err := e.seatID(handle)
	if err != nil {
		return err
	}
	subject := topics.AgentInbox(id)
	for _, ev := range evs {
		if err := e.backends.Queue.Publish(ctx, subject, ev); err != nil {
			// THE SEAT BY NAME, not only the subject it is addressed by.
			// A mailbox is named by the seat's id, so a failure reported
			// as the subject alone hands whoever reads it a uuid and no
			// way to tell which colleague lost their mail.
			return fmt.Errorf("engine: requeue %s onto seat %q (%s): %w",
				ev.Type, handle, subject, err)
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
	return turn.Input{RunID: req.RunID, Depth: req.Depth, Reply: reply}
}

// runTurn is the default turn: build the seat's runner and drive the loop.
func (e *Engine) runTurn(ctx context.Context, req Request) (turn.Result, error) {
	// THIS EXECUTION'S OWN IDENTITY, settled before anything else so the
	// span, the events and the tools all name the same run.
	//
	// It is NOT the work key, and that is the whole of ADR-0017: a trigger
	// that fails without acting is NAK'd and redelivered, so one work key
	// legitimately runs several times — and a run that borrowed that
	// identity wrote its phases, its live row and its sandbox run on top of
	// the attempt it was repeating.
	//
	// THE DISPATCH'S WHERE THERE IS ONE, written back onto the local copy of
	// the request rather than kept beside it: [Dispatcher.Dispatch] mints it
	// so its own bookkeeping — the conversation entry, the abandon record —
	// names the run this turn publishes under, and two variables for one
	// identity is how those two stop agreeing. A Request assembled anywhere
	// else (a test, a direct driver) carries none, and a turn with no
	// identity is not an option, so one is minted here.
	if req.RunID == "" {
		req.RunID = newRunID()
	}
	// THE TURN'S OWN SPAN, opened before anything else so every phase, LLM
	// round and tool call below is a child of it and every event the turn
	// publishes carries its id. The dispatcher has already restored the
	// trigger's trace onto ctx, so this is a child of the wake rather than
	// a root.
	ctx, span := tracing.Start(ctx, "engine", "agent.turn",
		attribute.String("crewlet.seat", req.Handle),
		// `turn_id` IS the run id, and it is spelled the way every event
		// and every screen spells it — a span attribute that named the
		// same value something else would make a trace query find the
		// dispatch span and neither the resume span nor the box's own.
		attribute.String("crewlet.turn_id", req.RunID),
		attribute.String("crewlet.work_key", req.WorkKey),
		attribute.Bool("crewlet.coalesced", req.Coalesce),
		attribute.Int("crewlet.delegation_depth", req.Depth))
	defer span.End()

	// WHAT WOKE THIS TURN IS WHAT ITS READS ARE NO OLDER THAN: every
	// constituent's committing record goes into this node's floors before
	// the first read, so a list read — at `session`, floored at those
	// floors — shows the change the seat was woken for.
	e.observeTriggers(ctx, req.Events)

	// THE WORKING INDICATOR, up before this turn does anything slow. Here
	// rather than beside turn.Run because everything between the two is
	// already work a person is waiting through: the prefetch reads a chat
	// thread and searches the knowledge base over the network, and an
	// indicator raised after that says "thinking" only once the thinking is
	// about to start.
	//
	// It raises nothing at all for a trigger nobody is watching a composer
	// for — see [chatMetadataOf] — and a nil session's every method is a
	// no-op, so there is no branch here and none below.
	//
	// HELD UNDER THE RUN, not the unit of work: the turn parks on its
	// executor's run, and what takes the hold back is keyed by the run's row
	// — the resume that rejoins it ([Engine.resumeWorkingStatus]) and the
	// stop that releases it ([Engine.releaseWorkingStatus]) both read the
	// row's turn id, which is this run's id (ADR-0017). Held under the work
	// key, neither ever found it, and every parked turn's indicator stood
	// until the backend's own expiry.
	status := e.beginWorkingStatus(ctx, req.Handle, req.RunID, req.Ask())
	// ENDED ON EVERY PATH OUT OF THIS FRAME, the runner build failing below
	// included: that path raised an indicator and never ran a turn, and
	// nothing else would ever take it down. `working` stays false until a
	// suspension's own ROW says a detached run is coming back, so every other
	// path — including a redelivered trigger, which raises a fresh indicator
	// of its own when it runs again — clears. See [endWorkingStatus].
	var working bool
	defer func() { endWorkingStatus(ctx, status, working) }()

	// PINNED ONCE. Two reads of the epoch can straddle an apply, and a turn
	// that built its runner from one revision and took its round caps from
	// the next is running a company that never existed — the exact failure
	// publishing-instead-of-mutating exists to remove.
	company := e.Company()
	// Assembled BEFORE the runner, because the runner needs it: every phase
	// event carries the turn's identity, and a runner built without it
	// publishes phases attributed to nobody.
	tel, err := e.describeTurn(ctx, company, req)
	if err != nil {
		// Nothing started, so nothing ended, exactly as for a runner that
		// could not be built below: no phase was published, and the
		// dispatcher hands the delivery back rather than recording a turn
		// that never ran.
		return turn.Result{}, err
	}
	// THE TURN IS ON THE RECORD BEFORE ANYTHING SLOW, and the prefetch below
	// is the first slow thing: it reads a chat thread and searches the
	// knowledge base over the network, and until a phase opened nothing
	// else said this run existed. Every path out of this frame that returns
	// closes it — see [Engine.publishTurnStarted].
	e.publishTurnStarted(ctx, tel, req.Depth, req.DelegationChain, false)
	// THE TURN'S METER, BEFORE ITS FIRST MODEL CALL — and the prefetch below
	// makes several, on the seat's auxiliary chain, as the conversation
	// block's condensation does after it. Every one is charged through this
	// meter ([turnTelemetry.aux]), so a window they fill is held before the
	// executor's first round rather than learned from that round's refusal
	// once the vendor has billed it. Read off the PINNED epoch, so a revision
	// that raises a ceiling mid-turn cannot move the limit a round is judged
	// against.
	tel.budget = e.meterFor(company, req.Handle)
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
	blocks := e.prefetchFor(ctx, company, req, task, tel.aux(), tel.boundary)
	// The skills OFFERED to this turn, carried onto its completion so the
	// curator ages a skill on when it was last put in front of a model
	// rather than archiving the ones a seat reads every turn.
	tel.skills = blocks.SkillIDs

	reply := ReplyFor(req.Events)
	turnIdentity := tel.runnerTurn(company, req.Depth, req.DelegationChain,
		task, reply, e.WithheldContacts())
	// The executor's launcher, over the turn's pinned company — see
	// [Engine.agentRunFor] — and the box a person's note reaches this turn
	// through ([turnSteerBox]).
	launcher := e.agentRunFor(company, req.Handle, turnIdentity.Context)
	box := turnSteerBox()
	r, err := company.RunnerFor(req.Handle, e.seatRegistry(company, req.Handle), RunnerInput{
		Task:    task,
		Compact: e.compactorFor(company),
		Context: blocks,
		Skills:  e.skills,
		Reply:   reply,
		// BOUNDED AT RENDER, never at write. The stored row is the only copy
		// of the turn; what a prompt shows is a display decision, and this
		// one keeps the newest entries whole and condenses the rest.
		Conversation: e.conversationBlock(ctx, company, req.Handle, req.History, tel.aux()),
		Publisher:    e.backends.Queue,
		Turn:         turnIdentity,
		Sessions:     e.sessions,
		Metrics:      e.metrics,
		Launcher:     launcher,
		Steer:        box,
		Markers:      e.markers(),
		Latch:        e.onboarded,
		// The meter the context assembly was charged through — see above.
		Budget: tel.budget.budget(),
		// The round-cap extension judge, from the same pinned epoch, for
		// the onboarding pass — the one engine loop with a cap above its
		// budget. It was never supplied, so every exhaustion rescued with
		// "no_judge" and the extension mechanism was inert.
		Judge: e.judgeFor(company, req.Handle, tel.aux()),
		// The seat's headroom, for a sub-agent spawn. Three-valued on
		// purpose: nil is "no ceiling configured", and a FAILED read
		// refuses the spawn rather than granting it no ceiling.
		Remaining: e.remainingFor(company, req.Handle),
		// Built HERE, at turn start, because the epoch it closes on is
		// the grant THIS turn was admitted under — a fence built later
		// would compare a re-claim against itself.
		Fence: e.seatFence(req.Handle),
		// The working indicator's phase updates, taken from the one frame
		// that knows a phase has opened — the same call that publishes
		// agent_phase_started, so the words a person reads and the event
		// a dashboard reads can never name different phases.
		OnPhase: func(ph phase.Phase) { status.Phase(ph.String()) },
	})
	if err != nil {
		// CLOSED, BECAUSE IT WAS OPENED. This path used to publish nothing
		// on the reasoning that nothing had started — which stopped being
		// true the moment the turn announced itself above: its start is on
		// the record, its context was gathered, and a start with no end is
		// what a reader takes for a turn that is still running, or one whose
		// process died under it. So it ends as what it is, a run that failed
		// before its first phase, with the build's own error as the reason.
		//
		// It costs the retry nothing: the dispatch hands the delivery back,
		// the redelivery is a NEW run with a start and an end of its own
		// (ADR-0017), and the learning dispatcher marks a unit of work
		// spent only on a settled outcome, which the empty decision of a
		// turn that never reached its loop is not.
		//
		// AND CHARGED LIKE ANY OTHER ENDING: the run happened, on the item
		// it names, and a task's turn count is its attempts. Its context was
		// gathered, at a cost, which is on the stream before its end — see
		// [Engine.endSegment].
		e.endSegment(ctx, tel, runner.Spend{}, turn.Result{}, err,
			tel.chargeFor(runner.Spend{}, turn.Result{}, err, time.Now().UTC()))
		return turn.Result{}, err
	}

	// A PERSON MAY NOW STEER IT. The box is closed as soon as the turn
	// returns — before its completion is published, so the record of a note
	// the turn missed precedes the record that the turn ended — and the
	// defer is the backstop for every other way out of this frame. Closing
	// twice records nothing twice. See steer.go.
	closeSteer := e.openSteer(req.RunID, req.Handle, box, r)
	defer closeSteer(ctx)

	// BEFORE THE EXECUTOR, on its own budget. A seat's first ever turn used
	// to onboard inside the phase that decides what to do, and could spend
	// that phase's whole budget reading pages: the turn least likely to get
	// any work done was the one where a seat had never worked before.
	//
	// A failure here does NOT fail the turn: the seat is un-onboarded, which
	// is the state it was already in, and refusing to work over it would
	// make a knowledge base that is briefly unreachable stop the company.
	//
	// EXCEPT A BUDGET REFUSAL, which ends the turn here, as the refusal it
	// is. Its refused round is counted, so the window it named reads past
	// its ceiling and every later call of the turn is certain to be refused
	// on the turn's pinned clock ([toolloop.BudgetMeter.Refused] states the
	// one edge of that): entering the loop paid for nothing (the meter stops
	// the executor's first round before it is made), and it put an executor
	// phase on the record that never ran a round. Ended here, the turn is
	// published as budget_exhausted naming the onboarding's refusal, and the
	// dispatch parks the seat until the window turns over, exactly as a
	// refusal inside the loop does.
	var res turn.Result
	ran, onboardErr := r.Onboard(ctx)
	refused := errors.Is(onboardErr, toolloop.ErrBudgetExhausted)
	switch {
	case refused:
		log.InfoContext(ctx, "onboarding_pass_refused", "handle", req.Handle,
			"error", onboardErr, "detail", "the budget refused the onboarding pass; "+
				"the turn ends on the refusal and the seat retries once its window has room")
		err = fmt.Errorf("turn: onboarding: %w", onboardErr)
	case onboardErr != nil:
		log.WarnContext(ctx, "onboarding_pass_failed", "handle", req.Handle,
			"error", onboardErr, "detail", "the seat stays un-onboarded and retries "+
				"next turn; the turn continues")
	case ran:
		log.InfoContext(ctx, "onboarding_pass_ran", "handle", req.Handle)
	}
	if !refused {
		res, err = turn.Run(ctx, r, company.TurnSettings(tel.deadline),
			turnInputFor(req, reply))
		// WHAT THE TURN ANSWERED WITHOUT BEING WOKEN FOR IT: the waiting
		// messages its thread block showed it, which the dispatch records as
		// worked through beside its own triggers. Off the block this frame
		// assembled, because the loop never knew what a prompt showed — see
		// workedthrough.go. Only for a turn that RAN: a refused onboarding
		// showed nothing to a model, so nothing was worked through.
		res.WorkedThrough = workedThroughKeys(threadOf(req.Ask()), blocks.ThreadContextAnswered)
	}
	// THE BOX CLOSES THE MOMENT THE TURN RETURNS: no later round will read
	// a note, and one offered from here on is answered `closed`.
	closeSteer(ctx)
	// THE SEGMENT'S CHARGE IS DECIDED FIRST, because a segment charged to
	// nothing hands what it spent to the suspension written below.
	spend := r.Spend()
	charge := tel.chargeFor(spend, res, err, time.Now().UTC())
	// Published on BOTH paths. An error here means a phase broke, which is
	// precisely when a dashboard most needs the turn closed: the phase
	// events already put the seat into `working`, and returning without this
	// leaves it there until the seat happens to take another turn.
	//
	// AFTER WHAT ITS AUXILIARY CALLS COST — its card's rewrite the last of
	// them — so a reader that asks for the turn as it ends reads its context
	// and its rewrites beside its phases; and CHARGED to the work item it
	// was on after the record of its end exists. See [Engine.endSegment].
	//
	// AND BEFORE THE PARK IS WRITTEN, as the resumed path has always done:
	// the write below is what opens the run to everything that can END the
	// parked turn — its collection, a loss, a person's stop — and each of
	// those closes the turn on the record with an end that has to be newer
	// than this park. Published after the write, a stop that landed in
	// between was dated before the park it closed, and every reader that
	// takes the newest of a turn's records left the turn parked for good.
	// The runner holds the parked state until the frame unwinds, which this
	// does not.
	e.endSegment(ctx, tel, spend, res, err, charge)
	if res.Suspended {
		// ITS ANSWER IS WHAT THE INDICATOR FOLLOWS, not `res.Suspended` —
		// the intent to park and a run that can actually be resumed are
		// different facts, and this call is where the second one is
		// established. See [stillWorking].
		parked, parkErr := e.persistSuspension(ctx, r, req.RunID, tel, charge)
		if turn.Stopped(parkErr) {
			// A PERSON STOPPED THE RUN BETWEEN ITS LAUNCH AND ITS PARK:
			// the run is ended and announced, the refused park closed the
			// turn as stopped ([Engine.closeStoppedAtPark]), and this frame
			// ends as a turn stopped at its next round does — the
			// dispatcher's own record of who stopped it and that the
			// trigger is worked ([Dispatcher.stopped]).
			res.Suspended, err = false, parkErr
		} else {
			working = stillWorking(parked, parkErr)
		}
	}
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
	// The MCP rows are this process's own record of what it started, read
	// under a lock and never from a child.
	status := coord.NodeStatus{
		StartedAt: e.startedAt, AdminPosture: e.adminPosture,
		MCP: e.mcpStatus(ctx),
	}
	if b := e.backends; b != nil && b.Queue != nil {
		status.InFlight = b.Queue.InFlightCount()
	}
	if n := e.node; n != nil {
		if host := n.Host(); host != nil {
			status.Draining = host.Draining()
		}
	}
	// The replication loops — every state-log domain's applier, on every
	// data node; none on a node without `data`, which runs no applier — so
	// an operator asking why a fresh node holds no seats can see
	// the answer in the fleet view rather than inferring it from an empty
	// claim list. See [coord.NodeStatus.ProjectionsReady] for why this is
	// not a readiness signal.
	for _, loop := range e.StateLogStatus(ctx) {
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

// SetOnCompanyPublished registers a hook run after every published company has
// been converged on, for surfaces that render the COMPANY rather than its
// activity.
//
// The dashboard's roster, org tree and tool catalogue are all derived from the
// company, so nothing that happens afterwards will correct them: a change that
// adds a seat, edits one or removes one changes all three and produces no
// event a projection could learn from. Without this, an open dashboard kept
// rendering the company it connected to until someone reloaded the page — and
// the client cannot paper over a deletion either, because an overlay merge has
// no way to express a row going away.
//
// # A COMPANY publish, not an apply
//
// It is called from the steps every published company goes through
// ([Engine.followCompany]), at boot and on every apply, and therefore LAST:
// everything the payloads are cut from has been rebuilt by the time it runs.
//
// A SETTER for the same reason SetPosture is one: the API half is built after
// the engine, so there is no moment at construction when a real wiring could
// pass one.
//
// Safe to call while the engine is running, and safe to leave unset — an
// engine with no hook publishes exactly as before.
func (e *Engine) SetOnCompanyPublished(fn func(context.Context)) { e.onPublished.Store(&fn) }

// notifyCompanyPublished runs the hook, if one is set.
func (e *Engine) notifyCompanyPublished(ctx context.Context) {
	if fn := e.onPublished.Load(); fn != nil && *fn != nil {
		(*fn)(ctx)
	}
}

// SetOnCatalogueChanged registers a hook run after the current epoch's tool
// catalogue changed WITHOUT a published company: a shared MCP server its
// bridge brought back with different tools, or one whose failed start a retry
// brought up, refiled into the epoch that is current (mcpsupervise.go).
//
// The one part of a published company that moves after it is published. The
// dashboard's tool catalogue is a push re-sent on every published company
// ([Engine.SetOnCompanyPublished]) and corrected by no event, so without this
// an open dashboard listed a server's tools as they were when the company was
// published — none, for a server whose start had failed — while its agents
// were already calling the new ones, until somebody reloaded the page.
//
// Its own hook rather than the published one: nothing else a published
// company re-sends has moved, and the published hook also decides every open
// socket again against the company, which a catalogue change gives no reason
// to do. A SETTER for [Engine.SetOnCompanyPublished]'s reason. Run on the
// supervisor's goroutine, after the refile, holding no lock of the engine's.
func (e *Engine) SetOnCatalogueChanged(fn func(context.Context)) { e.onCatalogue.Store(&fn) }

// notifyCatalogueChanged runs the hook, if one is set.
func (e *Engine) notifyCatalogueChanged(ctx context.Context) {
	if fn := e.onCatalogue.Load(); fn != nil && *fn != nil {
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

// tokenMaterial is the Tier A keyring the per-run token issuers derive from,
// nil-safe for an engine built without one.
func tokenMaterial(boot *config.Bootstrap) runtoken.Material {
	if boot == nil {
		return runtoken.Material{}
	}
	return boot.Secrets.TokenMaterial()
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

// observeTriggers hands this node's read-your-writes floors the position of
// every record whose wake is among evs ([notify.TriggerOf]) — the position a
// first-party change feed stamped on it.
//
// THE NODE'S FLOORS, not the turn's: they are what every read this node routes
// carries ([estate.Session]), to whichever holder answers it, this node's own
// copy included. A floor a turn raises is one every seat on the node then reads
// past, which is conservative rather than wrong — never a read from before it.
//
// A token this build cannot read (a newer build's) is logged and skipped: the
// turn still runs, and reads exactly as one woken by a wake no change feed
// derived, which carries none.
func (e *Engine) observeTriggers(ctx context.Context, evs []*events.Event) {
	if e.router == nil {
		return
	}
	for _, ev := range evs {
		token := notify.TriggerOf(ev)
		if token == "" {
			continue
		}
		at, err := statelog.ParsePosition(token)
		if err != nil {
			log.WarnContext(ctx, "turn_trigger_unreadable", "event", ev.ID.String(),
				"trigger", token, "error", err.Error(),
				"detail", "the turn runs, and its reads are not floored at what woke it")
			continue
		}
		e.router.Observe(at)
	}
}
