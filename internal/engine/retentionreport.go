package engine

import (
	"context"
	"os"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE RETENTION ANSWER, assembled on whichever node was asked.
//
// # Why any node can answer and not just the duty holder
//
// The question an operator runs this for is "why is the log not shrinking",
// and the honest answer has two halves. One is fleet-wide and lives in
// coordination — the positions, the holds, the backup points and the floor the
// trim published, which any node can read. The other is about THIS node — its
// own applier's lag, its snapshot, its disk — and is the half a node cannot
// answer about a peer.
//
// So the report is assembled locally from both, and says whose it is:
// [statelog.Report.NodeID] is on the document rather than beside it, because a
// report pasted into a ticket without its author is three per-node facts
// attributed to a fleet.
//
// # What it deliberately does NOT do
//
// It runs no trim tick of its own. The blocking term and how long it has been
// blocking are read from the published floor, because re-deriving them here
// would be a second opinion about one event — and `blocked_since` cannot be
// re-derived at all, since it is a property of a series of ticks rather than
// of any one of them.

// Report assembles this node's answer about the state log's retention.
func (r *retention) Report(ctx context.Context) statelog.Report {
	now := time.Now().UTC()
	in := statelog.ReportInputs{
		NodeID:      r.nodeID,
		At:          now,
		BackupOwner: r.backupOwner,
		Replica:     r.replica(),
	}

	// FLEET-WIDE, and every failure is reported as UNREADABLE rather than
	// as empty: an empty node block is a fleet with no nodes, which cannot
	// happen, and it is exactly the rendering somebody gets during the
	// outage they are running this command in.
	if positions, err := r.fleet.Positions(ctx); err == nil {
		in.Register, in.RegisterReadable = positions, true
	}
	if r.leases != nil {
		if live, err := livePresences(ctx, r.leases); err == nil {
			in.Live = live
		}
	}
	// AN UNREADABLE FLOOR REGISTER IS NOT AN EMPTY ONE: every domain then
	// reports its trim as unreadable rather than as a trim that has
	// concluded nothing, which is a different thing to go and look at.
	floors := map[string]coord.TrimFloor{}
	rows, floorsErr := r.fleet.Floors(ctx)
	for _, row := range rows {
		floors[row.Domain] = row
	}
	backups, _ := r.fleet.BackupPoints(ctx)
	newest, haveBackup := coord.NewestBackup(backups)

	// perLog is each identity-claiming log's own tombstones, which the node
	// block folds into one answer per node below.
	var perLog [][]statelog.Tombstone
	// healths is each domain's readiness, read ONCE here and handed to the
	// alarm reading below rather than read again there: both are one
	// node's answer about the same instant, and two reads a broker round
	// trip apart could disagree about it.
	healths := make(map[string]domainHealth, len(r.state.order))
	for _, name := range r.state.order {
		running := r.state.domains[name]
		if running == nil {
			continue
		}
		d := statelog.DomainInputs{
			Domain:     name,
			Stream:     running.domain.Stream().Name,
			Generation: running.runner.Committed().Generation,
			Replay:     running.domain.Stream().Replay,
			Reserved:   statelog.KeepsGateReserve(running.domain),
		}
		if stats, err := running.log.Stats(ctx); err == nil {
			d.FirstSeq, d.LastSeq = stats.FirstSeq, stats.LastSeq
			d.Bytes, d.MaxBytes = stats.Bytes, stats.MaxBytes
			d.StreamReadable = true
		}
		d.FloorState = statelog.TrimFloorNoneAtGeneration
		if floorsErr != nil {
			d.FloorState = statelog.TrimFloorUnreadable
		}
		if floor, published := floors[name]; published && floor.Generation == d.Generation {
			// THE FLOOR AND THE CONCLUSION ARE TWO FIELDS OF THE ROW,
			// and the report carries both: filled from one, the two
			// figures were always equal and a blocked domain reported
			// a floor of zero over a log a purge had already emptied.
			//
			// ONLY A ROW AT THIS DOMAIN'S OWN GENERATION, which is the
			// rule every reader of the floor follows ([floorFor], the
			// trim's own [nextFloor]): a row from another generation
			// is a conclusion about another number space. Just after a
			// reanchor the published row is the OLD stream's, so its
			// floor, its terms and its blocking term were printed
			// beside the adopted stream's first and last sequences as
			// though they bounded them — where the trim has concluded
			// nothing yet about the adopted one, which is what the
			// empty row says until its first tick at this generation.
			// A row ABOVE this node's generation is the fleet's on a
			// stream this node has not adopted; its numbers compare
			// with nothing this node holds, and a node on the new
			// generation reports them.
			d.TrimFloor = floor.Floor
			d.BlockedSince = floor.BlockedSince
			d.Decision = decisionOf(floor)
			d.FloorState = statelog.TrimFloorPublished
		}
		// THIS NODE'S OWN REFUSAL OF THE DOMAIN, from the readiness its
		// probe reads — so the not-ready state the guides send an operator
		// here to find is a line this report prints, naming the finding.
		health, healthErr := r.state.health(ctx, running)
		healths[name] = domainHealth{health: health, err: healthErr}
		if healthErr != nil {
			// THE PROBE'S OWN ANSWER to a health nobody could read.
			d.NotReady = &statelog.DomainRefusal{
				Code:   string(statelog.RefuseBrokerUnreachable),
				Detail: healthErr.Error(),
			}
		} else {
			d.NotReady = health.NotReady(now, d.Stream, running.runner.StreamIdentity())
		}
		if truncated := running.runner.Truncated(); truncated != nil {
			d.WritesRefused = &statelog.DomainRefusal{
				Code: string(statelog.ReasonLogTruncated), Detail: truncated.Error(),
			}
		}
		// THE EVICTIONS ARE NOT GATED ON A PUBLISHED FLOOR. They are
		// applied rows, present whether or not the trim has ever run,
		// and the node block renders an evicted node from them — so
		// gating them on the duty having ticked would make a fleet's
		// evictions invisible for the first fifteen minutes of its life
		// and for the whole life of a fleet whose duty is not running,
		// which is exactly when somebody is reading this.
		//
		// AN UNREAD LOG IS NAMED ON ITS OWN ROW, because the node block
		// cannot say it: the log contributes no tombstone, so every node
		// reads as not evicted there — the right side for the COUNTED
		// column and a guess for the EVICTED one.
		if running.domain.ClaimsIdentity() {
			tombs, read := r.tombstones(ctx, running, d.Generation)
			perLog = append(perLog, tombs)
			d.EvictionsUnreadable = !read
		}
		d.SnapshotSkip = r.skipFor(in.Register, name)
		in.Domains = append(in.Domains, d)
	}
	in.Tombstones = fleetTombstones(perLog)

	in.Reading = r.reading(ctx, now, newest, haveBackup, healths)
	in.Maintenance = r.openMaintenance(ctx)
	return statelog.NewReport(in)
}

// fleetTombstones folds each identity-claiming log's own tombstones into the
// one answer per node the report's node block renders: a node is shown evicted
// only where EVERY such log holds its tombstone, as of the LATEST of them.
//
// # Why the intersection, and why the latest
//
// The trim counts nodes per log, so "evicted" on the node block is a claim
// about all of them at once: a node evicted on the tracker's log and not yet on
// the pages log is still counted on the pages log and still pins it. Rendered
// from either log alone — which is what appending every log's rows did, the
// last one read winning — that node read as evicted, and the COUNTED column
// said no while one log's applied term waited on it. Until the gesture has
// reached every log the node is shown as it is on the log still counting it,
// which is also what the gesture's own answer tells the operator to finish.
//
// The window is measured from the latest tombstone because the node stops
// being counted on every log only once the last of them has aged past it; the
// earliest would call the eviction effective while one log still counts it.
//
// No log at all — a report assembled with no identity-claiming domain — is no
// tombstone.
func fleetTombstones(perLog [][]statelog.Tombstone) []statelog.Tombstone {
	if len(perLog) == 0 {
		return nil
	}
	latest := make(map[string]statelog.Tombstone, len(perLog[0]))
	held := make(map[string]int, len(perLog[0]))
	for _, tombs := range perLog {
		seen := make(map[string]bool, len(tombs))
		for _, t := range tombs {
			if seen[t.NodeID] {
				continue
			}
			seen[t.NodeID] = true
			held[t.NodeID]++
			if prev, ok := latest[t.NodeID]; !ok || t.At.After(prev.At) {
				latest[t.NodeID] = t
			}
		}
	}
	out := make([]statelog.Tombstone, 0, len(latest))
	for id, t := range latest {
		if held[id] == len(perLog) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// openMaintenance is the capacity operation currently holding the fleet, or
// nil.
//
// THE OLDEST OF THEM, for the reason the alarm takes the oldest: maintenance
// stops every publisher on every node, and two open operations do not stop it
// twice — what a reader needs is the one that has been holding longest.
//
// Read from the register rather than from whatever this node's own coordinator
// remembers, because an operation is opened by whichever node ran the verb and
// every other node is merely a PARTICIPANT — which is what makes maintenance
// an outage rather than an inconvenience, and what makes a per-node memory of
// it useless.
func (r *retention) openMaintenance(ctx context.Context) *statelog.MaintenanceReport {
	if r.fleet == nil {
		return nil
	}
	var oldest *statelog.MaintenanceReport
	for _, name := range r.state.order {
		running := r.state.domains[name]
		if running == nil {
			continue
		}
		op, open, err := r.fleet.Maintenance(ctx, running.domain.Stream().Name)
		if err != nil || !open {
			continue
		}
		row := &statelog.MaintenanceReport{
			Stream: op.Stream, OperationID: op.OperationID,
			Phase: string(op.Phase), Attempt: op.Attempt,
			TargetMaxBytes: op.TargetMaxBytes, OriginalMaxBytes: op.OriginalMaxBytes,
			Since: op.EnteredAt.UTC(), By: op.By,
			ParticipantsMissing: outstanding(ctx, r.fleet, op),
			Blocked:             op.Blocked,
		}
		if oldest == nil || row.Since.Before(oldest.Since) {
			oldest = row
		}
	}
	return oldest
}

// outstanding names the participants whose acknowledgement the seal is still
// waiting for.
//
// AN UNREADABLE ACK REGISTER YIELDS THE WHOLE PARTICIPANT SET rather than an
// empty one: "nobody is outstanding" is the state that says the operation is
// waiting on its operator, and reporting it because a read failed would send
// somebody to finish an operation that is still waiting on four nodes.
func outstanding(ctx context.Context, fleet coord.Fleet,
	op coord.MaintenanceOperation) []string {

	excluded := map[string]bool{}
	for _, node := range op.Excluded {
		excluded[node] = true
	}
	acked := map[string]bool{}
	acks, err := fleet.MaintenanceAcks(ctx)
	if err == nil {
		for _, ack := range acks {
			if ack.OperationID == op.OperationID && ack.Attempt == op.Attempt {
				acked[ack.NodeID] = true
			}
		}
	}
	var missing []string
	for _, node := range op.Participants {
		if !excluded[node] && !acked[node] {
			missing = append(missing, node)
		}
	}
	sort.Strings(missing)
	return missing
}

// decisionOf turns a published floor back into the decision that wrote it.
//
// THE PUBLISHED ROW IS THE AUTHORITY, not a fresh evaluation: which term came
// lowest is one tick's conclusion, and a reader that recomputed it would give
// a second answer about the same event — the failure the alarm table is
// written against.
func decisionOf(floor coord.TrimFloor) statelog.TrimDecision {
	d := statelog.TrimDecision{To: floor.TrimTo, BlockedBy: statelog.TermName(floor.BlockedBy)}
	for _, t := range floor.Terms {
		term := statelog.Term{
			Name: statelog.TermName(t.Name), Seq: t.Seq,
			Known: t.Known, Absent: t.Absent, Detail: t.Detail,
		}
		if term.Name == d.BlockedBy {
			d.Detail = t.Detail
		}
		d.Terms = append(d.Terms, term)
	}
	return d
}

// skipFor is this node's own snapshot skip reason for one domain, read back
// off the register it published rather than held in memory — so the answer is
// the same one every peer can see.
func (r *retention) skipFor(register []coord.NodePositions, domain string) statelog.SkipReason {
	for _, row := range register {
		if row.NodeID != r.nodeID {
			continue
		}
		if _, runs := row.Domains[domain]; runs {
			return statelog.SkipReason(row.SnapshotSkip)
		}
	}
	return ""
}

// replica is what THIS node costs to replace.
func (r *retention) replica() statelog.ReplicaReport {
	out := statelog.ReplicaReport{
		RejoinWindowSeconds: r.cfg.RejoinWindow().Seconds(),
	}
	if r.db != nil {
		if info, err := os.Stat(r.db.Replicated().Path()); err == nil {
			out.StoreBytes = info.Size()
		}
	}
	out.ProjectedJoinSeconds = statelog.ProjectJoinSeconds(out.StoreBytes)
	return out
}

// reading is this node's alarm inputs, MINUS the two the report evaluates per
// domain from the domain rows.
//
// A FIELD THIS NODE CANNOT MEASURE IS LEFT AT ITS ZERO VALUE, which every
// condition reads as "nothing to report" rather than as "at the floor" — see
// [statelog.Reading]. That is what lets a node with no recorder, no search
// backend and no maintenance in flight evaluate the same table as one with all
// three.
func (r *retention) reading(ctx context.Context, now time.Time,
	newest coord.BackupPoint, haveBackup bool,
	healths map[string]domainHealth) statelog.Reading {

	out := statelog.Reading{BackupMaxAge: r.cfg.BackupMaxAge()}
	if haveBackup {
		out.BackupAge = statelog.Age(now.Sub(newest.At))
	}
	// AND NO BACKUP AT ALL IS LEFT NIL, which is the state itself rather
	// than an age standing in for it. The alarm fires on nil exactly as it
	// fires on an age past the policy — "a company that never backs up
	// never trims" is the term it is about to hit — but it says which of
	// the two it is. What this replaces filled the field with
	// `BackupMaxAge + time.Hour` to reach the same threshold, and the
	// alarm then told a company four seconds old that its newest verified
	// backup was twenty-five hours old, one line above the trim term
	// reporting that no backup had been recorded at all.
	for _, name := range r.state.order {
		running := r.state.domains[name]
		if running == nil {
			continue
		}
		read, ok := healths[name]
		if !ok {
			read.health, read.err = r.state.health(ctx, running)
		}
		if read.err != nil {
			continue
		}
		health := read.health
		// THE WORST OF THE DOMAINS, because the reading describes one
		// NODE: a two-domain node whose second applier is wedged is a
		// node that is behind, and averaging would hide it.
		out.ApplyLag = max(out.ApplyLag, applyLagOf(health, running))
		if health.Deferred > 0 {
			out.DeferredAge = max(out.DeferredAge, statelog.DeferralGrace)
		}
		if health.TrimFloor == nil {
			out.FloorUnknownFor = max(out.FloorUnknownFor, statelog.FloorCacheStale)
		}
	}
	out.SemanticCoverage = r.semanticCoverage(ctx, now)
	if r.objectsMissing != nil {
		out.ObjectsMissing = r.objectsMissing()
	}
	r.space(&out)
	r.maintenance(ctx, now, &out)
	r.observed(&out)
	return out
}

// domainHealth is one domain's readiness as the report read it, or why it
// could not be read.
type domainHealth struct {
	health statelog.Health
	err    error
}

// maintenance is the oldest open capacity operation on any of this node's
// domains.
//
// THE OLDEST, because the alarm is about how long publishing has been stopped
// and two open operations do not stop it twice. Read from the register rather
// than from whatever this node's own coordinator remembers: an operation is
// opened by whichever node ran the verb, and a node that is merely a
// PARTICIPANT — which is every node, and is what makes maintenance an outage
// rather than an inconvenience — holds nothing about it in memory at all.
func (r *retention) maintenance(ctx context.Context, now time.Time, out *statelog.Reading) {
	if r.fleet == nil {
		return
	}
	for _, name := range r.state.order {
		running := r.state.domains[name]
		if running == nil {
			continue
		}
		op, open, err := r.fleet.Maintenance(ctx, running.domain.Stream().Name)
		if err != nil || !open || op.EnteredAt.IsZero() {
			continue
		}
		if since := now.Sub(op.EnteredAt); since > out.MaintenanceOpenFor {
			out.MaintenanceOpenFor, out.MaintenancePhase = since, string(op.Phase)
		}
	}
}

// applyLagOf converts a health's record backlog into the time the alarm table
// is written in.
//
// AT THE MEASURED DRAIN, so the number an alarm fires on is a duration rather
// than a count: "this node is 4m12s behind" is actionable and "this node is
// 500 000 records behind" is a number an operator has to divide.
func applyLagOf(health statelog.Health, running *runningDomain) time.Duration {
	if health.Lag == nil || *health.Lag == 0 {
		return 0
	}
	return time.Duration(*health.Lag) * time.Second /
		time.Duration(max(int64(running.runner.Drain()), 1))
}

// observed fills the fields that come from this process's own recorder.
//
// GUARDED ON THE RECORDER because a nil one is a legal deployment: an embedded
// engine and every test run without one, and every condition here reads a zero
// as "nothing to report".
func (r *retention) observed(out *statelog.Reading) {
	if r.metrics == nil {
		return
	}
	// EVERY READING HERE IS WINDOWED, and none may come from
	// [metrics.Recorder.Read]'s cumulative series.
	//
	// The alarms below apply a threshold, and a threshold against a counter
	// that only grows LATCHES: `search_degraded` fires on a fraction being
	// above zero, so one degraded search after boot lights it for the life
	// of the process and it can never go out; `search_slow` and
	// `barrier_slow` take a maximum, so one slow observation ever is
	// permanent. `crewlet retention status` derives its exit code from
	// these, so a cron watching it then fires for ever too. That is what
	// [metrics.Window] was written for, and reading Read() here is what
	// left it with no caller at all.
	reading := r.metrics.ReadWindow()

	// THE ANSWER COUNTERS FIRST, because two alarms are FRACTIONS of them
	// and a fraction needs its denominator before either numerator means
	// anything.
	var answers, scoped, degraded uint64
	for _, snapshot := range reading {
		if snapshot.Name != metrics.TrackerSearchAnswers {
			continue
		}
		answers += snapshot.Total
		if snapshot.Attrs["coverage"] == "scoped" {
			scoped += snapshot.Total
		}
		if snapshot.Attrs["semantic"] == "skipped" {
			degraded += snapshot.Total
		}
	}
	if answers > 0 {
		out.SearchScopedFraction = float64(scoped) / float64(answers)
		out.SearchDegradedFraction = float64(degraded) / float64(answers)
	}

	// THE DECLARED RATE, beside the observed one below. It is a constant
	// rather than a configured value because it is a term in the log's own
	// sizing: an operator who could set it would be silencing the alarm
	// rather than resizing the deployment it is about.
	out.LinearizableReadsExpected = statelog.LinearizableReadsPerDay

	for _, snapshot := range reading {
		switch snapshot.Name {
		case metrics.StatelogBarrierDuration:
			out.BarrierP95 = max(out.BarrierP95, quantileDuration(snapshot, 0.95))
		case metrics.TrackerSearchScanDuration:
			out.SearchP95 = max(out.SearchP95, quantileDuration(snapshot, 0.95))
		case metrics.StorePoolWait:
			// THE WORST FILE, not the sum of them. A caller queues on
			// ONE pool, and two estates each half-starved is not the
			// same node as one estate fully starved.
			out.PoolWaitP95 = max(out.PoolWaitP95, quantileDuration(snapshot, 0.95))
		case metrics.StatelogRecordsGated:
			out.RecordsGated += int(snapshot.Total)
		case metrics.TrackerFeedUnreadable:
			out.FeedUnreadable += int(snapshot.Total)
		case metrics.StatelogBarrierAppends:
			// THE BARRIER APPEND IS THE LINEARIZABLE READ. One is
			// appended per read that asks for the level, so the
			// counter and the census input are the same quantity —
			// which is exactly why the drift is checkable at all.
			out.LinearizableReads += int(snapshot.Total)
		case metrics.StatelogReadRefusals:
			// ANY REFUSAL AT ALL IS ONE, and the alarm's own
			// condition is a DURATION rather than a count — so what
			// this contributes is "there are refusals", and the
			// tracker beside it is what turns that into how long
			// they have been going on.
			if snapshot.Total > 0 && !refusalIsOrdinaryLag(snapshot) {
				out.RefusalsSince = max(out.RefusalsSince, statelog.RefusalAlarmFloor)
			}
		}
	}
}

// quantileDuration reads a histogram's quantile back as the duration its unit
// says it is.
func quantileDuration(s metrics.Snapshot, q float64) time.Duration {
	return time.Duration(s.Quantile(q) * float64(time.Millisecond))
}

// refusalIsOrdinaryLag reports whether a refusal series is one a caller only
// has to wait out.
//
// FROM THE CODE'S OWN CLASSIFICATION rather than from a list written here: the
// alarm exists to separate a fault from a wait, and a second list of which
// codes are waits is a second answer that drifts from the first. The names
// come off the series' own attribute, which is what the recorder wrote.
func refusalIsOrdinaryLag(s metrics.Snapshot) bool {
	code := statelog.ReadRefusal(s.Attrs["code"])
	return code == statelog.RefuseBehind || code == statelog.RefuseTooStale
}
