package engine

import (
	"context"
	"os"
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
		if leases, err := r.leases.ListLive(ctx, coord.NodePrefix); err == nil {
			for _, lease := range leases {
				if id, ok := coord.NodeID(lease.Resource); ok {
					in.Live = append(in.Live, statelog.Presence{NodeID: id})
				}
			}
		}
	}
	floors := map[string]coord.TrimFloor{}
	if rows, err := r.fleet.Floors(ctx); err == nil {
		for _, row := range rows {
			floors[row.Domain] = row
		}
	}
	backups, _ := r.fleet.BackupPoints(ctx)
	newest, haveBackup := coord.NewestBackup(backups)

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
		}
		if stats, err := running.log.Stats(ctx); err == nil {
			d.FirstSeq, d.LastSeq = stats.FirstSeq, stats.LastSeq
			d.Bytes, d.MaxBytes = stats.Bytes, stats.MaxBytes
			d.StreamReadable = true
		}
		if floor, published := floors[name]; published {
			d.TrimFloor = floor.TrimTo
			d.BlockedSince = floor.BlockedSince
			d.Decision = decisionOf(floor)
		}
		// THE EVICTIONS ARE NOT GATED ON A PUBLISHED FLOOR. They are
		// applied rows, present whether or not the trim has ever run,
		// and the node block renders an evicted node from them — so
		// gating them on the duty having ticked would make a fleet's
		// evictions invisible for the first fifteen minutes of its life
		// and for the whole life of a fleet whose duty is not running,
		// which is exactly when somebody is reading this.
		in.Tombstones = append(in.Tombstones,
			r.tombstones(ctx, running, d.Generation)...)
		d.SnapshotSkip = r.skipFor(in.Register, name)
		in.Domains = append(in.Domains, d)
	}

	in.Reading = r.reading(ctx, now, newest, haveBackup)
	return statelog.NewReport(in)
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
	newest coord.BackupPoint, haveBackup bool) statelog.Reading {

	out := statelog.Reading{BackupMaxAge: r.cfg.BackupMaxAge()}
	if haveBackup {
		out.BackupAge = now.Sub(newest.At)
	} else {
		// NO BACKUP AT ALL IS THE OLDEST BACKUP THERE IS, not the
		// youngest. A zero age would read as a copy taken this second,
		// which silences the one alarm a company that never backs up
		// most needs — and "a company that never backs up never trims"
		// is the term it is about to hit.
		out.BackupAge = out.BackupMaxAge + time.Hour
	}
	for _, name := range r.state.order {
		running := r.state.domains[name]
		if running == nil {
			continue
		}
		health, err := r.state.health(ctx, running)
		if err != nil {
			continue
		}
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
	r.observed(&out)
	return out
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
	for _, snapshot := range r.metrics.Read() {
		switch snapshot.Name {
		case metrics.StatelogBarrierDuration:
			out.BarrierP95 = max(out.BarrierP95, quantileDuration(snapshot, 0.95))
		case metrics.TrackerSearchScanDuration:
			out.SearchP95 = max(out.SearchP95, quantileDuration(snapshot, 0.95))
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
