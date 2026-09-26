package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/version"
)

// This node's STATE-LOG RUNTIME: which domains it runs, and the four objects
// each of them needs.
//
// # The four, and why each is separate
//
// A domain is a declaration. What makes it run is a LOG to append to, a
// CONSUMER to read it back, an APPLY LOOP that turns records into rows, and a
// PUBLISHER that decides what to append. They are four objects rather than one
// because two of them are the broker's and two are the store's, and every
// failure that matters is one of them being fine while another is not: a
// consumer that is delivering into an applier that has stopped, a publisher
// forming expectations from a checkpoint that stopped moving.
//
// # Why the consumer resumes from the ROWS
//
// The applier's checkpoint commits in the same transaction as the rows it
// covers, so it is the only durable statement of where this node is. The
// consumer's own acknowledgement floor is a second, weaker number. So the
// consumer is created at the checkpoint's own sequence — which is also the
// only way a node that has applied a million records avoids a million
// redeliveries on every restart.
//
// # And why seat admission waits on EVERY registered domain
//
// A seat whose mailbox attached before its node's tracker was established
// would answer "there is no such task" to its own tools — which is an answer
// it acts on, by filing a duplicate or abandoning work it was told to do. That
// gate is per DOMAIN and not per node: a domain whose health does not gate
// admission says so itself ([statelog.Domain.ReadinessInput]), which is how
// the vector domain's coverage number stays a number rather than an outage.

// domainHost is the broker as this node's state log needs it.
//
// DECLARED HERE, by the caller, and satisfied by the JetStream queue. It is
// the one place in the engine that names a queue backend, and it names it once
// and loudly: a state log needs a conditional append, a per-node durable
// consumer and a stream whose settings make an interior gap impossible, and
// none of those is in the EventQueue contract because nothing else needs them.
type domainHost interface {
	EnsureDomainStream(ctx context.Context, spec jetstream.DomainStream) error

	// Clustered is whether the broker underneath has PEERS, which is what
	// the provisioning budgets branch on — see [jsprovision.Clustered].
	Clustered() jsprovision.Clustered

	// StreamBudget is what the broker will actually let this node
	// reserve. A ceiling is a RESERVATION the broker refuses if it cannot
	// honour it, and the number it compares against is a limit of the
	// broker's own rather than the disk, so a ceiling derived from free
	// space alone is refused on machines that have the space.
	StreamBudget(ctx context.Context) (jetstream.StorageBudget, error)

	// DomainStreamCeiling is the ceiling a domain's stream already holds,
	// and whether it exists. What the logs hold counts as theirs when they
	// are sized, which is what sizes a restart as the first boot was.
	DomainStreamCeiling(ctx context.Context, stream string) (int64, bool, error)
	DomainLog(ctx context.Context, stream string) (*jetstream.DomainLog, error)
	DomainConsumer(ctx context.Context, stream, nodeID string, after uint64) (*jetstream.DomainConsumer, error)
}

// runningDomain is one domain this node runs, with everything it took.
type runningDomain struct {
	domain    statelog.Domain
	runner    *statelog.Runner
	publisher *statelog.Publisher
	log       *jetstream.DomainLog
	consumer  *jetstream.DomainConsumer

	// createdAt is the creation instant of the stream this domain's
	// committed checkpoint counts on — the stream every position this node
	// states for the domain is a number on. It is what the heartbeat
	// compares the live stream against to DETECT a recreated one (the
	// generation is the response), and what the positions register row
	// states beside those numbers. Read through [runningDomain.identity] and
	// moved only by [runningDomain.follow], under identityMu: a rejoin moves
	// it while the heartbeat compares the live stream against it.
	//
	// NOT ALWAYS THE STREAM THE APPLIER WAS HANDED. A node that boots over
	// a rebuilt log hands its applier the live stream, and the applier
	// stops on a checkpoint recorded under the deleted one — committing
	// nothing, so the checkpoint and every number read off it still count
	// on the deleted stream, and so does this ([checkpointStream]). Until
	// the domain follows the live stream, by a reanchor or an adoption,
	// that is the stream it states.
	identityMu sync.Mutex
	createdAt  time.Time

	// recreated is set when the position heartbeat finds the LIVE stream
	// is not the one this domain's checkpoint counts on
	// ([runningDomain.createdAt], compared in [runningDomain.observeLive]),
	// and cleared when the domain follows the live stream
	// ([runningDomain.follow]). It is atomic because the heartbeat writes
	// it while every health read takes it.
	recreated atomic.Bool

	// evicted is this domain's own eviction gate, taken from the write
	// fence so readiness reads the row the write path reads.
	//
	// THE SAME SOURCE ON BOTH PATHS, deliberately: an evicted node whose
	// writes are dropped by every peer while its reads answer normally is
	// the split D109 (b) calls the worst shape a node can be in, and two
	// sources for one fact is how it arises. Nil for a domain with no
	// eviction gate, which answers "not evicted" rather than refusing.
	evicted func(ctx context.Context) (bool, error)

	// progress is what a snapshot cannot see — whether the applied prefix
	// is moving, and how long an undecodable record has been held. See
	// [progress] for why it lives beside the runner rather than inside
	// [statelog.Health].
	progress progress

	// floor is how long this node has been unable to read the domain's
	// published trim floor, observed on the same heartbeat for the same
	// reason: "how long" is a property of a series of reads, and a failed
	// read is exactly the state in which a health read has nothing to say.
	floor floorWatch

	// reader is this domain's READ authority — the four levels, the
	// refusal ladder, the coverage probe and the barrier wait — and it is
	// what a domain's own reader answers through. Every running domain
	// has one; see [stateLog.readerFor] for the domain that has no read
	// index behind it.
	reader *statelog.Reader

	// applyStop ends this domain's apply loop and applyDone is closed once
	// the loop has returned; both nil while no loop runs. Written only
	// under the state log's applyMu — see [stateLog.launchAppliers].
	applyStop context.CancelFunc
	applyDone chan struct{}
}

// identity is the creation instant of the stream this domain's committed
// checkpoint counts on — see [runningDomain.createdAt].
func (r *runningDomain) identity() time.Time {
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	return r.createdAt
}

// observeLive compares the live stream's creation instant against the one this
// domain's checkpoint counts on and latches a recreation, reporting whether
// this call is the one that latched it.
//
// UNDER identityMu, so it cannot interleave with [runningDomain.follow]: a
// comparison against the instant a rejoin is replacing, latched after the
// rejoin cleared the latch, would leave a domain that follows the live stream
// refusing every read as though it did not — and nothing on the heartbeat
// clears a latch.
func (r *runningDomain) observeLive(live time.Time) bool {
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	if statelog.IdentityOf(r.createdAt, live, true) != statelog.StreamRecreated {
		return false
	}
	return !r.recreated.Swap(true)
}

// follow moves this domain onto the stream created at created: its applier's
// next run compares the checkpoint against that stream and records it, the
// heartbeat compares the live stream against it and the register states it,
// and a recreation latched against the stream this domain has left is
// cleared.
//
// FOR THE GAP BETWEEN TWO RUNS of the applier — see [statelog.Runner.Follow] —
// and only once the checkpoint counts on created: a reanchor has just
// committed it there, and an adoption installs one recorded under it
// ([stateLog.followAdopted]).
func (r *runningDomain) follow(created time.Time) {
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	r.createdAt = created
	r.runner.Follow(created)
	r.recreated.Store(false)
}

// checkpointStream is the stream a domain's committed checkpoint counts on, as
// the boot finds it: live is the broker's instant for the stream the applier is
// handed, and recorded and found the instant the checkpoint row names.
//
// THE CHECKPOINT'S OWN STREAM WHEN IT NAMES ANOTHER, because the applier
// handed the live stream stops on that checkpoint ([statelog.Runner]'s boot
// comparison) and commits nothing over it — so the numbers this node states
// for the domain are still positions on the stream the row names. Stated
// against the live stream instead, a node restarted after a rebuild tells
// every peer it has applied records off the live stream, and every peer's
// reanchor is refused naming it, as its own is naming theirs.
//
// THE LIVE STREAM OTHERWISE: a checkpoint recorded under it, and one recorded
// under no instant or not at all, which the applier resumes from and commits
// under the live stream from its first batch.
func checkpointStream(live, recorded time.Time, found bool) time.Time {
	if statelog.IdentityOf(recorded, live, found) == statelog.StreamRecreated {
		return recorded.UTC()
	}
	return live
}

// floorWatch is how long one domain's published trim floor has been
// unusable from this node, and which of the two ways: the `floor_unknown` and
// `generation_left` alarms' inputs.
//
// The floor is read LIVE on every health read and an unusable one refuses at
// once, so no single read can say how long the state has held — and the
// unreadable alarm is about duration, because one failed read during a
// coordination election is not a fault and failures past
// [statelog.FloorCacheStale] are. So the heartbeat observes a read each beat
// and this remembers when each run began, clearing it on the first read that
// does not continue it.
//
// TWO RUNS, BECAUSE THEY ARE TWO FAULTS with two remedies. An UNREADABLE floor
// is coordination not answering this node, which clears when it answers. A
// floor published at a generation this node has LEFT answered perfectly well:
// the fleet re-anchored the domain and this node's rows are keyed to the
// number space before it, which nothing clears but this node adopting the new
// one.
type floorWatch struct {
	mu     sync.Mutex
	lostAt time.Time
	leftAt time.Time
}

// floorRead is what one heartbeat's read of a domain's floor found.
type floorRead int

const (
	floorReadable floorRead = iota
	floorUnreadable
	floorLeft
)

// readOf classifies one heartbeat's read of a domain's floor at the generation
// this node is on: floorsErr is the register's own answer, and floors what it
// held when there was one.
func readOf(floors []coord.TrimFloor, floorsErr error, domain string, generation uint32) floorRead {
	if floorsErr != nil {
		return floorUnreadable
	}
	_, err := floorFor(floors, domain, generation)
	switch {
	case errors.Is(err, errGenerationLeft):
		return floorLeft
	case err != nil:
		return floorUnreadable
	}
	return floorReadable
}

// observe records one heartbeat's read of the floor.
func (w *floorWatch) observe(now time.Time, read floorRead) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if read != floorUnreadable {
		w.lostAt = time.Time{}
	} else if w.lostAt.IsZero() {
		w.lostAt = now
	}
	if read != floorLeft {
		w.leftAt = time.Time{}
	} else if w.leftAt.IsZero() {
		w.leftAt = now
	}
}

// unreadableFor is how long the floor has been unreadable as of now, and false
// when the last observation read it.
func (w *floorWatch) unreadableFor(now time.Time) (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return runLength(w.lostAt, now)
}

// leftFor is how long the floor has named a generation this node has left, as
// of now, and false when the last observation found it at this node's own.
func (w *floorWatch) leftFor(now time.Time) (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return runLength(w.leftAt, now)
}

// runLength is how long ago a run began, and false for a run not in progress.
func runLength(began, now time.Time) (time.Duration, bool) {
	if began.IsZero() {
		return 0, false
	}
	return max(now.Sub(began), 0), true
}

// stateLog is this node's whole state-log runtime.
type stateLog struct {
	domains map[string]*runningDomain

	// order is the register's own order, so every surface that walks the
	// domains renders them the same way. A map's iteration order would
	// make one screen's rows move between refreshes.
	order []string

	nodeID string
	db     *store.DB
	fleet  coord.Fleet

	// metrics is the process's one recorder, threaded down so the apply
	// loop's instruments are observed rather than merely declared.
	metrics *metrics.Recorder

	// skills is this build's tool-skill parser and nudge, threaded down
	// because the PAGES APPLIER is what notices a skill page arriving or
	// leaving: natively there is no page webhook and the change feed
	// deliberately drops those changes, so the apply is the only thing
	// that sees both halves. Nil answers "not a skill" and nudges nobody,
	// which is a build with no skill parser wired.
	skills      pages.SkillDetector
	nudgeSkills func()

	// ceilings is the byte ceiling each domain's stream is CREATED with,
	// sized from Tier A inside the broker's budget ([ceilingsFor]). It is
	// only ever applied at creation: a stream's configuration has one
	// writer and a booting node is not it, so re-applying it would let
	// restart order decide a shared limit and let a late node lower a
	// ceiling an emergency grant had just raised.
	ceilings map[string]domainCeiling

	// volume is the directory the ceilings were derived from, which a
	// refused reservation names when that volume is what bounds the
	// broker.
	volume string

	// snapshot is what this node's snapshot loop last concluded, carried
	// from that loop to the position heartbeat.
	//
	// TWO LOOPS, ONE ROW, on two cadences that cannot be merged: the
	// snapshot loop runs on the operator's staleness interval — a day, by
	// default — and the heartbeat on ten seconds, so what this node holds
	// has to be handed between them. An atomic pointer rather than a
	// mutex for [store]'s reason: the heartbeat reads it while the
	// snapshot loop is mid-copy, and a nil reads honestly as a node whose
	// loop has not concluded anything yet.
	snapshot atomic.Pointer[snapshotHeld]

	// run is the context the runtime's loops run under — the heartbeat,
	// the snapshot loop, the donor — and stop is what ends them. HELD
	// rather than re-derived, for the reason the native runtime states: a
	// goroutine started under the CALLER's context is one stop can never
	// end, and the wait then blocks for ever.
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup

	// THE APPLY LOOPS RUN UNDER CONTEXTS OF THEIR OWN, children of run,
	// because they are the one set of loops a running node ENDS AND
	// STARTS AGAIN: an adoption replaces the replicated file, which can
	// only happen while nothing holds a pinned connection on it, and the
	// pins are the appliers' — and a reanchor moves ONE domain's
	// checkpoint, which that domain's own loop would commit over. Each
	// domain's loop is stopped and joined through its own
	// [runningDomain.applyStop] and [runningDomain.applyDone], which
	// applyMu guards.
	applyMu sync.Mutex

	// rejoin is what the heartbeat calls when it finds this node below
	// the log's floor while running; the engine sets it, because the
	// join reaches the store bracket and the broker's own connection.
	// The bookkeeping beside it is single-flight with a widening retry:
	// a fleet with no donor is asked again, but not every ten seconds.
	rejoin      func(context.Context) error
	rejoinMu    sync.Mutex
	rejoining   bool
	rejoinAfter time.Time
	rejoinPause time.Duration

	// reanchoring marks a reanchor in progress, under rejoinMu beside
	// rejoining, because the two are the transitions that rewrite this
	// node's replicated estate with appliers halted, and AT MOST ONE RUNS:
	// an adoption replaces the file a reanchor is writing and relaunches
	// every applier — the one the reanchor halted among them — in the
	// middle of the transition, and a second reanchor halts and relaunches
	// the loop the first is holding down. See [stateLog.beginReanchor].
	reanchoring bool
}

// errTransitionRunning refuses a reanchor while this node is already
// rewriting its replicated estate.
var errTransitionRunning = errors.New("engine: this node is already " +
	"rewriting its replicated estate")

// beginReanchor claims the one estate transition this node may run, or
// refuses naming the one already running.
//
// A REFUSAL, carrying [statelog.ErrReanchorRefused] like every other: the
// operator's remedy is to wait for the transition that is running, never to
// treat the reanchor as failed.
func (s *stateLog) beginReanchor() error {
	s.rejoinMu.Lock()
	defer s.rejoinMu.Unlock()
	switch {
	case s.rejoining:
		return fmt.Errorf("%w: %w: it is adopting a peer's snapshot, which "+
			"replaces the estate a reanchor writes — run the reanchor once the "+
			"adoption has finished", statelog.ErrReanchorRefused, errTransitionRunning)
	case s.reanchoring:
		return fmt.Errorf("%w: %w: another reanchor is running on it",
			statelog.ErrReanchorRefused, errTransitionRunning)
	}
	s.reanchoring = true
	return nil
}

// endReanchor releases what [stateLog.beginReanchor] claimed.
func (s *stateLog) endReanchor() {
	s.rejoinMu.Lock()
	defer s.rejoinMu.Unlock()
	s.reanchoring = false
}

// RejoinRetryCeiling bounds how long a node below the floor waits between
// attempts to adopt a snapshot when the last attempt found no usable donor.
//
// FIVE MINUTES. The first retry is one heartbeat away and each one after
// doubles, so a fleet whose donor is a minute from taking its first snapshot
// is asked again inside that minute — and a fleet that genuinely has no donor
// is asked a dozen times an hour rather than three hundred and sixty, each
// ask being a five-second offer window the node spends refusing every read.
const RejoinRetryCeiling = 5 * time.Minute

// errNoDonor reports a runtime join that found nothing to adopt, which is a
// state to retry from rather than a failure: the node stays as it is, below
// the floor and refusing, until a peer can donate.
var errNoDonor = errors.New("engine: no peer could donate a usable snapshot")

// replicatedEstate is the replicated database AS THE FRAMEWORK MAY HOLD IT:
// resolved on every call through the node handle, never captured.
//
// An adoption closes the peer, renames the artefact over it and reopens it,
// so the peer is a different *store.DB afterwards. Every subsystem that took
// the handle at boot would go on answering from a file no longer at that name
// — which is why the framework takes this seam, and why the window in which
// there is no peer answers [store.ErrNoEstate] rather than a stale database.
type replicatedEstate struct{ node *store.DB }

func (r replicatedEstate) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	peer := r.node.Replicated()
	if peer == nil {
		return store.ErrNoEstate
	}
	return peer.Read(ctx, fn)
}

func (r replicatedEstate) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	peer := r.node.Replicated()
	if peer == nil {
		return store.ErrNoEstate
	}
	return peer.Tx(ctx, fn)
}

func (r replicatedEstate) Writer(ctx context.Context) (*store.Writer, error) {
	peer := r.node.Replicated()
	if peer == nil {
		return nil, store.ErrNoEstate
	}
	return peer.Writer(ctx)
}

// Caps answers the CURRENT estate's probe, and the zero value in the window
// where there is no estate: the caller is sizing a statement, not reading
// state, and [store.RowsPerInsert] reads a zero limit as one row per
// statement — the shape every applier had before the chunker was wired in.
func (r replicatedEstate) Caps() store.Capabilities {
	peer := r.node.Replicated()
	if peer == nil {
		return store.Capabilities{}
	}
	return peer.Caps()
}

// snapshotHeld is the artefact this node holds, and why it holds no current
// one.
//
// BOTH HALVES TOGETHER, because a node can have both: a skip does not delete
// what is already on disk, so a node that could not refresh yesterday's copy
// still donates it — and the register's readers need to know that it can
// (the trim's snapshot term) and that it has stopped refreshing (the operator
// asking why a join failed).
type snapshotHeld struct {
	// Manifest is the newest complete artefact on this node's disk, and
	// Have reports whether there is one at all.
	Manifest statelog.Manifest
	Have     bool

	// Skip is why this node holds no CURRENT artefact, empty when its
	// newest is within the operator's interval.
	Skip statelog.SkipReason
}

// startStateLog brings up every domain this node runs.
//
// PROVISION, RESUME, RUN — in that order, per domain, and the order is the
// contract. A consumer opened before the stream exists creates nothing to read
// from; a runner started before its consumer resumed from the rows reads from
// wherever the broker's default happened to put it.
func (e *Engine) startStateLog(ctx context.Context, boot *config.Bootstrap,
	nodeID string, epoch map[string]any) (*stateLog, error) {

	host, ok := e.backends.Queue.(domainHost)
	if !ok {
		return nil, fmt.Errorf("engine: this node's broker cannot host a state "+
			"log (%T) — the native tracker needs a conditional append and a "+
			"per-node durable consumer, and neither is in the event-queue "+
			"contract because nothing else needs them", e.backends.Queue)
	}
	if nodeID == "" {
		return nil, fmt.Errorf("engine: a state-log node has no id — it names " +
			"this node's own consumer, stamps every record it writes and is " +
			"what the eviction gate compares against")
	}

	// SIZED BEFORE ANYTHING IS STARTED, so the one failure here that is a
	// declaration rather than a broker (a registered domain Tier A has no
	// ceiling for) has nothing to unwind.
	ceilings, err := ceilingsFor(ctx, host, boot)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &stateLog{
		domains: map[string]*runningDomain{},
		nodeID:  nodeID, db: e.backends.Store, fleet: e.backends.Fleet,
		metrics: e.metrics,
		skills:  skillDetector{}, nudgeSkills: e.nudgeSkills,
		ceilings: ceilings, volume: streamVolume(boot),
		run: runCtx, stop: cancel,
	}
	// PROVISION EVERY LOG FIRST, and only then decide whether this node
	// can replay from where it is.
	//
	// The join below reads each stream's FIRST SURVIVING SEQUENCE, which
	// is a fact about a stream that exists. Interleaving provision with
	// the decision — the obvious per-domain loop — would ask the question
	// of the first domain before the second's stream had been created,
	// and a stream created a moment ago reports a first sequence of 1,
	// which reads as "nothing was trimmed" for a log the fleet has been
	// writing to for months.
	// ONE CEILING OVER THE WHOLE STATE-LOG BRING-UP, for
	// [jsprovision.SequenceBudget]'s reason: this is three replicated stream
	// creates AND three durable consumer creates, each of which would
	// otherwise discover a wedged cluster on its own per-create budget. The
	// queue's own sequence ceiling covers the engine's streams and not
	// these, so without it the state log added six more full budgets after
	// that ceiling had already been spent — and the package's claim to bound
	// a whole bring-up was not true of all of it.
	//
	// The consumer each start creates is a replicated object on the same
	// metadata group, and gets a ceiling of its own below — after the join,
	// which is a snapshot transfer rather than a create and must not spend
	// the creates' budget.
	provisionCtx, cancelProvision := context.WithTimeout(ctx, host.Clustered().SequenceBudget())
	defer cancelProvision()

	logs, err := s.provisionAll(provisionCtx, host)
	if err != nil {
		s.Stop()
		return nil, err
	}

	// ADOPT BEFORE ANY APPLIER RUNS, because a join REPLACES the
	// replicated database file — and it can only do that while nothing
	// holds a transaction open on it. An applier started first would be
	// mid-batch when the rename landed, writing rows into an inode with
	// no name and reporting a checkpoint nobody will ever read.
	if _, err := e.join(ctx, s, logs); err != nil {
		s.Stop()
		return nil, err
	}

	// A SECOND CEILING, over the consumer creates, and deliberately not the
	// same one: it is opened AFTER the join above, which transfers a
	// snapshot rather than creating metadata and can honestly take minutes.
	// Spanning both would let a large adoption eat the budget the creates
	// need.
	consumerCtx, cancelConsumers := context.WithTimeout(ctx, host.Clustered().SequenceBudget())
	defer cancelConsumers()

	for _, domain := range registeredDomains() {
		running, err := s.start(ctx, consumerCtx, host, domain, logs[domain.Name()], epoch)
		if err != nil {
			// EVERY DOMAIN OR NONE. A node running half its register
			// serves rows derived from one log while another's records
			// pile up unapplied, and nothing above it can tell that
			// from a node that is merely behind.
			s.Stop()
			return nil, err
		}
		s.domains[domain.Name()] = running
		s.order = append(s.order, domain.Name())
	}
	// THE STATE LOG'S OWN CONTEXT, never the boot call's: an applier is
	// joined by [stateLog.Stop], which ends that one.
	//nolint:contextcheck // s.run is [context.WithoutCancel] of the boot
	// context: an applier started under the CALLER's would stop the moment
	// start returned, and every read of that domain would go stale.
	s.launchAppliers(s.run)
	// A NODE THAT FALLS BELOW THE FLOOR WHILE RUNNING adopts the same way
	// it would at boot. The heartbeat is what notices, and this is what it
	// calls: the appliers are ended, the artefact installed, the appliers
	// started again over it. Until this existed the state was detected —
	// reads refused and the seats moved — and repaired only by a restart
	// an operator had to know to perform.
	s.rejoin = func(ctx context.Context) error { return e.rejoin(ctx, s) }
	// AFTER EVERY DOMAIN IS RUNNING. The heartbeat reports each domain's
	// position and the snapshot gate reads each one's health, so both need
	// the loops they describe to exist.
	s.startPositionHeartbeat()
	e.startSnapshots(ctx, boot, s)
	log.InfoContext(ctx, "statelog_started", "node", nodeID, "domains", s.order)
	return s, nil
}

// Stop ends every loop this node started and waits for them.
func (s *stateLog) Stop() {
	if s == nil {
		return
	}
	s.haltAppliers()
	s.stop()
	s.done.Wait()
}

// launchAppliers starts every domain's apply loop under a fresh context
// derived from base.
//
// Called once at boot and again after every adoption, over the same runners:
// every subsystem that holds one keeps holding it, and [statelog.Runner.Run]
// resumes from the checkpoint the file now keeps.
//
// THE BASE IS THE STATE LOG'S OWN LIFETIME ([stateLog.run]), never the
// caller's: an applier outlives the boot call that started it and the
// heartbeat tick that relaunched it after an adoption, and one derived from
// either would stop the moment that call returned. Passed rather than read
// off the struct so the call site says whose lifetime it is.
func (s *stateLog) launchAppliers(base context.Context) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	for _, name := range s.order {
		s.launchLocked(base, s.domains[name])
	}
}

// launchApplier starts one domain's apply loop, for the reanchor that halted
// it alone. A no-op while the loop runs.
func (s *stateLog) launchApplier(base context.Context, running *runningDomain) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.launchLocked(base, running)
}

// launchLocked starts one domain's loop under applyMu.
func (s *stateLog) launchLocked(base context.Context, running *runningDomain) {
	if running.applyStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(base)
	done := make(chan struct{})
	running.applyStop, running.applyDone = cancel, done
	// THE LATCH STARTS AGAIN WITH THE LOOP: after an adoption the rows under
	// it are a peer's, and after a reanchor they are keyed to a stream this
	// node has not yet drained.
	running.progress.restart()
	go func() {
		defer close(done)
		if err := running.runner.Run(ctx); err != nil && ctx.Err() == nil {
			// A STOPPED APPLIER IS NOT A CRASHED NODE. Its rows are
			// frozen and every read of them says so through the
			// coverage it reports, so what this costs is that the node
			// stops taking seats — which is what the readiness gate
			// below already does with it.
			log.ErrorContext(ctx, "statelog_applier_stopped",
				"domain", running.domain.Name(), "error", err.Error(),
				"detail", "this node stops claiming seats for that domain and "+
					"its rows are going stale; a build that can read what it "+
					"could not, or an operator's reanchor, is what resumes it")
		}
	}()
}

// haltAppliers ends every apply loop and waits for them, releasing the pinned
// connections an adoption needs closed. Idempotent, and a no-op before the
// first launch.
func (s *stateLog) haltAppliers() {
	s.applyMu.Lock()
	var halting []*runningDomain
	for _, name := range s.order {
		if running := s.domains[name]; running.applyStop != nil {
			halting = append(halting, running)
		}
	}
	stops := make([]context.CancelFunc, 0, len(halting))
	dones := make([]chan struct{}, 0, len(halting))
	for _, running := range halting {
		stops, dones = append(stops, running.applyStop), append(dones, running.applyDone)
		running.applyStop, running.applyDone = nil, nil
	}
	s.applyMu.Unlock()
	// ALL STOPPED BEFORE ANY IS WAITED FOR, so the loops wind down together
	// rather than one after another.
	for _, stop := range stops {
		stop()
	}
	for _, done := range dones {
		<-done
	}
}

// haltApplier ends one domain's apply loop and waits for it. A no-op when it
// is not running.
func (s *stateLog) haltApplier(running *runningDomain) {
	s.applyMu.Lock()
	stop, done := running.applyStop, running.applyDone
	running.applyStop, running.applyDone = nil, nil
	s.applyMu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-done
}

// registeredDomains is every domain this build runs, in a FIXED order.
//
// The list is here rather than in a registry each domain writes itself into,
// because the order is load-bearing for the operator surfaces and an
// init-order registration is exactly the thing nobody can read off the source.
func registeredDomains() []statelog.Domain {
	return []statelog.Domain{tracker.Domain{}, search.Domain{}, pages.Domain{}}
}

// provisionAll provisions every registered domain's stream, in the register's
// order, and opens each.
func (s *stateLog) provisionAll(ctx context.Context, host domainHost) (map[string]*jetstream.DomainLog, error) {
	logs := map[string]*jetstream.DomainLog{}
	for _, domain := range registeredDomains() {
		appendTo, err := s.provision(ctx, host, domain)
		if err != nil {
			return nil, err
		}
		logs[domain.Name()] = appendTo
	}
	return logs, nil
}

// provision creates one domain's stream if it is not there and opens it.
//
// SEPARATED FROM [stateLog.start] because the join between them needs every
// stream to exist before it reads any of their bounds — see the comment at
// the call.
func (s *stateLog) provision(ctx context.Context, host domainHost,
	domain statelog.Domain) (*jetstream.DomainLog, error) {

	spec := domain.Stream()
	ceiling, err := s.ceilingFor(domain)
	if err != nil {
		return nil, err
	}
	if err = host.EnsureDomainStream(ctx, jetstream.DomainStream{
		Name:          spec.Name,
		Subjects:      spec.Subjects,
		MaxBytes:      ceiling.Bytes,
		MaxPerSubject: spec.MaxPerSubject,
		MaxAge:        spec.MaxAge,
		Duplicates:    spec.Duplicates,
	}); err != nil {
		if errors.Is(err, jetstream.ErrInsufficientStorage) {
			return nil, s.storageRefused(ctx, host, domain, ceiling, err)
		}
		// THE CEILING AND ITS FIELD RIDE EVERY FAILURE, because a
		// clustered broker that cannot place the stream says
		// "insufficient storage" in prose, about members this node
		// cannot see, and names neither.
		return nil, fmt.Errorf("engine: provision %s's log at a %d-byte ceiling "+
			"(%s): %w", domain.Name(), ceiling.Bytes, ceiling.Field, err)
	}
	appendTo, err := host.DomainLog(ctx, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("engine: open %s's log: %w", domain.Name(), err)
	}
	return appendTo, nil
}

// start brings up one domain on the log [stateLog.provision] opened.
func (s *stateLog) start(ctx, provisionCtx context.Context, host domainHost, domain statelog.Domain,
	appendTo *jetstream.DomainLog, epoch map[string]any) (*runningDomain, error) {

	spec := domain.Stream()

	// THE BROKER SAYS WHICH STREAM THIS IS. Its creation instant is the
	// detector for a recreated stream, and the applier compares it against
	// the instant its checkpoint was committed under — so it has to be the
	// LIVE one. The cursor row's own value was passed here once, and a
	// value read out of the row is compared against itself: every recreated
	// stream went undetected, the manifest carried a zero instant on every
	// fresh node, and the reanchor verb asked an operator to confirm the
	// year one.
	stats, err := appendTo.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read %s's identity: %w — a node cannot "+
			"resume a domain on a stream it cannot identify, because a recreated "+
			"one is empty and would be applied from as though nothing were "+
			"missing", domain.Name(), err)
	}
	created := stats.CreatedAt.UTC()
	if created.IsZero() {
		return nil, fmt.Errorf("engine: the broker reports no creation instant "+
			"for %s, so this node cannot tell it from a recreated stream: %w",
			spec.Name, statelog.ErrStreamRecreated)
	}

	// THE ROWS SAY WHERE THIS NODE IS. The checkpoint commits with them,
	// so it is the only durable statement of it — and resuming a consumer
	// anywhere else is either a hole (at the head) or a million
	// redeliveries (at the beginning).
	at, recorded, found, err := statelog.CursorFor(ctx, s.db.Replicated(), spec.Name)
	if err != nil {
		return nil, err
	}
	// THE SEQUENCE'S CONTEXT, not the caller's: this is a replicated create
	// like the stream above it, and the three domains share one ceiling so
	// a wedged metadata group cannot spend a full per-create budget three
	// times over. Everything else here takes the ordinary boot context.
	consumer, err := host.DomainConsumer(provisionCtx, spec.Name, s.nodeID, at.Seq)
	if err != nil {
		return nil, fmt.Errorf("engine: open %s's consumer: %w", domain.Name(), err)
	}

	applier, err := s.applierFor(domain)
	if err != nil {
		return nil, err
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: domain, Applier: applier, Fetch: consumer,
		// THE REPLICATED HANDLE, not the node one. The applier PINS a
		// connection for the life of its loop, and the pins live on the
		// replicated estate's pool — that is where every applier writes,
		// and it is the pool [store.Options.PinnedWriters] grows. Handed
		// the node handle it refuses at its first round with "declared 0
		// pinned writers", which stops the loop before it applies a
		// single record: the domain's rows never move, and the only
		// symptom is a node that stays behind for ever.
		//
		// The seams beside it take the NODE handle and reach the peer
		// themselves ([tracker.NewGates] also reads this node's own
		// adoption row, which is deliberately not replicated), so the
		// asymmetry here is real rather than an oversight.
		DB: replicatedEstate{node: s.db}, Generation: at.Generation,
		StreamCreatedAt: created, Epoch: epoch, Metrics: s.metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: build %s's applier: %w", domain.Name(), err)
	}

	publisher, evicted, err := s.publisherFor(domain, appendTo, runner)
	if err != nil {
		return nil, err
	}

	running := &runningDomain{
		domain: domain, runner: runner, publisher: publisher,
		log: appendTo, consumer: consumer,
		createdAt: checkpointStream(created, recorded, found),
		evicted:   evicted,
	}
	// AFTER the struct exists, because the health closure the reader
	// holds reads through it — a reader built first would capture a
	// half-assembled domain and report its progress as never observed.
	if running.reader, err = s.readerFor(domain, appendTo, runner, running); err != nil {
		return nil, err
	}
	// THE LOOP IS NOT STARTED HERE. Every domain is built first and the
	// loops are launched together by [stateLog.launchAppliers], which is
	// also what an adoption calls to start them again.
	return running, nil
}

// publisherFor builds one domain's write authority.
//
// THE FENCE AND THE GATES ARE THE DOMAIN'S, and that is the difference the two
// domains here make visible: the tracker's fence reads its own eviction rows
// and its gates read a permanent deletion marker, while the vector domain
// declares both OPEN — a derived value the next duty tick recomputes has no
// history for a fence to protect, and what actually stops an evicted node
// writing there is the fleet singleton's lease.
// It also hands back the domain's EVICTION READER, because the fence it is
// built from is the one place that knows how to ask — and readiness must ask
// the same question the write path asks, from the same row. Two readers of
// one tombstone is how a node comes to refuse its writes and answer its reads.
func (s *stateLog) publisherFor(domain statelog.Domain, appendTo *jetstream.DomainLog,
	runner *statelog.Runner) (*statelog.Publisher, func(context.Context) (bool, error), error) {

	deps := statelog.Deps{
		Domain: domain, Log: appendTo, Waiter: runner, NodeID: s.nodeID,
		// READ FRESH ON EVERY PUBLISH rather than captured: a reanchor
		// moves the generation under a running process, and a publisher
		// stamping the old one would write records every applier reads
		// as safely stale.
		Generation: func() uint32 { return runner.Committed().Generation },
	}
	var evicted func(context.Context) (bool, error)
	switch domain.Name() {
	case tracker.Domain{}.Name():
		rows, err := tracker.NewRows(s.db)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: build %s's read seam: %w", domain.Name(), err)
		}
		fence := tracker.NewFence(s.db, s.nodeID)
		fence.Cursor = runner.Committed
		fence.Floor = s.trimFloor(domain.Name(), func() uint32 { return runner.Committed().Generation })
		deps.Rows, deps.Fence, deps.Gates = rows, fence, tracker.NewGates(s.db)
		evicted = fence.Evicted
	case search.Domain{}.Name():
		rows, err := search.NewRows(s.db)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: build %s's read seam: %w", domain.Name(), err)
		}
		// NO EVICTION READER, and that is the domain rather than an
		// omission: the vectors are DERIVED and compacted, so there is no
		// tombstone table to read and nothing an evicted node could serve
		// that a re-embed would not replace.
		deps.Rows, deps.Fence, deps.Gates = rows, search.NewFence(), search.NewGates()
	case pages.Domain{}.Name():
		rows, err := pages.NewRows(s.db)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: build %s's read seam: %w", domain.Name(), err)
		}
		fence := pages.NewFence(s.db, s.nodeID)
		fence.Cursor = runner.Committed
		fence.Floor = s.trimFloor(domain.Name(), func() uint32 { return runner.Committed().Generation })
		deps.Rows, deps.Fence, deps.Gates = rows, fence, pages.NewGates(s.db)
		evicted = fence.Evicted
	default:
		return nil, nil, fmt.Errorf("engine: domain %q is registered and has no "+
			"write authority, so nothing could ever append to its log",
			domain.Name())
	}
	publisher, err := statelog.NewPublisher(deps)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: build %s's write authority: %w", domain.Name(), err)
	}
	return publisher, evicted, nil
}

// readerFor builds a domain's READ authority: the four levels, the refusal
// ladder, the coverage probe and the quorum-committed barrier a linearizable
// read waits through.
//
// # Why the barrier encoder is a switch and not a method on Domain
//
// A barrier is the framework's append and the DOMAIN's record — the read
// index decides when one goes out and what its acknowledgement proves, and
// the domain decides what a record on its log looks like. A domain that has
// no barrier encoder gets no read index and therefore no `linearizable`,
// which is the correct answer for one whose reads make no freshness claim
// rather than a gap: the vectors are derived and compacted, so "as of a
// position" is not a question about them.
//
// A DOMAIN'S OWN READER ANSWERS THROUGH THIS rather than reading its rows
// straight out of the replicated estate and echoing the level it was asked for
// back in the answer. A reader that echoed the level would hand a seat tool
// asking for `session` whatever this node happens to hold, bound nothing with a
// dashboard's `max_lag_seconds`, and append no barrier for `linearizable`.
func (s *stateLog) readerFor(domain statelog.Domain, appendTo *jetstream.DomainLog,
	runner *statelog.Runner, running *runningDomain) (*statelog.Reader, error) {

	var encode func(statelog.Envelope) ([]byte, error)
	switch domain.Name() {
	case tracker.Domain{}.Name():
		encode = tracker.EncodeBarrier
	case pages.Domain{}.Name():
		encode = pages.EncodeBarrier
	}

	deps := statelog.ReaderDeps{
		Domain: domain,
		DB:     replicatedEstate{node: s.db},
		Waiter: runner,
		// READ FRESH ON EVERY READ, because every one of its terms can
		// change between two of them — and because a captured value
		// would freeze the very refusals this seam exists to deliver.
		//
		// UNDER THIS NODE'S OWN CONTEXT, which is the only one that can
		// be right here: [statelog.ReaderDeps.Health] takes none, and
		// the closure is called on every read for the life of the node
		// — so the boot context that built the reader would cancel the
		// moment boot finished and turn every later read into a refusal.
		// [stateLog.run] ends with the domain the health describes,
		// which is what stops a probe outliving its own apply loop.
		Health:  func() statelog.Health { return readerHealth(s.health(s.run, running)) },
		Drain:   runner.Drain,
		Metrics: s.metrics,
	}
	if encode != nil {
		index, err := statelog.NewReadIndex(domain, appendTo, encode,
			func() uint32 { return runner.Committed().Generation }, s.metrics)
		if err != nil {
			return nil, fmt.Errorf("engine: build %s's read index: %w", domain.Name(), err)
		}
		deps.Index = index
	}
	reader, err := statelog.NewReader(deps)
	if err != nil {
		return nil, fmt.Errorf("engine: build %s's read authority: %w", domain.Name(), err)
	}
	return reader, nil
}

// trimFloor is the published floor for one domain, as the write fence, the
// readiness gate and the join read it: the first sequence the trim has NOT
// licensed removing, at the generation the caller is on.
//
// # It is the trim's own decision, never a minimum this node is part of
//
// The trim publishes what it concluded on every tick, blocked or not, and
// that record is the floor: [coord.TrimFloor.TrimTo], the exclusive sequence
// it may remove up to, which is exactly the first sequence a node has to hold.
// A minimum over the fleet's published positions would include this node's
// own row, and a minimum that includes the reader can never exceed it — so the
// fence verifying an expectation of zero, the health arm refusing a node below
// the floor and the join's "what must an artefact cover" would each compare
// against a number that cannot fail them, and the floor theorem's second clause
// — F <= C verified within the call — would be verified against nothing.
//
// An unreadable register is UNKNOWN and refuses; an absent record is a trim
// that has never run and therefore licensed nothing.
//
// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is what the fence does with
// the error: this is the one check where failing open is a lost update rather
// than a duplicate.
func (s *stateLog) trimFloor(domain string, generation func() uint32) func(context.Context) (uint64, error) {
	return func(ctx context.Context) (uint64, error) {
		floors, err := s.fleet.Floors(ctx)
		if err != nil {
			return 0, fmt.Errorf("engine: read the fleet's published trim floors: %w", err)
		}
		return floorFor(floors, domain, generation())
	}
}

// floorFor is the floor's arithmetic over what the register holds, separated
// from the register so every branch is reachable in a table test.
//
//   - No record for the domain: the trim has never concluded anything about
//     this log, so nothing has been licensed for removal. Zero.
//   - A record at a LOWER generation: it names a dead number space, and in
//     this one the trim has concluded nothing yet. Zero — and the stream's
//     own first sequence, which every reader takes as a maximum beside this,
//     covers a purge that raced a reanchor. Reading it as unknown instead
//     would refuse every write at an expectation of zero for a whole trim
//     interval after every reanchor.
//   - A record at a HIGHER generation: this node is the one on the dead
//     number space. [errGenerationLeft], which refuses — and is its own
//     error rather than an unreadable register's, because its remedy is this
//     node adopting the new generation rather than coordination answering.
//   - Otherwise TrimTo, which is zero while the trim is blocked.
func floorFor(floors []coord.TrimFloor, domain string, generation uint32) (uint64, error) {
	for _, f := range floors {
		if f.Domain != domain {
			continue
		}
		switch {
		case f.Generation < generation:
			return 0, nil
		case f.Generation > generation:
			return 0, fmt.Errorf("%w: the published floor for %s is at generation "+
				"%d and this node is on %d — this node's positions name a sequence "+
				"space the fleet has left, so nothing it holds can be compared "+
				"against the floor; its next boot asks the fleet for a snapshot of "+
				"generation %d to adopt", errGenerationLeft, domain, f.Generation,
				generation, f.Generation)
		}
		return f.TrimTo, nil
	}
	return 0, nil
}

// errGenerationLeft reports a domain whose published floor is at a generation
// above the one this node's rows are on: the fleet re-anchored the domain and
// this node has not followed.
var errGenerationLeft = errors.New("engine: this node is on a generation the fleet has left")

// Domain answers one running domain by name, or nil.
func (s *stateLog) Domain(name string) *runningDomain {
	if s == nil {
		return nil
	}
	return s.domains[name]
}

// Established reports whether every domain whose health gates seat admission
// has one.
//
// PER DOMAIN, from the domain's own declaration. A compacted domain's gap is a
// coverage number rather than a fault, so shedding a company's seats for one
// would be the outage the number exists to avoid — and stating it on the
// domain is what keeps that a property of the domain rather than a list
// somewhere of the ones to skip.
func (s *stateLog) Established(ctx context.Context, strict bool) (bool, statelog.ReadRefusal) {
	if s == nil {
		return true, ""
	}
	for _, name := range s.order {
		running := s.domains[name]
		if !running.domain.ReadinessInput() {
			continue
		}
		health, err := s.health(ctx, running)
		if err != nil {
			// THE CODE EVERY READ ON THIS NODE REFUSES WITH, from the
			// same failure — see [readerHealth].
			return false, readerHealth(health, err).Refusal(time.Now())
		}
		if ok, refusal := health.Established(strict); !ok {
			return false, refusal
		}
	}
	return true, ""
}

// Healthy reports whether every domain whose health gates seat admission
// ([statelog.Domain.ReadinessInput]) permits this node to KEEP the seats it
// holds, and names the first that does not.
//
// # This is a different question from Established, and the difference is what
// # separates withholding work from giving it back
//
// [stateLog.Established] gates ADMISSION: a node mid-hydration keeps what it
// holds and claims nothing new, which is right, because its rows are merely
// incomplete and its seats' work would only wait. This one gates HOLDING, and
// the states it fires on are ones where the seats' work would be WRONG:
//
//   - the applier has STOPPED, so its rows are frozen at the record that
//     halted it and every later object is missing its consequences — and so
//     are they when it has retried one failure past
//     [statelog.ApplyRetryBudget], or its applied prefix has stood still past
//     [statelog.StallGrace] with records waiting;
//   - the node is EVICTED, so its peers drop every record it publishes and
//     its rows have already stopped being the fleet's;
//   - its log is NOT THE ONE ITS ROWS ARE KEYED TO: its checkpoint is past
//     the log's end, or the stream was rebuilt under it;
//   - the node is BELOW THE TRIM FLOOR, so records it never applied have been
//     deleted and its rows have a hole nothing will fill;
//   - it has held a record it CANNOT DECODE past [statelog.DeferralGrace],
//     which is D122: under the grace nothing changes, because that covers
//     every rolling upgrade; past it the honest reading is "this node cannot
//     run this company's records" rather than "this node is briefly behind";
//   - the fleet has RE-ANCHORED the domain past it: the published floor names
//     a generation this node's rows are not on, so every read on it refuses
//     while a peer on the current generation serves the same seats.
//
// THIS IS WHAT MOVES THE SEATS. [seat.Config.Serviceable] asks it on every
// sweep and a no gives back every seat the node holds, while the readiness
// gate only withholds claims — so the `deferred_old` and `apply_lag` alarms'
// word that a node's seats move is true because of this function, and a state
// it leaves out moves no seat.
//
// A DOMAIN WHOSE HEALTH CANNOT BE READ DOES NOT SHED. An unreachable broker is
// the outage during which a company most needs its seats to keep running, and
// tearing them down on an unread number is the failure mode `unknown` exists
// throughout this package to prevent. A floor at a generation this node has
// left is not one: coordination answered, and the answer is about this node
// alone, so its peers are serving and giving the seats to them moves them
// somewhere they work.
func (s *stateLog) Healthy(ctx context.Context) (bool, string) {
	if s == nil {
		return true, ""
	}
	now := time.Now()
	for _, name := range s.order {
		running := s.domains[name]
		if !running.domain.ReadinessInput() {
			continue
		}
		health, err := s.health(ctx, running)
		switch {
		case errors.Is(err, errGenerationLeft):
			return false, name
		case err != nil:
			continue
		}
		if !health.Healthy(now, running.progress.deferredSinceValue()) {
			return false, name
		}
	}
	return true, ""
}

// health assembles one domain's readiness from the four places it lives: this
// node's own checkpoint, its consumer's backlog, the stream's own bounds, and
// the fleet's published floor.
func (s *stateLog) health(ctx context.Context, running *runningDomain) (statelog.Health, error) {
	now := time.Now()
	at := running.runner.Committed()
	health := statelog.Health{Position: at, AppliedThrough: at.Seq}
	// THE APPLIER'S OWN STOP, FIRST. A halted applier's rows are frozen at
	// the record that stopped it, so every later record's consequences are
	// missing and no read over them can be certified — which is what
	// [statelog.Health.Refusal]'s `stalled` arm exists to say.
	if err := running.runner.Stopped(); applierHalted(err) {
		health.Err = err.Error()
	}
	// A FAULT PAST ITS BUDGET IS THE SAME REPORT. The applier is still
	// retrying — it never gives up on a failure that is not a stop — but
	// this node's rows have not moved for as long as it has been, and a
	// read served from them is old in a way the lag cannot show. Inside
	// the budget nothing is reported, which is what keeps a broker blip
	// from moving a company's seats.
	if msg, faulted := running.runner.Fault(now); faulted && health.Err == "" {
		health.Err = msg
	}
	health.Stalled = running.progress.stalled(now)
	// EVERY RETAINED RECORD, counted: [statelog.Health.Deferred] is how
	// many this node holds, and a flag would report one record and a
	// thousand as the same rolling upgrade.
	if deferral, held := running.runner.Deferred(); held > 0 {
		health.Deferred = held
		health.DeferredFrom = deferral.Position.Seq
		if deferral.Position.Seq > 0 {
			health.AppliedThrough = deferral.Position.Seq - 1
		}
	}
	// THE EVICTION ROW, from the write fence's own reader. An evicted node
	// must stop answering as well as stop writing: its peers drop every
	// record it publishes, so its rows stop advancing while its position
	// keeps being published, and a read served from them is served from a
	// copy the fleet has already abandoned.
	if running.evicted != nil {
		evicted, err := running.evicted(ctx)
		if err != nil {
			return health, err
		}
		health.Evicted = evicted
	}
	first, end, err := boundsOf(ctx, running.log)
	if err != nil {
		// NEITHER HALF OF THE INEQUALITY CAN BE EVALUATED, and both
		// failures are silent: a consumer above the stream's last
		// sequence waits for a record that never arrives, and one below
		// its first is clamped upward with no error and reports itself
		// caught up over a hole.
		return health, err
	}
	health.FirstSeq = &first
	// THE END ITSELF, beside the lag derived from it: the lag is clamped
	// at zero, so a checkpoint PAST the end reads as caught up through
	// it, and the end is what [statelog.Health.AheadOfLog] refuses on.
	health.LastSeq = &end
	// AND WHETHER THIS IS THE SAME STREAM AT ALL, as the heartbeat last
	// saw it — the one term here that is OBSERVED rather than derived,
	// because nothing this node holds can show a rebuilt log.
	health.StreamRecreated = running.recreated.Load()
	lag := uint64(0)
	if end > at.Seq {
		lag = end - at.Seq
	}
	health.Lag = &lag
	// THE LATCH, which this read feeds as well as reads: a read that finds
	// nothing past the checkpoint has seen this node drained. See
	// [progress] for why it is a latch and not this instant's lag.
	health.CaughtUp = running.progress.caughtUp(now, lag == 0)
	floor, err := s.trimFloor(running.domain.Name(),
		func() uint32 { return at.Generation })(ctx)
	if err != nil {
		return health, err
	}
	health.TrimFloor = &floor
	// THE THREE-VALUED FLOOR, stamped with the instant it was read. Both
	// halves are load-bearing and the field carries them together for the
	// reason its own doc gives: a state whose age nobody carries ages
	// silently into an assertion, and this one decides whether a node is
	// serving rows the fleet has already trimmed out from under it.
	//
	// FirstSeq may only RAISE it — it arrives on the same stream info from
	// a possibly-non-authoritative member, and a stale one is LOWER than
	// the truth, so trusting it downward is how a node below the real
	// floor keeps serving.
	health.Floor = statelog.Floor{State: statelog.FloorOK, ReadAt: now}
	// BELOW MEANS THE NEXT RECORD THIS NODE NEEDS IS GONE: the one at
	// checkpoint+1. A checkpoint one below the first surviving sequence
	// has applied everything that was ever removed — the same test
	// [statelog.Health.Established] and the join make, so the three
	// cannot disagree at the boundary.
	below := at.Seq+1 < floor
	if health.FirstSeq != nil && at.Seq+1 < *health.FirstSeq {
		below = true
	}
	if below {
		health.Floor.State = statelog.FloorBelow
	}
	return health, nil
}

// readerHealth is what a read is certified against: the health read's own
// answer, or — when that read failed — nothing but the failure, placed where
// [statelog.Health.Refusal] names it.
//
// AN UNREADABLE TERM REFUSES rather than serving. The health read reaches the
// broker and coordination, and a read certified against a health nobody could
// establish is certified against nothing. WHICH REFUSAL is the failure's, with
// the error as its detail:
//
//   - a broker that did not answer for the log's bounds is
//     `broker_unreachable`, which a caller comes back from;
//   - a floor published at a generation this node has left is
//     `generation_left`, which it does not — coordination answered, and
//     only this node adopting the fleet's generation clears it;
//   - anything else — the published floor unreadable, or this node's own
//     eviction rows unreadable — leaves the floor unestablished, and
//     `floor_unknown` is worth coming back for.
func readerHealth(h statelog.Health, err error) statelog.Health {
	switch {
	case err == nil:
		return h
	case errors.Is(err, errBoundsUnanswered):
		return statelog.Health{BrokerErr: err.Error()}
	case errors.Is(err, errGenerationLeft):
		return statelog.Health{
			Floor: statelog.Floor{State: statelog.FloorLeft, ReadAt: time.Now()},
			Err:   err.Error(),
		}
	}
	return statelog.Health{Err: err.Error()}
}

// errBoundsUnanswered reports a health read the broker did not answer about the
// log's bounds, which every freshness term after them is measured against.
var errBoundsUnanswered = errors.New("engine: the broker did not answer for the log's bounds")

// bounded is a domain log as a health read asks it for its bounds.
type bounded interface {
	Bounds(ctx context.Context) (first, last uint64, err error)
}

// boundsOf reads a log's first and last sequence, and marks a failure as the
// broker's ([errBoundsUnanswered]) so a read refuses naming the broker.
func boundsOf(ctx context.Context, log bounded) (first, last uint64, err error) {
	first, last, err = log.Bounds(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %w", errBoundsUnanswered, err)
	}
	return first, last, nil
}

// applierHalted reports whether the error a domain's applier recorded as its
// stop ([statelog.Runner.Stopped]) says its rows have stopped moving.
//
// ONLY A DECLARED STOP IS ONE, [statelog.ErrStopped], which is the runner's own
// contract: a stop is the runner saying this build cannot go on over these
// records, and every other failure is retried in place and reported as a fault
// instead. The stop this process asks for is not a declared one — a node
// shutting down, or ending its loops for an adoption, cancels their context,
// and the runner returns that cancellation from its run without recording it —
// so a node on its way out neither refuses the reads it is still serving nor
// sheds seats a drain is already handing back in order.
func applierHalted(err error) bool { return errors.Is(err, statelog.ErrStopped) }

// applierFor is the state machine each domain declares.
//
// A SWITCH RATHER THAN A METHOD on the domain, because an applier is not part
// of the declaration: the declaration is what a snapshot, a claim and a sweep
// read, and it must be answerable by a build that cannot construct the applier
// at all. What this costs is that a new domain fails HERE, at boot, naming
// itself — rather than being registered with no state machine and applying
// nothing.
func (s *stateLog) applierFor(domain statelog.Domain) (statelog.Applier, error) {
	switch domain.Name() {
	case tracker.Domain{}.Name():
		return tracker.NewApplier(s.nodeID), nil
	case search.Domain{}.Name():
		return search.NewApplier(), nil
	case pages.Domain{}.Name():
		// THE PARSER AND THE NUDGE COME FROM HERE, because the apply is
		// what notices a tool-skill page arriving or leaving and there is
		// no other delivery to hang the resync off. The nudge is safe to
		// take before the native runtime exists: it is a non-blocking
		// send that returns when there is nothing to send to.
		return pages.NewApplier(s.nodeID, s.skills, s.nudgeSkills), nil
	}
	return nil, fmt.Errorf("engine: domain %q is registered and has no applier, "+
		"so its records would be consumed and produce no rows on this node",
		domain.Name())
}

// Epoch is the per-epoch configuration every registered domain's applier
// reads, as a plain map.
//
// # Why an applier reads a MAP rather than the config
//
// An applier is a pure function of (rows, record, options), and the purity is
// the identity claim: two nodes applying one record must produce the same
// rows, so anything an applier could read from the world instead of from its
// arguments is a value the two can differ on. The framework fills this once
// per apply-loop generation and hands it in — which is also why the one table
// whose contents depend on it is classed as DIVERGING rather than compared.
//
// Keys are qualified by the domain that reads them, so a second domain adding
// one cannot collide with the first's.
func (c *Company) Epoch() map[string]any {
	if c == nil || c.Config == nil {
		return nil
	}
	epoch := map[string]any{}
	if native := c.Config.Tracker.Native; native != nil && native.InboxRetentionDays > 0 {
		epoch["tracker.native.inbox_retention_days"] = native.InboxRetentionDays
	}
	return epoch
}

// joinIfBehind adopts a peer's snapshot when this node cannot replay its way
// back, and does nothing at all when it can.
//
// # The question, and why it is asked here
//
// A node's checkpoint is a sequence on each domain's log. It can replay from
// there as long as the log still HOLDS that sequence — and it may not: the
// trim deletes records every node has applied, and a node that was down long
// enough, or that has never run at all, wakes up below the floor. There is
// nothing on the log for it to read, and no amount of waiting produces one.
//
// So the check is per domain: is this node's checkpoint at or above the first
// sequence the stream still holds? A node below it on ANY domain joins, and
// the join is wholesale — a snapshot names every registered domain or a
// recipient refuses it, because a domain the artefact does not name is one
// this node would believe it was caught up on.
//
// # Why a fresh node does not join
//
// A stream nobody has written to reports a first sequence of 0 and a node that
// has applied nothing has a checkpoint of 0, so a brand-new company's first
// node is at the floor rather than below it. That is the common case and it
// must not pay a fleet round trip: [statelog.OfferWindow] is five seconds, and
// a company that boots in five seconds of silence on every start is one whose
// operator learns to distrust the boot.
//
// # And why no offer is not a failure
//
// A single-node company has nobody to donate, and so does a fleet where every
// peer is as far behind as this node. Refusing to boot then would take a
// company that has merely lost history and make it a company that cannot
// start. The node comes up on what it has, its coverage says what it cannot
// account for, and the domains whose health gates seat admission keep it from
// claiming work it cannot answer for — which is the mechanism that already
// exists for exactly this state.
//
// # And why it is the same join a running node makes
//
// Falling below the floor is not a boot-time event: it is what happens to a
// node that was paused, partitioned or slow for longer than the log's replay
// window, and such a node is RUNNING when it finds out. So this is one
// function with two callers — the boot, before any applier exists, and
// [Engine.rejoin], which ends the appliers first and starts them again after
// — and it reports whether it adopted, because the runtime caller retries on a
// fleet that could not donate and the boot simply comes up.
func (e *Engine) join(ctx context.Context, s *stateLog,
	logs map[string]*jetstream.DomainLog) (adopted bool, err error) {

	conn, ok := e.backends.Queue.(interface{ Conn() *nats.Conn })
	if !ok || conn.Conn() == nil {
		// A JOIN NEEDS THE BROKER'S OWN CONNECTION — a snapshot is
		// megabytes over request/reply rather than anything the queue
		// contract carries. A backend with none is the memory twin, in
		// a test, with no peer to donate anyway.
		return false, nil
	}

	behind, want, err := s.replayable(ctx, logs)
	if err != nil {
		return false, err
	}
	if len(behind) == 0 {
		return false, nil
	}
	log.WarnContext(ctx, "statelog_below_the_floor",
		"node", s.nodeID, "domains", behind,
		"detail", "this node's checkpoint is below what the log still holds, so "+
			"it cannot replay the records it is missing; it will ask the fleet "+
			"for a snapshot")

	startedAt := time.Now().UTC()
	adopter, err := statelog.NewAdopter(statelog.AdoptDeps{
		Domains:  s.registered(),
		LivePath: e.backends.Store.ReplicatedPath(),
		NodeID:   s.nodeID,
		Conn:     conn.Conn(),
		Need: func(context.Context) (statelog.OfferRequest, error) {
			return want, nil
		},
		StillUsable: func(ctx context.Context, m statelog.Manifest) error {
			return s.stillUsable(ctx, logs, m)
		},
		Hold:   s.holdTail,
		Close:  func(context.Context) error { return e.backends.Store.CloseReplicated() },
		Reopen: e.backends.Store.ReopenReplicated,
		Record: func(ctx context.Context, donor string, m statelog.Manifest,
			phase statelog.AdoptionPhase) error {

			return statelog.RecordAdoption(ctx, e.backends.Store, startedAt,
				donor, m, phase)
		},
		Logger: log,
	})
	if err != nil {
		return false, err
	}
	manifest, err := adopter.Join(ctx)
	switch {
	case errors.Is(err, statelog.ErrNoOffer):
		// NOT A FAILURE — see the doc comment. The node comes up on what
		// it has and its coverage says what it cannot account for.
		log.WarnContext(ctx, "statelog_no_snapshot_offered",
			"node", s.nodeID, "domains", behind,
			"detail", "no peer could donate a usable snapshot, so this node "+
				"comes up on the history it has; reads report the coverage "+
				"they could not account for, the domains that gate seat "+
				"admission keep it from claiming work it cannot answer for, "+
				"and it asks again on a widening interval")
		return false, nil
	case err != nil:
		return false, fmt.Errorf("engine: this node is below the log's floor on %v "+
			"and the join failed: %w", behind, err)
	}
	log.InfoContext(ctx, "statelog_adopted",
		"node", s.nodeID, "donor", manifest.NodeID, "sha256", manifest.SHA256,
		"taken_at", manifest.TakenAt, "bytes", manifest.Bytes)
	return true, nil
}

// rejoin is the runtime adoption: end the appliers, join, start them again.
//
// # The order, and why each step is where it is
//
//  1. THE APPLIERS END FIRST and are joined, because each holds a pinned
//     connection on the file about to be replaced, and the store's bracket
//     closes a database only nothing has open. Every waiter on them is told
//     rather than released, so a read in flight refuses `behind` instead of
//     reading rows below its position.
//  2. THE JOIN is the boot's own: what it needs, who can donate, fetch,
//     verify, install. Nothing about it is different at runtime — that was
//     the point of making the framework resolve its estate per call.
//  3. THE CONSUMERS ARE RESET to the checkpoint the artefact keeps, because
//     the broker will not move a consumer's start and one left at the old
//     position would deliver every record in between to be dropped.
//  4. EACH DOMAIN FOLLOWS THE STREAM ITS ADOPTED CHECKPOINT NAMES, where that
//     is the live one ([stateLog.followAdopted]) — before the appliers start,
//     because a relaunched applier compares the checkpoint against the
//     stream it is told it runs on.
//  5. THE APPLIERS START AGAIN, whatever happened: a join that found no
//     donor leaves the node as it was, below the floor and refusing, and a
//     node with no appliers at all would be worse than that.
func (e *Engine) rejoin(ctx context.Context, s *stateLog) error {
	log.WarnContext(ctx, "statelog_rejoin_started", "node", s.nodeID,
		"detail", "this node is below the log's floor while running; its "+
			"appliers pause while it asks the fleet for a snapshot")
	s.haltAppliers()
	// ctx IS the state log's own run context here — [requestRejoin]
	// starts this under it — so the relaunched appliers get the lifetime
	// the boot launch gave them rather than a heartbeat tick's.
	defer s.launchAppliers(ctx)

	logs := make(map[string]*jetstream.DomainLog, len(s.domains))
	for name, running := range s.domains {
		logs[name] = running.log
	}
	adopted, err := e.join(ctx, s, logs)
	switch {
	case err != nil:
		return err
	case !adopted:
		return errNoDonor
	}
	for _, name := range s.order {
		running := s.domains[name]
		at, _, _, err := statelog.CursorFor(ctx, s.db.Replicated(), running.domain.Stream().Name)
		if err != nil {
			return fmt.Errorf("engine: read %s's adopted checkpoint: %w", name, err)
		}
		if err := running.consumer.Reset(ctx, at.Seq); err != nil {
			// CORRECTNESS IS THE CHECKPOINT'S and the applier resumes
			// from it regardless; what a consumer left behind costs
			// is the redeliveries between, which is worth a line.
			//
			// THAT HOLDS BECAUSE THE HANDLE REPAIRS ITSELF. A reset
			// deletes before it creates, so a create that fails leaves
			// the broker with no consumer at all — and the appliers
			// are relaunched below either way. [jetstream.DomainConsumer]
			// clears the handle on that path and rebuilds it on the
			// next fetch, at the position it held before; without that
			// this line would be logging the start of a domain that
			// never applies another record.
			log.WarnContext(ctx, "statelog_consumer_not_reset",
				"domain", name, "checkpoint", at.String(), "error", err.Error(),
				"detail", "the consumer is rebuilt at its previous position by "+
					"the next fetch, so this costs redeliveries rather than "+
					"correctness")
		}
	}
	if err := s.followAdopted(ctx); err != nil {
		return fmt.Errorf("engine: this node adopted a snapshot and could not "+
			"compare the stream it names against the live one, so a domain "+
			"whose adopted checkpoint names a stream other than the one this "+
			"node booted against stops rather than applying; restarting the "+
			"node reads both again: %w", err)
	}
	log.InfoContext(ctx, "statelog_rejoined", "node", s.nodeID)
	return nil
}

// followAdopted moves every domain onto the stream its adopted checkpoint
// names, where that is the live one.
//
// # Why a join has to say which stream it installed
//
// A join installs a peer's checkpoint, committed against the stream the peer
// was on — and after a log was deleted and rebuilt under this node, that is
// not the stream this node's appliers booted against. Left alone, the
// relaunched applier compares the adopted checkpoint against its boot instant
// and stops as though its log had been recreated, naming a re-anchor, until a
// restart reads the live instant again; and the heartbeat's latch, set when it
// named the rebuild, keeps every read refusing `wrong_stream`. So the live
// instant is read here as the boot reads it ([stateLog.start]), and a domain
// whose adopted checkpoint names that stream follows it.
//
// A CHECKPOINT NAMING ANY OTHER STREAM IS LEFT ALONE: then the log really is
// not the one this node's rows are keyed to, and the relaunched applier stops
// and says so.
func (s *stateLog) followAdopted(ctx context.Context) error {
	var failed []error
	for _, name := range s.order {
		running := s.domains[name]
		live, err := liveIdentity(ctx, running.log)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", name, err))
			continue
		}
		_, adopted, found, err := statelog.CursorFor(ctx, s.db.Replicated(),
			running.domain.Stream().Name)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if statelog.IdentityOf(adopted, live, found) == statelog.StreamSame {
			running.follow(live)
		}
	}
	return errors.Join(failed...)
}

// liveIdentity is a stream's creation instant as the broker reports it now,
// retried while the broker does not answer.
//
// RETRIED, because the one caller runs between an adoption and the relaunch it
// is for: a read that failed once would leave a domain's relaunched applier
// holding the instant it booted with, stopped with a re-anchor as its remedy
// when a restart is what would clear it. The retry takes the pacing and the
// budget the applier gives its own transient failures — [statelog.ApplyRetryBeat]
// doubling to [statelog.ApplyRetryCeiling], inside [statelog.ApplyRetryBudget]
// — because it is the same broker failing the same way.
func liveIdentity(ctx context.Context, stream interface {
	Stats(ctx context.Context) (jetstream.LogStats, error)
}) (time.Time, error) {
	deadline := time.Now().Add(statelog.ApplyRetryBudget)
	for attempt := 1; ; attempt++ {
		stats, err := stream.Stats(ctx)
		if err == nil {
			if stats.CreatedAt.IsZero() {
				return time.Time{}, fmt.Errorf("the broker reports no creation "+
					"instant for the stream: %w", statelog.ErrStreamRecreated)
			}
			return stats.CreatedAt.UTC(), nil
		}
		pause := backoff.Doubling(attempt, statelog.ApplyRetryBeat,
			statelog.ApplyRetryCeiling)
		if time.Now().Add(pause).After(deadline) {
			return time.Time{}, fmt.Errorf("read the stream's identity: %w", err)
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return time.Time{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// requestRejoin runs one adoption at a time, and after one that found no
// donor waits a widening interval before the next.
func (s *stateLog) requestRejoin(now time.Time) {
	s.rejoinMu.Lock()
	defer s.rejoinMu.Unlock()
	// A REANCHOR IN PROGRESS PASSES THIS BY rather than waiting: the
	// heartbeat that asked will ask again on its next beat, while the node
	// is still below the floor.
	if s.rejoin == nil || s.rejoining || s.reanchoring || now.Before(s.rejoinAfter) {
		return
	}
	s.rejoining = true
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		err := s.rejoin(s.run)
		s.rejoinMu.Lock()
		defer s.rejoinMu.Unlock()
		s.rejoining = false
		if err == nil {
			s.rejoinPause = 0
			return
		}
		if s.run.Err() != nil {
			return
		}
		if s.rejoinPause == 0 {
			s.rejoinPause = PositionHeartbeat
		} else {
			s.rejoinPause = min(s.rejoinPause*2, RejoinRetryCeiling)
		}
		s.rejoinAfter = time.Now().Add(s.rejoinPause)
		log.WarnContext(s.run, "statelog_rejoin_deferred",
			"node", s.nodeID, "retry_in", s.rejoinPause, "error", err.Error())
	}()
}

// replayable answers, for every registered domain, whether this node can reach
// the log from where its rows say it is — and what it would need if it cannot.
//
// NEED IS ONE BELOW THE FLOOR, per domain, which is the artefact's own
// acceptance test: a snapshot at that position leaves the node with the whole
// surviving log ahead of it and nothing missing in between. The generation
// travels beside it because a bare sequence from before a reanchor names a
// dead number space and would compare as if it were current.
func (s *stateLog) replayable(ctx context.Context, logs map[string]*jetstream.DomainLog) (
	behind []string, want statelog.OfferRequest, err error) {

	want = statelog.OfferRequest{
		Need:            map[string]uint64{},
		Generations:     map[string]uint32{},
		StreamCreatedAt: map[string]time.Time{},
	}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		appendTo, held := logs[name]
		if !held {
			return nil, statelog.OfferRequest{}, fmt.Errorf("engine: %s's log was not "+
				"provisioned before the join asked what it holds", name)
		}
		// ONE ROUND TRIP FOR EVERY TERM THE STREAM ANSWERS, which is
		// also the only way they describe one moment: the first
		// sequence decides whether this node is behind and the creation
		// instant decides whether the stream is even the one it was
		// behind ON, and read separately they can straddle a rebuild.
		stats, err := appendTo.Stats(ctx)
		if err != nil {
			return nil, statelog.OfferRequest{}, err
		}
		first := stats.FirstSeq
		at, _, found, err := statelog.CursorFor(ctx, s.db.Replicated(), domain.Stream().Name)
		if err != nil {
			return nil, statelog.OfferRequest{}, err
		}
		// A NODE ON A GENERATION BELOW THE FLEET'S ADOPTS, and a node
		// with no checkpoint is on no generation at all, which is below
		// every fleet that has ever re-anchored.
		//
		// A re-anchor keeps the re-anchoring node's rows and follows a new
		// stream from its head, so the records before it are not on the
		// log for anybody to replay: the only way onto the new number
		// space with its history is a peer's snapshot. A node that
		// replayed instead — which the behind test below would let it do,
		// since the new stream begins at sequence 1 — would stamp every
		// row at its old generation while its peers hold the same records
		// at the new one, report itself caught up over a database holding
		// only what was published since the re-anchor, and block the
		// fleet's trim, whose applied term reads a counted node at a lower
		// generation as unknown. And the floor comparison below, made at
		// its own generation against a floor published at the fleet's,
		// would fail its boot on the call that exists to send it to adopt.
		//
		// So the generation is the FLEET's whenever the fleet is ahead,
		// and a node behind it by generation is behind whatever its
		// sequence says. A node AHEAD of the fleet's published view keeps
		// its own: that is the node that re-anchored, before its peers or
		// the trim have published anything at the new generation.
		generation, err := s.fleetGeneration(ctx, name)
		if err != nil {
			return nil, statelog.OfferRequest{}, err
		}
		if found && at.Generation > generation {
			generation = at.Generation
		}
		floor, err := s.trimFloor(name, func() uint32 { return generation })(ctx)
		if err != nil {
			return nil, statelog.OfferRequest{}, err
		}
		want.Generations[name] = generation
		want.StreamCreatedAt[name] = stats.CreatedAt
		// AN ABSENT CHECKPOINT READS AS GENERATION ZERO, so this one
		// comparison is both halves of the rule above.
		if generation > at.Generation {
			behind = append(behind, name)
			continue
		}

		// WHAT AN ARTEFACT MUST COVER is one below the HIGHER of the
		// two: the stream's first surviving sequence is what is gone
		// already, and the published floor is what the trim has been
		// told it may delete and has not necessarily reached. A node
		// that accepted an artefact chosen from `first` alone would
		// install one the trim was about to pass.
		if usable := max(first, floor); usable > 0 {
			want.Need[name] = usable - 1
		}

		// WHETHER THIS NODE IS BEHIND is a different question, and it
		// is asked of `first` ALONE. The floor says what MAY be
		// deleted; only the stream says what IS. Testing against the
		// floor would make a single-node company adopt from itself:
		// the floor is a minimum over the fleet's published positions,
		// which on one node IS that node's own — so every moment its
		// published row ran ahead of its checkpoint would read as a
		// node below the floor, and the boot would spend the offer
		// window asking a fleet of one for a snapshot of itself.
		//
		// `first` is the first sequence the stream still HOLDS, and an
		// empty stream reports one past its last — so a fresh node at
		// checkpoint 0 against `first == 1` is a node with the whole
		// log ahead of it rather than one that has missed anything.
		if first > at.Seq+1 {
			behind = append(behind, name)
		}
	}
	return behind, want, nil
}

// fleetGeneration is the number space the FLEET is on for a domain, which a
// booting node compares its own rows' generation against.
//
// TWO SOURCES BECAUSE EITHER MAY BE THE ONLY ONE. Every live node publishes
// its own generation per domain in the position register, and the trim
// publishes the one it concluded at; a fleet whose peers are all restarting
// has the floor and no positions, and one that has never trimmed has positions
// and no floor. The MAXIMUM is taken because a re-anchor moves the fleet one
// node at a time: the highest anybody reports is the space the company is
// moving into, and an artefact from below it is one this node would have to
// adopt again.
//
// Zero is a real answer — a company that has never re-anchored — and it is
// what makes a genuinely new node in a genuinely new fleet replay from the
// beginning rather than ask for a snapshot nobody has.
func (s *stateLog) fleetGeneration(ctx context.Context, domain string) (uint32, error) {
	var newest uint32
	rows, err := s.fleet.Positions(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: read the fleet's published positions to "+
			"establish which generation %s is on: %w", domain, err)
	}
	for _, row := range rows {
		if at, named := row.Domains[domain]; named && at.Generation > newest {
			newest = at.Generation
		}
	}
	floors, err := s.fleet.Floors(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: read the fleet's published trim floors to "+
			"establish which generation %s is on: %w", domain, err)
	}
	for _, f := range floors {
		if f.Domain == domain && f.Generation > newest {
			newest = f.Generation
		}
	}
	return newest, nil
}

// stillUsable re-checks, after the transfer, that every position the artefact
// carries is still above the floor.
//
// THE HOLD IS THE MECHANISM AND THIS IS THE BELT. A fleet that trimmed past
// the artefact anyway — a peer on a build that does not honour holds, an
// operator forcing one — is one this node must not follow into a hole, and the
// only moment it can still refuse is before the install.
func (s *stateLog) stillUsable(ctx context.Context, logs map[string]*jetstream.DomainLog,
	m statelog.Manifest) error {

	for _, domain := range registeredDomains() {
		name := domain.Name()
		at, named := m.Domains[name]
		if !named {
			return fmt.Errorf("the artefact names no position for %s", name)
		}
		stats, err := logs[name].Stats(ctx)
		if err != nil {
			return err
		}
		first := stats.FirstSeq
		floor, err := s.trimFloor(name, func() uint32 { return at.Generation })(ctx)
		if err != nil {
			return err
		}
		// THE IDENTITY IS RE-CHECKED HERE TOO, and for the same reason
		// the floor is: the offer was judged against the stream as it
		// was when this node asked, and a rebuild during the transfer
		// leaves an artefact whose sequences name a history this node's
		// stream no longer has. A generation cannot see it — a rebuilt
		// stream comes back at generation 0 counting from 1.
		if statelog.IdentityOf(at.StreamCreatedAt, stats.CreatedAt, true) ==
			statelog.StreamRecreated {

			return fmt.Errorf("%s's artefact was taken against the stream "+
				"created at %s and this node's stream was created at %s, so "+
				"adopting it would install a history this log does not have",
				name, at.StreamCreatedAt.UTC().Format(time.RFC3339Nano),
				stats.CreatedAt.UTC().Format(time.RFC3339Nano))
		}
		// THE FLOOR IS INCLUDED HERE, unlike the behind test above, and
		// the asymmetry is the point: this asks whether the artefact is
		// still safe to install, and a position the trim has been told
		// it may delete is one that will be gone by the time this node
		// replays from it.
		if usable := max(first, floor); usable > 0 && at.Seq+1 < usable {
			return fmt.Errorf("%s's log now starts at %d and the artefact is "+
				"at %d, so adopting it would leave a hole", name, usable, at.Seq)
		}
	}
	return nil
}

// holdTail pins the replay tail for the whole transfer and returns the
// release.
//
// ONE HOLD FOR THE WHOLE JOIN, keyed on this node and this purpose, so a
// second attempt replaces the first rather than accumulating pins nobody
// releases. The release is called on every path out of the join — including
// the failures — because a hold nobody released stops the trim for its whole
// stale window, which is the one way a repair makes the fleet worse.
func (s *stateLog) holdTail(ctx context.Context, at map[string]uint64) (func(), error) {
	owner := s.nodeID + ":join"
	// KEYED BY STREAM, which is what [coord.TrimHold.Streams] holds: a
	// hold is a pin on a LOG, and the backup's own hold — taken from
	// `statelog_cursor`, which is keyed on the stream — has to land in the
	// same key space or the trim reads one of the two as pinning nothing.
	streams := make(map[string]coord.Position, len(at))
	for _, domain := range registeredDomains() {
		name := domain.Name()
		seq, held := at[name]
		if !held {
			continue
		}
		// THE GENERATION COMES FROM THIS NODE'S OWN CURSOR, because a
		// bare sequence names a number space: a hold stated in the
		// wrong generation pins a position on a log that no longer
		// exists, which the trim reads as no hold at all.
		cursor, _, _, err := statelog.CursorFor(ctx, s.db.Replicated(),
			domain.Stream().Name)
		if err != nil {
			return nil, err
		}
		streams[domain.Stream().Name] = coord.Position{
			Stream: domain.Stream().Name, Generation: cursor.Generation, Seq: seq,
		}
	}
	if err := s.fleet.PutHold(ctx, coord.TrimHold{
		Owner: owner, At: time.Now().UTC(), Streams: streams,
		Reason: "this node is below the log's floor and is fetching a peer's snapshot",
	}); err != nil {
		return nil, fmt.Errorf("engine: pin the replay tail for the join: %w", err)
	}
	return func() {
		// WithoutCancel: the release is undoing the hold, and the
		// failure it is most often undoing is the cancellation itself —
		// a release that inherited a dead context would leave the pin
		// standing for its whole stale window.
		if err := s.fleet.ReleaseHold(context.WithoutCancel(ctx), owner); err != nil {
			log.ErrorContext(ctx, "statelog_join_hold_not_released",
				"owner", owner, "error", err.Error(),
				"detail", "the trim is pinned at this node's join position until "+
					"the hold goes stale")
		}
	}, nil
}

// registered is every domain this node runs, as the framework's own surfaces
// want it: by name, with the health this node reports for each.
//
// The health is what a SNAPSHOT gates on — a node donates only what it can
// vouch for — and the artefact's acceptance is what a join gates on. Both walk
// the same map so a domain added to the register cannot appear in one and not
// the other.
func (s *stateLog) registered() map[string]statelog.Registered {
	out := map[string]statelog.Registered{}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		entry := statelog.Registered{Domain: domain}
		if running, held := s.domains[name]; held {
			// THIS NODE'S OWN CONTEXT, for [stateLog.readerFor]'s
			// reason: the snapshot loop and the adopter call this
			// closure on their own cadence, long after whoever built
			// the map returned, so there is no caller's context to
			// inherit — and [stateLog.run] is the one that ends when
			// the domain being vouched for does.
			entry.Health = func() statelog.Health {
				health, err := s.health(s.run, running)
				if err != nil {
					// AN UNREADABLE HEALTH IS NOT A HEALTHY ONE:
					// the zero value has CaughtUp false and no
					// first sequence, which every gate reads as
					// "cannot vouch for this".
					return statelog.Health{}
				}
				return health
			}
		}
		out[name] = entry
	}
	return out
}

// Status is what this node reports about each domain's apply loop.
//
// PER DOMAIN, and every registered one appears — including the domains whose
// health does NOT gate seat admission. The gate and the report answer
// different questions: the gate asks whether a seat may attach, and the vector
// domain deliberately answers "that is not mine to say"; the report asks how
// far along this node's copies are, which is a fact about every one of them.
// A domain missing from the count is one an operator cannot see falling
// behind.
func (s *stateLog) Status(ctx context.Context) []ReplicationStatus {
	if s == nil {
		return nil
	}
	out := make([]ReplicationStatus, 0, len(s.order))
	for _, name := range s.order {
		health, err := s.health(ctx, s.domains[name])
		out = append(out, replicationRow(name, health, err, time.Now()))
	}
	return out
}

// replicationRow is one domain's row, from the health read that describes it.
//
// NOT READY WHEREVER A READ ON THIS NODE WOULD REFUSE, and for the reason the
// read gives ([statelog.Health.Refusal]): a row reading ready while every read
// of the domain refuses tells an operator the one thing about this node that
// is false — and an evicted node, one below the trim floor and one whose log
// was rebuilt under it can each be caught up on what they hold. Past the
// refusals, the two ways a serving domain is still not ready — behind the
// log's head, or holding records back — say so.
func replicationRow(name string, health statelog.Health, err error, now time.Time) ReplicationStatus {
	row := ReplicationStatus{Name: name, Kind: "domain"}
	if err != nil {
		// UNREADABLE IS NOT READY. The alternative reads as a caught-up
		// loop on a node whose broker is unreachable, which is the one
		// state this count exists to surface.
		row.Detail = "this node cannot establish the domain's health: " + err.Error()
		return row
	}
	switch health.Refusal(now) {
	case statelog.RefuseEvicted:
		row.Detail = "evicted: this node has been removed from the fleet, so every " +
			"peer drops what it publishes; an operator readmits it"
	case statelog.RefuseBelowFloor:
		row.Detail = "below the trim floor: records this node never applied have " +
			"been trimmed, so no replay can supply them; it adopts a peer's snapshot"
	case statelog.RefuseWrongStream:
		if health.AheadOfLog() {
			row.Detail = fmt.Sprintf("this node's checkpoint %d is past the log's "+
				"end %d, so it names a stream that is not this one — a recreated "+
				"stream, or a broker restored from an older copy; `crewlet "+
				"retention reanchor` follows the new one from its head",
				health.Position.Seq, *health.LastSeq)
			break
		}
		// NOT "UNDER THIS NODE": the heartbeat names a rebuild the same
		// way whether it happened while this node ran or before it booted
		// over the rebuilt log ([checkpointStream]).
		row.Detail = "the log was deleted and rebuilt, so this node's rows are " +
			"keyed to a history the log does not have; `crewlet retention " +
			"reanchor` follows the new one from its head"
	case statelog.RefuseStalled:
		if health.Err != "" {
			// A HALTED OR FAULTED APPLIER IS NOT READY, whatever its
			// lag says: a loop halted on a recreated stream has a lag
			// of zero and applies nothing.
			row.Detail = "the applier is not applying: " + health.Err
			break
		}
		// STALLED BEFORE THE CAUGHT-UP ARMS, whatever the latch says: a
		// drain seen after the stall began keeps the latch set.
		row.Detail = stalledDetail(lagOf(health))
	case "":
		switch {
		case !health.CaughtUp:
			row.Detail = fmt.Sprintf("applying: %d record(s) behind the log's head",
				lagOf(health))
		case health.Deferred > 0:
			// CAUGHT UP AND STILL NOT READY. A retained record is one
			// this build cannot decode, or one held back behind such a
			// record because their scopes meet: the position moved past
			// both and the rows they would have written are not there,
			// so a loop reporting itself caught up would be claiming a
			// copy it does not have.
			row.Detail = fmt.Sprintf("caught up, retaining %d record(s) from "+
				"sequence %d rather than applying them — records this build "+
				"cannot decode, and records held back behind one; a build that "+
				"can decode them applies them all", health.Deferred,
				health.DeferredFrom)
		default:
			row.Ready = true
		}
	default:
		// EVERY OTHER REFUSAL, named by its own code: nothing a health
		// read that answered produces reaches here, and a code added to
		// the refusal ladder is not ready until this says why.
		row.Detail = "reads on this node refuse: " + string(health.Refusal(now))
	}
	return row
}

// stalledDetail is what a replication row says about a stalled domain with lag
// records past its checkpoint.
//
// THE STALL CLOCK IS THE HEARTBEAT'S ([progress.observe]), so for up to one
// beat after a stalled node drains, the clock still reads stalled while
// nothing is waiting — and a row counting "0 record(s) waiting" as the reason
// for a stall would contradict itself. It says what is true then instead.
func stalledDetail(lag uint64) string {
	if lag == 0 {
		return fmt.Sprintf("stalled: every heartbeat for more than %s found the "+
			"applied prefix standing still with records waiting; none are waiting "+
			"now, and a heartbeat that finds none waiting clears the stall",
			statelog.StallGrace)
	}
	return fmt.Sprintf("stalled: the applied prefix has not moved for more than "+
		"%s with %d record(s) waiting", statelog.StallGrace, lag)
}

// lagOf is a health's lag as a number, with the unmeasured case reported as
// zero rather than as a nil dereference.
//
// The fallback stays although no row built from a health read reaches it: a
// health read that could not measure the lag returns an error, which
// [replicationRow] reports before either arm that calls this — so what the
// fallback guards is a reordering there, which would otherwise panic the
// operator's status page rather than print a zero.
func lagOf(h statelog.Health) uint64 {
	if h.Lag == nil {
		return 0
	}
	return *h.Lag
}

// startSnapshots runs the two halves of the fleet's own recovery path: this
// node TAKES snapshots of its replicated estate on a timer, and SERVES them to
// a peer that asks.
//
// # Why both halves, and why neither is optional
//
// A node too far behind to replay adopts a peer's snapshot — that is
// [Engine.joinIfBehind], and it is the recipient. A recipient with no donor is
// a mechanism that can never complete: the join asks the fleet, nothing
// answers, and the node comes up on the history it has for ever. Half of this
// wired is worse than none, because the half that IS wired reports itself
// working.
//
// So they start together, and they are the same node's: every member both
// takes and serves, because there is no leader here to be the designated
// donor and a fleet whose only donor was down would have nothing to give.
//
// # Both are BEST EFFORT and neither gates the boot
//
// A node that cannot take a snapshot still serves its company perfectly — what
// it loses is the ability to help a peer that falls behind, which is a fleet
// property rather than this node's. A node that cannot serve one is the same
// fact from the other side. So a failure here is logged and the node comes up;
// the alternative is a company that will not start because a recovery path
// nobody is currently using could not be armed.
func (e *Engine) startSnapshots(ctx context.Context, boot *config.Bootstrap, s *stateLog) {
	if s == nil || boot == nil || e.backends == nil {
		return
	}
	// TWO SEAMS, deliberately: the join RIDES the engine's own connection
	// for one short request/reply exchange, and the donor gets its OWN
	// because it closes what it is given — a server of a subject for the
	// life of a node owns its connection, and handed the engine's it would
	// take the whole broker down when it stopped.
	broker, ok := e.backends.Queue.(interface {
		Conn() *nats.Conn
		DialOwned() (*nats.Conn, error)
	})
	if !ok || broker.Conn() == nil {
		// NO BROKER CONNECTION, which is the memory twin in a test. A
		// transfer is megabytes over request/reply rather than anything
		// the event-queue contract carries.
		return
	}
	dir := boot.Store.SnapshotDirFor()

	snapshotter, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains:       slices.Collect(maps.Values(s.registered())),
		DB:            e.backends.Store,
		Dir:           dir,
		NodeID:        s.nodeID,
		EngineVersion: version.String(),
		Counted:       e.countedNodes,
		Interval:      boot.Stream.TrackerRetention.SnapshotInterval(),
		Logger:        log,
	})
	if err != nil {
		log.ErrorContext(ctx, "statelog_snapshots_unavailable", "error", err.Error(),
			"detail", "this node takes no snapshots, so it can donate nothing to "+
				"a peer that falls below the log's floor; the fleet's other "+
				"members still can")
		return
	}
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: s.nodeID,
		Dial:   func(context.Context) (*nats.Conn, error) { return broker.DialOwned() },
		Newest: func() (statelog.Manifest, bool) { return newestSnapshot(dir) },
		Path: func(m statelog.Manifest) string {
			// THE NAME THE MANIFEST CARRIES. Deriving it here was one
			// of three independent derivations that had to agree, and
			// the derivation is what let a second take land on the
			// previous pair's name — see [statelog.Manifest.Artifact].
			return filepath.Join(dir, m.Artifact)
		},
		Logger: log,
	})
	if err != nil {
		log.ErrorContext(ctx, "statelog_donor_unavailable", "error", err.Error(),
			"detail", "this node answers no offer request, so a peer below the "+
				"log's floor cannot adopt from it")
		return
	}

	s.done.Add(2)
	go func() {
		defer s.done.Done()
		// THE DONOR FIRST and for the node's whole life: a peer asks at
		// ITS boot, which is any moment at all, so there is no window in
		// which not answering is acceptable.
		if err := donor.Serve(s.run); err != nil && s.run.Err() == nil {
			log.ErrorContext(s.run, "statelog_donor_stopped", "error", err.Error(),
				"detail", "a peer below the log's floor can no longer adopt from "+
					"this node; the fleet's other members still answer")
		}
	}()
	go func() {
		defer s.done.Done()
		e.snapshotLoop(s, snapshotter, dir,
			boot.Stream.TrackerRetention.SnapshotInterval())
	}()
}

// snapshotLoop takes one at boot and then on the interval — but a SKIP is not
// the interval's business.
//
// # Why a skipped tick retries soon and a taken one waits
//
// [statelog.Snapshotter.Take] has five preconditions, and every one of them is
// TRANSIENT AT BOOT: caught up on each domain, not too far behind, more than
// one node counted in the fleet, room on the volume, nothing deferred. A node
// coming up fails several of them for the first seconds of its life — the
// fleet is not counted until its peers have published a position, and it is
// not caught up until its appliers have drained.
//
// So a loop that only ever ticked on the configured interval would take its
// first snapshot a DAY after the node started, and a node restarted more often
// than that would take none at all. The fleet would then have no donor, and
// the only symptom is a peer that falls below the floor finding nothing to
// adopt — months later, in the one situation where it matters.
//
// A taken snapshot is different: the preconditions held, and the next one is a
// question about staleness rather than about readiness. That is what the
// operator's interval is for, and it is what waits.
//
// The retry is deliberately not tight. A skip is a state that clears on its
// own in seconds to minutes, the gate itself is a few reads, and a node that
// is genuinely unable to snapshot must not spend its life asking.
func (e *Engine) snapshotLoop(s *stateLog, snap *statelog.Snapshotter,
	dir string, interval time.Duration) {

	ctx := s.run
	// WHAT THIS NODE HOLDS is what decides how LOUD a skip is, and the
	// distinction is the one an operator actually has: a node holding an
	// artefact that skips a tick is a node whose fleet has a donor, and a
	// node holding none is a node the fleet cannot recover from.
	//
	// It is [snapshotHeld.Have] rather than "did THIS PROCESS take one",
	// which is the same question only until the first restart. A node that
	// snapshotted yesterday and was restarted skips for `recent` — its
	// artefact is inside the interval — and a process-scoped flag is false
	// at that moment, so every such restart warned that the node had never
	// taken a snapshot while its own register row was advertising the
	// artefact a peer could adopt. `Have` is read from the directory, so it
	// survives the restart the way the artefact does.
	//
	// THE REASON LAST REPORTED is the other half: a state that has not
	// changed is not news, and the loop retries every thirty seconds for as
	// long as it holds. A tick that changes nothing says nothing; the
	// register is still stamped, so the fleet screen and the trim see every
	// tick whether or not the log does.
	var reported statelog.SkipReason
	for {
		wait := interval
		m, err := snap.Take(ctx)
		// THE REGISTER IS TOLD ON EVERY TICK, taken or skipped, because
		// that row is the only place the rest of the fleet can see this
		// node's artefact at all: the trim's snapshot term counts
		// donors from it, and a node that stopped refreshing is
		// invisible until somebody needs to adopt.
		held := heldAfter(m, err, dir)
		s.snapshot.Store(&held)
		switch {
		case err == nil:
			reported = ""
		case !isSkip(err):
			// REPEATED DELIBERATELY, unlike a skip: this one RAN and
			// errored, the error text is what says why, and a disk that
			// went read-only is a fault an operator should keep seeing
			// rather than a posture they have already been told about.
			log.WarnContext(ctx, "statelog_snapshot_failed",
				"error", err.Error(), "holds_artefact", held.Have,
				"detail", "this node's newest artefact is older than the "+
					"interval, so a peer adopting from it replays further")
			reported = statelog.SkipFailed
			wait = min(snapshotSkipRetry, interval)
		default:
			reason, _ := statelog.Skipped(err)
			emitNoSnapshot(ctx, reportForSkip(reason, reported, held.Have))
			reported = remembered(reason, held.Have)
			wait = min(snapshotSkipRetry, interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// isSkip reports a tick that declined rather than failed.
func isSkip(err error) bool {
	_, skipped := statelog.Skipped(err)
	return skipped
}

// noSnapshotReport is what one skipped tick puts in the log: which event, and
// whether it is a warning. The zero value says nothing at all.
type noSnapshotReport struct {
	Event  string
	Reason statelog.SkipReason
	Warn   bool
	Detail string
}

// reportForSkip decides what a tick that took no snapshot should say, given
// why it skipped and what was last said.
//
// PURE OVER THE TWO REASONS, because the rule is the whole of the fix and
// exercising it through a running snapshot loop would mean standing a fleet up
// to assert a log level — which is how a rule ends up with no test at all.
//
// # Why sole_node is not a warning
//
// Every other reason here is a property of THIS NODE and clears by itself: it
// catches up, it drains its deferred records, somebody frees disk. Warning is
// right for those, because each one means a fleet that HAS peers currently has
// no donor for them, and each one ends without anybody doing anything.
//
// `sole_node` is a property of the FLEET'S SHAPE, and it is the documented
// default: one node is the whole supported topology for most companies, not a
// degraded two. Nothing about it clears on its own — it ends when an operator
// starts a second node — and there is nothing wrong while it holds, because a
// single node's recovery artefact is a BACKUP, which [internal/backup] takes
// and the trim's own backup term gates. So it is reported once, at info, as
// the statement of posture it is.
//
// Warned on every tick it was the default topology's steady state: a line
// every thirty seconds, for ever, on a healthy company — which is how an
// operator learns to filter out the subsystem that also reports the four
// conditions that are real. Its detail text made a claim that was false in
// exactly this case, too: that the preconditions clear "as its peers publish
// their positions", of a node that has no peers.
//
// The tick itself stays on the same retry, and cheaply: `sole_node` is the
// FIRST term [statelog.Snapshotter] gates on, so a solo node's whole attempt
// is one read of the positions register, and keeping it at that cadence is
// what arms the donor within a tick of a second node appearing.
func reportForSkip(reason, reported statelog.SkipReason, holds bool) noSnapshotReport {
	// A NODE THAT HOLDS AN ARTEFACT SAYS NOTHING, and `holds` is the whole
	// of that rule rather than a condition at the call site: written as a
	// branch in the loop, the predicate and the decision were two places
	// that had to agree about one question, and the pure half could be
	// called with a reason the loop would never have handed it.
	if holds || reason == reported {
		return noSnapshotReport{}
	}
	if reason == statelog.SkipSoleNode {
		return noSnapshotReport{
			Event:  "statelog_snapshot_sole_node",
			Reason: reason,
			Detail: "this node is the only member the fleet counts, so there " +
				"is nobody to donate a snapshot to and none is taken; a single " +
				"node's recovery artefact is a backup (retention.backup_owner) " +
				"rather than a donor snapshot, and this ends when a second node " +
				"joins",
		}
	}
	return noSnapshotReport{
		Event:  "statelog_no_snapshot_yet",
		Reason: reason,
		Warn:   true,
		Detail: "this node has never taken a snapshot, so it can donate " +
			"nothing to a peer that falls below the log's floor; the " +
			"preconditions clear on their own as this node catches up and its " +
			"peers publish their positions",
	}
}

// remembered is what the next tick compares against.
//
// A NODE HOLDING AN ARTEFACT REMEMBERS NOTHING, which is what keeps "say it
// once" true in both directions: should this node later hold none, that is a
// change worth saying however long ago it last said it.
func remembered(reason statelog.SkipReason, holds bool) statelog.SkipReason {
	if holds {
		return ""
	}
	return reason
}

// emitNoSnapshot writes what [reportForSkip] decided, and nothing for the tick
// it decided says nothing.
func emitNoSnapshot(ctx context.Context, say noSnapshotReport) {
	switch {
	case say.Event == "":
	case say.Warn:
		log.WarnContext(ctx, say.Event, "reason", string(say.Reason),
			"retry_in", snapshotSkipRetry, "detail", say.Detail)
	default:
		log.InfoContext(ctx, say.Event, "reason", string(say.Reason),
			"recheck_in", snapshotSkipRetry, "detail", say.Detail)
	}
}

// heldAfter is what a snapshot tick concluded this node holds.
//
// # Why it reads the directory rather than trusting the tick's own answer
//
// A SKIP DOES NOT DELETE WHAT IS ALREADY ON DISK, and one of them is a skip
// precisely BECAUSE something is: `recent` means the newest artefact is
// younger than the operator's interval, so a loop that reported "no snapshot"
// on that tick would erase from the register the very artefact that caused it.
// The others — `deferred`, `insufficient_space`, `lagging` — leave yesterday's
// copy in place, and a node holding one is still a donor the trim may count
// and a joining peer may adopt from. So the skip says why nothing was
// REFRESHED and the directory says what is HELD, and the row carries both.
func heldAfter(m statelog.Manifest, err error, dir string) snapshotHeld {
	if err == nil {
		return snapshotHeld{Manifest: m, Have: true}
	}
	// A HARD FAILURE IS PUBLISHED AS A REASON TOO. The error itself is
	// logged by the loop; what the fleet needs is that this node is not
	// refreshing, which an empty skip beside an old position reads as a
	// loop that simply has not come round yet.
	held := snapshotHeld{Skip: statelog.SkipFailed}
	if reason, skipped := statelog.Skipped(err); skipped {
		held.Skip = reason
	}
	if on, found := newestSnapshot(dir); found {
		held.Manifest, held.Have = on, true
		if held.Skip == statelog.SkipRecent {
			// THE ONE SKIP THAT IS NOT AN ANSWER TO "why can this
			// node not donate": it can, with an artefact inside the
			// interval the operator asked for. Publishing `recent`
			// here would put every healthy node in the fleet on the
			// operator's list of nodes that cannot donate.
			held.Skip = ""
		}
	}
	return held
}

// stampSnapshot writes what this node holds onto the row it is about to
// publish.
//
// A FUNCTION OVER VALUES, for the reason [statelog.TrimInputs] gives about the
// terms it feeds: the whole path from an artefact on one node's disk to a term
// in another node's trim runs through a live fleet, and the one part of it
// that can be exercised without one is this.
func stampSnapshot(row *coord.NodePositions, held *snapshotHeld) {
	if held == nil {
		// NOTHING CONCLUDED YET is not the same as nothing held, and
		// the difference matters for the first seconds of a node's
		// life: leaving both fields empty says "not known", where a
		// skip would say "known, and the answer is no".
		return
	}
	row.SnapshotSkip = string(held.Skip)
	if !held.Have {
		return
	}
	row.SnapshotBytes = held.Manifest.Bytes
	for name, at := range held.Manifest.Domains {
		// ONLY A DOMAIN THIS NODE STILL RUNS. An artefact taken by an
		// older build names domains this one does not register, and a
		// snapshot position under a domain with no committed position
		// beside it is a row the trim reads as a node holding that
		// log back at zero.
		pos, runs := row.Domains[name]
		if !runs {
			continue
		}
		pos.SnapshotSeq = at.Seq
		pos.SnapshotGeneration = at.Generation
		pos.SnapshotAt = held.Manifest.TakenAt.UTC()
		row.Domains[name] = pos
	}
}

// snapshotSkipRetry is how soon a skipped attempt is retried.
//
// THIRTY SECONDS, which is a boot's own settling time rather than a guess: a
// node's peers publish their positions on the heartbeat, its appliers drain
// what the log holds, and both are seconds on a healthy fleet. Shorter would
// spend a wedged node's life on a gate it cannot pass; much longer would leave
// a restarted node without an artefact for minutes, which is exactly the
// window a rolling upgrade lives in.
const snapshotSkipRetry = 30 * time.Second

// countedNodes is how many members the fleet counts, which is what decides
// whether there is anybody to donate to at all.
//
// FROM THE POSITIONS REGISTER rather than from the lease view, because that is
// the register the trim reads: a node counted there is one whose position
// holds the log back, and one holding the log back is exactly a node that
// might one day need a snapshot.
func (e *Engine) countedNodes(ctx context.Context) (int, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return 0, fmt.Errorf("engine: no coordination to count the fleet with")
	}
	rows, err := e.backends.Fleet.Positions(ctx)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// newestSnapshot reads the newest complete manifest in a directory.
//
// THE MANIFEST IS THE CLAIM, which is [internal/backup]'s rule and holds here
// for the same reason: the manifest is written last, so a directory entry
// without one is the debris of a run that did not finish rather than a
// snapshot somebody can adopt.
func newestSnapshot(dir string) (statelog.Manifest, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return statelog.Manifest{}, false
	}
	var newest statelog.Manifest
	var found bool
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		m, err := statelog.ReadManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		// AND THE BYTES BESIDE IT. A manifest whose artefact was rotated
		// away describes a transfer that would fail after the recipient
		// had already chosen it over every other offer.
		if _, err := os.Stat(filepath.Join(dir,
			m.Artifact)); err != nil {
			continue
		}
		if !found || m.TakenAt.After(newest.TakenAt) {
			newest, found = m, true
		}
	}
	return newest, found
}

// startPositionHeartbeat publishes this node's row in the fleet's position
// register, for as long as the node runs.
//
// # What reads it, and what an absent row costs
//
// The register is not telemetry. Four things read it and each one is wrong
// without this node's row:
//
//   - THE TRIM takes a minimum across every counted node's position to decide
//     what records the fleet has finished with. A node that publishes nothing
//     is a node the trim cannot see, so the log is trimmed past records this
//     node still needs — and the node discovers that by falling below the
//     floor and having to adopt a snapshot.
//   - THE WRITE FENCE reads the published floor to decide whether an
//     expectation of zero is safe on a quiet subject. With nobody publishing,
//     the floor is zero for ever, which is the conservative direction — but
//     it is conservative by accident rather than by design.
//   - THE SNAPSHOT GATE counts the register to decide whether there is
//     anybody to donate to. An empty register reads as a fleet of one, so no
//     node ever takes a snapshot and no node can ever donate one. That is not
//     a hypothetical: it is what a three-node fleet did before this existed.
//   - THE TRIM'S SNAPSHOT TERM reads the ARTEFACT each row names, and it is
//     the one reader whose failure is silent in the opposite direction: it
//     needs two counted nodes to hold a verified snapshot before it permits
//     removing anything, so a row that carries a position but no artefact
//     blocks the trim of every domain for the life of the deployment while
//     every other surface reports a healthy fleet.
//
// # Why it is a heartbeat rather than a write per commit
//
// The position moves on every applied record — thousands a minute on a busy
// company — and the register is a coordination bucket the whole fleet reads.
// Writing it per commit would put the fleet's slowest shared store on the
// applier's own loop. What every reader actually needs is a recent number
// rather than a current one: the trim is conservative in the safe direction
// with a stale row, and the gate's question changes on the scale of nodes
// joining. So it is a timer, and the row carries its own instant so a reader
// can say how stale it is.
func (s *stateLog) startPositionHeartbeat() {
	if s == nil || s.fleet == nil || len(s.order) == 0 {
		return
	}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		tick := time.NewTicker(PositionHeartbeat)
		defer tick.Stop()
		for {
			// AT ONCE, then on the tick: a node that published nothing
			// for its first interval is a node the trim cannot see for
			// that interval, and a node restarted more often than the
			// interval would never appear at all.
			s.publishPositions(s.run)
			select {
			case <-s.run.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// PositionHeartbeat is how often a node republishes its row.
//
// TEN SECONDS, taken from what the readers need rather than from what the
// writer can afford. The trim runs on a horizon of days and is conservative
// with a stale row; the snapshot gate's question — is there anybody else —
// changes when a node joins or leaves, which an operator expects to see
// reflected in seconds rather than minutes. Ten is well inside that and is a
// single small write per node per interval against a bucket with no age.
const PositionHeartbeat = 10 * time.Second

// publishPositions writes one row describing every domain this node runs.
//
// EVERY DOMAIN IN ONE ROW, which is the register's own shape: the trim asks
// "what has every node applied" about all of them at once, and a row per
// domain would be N writes saying one thing.
//
// AND THE SNAPSHOT THIS NODE HOLDS, stamped from what its own snapshot loop
// last concluded rather than read off the disk here — this runs every ten
// seconds and the artefact changes once a day, so re-reading a directory per
// beat would spend an I/O on an answer that did not move.
//
// A FAILURE IS LOGGED AND THE LOOP CONTINUES. The row is a recent number
// rather than a current one by construction, so one missed interval costs
// nothing a reader can notice — and a node that stopped its heartbeat because
// coordination blinked would then be invisible to the trim, which is the
// failure this whole loop exists to prevent.
func (s *stateLog) publishPositions(ctx context.Context) {
	row := coord.NodePositions{
		NodeID:        s.nodeID,
		At:            time.Now().UTC(),
		EngineVersion: version.String(),
		Domains:       make(map[string]coord.DomainPosition, len(s.order)),
	}
	// THE PUBLISHED FLOORS, read once for every domain the way a health
	// read reads them, so each domain's [floorWatch] observes the answer
	// its readers are given — an unreadable register and a floor at a
	// generation this node has left, which a health read refuses on alike,
	// kept apart because their remedies are not alike.
	floors, floorsErr := s.fleet.Floors(ctx)
	below := false
	for _, name := range s.order {
		running := s.domains[name]
		at := running.runner.Committed()
		// THE STREAM THESE NUMBERS COUNT ON, which is the one the
		// committed checkpoint was recorded under ([runningDomain.createdAt])
		// — not the live one, and not the one a node that booted over a
		// rebuilt log handed its applier. After a rebuild the numbers are
		// positions on the deleted stream until this node follows the live
		// one, and a peer deciding whether this node holds the live
		// stream's history — or whether it is the furthest along the
		// deleted one — has to be told which stream they count on.
		pos := coord.DomainPosition{
			Seq: at.Seq, Generation: at.Generation, AppliedThrough: at.Seq,
			StreamCreatedAt: running.identity().UTC(),
		}
		running.floor.observe(row.At, readOf(floors, floorsErr, name, at.Generation))
		// APPLIED_THROUGH IS LOWER WHEN SOMETHING IS DEFERRED, and the
		// two numbers are what tell a lagging node from a stalled one:
		// a position that advances while nothing is applied is exactly
		// what a retained record produces. Deferred is the count the
		// register documents — how many records this node retained.
		deferral, held := running.runner.Deferred()
		if held > 0 {
			pos.Deferred = int(held)
			if deferral.Position.Seq > 0 {
				pos.AppliedThrough = deferral.Position.Seq - 1
			}
		}
		// THE OBSERVATION RIDES THIS LOOP, because "has not moved" needs a
		// previous look and this is the one place that already takes one
		// every interval. See [progress]: it is what makes `Stalled`, the
		// deferral shed and the caught-up latch derivable at all, and the
		// series is measured here so no health read advances its clocks.
		//
		// THE LAG IS READ FROM THE BROKER'S OWN LAST SEQUENCE rather than
		// from the consumer's backlog: an idle node's applied prefix does
		// not move because there is nothing to move it, and a stall is
		// only a stall when there is work it owes. Nil when the broker did
		// not answer, which is neither work owed nor a drain.
		var lag *uint64
		if stats, err := running.log.Stats(ctx); err == nil {
			owed := uint64(0)
			if stats.LastSeq > at.Seq {
				owed = stats.LastSeq - at.Seq
			}
			lag = &owed
			// AND BELOW: the next record this node needs is gone from
			// the log. It cannot replay its way back, so it adopts —
			// the same repair the boot makes, made by a node that is
			// running.
			if stats.FirstSeq > at.Seq+1 {
				below = true
			}
			// AND WHETHER IT IS EVEN THE SAME LOG.
			//
			// This is the one place a running node compares the live
			// stream's creation instant against the one its checkpoint
			// counts on, which arrives in this same answer. The
			// sequence terms above cannot see a rebuild — a rebuilt
			// stream comes back at generation 0 counting from 1, so
			// once it has published past this node's checkpoint every
			// one of them reads as healthy while the node applies a
			// different history into rows keyed by the old one.
			if running.observeLive(stats.CreatedAt) {
				log.ErrorContext(ctx, "statelog_stream_recreated",
					"node", s.nodeID, "domain", name,
					"checkpoint_on", running.identity().UTC(),
					"live", stats.CreatedAt.UTC(),
					"detail", "this domain's log was deleted and rebuilt, so its "+
						"sequences name a history this node's rows are not keyed "+
						"to; reads and writes refuse until it follows the new "+
						"stream — by an operator's crewlet retention reanchor, "+
						"or by adopting a peer's snapshot taken on it once this "+
						"node is below the new stream's floor")
			}
		}
		running.progress.observe(row.At, pos.AppliedThrough, lag, held > 0)
		row.Domains[name] = pos
	}
	if below {
		s.requestRejoin(row.At)
	}
	stampSnapshot(&row, s.snapshot.Load())
	s.positionGauges(ctx, row)
	s.deferralGauges(row.At)
	if err := s.fleet.PutPositions(ctx, row); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.WarnContext(ctx, "statelog_position_not_published",
			"node", s.nodeID, "error", err.Error(),
			"detail", "the trim cannot see this node until it publishes again, "+
				"so it may delete records this node still needs; the next "+
				"heartbeat retries")
	}
}

// positionGauges publishes each domain's progress on the same beat the
// register row is written.
//
// ON THE HEARTBEAT rather than in the apply loop, because these are STATES
// rather than events: what a collector wants is "how far behind is this node
// now", and setting it per applied batch would make the answer a function of
// how busy the log is — a quiet log would leave the last burst's lag standing
// for as long as nothing was published, and a STOPPED loop, which applies no
// batch at all, would leave every one of them where its last batch put it.
func (s *stateLog) positionGauges(ctx context.Context, row coord.NodePositions) {
	if s.metrics == nil {
		return
	}
	for name, at := range row.Domains {
		attrs := metrics.Attrs{"domain": name}
		s.metrics.Set(metrics.StatelogAppliedThrough, float64(at.AppliedThrough), attrs)
		s.metrics.Set(metrics.StatelogDeferredCount, float64(at.Deferred), attrs)
		running := s.domains[name]
		if running == nil {
			continue
		}
		// THE CALLERS BLOCKED ON THE APPLIER, read from the waiters
		// themselves: a stopped loop ends no run, so a count sampled as
		// runs end is frozen at the moment the callers begin to pile up.
		s.metrics.Set(metrics.StatelogWaiters, float64(running.runner.Waiting()), attrs)
		s.metrics.Set(metrics.StatelogDrainRecordsPerSecond, running.runner.Drain(), attrs)
		// AND THE COMMIT RATE BESIDE IT, because the two are different
		// resources: records/s is progress and commits/s is the fsync
		// rate a device's write budget is spent by. A node whose
		// records/s is healthy and whose commits/s has doubled is doing
		// twice the disk work for the same progress, which neither figure
		// alone can say.
		s.metrics.Set(metrics.StatelogDrainCommitsPerSecond,
			running.runner.Commits(), attrs)
		health, err := s.health(ctx, running)
		if err != nil || health.Lag == nil {
			// UNREADABLE IS NOT ZERO, and a gauge has no third
			// value — so the lag gauges are left at whatever they
			// last held rather than being set to a number that
			// reads as caught up. The alarm reading takes the same
			// view from the same health.
			continue
		}
		s.metrics.Set(metrics.StatelogApplyLagSeq, float64(*health.Lag), attrs)
		age, err := s.applyAge(ctx, running, health, row.At)
		if err != nil {
			// THE SAME RULE for the age: left where it was rather
			// than set to a zero that reads as caught up.
			continue
		}
		s.metrics.Set(metrics.StatelogApplyLagSeconds, age.Seconds(), attrs)
	}
}

// applyAge is how old the oldest record this node has not applied is, as of
// now: now less the broker's own stored instant of the first record past the
// checkpoint that the log still holds, and zero when nothing is past it.
//
// # The same record on both kinds of log
//
// A strict log is contiguous above the trim, so the first record past the
// checkpoint is the one after it — or, when the trim removed that one, the
// first that survives, on a node that is below the floor and refuses on that
// account already. A compacted log keeps one record per subject, so the record
// after the checkpoint may have been superseded by a later one on its subject;
// superseded before this node reached it, it is gone from the log and is never
// applied here, so the first survivor is exactly the oldest record this node
// still owes.
// [jetstream.DomainLog.NextAt] asks the broker for that survivor in one read,
// which is what makes the figure exact on both.
//
// # An age rather than a projection
//
// A backlog over the measured drain is how long the backlog would take at the
// rate the loop last ran — and a loop that has stopped keeps the rate it last
// measured, so on a quiet log a stopped applier's projection stays as small as
// its backlog while the records it owes grow old. An age grows with the clock
// whether the loop runs or not, which is what [statelog.StallGrace] is written
// against: a stopped node's `apply_lag` fires once the first record it failed
// to apply is older than the grace.
//
// # Beside the health rather than inside it
//
// [stateLog.health] runs on every read at every level, and nothing on the read
// path consumes an age, so the probe — a broker round trip whenever this node
// is behind — is paid by its two consumers alone: the heartbeat's gauge and
// the alarm reading. Each hands in the health it already read, so the age is of
// that snapshot's own checkpoint and bounds.
//
// Measured against this node's clock, so a skew between it and the broker's
// is in the figure; a skew that would make the age negative reads as zero.
func (s *stateLog) applyAge(ctx context.Context, running *runningDomain,
	h statelog.Health, now time.Time) (time.Duration, error) {

	// A HEALTH WITH NO LAG is one whose broker read failed, which
	// [stateLog.health] reports as an error before either caller gets
	// here. Answered as an error rather than dereferenced, because the
	// other honest-looking answer — zero — is a node claiming to be caught
	// up on a read it could not make.
	if h.Lag == nil || h.LastSeq == nil {
		return 0, fmt.Errorf("engine: %s's lag was not read, so its age cannot be",
			running.domain.Name())
	}
	if *h.Lag == 0 {
		return 0, nil
	}
	_, _, stored, held, err := running.log.NextAt(ctx, h.Position.Seq+1)
	switch {
	case err != nil:
		return 0, fmt.Errorf("engine: read %s's oldest unapplied record: %w",
			running.domain.Name(), err)
	case !held:
		// NOTHING PAST THE CHECKPOINT SURVIVES to be read: the log lost
		// what the bounds named between the two reads. There is no age
		// to state, which is not an age of zero.
		return 0, fmt.Errorf("engine: %s holds no record past this node's "+
			"checkpoint %d, though its end was %d a moment ago",
			running.domain.Name(), h.Position.Seq, *h.LastSeq)
	}
	return max(now.Sub(stored), 0), nil
}

// deferralGauges publishes how long this node has held what it cannot decode.
//
// SECONDS RATHER THAN A COUNT, and beside the count rather than instead of it:
// a record held for an hour and one held for a second are the same count and
// completely different states, and it is the AGE that decides whether this
// node's seats have moved (D122).
//
// It rides the position heartbeat because that is where the deferral is
// observed — see [progress], which is the only thing that knows when the
// oldest one arrived, since a snapshot cannot say how long a state has held.
func (s *stateLog) deferralGauges(now time.Time) {
	if s.metrics == nil {
		return
	}
	for _, name := range s.order {
		running := s.domains[name]
		if running == nil {
			continue
		}
		held := running.progress.deferredSinceValue()
		age := 0.0
		if held.Held && !held.Since.IsZero() {
			age = now.Sub(held.Since).Seconds()
		}
		s.metrics.Set(metrics.StatelogDeferredOldestAgeSeconds, age,
			metrics.Attrs{"domain": name})
	}
}

// domainOf is the domain name whose log is this stream, or empty.
//
// A REVERSE LOOKUP over the register rather than a stored map, because the
// register is small and fixed and a second map is a second thing to keep in
// step with it.
func (s *stateLog) domainOf(stream string) string {
	if s == nil {
		return ""
	}
	for _, name := range s.order {
		if running := s.domains[name]; running != nil &&
			running.domain.Stream().Name == stream {
			return name
		}
	}
	return ""
}

// opsLedgers is every registered domain's operation ledger, keyed by domain.
//
// THE REGISTERED SET RATHER THAN A HAND-WRITTEN LIST, which is the whole point
// of building it here: a domain added to [statelogDomains] and forgotten in a
// sweep list is a table that grows for ever with nothing to notice, and that
// is exactly how these two came to be unswept.
func (s *stateLog) opsLedgers() map[string]maintenance.OpsLedger {
	if s == nil {
		return nil
	}
	out := make(map[string]maintenance.OpsLedger, len(s.domains))
	for name, running := range s.domains {
		if running != nil && running.runner != nil {
			out[name] = running.runner
		}
	}
	return out
}
