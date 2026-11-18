package engine

import (
	"context"
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

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/version"
	"github.com/crewlet/crewlet/internal/words"
)

// This node's state-log runtime: which domains it runs, and the four objects
// each needs — a LOG to append to, a CONSUMER to read it back, an APPLY LOOP
// turning records into rows, a PUBLISHER deciding what to append. Two are the
// broker's and two the store's, and every failure that matters is one being
// fine while another is not, so they stay four objects.
//
// The consumer resumes at the applier's checkpoint, which commits with the
// rows it covers: the only durable statement of where this node is, and what
// spares a restart a redelivery of every applied record. Seat admission waits
// on the domains a seat depends on ([statelog.Domain.GatesSeatAdmission]): a
// seat attached before its tracker would answer "no such task" and act on it.
// Whether a domain's copy is WRONG is [stateLog.judgeCopy]'s question (ADR-0079).

// domainHost is the broker as this node's state log needs it, declared here
// and satisfied by the JetStream queue: the one place the engine names a queue
// backend. A state log needs a conditional append, a per-node durable consumer
// and a stream whose settings make an interior gap impossible, none of which
// the EventQueue contract carries.
type domainHost interface {
	EnsureDomainStream(ctx context.Context, spec jetstream.DomainStream) error

	// Clustered is whether the broker has peers, which the provisioning
	// budgets branch on ([jsprovision.Clustered]).
	Clustered() jsprovision.Clustered

	// StreamBudget is what the broker will let this node reserve. A ceiling
	// is a reservation checked against the broker's own limit, not the disk,
	// so one derived from free space alone is refused on machines that have
	// the space.
	StreamBudget(ctx context.Context) (jetstream.StorageBudget, error)

	// GrowthBudget is how far a running log's ceiling may be raised: an
	// update is checked by the metadata group's leader, not weighed against
	// a member's own room as a create is. A capacity operation resizes an
	// existing stream, so it is held to this one.
	GrowthBudget(ctx context.Context) (jetstream.StorageBudget, error)

	// DomainStreamCeiling is the ceiling a domain's stream already holds,
	// and whether it exists. An existing log reserves what it holds, not
	// what it would ask for now, so a restart sizes as the first boot did
	// and a new log is sized beside it.
	DomainStreamCeiling(ctx context.Context, stream string) (int64, bool, error)
	DomainLog(ctx context.Context, stream string) (*jetstream.DomainLog, error)
	DomainConsumer(ctx context.Context, stream, nodeID string, after uint64) (*jetstream.DomainConsumer, error)
}

// runningLog is one domain's log as this node runs it, with everything it
// took.
type runningLog struct {
	domain statelog.Domain

	// spec is the log's stream, the key of every per-stream thing here.
	spec statelog.StreamSpec

	runner    *statelog.Runner
	publisher *statelog.Publisher
	log       *jetstream.DomainLog
	consumer  *jetstream.DomainConsumer

	// reserve holds this node's ordinary appends (the write authority's and
	// the read index's) out of the log's gate reserve. Nil for a log that
	// keeps none ([statelog.ReservationOf]).
	reserve *statelog.Reserve

	// No creation instant is held here: the stream a domain's positions are
	// keyed to ([statelog.Runner.StreamCreatedAt]) and whether the live
	// stream is still it ([statelog.Runner.StreamIdentity]) are the runner's,
	// since the write path refuses on them. A copy sampled at boot goes stale
	// when a stream is rebuilt under a running node.

	// evicted is this domain's eviction gate, taken from the write fence so
	// readiness reads the row the write path reads: two sources for one fact
	// is how a node whose writes every peer drops still answers reads (D109
	// (b)'s worst shape). Nil for a domain with no gate: "not evicted".
	evicted func(ctx context.Context) (bool, error)

	// progress is what a snapshot cannot see: whether the checkpoint moves,
	// and how long an undecodable record has been held ([progress] says why
	// it is not in [statelog.Health]).
	progress progress

	// ends is the log's two ends as a READ is served them: the last read
	// that answered, refreshed off the read's path (servedhealth.go).
	ends memo[streamEnds]
	// verifier opens a record's frame for the applier and the change feed
	// alike (a second consumer of the same bytes): one verifier, so neither
	// can be the reader that hands a decoder the signature. signer signs the
	// appends no write authority makes — a reanchor's generation record, a
	// read index's barrier, the trim's anchor sweep ([retention.sweepDeps]) —
	// since every applier drops a record that is not a frame this keyring
	// opens.
	verifier *recordseal.Verifier
	signer   *recordseal.Signer

	// reader is this domain's read authority (the four levels, the refusal
	// ladder, the coverage probe, the barrier wait). Nil for a domain with no
	// barrier encoder, whose reads make no freshness claim: vectors are
	// derived and compacted, so "as of a position" has no answer about them.
	reader *statelog.Reader

	// lagNanos is how far behind the log this applier is, as a duration,
	// sampled on the position heartbeat rather than asked for: every session
	// validation compares it against [statelog.StallGrace], and a broker
	// round trip on the authentication path would make a broker blip an
	// outage for everybody. It is one of two terms of [runningLog.Lag]. A
	// heartbeat whose stream stats failed leaves the last value standing:
	// unreadable is not caught up. Atomic: the heartbeat writes it while
	// every request reads it.
	lagNanos atomic.Int64
}

// standing is this log as a standing read takes it: every record's body,
// opened under its domain's verifier, beside the log's per-subject probe.
// Never the raw log of signed frames (ADR-0031), whose signature a standing
// read would decode as the envelope.
func (d *runningLog) standing() statelog.StandingLog {
	return statelog.StandingOf(d.log, d.verifier)
}

// observeLag stores this applier's distance behind the log as a duration.
func (d *runningLog) observeLag(last, applied uint64) {
	d.lagNanos.Store(int64(lagDurationOf(last, applied, d.runner.Drain())))
}

// backlogOf is whether a domain owes work, from one read of its log's
// statistics. An unreadable log carries the last look forward rather than
// reading as caught up ([progress.owed]).
func backlogOf(stats jetstream.LogStats, err error, applied uint64,
	previously bool) bool {

	if err != nil {
		return previously
	}
	return stats.LastSeq > applied
}

// lagDurationOf is how long behind a backlog of (last - applied) records is at
// this applier's measured drain rate (fleet nodes differ by an order of
// magnitude). Pure over values so the rule deciding whether a session is
// served or answered 503 is testable without a broker. A rate of zero is
// floored at one record a second: a node with no measured rate is the least
// able to claim it is nearly caught up.
func lagDurationOf(last, applied uint64, drain float64) time.Duration {
	if last <= applied {
		return 0
	}
	return time.Duration(last-applied) * time.Second /
		time.Duration(max(int64(drain), 1))
}

// Lag is how far behind the log this applier is: the backlog at the last
// heartbeat over its drain rate, or how long its checkpoint has been frozen
// while it owed work, whichever is larger. A retained record moves the
// checkpoint, so a deferral is never a lag. The second term makes a wedge
// visible: nothing moves the drain rate while no batch runs, so the first
// alone reads a wedged applier as caught up ([progress.frozenFor]).
func (d *runningLog) Lag() time.Duration { return d.lagAt(time.Now()) }

// lagAt is [runningLog.Lag] at an instant a case chooses.
func (d *runningLog) lagAt(now time.Time) time.Duration {
	return max(time.Duration(d.lagNanos.Load()), d.progress.frozenFor(now))
}

// stateLog is this node's whole state-log runtime.
type stateLog struct {
	// mode is what this node started for. In a mode that publishes nothing
	// the logs still apply, heartbeat and serve reads but append nothing: no
	// reader gets a read index, so a linearizable read is refused
	// `maintenance` ([statelog.ReaderDeps.Mode]), and the operator gestures
	// that append ([NodeGate], [Engine.Reanchor]) are refused through
	// [stateLog.appends].
	mode statelog.MaintenanceMode

	// logs is every log this node runs ([logSet]), written once where the
	// runtime is built: after every log started, before any loop reading the
	// set is launched. Nothing starts or stops a log while the node runs (an
	// adoption relaunches the appliers over the same logs; a reanchor re-keys
	// a runner in place), so readers walk it without a lock and may hold a
	// log. Nil on a runtime whose boot failed first, which runs nothing
	// ([stateLog.held]).
	logs *logSet

	nodeID string
	db     *store.DB
	fleet  coord.Fleet

	// floorMemos is each domain's published floor as a READ is served it:
	// the last read of its key that answered, refreshed off the read's path
	// (servedhealth.go).
	floorMemos floorMemos
	// epoch is where every domain's applier reads its per-epoch
	// configuration from, once per batch ([statelog.RunnerDeps.Epoch]) —
	// [Engine.applyEpoch], the published company's.
	epoch func() map[string]any

	// publishing serializes [stateLog.publishPositions], whose reading of
	// the runners and write of the row must not interleave with another's.
	publishing sync.Mutex

	// skew is the heartbeat's last measure of the fleet's clocks (this
	// node's and the register-leading broker member's distance from the data
	// nodes' median, [stateLog.measureSkew]); nil until a beat measures,
	// after two misses in a row, or once the heartbeat stops. Atomic: the
	// alarm reads it on another loop (clockskew.go).
	skew atomic.Pointer[skewMeasure]

	// skewMisses is how many publishes in a row measured nothing, which is
	// what drops [stateLog.skew] ([stateLog.recordSkew]). Guarded by
	// publishing, the one lock every publish holds.
	skewMisses int

	// clustered is whether the broker has peers, which is what a replicated
	// create's budget branches on — and so how long a reanchor's steps after
	// its append may take ([statelog.ReanchorDeps.CompletionBudget]).
	clustered jsprovision.Clustered

	// metrics is the process's one recorder, threaded down so the apply
	// loop's instruments are observed rather than merely declared.
	metrics *metrics.Recorder

	// witness is where every domain's applier reports a record it would
	// not authenticate. Nil reports nothing beyond the log and the metric.
	witness statelog.Witness

	// skills is this build's tool-skill parser, because the pages applier is
	// the only thing that sees a skill page arrive and leave: Doclet has no
	// page webhook and the change feed drops those changes. Nil answers "not
	// a skill".
	skills doclet.SkillDetector

	// applyHooks is what each domain's applier calls after a committed
	// batch — see [applyHooks].
	applyHooks

	// ceilings is the byte ceiling each domain's stream is created with,
	// sized from Tier A inside the broker's budget ([ceilingsFor]), or the
	// ceiling an existing stream is reported against. Applied only at
	// creation: a stream's configuration has one writer and a booting node is
	// not it, or restart order would set a shared limit and undo an emergency
	// grant.
	ceilings map[string]domainCeiling

	// volume is the directory the ceilings were derived from, which a
	// refused reservation names when that volume is what bounds the
	// broker.
	volume string

	// floor is the least the broker must reserve for a fresh node of this
	// Tier A ([BrokerFloorFor]), which a refused reservation quotes. Zero
	// says nothing.
	floor BrokerFloor

	// ring is the Tier A keyring every record is signed under and verified
	// against, read once: only a restart changes it.
	ring recordseal.Keyring

	// snapshot is what this node's snapshot loop last concluded, handed to
	// the position heartbeat: the loop runs on the staleness interval (a day
	// by default), the heartbeat every ten seconds. Atomic rather than a
	// mutex so the heartbeat reads it mid-copy; nil is a loop that has
	// concluded nothing yet.
	snapshot atomic.Pointer[snapshotHeld]

	// usageSeen is the usage log's generation on every data node as the
	// heartbeat last read the positions register (this node's from its
	// runner), which the usage publisher holds a peer's reanchor against
	// ([usage.PublisherDeps.Generations]). Nil before any reading. Atomic:
	// the heartbeat writes it while the publisher reads it.
	usageSeen atomic.Pointer[usage.Generations]

	// usagePublisher is this node's running usage publisher, else nil: the
	// usage applier's first drain wakes it ([stateLog.onDrained]), which ends
	// a reanchor of the usage log. Atomic: the engine arms and stops it while
	// an applier runs.
	usagePublisher atomic.Pointer[usage.Publisher]

	// donor is this node's donor once [Engine.startSnapshots] built it, nil
	// before (so during every boot's join). A join asks it whether it serves
	// before counting this node among the donors it waits for
	// ([Engine.joinExpectation]). Atomic: the runtime rejoin reads it on the
	// heartbeat, which starts first.
	donor atomic.Pointer[statelog.Donor]

	// snapshotNudge wakes the snapshot loop early for the events that make
	// this node's artefact one nobody can adopt — a reanchor moved a domain
	// to a new generation, an adoption replaced its rows, or a restore after
	// a failed adoption opened the artefact it installed — since a joiner
	// asks for each domain's current generation and would otherwise wait up
	// to the interval. Also for an applier's first drain, which ends
	// `unhydrated` ([statelog.RunnerDeps.OnDrained]). Buffered by one: a
	// second pending nudge adds nothing.
	snapshotNudge chan struct{}

	// heartbeatNudge wakes the position heartbeat early when this node has
	// just taken an artefact: the trim's snapshot term and a joiner count
	// donors from the register, and a row naming it up to
	// [statelog.PositionHeartbeat] late is a donor the fleet could have
	// counted. Buffered by one, as snapshotNudge is.
	heartbeatNudge chan struct{}

	// admit tells the engine's seat-claim gate that a log whose health
	// gates admission has come level ([stateLog.levelHook]); nil tells
	// nobody.
	admit func()

	// run is the context the runtime's loops (heartbeat, snapshot loop,
	// donor) run under, and stop ends them. Held rather than re-derived: a
	// goroutine started under the caller's context is one stop cannot end,
	// and the wait then blocks for ever (package doc, "The core runtime").
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup

	// The apply loops run under a context of their own, a child of run,
	// because a running node ends and restarts them: an adoption replaces the
	// replicated file, which needs the appliers' pinned connections released.
	// applyStop ends them, applyDone joins them, launchAppliers starts them
	// again. Each loop has its own context beneath that, since a reanchor
	// restarts only the one domain whose checkpoint it moves; applying holds
	// each loop's stop, and ending the set ends them all.
	applyMu   sync.Mutex
	applyCtx  context.Context
	applyStop context.CancelFunc
	applying  map[string]*applierRun
	applyDone sync.WaitGroup

	// recovering serialises what rewrites a log's checkpoint or re-keys its
	// runner under a running node: a runtime adoption (replaces the
	// replicated file), the restore of an estate a failed adoption left closed
	// (reopens it, re-keys every runner), a reanchor (moves one log's
	// checkpoint). Two interleaved would write into a file being replaced,
	// re-key to a moved checkpoint or relaunch a halted loop. One lock, since
	// every log's rows are in the one file.
	recovering sync.Mutex

	// rejoin is what the heartbeat calls when it finds this node below the
	// log's floor while running; the engine sets it, since the join reaches
	// the store bracket and the broker's connection. The fields beside it
	// make it single-flight with a widening retry, while a lost estate is
	// reopened on every beat ([stateLog.requestRejoin]).
	rejoin      func(context.Context) error
	rejoinMu    sync.Mutex
	rejoining   bool
	rejoinAfter time.Time
	rejoinPause time.Duration

	// tailHoldEvery is how often the join's pin on the replay tail is
	// renewed: [statelog.TrimHoldHeartbeat], set once at start. A field so
	// a case can renew on a cadence it can wait out ([statelog.HoldTail]).
	tailHoldEvery time.Duration

	// tailMu guards tailHeld: the release of the pin an installed artefact
	// still needs, held until this node's published position counts the tail
	// it pins ([stateLog.holdUntilCounted]); nil when no adoption waits. Not
	// publishing's lock: the join hands the pin over mid-publish, and a
	// publish decides by what it captured at its start
	// ([stateLog.publishPositions]).
	tailMu   sync.Mutex
	tailHeld *heldTail
}

// applyHooks are what each domain's applier calls after a committed batch,
// built once by [Engine.applyHooks]: an applier handed a nil hook compiles and
// passes its own package's cases while the consequence (a skill page nobody
// re-reads, a suspension that reaches no contact map, a stale screen) shows up
// far away. The directory and inbox cases here build their appliers from the
// register with the same value, so dropping either hook fails a case.
type applyHooks struct {
	// nudgeSkills is what the pages applier calls after a batch that moved a
	// tool-skill page ([stateLog.skills] says why the apply). Nil nudges
	// nobody.
	nudgeSkills func()

	// identityMoved is what the identity applier calls after a committed
	// batch with what it moved ([iamdomain.Moved]): a seat's standing for the
	// party registry, whose credentials to close. The apply is the only thing
	// that sees it on every data node: a suspension moves nothing a published
	// company carries, and the change feed relays a record to one node. A
	// node without `data` runs no applier: its registry re-reads the directory
	// through a data node ([Engine.startRoutedDirectory]) and it holds no
	// socket, since `ingress` requires `data`. It must not block — it runs on
	// the apply loop with the next batch waiting, so it signals
	// ([Engine.identityMoved]). Nil: nobody is listening.
	identityMoved func(iamdomain.Moved)

	// inboxMoved is what a [registration.Inbox] domain's applier calls after
	// a batch that moved somebody's inbox ([Engine.SetOnInboxMoved]), handed
	// only to those ([stateLog.inboxFor]). Every data node applies every
	// record and only data nodes serve sockets (ADR-0025), so each tells its
	// own. A node whose copy is out of service relays a sound peer's
	// announcements instead (liverelay.go), fed by this hook on that peer. It
	// must not block.
	inboxMoved func([]inbox.Movement)

	// artifactsMoved is what the artifacts applier calls after a batch with
	// what it moved ([artifactsdomain.Moved]): a version, a narrowing's
	// access epoch, a trash, a purge — for the serving origin's authorization
	// cache and the socket hub, which serve only this node. It must not block
	// ([Engine.OnArtifactsMoved]).
	artifactsMoved func([]artifactsdomain.Moved)

	// artifactDataMoved is what the DATASTORE APPLIER calls after a
	// committed batch, with the documents it moved ([artifactdata.Moved]),
	// for artifactsMoved's reason: the socket hub serves only this node's
	// own sockets. It must not block. See [Engine.OnArtifactDataMoved].
	artifactDataMoved func([]artifactdata.Moved)

	// chatApplied is what the chat applier calls after a batch with what it
	// wrote ([chatlet.Applied], [Engine.SetChatLive]), relayed like
	// inboxMoved while this node's copy is out of service, or a room open
	// here would go quiet until its socket moved. It must not block.
	chatApplied func([]chatlet.Applied)

	// replaced is what the state log calls once its replicated estate was
	// replaced under a running node (an adoption, or a reopen after a failed
	// join, [stateLog.estateReplaced]), for what is derived from those rows
	// by a forward-only walk: the chat index and the change index
	// ([search.ChatIndexer.RequestRebuild],
	// [search.ChangeIndexer.RequestRebuild]). No batch says it, so it is not a
	// post-commit hook. It must not block.
	replaced func()
}

// applyHooks is every post-commit hook this engine hands its appliers.
func (e *Engine) applyHooks() applyHooks {
	return applyHooks{
		nudgeSkills:       e.nudgeSkills,
		identityMoved:     e.identityMoved,
		inboxMoved:        e.inboxMoved,
		artifactsMoved:    e.artifactsMoved,
		artifactDataMoved: e.artifactDataMoved,
		chatApplied:       e.chatApplied,
		replaced:          e.estateReplaced,
	}
}

// logSet is the logs this node runs: each by its domain's name, and every
// name in the register's own order ([registeredDomains]), so every surface
// that walks them renders them the same way. A map's iteration order would
// make one screen's rows move between refreshes.
type logSet struct {
	byKey map[string]*runningLog
	order []string
}

// newLogSet is the set of runs, in the order given — the register's, since
// every runtime starts its logs by walking it.
func newLogSet(runs []*runningLog) *logSet {
	set := &logSet{byKey: make(map[string]*runningLog, len(runs))}
	for _, running := range runs {
		set.byKey[running.domain.Name()] = running
		set.order = append(set.order, running.domain.Name())
	}
	return set
}

// running is the set's logs in its order.
func (set *logSet) running() []*runningLog {
	if set == nil {
		return nil
	}
	out := make([]*runningLog, 0, len(set.order))
	for _, key := range set.order {
		out = append(out, set.byKey[key])
	}
	return out
}

// held is the logs this node runs — never nil, so a runtime that started none
// walks an empty set.
func (s *stateLog) held() *logSet {
	if s == nil || s.logs == nil {
		return &logSet{}
	}
	return s.logs
}

// running is every log this node runs, in the register's order.
func (s *stateLog) running() []*runningLog { return s.held().running() }

// Domain answers the named domain's log, or nil for a domain this node runs
// no log of.
func (s *stateLog) Domain(name string) *runningLog { return s.held().byKey[name] }

// RejoinRetryCeiling bounds how long a node below the floor waits between
// attempts to adopt a snapshot when the last found no usable donor. The first
// retry is a heartbeat away and each doubles, so a donor a minute from its
// first snapshot is asked within that minute, while a fleet with none is asked
// a dozen times an hour rather than 360 — each ask up to a five-second offer
// window in which the node refuses every read.
const RejoinRetryCeiling = 5 * time.Minute

// errNoDonor reports a runtime join that found nothing to adopt, which is a
// state to retry from rather than a failure: the node stays as it is, below
// the floor and refusing, until a peer can donate.
var errNoDonor = errors.New("engine: no peer could donate a usable snapshot")

// errJoinUnable reports a runtime join that ended on a fault of this node's own
// before anything was installed ([statelog.ErrJoinerFault]). Retried from like
// [errNoDonor], and for the same reason: the node is as it was, below the floor
// on its own intact rows. A separate value so the retry's line says whose fault
// it was — the join has already logged what to fix.
var errJoinUnable = errors.New("engine: this node could not adopt a peer's " +
	"snapshot, for a fault of its own")

// estate is the replicated estate's file AS THE RUNTIME HANDS IT to the
// framework and the domains: resolved through this node's handle on every
// call, never captured, because an adoption closes the file, renames a peer's
// artefact over it and opens it again — see [store.ReplicatedHandle].
func (s *stateLog) estate() store.ReplicatedHandle { return s.db.Replicated() }

// estateLogs is how many logs run on the replicated estate (one per registered
// domain, since every data node runs every domain's log) and so how many
// writers their apply loops pin on its file ([store.DB.OpenReplicated]). Each
// loop holds one pin for its life as its domain's single writer; the count is
// declared so the pool is sized for them, or a reader starves behind a writer
// that never returns its connection. Nothing else pins: the maintenance sweeps
// take pooled write transactions. A node without `data` opens no estate
// ([holdEstate]).
func estateLogs() int { return len(register()) }

// holdEstate opens this node's replicated estate for the node's life (the
// store's Close takes it down); a node without `data` ([HoldsEstate]) opens
// nothing. At boot, before any company is applied, not by the state-log
// runtime: a company that moved its tracker to Jira still leaves the file on
// disk, and a backup must not leave it out. Idempotent, for an engine started
// again over the same backends.
func holdEstate(ctx context.Context, db *store.DB, b *config.Bootstrap) error {
	if !HoldsEstate(b) {
		return nil
	}
	if _, err := db.OpenReplicated(ctx, estateLogs()); err != nil {
		return fmt.Errorf("engine: open the replicated estate this node holds: %w", err)
	}
	return nil
}

// domainEstate is the replicated estate as this node's domain-level consumers
// read it (a seat's tracker search, the search index and its coverage); a node
// without `data` answers [store.ErrNoEstate] through it. A read handle: what
// may write the estate is internal/store's applier gate's short list, and
// answering the write handle here would put every caller on it.
func (e *Engine) domainEstate() store.ReplicatedReader {
	if e.backends == nil || e.backends.Store == nil {
		return store.ReplicatedReader{}
	}
	return e.backends.Store.Replicated().Reader()
}

// estateOpen reports whether this node's replicated estate is open — which,
// while its state log runs, it is but for the window an adoption holds it
// closed, or after a join whose reopen failed.
func (s *stateLog) estateOpen() bool {
	_, err := s.db.ReplicatedDB()
	return err == nil
}

// reopenEstate opens the replicated estate if it is not open. It is the one
// way the runtime brings its file back: at its own start, and when a join left
// it closed.
func (s *stateLog) reopenEstate(ctx context.Context) error {
	if s.estateOpen() {
		return nil
	}
	if _, err := s.db.OpenReplicated(ctx, estateLogs()); err != nil {
		return fmt.Errorf("engine: open the replicated estate: %w", err)
	}
	return nil
}

// snapshotHeld is the artefact this node holds, and why it holds no current
// one. Both, since a skip deletes nothing: a node that could not refresh still
// donates its old copy, and the register's readers need both that (the trim's
// snapshot term) and that it stopped refreshing (an operator asking why a join
// failed).
type snapshotHeld struct {
	// Manifest is the newest complete artefact on this node's disk, and
	// Have reports whether there is one at all.
	Manifest statelog.Manifest
	Have     bool

	// Skip is why this node holds no CURRENT artefact, empty when its
	// newest is within the operator's interval.
	Skip statelog.SkipReason
}

// startStateLog brings up every registered domain's log as one runtime:
// provision, resume, run, per log, and the order is the contract. A consumer
// opened before its stream reads nothing; a runner started before its consumer
// resumed from the rows reads from wherever the broker's default put it.
func (e *Engine) startStateLog(ctx context.Context, boot *config.Bootstrap,
	nodeID string) (*stateLog, error) {

	host, ok := e.backends.Queue.(domainHost)
	if !ok {
		return nil, fmt.Errorf("engine: this node's broker cannot host a state "+
			"log (%T): Worklet and Doclet need a conditional append and a "+
			"per-node durable consumer, and neither is in the event-queue "+
			"contract because nothing else needs them", e.backends.Queue)
	}
	if nodeID == "" {
		return nil, fmt.Errorf("engine: a state-log node has no id — it names " +
			"this node's own consumer, stamps every record it writes and is " +
			"what the eviction gate compares against")
	}

	// The register is checked first, before a stream is sized or a
	// connection opened: its failures are facts about this build (a domain
	// with no applier, write authority or ceiling), and a refusal naming the
	// domain beats a half-started node failing later elsewhere.
	if err := checkRegister(register()); err != nil {
		return nil, err
	}

	// SIZED BEFORE ANYTHING IS STARTED, so the one failure here that is a
	// declaration rather than a broker (a registered domain Tier A has no
	// ceiling for) has nothing to unwind.
	ceilings, err := ceilingsFor(ctx, host, boot)
	if err != nil {
		return nil, err
	}
	// The keyring is required wherever a domain runs, checked here so the
	// refusal names the configuration. Every record is signed under it: the
	// broker's admission vouches for a connection, never a record.
	if _, err = recordseal.NewSigner(registeredDomains()[0].Name(), recordKeyring(boot)); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &stateLog{
		mode:   e.Mode(),
		ring:   recordKeyring(boot),
		nodeID: nodeID, db: e.backends.Store, fleet: e.backends.Fleet,
		epoch:   e.applyEpoch,
		metrics: e.metrics,
		witness: e.stateLogWitness(),
		skills:  skillDetector{}, applyHooks: e.applyHooks(),
		ceilings: ceilings, volume: streamVolume(boot), floor: BrokerFloorFor(boot),
		clustered: host.Clustered(),
		run:       runCtx, stop: cancel,
		snapshotNudge:  make(chan struct{}, 1),
		heartbeatNudge: make(chan struct{}, 1),
		tailHoldEvery:  statelog.TrimHoldHeartbeat,
		admit:          e.nudgeAdmission,
	}
	// Provision every log first, then decide whether this node can replay
	// from where it is: the join reads each stream's first surviving
	// sequence, and a stream created a moment ago reports 0, "nothing was
	// trimmed", for a log the fleet has written for months.
	//
	// One ceiling over the whole bring-up, for [jsprovision.SequenceBudget]'s
	// reason: a replicated stream create per log, on every data node, would
	// otherwise each find a wedged cluster on its own budget, and the queue's
	// ceiling covers only the engine's streams. The consumer creates get a
	// ceiling of their own, after the join, whose snapshot transfer must not
	// spend the creates' budget.
	provisionCtx, cancelProvision := context.WithTimeout(ctx, host.Clustered().SequenceBudget())
	defer cancelProvision()

	// THE ESTATE'S FILE FIRST, before anything reads a checkpoint out of
	// it: the join below asks each log's rows where they stand, and an
	// estate that is not open answers nothing it could be asked.
	if err = s.reopenEstate(ctx); err != nil {
		s.Stop()
		return nil, err
	}

	logs, err := s.provisionAll(provisionCtx, host)
	if err != nil {
		s.Stop()
		return nil, err
	}

	// Adopt before any applier runs: a join replaces the replicated file,
	// which it can do only while no transaction holds it open (an applier
	// mid-batch would write into an unnamed inode). Each runner is built
	// after this, against the live instant, over whatever file the join left.
	if _, _, err := e.join(ctx, s, logs); err != nil {
		s.Stop()
		return nil, err
	}

	// A second ceiling over the consumer creates, opened after the join,
	// which transfers a snapshot and can honestly take minutes: spanning both
	// would let a large adoption eat the creates' budget.
	consumerCtx, cancelConsumers := context.WithTimeout(ctx, host.Clustered().SequenceBudget())
	defer cancelConsumers()

	started := make([]*runningLog, 0, len(logs))
	for _, domain := range registeredDomains() {
		running, err := s.startLog(ctx, consumerCtx, host, domain, logs[domain.Name()])
		if err != nil {
			// EVERY LOG OR NONE. A node running half its logs serves
			// rows derived from one log while another's records pile
			// up unapplied, and nothing above it can tell that from a
			// node that is merely behind.
			for _, r := range started {
				r.consumer.Close()
			}
			s.Stop()
			return nil, err
		}
		started = append(started, running)
	}
	// THE SET, ONCE AND BEFORE ANY LOOP THAT READS IT: the appliers, the
	// heartbeat, the snapshot loop and the donor all start below, and the set
	// is never written again ([stateLog.logs]).
	s.logs = newLogSet(started)
	// THE STATE LOG'S OWN CONTEXT, never the boot call's: an applier is
	// joined by [stateLog.Stop], which ends that one.
	//nolint:contextcheck // s.run is [context.WithoutCancel] of the boot
	// context: an applier started under the CALLER's would stop the moment
	// start returned, and every read of that log would go stale.
	s.launchAppliers(s.run)
	// A node that falls below the floor while running adopts as at boot: the
	// heartbeat notices and calls this, which ends the appliers, installs the
	// artefact and starts them again over it.
	s.rejoin = func(ctx context.Context) error { return e.rejoin(ctx, s) }
	// AFTER EVERY LOG IS RUNNING. The heartbeat reports each log's
	// position and the snapshot gate reads each one's health, so both need
	// the loops they describe to exist.
	s.startPositionHeartbeat()
	e.startSnapshots(ctx, boot, s)
	log.InfoContext(ctx, "statelog_started", "node", nodeID, "domains", s.held().order)
	return s, nil
}

// startLog builds one domain's log on the stream [stateLog.provision] opened
// on host. Its applier is not launched here. What the domain must answer is a
// fact about this build, refused earlier by [checkRegister]
// ([checkIdentityEntry]); left here is the boot's fact: the stream was opened.
func (s *stateLog) startLog(ctx, consumerCtx context.Context, host domainHost,
	domain statelog.Domain, appendTo *jetstream.DomainLog) (*runningLog, error) {

	if appendTo == nil {
		return nil, fmt.Errorf("engine: %s's stream was not provisioned before "+
			"its log was started", domain.Name())
	}
	return s.start(ctx, consumerCtx, host, domain, appendTo)
}

// Stop ends every loop this node started and waits for them. The appliers are
// halted last, after [stateLog.done] drains: a runtime rejoin is in that group
// and relaunches them on its way out ([Engine.rejoin]), so halting them first
// let a Stop mid-rejoin return with a relaunched set running against a store
// its caller closes next. Cancelling the run ends the appliers too (their
// context is its child); the halt is what waits for them.
func (s *stateLog) Stop() {
	if s == nil {
		return
	}
	s.stop()
	s.done.Wait()
	// A pin no row will now count is released once the heartbeat that would
	// have counted it stops; left, its renewal would pin the trim for this
	// process's life and a stale window after.
	s.releaseTail()
	s.haltAppliers()
	// The consumers are closed last, once no loop can fetch through them: an
	// engine may be stopped and another started over the same connection,
	// and an open handle leaves its last pull request standing on the
	// broker, served before the next engine's into a buffer nobody reads
	// until the ack window.
	for _, running := range s.running() {
		if running.consumer != nil {
			running.consumer.Close()
		}
	}
	// Not the file: the node holds its replicated estate whether or not a
	// state log runs ([holdEstate]), and the store's Close takes it down.
}

// applierRun is one domain's apply loop: what ends it, and what reports it
// has ended.
type applierRun struct {
	stop context.CancelFunc
	done chan struct{}
}

// launchAppliers starts every domain's apply loop under a fresh context derived
// from base, at boot and after every adoption, over the same runners:
// [statelog.Runner.Run] resumes from the checkpoint the file now keeps. base is
// the state log's own lifetime ([stateLog.run]), never the caller's, which
// would stop the loop when the boot call or heartbeat tick returned; passed so
// the call site says whose lifetime it is.
func (s *stateLog) launchAppliers(base context.Context) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if s.applyStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(base)
	s.applyCtx, s.applyStop = ctx, cancel
	held := s.held()
	s.applying = make(map[string]*applierRun, len(held.order))
	for _, running := range held.running() {
		s.startApplierLocked(ctx, running)
	}
}

// startApplierLocked starts one log's loop under set, which is the set's own
// context. The caller holds applyMu and has checked the set is launched.
func (s *stateLog) startApplierLocked(set context.Context, running *runningLog) {
	ctx, cancel := context.WithCancel(set)
	run := &applierRun{stop: cancel, done: make(chan struct{})}
	s.applying[running.domain.Name()] = run
	s.applyDone.Add(1)
	go func() {
		defer s.applyDone.Done()
		defer close(run.done)
		defer cancel()
		// A stopped applier is not a crashed node: its rows freeze, the
		// coverage it reports says so, and the readiness gate stops taking
		// seats off [statelog.Runner.Stopped] rather than this error. No line
		// of its own: the runner logs `statelog_applier_stopped` with the
		// stream, position and what resumes it, under the component the
		// replication guide names.
		_ = running.runner.Run(ctx)
	}()
}

// haltApplier ends ONE domain's loop and waits for it, reporting whether it was
// still running (false for a loop that already stopped, or a set not launched).
// For a reanchor, which rewrites one domain's checkpoint: this loop is its only
// other writer, and a batch committed after the reanchor would write the old
// generation and stream instant back over it.
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
// first resetting its consumer to the checkpoint the rows keep, as a rejoin
// does: what it delivered was against the checkpoint a reanchor has just
// rewritten, and a record left in its buffer would reach the resumed loop too.
// A failed reset costs only redeliveries the loop drops, and is logged.
func (s *stateLog) resumeApplier(ctx context.Context, name string) {
	running := s.Domain(name)
	at, _, _, err := statelog.CursorFor(ctx, s.estate().Reader(), running.spec.Name)
	if err == nil {
		err = running.consumer.Reset(ctx, at.Seq)
	}
	if err != nil {
		log.WarnContext(ctx, "statelog_consumer_not_reset",
			"domain", name, "checkpoint", at.String(), "error", err.Error(),
			"detail", "the loop resumes from its checkpoint regardless, and drops "+
				"what the consumer hands over below it, so this costs "+
				"redeliveries rather than correctness")
	}
	s.launchApplier(name)
}

// launchApplier starts ONE log's loop again under the set's context. A no-op
// while the set is halted — the launch that ends that halt starts every log —
// and while the log already has a loop running.
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
	s.startApplierLocked(s.applyCtx, s.Domain(name))
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

// provisionAll provisions every registered domain's log, in the register's
// order, and opens each, keyed by the domain's name.
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

// provision creates one log's stream if it is not there and opens it, apart
// from [stateLog.start] because the join between them needs every stream to
// exist first.
func (s *stateLog) provision(ctx context.Context, host domainHost,
	domain statelog.Domain) (*jetstream.DomainLog, error) {

	spec, ceiling, err := s.specOf(domain)
	if err != nil {
		return nil, err
	}
	if err = host.EnsureDomainStream(ctx, jetstream.DomainStream{
		Name:     spec.Name,
		Subjects: spec.Subjects,
		MaxBytes: ceiling.Bytes,
		// The domain's largest record as the broker counts a message, so
		// a peer that does not refuse a larger one itself is held to the
		// size every node's gate reserve was sized by.
		MaxMessageBytes: spec.MaxAppendBytes(),
		MaxPerSubject:   spec.MaxPerSubject,
		MaxAge:          spec.MaxAge,
		Duplicates:      spec.Duplicates,
	}); err != nil {
		if errors.Is(err, jetstream.ErrInsufficientStorage) {
			return nil, s.storageRefused(ctx, host, spec, domain, ceiling, err)
		}
		// THE CEILING AND ITS FIELD RIDE EVERY FAILURE, because a
		// clustered broker that cannot place the stream says
		// "insufficient storage" in prose, about members this node
		// cannot see, and names neither.
		return nil, fmt.Errorf("engine: provision the %s log at a %d-byte ceiling "+
			"(%s): %w", domain.Name(), ceiling.Bytes, ceiling.Field, err)
	}
	appendTo, err := host.DomainLog(ctx, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("engine: open the %s log: %w", domain.Name(), err)
	}
	return appendTo, nil
}

// start brings up one log on the stream [stateLog.provision] opened.
func (s *stateLog) start(ctx, provisionCtx context.Context, host domainHost, domain statelog.Domain,
	appendTo *jetstream.DomainLog) (*runningLog, error) {

	spec, _, err := s.specOf(domain)
	if err != nil {
		return nil, err
	}

	// The stream says which stream this is: its creation instant
	// ([statelog.StreamCreatedAt], read by Stats) detects a recreated stream,
	// compared against the instant the checkpoint was committed under, so it
	// must be the live one, never the cursor row's own value.
	stats, err := appendTo.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read %s's identity: %w — a node cannot "+
			"resume a domain on a stream it cannot identify, because a recreated "+
			"one is empty and would be applied from as though nothing were "+
			"missing", domain.Name(), err)
	}
	created := stats.CreatedAt.UTC()
	if created.IsZero() {
		return nil, fmt.Errorf("engine: the log reports no creation instant "+
			"for %s, so this node cannot tell it from a recreated stream: %w",
			spec.Name, statelog.ErrStreamRecreated)
	}

	// THE ROWS SAY WHERE THIS NODE IS. The checkpoint commits with them,
	// so it is the only durable statement of it — and resuming a consumer
	// anywhere else is either a hole (at the head) or a million
	// redeliveries (at the beginning).
	checkpoint, _, err := statelog.CheckpointOf(ctx, s.estate().Reader(), spec.Name)
	if err != nil {
		return nil, err
	}
	at := checkpoint.At
	// The consumer ceiling, not the caller's context: a replicated create
	// like the stream's, and one ceiling shared by every domain, so a wedged
	// metadata group cannot spend a full per-create budget per domain.
	consumer, err := host.DomainConsumer(provisionCtx, spec.Name, s.nodeID, at.Seq)
	if err != nil {
		return nil, fmt.Errorf("engine: open %s's consumer: %w", domain.Name(), err)
	}

	applier, err := s.applierFor(domain)
	if err != nil {
		return nil, err
	}
	verifier, err := s.verifierFor(domain)
	if err != nil {
		return nil, err
	}
	signer, err := s.signerFor(domain)
	if err != nil {
		return nil, err
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: domain, Spec: spec, Applier: applier, Fetch: consumer, Verifier: verifier,
		// The replicated estate, not the node's own file: the applier pins
		// a connection for its loop's life on the estate's pool, sized one
		// per log ([estateLogs]). On the node's file it would refuse its
		// first round ("declared 0 pinned writers") and the domain would
		// silently stay behind for ever.
		DB: s.estate(),
		// AND THE NODE'S OWN, where a log that diverged from these rows is
		// remembered across a restart ([statelog.NodeEstate]).
		Node: s.db,
		// AT THE WHOLE CHECKPOINT, and the record it names: what this node
		// publishes and verifies before its loop has loaded the row.
		Checkpoint: at, CheckpointStoredAt: checkpoint.StoredAt,
		// THE SAME STREAM READ BY POSITION: how the applier establishes that
		// the log still holds, at its checkpoint, the record it consumed
		// there before it applies anything past it.
		Log:             appendTo,
		StreamCreatedAt: created, Metrics: s.metrics,
		// THE EPOCH AS A SOURCE, read per batch: the runner is built once,
		// at boot, and outlives every activation — see
		// [statelog.RunnerDeps.Epoch].
		Epoch: s.epoch,
		// A RECORD THIS NODE WOULD NOT AUTHENTICATE reaches the
		// company's audit feed as well as this node's log.
		Witness: s.witness,
		// Who this node is and whom the fleet evicted name the remedy for
		// a record from a generation these rows never entered (this node's
		// failed reanchor, run again; an evicted node's generation,
		// re-anchored past; a peer's, adopted). The log is read through its
		// verifier: it holds signed frames (ADR-0031), whose signature the
		// probe would read as an unreadable eviction.
		NodeID: s.nodeID,
		Evicted: func(ctx context.Context, node string) (bool, error) {
			evicted, readErr := s.evictedOn(ctx, domain, spec,
				statelog.StandingOf(appendTo, verifier), []string{node})
			return evicted[node], readErr
		},
		// The first drain ends `unhydrated`, the snapshot loop's commonest
		// boot skip ([stateLog.snapshotNudge]), and wakes the usage
		// publisher for the usage log ([stateLog.onDrained]).
		OnDrained: s.onDrained(domain),
		// AND EVERY TIME THE LOOP COMES LEVEL, the seat-claim gate's watch,
		// for a log whose health gates admission ([stateLog.levelHook]).
		OnLevel: s.levelHook(domain),
		// No logger, here or at any statelog constructor: an absent one is
		// the framework's `component=statelog` logger, the component its
		// warnings belong to.
	})
	if err != nil {
		return nil, fmt.Errorf("engine: build %s's applier: %w", domain.Name(), err)
	}
	// Where the log ends, against the checkpoint the loop will load, at boot
	// before any write reaches the publisher: boot is when a broker restored
	// from an older copy is met (the embedded one runs in this process), and
	// its stream keeps its creation instant, so the runner's `created`
	// comparison passes. Read end-first, and sound: nothing writes the
	// checkpoint row between the two reads (the join is done, no applier
	// runs), and on a healthy log the end only grows.
	s.observeEnd(ctx, domain.Name(), runner, runner.Stance(), stats.LastSeq)
	// And whether the log still holds, at that checkpoint, the record the row
	// names, for the same reason: a restored broker written past this node's
	// rows ends at or past the checkpoint, so the end reading finds nothing,
	// and the heartbeat's comparison is an interval away. Through the runner,
	// which also recalls a divergence recorded before a restart. An
	// unreadable log or estate leaves it to the loop, which will not apply
	// past the checkpoint before answering it.
	if established, readErr := runner.VerifyCheckpoint(ctx); readErr == nil && established {
		s.logDiverged(ctx, domain.Name(), runner)
	}

	reserve, err := reserveFor(domain, spec, appendTo)
	if err != nil {
		return nil, err
	}
	publisher, evicted, err := s.publisherFor(domain, spec, appendTo, runner, reserve)
	if err != nil {
		return nil, err
	}

	running := &runningLog{
		domain: domain, spec: spec,
		runner: runner, publisher: publisher,
		log: appendTo, consumer: consumer, evicted: evicted, reserve: reserve,
		verifier: verifier, signer: signer,
	}
	// AFTER the struct exists, because the health closure the reader
	// holds reads through it — a reader built first would capture a
	// half-assembled domain and report its progress as never observed.
	if running.reader, err = s.readerFor(domain, spec, appendTo, runner, running); err != nil {
		return nil, err
	}
	// THE LOOP IS NOT STARTED HERE. Every domain is built first and the
	// loops are launched together by [stateLog.launchAppliers], which is
	// also what an adoption calls to start them again.
	return running, nil
}

// publisherFor builds one domain's write authority. The fence and gates are
// the domain's, off its register entry: the tracker's fence reads its eviction
// rows and its gates a deletion marker, while the vector domain declares both
// open (a derived value has no history to protect; the fleet singleton's lease
// stops an evicted node writing). It also returns the domain's eviction
// reader, so readiness asks the write path's question of the same row.
func (s *stateLog) publisherFor(domain statelog.Domain, spec statelog.StreamSpec,
	appendTo *jetstream.DomainLog, runner *statelog.Runner,
	reserve *statelog.Reserve) (*statelog.Publisher, func(context.Context) (bool, error), error) {

	return s.publisherOver(domain, spec, appendTo, appendTo, runner, reserve)
}

// reserveFor is the gate reserve on one domain's log, reading its usage from
// the broker's stream state; nil for a log that keeps none. One per log, for
// the write authority and the read index alike, since its budget is everything
// this node has in flight there. Whether a log keeps one is derived from
// [statelog.ReservationOf], which the publisher and read index enforce, not
// stated on the register entry as a second answer; sized by the log's spec,
// whose largest record is what one node may hold in flight.
func reserveFor(domain statelog.Domain, spec statelog.StreamSpec,
	appendTo *jetstream.DomainLog) (*statelog.Reserve, error) {
	if !statelog.ReservationOf(domain).Kept {
		return nil, nil
	}
	reserve, err := statelog.NewReserve(spec,
		func(ctx context.Context) (statelog.Usage, error) {
			stats, err := appendTo.Stats(ctx)
			if err != nil {
				return statelog.Usage{}, err
			}
			return statelog.Usage{Bytes: stats.Bytes, MaxBytes: stats.MaxBytes}, nil
		})
	if err != nil {
		return nil, fmt.Errorf("engine: build %s's gate reserve: %w", domain.Name(), err)
	}
	return reserve, nil
}

// publisherOver is [stateLog.publisherFor] with the appender the write
// authority publishes through named apart from the log whose ends its fence
// reads: one DomainLog in production, and in a case that has to see exactly
// what reached the broker, that log behind a recorder.
func (s *stateLog) publisherOver(domain statelog.Domain, spec statelog.StreamSpec,
	publishTo statelog.Appender, appendTo *jetstream.DomainLog, runner *statelog.Runner,
	reserve *statelog.Reserve) (*statelog.Publisher, func(context.Context) (bool, error), error) {

	entry, found := registrationFor(domain.Name())
	if !found {
		return nil, nil, fmt.Errorf("engine: domain %q is not registered, so this "+
			"build has no write authority for its log", domain.Name())
	}
	return s.publisherFrom(entry, spec, publishTo, appendTo, runner, reserve)
}

// publisherFrom is [stateLog.publisherOver] over one register entry, which is
// what lets a test hand it an entry the shipped register does not hold — a
// write authority built short of a seam has to be refused HERE, before the
// domain starts, and the register's own entries cannot reach that arm.
func (s *stateLog) publisherFrom(entry registration, spec statelog.StreamSpec,
	publishTo statelog.Appender, appendTo *jetstream.DomainLog, runner *statelog.Runner,
	reserve *statelog.Reserve) (*statelog.Publisher, func(context.Context) (bool, error), error) {

	domain := entry.Domain
	if entry.NewSeams == nil {
		return nil, nil, fmt.Errorf("engine: domain %q is registered and has no "+
			"write authority, so nothing could ever append to its log",
			domain.Name())
	}
	signer, err := s.signerFor(domain)
	if err != nil {
		return nil, nil, err
	}
	verifier, err := s.verifierFor(domain)
	if err != nil {
		return nil, nil, err
	}
	deps := statelog.Deps{
		// EVERY APPEND IS SIGNED (ADR-0031): the publisher frames what its
		// decision encoded, and every applier refuses a record that is not
		// a frame this deployment's keyring opens.
		Domain: domain, Spec: spec, Log: publishTo, Signer: signer,
		Waiter: runner, NodeID: s.nodeID,
		// The same log read by position through its verifier, which a
		// resolution reads to learn whose a record it did not append is:
		// decoding the raw frame would read every record as nobody's.
		Records: statelog.VerifiedLog{Log: appendTo, Verifier: verifier},
		// AND THE RULES ITS REANCHORS VOID RECORDS BY, the runner's own, so
		// a write they dropped is refused under them.
		Voids: runner,
		// The runner is the identity, as it is the waiter: a write's
		// positions are the runner's, so "are they on the live stream" is
		// too, and the health refuses on exactly what the writes refuse on.
		Identity: runner,
		// READ FRESH ON EVERY PUBLISH rather than captured: a reanchor
		// moves the generation under a running process, and a publisher
		// stamping the old one would write records every applier reads
		// as safely stale.
		Generation: func() uint32 { return runner.Committed().Generation },
	}
	// A NIL RESERVE STAYS A NIL ADMISSION: a nil *Reserve in the interface
	// would read as a reserve given to a log that keeps none.
	if reserve != nil {
		deps.Admission = reserve
	}
	// THE DOMAIN'S OWN SEAMS, off its register entry rather than a switch
	// here — see statelogregister.go — over the replicated estate as this
	// log's spec declares it.
	seams, err := entry.NewSeams(s, spec, appendTo, runner)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: build %s's read seam: %w", domain.Name(), err)
	}
	deps.Rows, deps.Fence, deps.Gates = seams.Rows, seams.Fence, seams.Gates
	publisher, err := statelog.NewPublisher(deps)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: build %s's write authority: %w", domain.Name(), err)
	}
	return publisher, seams.Evicted, nil
}

// barrierEncoder is how a domain writes the framework's barrier as a record on
// its own log, or nil for a domain that has none. On the register's entry, not
// Domain: the read index decides when a barrier goes out, the domain what its
// record looks like. A domain without one gets no read index and so no
// `linearizable`, correct for reads that make no freshness claim (the derived,
// compacted vectors). [checkRegister] refuses an entry that states neither
// ([registration.Barrier]).
func barrierEncoder(domain statelog.Domain) func(statelog.Envelope) ([]byte, error) {
	entry, _ := registrationFor(domain.Name())
	return entry.Barrier
}

// trimmedEncoder is how a domain writes the trim's trimmed record (ADR-0080) on
// its own log, or nil for a domain whose log writes no anchor (the compacted
// vectors and usage). Off the register's entry ([registration.Trimmed]) for
// [barrierEncoder]'s reason. [checkRegister] refuses an encoder that disagrees
// with [statelog.WritesAnchors], and
// TestEveryLogThatWritesAnchorsHasATrimmedRecord holds every domain to it, so
// one with anchors and no encoder fails rather than growing an anchor per
// subject.
func trimmedEncoder(domain statelog.Domain) func(statelog.Envelope) ([]byte, error) {
	entry, _ := registrationFor(domain.Name())
	return entry.Trimmed
}

// readerFor builds a domain's read authority: the four levels, the refusal
// ladder, the coverage probe and the quorum-committed barrier a linearizable
// read waits through — the last only for a domain [barrierEncoder] knows. Off
// [registration] rather than a switch, because absent and deliberately absent
// must be told apart: a forgotten domain was a working node that quietly
// refused its strongest read level.
func (s *stateLog) readerFor(domain statelog.Domain, spec statelog.StreamSpec,
	appendTo *jetstream.DomainLog, runner *statelog.Runner,
	running *runningLog) (*statelog.Reader, error) {

	encode := barrierEncoder(domain)

	deps := statelog.ReaderDeps{
		Domain: domain,
		Spec:   spec,
		Mode:   s.mode,
		DB:     s.estate().Reader(),
		Waiter: runner,
		// Assembled on every read, since each term can change between
		// reads and a captured value would freeze the refusals this seam
		// delivers; the two remote terms (published floors, log ends) are
		// served from memory, re-read off the read's path once
		// [servingRecheck] old and never past [statelog.FloorCacheStale]
		// or [endsStale] ([stateLog.servedHealth]), or every read would
		// queue on a broker round trip. Under [stateLog.run], since the
		// closure outlives the boot context. An error is a readiness
		// nobody could read (refused `broker_unreachable`); an unrefreshed
		// floor is an aged one (refused `floor_unknown`).
		Health: func() (statelog.Health, error) {
			return s.servedHealth(s.run, running)
		},
		Drain:   runner.Drain,
		Metrics: s.metrics,
	}
	// NO READ INDEX IN A MODE THAT PUBLISHES NOTHING: it is an appender,
	// and the reader refuses a linearizable read there rather than
	// appending (see the mode field).
	if encode != nil && s.mode.Publishes() {
		// Signed like every record, and admitted through the write
		// authority's gate reserve: a barrier bypassing it would spend the
		// room a node gate is promised.
		var admission statelog.Admission
		if running.reserve != nil {
			admission = running.reserve
		}
		index, err := statelog.NewReadIndex(domain, spec, appendTo, appendTo.Members, running.signer, admission, encode,
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

// trimFloor is the published floor for one domain at the caller's generation:
// the first sequence every node must hold. It is [coord.TrimFloor.Floor],
// which never moves down within a generation and is written before the purge
// it licenses — not a minimum over positions (which includes the reader) nor
// TrimTo (zero on a blocked tick), each of which cleared the node a fence
// exists to refuse. A reader that must survive the next trim takes the higher
// of this and the stream's first sequence; one asking whether a replay is
// possible takes the stream's alone ([statelog.FloorReplaying]). Unknown
// refuses: failing open here is a lost update. Read on the write's path, after
// the write's read of its subject (statelog's floor theorem), so never from
// memory: one leader read of the domain's key ([coord.FloorRegister.Floor]).
func (s *stateLog) trimFloor(ctx context.Context, domain string, generation uint32) (uint64, error) {
	return readFloorsOf(ctx, s.fleet, []string{domain}).floor(domain, generation)
}

// publishedFloors is one read of the fleet's published trim floors for every
// domain a caller judges at one instant ([stateLog.judgeCopy], ADR-0079): per
// domain, its row, no row, or why it could not be read. Each domain by its key
// ([readFloorsOf]), never a listing of the register, whose ordered consumer is
// two metadata-group proposals on a clustered broker and timed out under load
// (`BenchmarkAFloorRead`, internal/coord/kv, measures both). A row and its
// failure are one domain's: a bad row or a timed-out request fails that key
// alone, so each domain answers from its own key ([publishedFloors.row]), and
// only a reader needing every domain at once (the join) refuses on any unread.
type publishedFloors struct {
	// domains is each domain the read asked about, and what its key said.
	// A domain the read did not ask about is absent, which is NEVER read as
	// a trim that has never run: [publishedFloors.row] refuses it.
	domains map[string]domainFloor

	// at is when the read landed — what a health built from it stamps its
	// floor with ([statelog.Floor.ReadAt]), since that is the instant the
	// floor was known, however long after it the health was assembled.
	at time.Time
}

// errNoFloorStore is the cause every domain's floor read carries on a node
// handed no coordination store ([readFloorsOf]).
var errNoFloorStore = errors.New("this node has no coordination store to read a " +
	"published floor from")

// domainFloor is what one domain's key answered: its row, no row (the trim
// has never written one for the domain), or why it could not be read.
type domainFloor struct {
	row   coord.TrimFloor
	found bool
	err   error
}

// readFloors reads every registered domain's published floor once, and keeps
// every domain's that answered for the reads served after it
// ([stateLog.servedFloor]).
func (s *stateLog) readFloors(ctx context.Context) publishedFloors {
	read := readFloorsOf(ctx, s.fleet, registeredNames())
	for domain, got := range read.domains {
		if got.err == nil {
			s.floorMemos.of(domain).store(read.only(domain), read.at)
		}
	}
	return read
}

// readFloorsOf reads the published floor of each of domains, one key read a
// domain, all at once: every [publishedFloors] comes from here. Each domain's
// answer is its own. No fleet is every domain unread ([errNoFloorStore]), never
// "no row": a node with no coordination store holds no floor, which is not a
// trim that has never run, and each caller's own unread arm answers.
func readFloorsOf(ctx context.Context, fleet coord.Fleet, domains []string) publishedFloors {
	if fleet == nil {
		out := publishedFloors{domains: make(map[string]domainFloor, len(domains)), at: time.Now()}
		for _, domain := range domains {
			out.domains[domain] = domainFloor{err: fmt.Errorf("engine: read %s's published "+
				"trim floor: %w", domain, errNoFloorStore)}
		}
		return out
	}
	got := make([]domainFloor, len(domains))
	var wg sync.WaitGroup
	for i, domain := range domains {
		wg.Go(func() {
			row, found, err := fleet.Floor(ctx, domain)
			if err != nil {
				err = fmt.Errorf("engine: read %s's published trim floor: %w", domain, err)
			}
			got[i] = domainFloor{row: row, found: found, err: err}
		})
	}
	wg.Wait()
	out := publishedFloors{domains: make(map[string]domainFloor, len(domains)), at: time.Now()}
	for i, domain := range domains {
		out.domains[domain] = got[i]
	}
	return out
}

// row is domain's published row out of this read: the row and true, false
// where the trim has never written one, or the error its key's read failed
// with — and an error for a domain the read never asked about, which is not a
// domain without a floor.
func (p publishedFloors) row(domain string) (coord.TrimFloor, bool, error) {
	got, asked := p.domains[domain]
	if !asked {
		return coord.TrimFloor{}, false, fmt.Errorf("engine: %s's published trim "+
			"floor was not read — a floor nobody asked for is not one the trim has "+
			"never written", domain)
	}
	return got.row, got.found, got.err
}

// floor is domain's floor at generation out of this read ([floorFor]); a key
// nobody could read is the error, which refuses — that domain alone.
func (p publishedFloors) floor(domain string, generation uint32) (uint64, error) {
	row, found, err := p.row(domain)
	if err != nil {
		return 0, err
	}
	return floorFor(row, found, domain, generation)
}

// unread is why every domain this read could not read failed, joined, in the
// order of the domains' names — nil when every key answered. For a reader
// that needs every domain at once, which no single domain's answer can stand
// in for.
func (p publishedFloors) unread() error {
	var errs []error
	for _, domain := range slices.Sorted(maps.Keys(p.domains)) {
		if err := p.domains[domain].err; err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// only is this read narrowed to one domain, as the read memo of that domain
// holds it.
func (p publishedFloors) only(domain string) publishedFloors {
	got, asked := p.domains[domain]
	if !asked {
		return publishedFloors{at: p.at}
	}
	return publishedFloors{domains: map[string]domainFloor{domain: got}, at: p.at}
}

// floorOf is [stateLog.trimFloor] for one domain as a write fence reads it: at
// the generation of the cursor the fence compares (the write's snapshot
// checkpoint, [statelog.Snap.Checkpoint]), not the applier's live one, which an
// adoption can move between snapshot and check into another sequence space. A
// floor published at a higher generation then gets [floorFor]'s refusal.
func (s *stateLog) floorOf(domain string) func(context.Context, uint32) (uint64, error) {
	return func(ctx context.Context, generation uint32) (uint64, error) {
		return s.trimFloor(ctx, domain, generation)
	}
}

// floorFor is the floor's arithmetic over one domain's key, apart from the
// register so every branch is table-tested:
//
//   - No record: the trim has licensed nothing. Zero.
//   - A record at a lower generation names a dead number space. Zero: the
//     stream's first sequence, which readers take beside it
//     ([stateLog.trimFloor]), covers a purge that raced a reanchor, where
//     unknown would refuse every zero-expectation write for a trim interval.
//   - A record at a higher generation: this node is on the dead space. Unknown.
//   - Otherwise the record's Floor, never TrimTo, which is zero while the trim
//     is blocked and falls with the counted minimum.
func floorFor(row coord.TrimFloor, found bool, domain string, generation uint32) (uint64, error) {
	switch {
	case !found, row.Generation < generation:
		return 0, nil
	case row.Generation > generation:
		return 0, fmt.Errorf("engine: the published floor for %s is at generation "+
			"%d and this node is on %d — this node's positions name a sequence "+
			"space the fleet has left, so nothing it holds can be compared "+
			"against the floor", domain, row.Generation, generation)
	}
	return row.Floor, nil
}

// logEndsOf is a domain log's two ends as the write fences read them beside
// [stateLog.trimFloor]. The first end may only raise the floor (a stale answer
// is lower than the truth, [statelog.Health.FirstSeq]); it covers what the
// floor cannot see and tells a node below the log from one replaying to the
// floor. The last end says this node is on this log at all: a checkpoint past
// it is refused. Unreadable refuses. It also hands the stream's identity to
// the runner, which the publisher asks before appending, so a log rebuilt
// since the last heartbeat is refused within a zero-expectation write, where
// letting it through is a lost update ([statelog.Fence.ClearForZero]); the end
// goes to the runner too.
func (s *stateLog) logEndsOf(domain string, l *jetstream.DomainLog,
	runner *statelog.Runner) func(context.Context) (statelog.LogEnds, error) {

	return func(ctx context.Context) (statelog.LogEnds, error) {
		// WHERE THE RUNNER STANDS BEFORE THE READ, which is the only order in
		// which the pair can say anything — see [statelog.Runner.ObserveEnd].
		stance := runner.Stance()
		stats, err := l.Stats(ctx)
		if err != nil {
			return statelog.LogEnds{}, fmt.Errorf("engine: read the log's ends: %w", err)
		}
		s.observeStream(ctx, domain, runner, stance, stats)
		return statelog.LogEnds{First: stats.FirstSeq, Last: stats.LastSeq}, nil
	}
}

// Readmissible is the node gate's judgement of a readmission: whether an
// evicted node has fallen below a floor it would be counted against again,
// asked once per gesture before any log is written ([NodeGate.Readmit]). Here
// because this object holds all three inputs (the positions register, the
// published floors, every log), and each log's bound is read as this node's
// write fence reads it ([stateLog.readmissionBound]): the fence that will hold
// the readmitted node is this one's, since this node writes the readmission.
// Identity-claiming domains only, every one (only they form an expectation of
// zero); each input read once and judged together. Every failed read refuses
// ([ReadmissionUnjudged]): an unlisted register is not one without the node.
func (s *stateLog) Readmissible(ctx context.Context, nodeID string) error {
	unjudged := func(log string, err error) error {
		return &ReadmissionUnjudged{Node: nodeID, Log: log, Err: err}
	}
	if s == nil || s.fleet == nil {
		return unjudged("", errors.New("this node reads no positions register, so it "+
			"cannot tell where the node stands against the trim floor"))
	}
	register, err := s.fleet.Positions(ctx)
	if err != nil {
		return unjudged("", fmt.Errorf("read the positions register: %w", err))
	}
	// EACH LOG'S FLOOR IS ITS OWN, so one that did not read refuses the
	// readmission naming that log ([ReadmissionUnjudged.Log]) rather than
	// the register as a whole.
	floors := s.readFloors(ctx)
	var bounds []statelog.ReadmissionBound
	for _, running := range s.running() {
		if !running.domain.ClaimsIdentity() {
			continue
		}
		bound, err := s.readmissionBound(ctx, running, floors)
		if err != nil {
			return unjudged(running.domain.Name(), fmt.Errorf("read its readmission bound: %w", err))
		}
		bounds = append(bounds, bound)
	}
	permitted := statelog.PermitReadmission(nodeID, register, bounds)
	// A LOG THE FLEET RE-ANCHORED AND THIS NODE HASN'T caught up on is one it
	// cannot judge the readmission on, which is this refusal's condition: told
	// to the operator, with the node to ask instead, rather than answered as
	// a fault with the remedy in this node's log alone.
	var behind *statelog.ReanchorNotApplied
	if errors.As(permitted, &behind) {
		return &ReadmissionUnjudged{Node: nodeID, Log: behind.Domain, Behind: true,
			Err: permitted}
	}
	return permitted
}

// readmissionBound is running's bound as this node's write fence reads it
// ([statelog.ReadmissionBound]): the floor published at the generation this
// node runs the log at, out of floors, and the log's first surviving sequence.
func (s *stateLog) readmissionBound(ctx context.Context, running *runningLog,
	floors publishedFloors) (statelog.ReadmissionBound, error) {

	generation := running.runner.Committed().Generation
	floor, err := floors.floor(running.domain.Name(), generation)
	if err != nil {
		return statelog.ReadmissionBound{}, err
	}
	first, _, err := running.log.Bounds(ctx)
	if err != nil {
		return statelog.ReadmissionBound{}, fmt.Errorf("engine: read %s's first "+
			"surviving sequence: %w", running.domain.Name(), err)
	}
	return statelog.ReadmissionBound{
		Domain: running.domain.Name(), Generation: generation, Floor: floor, First: first,
	}, nil
}

// observeStream hands one live reading of a domain log's state to its runner
// (the creation instant, and the end paired with the [statelog.Runner.Stance]
// read before the broker was asked) and names what it changed. One place for
// the heartbeat and the zero fence alike: whichever reads first establishes a
// finding, so the line is written by whoever got there.
func (s *stateLog) observeStream(ctx context.Context, domain string,
	runner *statelog.Runner, at statelog.Stance, stats jetstream.LogStats) {

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

// observeEnd hands one reading of a domain log's end to its runner, paired with
// where it stood before the read (its checkpoint, and the barriers its loop
// reached past it, which an end below them sends back unapplied,
// [statelog.Runner.ObserveEnd]), and names both transitions. The end reaching
// the checkpoint again is either a stale member corrected or a restored log
// written past it in another history; the end cannot tell, but the applier
// compares the record at the checkpoint before applying past it
// ([statelog.Runner.VerifyCheckpoint]), so the latter becomes
// `statelog_log_diverged`, a refusal that does not lift.
func (s *stateLog) observeEnd(ctx context.Context, domain string,
	runner *statelog.Runner, at statelog.Stance, last uint64) {

	established, reached := runner.ObserveEnd(at, last)
	switch {
	case established:
		log.ErrorContext(ctx, "statelog_ahead_of_log",
			"node", s.nodeID, "domain", domain,
			"checkpoint", at.At.Seq, "last_seq", last,
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
			"rows and another record has since been written at their checkpoint's "+
			"sequence, so the log continues a history they are not. This node "+
			"applies nothing past its checkpoint, remembers the finding across a "+
			"restart, "+
			"refuses every read and every write of the domain naming "+
			"`wrong_stream`, and donates no snapshot of it; an operator re-anchors "+
			"it (crewlet retention reanchor — the restored case) or replaces its "+
			"rows with a peer's")
}

// Established reports whether every domain whose health gates seat admission
// ([statelog.Domain.GatesSeatAdmission]) has one. Per domain, from its own
// declaration: a compacted domain's gap is a coverage number, and withholding
// seats over it would be the outage the number avoids. Admission only; whether
// a copy is WRONG is [stateLog.judgeCopy]'s (ADR-0079).
func (s *stateLog) Established(ctx context.Context, strict bool) (bool, statelog.ReadRefusal) {
	if s == nil {
		return true, ""
	}
	gating := slices.DeleteFunc(s.running(), func(running *runningLog) bool {
		return !running.domain.GatesSeatAdmission()
	})
	for _, read := range s.readHealths(ctx, gating) {
		if read.err != nil {
			return false, statelog.RefuseBrokerUnreachable
		}
		if ok, refusal := read.health.Established(strict); !ok {
			return false, refusal
		}
	}
	return true, ""
}

// healthRead is one domain's health as one reading assembled it, or why it
// could not be.
type healthRead struct {
	running *runningLog
	health  statelog.Health
	err     error
}

// readHealths reads every one of runs' health AT ONCE, beside ONE read of the
// published floors ([stateLog.readFloors]) — in runs' order.
func (s *stateLog) readHealths(ctx context.Context, runs []*runningLog) []healthRead {
	return readHealthsWith(ctx, runs, s.readFloors, s.observe)
}

// readHealthsWith is [stateLog.readHealths] over its two reads, separated so
// the concurrency is exercised without a broker. Each log is its own raft
// group, so logs are read at once: in series under one deadline, one
// leaderless group spent it and left every later log unread, hiding their
// faults (ADR-0079's isolation). The floors are read beside the logs, since a
// health needs its floor only at the end ([floored]), and every health is
// built from that one read ([readFloorsOf]).
func readHealthsWith(ctx context.Context, runs []*runningLog,
	readFloors func(context.Context) publishedFloors,
	observe func(context.Context, *runningLog) (statelog.Health, error)) []healthRead {

	floors := make(chan publishedFloors, 1)
	go func() { floors <- readFloors(ctx) }()
	out := make([]healthRead, len(runs))
	var wg sync.WaitGroup
	for i, running := range runs {
		wg.Go(func() {
			// EACH INTO ITS OWN SLOT, so one log's answer — or its
			// silence — touches no other's.
			h, err := observe(ctx, running)
			out[i] = healthRead{running: running, health: h, err: err}
		})
	}
	wg.Wait()
	read := <-floors
	for i := range out {
		if out[i].err != nil {
			continue
		}
		out[i].health, out[i].err = floored(out[i].health, out[i].running.domain.Name(), read)
	}
	return out
}

// judgeCopy judges this node's copy of every registered domain (ADR-0079) from
// one reading of each log's health and one read of the published floors, all
// at once ([stateLog.readHealths]). A wrong copy is taken out of service for
// that domain's operations alone ([localEstate.For]); every other domain keeps
// answering here. Every domain is judged whatever it declares:
// [statelog.Domain.GatesSeatAdmission] decides only the seat copy
// ([estate.SeatCopy]). WRONG is rows that will not become right by applying
// more ([statelog.Health.Fault]: stopped, evicted, below the log, wrong stream,
// stalled, an undecodable record past [statelog.DeferralGrace]); being behind
// is admission's business ([stateLog.Established]) and fires nothing here. A
// health nobody could read is UNREAD, not wrong, and keeps its last verdict
// ([copyVerdict.carried]): an unreachable broker is when copies must answer.
func (s *stateLog) judgeCopy(ctx context.Context) copyVerdict {
	out := copyVerdict{domains: map[string]domainVerdict{}}
	if s == nil {
		// NO STATE LOG, NOTHING TO JUDGE: every domain this build
		// registers is sound and answering, which is what a runtime with
		// no log to fall behind on is.
		for _, domain := range registeredDomains() {
			out.domains[domain.Name()] = domainVerdict{answers: true,
				gates: domain.GatesSeatAdmission()}
		}
		return out
	}
	// A file that is not open cannot answer at all (lost to a rejoin that
	// could not reopen it, or between an adoption's rename and reopen), and
	// with its appliers halted and nothing written it owes no progress, so
	// nothing below would call it wrong: shut sends its calls to a peer.
	if s.db != nil {
		if _, err := s.db.ReplicatedDB(); err != nil {
			return copyVerdict{shut: true}
		}
	}
	return judgeReads(time.Now(), s.readHealths(ctx, s.running()))
}

// judgeReads is each domain's verdict out of one reading of the logs' health
// ([stateLog.readHealths]) at now — a domain whose health was not read is
// UNREAD, for [stateLog.judgeCopy]'s reason, and on its own.
func judgeReads(now time.Time, reads []healthRead) copyVerdict {
	out := copyVerdict{domains: make(map[string]domainVerdict, len(reads))}
	for _, read := range reads {
		running := read.running
		d := domainVerdict{gates: running.domain.GatesSeatAdmission()}
		if read.err != nil {
			d.unread = true
		} else {
			d.why = read.health.Fault(now, running.progress.deferredSinceValue())
			d.wrong = d.why != ""
			d.answers, _ = read.health.Answers()
		}
		out.domains[running.domain.Name()] = d
	}
	return out
}

// health assembles one domain's readiness from the four places it lives: this
// node's own checkpoint, its consumer's backlog, the stream's own bounds, and
// the fleet's published floor — out of floors, the caller's one read of the
// register for every domain it asks about at this instant
// ([stateLog.readFloors]).
func (s *stateLog) health(ctx context.Context, running *runningLog,
	floors publishedFloors) (statelog.Health, error) {
	health, err := s.observe(ctx, running)
	if err != nil {
		return health, err
	}
	return floored(health, running.domain.Name(), floors)
}

// observe is every term of one domain's health but its floor: the checkpoint,
// the applier's own state, the eviction row and the stream's bounds, read now
// — the half a reading can take before the published floors land
// ([readHealthsWith]). [floored] completes it.
func (s *stateLog) observe(ctx context.Context, running *runningLog) (statelog.Health, error) {
	return s.observeWith(ctx, running, func(ctx context.Context,
		at statelog.Position) (streamEnds, error) {
		return freshEnds(ctx, running, at)
	})
}

// observeWith is [stateLog.observe] with the log's ends from endsOf (read now,
// or served from memory, [servedEnds]), handed the position this health
// describes: the runner's reached position ([statelog.Runner.Reached]), never
// its committed checkpoint, since a read is served from rows current through
// it and a reached, not yet checkpointed barrier changes none of them. What is
// durable stays the checkpoint ([stateLog.publishPositions]). Ends from memory
// may predate this position: if the position they were read beside was not
// past them, the end is at least this position (the applier read those
// records from the log); if it was past them, the node was ahead of its log
// and stays so.
func (s *stateLog) observeWith(ctx context.Context, running *runningLog,
	endsOf func(context.Context, statelog.Position) (streamEnds, error)) (statelog.Health, error) {
	now := time.Now()
	at := running.runner.Reached()
	health := statelog.Health{Position: at, AppliedThrough: at.Seq}
	// The applier's own stop, first: a halted applier's rows are frozen, so
	// no read over them can be certified ([statelog.Health.Refusal]'s
	// `stalled` arm). A stop this process asked for is not a fault: every
	// applier returns its context's error on shutdown, and reading that as a
	// halt would refuse reads and take the copy out of service mid-drain.
	if err := running.runner.Stopped(); err != nil && !errors.Is(err, context.Canceled) {
		health.Err = err.Error()
	}
	// A fault past its budget is the same report: the applier still retries,
	// but the rows have not moved and a read is old in a way lag cannot show.
	// Inside the budget nothing is reported, so a broker blip moves no seats.
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
	// The eviction row, from the write fence's own reader: an evicted node's
	// peers drop its records, so its rows stop advancing while its position
	// is still published, and a read from them serves an abandoned copy.
	if running.evicted != nil {
		evicted, err := running.evicted(ctx)
		if err != nil {
			return health, err
		}
		health.Evicted = evicted
	}
	ends, err := endsOf(ctx, at)
	if err != nil {
		// Neither half of the inequality can be evaluated, and both fail
		// silently: a consumer above the last sequence waits for ever, one
		// below the first is clamped up and reports caught up over a hole.
		return health, err
	}
	first, end := ends.first, ends.last
	if ends.before.Seq <= end && at.Seq > end {
		end = at.Seq
	}
	health.FirstSeq = &first
	// THE END ITSELF, beside the lag derived from it: the lag is clamped
	// at zero, so a checkpoint PAST the end reads as caught up through
	// it, and the end is what [statelog.Health.AheadOfLog] refuses on.
	health.LastSeq = &end
	// And whether this is the same stream at all: observed, not derived, and
	// from the runner, where the write path asks it, so reads and writes
	// refuse together. Its recreation half only: a checkpoint past the end is
	// decided above from this health's own end, or a broker back from an
	// older copy would be named a rebuild.
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
	// Drained is the applier's record of having reached its log's end at
	// least once, a property of a series, not of the lag beside it (deriving
	// it from the lag released every seat on each new work item). A stall
	// clears it: frozen rows stop being a whole state.
	health.Drained = running.runner.Drained() && !health.Stalled
	return health, nil
}

// floored completes an [stateLog.observe]d health with domain's floor out of
// floors: the published floor, and the four-valued state its checkpoint is in
// against it and against the stream's first record.
func floored(health statelog.Health, domain string, floors publishedFloors) (statelog.Health, error) {
	at := health.Position
	floor, err := floors.floor(domain, at.Generation)
	if err != nil {
		return health, err
	}
	first := uint64(0)
	if health.FirstSeq != nil {
		first = *health.FirstSeq
	}
	health.TrimFloor = &floor
	// The four-valued floor, stamped with the instant it was read, since a
	// state whose age nobody carries ages silently into an assertion
	// ([statelog.Floor]). FirstSeq may only raise it: a stale member's answer
	// is lower than the truth.
	health.Floor = statelog.Floor{State: statelog.FloorOK, ReadAt: floors.at}
	// Below means the next record this node needs (the one after the
	// position its rows are current through) may be gone; the heartbeat asks
	// the same of the committed checkpoint ([stateLog.publishPositions]).
	// Which below is asked of `first` alone, since a floor can be published
	// and its purge fail: under a floor above the log's first record, a node
	// whose next record survives is REPLAYING (`behind`, clearing on its
	// own), and only one whose next record is gone is BELOW, adopting a
	// snapshot. Every reader shares the predicate ([statelog.Replayable]) and
	// chooses only its bound ([stateLog.trimFloor]).
	switch {
	case !statelog.Replayable(at.Seq, first):
		health.Floor.State = statelog.FloorBelow
	case !statelog.Replayable(at.Seq, max(floor, first)):
		health.Floor.State = statelog.FloorReplaying
	}
	return health, nil
}

// applierFor is the state machine each domain declares, out of the register:
// on the entry, not the domain, because the declaration is read by snapshots,
// claims, sweeps and the trim and must be answerable by a build that cannot
// construct the applier. A domain without one fails the boot check by name.
func (s *stateLog) applierFor(domain statelog.Domain) (statelog.Applier, error) {
	entry, found := registrationFor(domain.Name())
	if !found || entry.NewApplier == nil {
		return nil, fmt.Errorf("engine: domain %q is registered and has no applier, "+
			"so its records would be consumed and produce no rows on this node",
			domain.Name())
	}
	applier, err := entry.NewApplier(s)
	if err != nil {
		return nil, fmt.Errorf("engine: build %s's applier: %w", domain.Name(), err)
	}
	return applier, nil
}

// Epoch is the per-epoch configuration every registered domain's applier
// reads, as a plain map. An applier is a pure function of (rows, record,
// options), so two nodes applying one record produce the same rows; the
// framework fills this once per apply-loop generation, which is why the one
// table depending on it is classed DIVERGING. Keys are the company settings'
// own paths ([inbox.RetentionKey]), so one setting is one key. The horizon is
// stated with its default applied ([config.Inbox.Days]), so the applier and
// the sweep ([Engine.inboxRetention]) read one horizon. A nil company
// declares none, and every applier applies its own default.
func (c *Company) Epoch() map[string]any {
	if c == nil || c.Config == nil {
		return nil
	}
	return map[string]any{inbox.RetentionKey: c.Config.Inbox.Days()}
}

// applyEpoch is what every domain's applier reads per batch: the epoch of the
// company this node has PUBLISHED (one atomic load), never the revision being
// built, which a refused apply never makes this node's company. A node with no
// company hands its appliers none.
func (e *Engine) applyEpoch() map[string]any { return e.Company().Epoch() }

// join adopts a peer's snapshot when this node cannot replay its way back,
// for the boot and for [Engine.rejoin]. Per domain: is the record after the
// checkpoint still on the log ([statelog.Replayable])? Any domain that cannot
// joins them all, since a snapshot names every registered domain. A fresh
// node's next record is the first, so it asks nobody, sparing the common case
// a round trip that refuses reads for up to [statelog.OfferWindow]. No offer,
// or a fault of this node's own before install ([statelog.ErrJoinerFault]),
// leaves it on its intact rows: it comes up gated by seat admission and
// retries on a widening interval, rather than failing to start. An unrecorded
// install ([statelog.ErrAdoptionUnrecorded]) is an adoption, the rename being
// the point of no return; a lost estate ([statelog.ErrEstateNotRestored])
// stops a boot, and a running node reopens it ([stateLog.restoreEstate]).
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
		LivePath: e.backends.Store.ReplicatedFile(),
		NodeID:   s.nodeID,
		Conn:     conn.Conn(),
		// Every node that can donate ([Engine.joinExpectation]): the join
		// stops collecting once each has answered, not at the offer
		// window.
		Donors: e.joinExpectation(s),
		Need: func(context.Context) (statelog.OfferRequest, error) {
			return want, nil
		},
		StillUsable: func(ctx context.Context, m statelog.Manifest) error {
			return s.stillUsable(ctx, logs, m)
		},
		Hold:  s.holdTail,
		Close: func(context.Context) error { return e.backends.Store.CloseReplicated() },
		Reopen: func(ctx context.Context) error {
			if _, reopenErr := e.backends.Store.OpenReplicated(ctx, estateLogs()); reopenErr != nil {
				return reopenErr
			}
			return nil
		},
		Record: func(ctx context.Context, began time.Time, donor string,
			m statelog.Manifest, phase statelog.AdoptionPhase) error {

			return statelog.RecordAdoption(ctx, e.backends.Store, began,
				donor, m, phase)
		},
	})
	if err != nil {
		return "", statelog.OfferRequest{}, err
	}
	adoption, err := adopter.Join(ctx)
	// AN INSTALLED ARTEFACT'S PIN OUTLIVES THE JOIN, whatever came beside
	// it: the tail it pins is the one this node is about to replay, and
	// nothing in the fleet counts that until a row of this node's own does
	// — see [stateLog.holdUntilCounted].
	if adoption.Release != nil {
		s.holdUntilCounted(adoption.Release)
	}
	switch {
	case errors.Is(err, statelog.ErrNoOffer):
		// Not a failure (see the doc comment). The error carries each
		// donor's refusal, which says why nobody could donate.
		log.WarnContext(ctx, "statelog_no_snapshot_offered",
			"node", s.nodeID, "domains", slices.Concat(behind, passed),
			// And why each peer was turned down: the commonest cause, a
			// donor running fewer domains, is permanent.
			"refusals", err.Error(),
			"detail", "no peer could donate a usable snapshot, so this node "+
				"comes up on the history it has; reads report the coverage "+
				"they could not account for, the domains that gate seat "+
				"admission keep it from claiming work it cannot answer for, "+
				"and it asks again on a widening interval")
		return joinNoDonor, want, nil
	case errors.Is(err, statelog.ErrAdoptionUnrecorded):
		// ADOPTED — see the doc comment. The adopter has written both
		// lines this needs: `statelog_adopted`, and the error naming the
		// history row it could not advance.
		return joinAdopted, want, nil
	case errors.Is(err, statelog.ErrJoinerFault):
		// BEHIND, AND THIS NODE'S OWN FAULT — see the doc comment. At
		// error rather than warn, unlike nobody donating: an operator
		// may have something to fix (a full volume, an unwritable
		// directory), and the line names it.
		log.ErrorContext(ctx, "statelog_join_unable",
			"node", s.nodeID, "domains", slices.Concat(behind, passed),
			"error", err.Error(),
			"detail", "this node is behind and could not adopt a peer's snapshot "+
				"for a fault of its own, before anything was installed — no donor "+
				"refused it; it comes up on the history it has, as when nobody "+
				"could donate, and asks again on a widening interval, meeting the "+
				"same fault until what the error names is fixed")
		return joinUnable, want, nil
	case err != nil:
		return "", statelog.OfferRequest{}, fmt.Errorf("engine: this node cannot "+
			"replay its way to the fleet on %s and the join failed: %w",
			words.And(slices.Concat(behind, passed)), err)
	}
	// NO LINE OF ITS OWN: the adopter writes `statelog_adopted` with the
	// donor, the artefact's checksum and when it was taken, and a second
	// line here made one adoption read as two.
	return joinAdopted, want, nil
}

// joinOutcome is what a join concluded, for the caller that acts on the
// difference. Four answers, since "adopted nothing" is three facts to a running
// node: current (nothing asked, but [Engine.rejoin] still re-keys, since the
// file may be an artefact an earlier rejoin installed and
// [stateLog.restoreEstate] opened since), no donor, or a fault of its own. The
// last two are acted on alike (rows untouched) and differ only in naming whose
// fault it was: an operator reads the donors' refusals for one and fixes this
// node for the other. Never read off the wire, so it has no Valid.
type joinOutcome string

const (
	// joinCurrent — every domain can replay from where its rows say it
	// is, so the fleet was not asked.
	joinCurrent joinOutcome = "current"

	// joinAdopted — a peer's snapshot is installed and open: complete, or
	// complete in everything but this node's history row of it
	// ([statelog.ErrAdoptionUnrecorded]).
	joinAdopted joinOutcome = "adopted"

	// joinNoDonor — this node is behind and no peer could donate, or it
	// has no broker connection to ask one over.
	joinNoDonor joinOutcome = "no_donor"

	// joinUnable — this node is behind and could not adopt a snapshot, for
	// a fault of its own before anything was installed
	// ([statelog.ErrJoinerFault]). Its rows are untouched, so it stands
	// exactly where [joinNoDonor] leaves a node.
	joinUnable joinOutcome = "unable"
)

// joinExpectation is who a join on this node waits to hear from
// ([statelog.AdoptDeps.Donors]): the live data nodes listed now
// ([Engine.holdersOf]) and the positions register ([joinDonors]). Listed, not
// the watched view, which may not have seen a donor yet; both are read inside
// the offer window, and a failed read leaves the window to decide rather than
// end the collection before a donor's offer.
func (e *Engine) joinExpectation(s *stateLog) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		live, err := e.holdersOf().LiveData(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the live data nodes: %w", err)
		}
		// NO REGISTER, NO ROWS: nothing on this node publishes one, so
		// there is nobody a row could add.
		var rows []coord.NodePositions
		if s.fleet != nil {
			if rows, err = s.fleet.Positions(ctx); err != nil {
				return nil, fmt.Errorf("read the positions register: %w", err)
			}
		}
		own := s.donor.Load()
		return joinDonors(time.Now(), s.nodeID, own != nil && own.Serving(), live, rows), nil
	}
}

// joinDonors is every node a join on node own waits to hear from, a superset
// of every node that can offer, since the collection ends on the last one
// named ([statelog.CollectOffers]). Sorted, so one fleet is one expectation:
//
//   - every live data node, each running a donor (its row may lag a beat);
//   - every node whose fresh row names an artefact ([donorRowFresh]), whatever
//     its presence: a draining node gives presence up first but serves its
//     donor until its state log stops, and in a rolling upgrade it likely
//     holds the newest artefact;
//   - this node only when its own donor serves (ownServing): the boot's join
//     runs before any donor starts and would wait the whole window on itself.
func joinDonors(now time.Time, own string, ownServing bool,
	live []statelog.Presence, rows []coord.NodePositions) []string {

	named := make(map[string]bool, len(live)+len(rows)+1)
	for _, p := range live {
		named[p.NodeID] = true
	}
	for _, row := range rows {
		if now.Sub(row.At) <= donorRowFresh && namesArtefact(row) {
			named[row.NodeID] = true
		}
	}
	if ownServing {
		named[own] = true
	} else {
		delete(named, own)
	}
	return slices.Sorted(maps.Keys(named))
}

// donorRowFresh is how recently a node must have written its register row for
// a join to wait on the artefact it names ([joinDonors]): four heartbeats, as
// [statelog.TrimHoldStale] derives, since a node may miss three and still run
// its donor. Waiting on a node just dead costs the [statelog.OfferWindow];
// leaving out a running one costs its offer, which may be the only one.
const donorRowFresh = 4 * statelog.PositionHeartbeat

// namesArtefact reports whether a register row says its node holds an
// artefact of the estate ([stampSnapshot]): a size on the row, or a snapshot
// instant on any log's entry — the instant rather than the sequence, because
// an artefact taken while a log was still empty covers it at position zero.
func namesArtefact(row coord.NodePositions) bool {
	if row.SnapshotBytes > 0 {
		return true
	}
	for _, pos := range row.Domains {
		if !pos.SnapshotAt.IsZero() {
			return true
		}
	}
	return false
}

// rejoin is the runtime adoption, over an open estate ([stateLog.requestRejoin]
// reopens a lost one first with [stateLog.restoreEstate]). The order:
//
//  1. The appliers end: each pins a connection on the file to be replaced.
//     Their waiters are told, so a read in flight refuses `behind`.
//  2. The boot's own join ([Engine.join]).
//  3. Each runner is re-keyed to the file's checkpoint
//     ([statelog.Runner.Rejoined]) and its consumer reset there
//     ([stateLog.resetConsumers]), even after a CURRENT join: the file may be
//     an artefact an earlier rejoin installed and could not open.
//  4. The appliers start again whatever happened: a node with none is worse
//     than one below the floor.

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
	// replaced is whether the join may have put a donor's rows in place,
	// which no committed batch will ever say — see [stateLog.estateReplaced].
	replaced := false
	// ctx is the state log's own run context ([stateLog.requestRejoin]
	// starts this under it), so the relaunched appliers get the boot
	// launch's lifetime. On every exit, failures included: a rejoin that
	// found no donor changed no rows.
	defer func() {
		s.launchAppliers(ctx)
		if replaced {
			s.estateReplaced()
		}
	}()

	outcome, want, err := e.join(ctx, s, s.openLogs())
	switch {
	case err != nil:
		// Only a join that may have put another file in place, or left none
		// open, announces the rows replaced: [statelog.ErrEstateNotRestored],
		// the one error beside an installed artefact (an unrecorded one is
		// [joinAdopted]) or after a rollback that could not reopen. Every
		// other failure left the live file open, and announcing it would
		// close every socket and reset every artifact room on each retry.
		// Said here, not left to [stateLog.restoreEstate], a heartbeat later
		// at the soonest, while a socket the donor's rows ended streams on.
		replaced = errors.Is(err, statelog.ErrEstateNotRestored)
		return err
	case outcome == joinNoDonor:
		return errNoDonor
	case outcome == joinUnable:
		// NOTHING WAS INSTALLED and the live file was never replaced, so
		// nothing is announced: as [joinNoDonor], the node is where it
		// was, and the retry widens.
		return errJoinUnable
	}
	// Against the instant the join judged the artefact by (the live one the
	// adopter checked the offer against), not a fresh read that could fail
	// and leave a runner keyed to the replaced history.
	s.resetConsumers(ctx, func(name string) (time.Time, error) {
		return want.StreamCreatedAt[name], nil
	})
	if outcome == joinAdopted {
		replaced = true
		s.nudgeSnapshot()
	}
	log.InfoContext(ctx, "statelog_rejoined", "node", s.nodeID, "outcome", string(outcome))
	return nil
}

// restoreEstate reopens the replicated estate a failed join left closed, and
// moves every consumer to the checkpoint the opened file keeps, with the
// appliers halted around it as around a join. The consumers move because the
// file may be the donor's, from an artefact installed but not opened, which no
// consumer was moved to and, when current, no rejoin follows. The adoption row
// stays incomplete: nothing on the write path reads it, since each file
// carries its own ledger and watermark ([statelog.Rows.LostBefore]). A failed
// reopen is [statelog.ErrEstateNotRestored], like every outcome leaving no
// replicated database.
func (s *stateLog) restoreEstate(ctx context.Context) error {
	// One recovery at a time: a reanchor between this reopen and the re-key
	// below would be re-keyed back and see its halted applier relaunched.
	s.recovering.Lock()
	defer s.recovering.Unlock()
	s.haltAppliers()
	// ctx is the state log's run context, for [Engine.rejoin]'s reason. A
	// file that opened is announced replaced once the appliers run: it may
	// be the donor's ([stateLog.estateReplaced]).
	reopened := false
	defer func() {
		s.launchAppliers(ctx)
		if reopened {
			s.estateReplaced()
		}
	}()
	if err := s.reopenEstate(ctx); err != nil {
		return fmt.Errorf("%w: %w", statelog.ErrEstateNotRestored, err)
	}
	reopened = true
	log.WarnContext(ctx, "statelog_estate_restored", "node", s.nodeID,
		"path", s.db.ReplicatedFile(),
		"detail", "the replicated database an earlier join left closed is "+
			"open again; the node asks the fleet for a snapshot only if it is "+
			"still below the floor, and no sooner than its retry interval allows")
	// No join ran, so each runner is re-keyed against a fresh read of its
	// live stream; one unreadable is re-derived by its loop
	// ([statelog.Runner.Run]), and a verdict left wrong is named by the next
	// heartbeat ([statelog.Runner.RecreationStale]).
	s.resetConsumers(ctx, func(name string) (time.Time, error) {
		stats, err := s.Domain(name).log.Stats(ctx)
		if err != nil {
			return time.Time{}, err
		}
		return stats.CreatedAt, nil
	})
	// And the snapshot loop is woken, as after an adoption: the file may be
	// at a generation this node's artefact does not name
	// ([stateLog.snapshotNudge]); otherwise its gate declines.
	s.nudgeSnapshot()
	return nil
}

// resetConsumers re-keys every domain's runner to the checkpoint the
// replicated file now keeps ([statelog.Runner.Rejoined], against the instant
// live names) and moves its consumer there: a runner keyed to the replaced
// history stops again on it, and the broker will not move a consumer's start,
// so one left behind delivers every record between to be dropped. A failure
// is a line, not a failed call. The consumer: correctness is the checkpoint's,
// so one left behind costs redeliveries, and [jetstream.DomainConsumer]
// rebuilds on its next fetch a handle whose reset deleted and failed to
// create. The runner: an unrun re-key leaves the verdict to its relaunched
// loop ([statelog.Runner.Run]), and a stream it never read is named by the
// next heartbeat ([statelog.Runner.RecreationStale]). Neither strands a node.
func (s *stateLog) resetConsumers(ctx context.Context, live func(name string) (time.Time, error)) {
	for _, running := range s.running() {
		name := running.domain.Name()
		at, keyed, _, err := statelog.CursorFor(ctx, s.estate().Reader(), running.spec.Name)
		if err == nil {
			// The runner first, against the live instant the caller judged
			// the file by, or it stops again on the checkpoint it now holds.
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

// requestRejoin is what the heartbeat calls on a node below the floor (a node
// with no estate reads as below: only a join closes it, and nothing moves the
// runner's in-memory position meanwhile). It reopens a lost estate on the next
// heartbeat whatever the interval says, since that asks nobody and until it
// opens the node serves no replicated read; a reopen moves no schedule. Asking
// the fleet costs an offer window of refusals and maybe a whole transfer, so
// it runs one attempt at a time and every failed ask widens the interval, one
// that lost the estate included. A lost estate is logged at ERROR, naming the
// file, as the one outcome an operator may fix by hand.
func (s *stateLog) requestRejoin(now time.Time) {
	s.rejoinMu.Lock()
	defer s.rejoinMu.Unlock()
	if s.rejoin == nil || s.rejoining {
		return
	}
	lost := !s.estateOpen()
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
				s.rejoinPause = statelog.PositionHeartbeat
			} else {
				s.rejoinPause = min(s.rejoinPause*2, RejoinRetryCeiling)
			}
			s.rejoinAfter = time.Now().Add(s.rejoinPause)
		}
		if errors.Is(err, statelog.ErrEstateNotRestored) {
			log.ErrorContext(s.run, "statelog_estate_lost",
				"node", s.nodeID, "path", s.db.ReplicatedFile(), "error", err.Error(),
				"asks_fleet_again_in", max(0, time.Until(s.rejoinAfter)).Round(time.Second),
				"detail", "this node has no replicated database open, so it "+
					"answers no Worklet, Doclet, usage or search read from its own copy "+
					"and applies no record — its seats read the estate from the "+
					"other data nodes meanwhile; it reopens the file on its next "+
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
// the log from where its rows say it is, and what it would need if not. Two
// lists, since an operator reads two incidents: BEHIND (the record after the
// checkpoint is gone) and PASSED (a peer re-anchored past this node's
// generation, [passedByAReanchor]), each repaired by the same adoption. Need
// is one below the floor per domain, the artefact's acceptance test, with its
// generation: a bare sequence from before a reanchor names a dead number
// space, and for a passed domain it is the fleet's generation.
func (s *stateLog) replayable(ctx context.Context, logs map[string]*jetstream.DomainLog) (
	behind, passed []string, want statelog.OfferRequest, err error) {

	want = statelog.OfferRequest{
		Need:            map[string]uint64{},
		Generations:     map[string]uint32{},
		StreamCreatedAt: map[string]time.Time{},
	}
	// WHERE EACH LOG'S ROWS STAND, read first: the fleet read below asks
	// whether a node is evicted only of one ahead of these, so a fleet on
	// one generation asks nothing of the logs.
	above := map[string]uint32{}
	for _, domain := range registeredDomains() {
		at, _, found, readErr := statelog.CursorFor(ctx, s.estate().Reader(), domain.Stream().Name)
		if readErr != nil {
			return nil, nil, statelog.OfferRequest{}, readErr
		}
		if found {
			above[domain.Name()] = at.Generation
		}
	}
	// The fleet's generations and floors, read once for every domain, so all
	// are judged against one answer. Not ordered after any subject read, as
	// this is not a write's fence, so the floor theorem asks no second read.
	rows, err := s.fleet.Positions(ctx)
	if err != nil {
		return nil, nil, statelog.OfferRequest{}, fmt.Errorf("engine: read the "+
			"fleet's published positions to establish which generation each "+
			"domain is on: %w", err)
	}
	floors := s.readFloors(ctx)
	fleet, unread := s.fleetGenerations(ctx, rows, floors, logs, above)
	// EVERY DOMAIN AT ONCE: an artefact covers them all, so a join with any
	// domain unanswered cannot say what to ask for.
	for _, name := range registeredNames() {
		if err := unread[name]; err != nil {
			return nil, nil, statelog.OfferRequest{}, err
		}
	}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		appendTo, held := logs[name]
		if !held {
			return nil, nil, statelog.OfferRequest{}, fmt.Errorf("engine: the %s "+
				"log was not provisioned before the join asked what it holds", name)
		}
		// One round trip for every term the stream answers, so they describe
		// one moment: the first sequence (behind?) and the creation instant
		// (on which stream?) read separately can straddle a rebuild.
		stats, err := appendTo.Stats(ctx)
		if err != nil {
			return nil, nil, statelog.OfferRequest{}, err
		}
		first := stats.FirstSeq
		at, _, found, err := statelog.CursorFor(ctx, s.estate().Reader(), domain.Stream().Name)
		if err != nil {
			return nil, nil, statelog.OfferRequest{}, err
		}
		// A node with no checkpoint is at no generation, not generation 0:
		// in a fleet that has re-anchored, reading it as 0 would replay from
		// sequence 1 at the wrong generation, report caught up over a partial
		// database, block the trim, and fail its own boot on the floor
		// comparison, terminally. So the generation comes from the fleet, and
		// such a node adopts, as does one whose rows the fleet has passed.
		generation := at.Generation
		reanchored := passedByAReanchor(domain, at, found, fleet[name])
		if !found || reanchored {
			generation = fleet[name]
		}
		floor, err := floors.floor(name, generation)
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

		// What an artefact must cover (set above) is one below the higher of
		// the stream's first surviving sequence (gone already) and the
		// published floor (licensed for deletion, maybe not yet reached): an
		// artefact chosen from `first` alone could be one the trim was about
		// to pass. One below is the lowest checkpoint [statelog.Replayable]
		// accepts, sent as a number since the donor has no view of this stream.

		// Whether this node is behind is asked of `first` alone: the floor
		// says what MAY be deleted, only the stream what IS, and a node
		// between the two can replay what it lacks (judging it behind would
		// ask the fleet for records it could read; alone, a snapshot of
		// itself). [statelog.Replayable] spells the zero cases: a
		// never-written stream reports 0, a purge-emptied one one past its
		// last.
		if !statelog.Replayable(at.Seq, first) {
			behind = append(behind, name)
		}
	}
	return behind, passed, want, nil
}

// passedByAReanchor reports a domain a peer has re-anchored past this node's
// rows: below the fleet's generation, on a domain whose nodes must hold
// identical rows. One predicate for the join and the heartbeat that requests
// one, or a rejoin is requested that adopts nothing, on every retry. The
// generation alone decides: a reanchor opens its generation from one node's
// rows, so no other node's rows are that history, and on a broker restored
// from an older copy the instant shows nothing. Never for a domain that claims
// no identity, whose nodes re-anchor their copies one at a time
// ([statelog.PermitReanchor]), nor for a node with no checkpoint, which the
// join already sends to adopt.
func passedByAReanchor(domain statelog.Domain, at statelog.Position, found bool,
	fleet uint32) bool {

	return domain.ClaimsIdentity() && found && fleet > at.Generation
}

// stillUsable re-checks after the transfer that every position the artefact
// carries is still above the floor: the hold is the mechanism and this the
// belt, against a hold that went stale or a hand purge, and only before the
// install can this node refuse. Every domain at once (floors and each log's
// state concurrently), costing the slowest round trip, not the sum.
func (s *stateLog) stillUsable(ctx context.Context, logs map[string]*jetstream.DomainLog,
	m statelog.Manifest) error {

	names := registeredNames()
	for _, name := range names {
		if _, named := m.Domains[name]; !named {
			return fmt.Errorf("the artefact names no position for %s", name)
		}
	}
	floors := make(chan publishedFloors, 1)
	go func() { floors <- s.readFloors(ctx) }()
	stats := make([]jetstream.LogStats, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { stats[i], errs[i] = logs[name].Stats(ctx) })
	}
	wg.Wait()
	read := <-floors
	for i, name := range names {
		at := m.Domains[name]
		if errs[i] != nil {
			return errs[i]
		}
		first := stats[i].FirstSeq
		floor, err := read.floor(name, at.Generation)
		if err != nil {
			return err
		}
		// The identity is re-checked too: a rebuild during the transfer
		// leaves an artefact naming a history this stream lacks, which a
		// generation cannot see (a rebuilt stream restarts at 0, from 1).
		if statelog.IdentityOf(at.StreamCreatedAt, stats[i].CreatedAt, true) ==
			statelog.StreamRecreated {

			return fmt.Errorf("%s's artefact was taken against the stream "+
				"created at %s and this node's stream was created at %s, so "+
				"adopting it would install a history this log does not have",
				name, at.StreamCreatedAt.UTC().Format(time.RFC3339Nano),
				stats[i].CreatedAt.UTC().Format(time.RFC3339Nano))
		}
		// The floor is included here, unlike the behind test: a position
		// the trim may delete will be gone by the time this node replays.
		if usable := max(first, floor); !statelog.Replayable(at.Seq, usable) {
			return fmt.Errorf("%s's log now starts at %d and the artefact is "+
				"at %d, so adopting it would leave a hole", name, usable, at.Seq)
		}
	}
	return nil
}

// holdTail pins the replay tail for the whole transfer and returns the release
// ([statelog.AdoptDeps.Hold]). One hold per join, keyed on this node and
// purpose ([coord.HoldOwner]), so a retry replaces it. The adopter releases it
// on every path that installed nothing, or it would renew and hold the trim
// for this node's life; an installed artefact's pin is kept until this node is
// counted ([stateLog.holdUntilCounted]). Renewed by [statelog.HoldTail]: put
// once, the trim ignored it a minute into a transfer.
func (s *stateLog) holdTail(ctx context.Context, at map[string]statelog.DomainPosition) (func(), error) {
	// An earlier adoption still waiting to be counted gives its pin up
	// first: one key per owner, so its renewals would put old positions back.
	s.releaseTail()
	// Keyed by stream, as [coord.TrimHold.Streams] is and the backup's hold
	// (from `statelog_cursor`) is, or the trim reads one as pinning nothing.
	streams := make(map[string]coord.Position, len(at))
	for _, domain := range registeredDomains() {
		name, stream := domain.Name(), domain.Stream().Name
		pos, held := at[name]
		if !held {
			continue
		}
		// The artefact's generation, never this node's cursor's, which is the
		// one thing a join knows is wrong (absent, or passed by a reanchor):
		// a pin below the log's generation is an unknown term to the trim.
		streams[stream] = coord.Position{
			Stream: stream, Generation: pos.Generation, Seq: pos.Seq,
		}
	}
	pin, err := statelog.HoldTail(ctx, statelog.TailHold{
		Register: s.fleet,
		Owner:    coord.HoldOwner(s.nodeID, joinHoldPurpose),
		Streams:  streams,
		Reason:   "this node is below the log's floor and is fetching a peer's snapshot",
		Every:    s.tailHoldEvery,
		Logger:   log,
	})
	if err != nil {
		// UNWRAPPED: the error names the hold by its owner, `<node>/join`,
		// and the adopter's own says what it was pinning for.
		return nil, err
	}
	return pin.Release, nil
}

// joinHoldPurpose names a join in the trim-hold register ([coord.HoldOwner]),
// beside the backup's: an operator reading a stalled trim sees which node pins
// it and what it is doing.
const joinHoldPurpose = "join"

// heldTail is an installed artefact's pin, waiting for this node to be counted
// ([stateLog.holdUntilCounted]). A pointer, because WHICH pin a publish
// captured is a question of identity: a join may hand over a newer one while
// a publish is in flight.
type heldTail struct {
	release func()
}

// holdUntilCounted keeps the pin an installed artefact still needs until this
// node's own published position counts the tail it pins. The trim removes what
// every COUNTED node has applied, and an adopter is about to replay the tail
// above the artefact: a boot's join is counted by nothing yet, and a running
// node's row names the replaced checkpoint until its next publish, so a pin
// released with the join could let a trim remove the tail just installed.
// Released by a counting publish ([stateLog.releaseCountedTail]), a later
// join's own pin ([stateLog.holdTail]) or the state log's stop.
func (s *stateLog) holdUntilCounted(release func()) {
	s.tailMu.Lock()
	previous := s.tailHeld
	s.tailHeld = &heldTail{release: release}
	s.tailMu.Unlock()
	if previous != nil {
		previous.release()
	}
}

// waitingTail is the pin waiting to be counted, or nil.
func (s *stateLog) waitingTail() *heldTail {
	s.tailMu.Lock()
	defer s.tailMu.Unlock()
	return s.tailHeld
}

// releaseTail releases the pin waiting to be counted, if there is one.
func (s *stateLog) releaseTail() {
	s.tailMu.Lock()
	held := s.tailHeld
	s.tailHeld = nil
	s.tailMu.Unlock()
	if held != nil {
		held.release()
	}
}

// releaseCountedTail releases pin, the one a publish captured at its start,
// once row, which that publish just wrote, counts the tail it pins: on every
// log, a position no further than the rows this node holds, so the trim keeps
// all it still has to apply. Read against the file's checkpoint after the row
// was written, not trusted from the row, whose runner may still be keyed to
// the replaced checkpoint. A row at a lower generation counts (the trim keeps
// everything for it); what cannot be established keeps the pin for the next
// publish. The captured pin, since a newer one may cover a file this row's
// runners never read.
func (s *stateLog) releaseCountedTail(ctx context.Context, pin *heldTail, row coord.NodePositions) {
	if pin == nil {
		return
	}
	counts, err := s.rowCountsTail(ctx, row)
	if err != nil {
		// AN ESTATE NOT OPEN IS SAID ALREADY, at error on every beat
		// (`statelog_estate_lost`), and the pin is kept for the same
		// reason the line is written: nothing is counted until it opens.
		if ctx.Err() == nil && !errors.Is(err, store.ErrNoEstate) {
			log.WarnContext(ctx, "statelog_join_hold_kept",
				"node", s.nodeID, "error", err.Error(),
				"detail", "whether this node's published position counts the "+
					"tail its adoption needs could not be read, so the join's "+
					"pin on the trim is kept, renewed, until a later heartbeat can")
		}
		return
	}
	if !counts {
		return
	}
	s.tailMu.Lock()
	current := s.tailHeld == pin
	if current {
		s.tailHeld = nil
	}
	s.tailMu.Unlock()
	if !current {
		// RELEASED ALREADY, or replaced by a later join's — whose own
		// pin is what that join's installed file is protected by.
		return
	}
	pin.release()
	log.InfoContext(ctx, "statelog_join_hold_released", "node", s.nodeID,
		"detail", "this node's own published position now counts the tail its "+
			"adoption replays, so the join's pin on the trim is released")
}

// rowCountsTail is whether row names, on every registered log, a position no
// further along than this node's own rows hold — see
// [stateLog.releaseCountedTail].
func (s *stateLog) rowCountsTail(ctx context.Context, row coord.NodePositions) (bool, error) {
	for _, domain := range registeredDomains() {
		name, stream := domain.Name(), domain.Stream().Name
		published, named := row.Domains[name]
		if !named {
			return false, nil
		}
		held, _, found, err := statelog.CursorFor(ctx, s.estate().Reader(), stream)
		if err != nil {
			return false, fmt.Errorf("read %s's checkpoint: %w", name, err)
		}
		if !found {
			// NO CHECKPOINT ROW IS THE ZERO POSITION: a log nobody has
			// written to has applied nothing.
			held = statelog.Position{}
		}
		switch {
		case published.Generation < held.Generation:
		case published.Generation > held.Generation:
			return false, nil
		case published.Seq > held.Seq:
			return false, nil
		}
	}
	return true, nil
}

// registered is every registered domain's log as the framework's surfaces want
// it: by name, with the health this node reports for each, or the zero health
// (vouching for nothing) for a log this runtime does not run. A snapshot gates
// on the health and a join on the artefact's acceptance; both walk this map,
// so a log cannot appear in one and not the other.
func (s *stateLog) registered() map[string]statelog.Registered {
	out := map[string]statelog.Registered{}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		entry := statelog.Registered{Domain: domain, Spec: domain.Stream()}
		running := s.Domain(name)
		if running == nil {
			// A LOG THIS NODE DOES NOT RUN IS ONE IT CANNOT VOUCH FOR,
			// which the zero value says.
			entry.Health = func() statelog.Health { return statelog.Health{} }
			out[name] = entry
			continue
		}
		entry.Spec = running.spec
		// The running log, captured: the snapshot loop and the adopter keep
		// this for the process's life, the set's own ([stateLog.logs]), and
		// its runner is relaunched or re-keyed in place, so every call reads
		// the live checkpoint. Under [stateLog.run], for
		// [stateLog.readerFor]'s reason: no caller's context outlives it.
		entry.Health = func() statelog.Health {
			health, err := s.health(s.run, running, s.readFloors(s.run))
			if err != nil {
				// AN UNREADABLE HEALTH IS NOT A HEALTHY ONE: the zero
				// value has Drained false, an unread floor and no
				// first sequence, which every gate reads as "cannot
				// vouch for this".
				return statelog.Health{}
			}
			return health
		}
		out[name] = entry
	}
	return out
}

// Status is what this node reports about each domain's apply loop: every
// registered domain, including those that do not gate seat admission, since
// the report asks how far along every copy is, and a domain missing from it is
// one an operator cannot see falling behind.
func (s *stateLog) Status(ctx context.Context) []ReplicationStatus {
	if s == nil {
		return nil
	}
	held := s.running()
	out := make([]ReplicationStatus, 0, len(held))
	floors := s.readFloors(ctx)
	for _, running := range held {
		name := running.domain.Name()
		row := ReplicationStatus{Name: name}
		health, err := s.health(ctx, running, floors)
		switch {
		case err != nil:
			// UNREADABLE IS NOT READY. The alternative reads as a
			// caught-up loop on a node whose broker is unreachable,
			// which is the one state this count exists to surface.
			row.Detail = "this node cannot read the domain's own position: " +
				err.Error()
		case health.StreamRecreated || health.GenerationPassed || health.LogDiverged:
			// A rebuilt, passed or diverged log first, ahead of the stop it
			// may have caused: this is the cause, with the remedy, and a node
			// on a rebuilt log not yet faulted past its budget would read as
			// ready.
			row.Detail = running.runner.StreamIdentity().Error()
		case health.Err != "":
			// A stopped applier is not ready whatever its lag says: one
			// halted on a recreated stream has a lag of zero.
			row.Detail = "the applier stopped: " + health.Err
		case health.AheadOfLog():
			row.Detail = fmt.Sprintf("this node's checkpoint %d is past the log's "+
				"end %d, so it names a stream that is not this one — a recreated "+
				"stream, or a broker restored from an older copy; `crewlet "+
				"retention reanchor` follows the new one from its head",
				health.Position.Seq, *health.LastSeq)
		case health.Lag == nil:
			// The head could not be read (bounds answered, stats not): never
			// print "0 record(s) behind", the number a reader would act on.
			row.Detail = "this node cannot tell how far behind the log it is"
		case *health.Lag > 0:
			// BEHIND IS NOT READY AND NOT WRONG. The seats this node
			// holds stay put (see [statelog.Health.Healthy]); what
			// this row feeds is the fleet view's "3 of 5", which is
			// how an operator sees a fresh node catching up.
			row.Detail = fmt.Sprintf("applying: %d record(s) behind the log's head",
				*health.Lag)
		case health.Deferred > 0:
			// Caught up and still not ready: a deferred record is one this
			// build cannot decode, so the rows it would write are missing.
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

// startSnapshots runs both halves of the fleet's recovery path: this node takes
// snapshots of its replicated estate on a timer and serves them to a peer that
// asks. Both, on every member: a recipient ([Engine.join]) with no donor never
// completes, half wired reports itself working, and there is no leader to be
// the donor. Both are best effort and never gate the boot: a node that cannot
// snapshot still serves its company, and only its peers' recovery loses.
func (e *Engine) startSnapshots(ctx context.Context, boot *config.Bootstrap, s *stateLog) {
	if s == nil || boot == nil || e.backends == nil {
		return
	}
	// Two seams: the join rides the engine's connection for one short
	// exchange, while the donor dials its own, since it closes what it is
	// given and would take the engine's broker connection down with it.
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
	root := boot.Store.SnapshotDirFor()
	interval := boot.Stream.TrackerRetention.SnapshotInterval()

	donor, err := statelog.NewDonor(s.donorDeps(root,
		func(context.Context) (*nats.Conn, error) { return broker.DialOwned() }))
	if err != nil {
		log.ErrorContext(ctx, "statelog_donor_unavailable", "error", err.Error(),
			"detail", "this node answers no offer request, so a peer below the "+
				"log's floor cannot adopt from it")
		return
	}
	// The snapshotter once, for the node's life: its logs are the boot's set
	// ([stateLog.logs]) and its file a handle resolved per call
	// ([stateLog.estate]), so nothing can move under it. A build refusal is a
	// registration no later attempt would find different, so it is logged
	// once and stamped on the heartbeat's row, not logged every tick.
	snapshotter, snapErr := e.snapshotterOf(s, root, interval)
	if snapErr != nil {
		log.ErrorContext(ctx, "statelog_snapshots_unavailable",
			"error", snapErr.Error(), "detail", "this node takes no snapshot, so it "+
				"can donate nothing newer than what its directory already holds "+
				"to a peer that falls below the log's floor; the fleet's other "+
				"data nodes still can")
		held := heldAfter(statelog.Manifest{}, snapErr, root)
		s.snapshot.Store(&held)
	}

	s.donor.Store(donor)
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		// THE DONOR FIRST and for the node's whole life: a peer asks at
		// ITS boot, which is any moment at all, so there is no window in
		// which not answering is acceptable.
		keepDonorServing(s.run, donor, donorRedial)
	}()
	if snapErr != nil {
		return
	}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		e.snapshotLoop(s, snapshotter, root, snapshotWaitsFor(interval))
	}()
}

// donorDeps is this node's donor: it answers every request with its newest
// artefact of the replicated estate, kept whole from boot whether or not this
// node may write, since a barred machine back with its files may hold the copy
// the fleet lacks.
func (s *stateLog) donorDeps(root string, dial statelog.Dialer) statelog.DonorDeps {
	return statelog.DonorDeps{
		NodeID: s.nodeID,
		Dial:   dial,
		Newest: func() (statelog.Manifest, bool) { return newestSnapshot(root) },
		Path: func(m statelog.Manifest) string {
			// The name the manifest carries, never re-derived: a derivation
			// let a second take land on the previous pair's name
			// ([statelog.Manifest.Artifact]).
			return filepath.Join(root, m.Artifact)
		},
	}
}

// snapshotterOf is the snapshotter of the replicated estate, over every log
// this node runs, writing its artefacts to dir.
func (e *Engine) snapshotterOf(s *stateLog, dir string, interval time.Duration) (*statelog.Snapshotter, error) {
	registered := s.registered()
	logs := make([]statelog.Registered, 0, len(registered))
	for _, running := range s.running() {
		logs = append(logs, registered[running.domain.Name()])
	}
	return statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains:       logs,
		File:          s.estate(),
		Dir:           dir,
		NodeID:        s.nodeID,
		EngineVersion: version.String(),
		Recipients: func(ctx context.Context) (int, error) {
			return e.recipients(ctx, s, time.Now())
		},
		Interval: interval,
	})
}

// snapshotLoop takes one at boot and then on the interval, but a skip is not
// the interval's business. [statelog.Snapshotter.Take]'s preconditions (caught
// up, not too far behind, more than one node counted, room on the volume,
// nothing deferred) are all transient at boot, so a skip retries soon, or a
// node restarted more often than the interval (a day) would never take one and
// the fleet would have no donor. A taken snapshot, or `recent`, is a question
// of staleness and waits ([snapshotWaits.after]). A reanchor, an adoption, a
// restore or a first drain wakes the loop at once ([stateLog.nudgeSnapshot]),
// since each leaves an artefact a joiner refuses or ends `unhydrated`.
func (e *Engine) snapshotLoop(s *stateLog, snap snapshotTaker,
	dir string, waits snapshotWaits) {

	ctx := s.run
	// What this node holds decides how loud a skip is: one holding an
	// artefact still gives its fleet a donor, one holding none does not. It
	// is [snapshotHeld.Have], read from the directory, so it survives a
	// restart as the artefact does (a process-scoped flag warned "never
	// taken" on every restart). The reason last reported is the other half:
	// an unchanged state is not news, though the register is stamped every
	// tick.
	var reported statelog.SkipReason
	// streak counts the boot skips in a row the retry is backing off over.
	streak := 0
	for {
		if ctx.Err() != nil {
			return
		}
		m, err := snap.Take(ctx)
		// The register is told every tick, taken or skipped: that row is the
		// only place the fleet sees this node's artefact (the trim's snapshot
		// term counts donors from it).
		held := heldAfter(m, err, dir)
		s.snapshot.Store(&held)
		var wait time.Duration
		wait, streak = waits.after(err, held, streak, time.Now())
		switch {
		case err == nil:
			reported = ""
			// AND AT ONCE rather than on the heartbeat's next beat: the
			// row naming this artefact is what makes the node a donor
			// anybody counts — see [stateLog.heartbeatNudge].
			s.nudgeHeartbeat()
		case !isSkip(err):
			// Repeated, unlike a skip: this one ran and errored, and a disk
			// gone read-only is a fault an operator should keep seeing.
			log.WarnContext(ctx, "statelog_snapshot_failed",
				"error", err.Error(), "holds_artefact", held.Have,
				"retry_in", wait,
				"detail", "this node's newest artefact is older than the "+
					"interval, so a peer adopting from it replays further")
			reported = statelog.SkipFailed
		default:
			reason, _ := statelog.Skipped(err)
			emitNoSnapshot(ctx, reportForSkip(reason, reported, held.Have), wait)
			reported = remembered(reason, held.Have)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-s.snapshotNudge:
			// A REANCHOR, AN ADOPTION, A RESTORE OR A FIRST DRAIN: the
			// next take is a question about whether the artefact is
			// current, which the gate answers — see
			// [stateLog.snapshotNudge].
		}
	}
}

// snapshotWaits is how long the snapshot loop waits after each tick.
type snapshotWaits struct {
	// Interval is the operator's staleness interval
	// (`stream.tracker_retention.snapshot_interval`): how long a taken
	// snapshot stands before the next is taken.
	Interval time.Duration
	// FirstRetry and Retry are the doubling retry after a skip that clears
	// on its own as a boot settles — its first wait, and the ceiling it
	// doubles to, which every other declined or failed tick waits flat.
	FirstRetry, Retry time.Duration
}

// snapshotWaitsFor is the production schedule under the operator's interval.
func snapshotWaitsFor(interval time.Duration) snapshotWaits {
	return snapshotWaits{Interval: interval,
		FirstRetry: snapshotSkipFirstRetry, Retry: snapshotSkipRetry}
}

// after is how long the loop waits after a tick that ended in err holding held
// (never past the interval), and the run of boot skips it carries on. Pure,
// for [reportForSkip]'s reason: the schedule makes a restarted node a donor.
//
//   - A taken snapshot waits the interval.
//   - `recent` waits until its artefact ages past the interval, when the
//     gate's answer next changes; a nudge covers a generation moving.
//   - `unhydrated`, `sole_node` and `lagging` clear as a boot settles, so
//     they double from [snapshotSkipFirstRetry] to [snapshotSkipRetry]:
//     prompt in a rolling upgrade, settled within a minute if they persist.
//   - Every other skip, and a failed take, waits flat at the ceiling, since
//     each waits on something outside the boot.
func (w snapshotWaits) after(err error, held snapshotHeld, streak int,
	now time.Time) (time.Duration, int) {
	retry := min(w.Retry, w.Interval)
	if err == nil {
		return w.Interval, 0
	}
	reason, skipped := statelog.Skipped(err)
	switch {
	case !skipped:
		return retry, 0
	case reason == statelog.SkipRecent && held.Have:
		stale := w.Interval - now.Sub(held.Manifest.TakenAt)
		return min(max(stale, min(w.FirstRetry, retry)), w.Interval), 0
	case clearsAtBoot(reason):
		streak++
		return backoff.Doubling(streak, w.FirstRetry, retry), streak
	default:
		return retry, 0
	}
}

// clearsAtBoot reports a skip that ends by itself as a node's boot settles —
// see [snapshotWaits.after].
func clearsAtBoot(reason statelog.SkipReason) bool {
	switch reason {
	case statelog.SkipUnhydrated, statelog.SkipSoleNode, statelog.SkipLagging:
		return true
	}
	return false
}

// snapshotTaker is the snapshotter as its loop uses it: one attempt, which
// either took an artefact or says why not. [statelog.Snapshotter] is the
// implementation.
type snapshotTaker interface {
	Take(ctx context.Context) (statelog.Manifest, error)
}

// levelHook is what a domain's apply loop tells each time it comes level with
// its log ([statelog.RunnerDeps.OnLevel]): the seat-claim gate's watch for a
// domain whose health gates admission, and nobody for one whose health does
// not, whose level the gate never asks about — see admission.go.
func (s *stateLog) levelHook(domain statelog.Domain) func() {
	if s.admit == nil || !domain.GatesSeatAdmission() {
		return nil
	}
	return s.admit
}

// nudgeSnapshot wakes the snapshot loop, without waiting for it — see
// [stateLog.snapshotNudge].
func (s *stateLog) nudgeSnapshot() {
	select {
	case s.snapshotNudge <- struct{}{}:
	default:
	}
}

// nudgeHeartbeat wakes the position heartbeat, without waiting for it — see
// [stateLog.heartbeatNudge].
func (s *stateLog) nudgeHeartbeat() {
	select {
	case s.heartbeatNudge <- struct{}{}:
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
// why it skipped and what was last said; pure, so the rule is tested without a
// fleet. Every reason but `sole_node` is a property of this node that clears
// by itself while its peers lack a donor in it: a warning. `sole_node` is the
// fleet's shape and the documented default (one node, recovered from a backup,
// [internal/backup]), ending only when an operator adds a node, so it is said
// once at info; warned every tick, it taught operators to filter the
// subsystem. The tick stays on the retry: `sole_node` is the snapshotter's
// first gate, one register read, so the donor arms a tick after a peer joins.
func reportForSkip(reason, reported statelog.SkipReason, holds bool) noSnapshotReport {
	// A node that holds an artefact says nothing, decided here rather than
	// at the call site so the predicate and the decision are one place.
	if holds || reason == reported {
		return noSnapshotReport{}
	}
	if reason == statelog.SkipSoleNode {
		return noSnapshotReport{
			Event:  "statelog_snapshot_sole_node",
			Reason: reason,
			Detail: "this node is the only member the fleet counts, so there " +
				"is nobody to donate a snapshot to and none is taken; a single " +
				"node is recovered from a backup (retention.backup_owner) " +
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

// remembered is what the next tick compares against. A node holding an
// artefact remembers nothing, so if it later holds none, that is said again.
func remembered(reason statelog.SkipReason, holds bool) statelog.SkipReason {
	if holds {
		return ""
	}
	return reason
}

// emitNoSnapshot writes what [reportForSkip] decided, and nothing for the tick
// it decided says nothing. wait is how long the loop waits before it asks
// again ([snapshotWaits.after]).
func emitNoSnapshot(ctx context.Context, say noSnapshotReport, wait time.Duration) {
	switch {
	case say.Event == "":
	case say.Warn:
		log.WarnContext(ctx, say.Event, "reason", string(say.Reason),
			"retry_in", wait, "detail", say.Detail)
	default:
		log.InfoContext(ctx, say.Event, "reason", string(say.Reason),
			"recheck_in", wait, "detail", say.Detail)
	}
}

// heldAfter is what a snapshot tick concluded this node holds, read from the
// directory: a skip deletes nothing, and `recent` is a skip because an artefact
// is there, while the other skips leave yesterday's copy still donatable. The
// skip says why nothing was refreshed, the directory what is held.
func heldAfter(m statelog.Manifest, err error, dir string) snapshotHeld {
	if err == nil {
		return snapshotHeld{Manifest: m, Have: true}
	}
	// A hard failure is published as a reason too: an empty skip beside an
	// old position reads as a loop not yet come round.
	held := snapshotHeld{Skip: statelog.SkipFailed}
	if reason, skipped := statelog.Skipped(err); skipped {
		held.Skip = reason
	}
	if on, found := newestSnapshot(dir); found {
		held.Manifest, held.Have = on, true
		if held.Skip == statelog.SkipRecent {
			// `recent` is no answer to "why can this node not donate": it
			// can, and publishing it would list every healthy node as unable.
			held.Skip = ""
		}
	}
	return held
}

// stampSnapshot writes what this node holds onto the row it is about to
// publish: the artefact's size and skip on the row, each log's artefact
// position on its entry. Over values, for [statelog.TrimInputs]'s reason: the
// one part of the path from disk to another node's trim testable without a
// fleet.
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
	// A short artefact stamps nothing: [statelog.Offer.Usable] refuses
	// wholesale one that lacks a domain the recipient registers, so stamping
	// the domains it covers would let the trim count a donor that can never
	// hand over and trim past what no joiner could replay.
	for name := range row.Domains {
		if _, covered := held.Manifest.Domains[name]; !covered {
			return
		}
	}
	row.SnapshotBytes = held.Manifest.Bytes
	for name, at := range held.Manifest.Domains {
		// Only a domain this node runs: an artefact from another build can
		// name one this build does not register, which the trim would read
		// as a node holding that log back at zero.
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

// snapshotSkipRetry is the longest a declined attempt waits to be retried: the
// ceiling a boot skip's doubling settles on, and the flat retry of every other
// skip and a failed take ([snapshotWaits.after]). Thirty seconds, a boot's own
// settling time (peers publish on the heartbeat, appliers drain in seconds):
// shorter spends a wedged node on a gate it cannot pass, much longer leaves a
// late-clearing node without an artefact through a rolling upgrade.
const snapshotSkipRetry = 30 * time.Second

// snapshotSkipFirstRetry is the first wait of a boot skip's doubling retry: one
// second, the scale of the events that end one (an applier's empty fetch, a
// peer's first heartbeat). Each retry is a few reads, so the five doublings to
// [snapshotSkipRetry] cost a booting node five register reads.
const snapshotSkipFirstRetry = time.Second

// counted is the nodes the fleet counts on the estate's logs at now, sorted,
// which decides whether there is anybody to donate to: the trim's own counted
// set ([statelog.CountedSet]), so the loop and the trim's snapshot term ask
// one question. Rows naming the log, plus every live data node (a joiner has
// no row until it adopts the copy this decides whether anybody takes), less
// tombstones past their window (an evicted node's row never expires). The live
// half comes from the watched view ([Engine.watchedHolders]), asked on every
// retry; a view that cannot answer leaves it out, a lower bound that declines
// a copy for a tick rather than failing a take over a coordination blip.
// Unreadable eviction rows subtract none, erring toward an unneeded copy.
func (e *Engine) counted(ctx context.Context, s *stateLog, now time.Time) ([]string, error) {
	if s.fleet == nil {
		return nil, fmt.Errorf("engine: no coordination to count the fleet with")
	}
	rows, err := s.fleet.Positions(ctx)
	if err != nil {
		return nil, err
	}
	var live []statelog.Presence
	if nodes, err := e.watchedHolders().LiveData(ctx); err == nil {
		live = nodes
	}
	return slices.Sorted(maps.Keys(e.countedOn(ctx, rows, live, now))), nil
}

// recipients is how many nodes other than this one the fleet counts on the
// estate's logs at now ([Engine.counted]), who an artefact could be donated to
// ([statelog.SnapshotDeps.Recipients]). Others, since this node need not be
// counted: an evicted, barred node back with its files may hold the fleet's
// only copy, and "fewer than two counted" read its one joiner as nobody.
func (e *Engine) recipients(ctx context.Context, s *stateLog, now time.Time) (int, error) {
	counted, err := e.counted(ctx, s, now)
	if err != nil {
		return 0, err
	}
	others := 0
	for _, node := range counted {
		if node != s.nodeID {
			others++
		}
	}
	return others, nil
}

// newestSnapshot reads the newest complete manifest in a directory. The
// manifest is the claim, as in [internal/backup]: written last, so an entry
// without one is the debris of an unfinished run.
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
// register for as long as the node runs. Three readers are wrong without it:
// the trim's minimum (it would trim past records this node needs), the
// snapshot gate (an empty register is a fleet of one, so nobody takes a
// snapshot), and the trim's snapshot term (a row with a position but no
// artefact blocks every domain's trim while all else looks healthy). The write
// fence must not read it: a minimum including the reader can never refuse it,
// so the fence reads the published floor, and a node the trim passed is
// refused there. A timer, not a write per commit, since readers need a recent
// number, not the fleet's slowest store on the apply loop; each row carries
// its instant. [statelog.PositionHeartbeat] lives in the framework, since the
// alarm table evaluates on it ([statelog.AlarmInterval]).
func (s *stateLog) startPositionHeartbeat() {
	if s == nil || s.fleet == nil || len(s.running()) == 0 {
		return
	}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		s.beat(s.run, statelog.PositionHeartbeat, s.publishPositions)
	}()
}

// beat is the heartbeat's loop: publish at once (a node restarted more often
// than the interval would never appear otherwise), then on every tick of
// interval and every nudge ([stateLog.heartbeatNudge]), until ctx ends. The
// publish is a parameter so the schedule, which notices a running node fall
// below the floor, runs under an interval a test can wait out. An ended loop
// holds no clock measure, or an alarm would report its last one
// (clockskew.go).
func (s *stateLog) beat(ctx context.Context, interval time.Duration,
	publish func(context.Context)) {
	defer s.forgetSkew()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		publish(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.heartbeatNudge:
		}
	}
}

// publishPositions writes one row describing every domain this node runs (the
// register's shape: the trim asks about all of them at once), with the
// snapshot this node holds as its snapshot loop last concluded, not re-read
// from disk every ten seconds for an answer that moves daily. A failure is
// logged and the loop continues: a node whose heartbeat stopped on a
// coordination blink would be invisible to the trim, the failure this loop
// exists to prevent.
func (s *stateLog) publishPositions(ctx context.Context) {
	// NO REGISTER, NOTHING TO PUBLISH TO — the condition the heartbeat is
	// never started under, checked here too because a reanchor publishes
	// through this directly.
	if s.fleet == nil {
		return
	}
	// One publish at a time, from reading the runners to writing the row:
	// the register keeps the last row written, and a beat that read before a
	// reanchor re-keyed a runner and wrote after it put the old generation
	// back.
	s.publishing.Lock()
	defer s.publishing.Unlock()
	// THE PIN AN ADOPTION IS WAITING TO HAVE COUNTED, captured BEFORE the
	// runners are read: one handed over after this point belongs to an
	// install this reading may predate, and is the next publish's to judge
	// ([stateLog.releaseCountedTail]).
	tail := s.waitingTail()
	held := s.running()
	row := coord.NodePositions{
		NodeID:        s.nodeID,
		At:            time.Now().UTC(),
		EngineVersion: version.String(),
		Domains:       make(map[string]coord.DomainPosition, len(held)),
	}
	below := false
	// The fleet's generations, for the one way a node falls out of step that
	// its own log cannot show: a peer re-anchored past it. Unread, this beat
	// judges nothing by them. An evicted peer's generation is not the fleet's
	// ([stateLog.fleetGenerations]), so a decommissioned peer that re-anchored
	// stops sending this node after a donor that no longer exists.
	above := make(map[string]uint32, len(held))
	for _, running := range held {
		above[running.domain.Name()] = running.runner.Committed().Generation
	}
	// The register is read once for both questions this beat asks (the
	// fleet's generation, and whether a peer's rows hold records a log lost,
	// [stateLog.truncation]), judged from one successful reading or not at
	// all: the peer holding what the log lost is often the one that then
	// re-anchored, and judging one without the other lifted the write fence
	// with no passed verdict to replace it. Per domain: one whose floor or
	// evicted peers could not be read waits while the others are judged
	// ([stateLog.fleetGenerations]). These floors also build the gauges below.
	rows, rowsErr := s.fleet.Positions(ctx)
	floors := s.readFloors(ctx)
	generations, unreadGen := map[string]uint32(nil), map[string]error{}
	var clocks *skewMeasure
	if rowsErr == nil {
		s.observeUsageGenerations(rows)
		generations, unreadGen = s.fleetGenerations(ctx, rows, floors, s.openLogs(), above)
		// And how far this node's clock, and the broker member's, stand from
		// the fleet's, off the rows just read, so `clock_skew` costs no read
		// of its own (clockskew.go).
		clocks = s.measureSkew(rows)
	}
	// RECORDED ON EVERY PUBLISH, a reading that failed included: a publish
	// that measured nothing is a miss, and two in a row drop the measure.
	s.recordSkew(clocks)
	genErr := func(name string) error {
		if rowsErr != nil {
			return rowsErr
		}
		return unreadGen[name]
	}
	if ctx.Err() == nil {
		for _, running := range held {
			name := running.domain.Name()
			if err := genErr(name); err != nil {
				log.WarnContext(ctx, "statelog_generations_unread",
					"node", s.nodeID, "domain", name, "error", err.Error(),
					"detail", "which generation the fleet is on for this log could "+
						"not be established this beat, so neither whether a peer "+
						"re-anchored past this node nor whether a peer's rows hold "+
						"records the log lost is judged; both verdicts stay where "+
						"they were until a beat can read them together")
			}
		}
	}
	for _, running := range held {
		name := running.domain.Name()
		stance := running.runner.Stance()
		at, record := stance.At, stance.StoredAt
		// AND PASSED BY A REANCHOR: the same predicate the join asks, so
		// the rejoin this requests is one the join then acts on. The
		// runner is told first, which is what refuses this node's reads
		// and writes of the domain until the adoption lands.
		if genErr(name) == nil && passedByAReanchor(running.domain, at, true, generations[name]) {
			if running.runner.ObserveFleetGeneration(generations[name]) {
				log.WarnContext(ctx, "statelog_generation_passed",
					"domain", name, "stream", running.spec.Name,
					"generation", at.Generation, "fleet_generation", generations[name],
					"detail", "a peer re-anchored this log past the generation "+
						"this node's rows are at, so the log now continues from "+
						"that peer's rows; this node refuses reads and writes of "+
						"the domain and adopts a snapshot from a peer at the new "+
						"generation")
			}
			below = true
		}
		// AND WHO HOLDS THE GENERATION A VERDICT NAMES, from the same
		// reading: the remedy its refusals name moves while it holds — an
		// operator evicting the peer that opened it leaves nothing to adopt
		// ([statelog.Runner.RecheckPassed]).
		if genErr(name) == nil {
			if _, err := running.runner.RecheckPassed(ctx, generations[name]); err != nil &&
				ctx.Err() == nil {
				log.WarnContext(ctx, "statelog_generation_holder_unread",
					"node", s.nodeID, "domain", name, "error", err.Error(),
					"detail", "whether the node that opened the generation this "+
						"node's rows were passed by is evicted could not be read, "+
						"so the remedy its refusals name stays as it was until a "+
						"beat can")
			}
		}
		// The instant the rows are keyed to rides beside the generation,
		// which alone cannot say which stream a sequence is on
		// ([coord.DomainPosition.StreamCreatedAt]); the record the checkpoint
		// stands on lets a peer tell a node on the log from one whose history
		// the log lost ([coord.DomainPosition.CheckpointStoredAt]).
		pos := coord.DomainPosition{
			Seq: at.Seq, Generation: at.Generation, AppliedThrough: at.Seq,
			StreamCreatedAt: running.runner.KeyedTo(), CheckpointStoredAt: record,
			// WHICH RECORDS THIS BUILD READS, so a writer of a kind older
			// builds cannot even defer waits for every node to read it —
			// see [coord.DomainPosition.RecordVersion].
			RecordVersion: running.domain.RecordVersion(),
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
		// The progress observation rides this loop, the one place that takes
		// a look every interval, keeping a health read pure ([progress]).
		// Behind is read from the broker's last sequence, not the consumer's
		// backlog: an idle node owes nothing, and a stall needs owed work.
		stats, statsErr := running.log.Stats(ctx)
		behind := backlogOf(stats, statsErr, at.Seq, running.progress.owed())
		if statsErr == nil {
			// And how long behind, for the request path
			// ([runningLog.lagNanos]).
			running.observeLag(stats.LastSeq, at.Seq)
			// And below: the next record this node needs is gone, so it
			// adopts as a boot would. Asked of `first` alone, as the join asks
			// it, or the rejoin it requests adopts nothing, on every retry.
			if !statelog.Replayable(at.Seq, stats.FirstSeq) {
				below = true
			}
			// And whether it is even the same log: a stream rebuilt under a
			// running node restarts at generation 0 from 1, invisible to the
			// sequence terms once it passes this node's checkpoint. Its
			// creation instant, in this same answer, goes to the runner, whose
			// verdict reads and writes refuse on (a zero-expectation write
			// also hands one over, without waiting for this beat). With it the
			// end, paired with `at` read before this answer, the only order in
			// which a checkpoint past the end means another history rather
			// than a record landing between reads: what fence 0 refuses on.
			s.observeStream(ctx, name, running.runner, stance, stats)
			// And whether the log still holds, at the checkpoint, the record
			// it consumed there: the loop asks before applying past it; this
			// asks for a node with nothing to apply (a restored log written
			// exactly to its checkpoint) or stopped, and of a broker restored
			// between the loop's moments. One direct read per domain per beat.
			established, verifyErr := running.runner.VerifyCheckpoint(ctx)
			if established {
				s.logDiverged(ctx, name, running.runner)
			}
			// AND WHETHER A PEER'S ROWS HOLD RECORDS THIS LOG LOST, which
			// refuses this node's writes of it — against the same end, and
			// only on the reading the passed verdict above was judged from.
			if genErr(name) == nil {
				truncation, truncErr := s.truncation(ctx, running, rows, at, stats.LastSeq)
				switch {
				case truncErr == nil:
					s.observeTruncation(ctx, name, running.runner, stance, truncation)
				case ctx.Err() == nil:
					log.WarnContext(ctx, "statelog_truncation_unread",
						"node", s.nodeID, "domain", name, "error", truncErr.Error(),
						"detail", "whether a peer's rows hold records this log lost "+
							"could not be established this beat; the verdict stays "+
							"where it was until one can")
				}
			}
			if verifyErr != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "statelog_checkpoint_unverified",
					"node", s.nodeID, "domain", name, "error", verifyErr.Error(),
					"detail", "whether the log still holds the record at this "+
						"node's checkpoint could not be read this beat; the "+
						"applier asks again before it applies past it, and so "+
						"does the next beat")
			}
			// And a recreation verdict the live instant contradicts
			// ([statelog.Runner.RecreationStale]): only a join re-keys it and
			// a reanchor refuses such rows, so a rejoin is asked for, which
			// finds every domain current and re-keys each runner.
			if running.runner.RecreationStale(stats.CreatedAt) {
				below = true
			}
		}
		running.progress.observe(row.At, pos, behind)
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
	s.positionGauges(ctx, row, floors)
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
		return
	}
	// AND A ROW THE TRIM NOW COUNTS releases the pin an adoption held for
	// exactly this moment — only once it has been written.
	s.releaseCountedTail(ctx, tail, row)
}

// positionGauges publishes each domain's progress on the beat the register row
// is written: these are states, and set per applied batch a quiet log would
// leave the last burst's lag standing. floors is the beat's own read, the one
// the generations were judged from.
func (s *stateLog) positionGauges(ctx context.Context, row coord.NodePositions,
	floors publishedFloors) {
	if s.metrics == nil {
		return
	}
	for name, at := range row.Domains {
		attrs := metrics.Attrs{"domain": name}
		s.metrics.Set(metrics.StatelogAppliedThrough, float64(at.AppliedThrough), attrs)
		s.metrics.Set(metrics.StatelogDeferredCount, float64(at.Deferred), attrs)
		running := s.Domain(name)
		if running == nil {
			continue
		}
		s.metrics.Set(metrics.StatelogDrainRowsPerSecond, running.runner.Drain(), attrs)
		// And the commit rate beside it: rows/s is progress, commits/s the
		// fsync rate a device's write budget is spent by, and doubled
		// commits for the same rows shows in neither alone.
		s.metrics.Set(metrics.StatelogDrainCommitsPerSecond,
			running.runner.Commits(), attrs)
		health, err := s.health(ctx, running, floors)
		if err != nil || health.Lag == nil {
			// Unreadable is not zero, and a gauge has no third value: the
			// lag gauges keep their last value, as the alarm reading does.
			continue
		}
		s.metrics.Set(metrics.StatelogApplyLagSeq, float64(*health.Lag), attrs)
		s.metrics.Set(metrics.StatelogApplyLagSeconds,
			applyLagOf(health, running).Seconds(), attrs)
	}
}

// deferralGauges publishes how long this node has held what it cannot decode:
// seconds beside the count, since the age decides whether this node's copy
// stays in service (D122). On the heartbeat, where [progress] observes when
// the oldest arrived.
func (s *stateLog) deferralGauges(now time.Time) {
	if s.metrics == nil {
		return
	}
	for _, running := range s.running() {
		name := running.domain.Name()
		if running == nil {
			continue
		}
		age := running.progress.deferredSinceValue().Age(now)
		s.metrics.Set(metrics.StatelogDeferredOldestAgeSeconds, age.Seconds(),
			metrics.Attrs{"domain": name})
	}
}

// logOf is the running log whose stream this is, or nil.
//
// A REVERSE LOOKUP over the set rather than a second map beside it: the set
// holds a handful of logs, and one index is one thing to build.
func (s *stateLog) logOf(stream string) *runningLog {
	for _, running := range s.running() {
		if running.spec.Name == stream {
			return running
		}
	}
	return nil
}

// opsLedgers is the operation ledger of every log this node runs, keyed by its
// domain's name, which names the sweep's `<domain>_ops` job
// ([maintenance.StatelogJobs]), each with its register entry's horizons. Built
// from the running set, not a hand-written list a new domain could be missed
// from; read once where the sweep is built, since the set is fixed
// ([stateLog.logs]).
func (s *stateLog) opsLedgers() map[string]maintenance.OpsHorizon {
	out := map[string]maintenance.OpsHorizon{}
	for _, running := range s.running() {
		if running.runner == nil {
			continue
		}
		name := running.domain.Name()
		entry, found := registrationFor(name)
		// A domain that keeps no ledger has nothing to sweep (the compacted
		// ones), or a sweep job would purge a table that does not exist;
		// [checkRegister] refuses OpsRetention on such a domain.
		if !found || running.domain.OpsTable() == "" {
			continue
		}
		out[name] = maintenance.OpsHorizon{
			Ledger:    domainLedger{runner: running.runner},
			Retention: entry.OpsRetention,
			Kinds:     entry.OpsKindRetention,
		}
	}
	return out
}

// domainLedger is one domain's operation-ledger sweeps, this node's own rows,
// not its arbitration anchors: those the trim removes with a trimmed record
// every node applies at one position (ADR-0080, [retention.sweepAnchors]), or
// a node would be a second writer of rows every copy holds alike (ADR-0002).
type domainLedger struct {
	runner *statelog.Runner
}

func (l domainLedger) PurgeOps(ctx context.Context, cutoff time.Time) (int64, error) {
	return l.runner.PurgeOps(ctx, cutoff)
}

func (l domainLedger) PurgeOpsOfKind(ctx context.Context, kind string,
	cutoff time.Time) (int64, error) {

	return l.runner.PurgeOpsOfKind(ctx, kind, cutoff)
}
