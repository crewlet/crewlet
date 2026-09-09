package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// REANCHORING: the one recovery for a stream that was genuinely recreated.
//
// A recreated stream restarts its sequences at 1, so every position the fleet
// holds names a number space that no longer exists — a stored version, a
// consumer's cursor, an arbitration anchor. The node's answer to that is to
// refuse to serve, which is correct and is not the only outcome available.
//
// What a reanchor says is: THESE ROWS ARE WHAT THEY ARE; follow the new stream
// from its head. The durable tables are the record of truth and the stream is
// a replay window, so the rows survive and the window is replaced. What the
// GENERATION adds over an earlier re-stamp is that an old position becomes
// COMPARABLE and safely stale rather than indistinguishable from a current
// one: a stored version at a lower generation forms `expect = 0` on its next
// write, and a client's cursor at a lower one is refused by name.
//
// It does NOT recover records that were on the old stream and never applied
// here, and the refusal says so.

// ReanchorRequest is what an operator asked for.
type ReanchorRequest struct {
	// Stream is which log was recreated, and Confirm the stream's own
	// creation instant as the operator read it — the value that says they
	// looked at the thing they are about to re-anchor.
	Stream  string
	Confirm string

	// Force is the escape for a fleet whose peers cannot be established.
	// It is refused unless the operator supplies it, because adopting a
	// hydrated peer's snapshot is strictly better than re-anchoring.
	Force bool

	// By names the operator, for the audit row.
	By string
}

// Reanchor runs the generation transition.
//
// # Why it refuses while any peer is hydrated on the live stream
//
// A peer that is caught up on the stream this node cannot read has the history
// this node is about to declare unreachable. Adopting its snapshot recovers
// that history; re-anchoring discards it. The refusal names the peer.
func (e *Engine) Reanchor(ctx context.Context, req ReanchorRequest) (uint32, error) {
	if e.native == nil || e.native.log == nil || e.native.writer == nil {
		return 0, errors.New("engine: this node runs no state log, so it has " +
			"nothing to re-anchor")
	}
	s := e.native.log
	name := s.domainOf(req.Stream)
	running := s.domains[name]
	if running == nil {
		return 0, fmt.Errorf("engine: %q is not a domain log this build runs — "+
			"the streams a reanchor applies to are %v", req.Stream,
			maintenanceStreams())
	}

	in, err := e.reanchorInputs(ctx, running)
	if err != nil {
		return 0, err
	}
	gen, err := statelog.Reanchor(ctx, statelog.ReanchorDeps{
		Domains: s.registered(),
		DB:      e.backends.Store.Replicated(),
		ResetVersions: func(ctx context.Context, gen uint32) error {
			return tracker.ResetVersions(ctx, e.backends.Store.Replicated(), gen)
		},
		PublishGeneration: e.native.writer.PublishGeneration,
		RecordGeneration: func(ctx context.Context, tx *sql.Tx, gen uint32,
			in statelog.ReanchorInputs) error {
			return tracker.RecordGeneration(ctx, tx, gen, in, req.By)
		},
		Now: time.Now,
	}, in, statelog.ReanchorGuard{Confirm: req.Confirm, Force: req.Force})
	if err != nil {
		if in.PeersHydrated > 0 {
			rows, _ := e.backends.Fleet.Positions(ctx)
			return 0, fmt.Errorf("%w (hydrated peers: %v)", err,
				hydratedPeers(rows, running.domain.Name(), in.Generation, e.id))
		}
		return 0, err
	}
	log.WarnContext(ctx, "statelog_reanchored", "stream", req.Stream,
		"generation", gen, "by", req.By, "prev_last_seq_seen", in.Highest,
		"detail", "every position below this generation is now comparable and "+
			"safely stale; records that were on the old stream and were never "+
			"applied here are not recovered")
	return gen, nil
}

// reanchorInputs reads the seven facts the permission check is decided from.
//
// EVERY ONE OF THEM IS READ HERE rather than inside the arithmetic, which is
// what makes every refusal reachable in a table test: a permission that could
// only be exercised against a recreated stream is one nobody re-checks.
func (e *Engine) reanchorInputs(ctx context.Context, running *runningDomain) (
	statelog.ReanchorInputs, error) {

	in := statelog.ReanchorInputs{
		StreamCreatedAt: running.createdAt,
		Generation:      running.runner.Committed().Generation,
		Position:        running.runner.Committed().Seq,
	}
	if stats, err := running.log.Stats(ctx); err == nil {
		in.FirstSeq = stats.FirstSeq
	}

	rows, err := e.backends.Fleet.Positions(ctx)
	if err != nil {
		// UNREADABLE IS NOT "no peers". A register nobody could list is
		// exactly the outage during which re-anchoring is most tempting
		// and least justified, so the flag says so and the permission
		// refuses on it.
		return in, nil
	}
	in.RegisterReadable = true
	domain := running.domain.Name()
	for _, row := range rows {
		if at, runs := row.Domains[domain]; runs && at.Seq > in.Highest {
			in.Highest = at.Seq
		}
	}
	in.PeersHydrated = len(hydratedPeers(rows, domain, in.Generation, e.id))
	return in, nil
}

// ReanchorStatus is what an operator reads before running it: the stream's own
// creation instant, which is the value the confirmation has to echo.
func (e *Engine) ReanchorStatus(ctx context.Context, stream string) (
	time.Time, uint32, error) {

	if e.native == nil || e.native.log == nil {
		return time.Time{}, 0, errors.New("engine: this node runs no state log")
	}
	running := e.native.log.domains[e.native.log.domainOf(stream)]
	if running == nil {
		return time.Time{}, 0, fmt.Errorf("engine: %q is not a domain log this "+
			"build runs", stream)
	}
	_ = ctx
	return running.createdAt, running.runner.Committed().Generation, nil
}

// hydratedPeers names every peer that is caught up on the LIVE stream.
//
// ONE DEFINITION of what "hydrated on the live stream" means, because two
// places need it and they must agree: the permission check counts them, and
// the refusal names them. A peer that stands at this generation and has
// applied anything at all holds history a reanchor would declare unreachable,
// and its snapshot is strictly the better recovery.
func hydratedPeers(rows []coord.NodePositions, domain string, generation uint32,
	self string) []string {

	var hydrated []string
	for _, row := range rows {
		if row.NodeID == self {
			continue
		}
		if at, runs := row.Domains[domain]; runs &&
			at.Generation == generation && at.AppliedThrough > 0 {
			hydrated = append(hydrated, row.NodeID)
		}
	}
	return hydrated
}
