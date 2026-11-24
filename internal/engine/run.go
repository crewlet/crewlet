package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/skillsync"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlaswake"
	"github.com/crewlet/crewlet/internal/auxspend"
	awsint "github.com/crewlet/crewlet/internal/aws"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlet"
	gitletcompact "github.com/crewlet/crewlet/internal/gitlet/compact"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/gitlet/mirror"
	"github.com/crewlet/crewlet/internal/gitlet/readaudit"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iamdomain"
	personinbox "github.com/crewlet/crewlet/internal/inbox" // a PERSON'S, apart from the seat's (internal/agent/inbox)
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/version"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Engine is one process running a company.
//
// It owns the epoch, the backends and the node, and the wiring between them;
// the guard order, the turn's rules and the seat math belong to the inbox,
// turn and placement packages.
type Engine struct {
	// epoch is the company this engine is running, replaced whole by an
	// apply and never mutated, except its tool registry's shared MCP tools,
	// which the bridge's supervision refiles (mcpsupervise.go). See epoch.go.
	epoch epoch

	// directoryNudge carries the identity applier's post-commit signal to
	// the party registry's rebuild loop. One slot is the coalescing window
	// ([Engine.nudgeDirectory]).
	directoryNudge chan struct{}

	backends *Backends
	node     *node.Node

	// githubIssuer holds the installation tokens this process has issued its
	// agents' GitHub Apps, one per installation and scope, in memory only.
	// ONE for every reader of a seat's token, so an installation costs one
	// mint an hour however many ask, and a refusal one request per wait of
	// [Engine.seatRetryDelay]. See githubtoken.go.
	githubIssuer github.Issuer

	// awsIssuer holds the AWS sessions this process has issued its agents,
	// each until it runs short ([awsSeatIssuer]).
	awsIssuer awsint.Issuer

	// googleIssuer holds the access tokens issued to agents' Google
	// accounts, the cloud identity's and the growth identity's, one per
	// account and scope set, in memory only. See googlecloudtoken.go and
	// googlegrowth.go.
	googleIssuer googleauth.SeatIssuer
	// issuers replaces the integrations that issue seat credentials, for a
	// test of their shared contract; nil is the build's ([Engine.seatIssuers]).
	issuers []seatIssuer
	// boxSupply replaces what the vendors put in a seat's box, for a test;
	// nil is theirs ([Engine.boxDefaults]).
	boxSupply func(c *config.Company, handle string) []seataccess.BoxDefault
	// githubPulls reads a pull request for a chat link's preview through the
	// agents' apps the participant lookup uses, rebuilt with it and emptied
	// when GitHub is retired. See githubpulls.go.
	githubPulls githubPullReader
	// changePoll is where the poll of open changes stopped (ADR-0121).
	changePoll changePoll
	// tokenWaits is what this node's polls have said of running jobs whose
	// seat's issued tokens could not be issued, so each wait is said once per
	// job. See issued.go.
	tokenWaits tokenWaits

	// authEvents is this node's authentication audit trail, and
	// stopAuthTrail ends its loop. See authevents.go.
	authEvents    *authevents.Trail
	stopAuthTrail func()

	// externalBase is where a PERSON reaches this deployment:
	// `api.external_url` without its trailing slash, the base of every link
	// somebody clicks ([Engine.publicBase]). webhookBase is where a VENDOR
	// reaches it ([config.API.WebhookBase]): the public listener once
	// `api.public` is set (webhook routes are served only there), else
	// `api.external_url`; every vendor registration, manifest and teardown
	// uses it, no person's link does. webhookBaseKey is the Tier A key it is
	// read from ([config.API.WebhookBaseKey]), from the same block so the two
	// cannot name different listeners; a pass with no address names it. On the
	// engine, not an epoch: process facts, fixed at boot.
	externalBase   string
	webhookBase    string
	webhookBaseKey string

	// adminPosture is this node's own `api.auth.admin`, published on every
	// heartbeat, empty on a node serving no API. Tier A, read once at boot: a
	// fleet compares these to catch a rolling restart that left them
	// different, which a value changing in-process would defeat. See
	// [coord.NodeStatus.AdminPosture].
	adminPosture iam.AdminPosture

	// tierATokens are the ids `api.auth.tokens` declares, the holders of every
	// `token:<id>` login this node can authenticate, with each one's grants:
	// for [Engine.HolderRecord] (a login no entry declares is nobody's) and
	// for a turn such a token woke, whose every call is bounded by the apps it
	// holds HERE (turnRequesters.Credential, ADR-0109). Read once at boot.
	tierATokens map[string][]iam.Grant

	// companyManaged is whether only named systems write the company document
	// (`api.auth.company_writers`), which decides who holds an agent's GitHub
	// App ([config.Company.SeatCredentialMode]). Tier A, read once at boot.
	companyManaged bool

	// identity is the identity duties' loops, nil on a node arming none. See
	// identityduties.go. Atomic: `/health` reads the roster from the socket's
	// tick while a stop clears it.
	identity atomic.Pointer[identityDuties]

	// configWriter is how a disconnect removes a block, installed by the
	// config surface's wiring. Atomic: the loop reads it while the API
	// installs it.
	configWriter atomic.Pointer[ConfigWriter]

	// grantSaver saves a chosen request's grant ([Engine.UseGrantSaver]);
	// atomic, as the API installs it while requests read it.
	grantSaver atomic.Pointer[GrantSaver]

	// postures are the seats' live facts an item's flow reads
	// ([Engine.UsePostures]); atomic, like grantSaver.
	postures atomic.Pointer[worklet.Postures]

	// seatAccessPending is why the migration onto per-seat access left each
	// integration (by integration.Kind) unmigrated on its last attempt, for
	// the access pass's finding ([Engine.MigrateSeatAccess]).
	seatAccessPending sync.Map

	// offers is each integration's per-seat offer, read live from its vendor
	// and reused per revision ([Engine.seatOffers]).
	offers offerCache

	// setupRunner is the provisioning runner this node serves, built once.
	// ONE INSTANCE: [setup.Runner.Hold] guards a surface with an in-process
	// claim as well as a fleet lease, so separate runners' claim maps guard
	// nothing against each other. The operator's pass, the reconcile tick and
	// a disconnect's teardown all take it from here.
	setupRunner func() *setup.Runner
	// metrics is the process's one recorder, from [Options.Metrics].
	metrics *metrics.Recorder
	// answerBudget is the node's answer budget, which every payload answerer
	// it registers and every box collection it makes acquires from
	// ([queue.AnswerBudget]; answermeter.go). Built once, in [New].
	answerBudget *queue.AnswerBudget

	// deadLetterLedger is the queue's dead-letter verbs as this node's wake
	// feeds and operator surface call them, accounting for every entry a
	// replay or discard drops so the alarm reading never counts it as a wake
	// the retention lost (feeddeadletters.go). One per process, built in
	// [New]. Nil on an engine literal a case builds without a queue.
	deadLetterLedger *deadLetterLedger

	// objectives is the node's service objective meter, which every artifacts
	// recording site counts into and the alarm table reads burn rates from.
	// One per process, so a burn is judged over every event this node
	// measured. Never nil on an engine [New] built.
	objectives *objectives.Meter

	// searching is how many knowledge scans this node runs now, the gauge
	// behind `crewlet.tracker.search.concurrency`. On the engine, not the
	// searcher: that is rebuilt per epoch, and a rebuilt counter under an
	// in-flight scan would decrement a fresh zero to minus one.
	searching atomic.Int64

	// mode is what this node started for, and incarnation is this PROCESS's
	// identity: the facts the capacity window's handshake and barrier use.
	mode        statelog.MaintenanceMode
	incarnation string

	// id is this node's stable identity, resolved once at construction. A
	// placement, not a process: what registers under it survives a restart.
	id string

	// duties names every fleet duty this engine claims, so a graceful stop
	// can give each back. See [Engine.releaseDuties].
	duties claimedDuties

	// keyMint keeps two goroutines from minting one company key at once, and
	// personBlinds is the identity estate's blinder once resolved. See
	// companykeys.go.
	keyMint      mintGate
	personBlinds personBlinds

	// startedAt is when THIS engine was built, i.e. the node started; the API
	// reports the same instant. Carried on the presence heartbeat so a peer
	// can tell a week-old node from one restarted a minute ago.
	startedAt time.Time

	// posture reports this node's config lag, from whoever owns the control
	// plane. Nil publishes none ([Engine.SetPosture]).
	posture atomic.Pointer[func(context.Context) (string, error)]

	// onPublished is run after each published company is converged on, for
	// surfaces derived from it. Nil runs nothing ([Engine.SetOnCompanyPublished]).
	onPublished atomic.Pointer[func(context.Context)]

	// onCatalogue is run after the shared bridge's supervision refiled the
	// current epoch's catalogue. Nil runs nothing ([Engine.SetOnCatalogueChanged]).
	onCatalogue atomic.Pointer[func(context.Context)]

	// onInbox is told whose inbox each committed batch of a domain's applier
	// moved here. Nil tells nobody ([Engine.SetOnInboxMoved]).
	onInbox atomic.Pointer[func([]personinbox.Movement)]

	// onInboxGap is told that the movements of one source onInbox hears have
	// a gap. Nil tells nobody ([Engine.SetOnInboxGap]).
	onInboxGap atomic.Pointer[func(string)]

	// chatLive is the chat live half the process attached, where a committed
	// chat batch is announced to this node's sockets. Nil announces to
	// nobody ([Engine.SetChatLive]).
	chatLive atomic.Pointer[ChatLive]

	// liveRelay is this data node's live relay: the batches its appliers
	// announced, for a peer whose copy is out of service, and the follower
	// hearing a peer's while this node's own copy is. stopLiveRelayFn ends
	// both. Nil without `data`. See liverelay.go.
	liveRelay       atomic.Pointer[liveRelay]
	stopLiveRelayFn func(context.Context)
	// relayWire signs every ask and answer of that relay and verifies its
	// peers', built in [New] from the fleet keyring on every node; the relay
	// exists only on a data node and is refused rather than armed unsigned
	// ([newRelayWire]).
	relayWire *relayWire

	// onIdentity is told whose credentials each committed identity batch
	// moved here. Nil tells nobody ([Engine.SetOnIdentityMoved]).
	onIdentity atomic.Pointer[func(iamdomain.Moved)]

	// artifactsListeners is told what each committed artifacts batch moved
	// here ([Engine.OnArtifactsMoved]).
	artifactsListeners moveFanout[artifactsdomain.Moved]

	// artifactDataListeners is told what each committed datastore batch moved
	// here ([Engine.OnArtifactDataMoved]).
	artifactDataListeners moveFanout[artifactdata.Moved]

	// dataFootprint signals the datastore footprint sampler that a batch
	// committed ([Engine.runDataFootprint]). Made by [Engine.startCore] before
	// any applier runs; nil without the estate, where nothing sends.
	dataFootprint chan struct{}

	// assistantErasure signals the assistant's erasure loop that the identity
	// applier removed somebody ([Engine.runAssistantErasure]). Made by
	// [Engine.startCore].
	assistantErasure chan struct{}

	// ownsBackends says whether Stop closes them. Separate from use: a
	// borrowed Backends is still what this engine requeues and pauses through,
	// and one nilable field for both made the park path reach for a nil queue
	// on lent engines.
	ownsBackends bool

	// dispatch turns one inbox partition into one turn. Held so a test can
	// substitute its own without a broker.
	dispatch *Dispatcher

	// modelHolds is every seat inbox this node paused because its company had
	// no model, which the apply that brings one releases. See nomodels.go.
	modelHolds modelHolds

	// inboxes is the attachment of each seat's inbox this node holds, which
	// every hold on one names. See seatinbox.go.
	inboxes seatInboxes

	// budgetParks is every seat inbox held because a capped token window is
	// refusing, each with the alarm that releases it when the window turns
	// over. See budgetpark.go.
	budgetParks budgetParks

	// floorWaits is every delivery whose launch parked its seat for below its
	// floor, by seat and the ids of the delivery's events, with the window the
	// park waited on, for the next launch to read. See budgetpark.go.
	floorWaits floorWaits

	// providerParks is every seat inbox held because every model of the
	// seat's chain is limited, each with the alarm that lifts it for a probe
	// when the first is tried again. See providerpark.go.
	providerParks providerParks
	// failures is each seat's latest failure a retry cannot cure: its second
	// in a row pauses the seat (failurehold.go).
	failures lastFailures
	// conversations is each held seat's held conversations as this node read
	// them, and which conversations' last run ran out of context.
	conversations heldConversations

	// limitProbes is one probe per limited chain, which settles every seat
	// parked on it. See providerpark.go.
	limitProbes limitProbes

	// parkNotices is every seat's provider park notice not yet published: one
	// per change, retried while the broker refuses. See parknotice.go.
	parkNotices parkNotices

	// clock is the instant every token-budget window is read at; nil is the
	// wall clock, which every engine [New] builds runs on. See [Engine.now].
	clock func() time.Time

	// pauses is this node's watched copy of every seat a person paused, and
	// the inbox holds taken for them. See seatpause.go.
	pauses seatPauses

	// runners is this node's executor-cell readiness and its loop
	// ([runnerProbe]), published on the node's status record.
	runners runnerProbe

	// hostLogins is what each model entry's harness says of its login on this
	// node's disk, and its loop ([hostLogins]).
	hostLogins hostLogins
	// verified is each model's last answer heard by a test of its credential
	// rather than by its own calls ([Engine.Verified]).
	verified verifiedAnswers

	// retries is this node's watched copy of the fleet's model retries
	// ([modelRetries]).
	retries modelRetries

	// restarts is this node's watched copy of the fleet's MCP restarts
	// ([mcpRestarts]).
	restarts mcpRestarts

	// oauth is this node's part in its MCP servers' OAuth sign-ins: the state
	// signer and each server's access token source ([oauthState]).
	oauth oauthState

	// listings is this node's published MCP listings and its watched copy of
	// every node's ([mcpListings]).
	listings mcpListings

	// mintFindings holds the findings the credential judgement reads off the
	// path of every caller ([Engine.mintingStates]).
	mintFindings mintFindings

	// statusRecords is this node's status record writer: its MCP, model and
	// runner rows and infra summary, beside its presence (nodestatusrecord.go).
	statusRecords statusRecords

	// growthSignals is what this node's growth products met at their vendors,
	// published on its status record ([Engine.GrowthSignals]).
	growthSignals atomic.Pointer[growth.Signals]

	// reviews is this node's watched copy of the definitions the fleet's MCP
	// tools' levels were decided on ([mcpReviews]).
	reviews mcpReviews

	// signIns is the device sign-ins this node runs, and the watch that hears
	// their cancels ([deviceSignIns]).
	signIns deviceSignIns
	// modelChecks is the model checks this node runs on its own time
	// (modelcheckrun.go).
	modelChecks modelChecks

	// testNode is the node id an engine built with no node answers for
	// itself, for a case standing up several engines on one coordination store.
	testNode string

	// checkHomes is where a candidate credential's test makes its throwaway
	// home ([Engine.CheckModel]); empty is the temporary directory.
	checkHomes string

	// entryInputs is the last good read of the per-entry alarms' fleet-wide
	// inputs ([Engine.entriesReading]).
	entryInputs entryInputs

	// blockedSaid is the last word of each live node on its seats' runs
	// waiting for an operator ([Engine.executorReading]).
	blockedSaid blockedSaid

	// applying serialises [Engine.Apply] against [Engine.Drain]; stopped,
	// which it guards, refuses an apply once a drain has begun.
	applying sync.Mutex
	stopped  bool

	// watchdog ends this process when the seat host's heartbeat stops turning
	// past the lease TTL. On the engine, not the seat host, for
	// [seat.Watchdog.Stop]'s reason: disarming must happen BEFORE the drain,
	// because teardown legitimately blocks long (reaping MCP trees, joining
	// goroutines, tearing sandboxes down) and an armed watchdog would shoot a
	// node mid-release.
	watchdog *seat.Watchdog

	// shuttingDown is set at the first moment of [Engine.Drain], never cleared
	// ([Engine.ShuttingDown]).
	shuttingDown atomic.Bool

	// drainOnce makes the drain one operation however many callers ask:
	// [Engine.Stop] drains for a caller that did not, and a second run would
	// announce a second stop.
	drainOnce sync.Once

	// stopBudget is the allowance every lease this node's stop gives back
	// shares ([seat.StopBudget]), built by whichever of drain and teardown
	// reaches it first ([Engine.stopping]). Written once inside
	// stopBudgetOnce, read only through it.
	stopBudget     *seat.StopBudget
	stopBudgetOnce sync.Once

	// teardownBegun is done from the moment the TEARDOWN begins
	// ([Engine.tearingDown]), never the drain (whose wait for running turns
	// the integration loop runs straight through); a give-back no stop's
	// context reaches watches it to become a step of the stop
	// ([Engine.stepOnceTornDown]). Made on first use under teardownOnce, so
	// the zero engine a failed boot tears down has one.
	teardownOnce  sync.Once
	teardownBegun context.Context
	beginTeardown context.CancelFunc

	// stopOnce makes the teardown [Engine.Stop] runs after the drain run once:
	// every step runs against what the first closed, so a repeat warned that
	// the admission could not be withdrawn (telling an operator to exclude a
	// node already removed) and announced `engine_stopped` twice.
	stopOnce sync.Once

	// sshRunner runs the SSH tools' commands, holding each turn's read
	// certificate in memory until it ends ([Engine.sshRun]).
	sshRunnerOnce sync.Once
	// growthOnce makes the growth products once ([Engine.growthProducts]).
	growthOnce sync.Once
	growth     []builtin.GrowthProduct
	sshRunner  *sshint.Runner

	// batch is the inbox coalescing window and cap, shared with every seat
	// attachment here. ONE VALUE on the engine, mutated in place:
	// [queue.BatchOptions] guards its own fields so a hot reload takes effect
	// on the next batch without re-subscribing, where a fresh value per attach
	// would leave an earlier-claimed seat on the old window. It carries
	// notification_coalesce_window_seconds and notification_coalesce_max_batch.
	batch *queue.BatchOptions

	// sandbox is this node's code-work machinery: the coordinator holding the
	// busy set and the durable row store behind it. On the engine, not an
	// epoch: rebuilding on an apply would forget which seats are mid-run.
	//
	// An atomic pointer, published whole at most once (nil until the first
	// company reaching a sandbox cell, never cleared), for [Engine.native]'s
	// reason: a node booted with no sandbox has a seat host, dispatcher and
	// tool loop reading it before an apply brings it up. Every reader loads it
	// once. See [Engine.startSandbox].
	sandbox atomic.Pointer[sandboxRuntime]
	// sandboxStart serializes the build and publish of sandbox, so no
	// coordinator is built to lose the race: each holds this process's taker
	// lease from its build ([Engine.publishSandbox]).
	sandboxStart sync.Mutex

	// sandboxWaiter is this node's completion poll, for the boxes it launched,
	// which also claims the launch-node watch ([sandbox.Waiter]). It starts
	// once, on a node that publishes, when a runtime and the node exist.
	sandboxWaiter atomic.Pointer[sandbox.Waiter]

	// sandboxPollInterval is Options.SandboxPollInterval, kept because an
	// apply can start the poll.
	sandboxPollInterval time.Duration

	// directoryRefresh is Options.DirectoryRefresh: the net's interval when
	// set, [DirectoryRefresh] otherwise.
	directoryRefresh time.Duration

	// gitHookArgv is Options.GitHook, kept because an apply can build Gitlet
	// ([Engine.gitHook]).
	gitHookArgv []string

	// cacheDisk is Options.cacheDisk, kept likewise: nil (every engine built
	// outside this package's tests) reads the node's disk ([Engine.cacheVolume]).
	cacheDisk *cacheVolume

	// sizedByTheMachine is the reason [SizedByTheMachine] put on the context
	// New was given, kept likewise: "" holds Gitlet in a test to declared
	// figures ([Engine.undeclaredCodeCache]).
	sizedByTheMachine string

	// gitMemory is Options.gitMemory, kept likewise: zero has the git pool
	// read the node's own memory ([Engine.buildGitletHost]).
	gitMemory int64

	// setMemoryLimit is Options.setMemoryLimit, kept likewise ([Engine.limitMemory]).
	setMemoryLimit func(int64) int64

	// compactorEstate is Options.compactorEstate, kept likewise: nil hands the
	// compactor the router's code seam as it is ([Engine.buildGitletHost]).
	compactorEstate func(gitletcompact.Estate) gitletcompact.Estate

	// homeSeams is Options.homeSeams, kept likewise: nil hands the home's
	// change work the router's code seam and Gitlet's chain builder as they
	// are ([Engine.buildGitletHost]).
	homeSeams func(gitlethome.Estate, gitlethome.Chains) (gitlethome.Estate, gitlethome.Chains)

	// committerPublisher is Options.committerPublisher, kept because the first
	// apply that hosts code builds its committer: nil hands it the code log's
	// write authority as it is ([Engine.startNative]).
	committerPublisher func(gitlet.Publisher) gitlet.Publisher
	// plans is the node's registry of harness sign-ins ([harness.Plans]),
	// made once at boot under a key derived from the keyring and handed to
	// every revision's build, so a sit-out outlives every apply and a
	// sign-in's id is the same on every node.
	plans *harness.Plans

	// fakePlay is Options.fakePlay: what a `providers.sandbox.fake` company's
	// runs do, nil for the double's own demonstration. Kept because every
	// apply rebuilds the catalogue.
	fakePlay fakePlayer

	// mirrorSeams is Options.mirrorSeams, kept likewise: nil builds a Gitlet
	// node's mirrors with the one outbound dial and the wall clock
	// ([Engine.buildMirrors]).
	mirrorSeams func(mirror.Options) mirror.Options

	// sandboxSeats are the seats whose sandbox control topic this node has
	// attached, so a seat is attached once however it was prepared (its own
	// acquisition, or the apply that brought the runtime up while it was
	// held). See [Engine.prepareHeldSeats].
	sandboxSeatsMu sync.Mutex
	sandboxSeats   map[string]bool

	// leaseTTL is the seat lease TTL, also the seat lease bucket's age,
	// resolved once from Tier A. It is a CEILING for every SEAT claim this
	// engine makes outside the seat host: the KV's expiry is bucket-wide, so
	// it refuses a seat lease asked to outlive it, and the mailbox retirement
	// claims a removed seat's lease with exactly this TTL. Only a seat or
	// presence lease is bounded by it (coord.HeldToSeatTTL); duty and walk
	// claims live in a bucket ceilinged by coord.MaxDutyTTL, and a duty
	// clamped to this value lapsed between its own ticks.
	leaseTTL time.Duration

	// memory carries a seat's memory between the nodes that run it. On the
	// engine, not an epoch, like the sandbox machinery: its per-seat
	// watermarks record what THIS PROCESS has published, and rebuilding on an
	// apply would republish every seat's history. Nil on a node with no broker
	// or store.
	memory *memsync.Syncer

	// memorySync is the loop that publishes it, and Stop's handle on it. Nil
	// where memory is.
	memorySync *memorySync

	// fill is the loop that fills the vectors the memory of this node's seats
	// is missing (memoryfill.go), and Stop's handle on it. Nil on a node with
	// no store or seat host.
	fill *memoryFill

	// sandboxOtel mints each launch's telemetry endpoint. Nil exports nothing
	// from inside a box, the ordinary configuration. Held here and handed OUT
	// to the API this process serves, not built twice: that API verifies the
	// tokens this engine minted, and one object cannot drift from its twin. A
	// peer verifies with its own receiver, so the key derives from the fleet's
	// keyring.
	sandboxOtel *sandbox.OtelReceiver

	// bridge serves a running seat's tool surface to its executor's harness
	// over MCP. Nil where no bridge URL is configured and this node serves no
	// bridge route, which refuses every executor launch here, naming the
	// variable. leaveBridgeFleet withdraws it from the fleet (bridgefleet.go).
	bridge           *mcpbridge.Bridge
	leaveBridgeFleet queue.Unsubscribe

	// sessions is where every phase the engine drives serves its tools to its
	// harness on this host: the PROCESS's one [toolsession.Host], a directory
	// only the engine's user can open, one socket per harness run, dialled by
	// `crewlet mcp-relay` ([RunnerInput.Sessions]). The executor's run, in a
	// box, uses the bridge above instead. On the engine, made once by [New]
	// and removed by the teardown after every turn has ended, so it has an
	// owner. Never nil on an engine [New] built. It binds no port.
	sessions *toolsession.Host

	// boxTokens mints and verifies a coding box's git credential. Built on
	// EVERY node: the node that launches a run mints and whichever Gitlet node
	// its box dials verifies, so each derives its key from the fleet keyring.
	// Gitlet admits boxes by it ([host.Options.Boxes]).
	boxTokens *host.BoxTokens
	// checkTokens does the same for a check's box, on every node for the same
	// reason: the runner claiming a check mints, a Gitlet node verifies.
	checkTokens *host.CheckTokens

	// boxGitURL is Gitlet's base a coding box clones and pushes through, read
	// by [ReadBoxURLs] from [sandbox.BoxGitURLVar]. "" refuses every run this
	// node launches, naming the variable, rather than handing out clone
	// commands that dial nowhere. Read once at boot.
	boxGitURL string

	// reflector is the learning write side: one dispatcher for the life of
	// the process, whose org and workers an apply swaps. On the engine because
	// its redelivery ring is process state (learning.Reflector). Atomic: every
	// held seat hands its reflection wakes to it from queue goroutines while
	// the first apply on a node booted with none builds it.
	reflector atomic.Pointer[learning.Reflector]

	// core is this node's always-on runtime: the state log for every
	// registered domain, the node gate, and the identity estate's two sides.
	// See core.go. Published once by [New] on EVERY data node and never
	// cleared, not even by the teardown; nil for the life of a node without
	// `data`, which reaches the estate through [Engine.router]. On the engine
	// because the runtime follows the fleet's logs, which a company revision
	// does not change. Atomic and monotonic nil-to-runtime, so lock-free
	// readers may rely on a later read.
	core atomic.Pointer[core]

	// native is the half of this node's durable state only a company can say
	// anything about: the tracker's and knowledge base's read and write sides
	// over the core's logs, the lexical index and the change feeds. Nil before
	// a company, and on a node without `data`, which runs [Engine.remote]
	// instead (native.go). Atomic, written at most once, by the node's first
	// company ([Engine.startNativeFor], at boot or apply), never cleared: a
	// later plain write would race the seat host, API and tools already
	// reading. Monotonic like core.
	native atomic.Pointer[native]

	// remote is native on a node holding no data: which halves its seats'
	// tools reach through the router. At most one of the two is set, on the
	// same terms (remote.go).
	remote atomic.Pointer[remoteNative]

	// router is how every seat tool on this node reaches the estate: its own
	// copy while in service, another data node's otherwise. Built in [New] on
	// EVERY node before anything publishes (router.go). local is this data
	// node's own copy, nil without data.
	router *estate.Router
	local  *localEstate

	// toldUnreadable is every told record whose changes this node could not
	// decode and has said so once (capability.go).
	toldUnreadable sync.Map

	// executorLog is the executor log's writes the sandbox runtime records
	// every capture of a launch's session and every turn's end through: the
	// router's (estate.ExecutorWrites), set in [New]; nil records neither. A
	// field, not a router read per launch, so the coordinator is built over
	// one answer.
	executorLog executorLogWrites

	// executorHeads is the read a turn's dispatch makes of its conversation's
	// head ([Engine.readConversation]): the router's (estate.ExecutorReads),
	// set in [New]; nil means turns read no conversation.
	executorHeads headReader

	// abandonedLaunches are the turns whose executor launch was abandoned
	// AFTER writing its run's record (refused by the turn's fence, or failed
	// on its way to its box), by run id: the launch abandoned that record, so
	// no ending of the run owes the turn's end and the frame ending the turn
	// records it ([Engine.runTurn], [Engine.resumeTurn], each taking its own
	// off). Set by [agentLauncher.launch].
	abandonedLaunches sync.Map

	// resumedSegments are the turns with a resumed segment running here, by
	// run id, each with the company the segment was admitted under
	// ([resumeInput.Company]): the revision a relaunch runs on, which answers
	// whether a relaunch would continue the session ([Engine.relaunchCompany]).
	// Set by [Engine.resumeTurn] while it runs.
	resumedSegments sync.Map

	// envelopes sign every estate request this node sends and reply it
	// answers, and verify both directions; one value for router and server
	// ([estateEnvelopes]).
	envelopes *estate.Envelopes

	// routedDirectory is the party registry's directory trigger on a node
	// without `data`, which reads the identity directory through a data node;
	// nil on a data node, whose core runs its own ([Engine.startRoutedDirectory]).
	routedDirectory *directoryWatch

	// custody publishes a stateless node's events for a data node to keep; nil
	// on a data node. keeper is a data node's half, writing them into its own
	// log; nil elsewhere.
	custody *observe.Custody

	// issueReports is the opt-in defect reporter (ADR-0101); nil on an engine
	// with no broker, which has nothing to detect.
	issueReports *issuereport.Reporter
	// endIssueSender stops the sender [Engine.startIssueSender] started.
	endIssueSender func()
	keeper         *observe.Keeper

	// nextStart is [Options.NextStart].
	nextStart *issuereport.NextStart

	// stopEstate withdraws this data node as an estate server, nil where it
	// serves none ([Engine.serveEstate]).
	stopEstate queue.Unsubscribe

	// stopFleetBroker withdraws this member's broker-membership subject, nil
	// where it serves none ([Engine.serveFleetBroker]).
	stopFleetBroker queue.Unsubscribe

	// brokerWire signs every broker-membership request this node sends and
	// reply it answers, and verifies both; built in [New] on EVERY node, since
	// any node asks ([newBrokerWire]).
	brokerWire *brokerWire

	// dataView watches the fleet's presence leases and answers every
	// per-request question about data nodes: which one a stateless node's tool
	// call goes to, and which divide a search. Nil without coordination. Built
	// in [New], run from [Engine.startDataView], stopped by [Engine.teardown]
	// (remote.go).
	dataView     *coord.LeaseView
	stopDataView func()

	// claims is the seat-claim gate as the seat host asks it, plus the watch
	// that asks the host for a pass when the gate opens after refusing one;
	// stopAdmission ends it, nil while none runs. Built in [New]
	// (admission.go).
	claims        *gateWatch
	stopAdmission func()

	// objects is this node's object store and, on a data node, the
	// collector's duty (objects.go).
	objects *objectStore

	// uploads is this node's artifact upload and publish pipeline, built on
	// first use (artifactupload.go).
	uploads artifactUploads

	// boot is the Tier A configuration this engine was built from. Immutable;
	// kept because duties an apply or later loop arms read it (the trim's
	// retention policy, the identity horizons, the backup owner).
	boot *config.Bootstrap

	// env is this node's ${VAR} resolver and the store snapshot it answers
	// from: the secret store in front of the process environment, refreshed on
	// every apply and after a provisioning pass seals a credential (secrets.go).
	// One pointer to both, so a reader never pairs a resolver from one
	// snapshot with another's values.
	env atomic.Pointer[secretView]

	// secretsMu serialises every writer of env (an apply's refresh and a
	// provisioning pass's), so the store is read under the lock the snapshot
	// is installed under and neither installs a snapshot older than the
	// other's.
	secretsMu sync.Mutex

	// environ is the environment every resolver ends in: [Options.Environment],
	// the process's when nil. Read through [Engine.environment].
	environ config.Source

	// republish coalesces the re-activations a provisioning pass asks for when
	// it seals a credential (republish.go).
	republish republisher

	// cipher is the keyring this node seals and opens secret rows with.
	// [New] refuses a node without one, so it is nil only on a hand-built
	// Engine in a test. ONE per process keeps a row this node wrote readable.
	cipher secrets.Cipher

	// profile is what this node declared it does: claims seats, serves inbound
	// traffic, runs the fleet's singleton duties. The duty gate reads it on
	// every claim (duty.go).
	profile placement.NodeProfile

	// learning is the loops of the background passes no turn drives: episode
	// compaction, skill ageing, clustered synthesis and promotion. On the
	// engine like the sandbox waiter: a rebuild on an apply would restart
	// their clocks. Every apply hands over the revision's work
	// ([Engine.startLearningBackground]).
	learning *learning.Background

	// notify is this node's inbound edge: the party registry, the notification
	// service and the third-party app transports. On the engine, not an epoch:
	// a transport holds live sockets and resolved identities, so rebuilding on
	// every apply would drop every connection. Only the registry inside is
	// swapped per epoch (notifications.go).
	notify notifications

	// skills is this node's tool-skill registry, built once, OUTLIVING every
	// epoch: its content comes from the knowledge base, not config, and a
	// per-epoch rebuild would empty it on every apply and leave seats without
	// their company's guidance until the next sync walk, possibly never on a
	// webhook-driven sync.
	skills *skills.Registry

	// skillSync is the one loop that keeps [Engine.skills] current: the source
	// each apply names, page changes this node and its peers heard, a periodic
	// walk and a bounded retry (skillsync.go). On the engine for the
	// registry's reason.
	skillSync *skillsync.Syncer

	// mcp supervises every MCP child this NODE runs: the shared servers and
	// the per-role children of its seats. One per engine: the children are
	// processes, and an apply replacing the bridge would orphan every one.
	mcp *mcp.Bridge

	// mcpFileMu serialises filing the shared bridge's catalogue into an
	// epoch's registry outside an apply's own build: what supervision changed
	// (a retried start that came up, a restart with different tools) into the
	// current epoch, and the bridge as it stands into an epoch being published
	// (mcpsupervise.go). Its own lock, not applying: the supervisor calls the
	// refile, and an apply's Stop waits for it.
	//
	// mcpRetryBase is the restart schedule's first wait for every bridge and
	// seat runner, zero for [mcp.RestartBase]; [Options.mcpRetryBase].
	// mcpRelistBase is the soonest a running MCP session is listed again, zero
	// for [mcp.RelistFloor]; [Options.mcpRelistBase].
	mcpFileMu     sync.Mutex
	mcpRetryBase  time.Duration
	mcpRelistBase time.Duration

	// seatMCP is one bridge per held seat, for its per-role children
	// (seatservers.go). Separate from the shared bridge because a catalogue is
	// keyed by tool name, and two seats' children of one template publish the
	// same names. seatTools is that seat's registry (the epoch's surface
	// cloned, the bridge's catalogue filed in). Both live here, per seat
	// lease, not on the per-epoch [Company], are guarded by mcpMu and must
	// never disagree: a registry naming a gone bridge's tools offers entries
	// that only fail. mcpShared and mcpSeats are each server start's outcome
	// for [coord.NodeStatusRecord.MCP], under mcpMu, forgotten with the seat's
	// bridge.
	mcpMu     sync.Mutex
	seatMCP   map[string]*seatChildren
	seatTools map[string]*tools.Registry
	mcpShared []mcpOutcome
	mcpSeats  map[string][]mcpOutcome

	// hooks caches the webhook verification material for the company it was
	// assembled from; see [Engine.WebhookSecrets] for the keying and why lazy.
	hooks struct {
		mu       sync.Mutex
		built    *Company
		material webhooks.Secrets
	}

	// embeddings is the company's vector backend, swapped on apply. Atomic,
	// not a mutex: read on the turn's prefetch hot path, written only by an
	// apply, so a read must not queue behind anything.
	embeddings atomic.Pointer[embeddings.Embedder]

	// maintenance is the retention sweep for the short-horizon tables. On the
	// engine like the sandbox machinery: a loop this process runs, which an
	// apply would churn for nothing new to sweep.
	//
	// Rebuilt once more on a node's FIRST company, the one time its job list
	// changes after boot (an unconfigured boot has no native halves and sweeps
	// no inbox). Horizons need no rebuild; each is asked per sweep. The old
	// worker is stopped, its in-flight tick waited out, before the new one
	// starts, so there is never a second loop. Atomic because that write
	// happens while running ([Engine.rebuildMaintenance]).
	maintenance  atomic.Pointer[maintenance.Worker]
	integrations *integration.Worker

	// atlasCollect reads each connected integration's vendor whole for the
	// atlas, a fleet singleton on a data node that publishes
	// ([Engine.startAtlasCollect]).
	atlasCollect *collect.Worker
	// atlasWake wakes the agent seat that owns a drifting atlas entry
	// ([Engine.startAtlasWake]).
	atlasWake *atlaswake.Worker

	// mailboxes registers each seat's mailbox with the fleet and retires a
	// departed seat's, swept by the maintenance duty. Built BEFORE the node,
	// which registers through it. Nil with no fleet store.
	mailboxes *maintenance.Mailboxes

	// following is held by a reconciler tick from its read of the pointer to
	// its record of what this node now serves ([Reconciler.Tick]), so one
	// engine follows the pointer one tick at a time. On the engine, not the
	// reconciler, because it orders the engine's apply: interleaved ticks let
	// the one that read the OLDER epoch apply last and walk the boot row back.
	// A second reconciler ([Engine.NewReconciler] does not refuse one) is held
	// to the same order.
	following sync.Mutex

	// reconciler is the loop converging this engine on the activation
	// pointer, recorded by [Engine.NewReconciler]. Read by what answers for
	// the epoch the fleet activated rather than the one this node applied:
	// [Engine.CodeUnserved] (a revision hosting code still being applied vs
	// one hosting none) and [Engine.servedFleetRevision] (the mailbox sweep's
	// roster, every irreversible judgement against the active revision, and
	// this node's lag). An apply is handed a document, not an epoch.
	reconciler atomic.Pointer[Reconciler]

	// rewired remembers what the last seat-identity retry resolved on each
	// surface, so recovery is logged on the transition, not every pass
	// ([rewireLog]).
	rewired rewireLog

	// retention is the state log's trim: the fleet singleton that decides how
	// far each domain's log may be purged and publishes what it concluded. On
	// the engine like maintenance: a rebuild on an apply would leave two loops
	// publishing one floor. Atomic: armed after the node exists, from the
	// boot's publishing tail, while the API may already read it for `crewlet
	// retention status`; cleared by the teardown.
	retention atomic.Pointer[retention]

	// alarms is the retention loop's alarm tracker, published for the API's
	// health envelope ([Engine.Alarms]). Atomic and never cleared: read from
	// every health request and push tick on goroutines this engine does not
	// own while the writing loop starts and stops, so a plain field would
	// race startRetention, and nil-ing on stop would turn a draining node's
	// last reading into "no evaluation has ever run".
	alarms atomic.Pointer[statelog.Tracker]

	// accessReviews is this node's debounce of its seats' access reviews
	// (accessreview.go). Atomic: armed by Start while an apply may already
	// kick it, stopped by the drain.
	accessReviews atomic.Pointer[accessReviews]

	// waitingWatch stops this node listening on the seats' waiting channel
	// (queued.go); nil when not listening.
	waitingWatch queue.Unsubscribe

	// budgetReports is the live token-meter loop. Every node runs one: the
	// counters are shared, so it is a frame, not a duty.
	budgetReports *budgetReporter

	// inboxReports is this node's waiting-mail loop over its seats
	// (inboxreport.go); nil until Start arms it.
	inboxReports *inboxReporter

	// refusals is how this node's gates tell that loop a window has started
	// refusing ([refusalLatch]). Built with the engine, not the loop: the
	// gates charge through it from the first apply.
	refusals *refusalLatch

	// embedding is the vector domain's one writer: the fleet singleton that
	// turns sources whose text has moved into vector records. On the engine
	// like the trim: a rebuild on an apply would leave two loops holding one
	// provider budget. It reads the current epoch's embedder per tick, so a
	// model change lands without a restart.
	embedding *embedDuty

	// trashReaper is the trash reaper's duty loop (trashreap.go). On the
	// engine for the embedding duty's reason.
	trashReaper trashReaper

	// usage is this node's usage publisher: the loop that republishes the days
	// this node's own event log holds. On the engine for the embedding duty's
	// reason (a second loop would be two writers on this node's subjects); it
	// reads the epoch's clock and chart per tick.
	usage *usageLoop

	// codeRetention is the code-retention duty's loop: the fleet singleton
	// that purges repositories deleted past their retention. On the engine for
	// the embedding duty's reason; armed only where the runtime hosts code and
	// the node publishes; reads the epoch's retention per pass.
	codeRetention *codeRetentionDuty

	// codeRepos runs this node's reconciles of code.repos beside the applies
	// that ask for them ([codeReconciler]): never nil, idle on a node hosting
	// no code.
	codeRepos *codeReconciler
	// chatPrune is the chat domain's retention duty: the fleet singleton that
	// publishes the cutoff every node deletes below. On the engine for the
	// trim's reason; reads the epoch's horizon per tick, so a retention change
	// lands without a restart.
	chatPrune *chatPrune
	// delegatedSweep puts a delegated request to people once its delegates ran
	// out of time or delegated decisions were turned off: a fleet singleton
	// reading the company per pass, so no apply re-arms it.
	delegatedSweep *delegatedSweep

	// flowDuty records items' flow and tells who must know (ADR-0162).
	flowDuty *flowDutyLoop

	// usageEvery is Options.usageEvery.
	usageEvery time.Duration
	// resumeGate is Options.resumeGate.
	resumeGate func(sandbox.TurnGate) sandbox.TurnGate
	// turns is how many turns this node runs at once, node.max_concurrent
	// as its gate resolves it, read at boot since the coordinator is built
	// before the node: the reads of its runs' boxes it makes at once
	// ([sandbox.CoordinatorOptions.Collections]).
	turns int

	// history is the fleet's turn-level history reader, and stopHistoryServe
	// withdraws this node as an answerer (history.go).
	history          *eventfan.Fleet
	stopHistoryServe queue.Unsubscribe
	// stopMCPInstanceServe withdraws this node as an answerer of reads of its
	// MCP instances ([Engine.startMCPInstances]).
	stopMCPInstanceServe queue.Unsubscribe

	// steers is this node's running turns' note boxes, and stopSteerServe
	// withdraws the node as their answerer (steer.go).
	steers         *steerDesk
	stopSteerServe queue.Unsubscribe

	// connectMarks is this node's writes of a harness's bridge connection under
	// way, which a collection waits for (connectmarks.go).
	connectMarks *connectMarks

	// runSteers is this node's half of the notes a turn's executor holds on
	// its record, from launch to the turn's end (runsteer.go, ADR-0067).
	runSteers *runSteers
	// wakeRouters is this node's routers of its busy seats' held inboxes
	// (wakerouter.go, ADR-0167).
	wakeRouters seatRouters
	// runWaits is this node's watches over its seats' runs parked by
	// `wait_for_run` (runwaits.go, ADR-0176).
	runWaits runWaits

	// waitsTold is what each wake waiting behind a busy seat was told of why
	// it waits, so a person is told each reason once (foldin.go).
	waitsTold waitsTold
	// codeSearchWire is this node's code search scatter envelopes, both
	// halves, and codeSearchLocal the answerer its own searches go to where it
	// has no broker (codesearch.go).
	codeSearchWire  *codesearch.Wire
	codeSearchLocal atomic.Pointer[codesearch.Server]

	// presenceWire seals this node's presence frames and opens its peers', and
	// presence is the hub holding its rooms (presence.go).
	presenceWire *presence.Wire
	presence     *presence.Hub

	// answers seals a person's answer onto the parked run it answers and opens
	// it before a resume reads it: the coordinator's, over the fleet keyring
	// like every instruction channel ([sandbox.CoordinatorOptions.Answers]).
	answers *sandbox.AnswerSeal

	// leads seals what carried each session of a run on into the run's leads,
	// kept beside its record, and opens it for a resume that tells it again:
	// the coordinator's and the resume's ([sandbox.CoordinatorOptions.Leads],
	// [runnerResume]).
	leads *sandbox.LeadSeal

	// dataTurns is this node's tally of each turn segment's datastore ops,
	// shared by every registry it builds ([builtin.DataTurns]).
	dataTurns *builtin.DataTurns

	// folderTurns is this node's tally of the folder records each agent turn
	// segment writes, shared by every registry ([builtin.FolderTurns]).
	folderTurns *builtin.FolderTurns

	// renderState is render_artifact's renderer, built at first use, and this
	// node's tally of each segment's renders (artifactrender.go).
	renderState

	// memoryReads answers a seat's memory from the node holding it, and
	// stopMemoryServe withdraws this node as an answerer (memoryread.go).
	memoryReads     *memread.Reader
	stopMemoryServe queue.Unsubscribe

	// turnHolds is what this process's running turns hold from Gitlet's
	// expiry, and stopCodeHoldsServe withdraws the node as their answerer
	// (codeholds.go).
	turnHolds          runningTurns
	stopCodeHoldsServe queue.Unsubscribe

	// executorTails answers a running executor's live output from the node
	// that owns it, sandboxFeeds is this node's readings of the runs it owns,
	// and stopTailServe withdraws this node as an answerer (executortail.go).
	executorTails *sandbox.TailReader
	sandboxFeeds  *sandbox.LiveFeeds
	stopTailServe queue.Unsubscribe

	// stopRelaunchServe withdraws this node as an answerer of what a relaunch
	// of a seat it holds would be launched on (relaunchtarget.go).
	stopRelaunchServe queue.Unsubscribe

	// stopHeldParkServe withdraws this node as an answerer of whether one of
	// its parks holds the inbox of a seat it holds (parkheld.go).
	stopHeldParkServe queue.Unsubscribe

	// boxReads asks a box's host to read it back and answers for boxes on this
	// node's host; stopBoxReadServe and stopNudgeListen withdraw this node as
	// answerer of those reads and listener for the nudges its waiter acts on.
	// Nil without a broker (sandboxhost.go).
	boxReads         *boxReads
	stopBoxReadServe queue.Unsubscribe
	stopNudgeListen  queue.Unsubscribe

	// mcpChecks asks the fleet to test an MCP server and answers the tests of
	// the servers this node runs. Nil without a broker, which is the whole
	// fleet and tests everything itself (mcpcheck.go).
	mcpChecks *mcpChecks
	// mcpTesting is the tests running here, by [mcpTestKey]: one at a time per
	// entry and seat.
	mcpTesting sync.Map

	// scheduler is the role/unit cron tick. On the engine like maintenance: a
	// rebuild on an apply would leave two loops racing for one company's
	// fires. reconcileScheduler arms and disarms it.
	scheduler schedulerLoop

	// pickup is the work pickup's loop over the agent seats this node holds
	// (ADR-0112), and which of them its last pass found idle.
	pickup pickupLoop

	// onboarded remembers which seats this PROCESS has seen onboarded. On the
	// engine, not an epoch: it records what the process observed, and a latch
	// replaced by an apply would re-run a pass for agents already marked.
	// Keyed by chain hash, so a live restructure still re-onboards.
	onboarded *runner.Latch

	// rewrites is the cache every compaction this process makes is kept in
	// ([Engine.compactorFor]). On the engine for the latch's reason: a rewrite
	// is a function of its source text, so it stays right across an apply
	// that did not touch it, and a turn spanning that apply re-renders its
	// ledger against the same payloads.
	rewrites *compact.Cache

	// auxSpend is this node's auxiliary-spend ledger: every auxiliary
	// completion the seam sees, coalesced and published as `auxiliary_spend`
	// (auxiliary.go, internal/auxspend). On the engine for the cache's reason,
	// more strongly: a bucket is spend already made, and a ledger replaced
	// with an epoch would drop what the previous one had not flushed.
	auxSpend *auxspend.Ledger

	// codeReads is this node's reads ledger: every read of a `visibility:
	// unit` repository that Gitlet's git surface and git reads answer,
	// coalesced per reader and published as `code_fetched`
	// ([Engine.recordCodeRead], internal/gitlet/readaudit). On the engine for
	// auxSpend's reason: Gitlet is built at an apply after boot as often as at
	// boot, and a ledger tied to the runtime would drop what a rebuilt one had
	// not flushed.
	codeReads *readaudit.Ledger
}

// Options configure an engine.
type Options struct {
	Bootstrap *config.Bootstrap
	Company   *config.Company

	// ActivatedAt is when Company was activated: the instant of the activation
	// this node last applied it from, which its store keeps on the revision it
	// boots on. Zero for a company no activation has named (one built in code,
	// or an offline offer ([store.Offer]) the fleet has not taken): an offline
	// instant would put into the replicated estate a document the fleet may
	// never run, dated later than the fleet's, and refuse the fleet's own
	// stamps; the first apply of the activation that publishes it stamps it.
	// Never the boot's clock: the chart apply stamps every project with it so
	// an older configuration cannot walk a newer one back, and a stale
	// revision stamped NOW would ([Engine.applyChart]).
	ActivatedAt time.Time

	// Mode is what this node starts for: normal, or one of the two maintenance
	// modes a capacity change restarts the fleet into. A run's property, never
	// read from Tier A: a file carrying it would leave a node that came back
	// after an unrelated restart still refusing to publish. Empty is normal.
	Mode statelog.MaintenanceMode

	// Recovered is called once from New between the two halves of a boot, on
	// every boot that gets that far. Nil calls nothing.
	//
	// Before it is the node's own (store, broker, streams, the replicated
	// estate, state logs, a join's snapshot); it takes as long as the data is
	// large, so the deployment bounds it with a startup probe. Applying the
	// logs is not part of it: a seat's acquisition waits for that
	// ([Engine.StateLogHydrated]). After it is the company's (secrets, epoch,
	// sandboxes, tools, logins), which may not hold a probe sized for a store,
	// so a caller answering probes (internal/api's Boot) treats this call as
	// the moment the node is alive.
	Recovered func()

	// NextStart is what this node keeps on its disk between processes for its
	// issue reports: the setting's copy and a crash's output, opened once by
	// the process before the engine ([issuereport.OpenNextStart]). Nil keeps
	// nothing.
	NextStart *issuereport.NextStart

	// Metrics is where this node's measurements are written. ONE recorder for
	// the process, shared with [tracing.Configure], whose MeterProvider READS
	// it: two recorders would make a collector's dashboard and `crewlet
	// retention status` disagree about one event. Nil records nothing, a legal
	// deployment (a test, an embedded engine) but not the normal one:
	// instruments in [metrics.Catalogue] are declared either way.
	Metrics *metrics.Recorder

	// OtelReceiver mints the per-run OTLP endpoints a sandbox exports to. Nil
	// builds one from the environment and BoxHostURL ([sandbox.BuildOtelReceiver]).
	OtelReceiver *sandbox.OtelReceiver

	// Bridge serves a seat's tools to a harness. Nil builds one from the
	// environment and BoxHostURL: [mcpbridge.BaseURLVar] as [ReadBoxURLs]
	// reads it, then [mcpbridge.Build].
	Bridge *mcpbridge.Bridge

	// BoxHostURL is where a box that shares this node's loopback (the `direct`
	// cell) reaches the tool bridge and the telemetry receiver: the node's own
	// listener serving them, on loopback ([mcpbridge.Options.HostURL],
	// [sandbox.OtelReceiverOptions.HostURL]). The caller that bound the
	// listener derives it on a node that runs seats: an ingress node serves
	// the routes to anyone, a node without the role serves them on api.port to
	// this host's loopback alone (internal/api's probe surface). Empty where
	// the node binds no such listener; its boxes then dial
	// [mcpbridge.BaseURLVar] and [sandbox.OtelReceiverURLVar].
	BoxHostURL string

	// Backends may be supplied by a caller that already opened them and keeps
	// their lifetime. Nil opens them from the bootstrap config, and the engine
	// owns them. `crewlet run` leaves it nil (the API reads them back through
	// [Engine.Backends]); suppliers need the estate before or after the engine
	// (a peer's record written before this node exists, a failed boot's
	// leftovers). A supplied set must be complete ([Backends.Complete]).
	Backends *Backends

	// Dispatch overrides the default dispatcher. Fields left nil are FILLED IN
	// from this engine, so a test pinning ownership answers supplies Conditions
	// and still gets the real completion ledger, and one observing parks
	// supplies Park and still gets the real screening.
	Dispatch *Dispatcher

	// SandboxRuns reports what a seat's detached runs mean for its inbox:
	// whether one HOLDS the seat (a job outlasting any broker ack window, so
	// its mail is parked) and whether one awaits a person's answer, which any
	// delivery might be. Nil reads this engine's own coordinator live: no to
	// both until one exists, correct for a node that never ran code. One seam
	// for both because they move together: a run parking on a question stops
	// holding its seat the moment it starts awaiting an answer, and two seams
	// could be read either side of that.
	SandboxRuns func(handle string) (held, awaitsAnswer bool)

	// SandboxPollInterval overrides the completion poll's cadence. Zero takes
	// the production value, sized against jobs that run for minutes.
	SandboxPollInterval time.Duration

	// DirectoryRefresh overrides the interval of the party registry's net
	// behind the directory's own signal ([DirectoryRefresh]). Zero takes the
	// production value. A node without `data` has no signal and reads through
	// a data node every tick, so a test shrinks it.
	DirectoryRefresh time.Duration

	// GitHook is the argv the code cache's proc-receive hook execs on a node
	// serving git. Nil (production) is THIS binary as host.HookCommand names
	// it (`crewlet gitlet hook proc-receive`), resolved through os.Executable.
	// A test binary is not crewlet, so a suite that pushes for real names one
	// (gitlettest.HookCommand).
	GitHook []string

	// Environment is what a company's `${VAR}` references fall back to behind
	// the secret store, and what the per-run endpoints' settings are read from
	// ([ReadBoxURLs], whose refusals refuse the boot even when a receiver or
	// bridge is supplied, and [sandbox.BuildOtelReceiver]). Nil is the process
	// environment. A source rather than the process's own, for
	// [org.HumanContact.ResolvedIdentities]'s reason: ambient input serialises
	// every test that sets it (t.Setenv is forbidden beside t.Parallel). Tier A
	// is not resolved through it: the bootstrap is the root of trust, resolved
	// by the loader and [OpenBackends] from the loading process.
	Environment config.Source

	// The unexported fields below are test seams, settable only from this
	// package's tests (export_test.go): each is the domain's or the node's
	// decision, not a deployment's, and exists so a case can watch what a
	// production value would make it wait for.

	// usageEvery is the usage publisher's cadence ([usage.PublisherDeps.Every]),
	// zero for [usage.FlushInterval].
	usageEvery time.Duration

	// mcpRetryBase is the first wait of the MCP restart schedule
	// ([mcp.RestartWait]) before a failed start is retried (a shared server's
	// by its bridge, a seat's per-role child's by the seat's runner) and before
	// a child that died soon after starting is restarted; zero for
	// [mcp.RestartBase]. The rest of the schedule scales with it.
	mcpRetryBase time.Duration

	// mcpRelistBase is the soonest a running MCP session is listed again
	// ([mcp.RelistFloor]), zero for it; the rest of that schedule scales.
	mcpRelistBase time.Duration

	// fakePlay scripts what a `providers.sandbox.fake` company's runs do
	// ([sandbox.FakeRunner.Player]); nil plays the double's own demonstration
	// ([demoRun]).
	fakePlay fakePlayer

	// sweepEvery is the seat host's placement cadence
	// ([node.Config.SweepInterval]), zero for [seat.SweepInterval].
	sweepEvery time.Duration

	// cacheDisk is what sizing the code cache reads of this node's disk in
	// place of the disk itself ([Engine.cacheVolume]), nil for the disk, so a
	// case about the budget a disk derives is not decided by the machine's
	// free space.
	cacheDisk *cacheVolume

	// gitMemory is the memory, in bytes, the git pool sizes its lanes by and
	// the code cache budgets its background work from in place of the node's
	// own ([gitproc.Options.Memory]), zero for the node's own.
	gitMemory int64

	// setMemoryLimit is what sets the runtime's memory limit in place of
	// debug.SetMemoryLimit, so a case reads the limit its node would set; nil
	// sets it, but in a test binary, which no case's figure may limit.
	setMemoryLimit func(int64) int64

	// compactorEstate stands between the Gitlet compactor and the router's
	// code seam it lists and proposes through ([gitletcompact.Options.Estate]),
	// nil for none, so a case about the compaction-behind alarm can show a
	// repository at the pack bound without pushing two thousand packs.
	compactorEstate func(gitletcompact.Estate) gitletcompact.Estate

	// homeSeams stands between a Gitlet node's home change work and the
	// router's code seam and Gitlet's chain builder
	// ([gitlethome.Options.Estate], [gitlethome.Options.Chains]), nil for none,
	// so a case about the merge-rate alarm can show a queue asked past its
	// capacity without an hour of rounds.
	homeSeams func(gitlethome.Estate, gitlethome.Chains) (gitlethome.Estate, gitlethome.Chains)

	// committerPublisher stands between a data node's committer and the code
	// log's write authority ([gitlet.CommitterOptions.Publisher]), nil for
	// none, so a case about the busy alarm can hold the first batch at its
	// publish and fill a repository's queue.
	committerPublisher func(gitlet.Publisher) gitlet.Publisher
	// resumeGate wraps the gate the sandbox coordinator resumes through
	// ([Engine.turnGate]), nil for the node's admission as it stands, so a case
	// can refuse a resume on a condition it cannot produce on demand.
	resumeGate func(sandbox.TurnGate) sandbox.TurnGate

	// mirrorSeams stands between a Gitlet node's mirrors and the options they
	// are built with ([mirror.Options]), nil for none, so a case about the
	// mirror alarm can show a target refusing every dial and a clock it moves.
	mirrorSeams func(mirror.Options) mirror.Options
}

// New assembles an engine.
//
// Config errors surface HERE, before anything is dialled or claimed: a node
// booting on a bad config and discovering it at the first turn has already
// told its peers it owns seats. The error is named for the one deferred
// failure path below, which adds a lost broker connection's cause to
// whatever step failed.
func New(ctx context.Context, opts Options) (_ *Engine, err error) {
	if opts.Bootstrap == nil {
		return nil, fmt.Errorf("engine: no bootstrap config")
	}
	// Tier A's own rules, held here and not only at the loader, which
	// validates only a file: a bootstrap built in code or edited after loading
	// (`crewlet run`'s flag overrides) arrives unchecked, and a broken rule
	// fails later as another problem (`coordination.type: local` over a
	// clustered stream runs every member as a fleet of one). It NORMALIZES in
	// place, deliberately: what the rules check is what this engine runs.
	if err = opts.Bootstrap.Validate(); err != nil {
		return nil, fmt.Errorf("engine: the bootstrap config (Tier A) is invalid: %w", err)
	}
	// The keyring first: everything below derives from it (per-run token
	// issuers, the state logs' signatures, the secret store, the company
	// document a peer fetches), and building it opens nothing, so a node
	// without a usable one is refused with nothing to unwind.
	cipher, err := openCipher(opts.Bootstrap)
	if err != nil {
		return nil, err
	}
	// The sign-in and entry keys, from the keyring [config.Secrets.Keyring]
	// decodes (openCipher has just proved it): what makes a model's sign-in id
	// and entry id the same on every node and across restarts.
	ring, err := opts.Bootstrap.Secrets.Keyring()
	if err != nil {
		return nil, fmt.Errorf("engine: secrets keyring: %w", err)
	}
	plans := harness.FleetPlans(ring.Derive)
	// The estate's envelopes: every request this node's router sends is signed
	// and every reply verified, and a data node answers its peers under the
	// same ones ([estateEnvelopes]).
	envelopes, err := estateEnvelopes(opts.Bootstrap)
	if err != nil {
		return nil, err
	}
	// Two more instruction channels the engine serves: the broker's
	// membership, which a member changes through its own system account, and
	// a note to a running turn, which the running node offers as the person
	// who sent it (internal/signedask). The steer desk is built below, once
	// the node is named, since its answer to an unverifiable note names it.
	wire, err := newBrokerWire(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// A note waiting on an executor's record, which any node files and reads:
	// sealed in domains of its own, like the scatter's (runsteer.go).
	steerSeals, err := newSteerSeals(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// A code search, which every Gitlet node carries out as the asker it
	// names (codesearch.go).
	codeSearchWire, err := newCodeSearchWire(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// The presence channel: no instruction, sealed all the same because a
	// frame names a person (presence.go).
	presenceWire, err := newPresenceWire(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// The live relay's: no instruction either, signed because its answers are
	// pushed to a peer's sockets as this node's appliers' work (liverelay.go).
	relayWire, err := newRelayWire(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// A person's answer to a parked run waits on the run's row in the
	// coordination store and reaches the resumed run as their words: sealed
	// like every instruction (internal/sandbox, answerseal.go).
	answers, err := sandbox.NewAnswerSeal(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// What carried each session of a run on, kept in the run's leads beside its
	// record for a later brief to a session that may lack its own to tell
	// again. An answer's words are among it, told as that person's, so it is
	// sealed like the answer, in a domain of its own (internal/sandbox,
	// leadseal.go).
	leads, err := sandbox.NewLeadSeal(recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}
	// The completion poll's cadence, checked whether or not this node ever runs
	// a sandbox: an apply can bring it its first ([checkSandboxPollInterval]).
	if err = checkSandboxPollInterval(opts.SandboxPollInterval); err != nil {
		return nil, err
	}

	// Everything that can fail with nothing open comes first: the telemetry
	// receiver, the bridge and this node's identity resolve above the backends,
	// as [Engine.startCore] does with its node-id read. Below them each failure
	// would return past an already open store and broker, and the store is
	// exclusive to one process, so the next attempt would contend with the
	// corpse. The receiver is built before the sandbox runtime, which takes it:
	// one built after the first apply leaves every run launched in between
	// exporting nowhere, silently.
	environ := opts.Environment
	if environ == nil {
		environ = config.EnvSource{}
	}
	// Every URL a box dials back to the engine, read once and refused together
	// ([ReadBoxURLs], which `crewlet llm doctor` reports too). One set and
	// unusable is a deployment that asked for the feature and would get none,
	// silently inside a box the engine never hears from again: a receiver
	// endpoint the exporter could not use, a bridge whose minted endpoints all
	// failed (every executor run's tool calls failing, the run rescued as
	// incomplete), a Gitlet base handing every checkout a clone that dials
	// nowhere.
	boxURLs := ReadBoxURLs(getenv(environ))
	if err = boxURLs.Refusal(); err != nil {
		return nil, fmt.Errorf("engine: a URL a box dials back to the engine: %w", err)
	}
	// The bridge: a fleet mints on one node and may verify on another, so every
	// node derives its key from the fleet's keyring. Its base is the bridge URL
	// [ReadBoxURLs] read.
	bridge := opts.Bridge
	if bridge == nil {
		if bridge, err = mcpbridge.Build(boxURLs.Bridge.URL, tokenMaterial(opts.Bootstrap),
			opts.BoxHostURL); err != nil {
			return nil, fmt.Errorf("engine: tool bridge: %w", err)
		}
	}
	// Same key material, different domain, built here for the bridge's reason.
	otel := opts.OtelReceiver
	if otel == nil {
		otel, err = sandbox.BuildOtelReceiver(getenv(environ),
			tokenMaterial(opts.Bootstrap), opts.BoxHostURL)
		if err != nil {
			return nil, fmt.Errorf("engine: sandbox telemetry: %w", err)
		}
	}
	// A coding box's git credential, the third per-run token beside the
	// receiver's and the bridge's, in a domain of its own and built here for
	// their reason: the launching node mints it and whichever Gitlet node its
	// box dials verifies it. Built on every node whatever its roles.
	boxTokens, err := host.NewBoxTokens(tokenMaterial(opts.Bootstrap), nil)
	if err != nil {
		return nil, fmt.Errorf("engine: box tokens: %w", err)
	}
	// A check's box's, likewise. EVERY node verifies it, Gitlet built or not:
	// a box route on a node serving none of it tells a box why only once its
	// token verifies (api.CodeSurface.VerifyBox).
	checkTokens, err := host.NewCheckTokens(tokenMaterial(opts.Bootstrap), nil)
	if err != nil {
		return nil, fmt.Errorf("engine: check tokens: %w", err)
	}

	// The mode and the incarnation, resolved once. An unset mode is normal. The
	// incarnation is minted here, not at the lease, because the capacity
	// barrier compares it and a second mint would be a second identity for one
	// process; it owns every lease this process holds for itself (the seat
	// host's presence, seats and duties). The node id is resolved once too, so
	// two copies cannot disagree about what this node is called.
	mode := opts.Mode
	if mode == "" {
		mode = statelog.ModeNormal
	}
	nodeID, err := config.ResolveNodeID(opts.Bootstrap, nil)
	if err != nil {
		return nil, err
	}
	incarnation := config.NewIncarnation(nodeID)
	steers, err := newSteerDesk(nodeID, recordKeyring(opts.Bootstrap))
	if err != nil {
		return nil, err
	}

	// This process's tool sessions, the last thing that can fail with nothing
	// open: every phase offering tools serves them to its harness through a
	// socket in this directory, and a TMPDIR long enough to put the socket past
	// the platform's unix socket path bound is refused HERE, naming TMPDIR,
	// rather than at every seat's first phase as a bind error naming no
	// setting. It is a directory, so the one thing above with something to
	// remove: this guard removes it until the engine's own failure path,
	// armed below, takes over.
	var sessions *toolsession.Host
	if sessions, err = toolsession.NewHost(); err != nil {
		return nil, fmt.Errorf("engine: the directory every seat's tools are served "+
			"to its harness from: %w", err)
	}
	sessionsHanded := false
	defer func() {
		if !sessionsHanded {
			_ = sessions.Close()
		}
	}()

	// Only what this engine OPENED does it close. A caller-supplied set keeps
	// its lifetime and must be complete ([Backends.Complete]).
	backends := opts.Backends
	ownsBackends := false
	if backends != nil {
		if err = backends.Complete(); err != nil {
			return nil, err
		}
	} else {
		// Assigned, not `opened, err :=`, which would shadow the err above, the
		// one shape govet cannot tell from the bug its check is on for.
		backends, err = OpenBackends(ctx, opts.Bootstrap, opts.Company, opts.Metrics)
		if err != nil {
			return nil, err
		}
		ownsBackends = true
	}
	// The replicated estate this node holds, opened before anything can read or
	// copy it, lent backends included: holding is a fact about the node, not
	// about whoever opened its store.
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
		// Set once, before anything reads it: the admission handshake asks it
		// whether this node publishes and whether its broker is a member, and the
		// node is handed this exact value. The bootstrap's accessor parses roles
		// with the validator that refused unknown ones at load and is what the
		// fleet view reads a peer's presence row back through.
		profile:  opts.Bootstrap.Profile(nodeID),
		backends: backends, ownsBackends: ownsBackends, cipher: cipher,
		envelopes: envelopes, brokerWire: wire,
		onboarded: runner.NewLatch(), skills: skills.NewRegistry(),
		rewrites: compact.NewCache(),
		// On the node's own queue, which without `data` is the publish its
		// custody hands to a data node, so a stateless node's auxiliary spend
		// reaches the history like its phases do.
		auxSpend: auxspend.NewLedger(backends.Queue),
		// Unit-repository reads, on the same queue: every Gitlet node holds data,
		// so its own event store writes them.
		codeReads: readaudit.New(backends.Queue),
		steers:    steers,
		// Over the fleet's run records, built here, not with the sandbox runtime:
		// an ingress-only node with no runtime still files a person's note on a
		// run another node launched.
		connectMarks: newConnectMarks(),
		runSteers: newRunSteers(steerSeals, sandbox.NewCoordStore(backends.Fleet), backends.Queue).
			claimingIn(ledgerstore.NewFleetCompletions(backends.Fleet)).
			seeingThrough(ledgerstore.NewSeenThrough(backends.Fleet)),
		codeSearchWire:      codeSearchWire,
		presenceWire:        presenceWire,
		relayWire:           relayWire,
		answers:             answers,
		leads:               leads,
		dataTurns:           builtin.NewDataTurns(nil),
		folderTurns:         builtin.NewFolderTurns(nil),
		renderState:         renderState{renderTurns: builtin.NewRenderTurns(nil)},
		mcpRetryBase:        opts.mcpRetryBase,
		mcpRelistBase:       opts.mcpRelistBase,
		sandboxOtel:         otel,
		sandboxPollInterval: opts.SandboxPollInterval,
		directoryRefresh:    opts.DirectoryRefresh,
		gitHookArgv:         slices.Clone(opts.GitHook),
		cacheDisk:           opts.cacheDisk,
		sizedByTheMachine:   sizedByTheMachine(ctx),
		gitMemory:           opts.gitMemory,
		setMemoryLimit:      opts.setMemoryLimit,
		compactorEstate:     opts.compactorEstate,
		homeSeams:           opts.homeSeams,
		committerPublisher:  opts.committerPublisher,
		fakePlay:            opts.fakePlay,
		plans:               plans,
		mirrorSeams:         opts.mirrorSeams,
		usageEvery:          opts.usageEvery,
		resumeGate:          opts.resumeGate,
		turns:               node.TurnsAtOnce(opts.Bootstrap.Node.MaxConcurrent),
		sandboxSeats:        map[string]bool{},
		bridge:              bridge,
		boxTokens:           boxTokens,
		boxGitURL:           boxURLs.Git.URL,
		checkTokens:         checkTokens,
		sessions:            sessions,
		metrics:             opts.Metrics,
		answerBudget:        queue.NewAnswerBudget(answerMeter{metrics: opts.Metrics}),
		deadLetterLedger:    newDeadLetterLedger(backends.Queue, backends.Fleet),
		objectives:          newObjectives(opts.Metrics),
		mode:                mode,
		incarnation:         incarnation,
		id:                  nodeID,
		startedAt:           time.Now().UTC(),
		// Before equip, which writes the company's numbers into it, and before
		// node.New, which hands the same value to every seat attachment.
		batch: queue.DefaultBatchOptions(),
		// Before startCore, which threads the nudge into the identity applier: a
		// nil channel would block every send forever on the apply loop.
		directoryNudge: make(chan struct{}, 1),
	}
	e.nextStart = opts.NextStart
	// The claim gate's watch: every log whose health gates admission tells it
	// from its apply loop from boot, and the seat host is built over its gate.
	e.claims = newGateWatch(e.seatsAdmitted)
	// The desk files on the same records: a turn's box, handed over in the
	// write its launch makes to create the run's record (before any box or
	// harness exists), and every late offer after it (steer.go).
	steers.runs = e.runSteers
	// A wake a run reads takes its message's queued line down (queued.go).
	e.runSteers.telling(e.settleWaiting)
	// The settles those records take off the path that took end with the node:
	// each asks its store until it answers, no longer than this node's
	// teardown, which is when its store goes. Every take is asked once more as
	// teardown begins, in a window beside the give-backs, never on the stop's
	// allowance (runsteer.go, steerSettleStopBudget).
	e.runSteers.stopWith(e)
	// The Tier A facts the process holds, read once before anything below reads
	// them (the address people and vendors reach this deployment at, this
	// node's admin posture, its Tier A tokens): each is fixed at boot, and the
	// first epoch already composes links on the first and resolves logins on
	// the last.
	e.externalBase = opts.Bootstrap.API.ExternalBase()
	e.webhookBase = opts.Bootstrap.API.WebhookBase()
	e.webhookBaseKey = opts.Bootstrap.API.WebhookBaseKey()
	if opts.Bootstrap.ServesAPI() {
		e.adminPosture = opts.Bootstrap.API.Auth.Admin
	}
	e.companyManaged = opts.Bootstrap.API.Auth.CompanyManaged()
	e.tierATokens = make(map[string][]iam.Grant, len(opts.Bootstrap.API.Auth.Tokens))
	for _, token := range opts.Bootstrap.API.Auth.Tokens {
		e.tierATokens[token.ID] = slices.Clone(token.Grants)
	}
	// The event log files an event under every agent it involves, by the agent
	// id the org chart derives for each seat a participant tag names. Only a
	// process holding the chart can say it, so it is the engine's, off the live
	// epoch when each event is written. Installed before anything can publish;
	// an event with no company active is filed under its own agent alone
	// ([store.DB.SetEventSeats]).
	backends.Store.SetEventSeats(func(handle string) (string, bool) {
		return seatAgentID(e.Company(), handle)
	})

	// One runner per engine: its in-process guard is half of what keeps two
	// writers off one third-party app ([setup.Runner.Hold]).
	e.setupRunner = sync.OnceValue(e.newSetupRunner)
	// Built before the first apply that asks it (the boot's); starts nothing
	// until one does.
	e.codeRepos = newCodeReconciler(e.decideCodeRepos)
	// After the engine exists, because the bridge's supervision refiles this
	// engine's registries when a catalogue changes; before equip, which starts
	// the company's shared servers on it.
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	// A mint GitHub refused is held on the restart schedule the seats' tool
	// servers retry that refusal on, before anything reads a seat's token:
	// every reader of one installation meets the held refusal within the wait
	// instead of asking GitHub again ([github.Issuer]). The same for every
	// agent's Google tokens ([googleauth.SeatIssuer]).
	e.githubIssuer.RetryWait = e.seatRetryDelay
	e.googleIssuer.RetryWait = e.seatRetryDelay

	// The view of the fleet's data nodes, built before the router that asks it
	// on every request. Building lists nothing: it runs once the boot's failure
	// path is armed below and answers unknown until its first listing
	// (remote.go).
	if e.dataView, err = newDataView(opts.Bootstrap, backends.Coord); err != nil {
		if ownsBackends {
			backends.Close(ctx)
		}
		return nil, err
	}

	// Every node routes the estate: a data node answers from its own copy and
	// asks a peer only while that cannot, a node without `data` asks for
	// everything (router.go). Built before anything publishes, since a tool is
	// handed its facades as soon as a seat is.
	if HoldsEstate(opts.Bootstrap) {
		e.local = newLocalEstate(e)
	}
	if e.router, err = e.newRouter(backends.Queue, nodeID, envelopes); err != nil {
		if ownsBackends {
			backends.Close(ctx)
		}
		return nil, err
	}
	// The executor log's writes the sandbox runtime records through are the
	// router's, so a seat's capture and its turn's end reach the log from a node
	// without `data` too (executorsession.go).
	e.executorLog = e.router.ExecutorWrites()
	// A turn's dispatch reads its conversation through the same router at
	// `linearizable`, so an end another node published is in the copy that
	// answers (conversation.go).
	e.executorHeads = e.router.ExecutorReads()
	// A read of a box on another node's host is carried out by that node (it
	// pauses the box a collection reads), so it is signed like every such
	// channel (sandboxhost.go). None without a broker: that node is the whole
	// fleet and every box is its own.
	if backends.Queue != nil {
		if e.boxReads, err = newBoxReads(recordKeyring(opts.Bootstrap), nodeID, backends.Queue,
			e.sandboxCoordinator); err != nil {
			if ownsBackends {
				backends.Close(ctx)
			}
			return nil, err
		}
		// A test of an MCP server where it runs starts a process or dials as the
		// company, so it is signed too (mcpcheck.go).
		if e.mcpChecks, err = newMCPChecks(recordKeyring(opts.Bootstrap)); err != nil {
			if ownsBackends {
				backends.Close(ctx)
			}
			return nil, err
		}
	}

	// The issue reporter, off until a revision turns it on. Its listener goes on
	// before anything publishes, like the custody listener below.
	e.startIssueReports(opts.Bootstrap, backends)
	if backends != nil && backends.Queue != nil {
		// Every run this node collects, for conversations whose context does not
		// fit (conversationhold.go).
		backends.Queue.AddPublishListener(e.noteConversationRun)
	}

	// A node without `data` hands the data nodes the record of what it did. The
	// custody listener must be in place before anything publishes, as the first
	// turn a restarted node picks off its durable inbox would otherwise escape.
	if !HoldsEstate(opts.Bootstrap) {
		e.custody = observe.NewCustody(backends.Queue)
		backends.Queue.AddPublishListener(e.custody.Listen())
		e.custody.Start(ctx)
	}

	// One failure path for everything below, armed before the first thing that
	// outlives this call and stood down once the engine is the caller's: a
	// discarded engine cannot be asked to stop what it started, and closing only
	// the store and broker beneath it is the mid-batch write [Engine.Stop]
	// avoids. It takes [context.WithoutCancel]: the failure is often the
	// caller's cancellation.
	//
	// The error names a lost broker connection: a step failing because NATS
	// closed a connection for good says only "connection closed", the cause
	// sitting on the queue ([lostDuring]). Read BEFORE the teardown: the closed
	// handler runs on its own goroutine, and after the backends close every
	// connection reads as closed.
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
	// The sessions are the engine's from here: the failure path removes them.
	sessionsHanded = true

	// The first thing that outlives this call, so the failure path stops it if
	// the boot goes no further.
	e.startDataView(ctx)

	// The audit trail before the native backends, whose identity writer and
	// appliers announce through it.
	if err = e.startAuthEvents(ctx); err != nil {
		return nil, err
	}

	// The skill sync is built before the native backends, whose pages applier
	// nudges it post-commit: a nudge with nothing to land on would be lost.
	// Building starts nothing.
	if err = e.newSkillSync(nodeID); err != nil {
		return nil, fmt.Errorf("engine: tool skills: %w", err)
	}

	// This node answers for its own history from the moment it has a store and
	// a broker, in every node and mode (history.go): answering is not publishing.
	if err = e.armHistory(ctx); err != nil {
		return nil, fmt.Errorf("engine: serve the fleet's history: %w", err)
	}
	// And for the notes a person sends its turns, which only this node can hand
	// to them (steer.go).
	if err = e.armSteer(ctx); err != nil {
		return nil, fmt.Errorf("engine: serve notes to running turns: %w", err)
	}
	// And for what its appliers announce (whose inbox moved, what landed in a
	// room, what moved on an artifact and its datastore): relayed to a peer's
	// dashboards while the peer's own copy is out of service, with the follower
	// doing the same for this node's (liverelay.go).
	if err = e.armLiveRelay(ctx); err != nil {
		return nil, err
	}
	// And for who is in each artifact's room, held for its own sockets and
	// agents and told to its peers (presence.go).
	if err = e.armPresence(ctx); err != nil {
		return nil, err
	}

	// The admission handshake, before any publisher; the earliest it can run,
	// needing only the coordination backend. A node in a maintenance mode skips
	// it and acknowledges instead: it is offering the evidence the capacity
	// barrier is established from, and first retires the admission a previous
	// process left (maintenance_mode.go).
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.handshake(ctx, maintenanceStreams()); err != nil {
		return nil, err
	}

	// Serving before any seat, in every mode: a node restarted into a
	// maintenance mode still answers reads; its writers are what
	// [Engine.estateBackend] withholds.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.serveEstate(ctx); err != nil {
		return nil, err
	}
	// And keeping the stateless nodes' events, in every mode: it writes this
	// node's own event log, which no mode holds still.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.takeCustody(ctx); err != nil {
		return nil, err
	}
	// And the broker's membership, on a member, in every mode: any member may
	// answer a leaf's read of the metadata group or carry an operator's removal
	// of a gone member, and a fleet restarted into a maintenance mode is when an
	// operator looks at who its broker still counts.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.serveFleetBroker(ctx); err != nil {
		return nil, err
	}
	// The lease TTL in force, before anything claims a lease on the seat
	// heartbeat (presence, seats, the mailbox retirement).
	e.leaseTTL = effectiveLeaseTTL(opts.Bootstrap, backends.Coord)
	// And the object store, over the opened backend: every node reads and
	// writes files through it from boot, in any mode.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.startObjects(); err != nil {
		return nil, err
	}

	// The core runtime, on every data node and before anything derived from a
	// company: the state log for every registered domain, the node gate and the
	// identity estate. A node with no company still signs people in; only its
	// first PERSON waits for one, since every person holds a human seat
	// (ADR-0032; core.go). It does NOT wait for hydration, which would serve no
	// dashboard and hold the startup probe at the recovery's 503
	// ([Options.Recovered] is called below); seat acquisition waits
	// ([Engine.StateLogHydrated]). A node without `data` starts none of it (no
	// estate for a log, ADR-0025; no sign-in) and reads the directory from a
	// data node ([Engine.startRoutedDirectory]).
	if HoldsEstate(opts.Bootstrap) {
		if err = e.startCore(ctx, opts.Bootstrap); err != nil {
			return nil, err
		}
	} else {
		e.startRoutedDirectory(ctx)
	}
	// After the logs, so a raise of the bucket is weighed against every
	// reservation they hold and never takes one a log being created needed.
	e.raiseObjectsAtBoot(ctx, opts.Bootstrap, incarnation)
	// The boundary: above is what this node holds, recovered; below is what its
	// company configures ([Options.Recovered]). Logged because a deployment
	// sizes its startup probe by the time from the process's first line to this
	// one.
	log.InfoContext(ctx, "engine_recovered", "node", nodeID,
		"detail", "what this node holds is open and recovered; what its company "+
			"configures (its secret snapshot, sandboxes, Worklet, Doclet and Chatlet, tool "+
			"servers, harness logins) starts next")
	if opts.Recovered != nil {
		opts.Recovered()
	}
	// The secret snapshot before the first epoch, which resolves every ${VAR} as
	// it is built: loaded afterwards, the first epoch would resolve from the
	// environment alone and later ones from the store, so a rotated secret would
	// work on the second apply and not on boot. Below the boundary: it is the
	// company's credentials, its cost grows with how many it keeps, and the
	// startup probe is sized for the store. On every node, one with no company
	// included, because the first apply bringing one builds from it when its own
	// refresh cannot read the store ([Engine.bootSecrets]).
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := e.bootSecrets(ctx); err != nil {
		return nil, err
	}
	// A nil company is the unconfigured node, not a caller's mistake: the store
	// is authoritative at runtime and a fleet's first revision may arrive over
	// the API, so a node runs with no company, serving HTTP, reporting itself
	// unconfigured and holding no seats until the control plane hands it an
	// epoch. A nil epoch means nothing to equip or place, never a panic.
	//
	// Built below the boundary with the rest of what the company configures. A
	// revision that cannot be built exits the boot here, past the probe.
	var company *Company
	if opts.Company != nil {
		// On the node's registry of sign-ins, as every apply's revision is
		// ([Engine.Apply]): built on none, the boot's entries kept sit-outs in
		// registries the first apply's entries never found.
		company, err = newCompany(opts.Company, e.resolver(), e.plans)
		if err != nil {
			return nil, err
		}
		// Stamped once with the activation the caller says this company came
		// from, zero for none ([stampActivation]).
		stampActivation(company, opts.ActivatedAt)
	}
	// The rest is skipped with no epoch: it is all derived from a company, and
	// the apply bringing the first revision builds it then (the native halves,
	// [Engine.startNativeFor], and the sandbox runtime, [Engine.startSandbox],
	// each before that apply equips).
	if company != nil {
		// Before equip, which hands the artifact tools the boxes of a turn's runs
		// only on a node with the runtime ([ArtifactToolDeps]): an absent
		// dependency is omitted, not registered broken, so equipping first would
		// build a company whose seats could never publish what their executors'
		// runs built.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.startSandboxFor(ctx, company); err != nil {
			return nil, fmt.Errorf("engine: sandbox: %w", err)
		}
		// Before equip too, likewise: the Worklet, Doclet and Chatlet tools are
		// registered only where their halves exist.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.startNative(ctx, opts.Bootstrap, company); err != nil {
			return nil, err
		}
		// Not the projects, containers or declared repositories here: they follow
		// the published company through [Engine.followCompany], which the end of
		// this constructor runs, stamped with the activation the company was
		// applied from.
		// Equipped before published: a turn can start the instant the epoch is
		// current, and one finding an empty registry would run a seat with no
		// tools at all.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := e.equip(ctx, company); err != nil {
			return nil, err
		}
		// The configured credential bundles last, on Apply's rule: every step
		// above can still fail the boot, and a failed boot must leave no login in
		// a state directory the next epoch's seats share. Before the epoch is
		// current, so its first model call finds the login
		// ([Company.restoreBundles]). Below the boundary: a restore can wait on
		// the shared login's lock, which a startup probe sized for the store
		// knows nothing of.
		company.restoreBundles(ctx)
	}
	e.installEpoch(ctx, company)

	// Before the node, which registers every seat's mailbox through it on its
	// first walk, and after the lease TTL, which a retirement claims a seat for.
	if e.mailboxes, err = e.buildMailboxes(backends, nodeID); err != nil {
		return nil, fmt.Errorf("engine: seat mailboxes: %w", err)
	}
	n, err := node.New(node.Config{
		Queue:     backends.Queue,
		Coord:     backends.Coord,
		Mailboxes: e.mailboxRegistry(),
		// The node ID is stable across restarts; the owner is this INCARNATION,
		// minted once above: a restarted process reusing its owner id would be
		// indistinguishable from the one that died and inherit leases it never
		// renewed, and two owners under one node id look like a second process.
		// Every lease this process holds for itself is under e.incarnation; the
		// one exception is the mailbox retirement's, which claims a seat lease
		// as a separate party and must lose to this host ([Engine.buildMailboxes]).
		NodeID: nodeID,
		Owner:  e.incarnation,
		// Read fresh through the epoch, never bound to the starting company: an
		// apply replaces the seat set, and a captured method value would keep
		// claiming seats a deleted role no longer has and never claim a new one.
		Seats: func() []placement.Seat { return e.Company().Seats() },
		// A seat attached before its tools' copy of the records caught up would
		// answer "there is no such item", which a seat ACTS on (files the
		// duplicate, tells a person their link is dead). So a node that is behind
		// keeps its seats and claims nothing new until caught up
		// ([Engine.StateLogHydrated]): on a data node every log whose health
		// gates admission, and for each natively run half a copy the estate's
		// router says serves it. Never in a maintenance mode, where this host
		// still runs for the presence it renews ([Engine.seatsAdmitted]). Asked
		// through the claim gate's watch, which records a refusal so the gate's
		// opening asks the host for a pass instead of waiting a tick
		// (admission.go).
		SeatsAdmitted: e.claims.gate,
		// The other direction: admission withholds new work from a node merely
		// behind; this gives back work held by a node that can no longer ROUTE
		// its seats' calls (no sound copy of its own, a presence view unknown
		// and stale past [statelog.FloorCacheStale]). A WRONG copy is no reason
		// to shed: it goes out of service ([localEstate.For]) and the seats stay,
		// answered by a sound data node. Two gates because the remedies differ
		// ([Engine.SeatsServiceable]).
		SeatsServiceable: e.SeatsServiceable,
		Profile:          e.profile,
		// What this node is doing, advertised on every heartbeat: only the node
		// running a seat knows its in-flight count and drain state, and /health
		// answers for whichever node served the request, so behind a load
		// balancer a refresh would tell a different story each time.
		Status: e.nodeStatus,
		// On a Gitlet node, what its git cache gives its background work, beside
		// the status and never in it: what every Gitlet node places a
		// repository's maintenance by ([estate.Router.Maintainer]), so a beat
		// whose status read overran must still say it ([Engine.gitBackground]).
		GitBackground: e.gitBackground(),
		// Which roles the company needs, off the current epoch: git only where it
		// hosts code, so a fleet declaring roles without git is not told nobody
		// serves it until it matters.
		RoleNeeded: e.roleNeeded,
		// Each new epoch its presence is claimed at, on which this node's status
		// record is written at once rather than on the writer's next tick: until
		// then every reader counts nothing of this node's
		// ([Engine.statusRecordClaimed]).
		PresenceClaimed: e.statusRecordClaimed,
		Turn:            e.mailboxTurn,
		// Before the mailbox opens and after it closes (node.Config). A seat
		// mid-detached-run has its runs recovered and mail parked before anything
		// can deliver to it, and a seat a person PAUSED is held before its inbox
		// is attached, so placement moving it here does not deliver the mail it
		// holds: the release on the node it left dropped that node's hold with the
		// attachment (seatpause.go).
		SeatReady: func(ctx context.Context, s placement.Seat, lease coord.Lease, inbox queue.Attachment) error {
			return e.prepareSeat(ctx, s, lease.Epoch, lease.Owner, inbox)
		},
		SeatDone: e.releaseSeat,
		// WHICH NODE HELD THE SEAT WHEN (seatplacement.go).
		SeatPlaced: func(ctx context.Context, s placement.Seat, lease coord.Lease) {
			e.publishSeatPlacement(ctx, s, seatPlaced(nodeID, lease))
		},
		SeatReleased: func(ctx context.Context, s placement.Seat, lease coord.Lease, reason seat.ReleaseReason) {
			e.publishSeatPlacement(ctx, s, seatReleased(nodeID, lease, reason))
		},
		// A seat that starts admitting turns re-checks at once what was refused
		// for want of one: an answer whose resume the preparation inherited waits
		// on this, not the next renew ([Engine.mayResumeAnswer]). By handle,
		// because an owed answer is filed under it ([sandbox.Coordinator.Readmit])
		// and the hook gets the whole seat ([node.Config.SeatAdmitted]).
		SeatAdmitted: func(_ context.Context, s placement.Seat) {
			e.readmitResumes(waitOwnership, s.Handle)
			// A seat now held here is measured for a review after the quiet
			// window: its previous holder may not have lived to ask.
			e.kickAccessReviews(s.Handle)
		},
		// A SEAT RUNS UP TO ITS CAP OF TURNS, one per conversation, read
		// fresh so an apply that changes the cap moves the next admission.
		SeatTurns: e.seatRunCap,
		// A seat whose turn has ended re-checks what was refused for the turns
		// running ([node.ErrSeatFull], [node.ErrConversationBusy]): an answer by
		// turn that arrived mid-turn is the seat's next turn ([Engine.admitResume]).
		SeatTurnEnded: func(handle string) {
			e.readmitResumes(waitSeatTurns, handle)
		},
		LeaseTTL:      e.leaseTTL,
		SweepInterval: opts.sweepEvery,
		// The host's ceiling, from Tier A, per NODE (a fleet's is N times this).
		// Passed unresolved: zero is an absent key and node.New turns it into the
		// default, so the number lives in one place (node.DefaultMaxConcurrent).
		MaxConcurrent: opts.Bootstrap.Node.MaxConcurrent,
		// The company's coalescing knobs, live: an apply writes through this and
		// every seat attachment reads it.
		BatchOptions: e.batch,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: node: %w", err)
	}
	// Before the node runs, because the first seat it acquires hydrates through
	// this. Nil is the honest shape for a node with no broker or store
	// (prepareSeat skips the step). Over the coordination store's connection
	// (ADR-0001): a seat's replay is an ordered consumer over its whole history
	// and on the queue's connection would sit on the read loop every mailbox
	// consumes through ([Backends.Conn] is nil for an external cluster). The
	// seat's derived id, never a roster lookup, so a seat a revision removed
	// still flushes on release ([memoryAgentID]); [memoryConfig] says how it
	// reaches the changelog.
	e.memory, err = memsync.New(backends.Store, backends.CoordinationJetStream(),
		func(handle string) string { return memoryAgentID(e.Company(), handle) },
		memoryConfig(opts.Bootstrap, backends))
	if err != nil {
		return nil, fmt.Errorf("engine: memory sync: %w", err)
	}
	// Armed here, started in Start. The watchdog stands down permanently the
	// first time no watched duty is live, and the host is not live until
	// node.Start runs it, so building it here and starting it there is the only
	// ordering that watches anything. At the host's own TTL, the lease its seats
	// are held under ([seat.Watchdog] argues the two are one number): asked of
	// the host, which resolved it, not e.leaseTTL.
	e.watchdog = seat.NewWatchdog(n.Host().TTL())
	e.watchdog.Watch("seat-host", n.Host())
	// And the broker, whose death ends the process through the same exit as soon
	// as there is a watchdog to end it ([Engine.watchBroker]).
	e.watchBroker(ctx, backends.death)
	e.node = n
	e.dispatch = e.buildDispatcher(opts, backends)
	// This node answers for the memory of the seats it holds, in every mode and
	// whether or not it serves the API (memoryread.go). After the node, whose
	// incarnation a seat's lease names.
	if err = e.armMemoryReads(ctx); err != nil {
		return nil, fmt.Errorf("engine: serve seats' memory: %w", err)
	}
	// And what its running turns hold from Gitlet's expiry: the expiry asks
	// every seat holder, so this node must answer before its host claims its
	// first seat (codeholds.go).
	if err = e.armCodeHolds(ctx); err != nil {
		return nil, err
	}
	// And the live output of the executors' boxes it launched, likewise
	// (executortail.go).
	if err = e.armExecutorTails(ctx); err != nil {
		return nil, fmt.Errorf("engine: serve executors' live output: %w", err)
	}
	// And what a relaunch of its seats' turns would be launched on, which its
	// own bench decides (relaunchtarget.go).
	if err = e.armRelaunchTargets(ctx); err != nil {
		return nil, err
	}
	// And whether one of its parks holds the inbox of a seat it holds, which
	// only its process knows (parkheld.go).
	if err = e.armHeldParks(ctx); err != nil {
		return nil, err
	}
	// And the boxes on its host, which no other node can reach (sandboxhost.go).
	if err = e.armSandboxHost(ctx); err != nil {
		return nil, err
	}
	// And the tool bridge of the executor runs its seats launch, wherever a
	// box's call lands (bridgefleet.go).
	if err = e.joinBridgeFleet(ctx, opts.Bootstrap); err != nil {
		return nil, fmt.Errorf("engine: serve the tool bridge to the fleet: %w", err)
	}

	// Everything below this line publishes, and a maintenance-mode node starts
	// none of it (duties, scheduler, change feeds, notifications, retention,
	// reflection). The node above is not one: [Engine.Start] starts it in every
	// mode, its seat host renewing PRESENCE (membership, not work), and what
	// would publish through it is withheld where decided: the host claims no
	// seat unless the mode publishes ([Engine.seatsAdmitted]) and no surface
	// gets a writer ([Engine.writers]). One gate rather than a condition per
	// loop, as a forgotten publisher breaks the exclusion; it is the LAST thing
	// in the constructor, so a later loop cannot start by accident.
	if !e.mode.Publishes() {
		log.InfoContext(ctx, "maintenance_mode_started",
			"mode", e.mode, "node", nodeID, "incarnation", e.incarnation,
			"detail", "the broker and the coordination estate are up and no "+
				"publisher is: no seats, no duties, no scheduler, no change "+
				"feed, no object repair or collection and no write routes")
		// The engine is the caller's from here, so the guard stands down and
		// [Engine.Stop] ends it. This return is a success: a maintenance node
		// unwinding itself would take down the posture the capacity barrier
		// waits to read.
		booted = true
		return e, nil
	}

	// Every mailbox a wake travels through, before anything below can send one:
	// each seat's inbox and priority lane, and the inbound edge's durable. Both
	// streams drop what is published to a subject no consumer covers, and the
	// scheduler, change feeds and notification router publish the moment they
	// start, so mailboxes made after them lost a boot's first wakes (a feed
	// replaying a person's message). The wake queue ([Engine.wakes]) backstops a
	// seat whose create the broker refused, not the boot path. Best effort per
	// seat as everywhere; the inbound edge's is fatal, like the router's own
	// subscribe ([Engine.ensureInboundMailbox]).
	if e.node != nil {
		e.node.EnsureMailboxes(ctx)
	}
	if e.Company() != nil {
		if err := e.ensureInboundMailbox(ctx); err != nil {
			return nil, fmt.Errorf("engine: %w", err)
		}
	}
	// Which seats a person paused, read before the seat host claims anything
	// ([Engine.Start] runs it), so the first mail this node is handed meets a
	// node that knows. Bounded: past its budget the watch keeps trying and the
	// screening defers until it answers.
	e.startSeatPauses(ctx)
	// Where this node can start a box, probed now and kept: what its status
	// record tells the Models page.
	e.startRunnerProbe(ctx)
	e.startHostLogins(ctx)
	// The retries people asked for, applied as every node hears them.
	e.startModelRetries(ctx)
	e.startDeviceSignIns(ctx)
	// The model checks people ask for, each run on this node's time.
	e.startModelChecks(ctx)
	// What this node's MCP servers list, published, and every node's, watched:
	// the catalogue a per-agent server's tools reach.
	e.startMCPListings(ctx)
	// What this node says of its servers, models, runners and host: its status
	// record beside its presence.
	e.startStatusRecords(ctx)
	// How each server is, answered to a server's page on any node, and the
	// restarts people asked for, applied as every node hears them.
	e.startMCPInstances(ctx)
	e.startMCPRestarts(ctx)
	// The tests people ask for, of the servers this node runs.
	e.startMCPChecks(ctx)
	// Last, because its fleet-singleton duty is claimed under the node's own
	// incarnation.
	if err := e.startSandboxWaiter(ctx); err != nil {
		return nil, fmt.Errorf("engine: sandbox waiter: %w", err)
	}
	e.startMaintenance(ctx)
	// The core's duties (the log's own trim, the identity estate's) and this
	// node's usage publisher, on every publishing data node, company or not: a
	// fleet nobody configured still writes its identity log, and without the
	// trim a domain's log grows to its ceiling, where appends are refused. A
	// node without `data` runs no core. After the node exists: each duty is
	// claimed under its incarnation, and one run before the lease existed would
	// run on every node at once.
	e.startCoreDuties(ctx)
	// And the native half's (the vector domain's one writer) where native halves
	// exist; an unconfigured node has none, and the apply bringing its first
	// company arms it ([Engine.startNativeFor]). The live token meters are
	// [Engine.Start]'s, after the host runs ([Engine.startBudgetReports]).
	e.startNativeDuties(ctx)
	// And the object store's collector, behind the same gate: each pass pins the
	// estate with a barrier, which a capacity window refuses.
	e.startNativeCollector(ctx)
	// A fleet singleton too: two nodes reconciling one third-party app at once
	// can each create an identity for one seat, and no later pass can detect or
	// repair that.
	e.startIntegrations(ctx)
	// And the atlas's read of those integrations' vendors, a singleton on its
	// own cadence (ADR-0129).
	e.startAtlasCollect(ctx)
	e.startAtlasWake(ctx)
	e.startMemorySync(ctx)
	e.startMemoryFill(ctx)
	e.startScheduler(ctx)
	// Beside the scheduler, the other producer of a seat's own work; it reads
	// the company every pass, so no apply re-arms it.
	e.startPickups(ctx)
	// And the sweep that hands delegated requests back to people.
	e.startDelegatedSweep(ctx)
	// And the flow duty, held where the live projection is (ADR-0162).
	e.startFlowDuty(ctx)
	// After the epoch is current, because the dispatcher resolves a turn's seat
	// against it, and before notifications, so a turn woken by the first inbound
	// message has somewhere to write what it learns. Fatal on failure: the
	// subscription is the only path to the write side, and a company booting
	// without it learns nothing while looking healthy.
	if err := e.startReflection(ctx); err != nil {
		return nil, err
	}
	// The background passes, after the node exists: each is a fleet singleton
	// claimed under its own incarnation.
	e.startLearningBackground(ctx)
	// Before notifications, because a feed publishes onto the inbound edge the
	// service consumes: started after, changes committed in between would reach
	// the record and wake nobody.
	e.startNativeFeeds(ctx)
	// Before notifications too: the Confluence parser hands it every page change
	// it hears ([Engine.startSkillSync]).
	e.startSkillSync(ctx)
	if err := e.startNotifications(ctx, e.Company()); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	// The source after them, because the Confluence source is read off the
	// wiring startNotifications just built. This is the boot walk; an apply
	// calls the same for every later epoch.
	e.reconcileSkills(e.Company())

	// What follows a published company, last: the same list an apply runs,
	// through [Engine.followCompany], so the two paths publishing a company
	// cannot disagree about what follows one. Boot brings most of it up by its
	// own route, so this is mostly a safety net, but the tracker's projects and
	// the knowledge containers have no other route at boot: here a company
	// booted on an activated revision is charted.
	e.followCompany(ctx, e.Company())
	booted = true
	return e, nil
}

// buildDispatcher wires the dispatcher once, at construction, never per
// delivery. The node runs one consume loop per seat, so every attached seat can
// be inside Dispatch at once; assigning the shared dispatcher's fields on the
// way in is a data race whatever the values, and survives testing because they
// are the same on every call.
func (e *Engine) buildDispatcher(opts Options, backends *Backends) *Dispatcher {
	d := opts.Dispatch
	if d == nil {
		d = &Dispatcher{}
	}
	if d.Conditions == nil {
		runs := opts.SandboxRuns
		if runs == nil {
			// Read live, never bound here: this runs once in New, and where the
			// first sandbox company arrives by apply there is no coordinator yet,
			// so deliveries would be screened as though no run could exist and a
			// seat with a job running would take new turns beside it.
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
		// Read off the live epoch per dispatch: buildDispatcher runs before
		// startNotifications, so a captured registry would be the empty boot one
		// and every merge would supersede nothing for the process's life.
		d.Prompts = e.notifyPrompts
	}
	if d.Parties == nil {
		// Live registry, likewise: a reply offered to a parked run is labelled by
		// who wrote it, as its trigger was.
		d.Parties = e.liveParties
	}
	if d.Park == nil {
		d.Park = e.park
	}
	if d.Pause == nil {
		d.Pause = e.holdInbox
	}
	if d.HoldSandbox == nil {
		// Live, as Answer below: the coordinator may arrive by apply.
		d.HoldSandbox = e.holdSandboxSeat
	}
	if d.Beside == nil {
		d.Beside = e.beside
	}
	if d.SetAside == nil {
		d.SetAside = e.setAside
	}
	if d.HoldAside == nil {
		d.HoldAside = e.holdAside
	}
	if d.Budget == nil {
		d.Budget = e.budgetPark
	}
	if d.Provider == nil {
		d.Provider = e.providerPark
	}
	if d.Failure == nil {
		d.Failure = e.failureHold
	}
	if d.FailureCleared == nil {
		d.FailureCleared = e.failureCleared
	}
	if d.Held == nil {
		d.Held = e.conversationHeld
	}
	if d.Answer == nil {
		// Live, as the conditions above: bound here, a node whose sandbox arrived
		// by apply would offer no reply to any parked run for the process's life.
		d.Answer = e.answerParkedRun
	}
	if d.NoteDeferred == nil {
		d.NoteDeferred = e.node.Host().NoteDeliveryDeferred
	}
	if d.ReviewSettled == nil {
		d.ReviewSettled = e.AccessReviewSettled
	}
	if d.DelegatedRequests == nil {
		d.DelegatedRequests = e.readDelegatedRequests
	}
	if d.StaleWakes == nil {
		d.StaleWakes = e.dropStaleWakes
	}
	if d.Observe == nil {
		d.Observe = e.observe
	}
	if d.Settled == nil {
		d.Settled = e.settleWaiting
	}
	if d.Idle == nil {
		d.Idle = e.nudgeRouter
	}
	if d.DeadLettered == nil {
		d.DeadLettered = e.reportDeadLetter
	}
	if d.Gone == nil {
		d.Gone = e.budgetDeliveryGone
	}
	if d.Identify == nil {
		// Live epoch at the moment of the panic: an apply can remove the seat
		// after the dispatcher is built.
		d.Identify = func(handle string) (string, string) {
			return seatIdentity(e.Company(), handle)
		}
	}
	// The completion ledger is the fleet's: a redelivery landing on a peer must
	// find the record or the turn runs twice, so it lives on the coordination
	// store. What a conversation's next turn is told of earlier ones is the
	// fleet's too: the executor log, replicated to every data node and read by
	// the turn's dispatch (conversation.go). Not nil-checked: [New] refuses
	// Backends without a fleet.
	if d.Completions == nil {
		d.Completions = ledgerstore.NewFleetCompletions(backends.Fleet)
	}
	if d.SeenThrough == nil {
		d.SeenThrough = ledgerstore.NewSeenThrough(backends.Fleet)
	}
	return d
}

// Start brings the engine up.
//
// The long-lived loops run on a DETACHED context: the seat host derives its
// heartbeat and sweep loops from it, so on a signal context leases stop
// renewing at SIGTERM, before the drain, and peers claim seats this node still
// runs turns on. STOP ends an engine; ctx's end counts only while the node
// starts. The first placement pass runs acquire hooks inline with no total,
// and the caller's shutdown begins only once Start returns, so ctx ending
// during the start begins the host's drain at once ([node.Node.BeginDrain],
// on [Engine.stopping]'s allowance): the pass claims nothing more.
func (e *Engine) Start(ctx context.Context) error {
	cutDone := make(chan struct{})
	stopCut := context.AfterFunc(ctx, func() {
		defer close(cutDone)
		e.node.BeginDrain(seat.WithStopBudget(context.WithoutCancel(ctx), e.stopping()))
	})
	err := e.node.Start(context.WithoutCancel(ctx))
	// A drain already begun is waited for, so a Start returning after its context
	// ended returns with the host draining, and the goroutine Start began ends
	// inside Start: left running, its presence give-back could still ask the
	// store the caller's teardown is closing.
	if !stopCut() {
		<-cutDone
	}
	if err != nil {
		return fmt.Errorf("engine: start: %w", err)
	}
	// The issue reporter's sender, on every node: no singleton, each sends what
	// it detected and the fleet's record decides who may.
	e.startIssueSender(ctx)
	// The claim gate's watch, after the host's first pass: it asks the host to
	// sweep again once a refused pass can be answered, and before the host runs
	// there is none to have been refused. Detached like the host (admission.go).
	e.startAdmissionWatch(ctx)
	// The live token meters, which the dashboard's header pushes from. After the
	// host is running: the first frame is published at once onto the queue
	// node.Start starts, and a backend requiring Start refuses a publish before
	// it, which would lose that frame and leave every dashboard ignorant of the
	// budget for a whole interval. Detached like everything here
	// ([Engine.startBudgetReports]).
	e.startBudgetReports(ctx)
	// And what waits in the inboxes of the seats this node holds, after the host:
	// it reads the seats the host claimed (inboxreport.go).
	e.startInboxReports(ctx)
	// A person's message queued behind a busy seat, shown as queued (queued.go).
	// After the host: it answers whether this node runs the seat.
	e.startWaitingWatch(ctx)
	// The access reviews, armed with every seat this node holds: a review a node
	// did not live to ask for is asked for by whoever holds the seat now.
	reviews := newAccessReviews(context.WithoutCancel(ctx), e.checkAccessReviews)
	e.accessReviews.Store(reviews)
	reviews.kick()
	// The auxiliary-spend ledger's timer, detached on the meters' cadence
	// (coalesce.FlushInterval). Calls before it wait in their buckets for its
	// first tick, and [Engine.teardown] flushes what is left.
	e.auxSpend.Run(context.WithoutCancel(ctx))
	// And the reads ledger's, likewise.
	e.codeReads.Run(context.WithoutCancel(ctx))
	// After the host is running, detached like the host. Before it every watched
	// duty reads as not live and the watchdog stands down for the process's
	// life, looking exactly like a healthy armed one. The precondition is
	// CHECKED, because the failure is silent: a stood-down watchdog is
	// indistinguishable from outside until a node wedges and is never
	// restarted. [Engine.Stop] disarms it.
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
	// Printable before a company exists: an unconfigured node did start, and a
	// log saying so only once a revision lands would make the state an operator
	// most needs to recognise look like a boot that hung.
	company := e.Company()
	name := "(unconfigured)"
	if company != nil {
		name = company.Config.Name
	}
	log.InfoContext(ctx, "engine_started", "company", name,
		"seats", len(company.Seats()), "configured", company != nil)
	// And into the audit log, which an operator reads days later. Not on an
	// unconfigured node: the event names the company, and "(unconfigured)" would
	// be a company by that name in every listing grouping on it.
	if company != nil {
		e.publishLifecycle(ctx, events.New(
			types.OrgStarted{OrgName: company.Config.Name}, tracing.TraceOf(ctx)))
	}
	return nil
}

// publishLifecycle puts one node's start or stop into the audit event log, best
// effort and unattributed to any seat: the envelope's source is this node,
// which tells one member's pair from another's. On a bound of its own
// ([lifecycleContext]), never a step of the stop's allowance.
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
// start or stop, a seat's spawn or release. Five seconds, the JetStream client's
// request timeout for a publish with no deadline, matching the released seat's
// last memory publish ([memoryFlushTimeout]): a stream silent that long will
// not acknowledge in any stop grace, and the loss is an audit-log line.
//
// Never a step of the stop's allowance ([seat.StopBudget]), the coordination
// store's: the event stream is replicated apart and can lose quorum alone, and
// an unacknowledged publish waits out any deadline it is handed (which switches
// the client's five seconds off), so it would spend the time every lease
// give-back needs.
const lifecyclePublishBudget = 5 * time.Second

// lifecycleContext is what a lifecycle event is published on: ctx without its
// cancellation, bounded by [lifecyclePublishBudget] and nothing else.
//
// Free of ctx's cancellation because the event records something already
// happened (the node started or was told to stop, the seat was taken or let
// go): a caller that gave up on what follows has not taken that back, and a
// release's context is routinely one a shutdown has already ended.
func lifecycleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), lifecyclePublishBudget)
}

// Drain is the first half of a graceful stop: this node stops taking work,
// lets the turns already running finish, and hands its seats back. Every
// backend stays open; [Engine.Stop] is the half that closes them.
//
// Split from Stop for the HTTP surface, which comes down between the halves:
// /health and /ready are what an orchestrator reads while turns finish, and
// both read the broker and coordination store the teardown closes. Once: later
// calls wait for the first. Bounded by ctx ([node.Node.Drain]) and past it by
// [lifecyclePublishBudget] for the stop's announcement ([lifecycleContext]).
// After its seats are given back it waits for the tool calls their executor
// runs still had running ([mcpbridge.Bridge.AwaitCalls]), which release does
// not wait for and which write through what the teardown closes.
func (e *Engine) Drain(ctx context.Context) {
	// One allowance for every lease the stop gives back, from its first
	// ([Engine.stopping]).
	ctx = seat.WithStopBudget(ctx, e.stopping())
	e.drainOnce.Do(func() {
		// Before anything that can block, so every surface refuses new work from
		// the moment the drain was decided, not whenever the seat host reaches it
		// (the announcement below is a broker round trip).
		e.shuttingDown.Store(true)
		// First of what does block. The drain and teardown reap MCP trees, join
		// goroutines and wait on turns indefinitely, which an armed watchdog
		// would take for the wedge it exists to end; exiting mid-drain abandons
		// the seat release that makes it graceful and costs every peer a full TTL
		// of dark seats.
		if e.watchdog != nil {
			e.watchdog.Stop()
		}
		// No apply runs on a stopping node: one past this point would restart
		// what the drain and teardown end (the scheduler and its duty, passes
		// handed to loops that are gone, the inbound edge). The drain waits out
		// an apply already running (quick, on the cancelled context) and refuses
		// later ones. Here because it is the one gate that sees an apply arriving
		// over the STREAM from a peer, which the API's drain gate never does.
		// Before the announcement below, so the company it names is the one this
		// node stops on.
		e.applying.Lock()
		e.stopped = true
		e.applying.Unlock()
		// A draining node asks no review: its seats are about to be held
		// elsewhere, whose admission checks them.
		e.accessReviews.Load().stop()
		// Announced where the stop is decided, not completed: the drain's far end
		// would not survive a second interrupt or the supervisor's kill grace
		// running out, and the audit log must not lose that this node was told to
		// stop. Beside the drain, not in front, joined before it returns: a
		// stream that will not acknowledge holds the announcement for its whole
		// bound ([lifecyclePublishBudget]), time the presence give-back the drain
		// begins with need not wait out.
		var announced sync.WaitGroup
		if company := e.Company(); company != nil {
			ev := events.New(types.OrgStopped{OrgName: company.Config.Name},
				tracing.TraceOf(ctx))
			announced.Go(func() { e.publishLifecycle(ctx, ev) })
		}
		// The sessions of the direct jobs this node launched are saved beside the
		// wait for turns, not after: the stop usually takes such a job with its
		// container, unit or host, its relaunch continues its newest save, and the
		// wait for turns is time already spent. Joined before the drain returns,
		// so the teardown closes nothing a save writes through; bounded by its own
		// wait (the sandbox package doc's "A save as its node drains").
		saved := make(chan struct{})
		go func() {
			defer close(saved)
			if rt := e.sandbox.Load(); rt != nil {
				rt.coordinator.SaveHosted(ctx)
			}
		}()
		e.node.Drain(ctx)
		// The calls its seats' runs still had running, once every seat is given
		// back: each to its record before the teardown closes what it records
		// through.
		e.bridge.AwaitCalls(ctx)
		announced.Wait()
		awaitDrainSaves(ctx, saved)
	})
}

// awaitDrainSaves waits for the drain's saves ([Engine.Drain]) to end, saying
// so first where they have not, as the bridge's wait for its calls does: a
// drain that holds a node's stop says what it waits on.
func awaitDrainSaves(ctx context.Context, saved <-chan struct{}) {
	select {
	case <-saved:
		return
	default:
	}
	log.InfoContext(ctx, "executor_session_saves_draining",
		"detail", "the sessions of this node's direct jobs are still being saved as it drains; "+
			"waiting for each to pass its spool, for at most the drain save's wait")
	<-saved
}

// ShuttingDown reports whether a drain has begun: true from the first moment of
// [Engine.Drain], never false again. The HTTP surface refuses new work on it
// and /health and /ready report it; deliberately not the seat host's own
// draining flag, which is set once the drain reaches the host.
func (e *Engine) ShuttingDown() bool { return e.shuttingDown.Load() }

// Stop drains, unless [Engine.Drain] already has, and shuts down. The drain
// first is the difference between a restart that resumes cleanly and one that
// redelivers half-finished turns: it stops claiming, hands back every seat and
// waits for in-flight handlers before anything closes. Once, like the drain: a
// second call (a failed start's cleanup after the ordinary one) waits for the
// first and does nothing more ([Engine.stopOnce]).
func (e *Engine) Stop(ctx context.Context) {
	e.Drain(ctx)
	e.stopOnce.Do(func() {
		e.teardown(ctx)
		log.InfoContext(ctx, "engine_stopped")
	})
}

// stopping is the allowance this node's stop gives its leases back on
// (presence, seats, the sandbox taker lease, duties, loop holds, MCP listings
// and status record), built once by the drain or a failed boot's teardown,
// whichever comes first ([seat.StopBudget] says why one allowance). Its size is
// [seat.StopAllowance] of the lease TTL less the share reserved for withdrawing
// the admission ([Engine.admissionShare]).
//
// Not the lifecycle events, custody flush, last auxiliary spend or usage flush:
// they carry records on a stream, what they cannot publish is lost or waits for
// the next start, and a stream that will not acknowledge must not spend the
// time the coordination store is owed ([lifecyclePublishBudget]), so each keeps
// its own bound. Nor the admission, which has no lapse.
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
// The teardown and not the drain, because the drain's wait for running turns
// has no bound and nothing ends the loops that hold until the teardown does: a
// pass finishing during that wait is ordinary, its time its own, not the
// seats' the drain gives back at the end. A failed boot reaches the teardown
// too and begins it the same way.
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
// The give-back is a step of the stop's allowance from the moment the teardown
// begins (at once if it has) and is cancelled when the allowance runs out;
// before that it keeps ctx's own bound. Counted from the teardown's start, not
// the give-back's, because the teardown waits for it: one still running then
// is time the stop spends (against a vanished store, a client's whole request
// timeout), while one finished during the drain's wait spent nothing the stop
// is owed.
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
// One implementation for [Engine.Stop] and [New]'s failure path, for
// [core.shutdown]'s reason: a half-built engine leaks what a built one does
// (apply loops, MCP process trees holding the company's credentials, an
// admission saying "this process may be publishing"). Not here is what only a
// running engine does: disarm the watchdog, announce the stop, drain. Nil-safe
// and never-started-safe: the boot path reaches it with any suffix of this
// list undone, and with no node where the failure preceded [node.New].
func (e *Engine) teardown(ctx context.Context) {
	// The stop's one allowance, the drain's if there was one
	// ([Engine.stopping]), and from here the one a hold given back by a loop
	// stopped below is charged to ([Engine.tearingDown]).
	ctx = seat.WithStopBudget(ctx, e.tearingDown())
	// First, the claim gate's watch, which reads the logs, router and fleet view
	// everything below takes down, and whose one gesture (asking the host for a
	// pass) has nothing left to claim for.
	e.stopAdmissionWatch()
	// Then the trigger that re-derives what a company routes by (the party
	// registry's): a rebuild over vendor wiring already ended would register
	// identities nothing serves.
	e.stopViewTriggers()
	// After the drain: the waiter's keepalive stops a running box being reaped,
	// so stopping it first would start the orphan clock on every in-flight run
	// while turns finish, and the coordinator's taker lease, held through the
	// drain while its keeps upload, is given back on this allowance once the last
	// has ended.
	e.stopSandbox()
	// The routers of busy seats' held inboxes, once the coordinator that starts
	// them has stopped.
	e.stopRouters()
	// And the watches over its seats' waits, which answer through the same
	// coordinator.
	e.stopRunWaits()
	e.stopWaitingWatch(ctx)
	e.stopNotifications(ctx)
	e.stopMaintenance()
	// The native half's duties before the core's, in the order the halves come
	// down below: the vector domain's writer and the chat prune publish into
	// logs the core's trim is deciding how far to purge, as does this node's
	// usage publisher (not a duty, but it writes the usage log the trim counts).
	e.stopEmbedding()
	e.stopChatRetention()
	// Every writer of a row this node's usage day counts, before the usage
	// publisher's last flush, or what they write after it waits for a next start
	// and is dropped from the fleet's history on a node that never restarts: the
	// learning passes, the scheduler (a late fire records a dispatch), Gitlet
	// (files each git read of a `visibility: unit` repository in the reads
	// ledger), the two ledgers' last flush once their producers stopped, and the
	// custody keeper (its settles make a stateless node's rows count). The
	// memory fill stops beside them though it writes no such row: it and the
	// learning passes query the store backends.Close closes and tick on a
	// detached context on purpose (they must not stop at SIGTERM), so a late
	// tick would run compaction and paid summarisation on a closed one.
	e.stopMemoryFill()
	e.stopLearning()
	e.stopScheduler()
	e.stopPickups()
	e.stopDelegatedSweep()
	e.stopFlowDuty()
	// Gitlet's git surface with them, not with the publishers further down (the
	// committer and the estate subject, which it still stops before): its git
	// reads are answered for peers on this node's git lane until its cache
	// stops, and each read of a unit's repository is one this day counts. Its
	// client requests are already finished or cut by the API's listener, closed
	// before this teardown, so what stops here is the cache those reads are
	// answered from; a peer's later read is answered unready and its walk asks
	// the runner-up ([Engine.stopGitletHost]). Before the ledgers' last flush:
	// no read is filed after its cache stops.
	e.stopGitletHost(ctx)
	// Before the custody flush below, which carries a node without `data` its
	// records: the two at once, on one budget ([Engine.stopLedgers]).
	e.stopLedgers(ctx)
	// Leaving the group, not only ending the pass: the attachment would keep
	// taking batches until the queue closes. Before backends.Close, which closes
	// the store the keeper writes and the broker it settles against.
	e.stopKeeper(ctx)
	e.stopUsage()
	e.stopRetention()
	e.stopIdentityDuties()
	e.stopBudgetReports()
	e.stopInboxReports()
	// Before the node's stop detaches the inboxes: an alarm firing into a
	// client that is closing would log a release it could not make.
	e.stopBudgetParks()
	e.stopProviderParks()
	// And the pause watch, for the same reason: a resume landing on a
	// client that is closing would lift a hold on nothing.
	e.stopSeatPauses()
	// The code-retention duty, whose every pass publishes purge steps and
	// re-declarations on the code log: a pass running past the withdrawal below
	// would append to a log a capacity coordinator may be sealing. Before
	// [Engine.releaseDuties] too: its loop claims the duty afresh every turn, so
	// one running past the release takes it straight back.
	e.stopCodeRetention()
	// The reconcile of code.repos, which runs beside the applies that ask for it
	// and publishes a record for every changed repository ([codeReconciler]):
	// the one in flight is ended and the next boot reconciles.
	e.stopCodeRepos()
	// The object store's collector, whose every pass pins the estate with a
	// barrier on the log of each domain a declared table names
	// ([Engine.startNativeCollector]), for the code-retention duty's two
	// reasons (an append once the admission is gone; a loop taking its duty
	// straight back past the release). Before the native runtime below, whose
	// tracker reader it reads the references through.
	e.stopObjectCollector()
	// The trash reaper, for the collector's reasons: its passes purge through the
	// domains' own publishers and its loop claims its duty afresh every turn.
	// Before the native runtime below, whose knowledge store they purge through.
	e.stopTrashReaper()
	// What this node publishes for its peers: the committer (a record for every
	// batch of pushes a peer proposes) and the estate subject (writes and
	// linearizable reads routed here). The committer before the subject it is
	// asked on: withdrawing the subject cancels answers in flight, so a queued
	// proposal would be answered with its own cancellation, a final failure for
	// a push that may yet land. Stopped first, the committer answers queued
	// proposals "not serving" (the asker takes them to the runner-up) and
	// finishes the batch in flight ([Engine.stopCommitter]). Gitlet's git
	// surface stops before both, above: its pushes and syncs publish through
	// them.
	e.stopCommitter(ctx)
	e.stopServingEstate(ctx)
	e.stopRunnerProbe()
	e.stopHostLogins()
	e.stopModelRetries()
	// This node's MCP listings' publisher, now its servers are down, and its
	// status record's writer, whose rows describe them (what each wrote is
	// removed once the seats are given back, below), and its instances no longer
	// answered.
	e.stopStatusRecords()
	e.stopMCPListings()
	e.stopMCPInstances(ctx)
	e.stopMCPChecks(ctx)
	e.stopMCPRestarts()
	// A sign-in session this node runs ends as failed, its record saying why, so
	// the person starts again on a node that is up.
	e.stopDeviceSignIns()
	// A check this node runs ends saying the node stopped, likewise.
	e.stopModelChecks()
	// After the drain and every loop that publishes, which is what the admission
	// says: the key means "this process may be publishing" and keeps a capacity
	// coordinator from taking its exclusion while this node runs
	// (statelog.AdmissionBlocks). Every loop above that appends to a log
	// (embedding, usage, trim, identity duties, code retention, the code.repos
	// reconcile, the collector's barriers, the trash reaper, Gitlet's syncs and
	// proposals, the committer, routed writes) stops before it is withdrawn;
	// withdrawing while a seat finished a turn or a duty published would tell a
	// coordinator the opposite of the truth.
	if err := e.withdraw(ctx); err != nil {
		log.WarnContext(ctx, "admission_not_withdrawn", "err", err,
			"detail", "a capacity operation will see this node as a possible "+
				"publisher until it boots again or an operator excludes it")
	}
	e.stopIntegrations()
	e.mintFindings.stop()
	e.stopAtlasCollect()
	e.stopAtlasWake()
	// After the integration loop, which runs the passes asking for a
	// re-activation: disarming first would let a pass arm one more on a node that
	// is shutting down.
	e.republish.stop()
	// After the drain, which released every seat and flushed each one's memory;
	// stopping before would leave the releases to the bounded flush alone. And
	// after the memory fill and learning passes, which write the memory it
	// carries.
	e.stopMemorySync()
	// After every duty loop above stopped and waited out its tick, so no tick of
	// this node runs once a peer can take the duty and no turn can claim one once
	// it is given back.
	e.releaseDuties(ctx)
	// Before the native backends, whose pages its walk reads, and before
	// backends.Close, which closes the broker its nudge listens on.
	e.stopSkillSync(ctx)
	e.stopServingFleetBroker(ctx)
	// Before backends.Close, with more at stake: the apply loops and the indexer
	// write, and an apply landing after the close would fail its transaction
	// mid-batch and leave the cursor ahead of the rows it describes. The native
	// half first (feeds, index, search answerer, all reading what the logs
	// write), then the core, whose logs are the last thing standing; its view
	// triggers ended at the top.
	e.stopNative(ctx)
	// Before backends.Close, which closes the store an answer reads: a peer's
	// question mid-teardown is declined rather than read from a closing file.
	e.stopHistory(ctx)
	// Likewise: a note answered `accepted` by a tearing-down node is one no round
	// will read.
	e.stopSteer(ctx)
	// Presence, whose leaves must reach the fleet while the broker is there.
	e.stopPresence(ctx)
	// The live relay's follower and answerer, on the same broker.
	e.stopLiveRelay(ctx)
	// A seat's memory, read from the store backends.Close is about to close.
	e.stopMemoryReads(ctx)
	// A run's live output, read from boxes this node is about to stop driving,
	// and a peer's read of a box on its host.
	e.stopExecutorTails(ctx)
	// What a relaunch of a seat it holds would be launched on, which it is about
	// to stop holding.
	e.stopRelaunchTargets(ctx)
	// Whether one of its parks holds a seat's inbox, likewise.
	e.stopHeldParks(ctx)
	e.stopSandboxHost(ctx)
	// The bridge's relay, whose exchanges serve sessions this node is about to
	// stop holding.
	e.stopBridgeFleet(ctx)
	// The settles of takes whose answers were lost, once nothing is left to lose
	// one (the drain ended the turns, the sandbox's resumes and the bridge's
	// relay are stopped) and while the coordination store they write is there.
	// Each loop made its last attempt in the window the teardown opened as it
	// began, over within steerSettleStopBudget of that (the ledgers' flush above
	// covers it where that flush spends its own bound); waited out here, having
	// spent none of the allowance the later give-backs are owed.
	e.runSteers.waitSettles()
	e.stopIssueSender()
	// Then the core, last thing standing (see stopNative above).
	e.stopCore()
	if e.node != nil {
		e.node.Stop(ctx)
	}
	// After the seats are released, with the broker still there: until a seat
	// lease is given back Gitlet's expiry reads this node among the holders, and
	// a holder that stops answering first costs a pass its expiry (codeholds.go).
	e.stopCodeHolds(ctx)
	// What this node published of itself (MCP listings, status record) is
	// removed together on what the give-backs left of the allowance: nothing
	// counts either since the presence went, and a removal running out of it
	// leaves them to the sweep and the bucket's age ([Engine.removeMCPListings],
	// [Engine.removeStatusRecord]). Together, so the time they overlap is charged
	// once.
	var removals sync.WaitGroup
	removals.Go(func() { e.removeMCPListings(ctx) })
	removals.Go(func() { e.removeStatusRecord(ctx) })
	removals.Wait()
	// After the seats are released, so a per-role child is normally already gone.
	// The backstop for the rest: a stdio server is a process TREE holding a seat's
	// credentials, and one left behind outlives the engine that vouched for it.
	e.stopSharedServers(ctx)
	// After everything that announces through it, before the broker its final
	// flush publishes onto is closed, and before the custody flush below.
	e.stopAuthEvents()
	// After every seat and loop that publishes, so their last events are in the
	// buffer, and before the broker closes under the last batch.
	e.stopCustody(ctx)
	e.stopWatchingDataNodes()
	if e.ownsBackends {
		e.backends.Close(ctx)
	}
	// Last, once nothing can run a phase: a phase still serving from the
	// directory would find its socket removed under its harness.
	e.closeSessions(ctx)
}

// closeSessions removes this process's tool-session directory. Nil-safe: a
// boot that failed before making it has none.
func (e *Engine) closeSessions(ctx context.Context) {
	if e.sessions == nil {
		return
	}
	if err := e.sessions.Close(); err != nil {
		// Nothing to retry: the directory holds sockets no process will dial
		// again. Logged so an operator who finds it knows why.
		log.WarnContext(ctx, "tool_sessions_not_removed", "dir", e.sessions.Dir(), "err", err)
	}
}

// custodyStopBudget is how long an orderly stop waits for the broker to take
// the events this node still holds. Ten seconds: many publishes to a broker
// that is up, short enough that a stop with no member reachable is not held
// long; what could not publish is logged as lost.
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

// StallLag is how far behind the worst live watched duty is: the time since its
// last beat, once it has missed one ([seat.Watchdog.Lag]). The watchdog's
// early warning, climbing towards the lease TTL where it fires. Zero means
// every watched duty is on its beat or nothing is watched, the same reading a
// health surface wants for both.
func (e *Engine) StallLag() time.Duration {
	if e.watchdog == nil {
		return 0
	}
	return e.watchdog.Lag()
}

// Node exposes this process's participation in the fleet, for the operator
// surfaces that report which seats it holds.
func (e *Engine) Node() *node.Node { return e.node }

// Backends exposes the infrastructure this engine runs on, for the API `crewlet
// run` serves beside it in one process: they share one broker and one store,
// two connections to one broker fail independently, and the store is exclusive
// to one process, so a second open would contend with itself.
func (e *Engine) Backends() *Backends { return e.backends }

// Cipher is this node's keyring: the ONE cipher of the process, exposed like
// [Engine.Backends]. `crewlet run` composes the config, secret and sign-in
// surfaces beside the engine and each seals or opens under the keyring built
// from Tier A (the reconciler authenticates a peer's revision under it, and a
// revision sealed under anything else no node could open). Handing them this
// one makes "one keyring" a fact. Never nil on an engine [New] built.
func (e *Engine) Cipher() secrets.Cipher { return e.cipher }

// Recorder is the process's one metrics recorder, or nil, exposed like
// [Engine.Backends]: a subsystem composed beside the engine (the backup service)
// measures something the catalogue declares, and a second recorder would be a
// second set of series. Nil is legal and records nothing.
func (e *Engine) Recorder() *metrics.Recorder { return e.metrics }

// Objectives is the node's service objective meter: what the API's ready
// reports are counted into beside the engine's own recording sites. Never nil
// on an engine [New] built.
func (e *Engine) Objectives() *objectives.Meter { return e.objectives }

// newObjectives builds the node's objective meter over rec, which may be nil:
// the events are then counted into the meter's own ledger alone, and the burn
// alarm still judges them.
func newObjectives(rec *metrics.Recorder) *objectives.Meter {
	// A nil recorder stays a nil interface; wrapped, the meter would call into it.
	if rec == nil {
		return objectives.NewMeter(nil, nil)
	}
	return objectives.NewMeter(rec, nil)
}

// mailboxTurn delivers one inbox partition to a seat: the node's TurnFunc
// ([node.Config.Turn]), which [New] installs and nothing else calls.
//
// Unexported because it is not an entry point: the node calls it only after
// admitting the delivery's turn ([node.Node.AdmitTurn]), so any other caller
// would start a turn the node never admitted (ADR-0063). A turn not arriving by
// the mailbox (a launch's completion, a person's recorded answer) re-enters
// through the sandbox coordinator's resume, which asks the same admission.
func (e *Engine) mailboxTurn(ctx context.Context, handle string, evs []*events.Event) queue.Result {
	return e.dispatch.Dispatch(ctx, handle, evs)
}

// conditionsFor answers the ownership and posture questions for one seat.
func (e *Engine) conditionsFor(executorRows func(string) (bool, bool)) func(string) inbox.Conditions {
	return func(handle string) inbox.Conditions {
		_, owned := e.node.Host().MayStart(handle)
		company := e.Company()
		// One call for both sandbox answers: a park frees a seat and opens a
		// question at once, and separate reads could see the seat between the
		// halves (free, nothing waiting), the delivery this seam exists to catch.
		var heldBySandbox, sandboxAwaitsAnswer bool
		if executorRows != nil {
			heldBySandbox, sandboxAwaitsAnswer = executorRows(handle)
		}
		paused, known := e.pauseOfHandle(handle)
		return inbox.Conditions{
			// Freshness, not membership: a renew at t proves exclusivity through
			// t+ttl, and a membership snapshot can be a full TTL stale, the window
			// this check exists to close.
			Owned: owned,
			// Off the epoch, not asserted true: a company with no providers.llm has
			// no model registry and every delivery to its seats is held on the
			// inbox until an apply brings one (nomodels.go).
			TurnEngineReady:     company != nil && company.Models != nil,
			SeatHeldBySandbox:   heldBySandbox,
			SandboxAwaitsAnswer: sandboxAwaitsAnswer,
			// The same gate the inbound edge and scheduler read, so a shedding node
			// refuses at every trigger admission, a seat's own inbox included.
			AdmitsTriggers: e.admits(),
			// A person's pause, off this node's watched copy, and whether that copy
			// has been read at all, which differs from "not paused" (seatpause.go).
			Paused:       paused,
			PauseUnknown: !known,
		}
	}
}

// seatFence is the per-round check a turn on this seat runs under: that this
// process still holds its admission grant, where it holds one, and that nobody
// has asked the turn to stop. The second half of the admission:
// [Engine.conditionsFor] asks [seat.Host.MayStart] once on delivery, and a turn
// then runs for minutes as a seat the placement sweep may have handed to a
// peer. The tool loop checks this fence every round ([seat.Host.Fence] says why
// it is on the epoch). A person's pause closes the same fence with
// [turn.ErrStoppedByPerson], ownership first (a seat this node no longer holds
// is the successor's to stop), read off this node's copy every round. The id is
// resolved once at build, since a pause is kept on it; a handle resolving to no
// agent seat is fenced on ownership alone.
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
			// No agent seat answers to the handle: no pause to read.
			return nil
		}
		return e.stopFor(seat, handle)
	}
}

// wakes is the queue every producer of a seat's wake publishes through: the
// node's ([node.Node.Wakes]), which confirms the seat's mailbox, its inbox and
// its priority lane, before a wake is sent to it, so a wake published while
// boot or an apply has not made the mailbox yet is retried rather than
// discarded by the stream. An engine built with no node (`crewlet validate`)
// has no mailboxes to confirm and publishes on its queue as it is.
func (e *Engine) wakes() queue.EventQueue {
	if e.node == nil {
		return e.backends.Queue
	}
	return e.node.Wakes()
}

// mailboxConfirmer is what a producer that spends something before it
// publishes (the scheduler's claim, an agent-to-agent channel) asks first:
// the node's [node.Node.ConfirmMailbox], so a refusal leaves nothing half
// done. Nil on an engine with no node, which confirms nothing.
func (e *Engine) mailboxConfirmer() interface {
	ConfirmMailbox(context.Context, uuid.UUID) error
} {
	if e.node == nil {
		return nil
	}
	return e.node
}

// park requeues a partition onto the seat's own inbox, republished one at a
// time. A failure part way is reported: the dispatcher acks only on success, so
// a partial requeue is a NAK and the whole partition is redelivered. Same-id
// copies the landed half left are collapsed by the dedupe at the top of the
// next screening, which is why that stage runs before any parking branch.
func (e *Engine) park(ctx context.Context, handle string, evs []*events.Event) (err error) {
	defer origin.Stamp(&err)
	id, err := e.seatID(handle)
	if err != nil {
		return err
	}
	subject := topics.AgentInbox(id)
	// As one unit: a partition holding a person's message returns to the lane
	// whole, siblings with it, not the marked event alone with the rest on the
	// inbox's tail ([queue.Together]).
	for _, ev := range queue.Together(evs) {
		if err := e.wakes().Publish(ctx, subject, ev); err != nil {
			// The seat by name: the subject is a uuid, which tells a reader nothing
			// of which colleague lost their mail.
			return fmt.Errorf("engine: requeue %s onto seat %q (%s): %w",
				ev.Type, handle, subject, err)
		}
	}
	return nil
}

// turnInputFor renders a dispatched [Request] as the loop's own [turn.Input].
//
// A named mapping, not a literal at the call site: every field is one the loop
// cannot derive and degrades silently without (a missing Depth checks the
// delegation cap against zero, a missing Reply skips every delivery gate, so a
// turn closes `done` having answered nobody). A literal drops a field quietly;
// a function with a test does not.
func turnInputFor(req Request, reply turn.Reply) turn.Input {
	return turn.Input{RunID: req.RunID, Depth: req.Depth, Reply: reply}
}

// runTurn is the default turn: build the seat's runner and drive the loop.
func (e *Engine) runTurn(ctx context.Context, req Request) (_ turn.Result, err error) {
	defer origin.Stamp(&err)
	// This execution's own identity, settled first so the span, events and tools
	// name the same run. Not the work key (ADR-0017): a trigger that fails
	// without acting is NAK'd and redelivered, so one key runs several times and
	// a run borrowing it would overwrite the attempt it repeats. The dispatch's
	// where there is one ([Dispatcher.Dispatch] mints it so its abandon record
	// names the run this turn publishes under); a Request assembled elsewhere
	// carries none, so one is minted here.
	if req.RunID == "" {
		req.RunID = newRunID()
	}
	// The turn's own span, opened first so every phase, LLM round and tool call
	// is a child and every event carries its id. The dispatcher already restored
	// the trigger's trace onto ctx, so this is a child of the wake.
	ctx, span := tracing.Start(ctx, "engine", "agent.turn",
		attribute.String("crewlet.seat", req.Handle),
		// `turn_id` is the run id, spelled as every event and screen spells it:
		// another name would make a trace query find the dispatch span and neither
		// the resume span nor the box's own.
		attribute.String("crewlet.turn_id", req.RunID),
		attribute.String("crewlet.work_key", req.WorkKey),
		attribute.Bool("crewlet.coalesced", req.Coalesce),
		attribute.Int("crewlet.delegation_depth", req.Depth))
	defer span.End()

	// What woke this turn is what its reads are no older than: every
	// constituent's committing record goes into this node's floors before the
	// first read, so a list read at `session` shows the change the seat was woken
	// for.
	e.observeTriggers(ctx, req.Events)

	// The working indicator, up before anything slow: the prefetch reads a chat
	// thread and searches the knowledge base over the network, work a person
	// already waits through. It raises nothing for a trigger nobody watches a
	// composer for ([chatMetadataOf]), and a nil session's methods are no-ops.
	// Held under the run, not the unit of work: the turn parks on its executor's
	// run, and the resume that rejoins it ([Engine.resumeWorkingStatus]) and the
	// stop that releases it ([Engine.releaseWorkingStatus]) read the row's turn
	// id, this run's (ADR-0017); under the work key neither finds it.
	status := e.beginWorkingStatus(ctx, req.Handle, req.RunID, req.Ask())
	// The messages this turn picked up no longer wait: their queued line has
	// become this turn's working one ([notify.StatusDriver.Begin]), and a wait
	// left behind would bring "queued" back when the turn ends.
	e.settleWaiting(ctx, req.Handle, wakeIDsOf(req.Events))
	// Ended on every path out of this frame, a failed runner build included
	// (it raised an indicator and ran no turn). `working` stays false until a
	// suspension's own ROW says a detached run is coming back, so every other
	// path clears, a redelivered trigger raising a fresh indicator when it runs
	// again ([endWorkingStatus]).
	var working bool
	defer func() { endWorkingStatus(ctx, status, working) }()

	// Pinned once: two epoch reads can straddle an apply, and a turn building its
	// runner from one revision and taking its round caps from the next runs a
	// company that never existed.
	company := e.Company()
	// Before the runner, which needs it: every phase event carries the turn's
	// identity, and a runner without it publishes phases attributed to nobody.
	tel, err := e.describeTurn(ctx, company, req)
	if err != nil {
		// Nothing started, so nothing ended: the dispatcher hands the delivery
		// back rather than recording a turn that never ran.
		return turn.Result{}, err
	}
	// The turn is on the record before anything slow, the prefetch being the
	// first: it reads a chat thread and searches the knowledge base over the
	// network, and until a phase opened nothing said this run existed. Every
	// returning path closes it ([Engine.publishTurnStarted]).
	e.publishTurnStarted(ctx, tel, req.Depth, req.DelegationChain, false)
	// The turn's meter, before its first model call: the prefetch makes several
	// on the seat's auxiliary chain, each charged through it
	// ([turnTelemetry.aux]), so a window they fill is held before the executor's
	// first round rather than learned from its refusal once the vendor has
	// billed it. Off the pinned epoch, so a ceiling raised mid-turn cannot move
	// the limit a round is judged against.
	tel.budget = e.meterFor(company, req.Handle)
	// The ask, not the partition: a coalesced conversation reaches the model as
	// one merged digest ([Request.Trigger], coalesce.go).
	task := tel.task
	// Rendered before the runner, which freezes it: the runner receives strings
	// with nowhere to re-fetch from, so a self_iterate loop cannot move the
	// system prompt under the executor. A thin trigger whose turn-start search
	// was skipped is served by the executor calling search_knowledge, on a query
	// it writes once it knows what the task needs. The seat's access and what
	// changed in it since last told, read once and frozen on the turn like the
	// endings below; its notes written before a told change are marked
	// (capability.go).
	told := e.capabilityFor(ctx, company, req.Handle, req.RunID)
	blocks := e.prefetchFor(ctx, company, req, task, tel.askParts, tel.aux(), tel.boundary, told,
		tel.workingFor, e.turnReadsAtlas(ctx, company, req.Handle, tel), tel.workItem)
	// The skills offered to this turn, carried onto its completion so the curator
	// ages a skill on when it was last put before a model, not archiving ones a
	// seat reads every turn.
	tel.skills = blocks.SkillIDs

	reply := ReplyFor(req.Events)
	turnIdentity := tel.runnerTurn(company, req.Depth, req.DelegationChain,
		task, reply, e.WithheldContacts())
	// The colleague's ask this turn answers, if one woke it: answered from this
	// frame where the turn ends here, from its run's record where it parks on
	// its executor's run (a2a.go).
	ask := colleagueAskOf(askOf(req.Events))
	// The turn's conversation, read once at dispatch under the turn's token
	// (conversation.go): how its earlier turns ended, told in every brief as the
	// runner freezes it, and the head the first launch continues and freezes for
	// its relaunches. No launch reads it again.
	conversation := e.readConversation(ctx, company, req, tel.boundary)
	// The executor's launcher over the pinned company ([Engine.agentRunFor]) and
	// the box a person's note reaches the turn through until it launches its
	// executor, which hands what the box holds to the executor's record
	// (steer.go). With the events the turn was dispatched for, under which a park
	// recorded the window an earlier try of this delivery's launch was refused
	// below its floor in, so this launch alone is held after it ([floorWaits]).
	launcher := e.agentRunFor(company, req.Handle, turnIdentity.Context, ask, conversation, told,
		deliveryOf(req.Events), nil, nil)
	box := steer.New()
	r, err := company.RunnerFor(ctx, req.Handle, e.seatRegistry(company, req.Handle), RunnerInput{
		Task:    task,
		Compact: e.compactorFor(company),
		Context: blocks,
		Skills:  e.skills,
		Reply:   reply,
		// The seat's own writes since the turn began answer a pickup (ADR-0122).
		PickupAnswer: e.pickupAnswerFor(req.Handle, tel.startedAt, reply),
		// Bounded at render: newest endings whole while they fit, the rest
		// counted ([prompts.EndingsMaxBytes]), frozen on every park.
		Endings: conversation.endingsBlock(),
		// The seat's access and what changed in it, frozen beside the endings.
		Access:            told.access,
		CapabilityChanges: told.changes,
		Publisher:         e.backends.Queue,
		Turn:              turnIdentity,
		Sessions:          e.sessions,
		Metrics:           e.metrics,
		Launcher:          launcher,
		Steer:             box,
		Markers:           e.markers(),
		Latch:             e.onboarded,
		// The meter the context assembly was charged through.
		Budget: tel.budget.budget(),
		// The round-cap extension judge, from the same pinned epoch, for the
		// onboarding pass, the one engine loop with a cap above its budget.
		Judge: e.judgeFor(company, req.Handle, tel.aux()),
		// The seat's headroom for a sub-agent spawn. Three-valued: nil is "no
		// ceiling configured", and a FAILED read refuses the spawn.
		Remaining: e.remainingFor(company, req.Handle),
		// Built here at turn start: the epoch it closes on is the grant THIS turn
		// was admitted under, and a later fence would compare a re-claim against
		// itself.
		Fence: e.seatFence(req.Handle),
		// The working indicator's phase updates, from the one frame that knows a
		// phase opened, the call that publishes agent_phase_started, so the
		// indicator and the event cannot name different phases.
		OnPhase: func(ph phase.Phase) { status.Phase(ph.String()) },
	})
	if err != nil {
		// Closed because opened: a start with no end reads as a turn still running
		// or whose process died, so it ends as a run that failed before its first
		// phase, the build's error as the reason. The retry loses nothing: the
		// redelivery is a NEW run with its own start and end (ADR-0017) and the
		// learning dispatcher marks work spent only on a settled outcome. Charged
		// like any ending: a task's turn count is its attempts and its context
		// cost is on the stream ([Engine.endSegment]).
		e.endSegment(ctx, tel, runner.Spend{}, turn.Result{}, err,
			tel.chargeFor(runner.Spend{}, turn.Result{}, err, time.Now().UTC()))
		return turn.Result{}, err
	}

	// A person may now steer it. The box closes as soon as the turn returns,
	// before its completion is published, so the record of a note the turn missed
	// precedes the record that it ended; the defer backstops every other exit.
	// Closing twice records nothing twice, and a box the launch handed to its
	// run's record closes nothing here (steer.go).
	closeSteer := e.openSteer(req.RunID, req.Handle, box, r)
	defer closeSteer(ctx)
	// And what it holds from Gitlet's expiry, answered for by this node while the
	// turn runs; by its return a launched run carries its holds on its row
	// (codeholds.go).
	defer e.turnHolds.running(tel.holds)()

	// Before the executor, on its own budget, so onboarding cannot spend the
	// deciding phase's whole budget reading pages. A failure does NOT fail the
	// turn: the seat stays un-onboarded, since refusing to work would let a
	// briefly unreachable knowledge base stop the company. Except a budget
	// refusal, which ends the turn: its refused round is counted, so the window
	// reads past its ceiling and every later call is certain to be refused on the
	// pinned clock ([toolloop.BudgetMeter.Refused]), and entering the loop would
	// pay for nothing and record an executor phase that ran no round. The turn
	// publishes as budget_exhausted and the dispatch parks the seat until the
	// window turns.
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
		// What the turn answered without being woken for it: the waiting messages
		// its thread block showed, which the dispatch records as worked through
		// beside its triggers. Off the block this frame assembled, as the loop
		// never knew what a prompt showed (workedthrough.go). Only for a turn that
		// RAN: a refused onboarding showed a model nothing. An edited message is
		// recorded at the version shown and for whom this turn works
		// (tel.workingFor, the carrier its ceiling is composed from), so the mark
		// drops only that edit's wake made for the same whom.
		res.WorkedThrough = workedThroughKeys(threadOf(req.Ask()), blocks.ThreadContextAnswered,
			tel.workingFor)
	}
	// The box closes the moment the turn returns: no later step reads a note, and
	// one offered from here on is answered `closed`, or filed on the run's record
	// where the turn launched one.
	closeSteer(ctx)
	// The segment's charge is decided first: a segment charged to nothing hands
	// what it spent to the suspension written below.
	spend := r.Spend()
	charge := tel.chargeFor(spend, res, err, time.Now().UTC())
	// Published on both paths: an error means a phase broke, when a dashboard
	// most needs the turn closed (phase events already put the seat into
	// `working`). After what its auxiliary calls cost, and charged to the work
	// item after the record of its end exists ([Engine.endSegment]). Before the
	// park is written: that write opens the run to everything that can END the
	// parked turn (its collection, a loss, a person's stop), and each end must be
	// newer than the park. Published after, a stop landing in between is dated
	// before the park and readers taking the newest record leave the turn parked
	// for good. The runner holds the parked state until the frame unwinds, which
	// this does not.
	e.endSegment(ctx, tel, spend, res, err, charge)
	stoppedAtPark := false
	if res.Suspended {
		// Its answer is what the indicator follows, not `res.Suspended`: intent
		// to park and a run that can be resumed differ, and this call establishes
		// the second ([stillWorking]).
		parked, parkErr := e.persistSuspension(ctx, r, req.RunID, tel, charge)
		if turn.Stopped(parkErr) {
			// A person stopped the run between its launch and its park: the run is
			// ended and announced, the refused park closed the turn as stopped
			// ([Engine.closeStoppedAtPark]), and this frame ends as a turn stopped
			// at its next round does, the dispatcher's own record of who stopped it
			// and that the trigger is worked ([Dispatcher.stopped]).
			res.Suspended, err, stoppedAtPark = false, parkErr, true
		} else {
			working = stillWorking(parked, parkErr)
		}
	}
	// And, if a colleague asked for this turn, the answer they wait for, where the
	// turn ended here (a parked turn is answered by the frame that ends it).
	// After the completion event, because the reply wakes another seat and this
	// turn's record should exist first. Not for a turn whose run a stop ended,
	// between launch and park or while launching ([stoppedByRun]): that stop's
	// ending answers the colleague and records the end on the executor log
	// ([sandbox.CoordinatorOptions.CloseStopped]); answered here too, the two
	// answers race each other's open check and both reach the asker.
	if stoppedAtPark || stoppedByRun(err) {
		e.abandonedLaunches.Delete(req.RunID)
		return res, err
	}
	answered := e.answerColleague(ctx, company, answeringOf(req), ask, res, err)
	_, abandoned := e.abandonedLaunches.LoadAndDelete(req.RunID)
	switch {
	case abandoned && turn.Stopped(err):
		// A launch the turn's fence refused once its run's record was written: the
		// launch abandoned the record, so no ending of the run owes the turn's end
		// and this frame records it, stopped, nothing captured since no job ran.
		e.endTurnNow(ctx, telTurnEnd(tel, stoppedEnding(res, reply, err, answered)))
	case abandoned:
		// A first launch that failed on its way to its box once its record was
		// written: abandoned as a stopped one is, and ended here, failed, saying
		// why, so the turn's row on the executor log (whose begin held the base it
		// froze) ends now and releases that base, and the conversation's next turn
		// is told how this one ended, instead of both waiting a day for retention
		// to end it as lost.
		e.endTurnNow(ctx, telTurnEnd(tel, launchAbandonedEnding(res, reply, answered, err)))
	case !res.Suspended && err == nil && req.ConversationKey != "":
		// A turn of a conversation that never launched its executor and ended here
		// without error (refused before its first round, at the delegation cap)
		// still ends on the executor log, so the next turn is told how; with no
		// run, its row names no session or read. Not a turn of no conversation,
		// which no later turn is told of, and not one whose frame failed: its
		// delivery goes back to the dispatcher to be parked or run again as a turn
		// of its own, and an ending recorded for the attempt would tell that turn
		// its work had failed.
		e.endTurnNow(ctx, telTurnEnd(tel, endingOf(res, reply, answered)))
	}
	return res, err
}

// nodeStatus is what this node advertises about itself on every heartbeat:
// the live core its presence carries. Its MCP, model and runner rows and what
// it runs on ride its status record instead ([Engine.statusRecordRows]). Read
// live per beat, not cached: a cached in-flight count reports work finished a
// minute ago. An error means it did not answer and the beat publishes nothing
// ([seat.Config].Status); decided here because only this frame knows which of
// its reads a dead context could have cut short.
func (e *Engine) nodeStatus(ctx context.Context) (coord.NodeStatus, error) {
	status := coord.NodeStatus{
		StartedAt: e.startedAt, AdminPosture: e.adminPosture, Build: version.String(),
		ParkedStateVersion: execstate.Version,
	}
	if b := e.backends; b != nil && b.Queue != nil {
		status.InFlight = b.Queue.InFlightCount()
	}
	if n := e.node; n != nil {
		if host := n.Host(); host != nil {
			status.Draining = host.Draining()
		}
	}
	// The replication loops (every state-log domain's applier, on data nodes
	// only), so an operator asking why a fresh node holds no seats sees the
	// answer in the fleet view. Not a readiness signal
	// ([coord.NodeStatus.AppliersReady]).
	for _, loop := range e.StateLogStatus(ctx) {
		status.AppliersTotal++
		if loop.Ready {
			status.AppliersReady++
		}
	}
	// A domain read the budget cut short counts as not ready, so a count taken
	// past the deadline understates a healthy node and cannot say which rows the
	// deadline reached. Checked once the reads are done: it may refuse a count
	// that finished right at the deadline and never publishes one that did not.
	if err := ctx.Err(); err != nil {
		return coord.NodeStatus{}, fmt.Errorf("node status: the beat's budget ended "+
			"while the replication positions were being read: %w", err)
	}
	// What is firing here, off the tracker the health envelope reads
	// ([Engine.Alarms]): the evaluation the gauge and alarm log lines were fed,
	// never a second, from memory. A node with no tracker or no evaluation yet
	// publishes no alarms, which a peer reads as "not saying", not a quiet node
	// ([coord.NodeStatus.AlarmsEvaluated]).
	if tracker := e.alarms.Load(); tracker != nil {
		status.Alarms, status.AlarmsEvaluated = tracker.Marks()
	}
	if fn := e.posture.Load(); fn != nil {
		// A posture read that did not answer says nothing, however it failed. The
		// reporter's fail-open answer is serve, and publishing it would put a
		// confident healthy posture on a node whose control plane is unreadable;
		// only an answered read is carried, while the rest of the status still is
		// (in-flight count, drain state and admin posture stay true). The
		// reporter's error, not this frame's clock, says which: the beat's context
		// caught only a read overrunning the budget, while a plane refusing at
		// once came back fast with serve.
		if posture, err := (*fn)(ctx); err == nil {
			status.Posture = posture
		}
	}
	return status, nil
}

// StartedAt is when this engine was built, which is when the node started: one
// instant for every surface. The API in this process reports it as `started_at`
// and the heartbeat carries it to every peer's fleet view, so the node's health
// and the fleet's picture cannot disagree about its uptime.
func (e *Engine) StartedAt() time.Time { return e.startedAt }

// SetPosture supplies the config-lag reporter the presence heartbeat
// advertises. A setter, not an option: the reconciler owning the posture is
// built after the engine it converges against. Safe to call while running; the
// heartbeat reads it per beat. The context is the beat's, already bounded
// ([seat.Config].Status). An error means the posture was not read and the
// heartbeat publishes none: hand it [Reconciler.ReadPosture], never the
// fail-open [Reconciler.Posture].
func (e *Engine) SetPosture(fn func(context.Context) (string, error)) { e.posture.Store(&fn) }

// SetAdmits supplies the gate deciding whether this node takes new inbound
// work. A setter for SetPosture's reason (an option at [New] could never be
// filled, leaving [notify.Service] admitting everything on a shedding node) but
// a separate wire: the reporter is read per heartbeat and may pay a live
// control-plane read, while this is read per delivery and must not
// ([Reconciler.Admits] says which value it answers from). Nil admits
// everything (single node, or before a control plane). A shedding node DEFERS a
// delivery back to the broker, consumer quiesced, rather than route it against
// a company it is unsure of.
func (e *Engine) SetAdmits(fn func() bool) {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	e.notify.admits = fn
}

// SetOnCompanyPublished registers a hook run after every published company has
// been converged on, for surfaces rendering the company rather than its
// activity: the dashboard's roster, org tree and tool catalogue derive from it,
// and a seat added, edited or removed produces no event a projection could
// learn from (an overlay merge cannot express a row going away). A company
// publish, not an apply: called last from [Engine.followCompany], at boot and
// on every apply, once everything the payloads are cut from is rebuilt. A
// setter for SetPosture's reason; safe while running, and to leave unset.
func (e *Engine) SetOnCompanyPublished(fn func(context.Context)) { e.onPublished.Store(&fn) }

// notifyCompanyPublished runs the hook, if one is set.
func (e *Engine) notifyCompanyPublished(ctx context.Context) {
	if fn := e.onPublished.Load(); fn != nil && *fn != nil {
		(*fn)(ctx)
	}
}

// SetOnCatalogueChanged registers a hook run after the current epoch's tool
// catalogue changed without a published company: a shared MCP server brought
// back with different tools, or whose failed start a retry brought up, refiled
// into the current epoch (mcpsupervise.go). The dashboard's tool catalogue is
// re-sent on every published company ([Engine.SetOnCompanyPublished]) and
// corrected by no event, so without this it lists tools as at publish while
// agents call the new ones. Its own hook, since the published one also decides
// every open socket again, which a catalogue change gives no reason to do. A
// setter likewise. Run on the supervisor's goroutine after the refile, holding
// no engine lock.
func (e *Engine) SetOnCatalogueChanged(fn func(context.Context)) { e.onCatalogue.Store(&fn) }

// notifyCatalogueChanged runs the hook, if one is set.
func (e *Engine) notifyCatalogueChanged(ctx context.Context) {
	if fn := e.onCatalogue.Load(); fn != nil && *fn != nil {
		(*fn)(ctx)
	}
}

// OtelReceiver is this node's sandbox telemetry receiver, or nil when
// CREWLET_SANDBOX_OTEL_RECEIVER_URL is unset. Handed to the API this process
// serves, so the route verifies with the receiver the engine mints against, not
// a second one built from a key nobody shares.
func (e *Engine) OtelReceiver() *sandbox.OtelReceiver { return e.sandboxOtel }

// Bridge is this node's MCP tool bridge, or nil, exposed like the receiver: the
// API mounts the route with this engine's own, so a session a run opened is the
// one the route resolves.
func (e *Engine) Bridge() *mcpbridge.Bridge { return e.bridge }

// BoxTokens is this node's coding-box git credential (what a box's run token is
// minted and verified by, over the fleet keyring under host.BoxKeyDomain), built
// at New on every node whatever its roles, so never nil. Exposed for the
// receiver's and bridge's reason: the API verifies a box's token with the
// tokens this engine mints and Gitlet admits by, never a second set, and where
// Gitlet is not built, since a box route on a node serving none of it tells a
// box why only once its token verifies (api.CodeSurface.VerifyBox).
func (e *Engine) BoxTokens() *host.BoxTokens { return e.boxTokens }

// CheckTokens is this node's check's-box credential, minted and verified over
// the fleet keyring under host.BoxKeyDomain, built at New on every node so
// never nil, exposed for [Engine.BoxTokens]' reason.
func (e *Engine) CheckTokens() *host.CheckTokens { return e.checkTokens }

// tokenMaterial is the Tier A keyring the per-run token issuers derive from,
// nil-safe for an engine built without one.
func tokenMaterial(boot *config.Bootstrap) runtoken.Material {
	if boot == nil {
		return runtoken.Material{}
	}
	return boot.Secrets.TokenMaterial()
}

// observe publishes an engine-side observability event, best effort: a failed
// publish is logged and swallowed, as failing real work to keep a feed row is
// the wrong trade in every use.
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
// every record whose wake is among evs ([notify.TriggerOf]): the position a
// first-party change feed stamped on it. The node's floors, not the turn's:
// every read this node routes carries them ([estate.Session]) to whichever
// holder answers, its own copy included; a floor a turn raises is one every
// seat then reads past, conservative rather than wrong. A token this build
// cannot read (a newer build's) is logged and skipped: the turn runs and reads
// as one woken by a wake no change feed derived, which carries none.
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
