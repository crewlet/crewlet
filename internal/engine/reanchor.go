package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
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
// one: an arbitration anchor at a lower generation is "no anchor at this
// generation", so its subject's next write asks the broker what the new stream
// holds rather than arbitrating against a dead number space, and a client's
// cursor at a lower generation is refused by name.
//
// It does NOT recover records that were on the old stream and never applied
// here, and the refusal says so.
//
// ONE DOMAIN AT A TIME, because a recreation is a fact about one stream: the
// other domains' checkpoints name streams nobody touched.

// ErrNotADomainLog reports a stream this build runs no domain on.
var ErrNotADomainLog = errors.New("engine: not a domain log this build runs")

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

	// By names the operator, for the generation record and the audit row.
	By string
}

// Reanchor runs the generation transition on one domain's log.
//
// # Why it refuses while any peer is hydrated on the live stream
//
// A peer that is caught up on the stream this node cannot read has the history
// this node is about to declare unreachable. Adopting its snapshot recovers
// that history; re-anchoring discards it. The refusal names the peer.
//
// # The domain's applier stops first and starts again last
//
// A running loop commits its own checkpoint after every batch, over the one the
// transition writes; and only a loop started again reads the moved checkpoint.
// Between the two the domain FOLLOWS the stream it re-anchored onto — its
// consumer reset to the moved checkpoint, and the live instant the one the
// relaunched loop and the heartbeat compare against — which is what lets it
// apply the new stream without a restart. The other domains' loops run
// throughout: nothing of theirs moves.
//
// # One estate transition at a time
//
// A runtime adoption also rewrites this node's replicated estate with its
// appliers halted, and relaunches every one of them when it finishes — so a
// reanchor is refused while one runs, and the heartbeat passes an adoption by
// while a reanchor does ([stateLog.beginReanchor]). So is a second reanchor:
// it would halt and relaunch the loop the first is holding down.
func (e *Engine) Reanchor(ctx context.Context, req ReanchorRequest) (uint32, error) {
	if !e.RunsStateLog() {
		return 0, errors.New("engine: this node runs no state log, so it has " +
			"nothing to re-anchor")
	}
	if req.By == "" {
		return 0, errors.New("engine: a reanchor names nobody who ran it — its " +
			"generation record and its audit row both say who did")
	}
	s := e.native.log
	running := s.domains[s.domainOf(req.Stream)]
	if running == nil {
		return 0, fmt.Errorf("%w: %q — the streams a reanchor applies to are %v",
			ErrNotADomainLog, req.Stream, maintenanceStreams())
	}
	deps, err := e.reanchorDeps(running, req.By)
	if err != nil {
		return 0, err
	}
	if err = s.beginReanchor(); err != nil {
		return 0, err
	}
	defer s.endReanchor()

	s.haltApplier(running)
	// THE STATE LOG'S OWN LIFETIME, never this request's: the relaunched
	// loop outlives the call that re-anchored it, and one started under the
	// caller's context would stop the moment the request returned.
	//nolint:contextcheck // s.run is [context.WithoutCancel] of the boot
	// context — see [stateLog.launchAppliers].
	defer s.launchApplier(s.run, running)

	in := e.reanchorInputs(ctx, running)
	gen, err := statelog.Reanchor(ctx, deps, in, statelog.ReanchorGuard{
		Confirm: req.Confirm, Force: req.Force,
	})
	if err != nil {
		if in.PeersHydrated > 0 {
			rows, _ := e.backends.Fleet.Positions(ctx)
			return 0, fmt.Errorf("%w (hydrated peers: %v)", err,
				hydratedPeers(rows, running.domain.Name(), in.Generation, e.id))
		}
		return 0, err
	}

	// THE CONSUMER STARTS WHERE THE CHECKPOINT NOW IS, because the broker
	// will not move a consumer's start and one left at the old position
	// waits for a sequence the new stream has not reached.
	cursor := uint64(0)
	if in.FirstSeq > 0 {
		cursor = in.FirstSeq - 1
	}
	if err := running.consumer.Reset(ctx, cursor); err != nil {
		// CORRECTNESS IS THE CHECKPOINT'S and the applier resumes from it
		// regardless: a consumer whose reset failed is rebuilt by the next
		// fetch — see [Engine.rejoin], which meets the same failure.
		log.WarnContext(ctx, "statelog_consumer_not_reset",
			"domain", running.domain.Name(), "checkpoint", cursor,
			"error", err.Error(),
			"detail", "the consumer is rebuilt by the next fetch, so this costs "+
				"redeliveries rather than correctness")
	}
	running.follow(in.StreamCreatedAt)
	log.WarnContext(ctx, "statelog_reanchored", "stream", req.Stream,
		"domain", running.domain.Name(), "generation", gen, "by", req.By,
		"prev_last_seq_seen", in.Highest,
		"detail", "every position below this generation is now comparable and "+
			"safely stale; records that were on the old stream and were never "+
			"applied here are not recovered")
	return gen, nil
}

// reanchorDeps is the transition's steps for one domain, as that domain takes
// them.
//
// A SWITCH, for [stateLog.publisherFor]'s reason: each step writes the domain's
// own rows or its own record, and a domain added to the register without a case
// here is refused by name rather than re-anchored with another domain's steps.
func (e *Engine) reanchorDeps(running *runningDomain, by string) (
	statelog.ReanchorDeps, error) {

	s := e.native.log
	deps := statelog.ReanchorDeps{Domain: running.domain, DB: e.backends.Store,
		Now: time.Now}
	switch running.domain.Name() {
	case tracker.Domain{}.Name():
		w, err := tracker.NewWriter(tracker.WriterDeps{
			Publisher: running.publisher, NodeID: s.nodeID,
			Actor: by, ActorKind: tracker.AuthorOperator,
		})
		if err != nil {
			return deps, err
		}
		w = w.As(by, tracker.AuthorOperator, tracker.Provenance{OperatorID: by})
		deps.ResetVersions = func(ctx context.Context, gen uint32) error {
			return tracker.ResetVersions(ctx, e.backends.Store.Replicated(), gen)
		}
		deps.PublishGeneration = w.PublishGeneration
		deps.RecordGeneration = func(ctx context.Context, tx *sql.Tx, gen uint32,
			in statelog.ReanchorInputs) error {
			return tracker.RecordGeneration(ctx, tx, gen, in, by, s.nodeID)
		}
	case pages.Domain{}.Name():
		// THE RECORD AND NOTHING ELSE. The audit row is the applier's own,
		// written from the published record on every node that applies
		// the new stream; and a row left at its old version is written
		// correctly, for the reason [statelog.ReanchorDeps.ResetVersions]
		// gives.
		kb, err := pages.NewStore(pages.Options{
			Publisher: running.publisher, DB: e.backends.Store,
		})
		if err != nil {
			return deps, err
		}
		actor := pages.Actor{Handle: by, Kind: pages.AuthorOperator, OperatorID: by}
		deps.PublishGeneration = func(ctx context.Context, gen uint32,
			in statelog.ReanchorInputs) error {
			return kb.PublishGeneration(ctx, actor, s.nodeID, gen, in)
		}
	case search.Domain{}.Name():
		// THE CHECKPOINT ALONE. The vectors claim no identity, so there is
		// no record for a second node to meet; no kind is arbitrated, so
		// there is no anchor a write depends on; and the apply's guard is
		// the record's own composed position, which a new generation
		// raises above every row.
	default:
		return deps, fmt.Errorf("engine: %s's log has no reanchor steps in this "+
			"build", running.domain.Name())
	}
	return deps, nil
}

// reanchorInputs reads the facts the permission check is decided from.
//
// EVERY ONE OF THEM IS READ HERE rather than inside the arithmetic, which is
// what makes every refusal reachable in a table test: a permission that could
// only be exercised against a recreated stream is one nobody re-checks.
//
// IT RETURNS NO ERROR, and that is the point of the shape rather than an
// omission: every read that can fail here fails INTO a fact the permission
// check already weighs — an unreadable stream leaves the live instant zero,
// which [statelog.PermitReanchor] refuses, and an unreadable register leaves
// RegisterReadable false, which it refuses unless the operator forces it. A
// read error returned instead would abort the call before the refusal that
// names what is actually wrong.
func (e *Engine) reanchorInputs(ctx context.Context,
	running *runningDomain) statelog.ReanchorInputs {

	in := statelog.ReanchorInputs{
		Generation: running.runner.Committed().Generation,
		Position:   running.runner.Committed().Seq,
	}
	// THE LIVE STREAM, from the broker in one answer: the instant the
	// confirmation is checked against and the moved checkpoint is committed
	// under, and the first sequence it still holds. The instant this node's
	// applier booted against is NOT it — a stream rebuilt under a running
	// node leaves that naming the stream that was deleted.
	if stats, err := running.log.Stats(ctx); err == nil {
		in.StreamCreatedAt = stats.CreatedAt.UTC()
		in.FirstSeq = stats.FirstSeq
	}
	// AND THE STREAM THIS NODE'S ROWS WERE APPLIED FROM, which the audit row
	// records: the checkpoint names it, and nothing else will once the
	// transition has moved the checkpoint.
	if _, prev, found, err := statelog.CursorFor(ctx, e.backends.Store.Replicated(),
		running.domain.Stream().Name); err == nil && found {
		in.PrevStreamCreatedAt = prev
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

// ReanchorStatus is what an operator reads before running it: the LIVE stream's
// own creation instant, which is the value the confirmation has to echo, and
// the generation this node's rows stand at.
//
// FROM THE BROKER, for [Engine.reanchorInputs]' reason: the instant the applier
// booted against names a deleted stream once one is rebuilt under a running
// node, and a confirmation echoed from it is refused by the transition it was
// read for. A broker that does not answer is an error rather than a zero
// instant nobody could confirm.
func (e *Engine) ReanchorStatus(ctx context.Context, stream string) (
	time.Time, uint32, error) {

	running, err := e.domainLog(stream)
	if err != nil {
		return time.Time{}, 0, err
	}
	stats, err := running.log.Stats(ctx)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("engine: read %s's creation instant "+
			"from the broker: %w", stream, err)
	}
	return stats.CreatedAt.UTC(), running.runner.Committed().Generation, nil
}

// StreamGeneration is the generation the named stream's rows stand at on this
// node — its applier's committed cursor — or [ErrNotADomainLog].
func (e *Engine) StreamGeneration(stream string) (uint32, error) {
	running, err := e.domainLog(stream)
	if err != nil {
		return 0, err
	}
	return running.runner.Committed().Generation, nil
}

// domainLog is the running domain on a stream, or [ErrNotADomainLog].
func (e *Engine) domainLog(stream string) (*runningDomain, error) {
	if !e.RunsStateLog() {
		return nil, fmt.Errorf("%w: this node runs no state log", ErrNotADomainLog)
	}
	running := e.native.log.domains[e.native.log.domainOf(stream)]
	if running == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotADomainLog, stream)
	}
	return running, nil
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
