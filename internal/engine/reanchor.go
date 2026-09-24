package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
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

	// By is who ran it: the author and the credential they acted through.
	//
	// THE GENERATION RECORD IS PUBLISHED AS THEM, so every node's audit
	// row names the operator — it was published through this node's own
	// writer, so every peer recorded the NODE as having re-anchored the
	// log while this node's own row named the person. The record is a
	// gate, pinned at its first version, so what it can say is the author
	// it already had a field for ([tracker.Generation.ReanchoredBy]) and
	// the operator id every tracker record carries.
	By iam.Actor
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
	if req.By.Name == "" {
		return 0, errors.New("engine: a reanchor names who ran it — it is the " +
			"one gesture that declares a log's history unreachable, and every " +
			"node's audit row records who did")
	}
	s := e.native.log
	name := s.domainOf(req.Stream)
	running := s.domains[name]
	if running == nil {
		return 0, fmt.Errorf("engine: %q is not a domain log this build runs — "+
			"the streams a reanchor applies to are %v", req.Stream,
			reanchorStreams())
	}
	if name != (tracker.Domain{}).Name() {
		// THE TRACKER'S STEPS ARE THE ONLY ONES THERE ARE. A reanchor is
		// three things only the log's own domain can do — reset its rows'
		// composed versions, publish its generation record, write its
		// audit row — and no other domain has them. Run for another log,
		// the tracker's reset the TRACKER's rows and published a TRACKER
		// generation for a log that was not the tracker's, while the
		// recreated log's own rows kept versions from a number space its
		// stream no longer has.
		return 0, fmt.Errorf("engine: the %s log cannot be re-anchored by "+
			"this build — its domain (%s) has no reset, generation record or "+
			"audit row of its own; recover it by adopting the snapshot of a "+
			"peer hydrated on the live stream, or by restoring from a backup. "+
			"The logs a reanchor applies to are %v", req.Stream, name,
			reanchorStreams())
	}

	in := e.reanchorInputs(ctx, running)
	operator := e.native.writer.As(req.By.Name, tracker.AuthorKind(req.By.Kind),
		tracker.Provenance{OperatorID: req.By.OperatorID})
	gen, err := statelog.Reanchor(ctx, statelog.ReanchorDeps{
		// THIS LOG'S DOMAIN AND NO OTHER: every input above is this
		// stream's own, and a second domain's cursor moved to a position
		// computed from it would point into a number space its own stream
		// never had.
		Target: s.registered()[name],
		DB:     e.backends.Store,
		ResetVersions: func(ctx context.Context, gen uint32) error {
			return tracker.ResetVersions(ctx, e.backends.Store.Replicated(), gen)
		},
		PublishGeneration: operator.PublishGeneration,
		RecordGeneration: func(ctx context.Context, tx *sql.Tx, gen uint32,
			in statelog.ReanchorInputs) error {
			return tracker.RecordGeneration(ctx, tx, gen, in, req.By.Name)
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
		"generation", gen, "by", req.By.Name, "operator", req.By.OperatorID,
		"prev_last_seq_seen", in.Highest,
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
//
// IT RETURNS NO ERROR, and that is the point of the shape rather than an
// omission: every read that can fail here fails INTO a fact the permission
// check already weighs — an unreadable stream leaves FirstSeq at zero, and an
// unreadable register leaves RegisterReadable false, which [statelog.Reanchor]
// refuses on unless the operator forces it. A read error returned instead
// would abort the call before the refusal that names what is actually wrong.
func (e *Engine) reanchorInputs(ctx context.Context,
	running *runningDomain) statelog.ReanchorInputs {

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
		return in
	}
	in.RegisterReadable = true
	domain := running.domain.Name()
	for _, row := range rows {
		if at, runs := row.Domains[domain]; runs && at.Seq > in.Highest {
			in.Highest = at.Seq
		}
	}
	in.PeersHydrated = len(hydratedPeers(rows, domain, in.Generation, e.id))
	return in
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

// reanchorStreams is every log a reanchor applies to: the tracker's, whose
// domain is the one with a reset, a generation record and an audit row of its
// own.
func reanchorStreams() []string {
	return []string{tracker.Domain{}.Stream().Name}
}
