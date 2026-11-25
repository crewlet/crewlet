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

// THE RETENTION ANSWER, assembled on whichever node was asked, not just the duty
// holder. "Why is the log not shrinking" has two halves: fleet-wide state in
// coordination (positions, holds, backup points, the published floor), which any
// node reads, and THIS node's own applier lag, snapshot and disk, which no node
// can answer for a peer. So the report is assembled locally from both and says
// whose it is: [statelog.Report.NodeID] is on the document, since a pasted
// report without its author attributes per-node facts to a fleet. It runs no
// trim tick: the blocking term and its age are read from the published floor, a
// second derivation would be a second opinion, and `blocked_since` is a property
// of a series of ticks and cannot be re-derived.

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
	// AN UNREADABLE FLOOR IS NOT AN ABSENT ONE: a domain whose floor could
	// not be read reports its trim as unreadable rather than as a trim that
	// has concluded nothing, which is a different thing to go and look at —
	// and only that domain does, since each domain's floor is its own key.
	//
	// READ ONCE, for the rows below and for every domain's readiness: two
	// reads of the floors a round trip apart could disagree about the floor
	// a row prints and the floor its refusal was judged by.
	read := readFloorsOf(ctx, r.fleet, registeredNames())
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
	// reading's other windowed figures are ([retention.census]) — and sized,
	// for a log whose census counts readers of its own, by the ceiling the
	// stream read below finds its stream HOLDING.
	census := r.census(held)
	// WHAT EACH LOG'S WAKE FEED GAVE UP ON, read once for every feed on every
	// report — the beat's above all — so each entry's age and the time it
	// has left are now's, and an entry that vanished at its expiry is counted
	// as the wake it lost (feeddeadletters.go): one count of every feed's
	// entries from the broker, and a listing of a feed's only when that count
	// moved. A read that failed carries the last listing that answered.
	feedLetters := r.feedLetters.read(ctx, r.feedSources, now)
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
		floor, published, floorErr := read.row(name)
		if floorErr != nil {
			d.FloorState = statelog.TrimFloorUnreadable
		}
		if published && floor.Generation == d.Generation {
			// THE FLOOR AND THE CONCLUSION ARE TWO FIELDS OF THE ROW, and
			// the report carries both: filled from one they were always equal
			// and a blocked domain reported a floor of zero over an emptied log.
			//
			// ONLY A ROW AT THIS DOMAIN'S OWN GENERATION, as every reader of
			// the floor follows ([floorFor], [nextFloor]): another generation's
			// row concludes about another number space. After a reanchor the
			// published row is the OLD stream's, and printing its floor, terms
			// and `blocked_since` beside the adopted stream's sequences would
			// present them as bounds; the empty row says the trim has concluded
			// nothing yet. A row ABOVE this node's generation compares with
			// nothing this node holds; a node on the new generation reports it.
			d.TrimFloor = floor.Floor
			d.BlockedSince = floor.BlockedSince
			d.Decision = decisionOf(floor)
			d.FloorState = statelog.TrimFloorPublished
		}
		// THIS NODE'S OWN REFUSAL OF THE DOMAIN, from the readiness its
		// probe reads — so the not-ready state the guides send an operator
		// here to find is a line this report prints, naming the finding.
		health, healthErr := r.state.health(ctx, running, read)
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
		d.FeedDeadLetters = feedLetters[name]
		// AT THE CEILING THE STREAM READ ABOVE FOUND IT HOLDING: a log whose
		// census counts readers of its own and whose stream this pass could
		// not read is left uncounted rather than judged without them, where
		// a healthy git host's reads would read as the seats' over-reading.
		// The next pass that reads the stream judges it.
		if c, counted := census[name]; counted {
			if expected, sized := c.expectedAt(d.MaxBytes, d.StreamReadable, r.cfg.MinAge()); sized {
				d.LinearizableReads, d.LinearizableReadsExpected = c.reads, expected
			}
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
// and cannot decode, and how long it has been held.
//
// THE MEASURED AGE, from the two facts the copy's judgement reads (the applier
// holding an undecodable record NOW, the heartbeat's first sighting of it)
// through the one arithmetic both share ([statelog.DeferredSince.Age]); the
// grace never stands in for the age, or a rule past the grace would compare it
// with itself and never fire. NOTHING HERE READS COORDINATION: both facts are
// this process's memory, so a blip neither clears a firing alarm nor hides a
// new one. PER LOG, and EVERY log's whatever it declares about seat admission:
// a deferral past the grace takes that log's copy out of service only
// ([stateLog.judgeCopy], ADR-0079).
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
}

// fleetTombstones folds each identity-claiming log's own tombstones into the
// one answer per node the report's node block renders: a node is shown evicted
// only where EVERY such log holds its tombstone, as of the LATEST of them.
//
// The trim counts nodes per log, so a node evicted on one log and not yet on
// another is still counted, and pins, the other; shown from either alone it
// reads evicted while a log's applied term waits on it. The window runs from the
// LATEST tombstone (the node stops being counted only once the last has aged
// past it), taken WHOLE so the operator named made the eviction the fleet's. No
// identity-claiming domain at all is no tombstone.
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
// EVERY ONE, oldest first: a window is per log and two can be open at once, each
// waiting on its own nodes, so one reported alone hid what the other awaited.
// The alarm still takes the oldest ([oldestWindow]). Read from the register, not
// this node's coordinator, because whichever node ran the verb opened the
// operation and every other is merely a PARTICIPANT, so a per-node memory is
// useless. A log whose window could not be read is the report's
// `maintenance_unread`, not here.
func (r *retention) openMaintenance(ctx context.Context, read windowsRead) []statelog.MaintenanceReport {
	var open []statelog.MaintenanceReport
	for _, op := range read.open {
		if r.judge != nil {
			// UNJUDGED, NOBODY IS WAIVED: each retired participant is then
			// reported as owing what any participant owes, the block's
			// sentence says it couldn't judge, and the log says why.
			var err error
			op, err = r.judge(ctx, op)
			r.windows.part(ctx, op.Stream, partRetired, err)
		}
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
// ([statelog.SealOutstanding], the seal's own rule); and once sealed, nobody,
// since the verify and the release wait on no node. The seal's rule alone is
// wrong elsewhere: at `opened` it names every node though the next step is the
// operator's, and at `sealed` or `confirmed` it would name a node restarted
// after the seal. FORMED ONLY FROM ACKNOWLEDGEMENTS THAT WERE READ: an
// unreadable register answered as everyone or as "nobody" misdirects an
// operator, so the report says it couldn't read them ([windowAcks]).
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
	return statelog.AdmissionBlocks(admissions, op.Waived()), nil
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
	// WHAT IS UP NOW, so an alarm that clears past its raise is judged against
	// the wider bound while it stands ([statelog.Reading.Up]).
	if r.alarms != nil {
		out.Up, _ = r.alarms.Standing()
	}
	// THE FLOORS ARE READ AT MOST ONCE, and only where a health was not
	// handed in: one read of every domain's key answers every domain's floor.
	var floors *publishedFloors
	for _, running := range r.state.running() {
		name := running.domain.Name()
		read, ok := healths[name]
		if !ok {
			if floors == nil {
				read := r.state.readFloors(ctx)
				floors = &read
			}
			read.health, read.err = r.state.health(ctx, running, *floors)
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
	// THE INBOUND GROUP'S UNHANDLED DELIVERIES, beside the feeds' dead
	// letters the domains' rows read (feeddeadletters.go): node-wide, since
	// the group is the fleet's one.
	out.InboundDeadLetters = r.feedLetters.readInbound(ctx, now)
	if r.index != nil {
		r.index(ctx, &out)
	}
	if r.objects != nil {
		r.objects(now, &out)
	}
	if r.code != nil {
		r.code(ctx, now, &out)
	}
	if r.entries != nil {
		r.entries(ctx, now, &out)
	}
	if r.feeds != nil {
		r.feeds(now, &out)
	}
	if r.executors != nil {
		r.executors(ctx, now, &out)
	}
	r.refsReading(&out)
	// WHAT THIS NODE MEASURES OF ITSELF ALONE, read by the one implementation a
	// node without a state log reads too ([nodeMeasures]), over one window of
	// the recorder that [retention.observed] reads as well.
	measures := r.measures()
	window := measures.window()
	measures.read(&out, now, window)
	oldestWindow(now, windows, &out)
	r.observed(&out, window)
	// HOW FAR THIS NODE'S CLOCK, AND THE BROKER MEMBER'S, STAND FROM THE
	// FLEET'S, as the position heartbeat last measured them — read back,
	// since the heartbeat is the loop that holds the register's instants and
	// decides how long a measure stands (clockskew.go).
	r.state.skewReading(&out)
	return out
}

// measures is this node's own measures as [nodeMeasures] reads them.
func (r *retention) measures() nodeMeasures {
	return nodeMeasures{db: r.db, metrics: r.metrics, objectives: r.objectives, volumes: r.volumes}
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

// observed fills the fields that come from this process's own recorder, read
// from window ([nodeMeasures.window]), but the pool's waits, which
// [nodeMeasures.read] fills.
//
// GUARDED ON THE RECORDER because a nil one is a legal deployment: an embedded
// engine and every test run without one, and every condition here reads a zero
// as "nothing to report". A nil window is a node with no recorder.
func (r *retention) observed(out *statelog.Reading, window []metrics.Snapshot) {
	if window == nil {
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

	// THE ANSWER COUNTERS FIRST, because two alarms are FRACTIONS of them
	// and a fraction needs its denominator before either numerator means
	// anything.
	var answers, scoped, degraded uint64
	for _, snapshot := range window {
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
	for _, snapshot := range window {
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

	for _, snapshot := range window {
		switch snapshot.Name {
		case metrics.StatelogBarrierDuration:
			out.BarrierP95 = max(out.BarrierP95, p95Duration(snapshot))
		case metrics.TrackerSearchScanDuration:
			// BY THE PATH THE SCAN RAN FOR, each against its own budget:
			// a turn start's prefetch and a deliberate search are two
			// promises, and read as one series the faster one hides the
			// other — while a prefetch series read as interactive would
			// fire `search_slow` for a slow turn start. The worst `rung`
			// of each, as the worst file is for the pool.
			switch scanPath(snapshot.Attrs["path"]) {
			case scanPrefetch:
				out.PrefetchP95 = max(out.PrefetchP95, p95Duration(snapshot))
			case scanInteractive:
				out.SearchP95 = max(out.SearchP95, p95Duration(snapshot))
			}
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
// the window, and the census it was sized for — its seats' and the engine's
// own terms, and the term of the readers its own sizing declares beside them
// ([clientBarriers]), nil for a log that declares none.
type logCensus struct {
	reads   int
	census  statelog.Census
	clients clientTerm
}

// expectedAt is the census the log was sized for at the byte ceiling its
// stream holds (read: whether the report's stream read found it), and false
// for a log whose census counts readers of its own on a pass that could not
// read that ceiling ([retention.census]). The ceiling is the one the broker
// enforces, never the Tier A figure this node would CREATE the stream with
// ([stateLog.specOf]), which an existing stream ignores: a raised
// `stream.code_log_max_bytes` would grow the census and silence the alarm, a
// `crewlet retention set-capacity` resize would leave it stale, and unset it is
// each node's own disk-derived figure, so two nodes would judge one log apart.
func (c logCensus) expectedAt(holds uint64, read bool, minAge time.Duration) (int, bool) {
	census := c.census
	if c.clients != nil {
		if !read {
			return 0, false
		}
		census.Clients = c.clients(int64(holds), minAge)
	}
	return census.Expected(), true
}

// census is each log's census half, keyed by domain: the barrier records
// received over the window and the census it was sized for, evaluated PER LOG
// ([statelog.DomainInputs.LinearizableReads]). Nil without a recorder.
// The rate is the LOG'S, read where applied: every data node applies every
// record and one without `data` applies none, so this applier's count is the
// fleet's; summing the barriers THIS NODE appended is only its share. The census
// is declared, not configured (a settable one would silence the alarm instead of
// resizing): per domain ([registration.Reads]) scaled by seats, plus the engine's
// periodic reads and the log's declared readers ([statelog.Census],
// [clientBarriers]). WINDOWED like every [retention.observed] input: a count
// since start only grows, and a threshold against it latches.
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
			census: statelog.Census{
				Reads:      reads,
				Seats:      seats,
				Background: background(name),
			},
			clients: clientBarriers(name),
		}
	}
	return out
}

// p95Duration reads a histogram's 95th percentile back as the duration its
// unit says it is.
//
// THE PERCENTILE IS FIXED HERE rather than an argument, because every budget
// the reading is held to — the barrier's, the pool wait's and each scan path's
// — is stated at p95, and a percentile each caller spelled for itself is one a
// caller could spell differently from the budget it is compared with.
func p95Duration(s metrics.Snapshot) time.Duration {
	return time.Duration(s.Quantile(0.95) * float64(time.Millisecond))
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
