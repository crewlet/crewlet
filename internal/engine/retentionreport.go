package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
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
	now := r.clock()
	in := statelog.ReportInputs{
		NodeID:      r.nodeID,
		At:          now,
		BackupOwner: r.backupOwner,
		Replica:     r.replica(),
		// THE WINDOW EVERY LOG MUST HOLD, which is the age term's own
		// input: the trim never removes a record younger than it, so a
		// ceiling that cannot hold it is one no working trim can save.
		ReplayWindow: r.cfg.MinAge(),
	}

	// FLEET-WIDE, and every failure is reported as UNREADABLE rather than
	// as empty: an empty node block is a fleet with no nodes, which cannot
	// happen, and it is exactly the rendering somebody gets during the
	// outage they are running this command in.
	if positions, err := r.fleet.Positions(ctx); err == nil {
		in.Register, in.RegisterReadable = positions, true
	}
	// THE LIVE DATA NODES, LISTED ONCE and from the trim's own source — the
	// gate's own reading ([livePresences]): they are both who the node block
	// marks live and the half of every log's counted set that is not the
	// register ([statelog.ReportInputs.Live]). Listed once for each question,
	// every node's every tick paid a second certified listing a round trip
	// after the first, and a screen assembling the set differently from the
	// gate is how the two come to disagree about a peer. Unread, each log's
	// mark counts its rows alone.
	if r.holders != nil {
		if live, err := r.holders.LiveData(ctx); err == nil {
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
	// THE BACKUP POLICY IS AGED PER LOG, against the point each log's trim
	// counts — see [retention.newestCounted] and the `BackupAt` filled per
	// domain below.
	in.BackupMaxAge = r.cfg.BackupMaxAge()

	// perLog is each identity-claiming log's own tombstones, which the node
	// block folds into one answer per node below.
	var perLog [][]statelog.Tombstone
	// healths is each domain's readiness, read ONCE here and handed to the
	// alarm reading below rather than read again there: both are one
	// node's answer about the same instant, and two reads a broker round
	// trip apart could disagree about it.
	held := r.state.running()
	healths := make(map[string]domainHealth, len(held))
	// EACH LOG'S CENSUS, read off the recorder once for every log, as the
	// reading's other windowed figures are ([retention.census]).
	census := r.census(held)
	for _, running := range held {
		name := running.domain.Name()
		d := statelog.DomainInputs{
			Domain:      name,
			Stream:      running.spec.Name,
			Generation:  running.runner.Committed().Generation,
			Replay:      running.spec.Replay,
			Reservation: statelog.ReservationOf(running.domain),
		}
		if stats, err := running.log.Stats(ctx); err == nil {
			d.FirstSeq, d.LastSeq = stats.FirstSeq, stats.LastSeq
			d.Bytes, d.MaxBytes = stats.Bytes, stats.MaxBytes
			d.StreamReadable = true
		}
		// THE TICK'S MEASUREMENT, read back rather than taken: see
		// [retention.measureRates] for why a report never searches a log.
		d.BytesPerDay = r.rateOf(name)
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
			// floor, its terms and its blocking term — and its
			// `blocked_since`, dated from before the reanchor — were
			// printed beside the adopted stream's first and last
			// sequences as though they bounded them, where the trim
			// has concluded nothing yet about the adopted one, which
			// is what the empty row says until its first tick at this
			// generation. A row ABOVE this node's generation is the
			// fleet's on a stream this node has not adopted; its
			// numbers compare with nothing this node holds, and a node
			// on the new generation reports them.
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
			d.Tombstones = tombs
		}
		d.SnapshotSkip = r.skipFor(in.Register, running)
		r.deferral(&d, running, now)
		// THE HEARTBEAT'S OBSERVATION OF THIS LOG'S FLOOR, read back rather
		// than taken, because its age is an interval between observations —
		// one taken per poll would make it whatever the poll rate is.
		d.FloorUnknownFor, d.FloorUnknownCause = r.floors.of(name)
		if c, counted := census[name]; counted {
			d.LinearizableReads, d.LinearizableReadsExpected = c.reads, c.expected
		}
		// THE FIELD THIS LOG'S CEILING IS SET BY, from the register's own
		// declaration ([capacityBounds]), which `census_drift` names on
		// the log's line: each log is sized by its own field.
		if _, ceiling, known := capacityBounds(running.spec.Name); known {
			d.CeilingField = ceiling.Field
		}
		// THE POINT THIS LOG'S TRIM COUNTS, under the operator's own
		// policy and for this log alone — not the newest of every owner,
		// and not the newest point whatever it covers. Under
		// `backup_floor: operator` the nodes' own nightly copies are fresh
		// while the trim waits on an acknowledgement, and each log is
		// acknowledged on its own: an alarm aging any one point stayed
		// green beside the logs that point left out.
		if newest, ok := r.newestCounted(backups, running.spec.Name); ok {
			d.BackupAt = newest.At
		}
		in.Domains = append(in.Domains, d)
	}
	in.Tombstones = fleetTombstones(perLog)

	// THE CAPACITY WINDOWS, READ ONCE and handed to both the banner and the
	// alarm, for the healths' reason: two reads a round trip apart could
	// disagree about whether a window is open, and the report would show one
	// answer under an alarm evaluated on the other. A log whose window
	// couldn't be read is named as such ([readWindows]), never read as one
	// with no window.
	windows := readWindows(ctx, r.fleet)
	in.Reading = r.reading(ctx, now, healths, r.windows.observe(ctx, now, windows))
	in.Maintenance = r.openMaintenance(ctx, windows)
	in.MaintenanceUnread = windows.unread
	return statelog.NewReport(in)
}

// deferral fills one log's held record: the oldest this node's applier retains
// on it and cannot decode, how long it has been held, and whether holding it
// past the grace takes this node's copy of the estate out of service.
//
// THE MEASURED AGE, from the two facts the copy's judgement itself reads — the applier
// holding a record it cannot decode NOW, and the position heartbeat's first
// sighting of it — through the one arithmetic both share
// ([statelog.DeferredSince.Age]). It stood the grace in for the age once, so a
// rule firing past the grace compared the grace with itself and could never
// fire.
//
// NOTHING HERE READS COORDINATION: both facts are this process's own memory,
// so a coordination blip that fails a health read — or the floor, or the
// register — neither clears a firing alarm nor hides a new one.
//
// PER LOG, which is the one arrangement in which the consequence the alarm
// states is true: the question [stateLog.judgeCopy] asks before it lets a
// deferral take the copy out of service is the log's own
// [statelog.Domain.ReadinessInput], so two logs holding records at once are two
// different answers.
func (r *retention) deferral(d *statelog.DomainInputs, running *runningLog,
	now time.Time) {

	record, holding := running.runner.Deferred()
	if !holding {
		return
	}
	d.DeferredAge = running.progress.deferredSinceValue().Age(now)
	d.DeferredRecord = fmt.Sprintf("at %s, written at record version %d against "+
		"the %d this build reads", record.Position, record.Version,
		running.domain.RecordVersion())
	d.DeferredJudgesCopy = running.domain.ReadinessInput()
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
// And the latest is taken WHOLE, so the operator it names is the one whose
// gesture made the eviction the fleet's.
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

// openMaintenance is every capacity operation one read found holding the fleet,
// the oldest first, as the report shows it.
//
// EVERY ONE, and the oldest first because it has been holding longest: a window
// is per log and two can be open at once, each waiting on its own nodes, so
// one reported alone hid what the other was waiting on. The alarm still takes
// the oldest ([oldestWindow]): two windows do not stop publishing
// twice.
//
// Read from the register rather than from whatever this node's own coordinator
// remembers, because an operation is opened by whichever node ran the verb and
// every other node is merely a PARTICIPANT — which is what makes maintenance
// an outage rather than an inconvenience, and what makes a per-node memory of
// it useless. A log whose window the read couldn't answer is not here: it is
// the report's `maintenance_unread`, beside it.
func (r *retention) openMaintenance(ctx context.Context, read windowsRead) []statelog.MaintenanceReport {
	var open []statelog.MaintenanceReport
	for _, op := range read.open {
		row := statelog.MaintenanceReport{
			Stream: op.Stream, OperationID: op.OperationID,
			Phase: string(op.Phase), Attempt: op.Attempt,
			TargetMaxBytes: op.TargetMaxBytes, OriginalMaxBytes: op.OriginalMaxBytes,
			Since: op.EnteredAt.UTC(), By: op.By, OperatorID: op.OperatorID,
			Blocked:     op.Blocked,
			AbandonedBy: op.AbandonedBy,
		}
		// WHAT COULDN'T BE READ IS SAID AS THAT, never as a list: a list
		// formed from nothing named every participant as owing a restart,
		// or no admission as holding the window, and both surfaces print
		// either as a fact. ONE READ OF THE ACKNOWLEDGEMENTS serves the
		// list and the block's sentence, which an abandoned window's seal
		// decides ([statelog.BlockedDetail]).
		acks, err := windowAcks(ctx, r.fleet, op)
		r.windows.part(ctx, op.Stream, partAcks, err)
		row.AcksUnread = err != nil
		if err == nil {
			row.ParticipantsMissing = outstanding(op, acks)
		}
		row.BlockedDetail = statelog.BlockedDetail(op, acks, err)
		held, err := holdingAdmissions(ctx, r.fleet, op)
		r.windows.part(ctx, op.Stream, partAdmissions, err)
		row.AdmissionsBlocking, row.AdmissionsUnread = held, err != nil
		open = append(open, row)
	}
	return open
}

// outstanding names the participants the window's next step is still waiting
// for: at `opened`, those with no restart into a maintenance mode recorded
// ([statelog.BaselineMissing], what the baseline reads); from the baseline to
// the seal, those whose acknowledgement the seal does not take yet
// ([statelog.SealOutstanding], the seal's own rule); and once it has sealed,
// nobody, since the verify and the release wait on no node. At `opened` the
// seal's reading named every node as owing an acknowledgement bound to the
// window, which no node writes before the verb runs, so a banner said the
// window was waiting on the whole fleet while the next step was the
// operator's. Past it, a reading of its own that counted any acknowledgement
// of the attempt said nothing was outstanding while the seal still waited on a
// node in maintenance mode. And read at `sealed` or `confirmed`, the seal's
// rule judges acknowledgements the seal has already taken against whatever
// each node has written since: a node restarted again after the seal (into
// maintenance mode, which a boot refused in service is told to do) was named
// as outstanding by a window waiting on nobody but its operator.
//
// IT IS FORMED ONLY FROM ACKNOWLEDGEMENTS THAT WERE READ: an unreadable
// register answered the whole participant set, which every surface printed as
// every node still owing a restart, and "nobody" would have sent somebody to
// finish an operation still waiting on four nodes. Neither is known, so the
// report says it couldn't read them ([windowAcks]).
func outstanding(op coord.MaintenanceOperation, acks []coord.MaintenanceAck) []string {
	switch op.Phase {
	case coord.PhaseSealed, coord.PhaseConfirmed:
		return nil
	case coord.PhaseOpened:
		return statelog.BaselineMissing(op, acks)
	}
	return statelog.SealOutstanding(op, acks)
}

// windowAcks reads a window's acknowledgements where anything about it reads
// them: every phase before the seal holds, which is what [outstanding] and an
// abandoned window's sentence judge. Once it has sealed nothing does, and none
// is read.
func windowAcks(ctx context.Context, fleet coord.Fleet,
	op coord.MaintenanceOperation) ([]coord.MaintenanceAck, error) {

	switch op.Phase {
	case coord.PhaseSealed, coord.PhaseConfirmed:
		return nil, nil
	}
	if fleet == nil {
		return nil, errNoFleet
	}
	return fleet.MaintenanceAcks(ctx, op.Stream)
}

// holdingAdmissions names the nodes whose admission keeps a window at `opened`
// from sending anything ([statelog.AdmissionBlocks]), and none past it, where
// no step reads one. An unreadable register is an error rather than nobody: an
// empty list reads as nothing holding the window.
func holdingAdmissions(ctx context.Context, fleet coord.Fleet,
	op coord.MaintenanceOperation) ([]string, error) {

	if op.Phase != coord.PhaseOpened {
		return nil, nil
	}
	if fleet == nil {
		return nil, errNoFleet
	}
	admissions, err := fleet.Admissions(ctx)
	if err != nil {
		return nil, err
	}
	return statelog.AdmissionBlocks(admissions, op.Excluded), nil
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

// skipFor is this node's own snapshot skip reason for one log — the estate's,
// since an artefact is a copy of the estate's file and covers every log — read
// back off the register it published rather than held in memory, so the
// answer is the same one every peer can see.
func (r *retention) skipFor(register []coord.NodePositions, running *runningLog) statelog.SkipReason {
	for _, row := range register {
		if row.NodeID != r.nodeID {
			continue
		}
		if _, runs := row.Domains[running.domain.Name()]; runs {
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
	// THE REPLICATED ESTATE'S FILE, because replacing the node is adopting
	// it. Measured at its path whether or not it is open at this instant:
	// an adoption holding it closed is replacing it, and the file the node
	// would have to be given again is still the one at that name.
	if path := r.db.ReplicatedFile(); path != "" {
		if info, err := os.Stat(path); err == nil {
			out.StoreBytes += info.Size()
		}
	}
	out.ProjectedJoinSeconds = statelog.ProjectJoinSeconds(out.StoreBytes)
	return out
}

// reading is this node's alarm inputs, MINUS the per-domain ones — the log's
// headroom, its ceiling against its window, a blocked trim and what it keeps,
// the age of the backup its trim counts, a record held past the deferral grace
// and an unreadable floor — which the report evaluates from the domain rows and
// [retention.Report]'s own per-domain inputs.
//
// A FIELD THIS NODE CANNOT MEASURE IS LEFT AT ITS ZERO VALUE, which every
// condition reads as "nothing to report" rather than as "at the floor" — see
// [statelog.Reading]. That is what lets a node with no recorder, no search
// backend and no maintenance in flight evaluate the same table as one with all
// three.
func (r *retention) reading(ctx context.Context, now time.Time,
	healths map[string]domainHealth, windows []judgedWindow) statelog.Reading {

	var out statelog.Reading
	for _, running := range r.state.running() {
		name := running.domain.Name()
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
	}
	out.SemanticCoverage = r.semanticCoverage()
	if r.index != nil {
		r.index(ctx, &out)
	}
	if r.objects != nil {
		r.objects(now, &out)
	}
	if r.models != nil {
		r.models(ctx, now, &out)
	}
	if r.feeds != nil {
		r.feeds(now, &out)
	}
	r.space(&out)
	oldestWindow(now, windows, &out)
	r.observed(&out)
	r.burning(&out)
	return out
}

// burning fills the service objectives this node is spending the error budget
// of too fast.
//
// THE OBJECTIVES' OWN JUDGEMENT, read whole: the meter owns the targets, the
// window pairs and their factors, and the alarm restates none of them
// (ADR-0015). A nil meter judges nothing burning.
func (r *retention) burning(out *statelog.Reading) {
	out.ObjectiveBurns = r.objectives.Burning()
}

// domainHealth is one domain's readiness as the report read it, or why it
// could not be read.
type domainHealth struct {
	health statelog.Health
	err    error
}

// oldestWindow is the oldest capacity window the alarm judges open.
//
// THE OLDEST, because the alarm is about how long publishing has been stopped
// and two open operations do not stop it twice. JUDGED, not merely read
// ([windowWatch.observe]): a window this node last read open and can't read
// now is still the window it knows of, AS OLD AS IT WAS WHEN THAT READ
// ANSWERED, never aged on this node's clock since. Aged, a window last read
// open at 59 minutes crossed the hour during a two-minute blip and raised the
// alarm about a window that might have closed meanwhile. So a blip neither
// clears a firing alarm nor raises one, and the detail says when the figure
// was read.
func oldestWindow(now time.Time, windows []judgedWindow, out *statelog.Reading) {
	for _, w := range windows {
		if w.op.EnteredAt.IsZero() {
			continue
		}
		if since := w.readAt.Sub(w.op.EnteredAt); since > out.MaintenanceOpenFor {
			out.MaintenanceOpenFor, out.MaintenancePhase = since, string(w.op.Phase)
			out.MaintenanceStream = w.op.Stream
			out.MaintenanceUnreadable, out.MaintenanceReadAgo = w.unreadable, 0
			if w.unreadable {
				out.MaintenanceReadAgo = now.Sub(w.readAt)
			}
		}
	}
}

// applyLagOf converts a health's record backlog into the time the alarm table
// is written in.
//
// AT THE MEASURED DRAIN, so the number an alarm fires on is a duration rather
// than a count: "this node is 4m12s behind" is actionable and "this node is
// 500 000 records behind" is a number an operator has to divide.
func applyLagOf(health statelog.Health, running *runningLog) time.Duration {
	if health.Lag == nil {
		// UNMEASURED IS NOT ZERO anywhere else in this file, and it is
		// zero here for one reason: an alarm reading has no third value
		// either, so the only honest thing a reading can say about a
		// lag nobody could measure is nothing.
		return 0
	}
	// ONE IMPLEMENTATION of the arithmetic, shared with the figure the
	// request path reads. Written twice, an alarm firing at the stall
	// grace and a session answering 503 at the same grace would
	// eventually disagree about the same node.
	return lagDurationOf(*health.Lag, 0, running.runner.Drain())
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

	// AND THE FLEET'S HISTORY READS, the same fraction over a different
	// counter: every question counts, because a partial turn and a partial
	// event page are the same missing node.
	var reads, partial uint64
	for _, snapshot := range reading {
		if snapshot.Name != metrics.HistoryAnswers {
			continue
		}
		reads += snapshot.Total
		if snapshot.Attrs["coverage"] == historyPartial {
			partial += snapshot.Total
		}
	}
	if reads > 0 {
		out.HistoryPartialFraction = float64(partial) / float64(reads)
	}
	// AND THE BUDGET THOSE READS WAITED ON, which the alarm's sentence names.
	// Read off the fan-out's own constant rather than restated: statelog
	// cannot import the layer above it, so the reading is where the one
	// number crosses, and a second copy of it in the alarm text would drift
	// the day somebody re-tuned the fan-out.
	out.HistoryReadBudget = eventfan.FleetReadBudget

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

// logCensus is one log's census half: the barrier records it received over
// the window, and the census it was sized for.
type logCensus struct {
	reads, expected int
}

// census is each log's census half, keyed by the log's domain: of every log
// this node applies whose domain has a read index, the barrier records it
// received over the window and the census it was sized for — evaluated PER LOG
// by the report ([statelog.DomainInputs.LinearizableReads]), because each log
// was sized for its own census and is past it on its own. Nil where this
// process keeps no recorder, which every condition reads as nothing to report.
//
// # The rate is the LOG'S, read where it is applied
//
// A barrier record is what a linearizable read costs the log, so the census
// input is the barrier records a log received — and every data node applies
// every record, while a node without `data` applies none and keeps no recorder,
// so the count this node's applier took of them is the whole fleet's, not this
// node's. What this replaced summed the barriers THIS NODE appended,
// which on a fleet of several serving nodes is only its share of the reads: a
// three-node fleet exceeding its census threefold read as a company exactly at
// it, and the alarm could not fire until the company was six times past what
// its logs were sized for.
//
// # The census is per seat, plus the engine's own
//
// Declared rather than configured — it is a term in the log's own sizing, so
// an operator who could set it would be silencing the alarm rather than
// resizing the deployment it is about — by each domain for its own log in the
// register ([registration.Reads]), scaled by the company's own seats, plus
// what the engine's own periodic reads put on the log whatever the seats do
// ([statelog.Census]).
//
// WINDOWED, like every alarm input [retention.observed] reads: a count since
// the process started only grows, and a threshold against it latches.
func (r *retention) census(running []*runningLog) map[string]logCensus {
	if r.metrics == nil {
		return nil
	}
	received := map[string]int{}
	for _, snapshot := range r.metrics.ReadWindow() {
		if snapshot.Name == metrics.StatelogBarriersApplied {
			received[snapshot.Attrs["stream"]] += int(snapshot.Total)
		}
	}
	seats := 0
	if r.seats != nil {
		seats = r.seats()
	}
	background := func(string) int { return 0 }
	if r.background != nil {
		background = r.background
	}
	out := make(map[string]logCensus, len(running))
	for _, l := range running {
		reads, declared := readModel(l.domain)
		if !declared {
			// A LOG NO READ APPENDS TO has no census to drift from: its
			// domain has no read index, so nothing it receives is a
			// linearizable read.
			continue
		}
		name := l.domain.Name()
		out[name] = logCensus{
			reads: received[l.spec.Name],
			expected: statelog.Census{
				Reads:      reads,
				Seats:      seats,
				Background: background(name),
			}.Expected(),
		}
	}
	return out
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
