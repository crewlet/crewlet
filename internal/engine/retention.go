package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE TRIM, and it is the one loop without which the log only grows.
//
// # What it is, in one paragraph
//
// Every domain's ordered log is a replay window rather than an archive: the
// durable tables are the record of truth, and the log's job is to carry
// records to nodes that have not applied them yet. Once every node the fleet
// counts has committed past a record — and nothing else needs it — the record
// can go. [statelog.Trim] is the arithmetic that decides how far, from six
// terms that each move the point DOWN; this file is what reads those terms
// from a live fleet, applies the answer to the broker, and publishes what it
// concluded so a node that is not holding the duty can still say why the log
// is not shrinking.
//
// # Why nothing here re-decides
//
// The six terms have an inversion in them that three readers got backwards,
// which is exactly why the arithmetic is a pure function over a struct of
// values. This loop's whole job is to fill that struct honestly — including
// with the THIRD value, "could not be read", which blocks the trim rather than
// being treated as satisfied. A term nobody could read is not a term nobody
// needs.
//
// # A fleet singleton, and what happens without one
//
// The trim is a burst of purges against shared streams, and two nodes deciding
// concurrently would race on the register's published floor: the loser's
// conclusion would overwrite the winner's and `blocked_since` — the field that
// says how long this has been going on — would reset on every tick. So it is a
// duty like the sweep, on the `worker:{duty}` lease, and a fleet whose nodes
// all declare `roles: [seats]` deliberately does not trim.

// RetentionInterval is how often the trim evaluates.
//
// FIFTEEN MINUTES, and the number comes from what the tick costs against what
// it can save. The cost is one stream info, one register listing and — at most
// — a binary search over the log for the age floor, per domain: a handful of
// round trips. What it buys is that a term clearing (a backup landing, a
// lagging node catching up) becomes disk within a quarter of an hour rather
// than within whatever the next restart happened to be.
//
// It is also the resolution `blocked_since` has, which is what the
// twenty-four-hour backup condition is measured in — so a longer interval
// would make the alarm coarser for no saving that matters at this cost.
const RetentionInterval = 15 * time.Minute

// retentionDutyName is the fleet singleton the trim claims.
const retentionDutyName = "retention"

// retentionDutyTTL is how long the duty survives without a re-claim.
//
// Three ticks, the ratio every other duty here uses: one missed tick must not
// hand the trim to a peer, because two nodes purging and publishing at once is
// exactly what the singleton avoids.
const retentionDutyTTL = 3 * RetentionInterval

// retention is the trim loop.
type retention struct {
	fleet  coord.Fleet
	leases coord.Backend
	state  *stateLog
	db     *store.DB
	cfg    config.TrackerRetention
	claim  schedule.DutyFunc
	nodeID string

	// backupOwner is `retention.backup_owner` — read here rather than
	// looked up beside the report, because the one place it matters is the
	// backup term's remedy and a remedy naming a value the document does
	// not carry is one a JSON reader never sees.
	backupOwner string

	// metrics is the process's one recorder, for the alarm inputs that are
	// percentiles rather than states. Nil records nothing, which every
	// condition reads as "nothing to report".
	metrics *metrics.Recorder

	// alarms turns each evaluation into the two surfaces that are not a
	// screen — the `crewlet.alarm.active{kind}` gauge a collector scrapes,
	// and one WARN on entry and one on exit.
	//
	// WITHOUT IT THE TABLE HAD ONE SURFACE OF THREE: `work_retention`
	// rendered the alarms to whoever asked, and nothing at all reached a
	// collector or a log. An alarm nobody is looking at a dashboard for is
	// an alarm that fires into an empty room, which is indistinguishable
	// from a fleet with nothing wrong.
	alarms *statelog.Tracker

	// coverage is the fraction of this node's sources carrying a current
	// vector, or false when this company has no embeddings configured.
	// Nil on a node that cannot measure it, which reads as "nothing to
	// report" rather than as no coverage.
	coverage func(context.Context) (float64, bool, error)

	// mu guards the coverage cache below. The tick and every API request
	// assemble a report, on different goroutines.
	mu sync.Mutex

	// coverAt, coverFraction and coverKnown are the last coverage
	// measurement and when it was taken.
	//
	// CACHED FOR ONE TICK, because the measurement is a scan of the whole
	// source corpus and a report is assembled on every operator request
	// and every dashboard poll — where the trim's own inputs are read once
	// per tick by construction. One [RetentionInterval] is also the
	// resolution every other alarm input here has, so a fresher coverage
	// number would be the only one on the reading that could disagree with
	// its neighbours about which tick it describes.
	coverAt       time.Time
	coverFraction float64
	coverKnown    bool

	// pooled is the last `sql.DBStats` wait counters seen per store file,
	// so the histogram is fed the DELTA rather than the process's
	// cumulative total. Keyed on the file, which is the attribute.
	pooled map[string]poolCounters

	stop context.CancelFunc
	done chan struct{}
}

// poolCounters is one store file's cumulative connection-wait counters.
type poolCounters struct {
	count  int64
	waited time.Duration
}

// startRetention arms the trim.
//
// Started with the state log rather than beside the sweep, because it is the
// state log's own gate: it has no meaning on a node running no domains, and a
// loop that ran there would publish a floor derived from streams it does not
// have.
func (e *Engine) startRetention(ctx context.Context, boot *config.Bootstrap, s *stateLog) {
	if s == nil || len(s.domains) == 0 || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	r := &retention{
		fleet:       e.backends.Fleet,
		leases:      e.backends.Coord,
		state:       s,
		db:          e.backends.Store,
		cfg:         boot.Stream.TrackerRetention,
		backupOwner: boot.Retention.BackupOwner,
		metrics:     e.metrics,
		claim:       schedule.DutyFunc(e.workerDuty(retentionDutyName, retentionDutyTTL)),
		nodeID:      s.nodeID,
		alarms:      statelog.NewTracker(e.metrics, nil),
		coverage:    e.vectorCoverage,
		pooled:      map[string]poolCounters{},
		done:        make(chan struct{}),
	}
	// DETACHED from the caller's context, for the reason every other
	// long-running loop here is: a loop bound to a signal context stops at
	// SIGTERM, which would make its lifetime differ from the appliers it
	// is trimming behind for no reason a reader could find.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	r.stop = stop
	e.retention = r
	go r.run(loop)
}

// stopRetention ends the trim, waiting for an in-flight tick.
func (e *Engine) stopRetention() {
	if e.retention == nil {
		return
	}
	e.retention.stop()
	<-e.retention.done
	e.retention = nil
}

// RetentionReport answers "is the log being trimmed, and what is stopping it"
// from outside the package — the question `crewlet retention status` exists
// for. The bool is false on a node running no state log, which is a real
// deployment and NOT a report of zeros: a document full of zeros would claim a
// fleet whose log is perfectly trimmed.
func (e *Engine) RetentionReport(ctx context.Context) (statelog.Report, bool) {
	if e.retention == nil {
		return statelog.Report{}, false
	}
	return e.retention.Report(ctx), true
}

// run ticks until the context ends.
//
// IT TICKS IMMEDIATELY, which matters more here than the usual reason: the
// published floor is what every other surface reads a blocked trim from, and a
// fleet that had just started would otherwise answer "no floor published" for
// fifteen minutes — indistinguishable from a fleet whose duty is not running.
func (r *retention) run(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(RetentionInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			// CHECKED BEFORE THE TICK, not only after: a loop started
			// under a context that is already done would otherwise
			// take one tick on the way out and log a duty claim
			// failing during a shutdown that is going fine.
			return
		}
		r.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick evaluates every domain once, if this node holds the duty.
func (r *retention) tick(ctx context.Context) {
	// FIRST, AND ON EVERY NODE — before the duty claim, deliberately.
	//
	// The trim is a fleet singleton because two nodes purging one log is
	// waste; the ALARMS are the opposite. [statelog.Reading] describes ONE
	// node — its own applier's lag, its own disk, its own refusals — so a
	// table evaluated only where the duty happens to sit would report the
	// duty holder's health as the fleet's, and the wedged node would be the
	// one nobody hears from. It is also what makes this loop useful on a
	// node that never wins the lease at all.
	r.evaluate(ctx)
	if r.claim != nil {
		mine, err := r.claim(ctx)
		if err != nil {
			log.WarnContext(ctx, "retention_duty_unclaimed", "err", err)
			return
		}
		if !mine {
			return
		}
	}
	shared, err := r.read(ctx)
	if err != nil {
		// EVERY TERM DERIVED FROM THE REGISTER IS UNKNOWN, which blocks
		// — so nothing is purged and the reason is said once rather
		// than once per domain.
		// ITS OWN EVENT NAME, not the per-domain one: a blocked-trim
		// line carries a domain and a term, and one with neither would
		// be counted by anything filtering on that name as a domain
		// blocked by a term called "register".
		log.WarnContext(ctx, "retention_inputs_unreadable", "err", err)
		return
	}
	for _, name := range r.state.order {
		if err := r.domain(ctx, name, shared); err != nil {
			log.WarnContext(ctx, "retention_trim_failed", "domain", name, "err", err)
		}
	}
}

// evaluate observes this node's alarms and records what one tick can measure
// about its own hardware.
//
// THE MEASUREMENT COMES FIRST, because the reading the table is evaluated
// against reads three of these back: a tick that observed before it measured
// would evaluate the previous tick's disk against this tick's log.
func (r *retention) evaluate(ctx context.Context) {
	r.capacity(ctx)
	if r.alarms == nil {
		return
	}
	r.alarms.Observe(ctx, r.Report(ctx).Alarms)
}

// fleetInputs is what one tick reads once and every domain shares.
type fleetInputs struct {
	at        time.Time
	positions []coord.NodePositions
	readable  bool
	holds     []coord.TrimHold
	backups   []coord.BackupPoint
	live      []statelog.Presence
	previous  map[string]coord.TrimFloor
}

// read fetches the fleet-wide half of the inputs.
//
// ONCE PER TICK RATHER THAN ONCE PER DOMAIN, and it is a correctness property
// rather than a saving: two domains evaluated against two listings of the same
// register can disagree about who is counted, so one domain's floor would be
// published against a fleet the other's was not.
func (r *retention) read(ctx context.Context) (fleetInputs, error) {
	in := fleetInputs{at: time.Now().UTC(), previous: map[string]coord.TrimFloor{}}

	positions, err := r.fleet.Positions(ctx)
	if err != nil {
		return in, fmt.Errorf("read the positions register: %w", err)
	}
	in.positions, in.readable = positions, true

	if in.holds, err = r.fleet.Holds(ctx); err != nil {
		return in, fmt.Errorf("read the trim holds: %w", err)
	}
	if in.backups, err = r.fleet.BackupPoints(ctx); err != nil {
		return in, fmt.Errorf("read the backup points: %w", err)
	}
	floors, err := r.fleet.Floors(ctx)
	if err != nil {
		return in, fmt.Errorf("read the published floors: %w", err)
	}
	for _, f := range floors {
		in.previous[f.Domain] = f
	}
	// THE PRESENCE LEASES ARE WHAT CATCH A NODE BETWEEN BOOT AND ITS
	// FIRST HEARTBEAT — which is exactly a node adopting a snapshot. It
	// counts at position zero and blocks, which is correct: trimming past
	// a node that is joining is deleting what it is about to replay.
	if r.leases != nil {
		leases, err := r.leases.ListLive(ctx, coord.NodePrefix)
		if err != nil {
			return in, fmt.Errorf("list the live nodes: %w", err)
		}
		for _, lease := range leases {
			if id, ok := coord.NodeID(lease.Resource); ok {
				in.live = append(in.live, statelog.Presence{NodeID: id})
			}
		}
	}
	return in, nil
}

// domain evaluates and applies one domain's trim.
func (r *retention) domain(ctx context.Context, name string, shared fleetInputs) error {
	running := r.state.domains[name]
	if running == nil {
		return nil
	}
	stats, err := running.log.Stats(ctx)
	if err != nil {
		// THE STREAM ITSELF IS UNREADABLE, so there is no ceiling to
		// report headroom against and no first sequence to purge from.
		// Publishing a floor here would name a fleet state nobody
		// observed, so the tick says so and leaves the previous floor
		// standing — which is older, and honestly labelled by its own
		// `at`.
		return fmt.Errorf("read the stream's state: %w", err)
	}
	generation := running.runner.Committed().Generation

	in := statelog.TrimInputs{
		Generation:      generation,
		Now:             shared.at,
		CountedReadable: shared.readable,
		HoldsReadable:   true,
		BackupMaxAge:    r.cfg.BackupMaxAge(),
		HoldStale:       statelog.TrimHoldStale,
	}
	in.Counted = statelog.CountedSet(shared.at,
		reportedPositions(shared.positions, name),
		shared.live, r.tombstones(ctx, running, generation))
	in.Holds = holdsFor(shared.holds, running.domain.Stream().Name)
	in.BackupFloor, in.BackupAt, in.BackupFloorGen, in.HasBackupFloor =
		r.backupTerm(shared.backups, running.domain.Stream().Name)
	in.FeedAckFloor, in.HasFeed, in.FeedReadable = r.feedTerm(ctx, running)
	in.AgeFloor, err = r.ageFloor(ctx, running.log, stats, r.cfg.MinAge(), shared.at)
	if err != nil {
		return fmt.Errorf("find the age floor: %w", err)
	}

	decision := statelog.Trim(in.Terms())
	r.gauges(name, stats, decision, shared)
	if !decision.Blocked() && decision.To > stats.FirstSeq {
		if err := running.log.Purge(ctx, decision.To); err != nil {
			return fmt.Errorf("purge below %d: %w", decision.To, err)
		}
		log.InfoContext(ctx, "retention_trimmed", "domain", name,
			"trim_to", decision.To, "was", stats.FirstSeq,
			"generation", generation)
	}
	return r.publish(ctx, name, generation, decision, shared)
}

// publish writes what this tick concluded.
//
// THE ONE PIECE OF STATE THAT SURVIVES A LEASE HANDOVER is `blocked_since`,
// and it is carried through the register rather than in memory for exactly
// that reason: the duty moves, and a value held by the holder would reset on
// every flap — so the twenty-four-hour backup condition would never be
// reached, which is the bug the field exists to close.
func (r *retention) publish(ctx context.Context, name string, generation uint32,
	decision statelog.TrimDecision, shared fleetInputs) error {

	row := coord.TrimFloor{
		Domain:     name,
		Generation: generation,
		TrimTo:     decision.To,
		BlockedBy:  string(decision.BlockedBy),
		At:         shared.at,
		By:         r.nodeID,
	}
	for _, t := range decision.Terms {
		row.Terms = append(row.Terms, coord.TrimTerm{
			Name: string(t.Name), Seq: t.Seq, Known: t.Known,
			Absent: t.Absent, Detail: t.Detail,
		})
	}
	previous, had := shared.previous[name]
	switch {
	case !decision.Blocked():
		// NOT BLOCKED IS NOT "blocked since now": the field is only
		// meaningful while something is stopping the trim, and leaving
		// a stale instant on an advancing domain would age into an
		// alarm on a healthy fleet.
		row.BlockedSince = time.Time{}
	case had && previous.Blocked() && !previous.BlockedSince.IsZero():
		// STILL BLOCKED, so the clock keeps running — and it keeps
		// running ACROSS A CHANGE OF TERM, deliberately. A fleet whose
		// blocking term rotates between `applied` and `backup_floor`
		// while the trim goes nowhere has been blocked throughout, and
		// restarting the clock on each rotation would hide the longest
		// outages behind the noisiest ones.
		//
		// There is no third case comparing the POINT, because there is
		// no point to compare: [statelog.Trim] reports Blocked exactly
		// when it permits removing up to zero, so a blocked tick's
		// TrimTo is always 0 and a "blocked further along than last
		// time" state does not exist.
		row.BlockedSince = previous.BlockedSince
	default:
		row.BlockedSince = shared.at
	}
	if err := r.fleet.PutFloor(ctx, row); err != nil {
		return fmt.Errorf("publish the trim floor: %w", err)
	}
	if decision.Blocked() {
		// ONE LINE PER TICK PER DOMAIN, at WARN, naming the term, what
		// it has and what it wants — because the published field is
		// invisible to anyone watching logs and the log line is
		// invisible to anyone watching a dashboard.
		log.WarnContext(ctx, "retention_trim_blocked", "domain", name,
			"term", decision.BlockedBy, "detail", decision.Detail,
			"blocked_since", row.BlockedSince, "trim_to", decision.To)
	}
	return nil
}

// reportedPositions is every node's committed position in one domain.
func reportedPositions(rows []coord.NodePositions, domain string) []statelog.NodePosition {
	out := make([]statelog.NodePosition, 0, len(rows))
	for _, row := range rows {
		at, runs := row.Domains[domain]
		if !runs {
			// A NODE THAT DOES NOT RUN THIS DOMAIN IS NOT A NODE AT
			// POSITION ZERO. Counting it would pin every domain at
			// the floor of the fleet's least-configured node.
			continue
		}
		out = append(out, statelog.NodePosition{
			NodeID: row.NodeID, Generation: at.Generation, Seq: at.Seq,
			SnapshotSeq: at.SnapshotSeq, HasSnapshot: at.SnapshotSeq > 0,
			At: row.At,
		})
	}
	return out
}

// holdsFor is every live pin on one domain's log.
//
// BY STREAM NAME, because that is [coord.TrimHold.Streams]' key space — a hold
// is a pin on a log, and both the backup and a joining node state theirs from
// `statelog_cursor`, which is keyed on the stream.
func holdsFor(holds []coord.TrimHold, stream string) []statelog.Hold {
	out := make([]statelog.Hold, 0, len(holds))
	for _, hold := range holds {
		at, pins := hold.Streams[stream]
		if !pins {
			continue
		}
		out = append(out, statelog.Hold{
			Owner: hold.Owner, Generation: at.Generation, Seq: at.Seq, At: hold.At,
		})
	}
	return out
}

// backupTerm is the newest backup's reach in one domain's log, under whichever
// policy the operator declared. BY STREAM NAME, for [holdsFor]'s reason.
//
// The two policies differ in WHICH rows count and in nothing else: `engine`
// follows the copies this fleet's own nodes wrote and verified, `operator`
// follows the acknowledgement a person gave once the copy left the host — a
// fact the engine cannot see for itself.
func (r *retention) backupTerm(points []coord.BackupPoint, stream string) (
	seq uint64, at time.Time, generation uint32, have bool) {

	eligible := points
	if r.cfg.Floor() == config.BackupFloorOperator {
		eligible = nil
		for _, p := range points {
			if p.Owner == coord.OperatorBackupOwner {
				eligible = append(eligible, p)
			}
		}
	}
	newest, ok := coord.NewestBackup(eligible)
	if !ok {
		return 0, time.Time{}, 0, false
	}
	reach, covers := newest.Streams[stream]
	if !covers {
		// A BACKUP THAT DOES NOT COVER THIS LOG IS NOT A BACKUP AT
		// SEQUENCE ZERO — it is no backup of this domain at all, which
		// blocks. The distinction is the whole reason the term is
		// three-valued.
		return 0, time.Time{}, 0, false
	}
	return reach.Seq, newest.At, reach.Generation, true
}

// feedTerm is how far this domain's wake feed has acknowledged.
func (r *retention) feedTerm(ctx context.Context, running *runningDomain) (
	seq uint64, has, readable bool) {

	if !running.domain.ClaimsIdentity() {
		// THIS DOMAIN HAS NO WAKE FEED, which is absent rather than
		// unreadable: a compacted domain never had one, and reporting
		// zero would block its trim for ever on a term it does not
		// have.
		return 0, false, false
	}
	floor, exists, err := running.log.GroupAckFloor(ctx, tracker.FeedGroup)
	switch {
	case err != nil:
		return 0, true, false
	case !exists:
		// THE FEED HAS NOT BEEN CREATED YET, which is a fleet that has
		// never started one rather than one whose consumer could not be
		// read. It permits nothing, because a record no feed has seen is
		// one nobody has been told about — and that is exactly what the
		// term says.
		return 0, true, true
	}
	return floor, true, true
}

// tombstones is every eviction this node has applied for one domain's log.
//
// READ FROM THE REPLICATED ROWS rather than from coordination, because that is
// where an eviction actually lives: it is a record on the log like any other,
// so every node's copy is identical and the applier's own gate depends on this
// same table. A second copy in a bucket would give the fleet two answers.
//
// A read failure yields NO tombstones, which is the conservative direction: an
// evicted node stays counted and pins the floor, rather than the trim
// advancing past a node it could not establish was gone.
func (r *retention) tombstones(ctx context.Context, running *runningDomain,
	generation uint32) []statelog.Tombstone {

	if r.db == nil || !running.domain.ClaimsIdentity() {
		// ONLY THE IDENTITY-CLAIMING DOMAIN CARRIES EVICTIONS. A
		// domain that does not claim identity has no say in who the
		// fleet counts, and asking it would be reading another
		// domain's table under this one's stream name.
		return nil
	}
	rows, err := tracker.Evictions(ctx, r.db.Replicated(), running.domain.Stream().Name)
	switch {
	case errors.Is(err, store.ErrNoEstate) || errors.Is(err, context.Canceled):
		// A STOP THIS PROCESS ASKED FOR IS NOT AN UNREADABLE TABLE. The
		// replicated estate closes during shutdown and during an
		// adoption's rename, and a tick already in flight reaches it —
		// which is the honest answer rather than a fault, and logging
		// it at WARN would put a line in every clean shutdown.
		return nil
	case err != nil:
		log.WarnContext(ctx, "retention_evictions_unreadable", "err", err)
		return nil
	}
	out := make([]statelog.Tombstone, 0, len(rows))
	for _, row := range rows {
		if row.IsBack {
			// READMITTED, so there is no tombstone: the node is
			// counted again and its position pins the floor as any
			// other node's does.
			continue
		}
		out = append(out, statelog.Tombstone{
			NodeID: row.NodeID, At: row.At, By: row.By, Generation: generation,
		})
	}
	return out
}

// ageFloor is the first sequence the age term will keep.
//
// # Why a binary search rather than a stored index
//
// The log is ordered by sequence and its timestamps are the BROKER's own, so
// they are monotone in sequence — which makes "the newest record older than T"
// a binary search over [first, last] with one [jetstream.DomainLog.At] per
// step. At a year-five log that is about twenty-eight reads a quarter of an
// hour, which is cheaper than any index that would have to be maintained on
// every append and correct after every purge.
//
// It answers the FIRST sequence to KEEP, exclusive-style like every other
// term: a log whose newest record is already older than the floor answers
// last+1 (everything may go, as far as this term is concerned), and one whose
// oldest record is younger answers first (nothing may go).
func (r *retention) ageFloor(ctx context.Context, log *jetstream.DomainLog,
	stats jetstream.LogStats, minAge time.Duration, now time.Time) (uint64, error) {

	return ageFloorOf(stats.FirstSeq, stats.LastSeq, stats.Messages,
		now.Add(-minAge), func(seq uint64) (time.Time, bool, error) {
			_, _, storedAt, ok, err := log.At(ctx, seq)
			return storedAt, ok, err
		})
}

// ageFloorOf is the arithmetic, over anything that can be probed by sequence.
//
// SEPARATED FROM THE BROKER deliberately, on this subsystem's own rule: a
// policy that can only be exercised through a live broker is one nobody
// re-checks, and every interesting case here — an empty log, a log entirely
// older than the cutoff, one entirely younger, and one with a purged hole in
// the middle — is reachable in a table test with no broker at all.
func ageFloorOf(first, last, messages uint64, cutoff time.Time,
	at func(seq uint64) (time.Time, bool, error)) (uint64, error) {

	if messages == 0 || first > last {
		// An empty log has nothing to keep and nothing to remove; the
		// term must not block, so it permits everything up to the head.
		return last + 1, nil
	}
	var probeErr error
	span := int(last - first + 1)
	idx := sort.Search(span, func(i int) bool {
		if probeErr != nil {
			return true
		}
		storedAt, ok, err := at(first + uint64(i))
		if err != nil {
			probeErr = err
			return true
		}
		if !ok {
			// A GAP IS NOT AN ERROR AND IS NOT YOUNG. A purged
			// sequence answers "not found", and treating it as
			// BELOW the cutoff is correct: anything already gone is
			// older than anything still held, so the search keeps
			// moving up rather than stopping on a hole.
			return false
		}
		return !storedAt.Before(cutoff)
	})
	if probeErr != nil {
		return 0, probeErr
	}
	return first + uint64(idx), nil
}

// gauges publishes the capacity and gate figures a collector reads.
//
// # Why here and not where each number is produced
//
// Every one of them is a fact about a DOMAIN at a moment, and this tick is the
// only place that holds all of them at once: the stream's size and ceiling
// come from one info call, the blocked term from one evaluation, and the
// backup's age from one register read. Setting them anywhere else would mean
// re-fetching, and a gauge set from a second fetch describes a state the
// decision beside it was not taken against.
//
// A NIL RECORDER RECORDS NOTHING, which is a legal deployment: the numbers are
// on the report either way, and a collector is what this adds.
func (r *retention) gauges(domain string, stats jetstream.LogStats,
	decision statelog.TrimDecision, shared fleetInputs) {

	if r.metrics == nil {
		return
	}
	at := metrics.Attrs{"domain": domain}
	r.metrics.Set(metrics.StatelogLogBytes, float64(stats.Bytes), at)
	if stats.MaxBytes > 0 {
		// HEADROOM ONLY WHERE THERE IS A CEILING. A fraction of an
		// unbounded log is not zero headroom, and zero is the value the
		// one alarm an operator cannot ignore fires on.
		r.metrics.Set(metrics.StatelogLogMaxBytes, float64(stats.MaxBytes), at)
		free := float64(stats.MaxBytes-min(stats.Bytes, stats.MaxBytes)) /
			float64(stats.MaxBytes)
		r.metrics.Set(metrics.StatelogLogHeadroomFraction, free, at)
	}
	blocked := 0.0
	if decision.Blocked() {
		if previous, had := shared.previous[domain]; had && !previous.BlockedSince.IsZero() {
			blocked = shared.at.Sub(previous.BlockedSince).Seconds()
		}
	}
	r.metrics.Set(metrics.StatelogTrimBlockedSeconds, blocked, at)
}
