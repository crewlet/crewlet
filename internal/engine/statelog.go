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

	// GrowthBudget is how far a RUNNING log's ceiling may be raised, which
	// is a different rule and so a different number: a create is placed
	// and weighed against a member's own room, an update is checked by
	// whichever server leads the metadata group. A capacity operation
	// resizes a stream that exists, so this is the one it is held to.
	GrowthBudget(ctx context.Context) (jetstream.StorageBudget, error)

	// DomainStreamCeiling is the ceiling a domain's stream already holds,
	// and whether it exists. What the logs hold counts as theirs when they
	// are sized, which is what sizes a restart as the first boot was — and
	// it is what a log being created is sized beside, since a log that
	// exists reserves what it holds rather than what it would ask for now.
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

	// NO CREATION INSTANT IS HELD HERE. Which stream this domain's
	// positions are keyed to is the runner's
	// ([statelog.Runner.StreamCreatedAt]), and whether the live stream is
	// still that one is the runner's too ([statelog.Runner.StreamIdentity]),
	// because the write path refuses on it. A copy sampled here at boot
	// went stale the moment a stream was rebuilt under a running node: the
	// reanchor asked an operator to confirm it while every refusal named
	// the live one, and once the runner was re-keyed it described a stream
	// the checkpoint no longer named.

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

	// reader is this domain's READ authority — the four levels, the
	// refusal ladder, the coverage probe and the barrier wait — and it is
	// what a domain's own reader answers through.
	//
	// Nil for a domain that declares no barrier encoder, which is the
	// honest state for one whose reads make no freshness claim: the
	// vectors are DERIVED and compacted, so "as of a position" is not a
	// question that has an answer about them.
	reader *statelog.Reader
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
	// sized from Tier A inside the broker's budget ([ceilingsFor]); for a
	// stream that already exists it is the value that stream is reported
	// against. It is only ever applied at creation: a stream's
	// configuration has one writer and a booting node is not it, so
	// re-applying it would let restart order decide a shared limit and let
	// a late node lower a ceiling an emergency grant had just raised.
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

	// snapshotNudge wakes the snapshot loop out of its interval, for the
	// events that make the artefact this node holds one nobody can adopt:
	// a reanchor moved one of its domains to a new generation, an adoption
	// replaced its rows, or the restore of an estate a failed adoption left
	// closed opened the artefact that adoption installed. A joiner asks for
	// an artefact at the generation each domain is on, so until the loop
	// takes another the peers that reanchor left behind have no donor — for
	// up to the interval, a day by default. Buffered by one: a nudge that
	// finds one already pending adds nothing to it.
	snapshotNudge chan struct{}

	// run is the context the runtime's loops run under — the heartbeat,
	// the snapshot loop, the donor — and stop is what ends them. HELD
	// rather than re-derived, for the reason the native runtime states: a
	// goroutine started under the CALLER's context is one stop can never
	// end, and the wait then blocks for ever.
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup

	// THE APPLY LOOPS RUN UNDER A CONTEXT OF THEIR OWN, a child of run,
	// because they are the one set of loops a running node ENDS AND
	// STARTS AGAIN: an adoption replaces the replicated file, which can
	// only happen while nothing holds a pinned connection on it, and the
	// pins are the appliers'. applyStop ends them, applyDone joins them,
	// and launchAppliers starts them again over whatever file is there.
	//
	// AND EACH LOOP UNDER A CONTEXT OF ITS OWN BENEATH THAT, because a
	// reanchor ends and restarts ONE: it moves one domain's checkpoint,
	// which only that domain's loop writes, and the other domains have no
	// reason to pause. applying holds each running loop's own stop; the
	// set's context is the parent of every one, so ending the set ends
	// them all.
	applyMu   sync.Mutex
	applyCtx  context.Context
	applyStop context.CancelFunc
	applying  map[string]*applierRun
	applyDone sync.WaitGroup

	// recovering serialises the three operations that rewrite a domain's
	// checkpoint, or re-key its runner, under a running node: a runtime
	// adoption, which replaces the replicated file whole; the restore of an
	// estate a failed adoption left closed, which reopens it and re-keys
	// every runner to it; and a reanchor, which moves one domain's
	// checkpoint to a stream it adopts. Any one interleaved with another
	// writes into a file the other is replacing, re-keys a runner to a
	// checkpoint the other has just moved, or relaunches a loop the other
	// has just halted.
	recovering sync.Mutex

	// rejoin is what the heartbeat calls when it finds this node below
	// the log's floor while running; the engine sets it, because the
	// join reaches the store bracket and the broker's own connection.
	// The bookkeeping beside it is single-flight with a widening retry:
	// a fleet that could not donate is asked again, but not every ten
	// seconds — while a lost estate is reopened on every beat, which
	// asks nobody. See [stateLog.requestRejoin].
	rejoin      func(context.Context) error
	rejoinMu    sync.Mutex
	rejoining   bool
	rejoinAfter time.Time
	rejoinPause time.Duration
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
		snapshotNudge: make(chan struct{}, 1),
	}
	// PROVISION EVERY LOG FIRST, and only then decide whether this node
	// can replay from where it is.
	//
	// The join below reads each stream's FIRST SURVIVING SEQUENCE, which
	// is a fact about a stream that exists. Interleaving provision with
	// the decision — the obvious per-domain loop — would ask the question
	// of the first domain before the second's stream had been created,
	// and a stream created a moment ago reports a first sequence of 0,
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

	// AN ADOPTION A BUILD FROM BEFORE THE LEDGER TRAVELLED RECORDED is one
	// whose ledger was scrubbed, on a file whose watermark does not say so
	// — carried into that watermark here, once, before the join below can
	// replace the file and before anything publishes. See
	// [statelog.FoldLegacyAdoptions].
	if err := statelog.FoldLegacyAdoptions(ctx, e.backends.Store, registeredDomains()); err != nil {
		s.Stop()
		return nil, err
	}

	// ADOPT BEFORE ANY APPLIER RUNS, because a join REPLACES the
	// replicated database file — and it can only do that while nothing
	// holds a transaction open on it. An applier started first would be
	// mid-batch when the rename landed, writing rows into an inode with
	// no name and reporting a checkpoint nobody will ever read. And no
	// runner exists yet to re-key: each is built after this, against the
	// live instant, over whatever file the join left.
	if _, _, err := e.join(ctx, s, logs); err != nil {
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
		// A DOMAIN THE TRIM COUNTS NODES ON MUST SAY WHO IS EVICTED ON IT.
		// Without an answer the trim can never stop counting an evicted
		// node on that log, and the log grows behind a machine that is not
		// coming back until its ceiling refuses writes — silently, since a
		// log with nobody evicted and one that cannot say look the same.
		if _, lists := domain.(evictionLister); domain.ClaimsIdentity() && !lists {
			s.Stop()
			return nil, fmt.Errorf("engine: domain %q claims identity and cannot "+
				"list the evictions on its own log, so the trim could never stop "+
				"counting an evicted node there", domain.Name())
		}
		// AND MUST LET ITS LOG BE ASKED DIRECTLY. A node a peer re-anchored
		// past never applies an eviction written after its applier
		// stopped, and the eviction of that peer is what releases it —
		// see [statelog.EvictedOnLog] and [stateLog.evictedOn].
		if _, probes := domain.(statelog.EvictionProbe); domain.ClaimsIdentity() && !probes {
			s.Stop()
			return nil, fmt.Errorf("engine: domain %q claims identity and cannot "+
				"say who is evicted on its log without applying it, so a node a "+
				"decommissioned peer re-anchored past could never see that "+
				"peer's eviction", domain.Name())
		}
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
//
// THE APPLIERS ARE JOINED LAST, after everything in [stateLog.done], because a
// runtime rejoin is in that group and RELAUNCHES them on its way out —
// [Engine.rejoin] starts them again whatever the join concluded, a cancelled
// one included. Halted FIRST, which is the obvious order, a Stop that landed
// mid-rejoin found them already halted by the rejoin, cancelled the run,
// waited for the rejoin, and returned while the set the rejoin had just
// relaunched was still running against a store its caller closes next. Once
// the group has drained nothing is left that could launch another, so a halt
// after it is the last word. Cancelling the run first ends the appliers too —
// their context is a child of it — and the halt is what waits for them.
func (s *stateLog) Stop() {
	if s == nil {
		return
	}
	s.stop()
	s.done.Wait()
	s.haltAppliers()
}

// applierRun is one domain's apply loop: what ends it, and what reports it
// has ended.
type applierRun struct {
	stop context.CancelFunc
	done chan struct{}
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
	if s.applyStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(base)
	s.applyCtx, s.applyStop = ctx, cancel
	s.applying = make(map[string]*applierRun, len(s.order))
	for _, name := range s.order {
		s.startApplierLocked(ctx, name)
	}
}

// startApplierLocked starts one domain's loop under set, which is the set's
// own context. The caller holds applyMu and has checked the set is launched.
func (s *stateLog) startApplierLocked(set context.Context, name string) {
	running := s.domains[name]
	ctx, cancel := context.WithCancel(set)
	run := &applierRun{stop: cancel, done: make(chan struct{})}
	s.applying[name] = run
	s.applyDone.Add(1)
	go func() {
		defer s.applyDone.Done()
		defer close(run.done)
		defer cancel()
		// A STOPPED APPLIER IS NOT A CRASHED NODE. Its rows are
		// frozen and every read of them says so through the
		// coverage it reports, so what this costs is that the
		// node stops taking seats — which is what the readiness
		// gate below already does with it, reading the stop off
		// [statelog.Runner.Stopped] rather than this error.
		//
		// NO LINE OF ITS OWN: the runner writes
		// `statelog_applier_stopped` as it stops, naming the
		// stream, the position it froze at and what resumes it. A
		// second line here made one stop read as two, and its
		// `component=engine` is not the component the replication
		// guide sends an operator to.
		_ = running.runner.Run(ctx)
	}()
}

// haltApplier ends ONE domain's loop and waits for it, reporting whether it
// was still running — false for a loop that had already stopped on its own, or
// a set that is not launched.
//
// It exists for a reanchor, which rewrites one domain's checkpoint: that
// checkpoint's only other writer is this loop, and a batch it committed after
// the reanchor's transaction would write the old generation and the old
// stream's instant straight back over it.
func (s *stateLog) haltApplier(name string) bool {
	s.applyMu.Lock()
	run := s.applying[name]
	delete(s.applying, name)
	s.applyMu.Unlock()
	if run == nil {
		return false
	}
	running := true
	select {
	case <-run.done:
		running = false
	default:
	}
	run.stop()
	<-run.done
	return running
}

// resumeApplier starts ONE domain's loop again after [stateLog.haltApplier],
// with its consumer moved back to the checkpoint the rows keep first.
//
// THE CONSUMER IS RESET for the reason a rejoin resets every one: a loop ended
// in the middle of a fetch leaves its pull request pending on the broker, which
// hands the next record to a reader that is gone and redelivers it only once
// the acknowledgement window has passed — so on a strict log the resumed loop
// sat waiting thirty seconds for a record the broker believed delivered. A
// reset that fails costs exactly that and no more, since the loop resumes from
// the checkpoint whatever the consumer says, and it is said.
func (s *stateLog) resumeApplier(ctx context.Context, name string) {
	running := s.domains[name]
	at, _, _, err := statelog.CursorFor(ctx, s.db.Replicated(), running.domain.Stream().Name)
	if err == nil {
		err = running.consumer.Reset(ctx, at.Seq)
	}
	if err != nil {
		log.WarnContext(ctx, "statelog_consumer_not_reset",
			"domain", name, "checkpoint", at.String(), "error", err.Error(),
			"detail", "the loop resumes from its checkpoint regardless; a record "+
				"handed to the fetch that was ended arrives once its "+
				"acknowledgement window passes, so this costs a delay rather "+
				"than correctness")
	}
	s.launchApplier(name)
}

// launchApplier starts ONE domain's loop again under the set's context. A
// no-op while the set is halted — the launch that ends that halt starts every
// domain — and while the domain already has a loop running.
func (s *stateLog) launchApplier(name string) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if s.applyStop == nil {
		return
	}
	if run := s.applying[name]; run != nil {
		select {
		case <-run.done:
		default:
			return
		}
	}
	//nolint:contextcheck // the SET's context, never a caller's: a loop
	// outlives the reanchor or the resume that restarted it, and one started
	// under that caller's context would stop the moment it returned — and
	// ending the set is what must end it, as it ends every other domain's.
	s.startApplierLocked(s.applyCtx, name)
}

// haltAppliers ends every apply loop and waits for them, releasing the pinned
// connections an adoption needs closed. Idempotent, and a no-op before the
// first launch.
func (s *stateLog) haltAppliers() {
	s.applyMu.Lock()
	stop := s.applyStop
	s.applyCtx, s.applyStop, s.applying = nil, nil, nil
	s.applyMu.Unlock()
	if stop == nil {
		return
	}
	stop()
	s.applyDone.Wait()
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
	checkpoint, _, err := statelog.CheckpointOf(ctx, s.db.Replicated(), spec.Name)
	if err != nil {
		return nil, err
	}
	at := checkpoint.At
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
		// THE SAME STREAM READ BY POSITION: how the applier establishes that
		// the log still holds, at its checkpoint, the record it consumed
		// there before it applies anything past it.
		Log:             appendTo,
		StreamCreatedAt: created, Epoch: epoch, Metrics: s.metrics,
		// NO LOGGER, here or at any other statelog constructor: an absent
		// one is the framework's own `component=statelog` logger. The
		// engine's own labelled those lines `component=engine`, and the
		// constructors used to read an absent one as silence — which is
		// what dropped every warning this applier and the write authority
		// beside it wrote.
	})
	if err != nil {
		return nil, fmt.Errorf("engine: build %s's applier: %w", domain.Name(), err)
	}
	// AND WHERE THE LOG ENDS, against the checkpoint the loop is about to
	// load — at boot, before any write can reach the publisher, because boot
	// is when a broker restored from an older copy is met: the embedded one
	// runs in this process, so bringing its store back IS a restart. Its
	// stream keeps its creation instant, so the comparison the runner makes
	// against `created` passes, and without this the only thing refusing an
	// ordinary write was the first heartbeat that happened to read a loaded
	// checkpoint.
	//
	// THE ONE PAIRING READ END-FIRST, and sound anyway: nothing writes the
	// checkpoint row between the two reads — the boot's join has finished
	// before any domain is started, and no applier runs yet — and on a
	// healthy log the row was committed under an end at least as high as the
	// one read now, because the end only grows.
	s.observeEnd(ctx, domain.Name(), runner, at, stats.LastSeq)
	// AND WHETHER THE LOG STILL HOLDS, at that checkpoint, THE RECORD THE ROW
	// NAMES — for the same reason and at the same moment. A restored broker
	// that was written past this node's rows before it booted ends at or past
	// the checkpoint, so the reading above finds nothing, and until the loop
	// has loaded the row nothing in the runner names the record a write would
	// have to be refused over. A log that cannot be read here leaves the
	// question to the loop, which will not apply past the checkpoint before it
	// has answered it.
	if held, diverged, readErr := statelog.CheckpointDiverged(ctx, appendTo, at,
		checkpoint.StoredAt); readErr == nil && diverged &&
		runner.ObserveDiverged(at, checkpoint.StoredAt, held) {
		s.logDiverged(ctx, domain.Name(), runner)
	}

	publisher, evicted, err := s.publisherFor(domain, appendTo, runner)
	if err != nil {
		return nil, err
	}

	running := &runningDomain{
		domain: domain, runner: runner, publisher: publisher,
		log: appendTo, consumer: consumer, evicted: evicted,
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
		// THE RUNNER IS THE IDENTITY, for the reason it is the waiter:
		// the positions a write forms its expectation from and resolves
		// its record against are the runner's, so the answer to "are
		// they on the live stream" has to be the runner's too — and it is
		// the one both the boot's comparison and every live reading land
		// in, so the health refuses on exactly what the writes refuse on.
		Identity: runner,
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
		fence.Floor = s.floorOf(domain.Name())
		fence.Ends = s.logEndsOf(domain.Name(), appendTo, runner)
		fence.Committed = runner.Committed
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
		fence.Floor = s.floorOf(domain.Name())
		fence.Ends = s.logEndsOf(domain.Name(), appendTo, runner)
		fence.Committed = runner.Committed
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

// barrierEncoder is how a domain writes the framework's barrier as a record on
// its own log, or nil for a domain that has none.
//
// # Why this is a switch and not a method on Domain
//
// A barrier is the framework's append and the DOMAIN's record — the read
// index decides when one goes out and what its acknowledgement proves, and
// the domain decides what a record on its log looks like. A domain that has
// no barrier encoder gets no read index and therefore no `linearizable`,
// which is the correct answer for one whose reads make no freshness claim
// rather than a gap: the vectors are derived and compacted, so "as of a
// position" is not a question about them.
func barrierEncoder(domain statelog.Domain) func(statelog.Envelope) ([]byte, error) {
	switch domain.Name() {
	case tracker.Domain{}.Name():
		return tracker.EncodeBarrier
	case pages.Domain{}.Name():
		return pages.EncodeBarrier
	}
	return nil
}

// readerFor builds a domain's READ authority: the four levels, the refusal
// ladder, the coverage probe and the quorum-committed barrier a linearizable
// read waits through — the last only for a domain [barrierEncoder] knows.
//
// Until this existed [statelog.NewReader] and [statelog.NewReadIndex] were
// constructed only by their own tests. Every domain reader read its rows
// straight out of the replicated estate and ECHOED the level back in the
// answer — `tracker.Reader.Tasks` assigning `answer.Level = q.Level`, and
// `Task` taking a level argument whose only use in the file was
// `out.Level = level`. So a seat tool asking for `session` got whatever this
// node happened to hold, a dashboard's `max_lag_seconds` bounded nothing, and
// `linearizable` appended no barrier at all.
func (s *stateLog) readerFor(domain statelog.Domain, appendTo *jetstream.DomainLog,
	runner *statelog.Runner, running *runningDomain) (*statelog.Reader, error) {

	encode := barrierEncoder(domain)

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
		Health: func() statelog.Health {
			h, err := s.health(s.run, running)
			if err != nil {
				// AN UNREADABLE TERM REFUSES rather than serving. The
				// health read reaches the broker and coordination, and
				// a read certified against a health nobody could
				// establish is certified against nothing.
				return statelog.Health{Err: err.Error()}
			}
			return h
		},
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

// trimFloor is the published floor for one domain, at the generation the caller
// is on: the first sequence every node must hold, because everything below it
// may have been removed.
//
// # It is the trim's own record, and it used to be something else — twice
//
// The value here was the minimum over every node's published position —
// including this node's own row. A minimum that includes the reader can never
// exceed the reader, so every comparison made against it was decided before it
// was made: the fence that verifies an expectation of zero is safe never
// refused, the health arm that refuses a node below the floor never fired, and
// the join's own "what must an artefact cover" was one heartbeat of this
// node's own position. The floor theorem's second clause — F <= C+1 verified
// within the call — was verified against a number that could not fail it.
//
// Its replacement read [coord.TrimFloor.TrimTo], the last tick's conclusion,
// and that is the same flaw by a second route. The conclusion is zero on every
// blocked tick, and whenever a node below the log is counted — readmitted,
// restored from an old backup, up on its own history because nobody could
// donate — it IS that node's position, since the applied term is a minimum
// that includes it. So the fence cleared exactly the node it exists to refuse.
// What is read now is [coord.TrimFloor.Floor], which never moves down within a
// generation and is written before the purge it licenses.
//
// # Which bound each reader takes
//
// Every reader that must SURVIVE THE NEXT TRIM takes the higher of this and
// the stream's own first sequence: the write fences, readiness
// ([stateLog.health] and [statelog.Health.Established]), what the join asks an
// artefact to cover, and the re-check after a transfer. The floor covers
// every purge the trim has licensed, including one still in flight; the
// stream covers what this number cannot see — a floor published at another
// generation, which [floorFor] reads as zero.
//
// The readers that ask whether a replay is POSSIBLE — the join's behind test,
// the heartbeat's rejoin request, and readiness when it decides which refusal
// a node below the floor gives — take the stream's first sequence alone,
// because only what IS gone makes a replay impossible: a node between the
// stream's first sequence and this floor can still read every record it
// lacks, so it refuses as `behind` while it replays them
// ([statelog.FloorReplaying]), and sending it to adopt a snapshot would halt
// its appliers to fetch what it could simply replay.
//
// An unreadable register is UNKNOWN and refuses; an absent record is a trim
// that has never run and therefore licensed nothing.
//
// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is what the fence does with
// the error: this is the one check where failing open is a lost update rather
// than a duplicate.
func (s *stateLog) trimFloor(ctx context.Context, domain string, generation uint32) (uint64, error) {
	floors, err := s.fleet.Floors(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: read the fleet's published trim floors: %w", err)
	}
	return floorFor(floors, domain, generation)
}

// floorOf is [stateLog.trimFloor] for one domain, as a write fence reads it:
// at the generation of the cursor the fence is comparing, which the fence
// names per call.
//
// NOT AT THE APPLIER'S LIVE GENERATION, which is what this was once bound to.
// The cursor a fence compares is the checkpoint the write's own snapshot read
// ([statelog.Snap.Checkpoint]), and an adoption can move this node to another
// generation between that snapshot and the check — a floor read there is a
// number in another sequence space, which says nothing about the cursor.
// Read at the cursor's own, a floor published at a higher generation is the
// refusal [floorFor] gives every reader on a dead number space.
func (s *stateLog) floorOf(domain string) func(context.Context, uint32) (uint64, error) {
	return func(ctx context.Context, generation uint32) (uint64, error) {
		return s.trimFloor(ctx, domain, generation)
	}
}

// floorFor is the floor's arithmetic over what the register holds, separated
// from the register so every branch is reachable in a table test.
//
//   - No record for the domain: the trim has never concluded anything about
//     this log, so nothing has been licensed for removal. Zero.
//   - A record at a LOWER generation: it names a dead number space, and in
//     this one the trim has concluded nothing yet. Zero — and the stream's
//     own first sequence, which every reader that must survive the next trim
//     takes as a maximum beside this (see [stateLog.trimFloor]), covers a
//     purge that raced a reanchor. Reading it as unknown instead would refuse
//     every write at an expectation of zero for a whole trim interval after
//     every reanchor.
//   - A record at a HIGHER generation: this node is the one on the dead
//     number space. Unknown, which refuses.
//   - Otherwise the record's Floor — never its TrimTo, which is zero while
//     the trim is blocked and falls with the counted minimum.
func floorFor(floors []coord.TrimFloor, domain string, generation uint32) (uint64, error) {
	for _, f := range floors {
		if f.Domain != domain {
			continue
		}
		switch {
		case f.Generation < generation:
			return 0, nil
		case f.Generation > generation:
			return 0, fmt.Errorf("engine: the published floor for %s is at generation "+
				"%d and this node is on %d — this node's positions name a sequence "+
				"space the fleet has left, so nothing it holds can be compared "+
				"against the floor", domain, f.Generation, generation)
		}
		return f.Floor, nil
	}
	return 0, nil
}

// logEndsOf is a domain log's two ends, as the write fences read them beside
// [stateLog.trimFloor].
//
// THE FIRST END MAY ONLY RAISE THE FLOOR, for the reason
// [statelog.Health.FirstSeq] gives: a stale answer is LOWER than the truth,
// never higher. That is why the published floor is written before the purge —
// it is what covers a purge the stream's own answer has not caught up with —
// and why this is still read: it covers what the floor cannot see — and it is
// what the fence asks, of this node's own checkpoint rather than the
// decision's, whether the node is below the log or only replaying up to the
// floor. THE LAST END is what says this node is on this log at all: the fence
// refuses a node whose checkpoint is past it. An unreadable stream is an error
// the fence refuses on, for the reason every unreadable floor is.
//
// AND IT REPORTS THE STREAM'S IDENTITY, because the one answer that carries the
// ends carries the creation instant beside them. This read is the only one an
// expectation of zero takes of the live log, and the publisher asks the
// runner's identity after it and before anything is appended — so a log
// rebuilt since the last heartbeat is refused within the write, on the one
// branch where letting it through is a lost update rather than a refusal (see
// [statelog.Fence.ClearForZero]). Thrown away, the zero branch's identity would
// be only as fresh as the heartbeat, which is ten seconds of a rebuilt log
// accepting records at zero from a node whose floor names the old one. The end
// goes to the runner too, so what this write found is what the next ordinary
// write's fence 0 refuses on.
func (s *stateLog) logEndsOf(domain string, l *jetstream.DomainLog,
	runner *statelog.Runner) func(context.Context) (statelog.LogEnds, error) {

	return func(ctx context.Context) (statelog.LogEnds, error) {
		// THE CHECKPOINT BEFORE THE READ, which is the only order in which
		// the pair can say anything — see [statelog.Runner.ObserveEnd].
		at := runner.Committed()
		stats, err := l.Stats(ctx)
		if err != nil {
			return statelog.LogEnds{}, fmt.Errorf("engine: read the log's ends: %w", err)
		}
		s.observeStream(ctx, domain, runner, at, stats)
		return statelog.LogEnds{First: stats.FirstSeq, Last: stats.LastSeq}, nil
	}
}

// Readmissible is the node gate's judgement of a readmission: whether an
// evicted node has fallen below a floor it would be counted against again.
// Asked ONCE per gesture, before any log is written — see [NodeGate.Readmit].
//
// HERE, because this is the one object holding all three inputs — the
// positions register the node's own heartbeat writes, the floor the trim
// publishes, and every domain's log — and each is read exactly as the write
// fence reads it: the floor through [floorFor] at the generation this node
// runs, and the stream's first sequence beside it, the higher of the two
// bounding what may be gone. A readmission judged against a different bound
// from the fence the node is about to be subject to would clear it for writes
// that fence refuses.
//
// IDENTITY-CLAIMING DOMAINS ONLY. The floor theorem is about an expectation of
// zero, which only a domain that claims identity can form; a compacted domain
// has no such write, and a node behind in one is a coverage figure rather
// than a node that cannot resume.
//
// Every read that fails is an error and REFUSES: a register nobody could list
// is not a register without the node in it, and a floor nobody could read is
// not a low one.
func (s *stateLog) Readmissible(ctx context.Context, nodeID string) error {
	if s == nil || s.fleet == nil {
		return fmt.Errorf("engine: this node reads no positions register, so it "+
			"cannot judge where %s stands against the trim floor", nodeID)
	}
	register, err := s.fleet.Positions(ctx)
	if err != nil {
		return fmt.Errorf("engine: read the positions register to judge %s's "+
			"readmission: %w", nodeID, err)
	}
	floors, err := s.fleet.Floors(ctx)
	if err != nil {
		return fmt.Errorf("engine: read the published trim floors to judge %s's "+
			"readmission: %w", nodeID, err)
	}
	var bounds []statelog.ReadmissionBound
	for _, name := range s.order {
		running := s.domains[name]
		if !running.domain.ClaimsIdentity() {
			continue
		}
		generation := running.runner.Committed().Generation
		floor, err := floorFor(floors, name, generation)
		if err != nil {
			return err
		}
		first, _, err := running.log.Bounds(ctx)
		if err != nil {
			return fmt.Errorf("engine: read %s's first surviving sequence to judge "+
				"%s's readmission: %w", name, nodeID, err)
		}
		bounds = append(bounds, statelog.ReadmissionBound{
			Domain: name, Generation: generation, Floor: floor, First: first,
		})
	}
	return statelog.PermitReadmission(nodeID, register, bounds)
}

// observeStream hands one live reading of a domain log's state to its runner —
// the creation instant, and the end paired with the checkpoint at, which the
// caller read BEFORE it asked the broker — and names what the reading changed.
//
// ONE PLACE FOR EVERY READER, the position heartbeat and the zero fence,
// because whichever reads the log first is the one that establishes a finding
// and the other then finds it established — so each line is written here,
// once, by whoever got there, rather than by the heartbeat alone and never when
// the fence was first.
func (s *stateLog) observeStream(ctx context.Context, domain string,
	runner *statelog.Runner, at statelog.Position, stats jetstream.LogStats) {

	if runner.ObserveStream(stats.CreatedAt) {
		log.ErrorContext(ctx, "statelog_stream_recreated",
			"node", s.nodeID, "domain", domain,
			"live", stats.CreatedAt.UTC(),
			"error", runner.StreamIdentity().Error(),
			"detail", "this domain's log was deleted and rebuilt under a running "+
				"node, so its sequences name a history this node's rows are not "+
				"keyed to; this node refuses every read and every write of it — "+
				"each refusal names `wrong_stream` — for as long as this process "+
				"runs against it, and an operator re-anchors it: crewlet retention "+
				"reanchor")
	}
	s.observeEnd(ctx, domain, runner, at, stats.LastSeq)
}

// observeEnd hands one reading of a domain log's end to its runner, paired
// with the checkpoint read before it, and names both transitions.
//
// BOTH, and the second is the one that would otherwise be silent. Past the end
// is logged as the error it is. The end reaching the checkpoint again is either
// nothing — a stale member's answer corrected by the next one — or a restored
// log written past this node's checkpoint in another history. The END cannot
// separate the two, and the line says so; the RECORD at the checkpoint can,
// and the applier compares it before it applies anything past the checkpoint
// ([statelog.Runner.VerifyCheckpoint]) — so the second is `statelog_log_diverged`
// and a refusal that does not lift, never a node carrying on.
func (s *stateLog) observeEnd(ctx context.Context, domain string,
	runner *statelog.Runner, at statelog.Position, last uint64) {

	established, reached := runner.ObserveEnd(at, last)
	switch {
	case established:
		log.ErrorContext(ctx, "statelog_ahead_of_log",
			"node", s.nodeID, "domain", domain,
			"checkpoint", at.Seq, "last_seq", last,
			"detail", "this node's checkpoint is past the log's end, so its rows "+
				"hold records at sequences the log has never issued — a stream "+
				"rebuilt under it, or a broker restored from a copy older than "+
				"those rows; this node refuses every read and every write of it, "+
				"each naming `wrong_stream`, while the end stays below the "+
				"checkpoint, and an operator re-anchors it: crewlet retention "+
				"reanchor")
	case reached:
		log.WarnContext(ctx, "statelog_ahead_of_log_cleared",
			"node", s.nodeID, "domain", domain, "last_seq", last,
			"detail", "the log's end has reached this node's checkpoint again, so "+
				"its writes are no longer refused on it. If the earlier reading "+
				"came from a member that had not caught up, nothing was wrong. If "+
				"the broker was restored from an older copy and has since been "+
				"written past this checkpoint, the log holds another record at it: "+
				"the applier compares that record with the one it consumed there "+
				"before it applies anything past it, and a different one is "+
				"statelog_log_diverged — a refusal that does not lift")
	}
}

// logDiverged names the one transition a divergence has: established. Unlike a
// reading of the end it is never cleared by a later reading — a member behind
// the log answers that it holds no record, never with another one — so there is
// no second line to write; a reanchor or an adoption is what ends it.
func (s *stateLog) logDiverged(ctx context.Context, domain string, runner *statelog.Runner) {
	log.ErrorContext(ctx, "statelog_log_diverged",
		"node", s.nodeID, "domain", domain,
		"error", runner.StreamIdentity().Error(),
		"detail", "the log's record at this node's checkpoint is not the one it "+
			"consumed there: the broker was restored from a copy older than these "+
			"rows and has since been written past them, so the log continues a "+
			"history they are not. This node applies nothing past its checkpoint, "+
			"refuses every read and every write of the domain naming "+
			"`wrong_stream`, and donates no snapshot of it; an operator re-anchors "+
			"it (crewlet retention reanchor — the restored case) or replaces its "+
			"rows with a peer's")
}

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
			return false, statelog.RefuseBrokerUnreachable
		}
		if ok, refusal := health.Established(strict); !ok {
			return false, refusal
		}
	}
	return true, ""
}

// Healthy reports whether every registered domain permits this node to KEEP
// the seats it holds, and names the first that does not.
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
//     halted it and every later object is missing its consequences;
//   - the node is EVICTED, so its peers drop every record it publishes and
//     its rows have already stopped being the fleet's;
//   - the node is BELOW THE LOG, so records it never applied have been
//     deleted and its rows have a hole nothing will fill — a node that is
//     only below the published trim floor, whose missing records the log
//     still holds, is replaying them and is behind rather than wrong — and a
//     floor NOBODY COULD READ for four heartbeats takes the same branch as
//     below, because an unread floor is not a floor that is satisfied;
//   - its checkpoint names A STREAM THAT IS NOT THIS ONE, a log deleted and
//     rebuilt under it, so its rows are keyed to a history that is gone;
//   - the applied prefix is STALLED, so the node owes progress it has not
//     made for [statelog.StallGrace] and every expectation it forms is stale;
//   - it has held a record it CANNOT DECODE past [statelog.DeferralGrace],
//     which is D122: under the grace nothing changes, because that covers
//     every rolling upgrade; past it the honest reading is "this node cannot
//     run this company's records" rather than "this node is briefly behind".
//
// BEING BEHIND IS NOT ON THAT LIST AND FIRES NOTHING HERE. A lag is the
// admission gate's business, and a term here that read one sheds a company's
// seats on its own writes: a single node released all seven of its seats on
// each burst of tracker records, because the record it had not applied yet
// made the reading `lag == 0` false for one heartbeat. What a stalled prefix
// says and a lag does not is that the node is not applying its way out.
//
// Until this had a caller the `deferred_old` alarm told an operator "its seats
// move at 30m0s" and its remedy said "its seats have already moved", and
// neither was true — [seat.Host] sheds only on a lost lease, a drain and a
// rebalance, and its own log line says a node that is not ready "keeps what it
// holds". The alarm was reporting a mitigation the engine did not perform.
//
// A DOMAIN WHOSE HEALTH CANNOT BE READ DOES NOT SHED. An unreachable broker is
// the outage during which a company most needs its seats to keep running, and
// tearing them down on an unread number is the failure mode `unknown` exists
// throughout this package to prevent.
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
		if err != nil {
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
	// [statelog.Health.Refusal]'s `stalled` arm exists to say, and it could
	// never fire while nothing assigned this field.
	//
	// A STOP THIS PROCESS ASKED FOR IS NOT A FAULT. Every applier returns
	// its run context's error on shutdown, and reading that as a halt makes
	// a node declare itself broken on the way out — refusing the reads it
	// is still serving and shedding seats a drain is already handing back
	// in order. The distinction is the cause, not the state: a cancelled
	// context is this node stopping, and anything else is the applier
	// stopping underneath it.
	if err := running.runner.Stopped(); err != nil && !errors.Is(err, context.Canceled) {
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
	if deferral, ok := running.runner.Deferred(); ok {
		health.Deferred = 1
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
	first, end, err := running.log.Bounds(ctx)
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
	// AND WHETHER THIS IS THE SAME STREAM AT ALL — the one term here that
	// is OBSERVED rather than derived, because nothing this node holds can
	// show a rebuilt log. From the runner, which is where the write path
	// asks it too: one answer, so the reads and the writes refuse together.
	//
	// ITS RECREATION HALF ONLY. The runner's identity also answers for a
	// checkpoint past the end, and that half is decided on the line above
	// from this health's own end rather than from whichever reading the
	// runner last had — reported as recreated, a node whose broker merely
	// came back from an older copy would name a rebuild that never happened.
	identity := running.runner.StreamIdentity()
	health.StreamRecreated = errors.Is(identity, statelog.ErrStreamRecreated)
	// AND ITS PASSED-GENERATION HALF, from the same answer: a peer
	// re-anchored this log past this node's rows, which is the one finding
	// here a restored broker below this node's checkpoint cannot show.
	health.GenerationPassed = errors.Is(identity, statelog.ErrGenerationPassed)
	// AND ITS DIVERGED HALF — from the runner's own verdict rather than the
	// first finding the identity names, so a node the log diverged from that
	// a peer has also re-anchored past reports both.
	health.LogDiverged = running.runner.Diverged()
	lag := uint64(0)
	if end > at.Seq {
		lag = end - at.Seq
	}
	health.Lag = &lag
	// THE INSTANT IS THE LAG ABOVE; THE HISTORY IS THIS. `Drained` is the
	// applier's own record of having reached the end of its log at least
	// once, which is a property of a series and therefore not derivable
	// from the subtraction beside it — and deriving it from that
	// subtraction is what released every seat on this node each time
	// somebody filed a work item. A STALL CLEARS IT, which is the "and not
	// stalled since" half of the field: rows frozen for the stall grace
	// stopped being a whole state the moment they stopped moving.
	health.Drained = running.runner.Drained() && !health.Stalled
	floor, err := s.trimFloor(ctx, running.domain.Name(), at.Generation)
	if err != nil {
		return health, err
	}
	health.TrimFloor = &floor
	// THE FOUR-VALUED FLOOR, stamped with the instant it was read. Both
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
	// BELOW MEANS THE NEXT RECORD THIS NODE NEEDS MAY BE GONE: the one at
	// checkpoint+1. A checkpoint one below the higher of the floor and the
	// first surviving sequence has applied everything that was, or may yet
	// be, removed.
	//
	// And WHICH below is asked of `first` alone, because the floor is
	// written before the purge it licenses and a purge can fail after it,
	// or a later tick can conclude less and purge nothing: under a floor
	// above the log's first record, a node whose next record is still
	// there is REPLAYING — it reads what it lacks and the state clears on
	// its own, which is `behind` to a reader and to its seats what any
	// other lag is — and only one whose next record is gone from the log
	// is BELOW, with a hole it adopts a snapshot to fill. The same split is the one the
	// join and the heartbeat make, which is what keeps a node this calls
	// replaying from being one they send to adopt.
	//
	// THE BOUNDARY CANNOT DISAGREE WITH ANY OTHER READER'S, by
	// construction rather than by agreement: [statelog.Replayable] is the
	// one predicate [statelog.Health.Established], the join, the
	// heartbeat, a backup and the write fences all call. What each site
	// still chooses is the BOUND it asks of, for the reason
	// [stateLog.trimFloor] gives.
	switch {
	case !statelog.Replayable(at.Seq, first):
		health.Floor.State = statelog.FloorBelow
	case !statelog.Replayable(at.Seq, max(floor, first)):
		health.Floor.State = statelog.FloorReplaying
	}
	return health, nil
}

// applierFor is the state machine each domain declares.
//
// A SWITCH RATHER THAN A METHOD on the domain, because an applier is not part
// of the declaration: the declaration is what a snapshot, a claim, a sweep and
// the trim read, and it must be answerable by a build that cannot construct the
// applier at all. What this costs is that a new domain fails HERE, at boot,
// naming itself — rather than being registered with no state machine and
// applying nothing.
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

// join adopts a peer's snapshot when this node cannot replay its way back,
// and does nothing at all when it can.
//
// # The question, and why it is asked here
//
// A node's checkpoint is a sequence on each domain's log. It can replay from
// there as long as the log still HOLDS the record after it — and it may not:
// the trim deletes records every node has applied, and a node that was down
// long enough, or that has never run at all, wakes up below the floor. There
// is nothing on the log for it to read, and no amount of waiting produces one.
//
// So the check is per domain: is the record after this node's checkpoint at or
// above the first sequence the stream still holds? That is
// [statelog.Replayable], the one spelling every reader of the floor shares. A
// node that cannot replay on ANY domain joins, and the join is wholesale — a
// snapshot names every registered domain or a recipient refuses it, because a
// domain the artefact does not name is one this node would believe it was
// caught up on.
//
// # Why a fresh node does not join
//
// A stream nobody has written to reports a first sequence of 0 and a node that
// has applied nothing has a checkpoint of 0, so a brand-new company's first
// node has missed nothing at all. A node added to a company whose log has been
// written and never trimmed is one below the first record rather than behind
// it: the first sequence is 1, and its next record is exactly that. Those are
// the common cases and they must not pay a fleet round trip. Where peers are
// listening it costs the whole [statelog.OfferWindow], five seconds refusing
// every read and write while donors with nothing usable stay silent; a node
// alone is answered "no responders" at once, which is why a single-node boot
// cannot show the mistake and the boot tests observe the ask itself.
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
// A join that LOST THIS NODE'S OWN DATABASE is not that, and is never reported
// as it: an install that failed and whose rollback could not reopen the live
// file — or an artefact installed and then not opened — leaves no replicated
// estate at all, which is [statelog.ErrEstateNotRestored]. A boot stops on it,
// naming both failures, rather than coming up with nothing to come up on; a
// running node reopens it on its next heartbeat ([stateLog.restoreEstate]) and
// asks the fleet again only when its retry interval allows.
//
// # And why it is the same join a running node makes
//
// Falling below the floor is not a boot-time event: it is what happens to a
// node that was paused, partitioned or slow for longer than the log's replay
// window, and such a node is RUNNING when it finds out. So this is one
// function with two callers — the boot, before any applier exists, and
// [Engine.rejoin], which ends the appliers first and starts them again after
// — and it reports what it concluded, because the runtime caller retries on a
// fleet that could not donate and the boot simply comes up.
func (e *Engine) join(ctx context.Context, s *stateLog,
	logs map[string]*jetstream.DomainLog) (joinOutcome, statelog.OfferRequest, error) {

	conn, ok := e.backends.Queue.(interface{ Conn() *nats.Conn })
	if !ok || conn.Conn() == nil {
		// A JOIN NEEDS THE BROKER'S OWN CONNECTION — a snapshot is
		// megabytes over request/reply rather than anything the queue
		// contract carries. A backend with none is the memory twin, in
		// a test, with no peer to donate anyway.
		return joinNoDonor, statelog.OfferRequest{}, nil
	}

	behind, passed, want, err := s.replayable(ctx, logs)
	if err != nil {
		return "", statelog.OfferRequest{}, err
	}
	if len(behind) == 0 && len(passed) == 0 {
		return joinCurrent, want, nil
	}
	if len(behind) > 0 {
		log.WarnContext(ctx, "statelog_below_the_floor",
			"node", s.nodeID, "domains", behind,
			"detail", "this node's checkpoint is below what the log still holds, "+
				"so it cannot replay the records it is missing; it will ask the "+
				"fleet for a snapshot")
	}
	if len(passed) > 0 {
		generations := make(map[string]uint32, len(passed))
		for _, name := range passed {
			generations[name] = want.Generations[name]
		}
		log.WarnContext(ctx, "statelog_behind_a_reanchor",
			"node", s.nodeID, "domains", passed, "generations", generations,
			"detail", "a peer re-anchored these logs past the generation this "+
				"node's rows are at, so the logs now continue from that peer's "+
				"rows and nothing on them can bring these level; it will ask the "+
				"fleet for a snapshot at the generation named")
	}

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
		Record: func(ctx context.Context, began time.Time, donor string,
			m statelog.Manifest, phase statelog.AdoptionPhase) error {

			return statelog.RecordAdoption(ctx, e.backends.Store, began,
				donor, m, phase)
		},
	})
	if err != nil {
		return "", statelog.OfferRequest{}, err
	}
	_, err = adopter.Join(ctx)
	switch {
	case errors.Is(err, statelog.ErrNoOffer):
		// NOT A FAILURE — see the doc comment. The node comes up on what
		// it has and its coverage says what it cannot account for. The
		// error rides along because it holds each donor's refusal, which
		// is what an operator reads to learn why nobody could donate.
		log.WarnContext(ctx, "statelog_no_snapshot_offered",
			"node", s.nodeID, "domains", slices.Concat(behind, passed),
			"error", err.Error(),
			"detail", "no peer could donate a usable snapshot, so this node "+
				"comes up on the history it has; reads report the coverage "+
				"they could not account for, the domains that gate seat "+
				"admission keep it from claiming work it cannot answer for, "+
				"and it asks again on a widening interval")
		return joinNoDonor, want, nil
	case err != nil:
		return "", statelog.OfferRequest{}, fmt.Errorf("engine: this node cannot "+
			"replay its way to the fleet on %v and the join failed: %w",
			slices.Concat(behind, passed), err)
	}
	// NO LINE OF ITS OWN: the adopter writes `statelog_adopted` with the
	// donor, the artefact's checksum and when it was taken, and a second
	// line here made one adoption read as two.
	return joinAdopted, want, nil
}

// joinOutcome is what a join concluded, for the caller that acts on the
// difference.
//
// THREE ANSWERS, because "adopted nothing" is two different facts to a running
// node. Every domain could replay from the checkpoint its rows keep — nothing
// was asked, and the consumers still belong at that checkpoint — or the node
// is behind and nobody could donate, which is retried on a widening interval.
// A bool reads the first as the second, and [Engine.rejoin] meets the first
// when the estate under it holds an artefact an earlier rejoin installed and
// could not open, which [stateLog.restoreEstate] has opened since: it would
// report "no donor", widen its pause and leave every consumer where the
// replaced file had it.
//
// Never read off the wire, so it carries no Valid: every value is one this
// package's own join returned.
type joinOutcome string

const (
	// joinCurrent — every domain can replay from where its rows say it
	// is, so the fleet was not asked.
	joinCurrent joinOutcome = "current"

	// joinAdopted — a peer's snapshot is installed and complete.
	joinAdopted joinOutcome = "adopted"

	// joinNoDonor — this node is behind and no peer could donate, or it
	// has no broker connection to ask one over.
	joinNoDonor joinOutcome = "no_donor"
)

// rejoin is the runtime adoption: end the appliers, join, start them again.
//
// IT RUNS OVER AN OPEN ESTATE. A previous join that could not reopen the live
// database after a failed install — or could not open the artefact it
// installed — left the node with none ([statelog.ErrEstateNotRestored]), and
// [stateLog.requestRejoin] reopens that with [stateLog.restoreEstate] before
// it calls this, on a schedule of its own: a reopen asks nobody, and this
// asks the fleet.
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
//  3. EACH RUNNER IS RE-KEYED to the checkpoint the file keeps
//     ([statelog.Runner.Rejoined]), which clears a recreated stream or a
//     passed generation when the file now holds the fleet's history — the
//     runner would otherwise stop again on the very checkpoint the adoption
//     installed, leaving a node a peer re-anchored past with no way out but
//     deleting its database. AND THE CONSUMERS ARE RESET to that checkpoint,
//     because the broker will not move a consumer's start and one left at
//     the old position would deliver every record in between to be dropped.
//     Both include a join that found every domain CURRENT: the file under it
//     may be an artefact an earlier rejoin installed and could not open,
//     which no runner was re-keyed to and no consumer moved to — see
//     [stateLog.resetConsumers], which [stateLog.restoreEstate] shares.
//  4. THE APPLIERS START AGAIN, whatever happened: a join that found no
//     donor leaves the node as it was, below the floor and refusing, and a
//     node with no appliers at all would be worse than that.
func (e *Engine) rejoin(ctx context.Context, s *stateLog) error {
	// ONE RECOVERY AT A TIME: a reanchor moving a checkpoint in the file
	// this is about to replace would be writing into a file with no name.
	s.recovering.Lock()
	defer s.recovering.Unlock()
	log.WarnContext(ctx, "statelog_rejoin_started", "node", s.nodeID,
		"detail", "this node cannot replay its way to where the fleet is — it is "+
			"below a log's floor, or a peer re-anchored a log past its "+
			"generation — or an applier holds a recreation verdict its rows no "+
			"longer bear out, so its appliers pause while it asks the fleet for "+
			"a snapshot if it needs one and re-keys each applier to the rows it "+
			"holds")
	s.haltAppliers()
	// ctx IS the state log's own run context here — [requestRejoin]
	// starts this under it — so the relaunched appliers get the lifetime
	// the boot launch gave them rather than a heartbeat tick's.
	defer s.launchAppliers(ctx)

	logs := make(map[string]*jetstream.DomainLog, len(s.domains))
	for name, running := range s.domains {
		logs[name] = running.log
	}
	outcome, want, err := e.join(ctx, s, logs)
	switch {
	case err != nil:
		return err
	case outcome == joinNoDonor:
		return errNoDonor
	}
	// AGAINST THE INSTANT THE JOIN JUDGED THE ARTEFACT BY — the live one it
	// read before asking, which the adopter checked the offer against and
	// re-checked before the install — rather than a fresh read that could
	// fail here and leave a runner keyed to the history the adoption just
	// replaced.
	s.resetConsumers(ctx, func(name string) (time.Time, error) {
		return want.StreamCreatedAt[name], nil
	})
	if outcome == joinAdopted {
		s.nudgeSnapshot()
	}
	log.InfoContext(ctx, "statelog_rejoined", "node", s.nodeID, "outcome", string(outcome))
	return nil
}

// restoreEstate reopens a replicated estate a failed join left closed, and
// moves every consumer to the checkpoint the file it opened keeps.
//
// THE APPLIERS ARE HALTED AROUND IT, as around a join: a consumer is reset
// only while nothing fetches from it, and the relaunch is what resumes each
// runner from the checkpoint of the file now open.
//
// THE CONSUMERS MOVE because the file may not be the one they were left at. A
// failed install whose rollback could not reopen leaves the live file, and the
// reset changes nothing a record would notice; an installed artefact that did
// not open leaves the DONOR'S file, whose checkpoint no consumer was ever
// moved to — and when that file is current, no rejoin follows to move them,
// because the heartbeat that finds the node caught up requests none.
//
// THE ADOPTION ROW IS LEFT AS THE FAILED JOIN LEFT IT: incomplete, because the
// adoption did not complete. Nothing on the write path reads it: whichever
// file the reopen finds carries its own ledger and that ledger's watermark
// (see [statelog.RecordLedgerLoss]). So a restore that finds the donor's file
// current is a recovery, not an adoption to finish.
//
// A failed reopen is [statelog.ErrEstateNotRestored], so every outcome that
// leaves this node with no replicated database reads the same.
func (s *stateLog) restoreEstate(ctx context.Context) error {
	// ONE RECOVERY AT A TIME, as around a join and a reanchor. A reanchor
	// that ran between this reopen and the re-key below would have its new
	// checkpoint re-keyed back to the one this read before it, and its
	// halted applier relaunched with every other in the middle of its
	// transition.
	s.recovering.Lock()
	defer s.recovering.Unlock()
	s.haltAppliers()
	// ctx is the state log's own run context, for [Engine.rejoin]'s
	// reason: the relaunched appliers outlive the heartbeat tick.
	defer s.launchAppliers(ctx)
	if err := s.db.ReopenReplicated(ctx); err != nil {
		return fmt.Errorf("%w: %w", statelog.ErrEstateNotRestored, err)
	}
	log.WarnContext(ctx, "statelog_estate_restored", "node", s.nodeID,
		"path", s.db.ReplicatedPath(),
		"detail", "the replicated database an earlier join left closed is "+
			"open again; the node asks the fleet for a snapshot only if it is "+
			"still below the floor, and no sooner than its retry interval allows")
	// NO JOIN RAN, so no instant was judged: each runner is re-keyed
	// against a fresh read of its live stream. One that cannot be read is
	// re-derived by its own loop against the instants it has read
	// ([statelog.Runner.Run]), and a verdict that leaves wrong is
	// named by the next heartbeat's reading, which asks for the join that
	// re-keys it ([statelog.Runner.RecreationStale]).
	s.resetConsumers(ctx, func(name string) (time.Time, error) {
		stats, err := s.domains[name].log.Stats(ctx)
		if err != nil {
			return time.Time{}, err
		}
		return stats.CreatedAt, nil
	})
	// AND THE SNAPSHOT LOOP IS WOKEN, as after an adoption: the file this
	// opened may be the artefact a failed join installed, at a generation
	// the artefact this node holds does not name — see
	// [stateLog.snapshotNudge]. Where it is the file the node already had,
	// the loop's own gate finds its artefact current and declines.
	s.nudgeSnapshot()
	return nil
}

// resetConsumers re-keys every domain's runner to the checkpoint the replicated
// file now keeps ([statelog.Runner.Rejoined], against the live instant live
// names) and moves its consumer there, because a runner still keyed to the
// history the file replaced stops again on it, and the broker will not move a
// consumer's start — one left at the old position would deliver every record
// in between to be dropped.
//
// A FAILURE IS A LINE, NOT A FAILED CALL, and each half says why it can be.
//
//   - THE CONSUMER: correctness is the checkpoint's and the applier resumes
//     from it regardless; what a consumer left behind costs is the redeliveries
//     between. So a checkpoint that cannot be read leaves that consumer where
//     it was, exactly as a failed reset does — and neither turns an adoption
//     that happened into one its caller reports as deferred and retries. THAT
//     HOLDS BECAUSE THE HANDLE REPAIRS ITSELF. A reset deletes before it
//     creates, so a create that fails leaves the broker with no consumer at
//     all — and the appliers are relaunched after this either way.
//     [jetstream.DomainConsumer] clears the handle on that path and rebuilds it
//     on the next fetch, at the position it held before; without that this
//     line would be logging the start of a domain that never applies another
//     record.
//   - THE RUNNER: a re-key that could not run — the live instant unreadable,
//     or the checkpoint itself — leaves the verdict to the loop, which
//     re-derives it against the row it loads when it is relaunched
//     ([statelog.Runner.Run]) using the instants it has read. The one
//     case that leaves wrong is a file keyed to a stream the runner never read,
//     and the next heartbeat's reading of the live instant names it
//     ([statelog.Runner.RecreationStale]) and asks for the join that re-keys
//     it. So neither failure strands a node the adoption brought back.
func (s *stateLog) resetConsumers(ctx context.Context, live func(name string) (time.Time, error)) {
	for _, name := range s.order {
		running := s.domains[name]
		at, keyed, _, err := statelog.CursorFor(ctx, s.db.Replicated(), running.domain.Stream().Name)
		if err == nil {
			// THE RUNNER FIRST, against the live instant the caller
			// judged the file by: a runner still keyed to the history
			// the file replaced would stop again on the very checkpoint
			// it now holds — a node a peer re-anchored past with no way
			// out but deleting its database.
			const rederived = "the applier re-derives its verdicts from this " +
				"checkpoint when it resumes, against the creation instants it has " +
				"read; a row keyed to a stream it has not read is named by the " +
				"next heartbeat, which asks for the join that re-keys it"
			if instant, lerr := live(name); lerr != nil {
				log.WarnContext(ctx, "statelog_runner_not_rekeyed",
					"domain", name, "checkpoint", at.String(), "error", lerr.Error(),
					"detail", "the live stream's creation instant could not be "+
						"read; "+rederived)
			} else if rerr := running.runner.Rejoined(at, keyed, instant); rerr != nil {
				log.WarnContext(ctx, "statelog_runner_not_rekeyed",
					"domain", name, "checkpoint", at.String(), "error", rerr.Error(),
					"detail", rederived)
			}
			err = running.consumer.Reset(ctx, at.Seq)
		}
		if err != nil {
			log.WarnContext(ctx, "statelog_consumer_not_reset",
				"domain", name, "checkpoint", at.String(), "error", err.Error(),
				"detail", "the consumer is rebuilt at its previous position by "+
					"the next fetch, so this costs redeliveries rather than "+
					"correctness; the applier re-derives its verdicts from the "+
					"checkpoint it loads when it resumes")
		}
	}
}

// requestRejoin is what the heartbeat calls on a node below the floor: it
// reopens a lost estate at once, and asks the fleet for a snapshot one attempt
// at a time, waiting a widening interval after one that failed.
//
// # Two schedules, because they are two costs
//
// The widening bounds how often the fleet is asked. Each ask is an offer
// window the node spends refusing, and one that finds a donor is a whole
// artefact's transfer — so EVERY failed ask widens it, one that lost this
// node's database on its way out included: that attempt fetched, and the next
// would fetch again. Reopening this node's own database asks nobody, and until
// it is open the node serves no replicated read and applies no record, so
// [stateLog.restoreEstate] runs on the next heartbeat whatever the interval
// says, and a reopen, failed or not, moves no schedule. A node the reopen
// leaves below the floor asks when the interval allows and not before.
//
// A lost estate is said at ERROR on every attempt, naming the file, because it
// is the one outcome here an operator may have to fix by hand.
//
// THE HEARTBEAT SEES A NODE WITH NO ESTATE AS BELOW, which is what makes it
// the one to call this: only a join closes the estate, a runtime join runs
// only below the floor, and the position the heartbeat reads is the runner's
// in memory, which nothing moves while the file is closed.
func (s *stateLog) requestRejoin(now time.Time) {
	s.rejoinMu.Lock()
	defer s.rejoinMu.Unlock()
	if s.rejoin == nil || s.rejoining {
		return
	}
	lost := s.db.Replicated() == nil
	ask := !now.Before(s.rejoinAfter)
	if !lost && !ask {
		return
	}
	s.rejoining = true
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		var err error
		if lost {
			err = s.restoreEstate(s.run)
		}
		asked := err == nil && ask
		if asked {
			err = s.rejoin(s.run)
		}
		s.rejoinMu.Lock()
		defer s.rejoinMu.Unlock()
		s.rejoining = false
		if err == nil {
			if asked {
				s.rejoinPause = 0
			}
			return
		}
		if s.run.Err() != nil {
			return
		}
		if asked {
			if s.rejoinPause == 0 {
				s.rejoinPause = PositionHeartbeat
			} else {
				s.rejoinPause = min(s.rejoinPause*2, RejoinRetryCeiling)
			}
			s.rejoinAfter = time.Now().Add(s.rejoinPause)
		}
		if errors.Is(err, statelog.ErrEstateNotRestored) {
			log.ErrorContext(s.run, "statelog_estate_lost",
				"node", s.nodeID, "path", s.db.ReplicatedPath(), "error", err.Error(),
				"asks_fleet_again_in", max(0, time.Until(s.rejoinAfter)).Round(time.Second),
				"detail", "this node has no replicated database open, so it "+
					"serves no tracker, page or search read, applies no record "+
					"and cannot keep its seats; it reopens the file on its next "+
					"heartbeat, and if that keeps failing the error names the "+
					"cause (the disk, the file's permissions, a file the store "+
					"refuses to open) for an operator to fix")
			return
		}
		log.WarnContext(s.run, "statelog_rejoin_deferred",
			"node", s.nodeID, "retry_in", s.rejoinPause, "error", err.Error())
	}()
}

// replayable answers, for every registered domain, whether this node can reach
// the log from where its rows say it is — and what it would need if it cannot.
//
// TWO WAYS NOT TO BE ABLE TO, answered in two lists because they are two
// different incidents an operator reads about. BEHIND: the record after this
// node's checkpoint is gone from the log. PASSED: a peer re-anchored the log
// past this node's generation, so the log now continues from that peer's rows
// rather than from these — see [passedByAReanchor]. Either one is repaired by
// the same adoption.
//
// NEED IS ONE BELOW THE FLOOR, per domain, which is the artefact's own
// acceptance test: a snapshot at that position leaves the node with the whole
// surviving log ahead of it and nothing missing in between. The generation
// travels beside it because a bare sequence from before a reanchor names a
// dead number space and would compare as if it were current — and for a
// passed domain it is the FLEET's generation, the only one a snapshot that
// repairs it can be at.
func (s *stateLog) replayable(ctx context.Context, logs map[string]*jetstream.DomainLog) (
	behind, passed []string, want statelog.OfferRequest, err error) {

	want = statelog.OfferRequest{
		Need:            map[string]uint64{},
		Generations:     map[string]uint32{},
		StreamCreatedAt: map[string]time.Time{},
	}
	// WHERE EACH DOMAIN'S ROWS STAND, read first: the fleet read below asks
	// whether a node is evicted only of one ahead of these, so a fleet on
	// one generation asks nothing of the logs.
	above := map[string]uint32{}
	for _, domain := range registeredDomains() {
		at, _, found, readErr := statelog.CursorFor(ctx, s.db.Replicated(), domain.Stream().Name)
		if readErr != nil {
			return nil, nil, statelog.OfferRequest{}, readErr
		}
		if found {
			above[domain.Name()] = at.Generation
		}
	}
	// THE FLEET'S GENERATIONS, READ ONCE for every domain: two
	// coordination reads per join rather than two per domain, and one
	// answer the domains are all judged against.
	fleet, err := s.fleetGenerations(ctx, logs, above)
	if err != nil {
		return nil, nil, statelog.OfferRequest{}, err
	}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		appendTo, held := logs[name]
		if !held {
			return nil, nil, statelog.OfferRequest{}, fmt.Errorf("engine: %s's log "+
				"was not provisioned before the join asked what it holds", name)
		}
		// ONE ROUND TRIP FOR EVERY TERM THE STREAM ANSWERS, which is
		// also the only way they describe one moment: the first
		// sequence decides whether this node is behind and the creation
		// instant decides whether the stream is even the one it was
		// behind ON, and read separately they can straddle a rebuild.
		stats, err := appendTo.Stats(ctx)
		if err != nil {
			return nil, nil, statelog.OfferRequest{}, err
		}
		first := stats.FirstSeq
		at, _, found, err := statelog.CursorFor(ctx, s.db.Replicated(), domain.Stream().Name)
		if err != nil {
			return nil, nil, statelog.OfferRequest{}, err
		}
		// A NODE WITH NO CHECKPOINT IS NOT AT GENERATION ZERO — IT IS AT
		// NO GENERATION AT ALL, and the difference is the whole of this.
		//
		// The absent row used to be read as `{generation 0, sequence 0}`,
		// which in a fleet that has never re-anchored is exactly right: a
		// fresh node has the whole log ahead of it. In one that HAS, it
		// is a lie in the direction that cannot be recovered from. The
		// stream still begins at sequence 1, so the behind test below
		// passes and the node replays — stamping every row it writes at
		// generation 0 while every peer holds the same records at N,
		// reporting itself caught up over a database that contains only
		// the records published since the re-anchor, blocking the fleet's
		// trim (a counted node at a lower generation makes the applied
		// term unknown), and, once a floor at N is published, failing its
		// own boot on the floor comparison — which is the call that would
		// have sent it to adopt, so the state is terminal.
		//
		// So the generation comes from the FLEET, and a node with no rows
		// in a fleet that has moved past zero must ADOPT: the records
		// before the re-anchor are not on the log to be replayed.
		//
		// AND SO DOES A NODE WHOSE ROWS THE FLEET HAS LEFT BEHIND: the
		// same fleet generation, above the one its rows stand at, on a
		// domain whose nodes must agree on their rows.
		generation := at.Generation
		reanchored := passedByAReanchor(domain, at, found, fleet[name])
		if !found || reanchored {
			generation = fleet[name]
		}
		floor, err := s.trimFloor(ctx, name, generation)
		if err != nil {
			return nil, nil, statelog.OfferRequest{}, err
		}
		want.Generations[name] = generation
		want.StreamCreatedAt[name] = stats.CreatedAt
		if usable := max(first, floor); usable > 0 {
			want.Need[name] = usable - 1
		}
		switch {
		case reanchored:
			passed = append(passed, name)
			continue
		case !found && generation > 0:
			behind = append(behind, name)
			continue
		}

		// WHAT AN ARTEFACT MUST COVER, set above for every domain, is one
		// below the HIGHER of the two: the stream's first surviving
		// sequence is what is gone already, and the published floor is
		// what the trim has been told it may delete and has not
		// necessarily reached. A node that accepted an artefact chosen from
		// `first` alone would install one the trim was about to pass. ONE
		// BELOW because that is the lowest checkpoint [statelog.Replayable]
		// accepts against it, sent as a number since the donor comparing it
		// holds no view of this node's stream.

		// WHETHER THIS NODE IS BEHIND is a different question, and it
		// is asked of `first` ALONE. The floor says what MAY be
		// deleted; only the stream says what IS. The floor is written
		// before the purge it licenses and a purge can fail after it,
		// so a node between the two can still replay every record it
		// lacks — and judging it behind would spend the offer window
		// asking the fleet for a snapshot of records it could simply
		// read, which on a node alone is a snapshot of itself.
		//
		// `first` is the first sequence the stream still HOLDS, and it
		// has two zero cases that are easy to confuse: a stream nobody
		// has written reports 0, NOT 1, and one a purge emptied reports
		// one past its last. A fresh node at checkpoint 0 is behind
		// neither a never-written stream nor one that still starts at
		// 1 — the whole log is ahead of it — and IS behind a stream a
		// purge emptied, because what it never saw went with the purge.
		// [statelog.Replayable] states each case and is the only
		// spelling of the boundary.
		if !statelog.Replayable(at.Seq, first) {
			behind = append(behind, name)
		}
	}
	return behind, passed, want, nil
}

// passedByAReanchor reports a domain a peer has re-anchored past this node's
// rows: they are at a generation below the one the fleet is on, on a domain
// whose nodes must hold identical rows at one checkpoint.
//
// ONE PREDICATE, asked by the join and by the heartbeat that requests one, for
// the reason [statelog.Replayable] is one: a rejoin requested on a bound the
// join does not test halts this node's appliers to ask, adopts nothing, and is
// asked again on the next retry.
//
// # Why the generation alone decides it, and the instant does not
//
// A reanchor opens its generation from ONE node's rows — whatever that node
// held, including what it held past everything the log still carries — so no
// other node's rows are that history, whatever they hold and whichever stream
// they are keyed to, and nothing on the log can make them so. On a recreated
// stream such a node also finds its instant differs; on a broker restored from
// an older copy it does not, and one whose checkpoint was below the restored end
// sees nothing wrong at all, and would carry on applying the new generation's
// records into rows missing the re-anchoring node's tail. Its only way to the
// fleet's history used to be deleting its database; it is now the adoption
// every other node that cannot replay makes.
//
// NEVER FOR A DOMAIN THAT CLAIMS NO IDENTITY, whose nodes re-anchor their own
// copies one at a time by design ([statelog.PermitReanchor]) — adopting a whole
// snapshot to repair one derived index is what that exemption exists to avoid.
// And never for a node with no checkpoint, which the join already sends to
// adopt at the fleet's generation.
func passedByAReanchor(domain statelog.Domain, at statelog.Position, found bool,
	fleet uint32) bool {

	return domain.ClaimsIdentity() && found && fleet > at.Generation
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
		floor, err := s.trimFloor(ctx, name, at.Generation)
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
		if usable := max(first, floor); !statelog.Replayable(at.Seq, usable) {
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
					// the zero value has Drained false, an
					// unread floor and no first sequence, which
					// every gate reads as "cannot vouch for
					// this".
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
		running := s.domains[name]
		row := ReplicationStatus{Name: name, Kind: "domain"}
		health, err := s.health(ctx, running)
		switch {
		case err != nil:
			// UNREADABLE IS NOT READY. The alternative reads as a
			// caught-up loop on a node whose broker is unreachable,
			// which is the one state this count exists to surface.
			row.Detail = "this node cannot read the domain's own position: " +
				err.Error()
		case health.StreamRecreated || health.GenerationPassed || health.LogDiverged:
			// A REBUILT LOG FIRST — or a log a peer re-anchored past this
			// node's generation, or one that diverged from its rows —
			// ahead of the stop it may also have
			// caused: the stop is a consequence (at boot it is this very
			// finding; on a running node the consumer went with the old
			// stream and its fetches fault), and this is the cause, with
			// the remedy. Without the arm a node on a rebuilt log that
			// had not yet faulted past its budget read as ready here,
			// caught up on a sequence the new log had merely reached.
			row.Detail = running.runner.StreamIdentity().Error()
		case health.Err != "":
			// A STOPPED APPLIER IS NOT READY, whatever its lag says: a
			// loop halted on a recreated stream has a lag of zero and
			// applies nothing, and the row read as ready for exactly as
			// long as nobody looked at the error beside the number.
			row.Detail = "the applier stopped: " + health.Err
		case health.AheadOfLog():
			row.Detail = fmt.Sprintf("this node's checkpoint %d is past the log's "+
				"end %d, so it names a stream that is not this one — a recreated "+
				"stream, or a broker restored from an older copy; `crewlet "+
				"retention reanchor` follows the new one from its head",
				health.Position.Seq, *health.LastSeq)
		case health.Lag == nil:
			// THE HEAD COULD NOT BE READ. It reaches here only from a
			// health whose broker answered its bounds and not its
			// stats, and the one thing this row must not print for it
			// is "0 record(s) behind" — the number an unread lag
			// cannot support, and the one a reader would act on.
			row.Detail = "this node cannot tell how far behind the log it is"
		case *health.Lag > 0:
			// BEHIND IS NOT READY AND NOT WRONG. The seats this node
			// holds stay put (see [statelog.Health.Healthy]); what
			// this row feeds is the fleet view's "3 of 5", which is
			// how an operator sees a fresh node catching up.
			row.Detail = fmt.Sprintf("applying: %d record(s) behind the log's head",
				*health.Lag)
		case health.Deferred > 0:
			// CAUGHT UP AND STILL NOT READY. A deferred record is one
			// this build cannot decode: the position moved past it
			// and the rows it would have written are not there, so a
			// loop reporting itself caught up would be claiming a
			// copy it does not have.
			row.Detail = fmt.Sprintf("caught up, holding %d record(s) this "+
				"build cannot decode from sequence %d — a newer build is "+
				"what applies them", health.Deferred, health.DeferredFrom)
		default:
			row.Ready = true
		}
		out = append(out, row)
	}
	return out
}

// startSnapshots runs the two halves of the fleet's own recovery path: this
// node TAKES snapshots of its replicated estate on a timer, and SERVES them to
// a peer that asks.
//
// # Why both halves, and why neither is optional
//
// A node too far behind to replay adopts a peer's snapshot — that is
// [Engine.join], and it is the recipient. A recipient with no donor is a
// mechanism that can never complete: the join asks the fleet, nothing answers,
// and the node comes up on the history it has for ever. Half of this wired is
// worse than none, because the half that IS wired reports itself working.
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
//
// # And a nudge is neither
//
// A reanchor, an adoption and the restore of a file a failed adoption
// installed each leave this node holding an artefact at a generation a joiner
// will refuse, so each wakes the loop at once ([stateLog.nudgeSnapshot])
// rather than leaving it to whichever wait it is in — the interval, a day by
// default, after a taken snapshot.
func (e *Engine) snapshotLoop(s *stateLog, snap snapshotTaker,
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
		case <-s.snapshotNudge:
			// A REANCHOR, AN ADOPTION OR A RESTORE: the next take is a
			// question about whether the artefact is current, which the
			// gate answers — see [stateLog.snapshotNudge].
		}
	}
}

// snapshotTaker is the snapshotter as its loop uses it: one attempt, which
// either took an artefact or says why not. [statelog.Snapshotter] is the
// implementation.
type snapshotTaker interface {
	Take(ctx context.Context) (statelog.Manifest, error)
}

// nudgeSnapshot wakes the snapshot loop, without waiting for it — see
// [stateLog.snapshotNudge].
func (s *stateLog) nudgeSnapshot() {
	select {
	case s.snapshotNudge <- struct{}{}:
	default:
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
// The register is not telemetry. Three things read it and each one is wrong
// without this node's row:
//
//   - THE TRIM takes a minimum across every counted node's position to decide
//     what records the fleet has finished with. A node that publishes nothing
//     is a node the trim cannot see, so the log is trimmed past records this
//     node still needs — and the node discovers that by falling below the
//     floor and having to adopt a snapshot.
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
// THE WRITE FENCE DOES NOT READ IT, and must not. It reads the floor the trim
// publishes and the stream's own first sequence — never a number derived from
// rows that include its own, because a minimum that includes the reader can
// never refuse it. That is also what makes the first bullet survivable: a
// node the trim passed because its row went missing is refused at the fence,
// and sent to adopt by this loop's own below-the-log check.
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
	// NO REGISTER, NOTHING TO PUBLISH TO — the condition the heartbeat is
	// never started under, checked here too because a reanchor publishes
	// through this directly.
	if s.fleet == nil {
		return
	}
	row := coord.NodePositions{
		NodeID:        s.nodeID,
		At:            time.Now().UTC(),
		EngineVersion: version.String(),
		Domains:       make(map[string]coord.DomainPosition, len(s.order)),
	}
	below := false
	// THE FLEET'S GENERATIONS, for the one way a node falls out of step
	// that nothing on its own log shows it: a peer re-anchored the log past
	// this node's generation. Unread, this beat judges nothing by them —
	// the same coordination store takes this node's own row below, and a
	// failure there is what gets said. An EVICTED peer's generation is not
	// the fleet's ([stateLog.fleetGenerations]), which is what lets the
	// eviction of a decommissioned peer that had re-anchored stop sending
	// this node to ask for a donor that no longer exists.
	above := make(map[string]uint32, len(s.order))
	for _, name := range s.order {
		above[name] = s.domains[name].runner.Committed().Generation
	}
	generations, genErr := s.fleetGenerations(ctx, s.openLogs(), above)
	for _, name := range s.order {
		running := s.domains[name]
		at := running.runner.Committed()
		// AND PASSED BY A REANCHOR: the same predicate the join asks, so
		// the rejoin this requests is one the join then acts on. The
		// runner is told first, which is what refuses this node's reads
		// and writes of the domain until the adoption lands.
		if genErr == nil && passedByAReanchor(running.domain, at, true, generations[name]) {
			if running.runner.ObserveFleetGeneration(generations[name]) {
				log.WarnContext(ctx, "statelog_generation_passed",
					"domain", name, "stream", running.domain.Stream().Name,
					"generation", at.Generation, "fleet_generation", generations[name],
					"detail", "a peer re-anchored this log past the generation "+
						"this node's rows are at, so the log now continues from "+
						"that peer's rows; this node refuses reads and writes of "+
						"the domain and adopts a snapshot from a peer at the new "+
						"generation")
			}
			below = true
		}
		// THE INSTANT THE ROWS ARE KEYED TO rides beside the generation,
		// because a generation alone cannot say which stream a sequence is
		// on — see [coord.DomainPosition.StreamCreatedAt].
		pos := coord.DomainPosition{
			Seq: at.Seq, Generation: at.Generation, AppliedThrough: at.Seq,
			StreamCreatedAt: running.runner.KeyedTo(),
		}
		// APPLIED_THROUGH IS LOWER WHEN SOMETHING IS DEFERRED, and the
		// two numbers are what tell a lagging node from a stalled one:
		// a position that advances while nothing is applied is exactly
		// what a retained record produces.
		deferral, held := running.runner.Deferred()
		if held {
			pos.Deferred = 1
			if deferral.Position.Seq > 0 {
				pos.AppliedThrough = deferral.Position.Seq - 1
			}
		}
		// THE OBSERVATION RIDES THIS LOOP, because "has not moved" needs a
		// previous look and this is the one place that already takes one
		// every interval. See [progress]: it is what makes `Stalled` and
		// the deferral shed derivable at all, and doing it here rather
		// than on a health read is what keeps a health read pure.
		//
		// BEHIND IS READ FROM THE BROKER'S OWN LAST SEQUENCE rather than
		// from the consumer's backlog: an idle node's applied prefix does
		// not move because there is nothing to move it, and a stall is
		// only a stall when there is work it owes.
		behind := false
		if stats, err := running.log.Stats(ctx); err == nil {
			behind = stats.LastSeq > at.Seq
			// AND BELOW: the next record this node needs is gone from
			// the log. It cannot replay its way back, so it adopts —
			// the same repair the boot makes, made by a node that is
			// running. Asked of `first` ALONE, as the join asks it: a
			// rejoin requested on a bound the join does not test halts
			// this node's appliers to ask, adopts nothing, and is asked
			// again on the next retry.
			if !statelog.Replayable(at.Seq, stats.FirstSeq) {
				below = true
			}
			// AND WHETHER IT IS EVEN THE SAME LOG.
			//
			// The live stream's creation instant is sampled once at
			// start, so without this a stream deleted and rebuilt under
			// a running node was never named as one. The sequence terms
			// above cannot see it — a rebuilt stream comes back at
			// generation 0 counting from 1, so once it has published
			// past this node's checkpoint every one of them reads as
			// healthy while the node applies a different history into
			// rows keyed by the old one. The instant arrives in this
			// same answer, and every beat hands it to the runner, whose
			// verdict both the reads and the writes refuse on. It is
			// not the only reader: an expectation of zero reads the
			// log within the write and hands its instant over too, so
			// that branch does not wait for this interval.
			//
			// AND THE END, paired with `at` — read above, BEFORE this
			// answer, which is the only order in which a checkpoint past
			// the end means the log is not this node's history rather
			// than that a record landed between the two reads. It is
			// what fence 0 refuses an ordinary write on, on a node whose
			// broker came back from an older copy with its instant.
			s.observeStream(ctx, name, running.runner, at, stats)
			// AND WHETHER THE LOG STILL HOLDS, at this node's checkpoint,
			// THE RECORD IT CONSUMED THERE. The loop asks before it applies
			// past the checkpoint after every moment that could have
			// changed it; this beat is what asks of a node whose loop has
			// nothing to apply — a restored log written EXACTLY to its
			// checkpoint — or has stopped for another reason, and of a
			// broker restored under a running node between two of the
			// loop's own moments. One direct read per domain per beat.
			established, verifyErr := running.runner.VerifyCheckpoint(ctx)
			if established {
				s.logDiverged(ctx, name, running.runner)
			}
			if verifyErr != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "statelog_checkpoint_unverified",
					"node", s.nodeID, "domain", name, "error", verifyErr.Error(),
					"detail", "whether the log still holds the record at this "+
						"node's checkpoint could not be read this beat; the "+
						"applier asks again before it applies past it, and so "+
						"does the next beat")
			}
			// AND A RECREATION VERDICT THE LIVE INSTANT CONTRADICTS: rows
			// an adoption keyed to the stream the broker serves, under a
			// runner whose re-key could not happen — see
			// [statelog.Runner.RecreationStale]. Nothing but a join
			// re-keys it, and a reanchor refuses such rows, so the rejoin
			// is asked for here; the join finds every domain current and
			// re-keys each runner against the instant it reads.
			if running.runner.RecreationStale(stats.CreatedAt) {
				below = true
			}
		}
		running.progress.observe(row.At, pos.AppliedThrough, behind, held)
		// AND WHETHER THE LOG DIVERGED FROM THESE ROWS, for the peers: a
		// node whose rows the log does not continue past its checkpoint is
		// one whose history a write by any other node on this log would
		// contradict — see [coord.DomainPosition.LogDiverged].
		pos.LogDiverged = running.runner.Diverged()
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
// for as long as nothing was published.
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
		s.metrics.Set(metrics.StatelogDrainRowsPerSecond, running.runner.Drain(), attrs)
		// AND THE COMMIT RATE BESIDE IT, because the two are different
		// resources: rows/s is progress and commits/s is the fsync rate
		// a device's write budget is spent by. A node whose rows/s is
		// healthy and whose commits/s has doubled is doing twice the
		// disk work for the same progress, which neither figure alone
		// can say.
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
		s.metrics.Set(metrics.StatelogApplyLagSeconds,
			applyLagOf(health, running).Seconds(), attrs)
	}
}

// deferralGauges publishes how long this node has held what it cannot decode.
//
// SECONDS RATHER THAN A COUNT, and beside the count rather than instead of it:
// one record held for an hour and sixty held for a second are the same count
// and completely different states, and it is the AGE that decides whether this
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
