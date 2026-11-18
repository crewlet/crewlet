package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// REANCHORING: the one recovery for a recreated stream, whose sequences restart
// at 1, so every position held on it names a vanished number space. The rows
// are the record of truth and the stream a replay window: the rows survive, and
// a new GENERATION makes an old position comparably stale (a lower anchor forms
// `expect = 0`, a lower client cursor is refused by name). ONE DOMAIN, the one
// named, and NO RESTART: only its loop halts and restarts, and every other
// domain stays put ([statelog.Reanchor]). Old-stream records never applied here
// are not recovered. The case ([statelog.ReanchorCase]) sets where the log is
// followed FROM: RECREATED (another instant) from its first record; RESTORED (an
// older copy, same instant) from its END, as replaying the prefix rolls every
// object back; ABANDONED (a generation only an evicted peer held) from the rows'
// own checkpoint, that generation's records void.

// ReanchorRequest is what an operator asked for.
type ReanchorRequest struct {
	// Stream is which log was recreated, and Confirm its creation instant as
	// the operator read it: proof they looked at what they re-anchor.
	Stream  string
	Confirm string

	// Force overrides the most-caught-up rule, for a fleet whose register
	// cannot say which node that is. It never overrides a peer that already
	// re-anchored the stream: its rows are the fleet's history in that
	// generation, and a second reanchor diverges silently
	// ([statelog.PermitReanchor]).
	Force bool

	// Discard accepts that a restored log's records this node's rows do not
	// hold — written after the restore — are applied on no node
	// ([statelog.ReanchorGuard.Discard]).
	Discard bool

	// By is who ran it: the author, the kind of party and the credential. The
	// generation record is published as them, all three
	// ([statelog.ReanchorDeps.By]), so every node's audit row names the
	// operator, not this node, and the credential they really acted through.
	By iam.Actor
}

// Reanchor runs one domain's generation transition, through the domain's own
// record ([registration.Generation]).
//
// It refuses, naming the peer, while a live peer has already re-anchored: its
// rows are the fleet's history in that generation, and a second opening of the
// number over another prefix is irreconcilable, so this node adopts its
// snapshot. An EVICTED peer's generation is abandoned and the transition opens
// the next ([statelog.ReanchorAbandoned]). The apply loop, the checkpoint's
// only other writer, is halted first, or a batch it commits afterwards writes
// the old generation back; it restarts on success (re-keyed), else only if it
// was running, so a mistyped confirmation leaves a healthy domain its applier.
func (e *Engine) Reanchor(ctx context.Context, req ReanchorRequest) (statelog.ReanchorPlan, error) {
	if req.By.Name == "" {
		return statelog.ReanchorPlan{}, refused("a reanchor names who ran it: " +
			"it's the one gesture that declares a log's history unreachable, and " +
			"every node's audit row records who did")
	}
	running, err := e.runningStream(req.Stream)
	if err != nil {
		return statelog.ReanchorPlan{}, err
	}
	c := e.core.Load()
	s := c.log
	if err = s.appends("a reanchor"); err != nil {
		return statelog.ReanchorPlan{}, err
	}
	record, err := generationEncoder(running.domain)
	if err != nil {
		return statelog.ReanchorPlan{}, err
	}
	name := running.domain.Name()
	stream := reanchorStreamOf(running)

	// ONE RECOVERY AT A TIME, with a runtime adoption: that replaces the
	// replicated file whole, and this writes a checkpoint in it.
	s.recovering.Lock()
	defer s.recovering.Unlock()

	wasRunning := s.haltApplier(name)
	completed := false
	defer func() {
		switch {
		case completed:
			// The transition already moved the consumer to the
			// checkpoint it committed, before committing it.
			s.launchApplier(name)
		case wasRunning:
			// A TEARDOWN, so it outlives a caller that has gone: the
			// loop it restores is the one this call ended.
			s.resumeApplier(context.WithoutCancel(ctx), name)
		}
	}()

	in, err := e.reanchorInputs(ctx, running)
	if err != nil {
		return statelog.ReanchorPlan{}, err
	}
	plan, err := statelog.Reanchor(ctx, statelog.ReanchorDeps{
		Domain:   running.domain,
		Spec:     running.spec,
		Stream:   stream,
		Record:   record,
		Consumer: running.consumer,
		Runner:   running.runner,
		Evicted:  running.evicted,
		DB:       s.estate(),
		// The operator in the three columns every write carries, in the
		// actor vocabulary every domain's generation record reads.
		By: req.By.Name, ByKind: req.By.Kind, OperatorID: req.By.OperatorID,
		NodeID: c.nodeID,
		// This broker's bring-up budget for the steps after the append: the
		// consumer's rebuild is a delete and a create, a raft round trip
		// each when clustered ([statelog.ReanchorCompletionBudget]).
		CompletionBudget: s.clustered.SequenceBudget(),
		Now:              time.Now,
	}, in, statelog.ReanchorGuard{Confirm: req.Confirm, Force: req.Force, Discard: req.Discard})
	if err != nil {
		return statelog.ReanchorPlan{}, err
	}
	completed = true
	// THE FLEET IS TOLD AT ONCE: every peer left in the old generation adopts
	// a snapshot from the new one ([passedByAReanchor]), learning of it from
	// this node's position row. Waiting for the heartbeat leaves them serving
	// the old generation up to ten seconds; waiting for the snapshot interval
	// leaves them nothing to adopt for up to a day.
	s.publishPositions(ctx)
	s.nudgeSnapshot()
	// A reanchor of the usage log owes every day again: a restored copy is
	// followed from its end, so what data nodes published after the restore is
	// in a tail never applied. Other nodes republish on the row above; this
	// node's publisher is woken by the relaunched applier's first drain
	// ([stateLog.onDrained], internal/usage's republish.go).
	// No log line here: [statelog.Reanchor] writes `statelog_reanchored`, and a
	// second one would make one transition read as two.
	return plan, nil
}

// generationEncoder is the domain's own record of a reanchor, or its
// declaration that it keeps none.
//
// Read off the register, like [barrierEncoder]: a record's shape is the
// domain's, and [checkRegister] refuses at boot an entry that states none, so a
// domain that could never be re-anchored is refused by name, not found
// mid-incident. The error arm is for a domain the register does not hold.
func generationEncoder(domain statelog.Domain) (statelog.GenerationEncoder, error) {
	entry, found := registrationFor(domain.Name())
	if !found || entry.Generation == nil {
		return nil, fmt.Errorf("engine: domain %q is registered and has no "+
			"generation record, so a recreated log of it could never be re-anchored",
			domain.Name())
	}
	return entry.Generation, nil
}

// reanchorStream is a domain log as [statelog.Reanchor] needs it: an append
// that SIGNS, reads that OPEN what they read, the per-subject probe the log
// already has, and its creation instant read LIVE from the broker.
//
// Both halves of the frame (ADR-0031), because the transition does both: the
// generation record is the one record no write authority publishes, so an
// unsigned append would open a generation every applier drops as tampered; and
// it reads back what it appended, and a restored tail, through the embedded
// [statelog.VerifiedLog], since the raw log answers the signature and every
// decode would fail.
type reanchorStream struct {
	statelog.VerifiedLog
	log    *jetstream.DomainLog
	signer *recordseal.Signer
}

// reanchorStreamOf is running's log as a reanchor writes and reads it, under
// this domain's own signer and verifier.
func reanchorStreamOf(running *runningLog) reanchorStream {
	return reanchorStream{
		VerifiedLog: statelog.VerifiedLog{Log: running.log, Verifier: running.verifier},
		log:         running.log, signer: running.signer,
	}
}

func (r reanchorStream) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {
	return r.log.Append(ctx, subject, msgID, expect, r.signer.Seal(body))
}

func (r reanchorStream) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return r.log.LastSeq(ctx, subject)
}

func (r reanchorStream) CreatedAt(ctx context.Context) (time.Time, error) {
	stats, err := r.log.Stats(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return stats.CreatedAt.UTC(), nil
}

// reanchorInputs reads the facts the permission check is decided from, here
// rather than in the arithmetic, so every refusal is reachable in a table test.
//
// The stream is read LIVE, once: its instant and both ends decide the case and
// key and position the checkpoint, which a boot-time sample would key to a
// stream rebuilt since. Unreadable, it is an error, never a zero (a zero first
// sequence claims a stream nobody looked at). Everything else fails into a fact
// the check weighs and names (an unreadable register leaves RegisterReadable
// false, refused unless forced). EVICTED peers count for neither fleet rule
// ([stateLog.evictedOn]); what they opened is [statelog.ReanchorInputs.Abandoned],
// read off the log too, since a node that died before its position row opened
// a generation no row names.
func (e *Engine) reanchorInputs(ctx context.Context,
	running *runningLog) (statelog.ReanchorInputs, error) {

	stream := running.spec.Name
	stats, err := running.log.Stats(ctx)
	if err != nil {
		return statelog.ReanchorInputs{}, reanchorUnread(err, "%s couldn't be read, "+
			"so neither its creation instant nor its first sequence (what the "+
			"checkpoint would be keyed to and positioned at) is known. Check the "+
			"broker and re-run", stream)
	}
	// THE CHECKPOINT ROW, not the runner's memory of it: the generation the
	// transition derives from and the instant the rows are keyed to are the
	// durable ones, and the loop that would move them is halted.
	checkpoint, _, err := statelog.CheckpointOf(ctx, e.domainEstate(), stream)
	if err != nil {
		return statelog.ReanchorInputs{}, err
	}
	in := streamFacts(stream, stats, checkpoint.At, checkpoint.KeyedTo)
	// AND WHETHER THE LOG STILL HOLDS THE RECORD THE ROW NAMES, read now: the
	// case is decided from the log as the operator asks, and a restored log
	// written past this node's rows is the restored case before any
	// verification runs. Unreadable refuses, as the stream does. A rebuilt
	// stream differs at every sequence too, but [statelog.ReanchorInputs.Case]
	// decides that case by the instant first.
	_, in.Diverged, err = statelog.CheckpointDiverged(ctx, running.log, checkpoint.At,
		checkpoint.StoredAt)
	if err != nil {
		return statelog.ReanchorInputs{}, reanchorUnread(err, "whether %s still "+
			"holds the record at this node's checkpoint couldn't be read, and it "+
			"decides whether this is the restored case. Check the broker and "+
			"re-run", stream)
	}
	// OR THE RUNNER'S VERDICT (found earlier, or recalled from the node estate
	// after a restart): the one statement surviving the log losing the
	// checkpoint's record, without which a node every read and write refuses
	// would have nothing to re-anchor.
	in.Diverged = in.Diverged || running.runner.Diverged()
	in.ClaimsIdentity = running.domain.ClaimsIdentity()
	if !in.ClaimsIdentity {
		// NO FLEET GUARD APPLIES, and no generation is anybody's but this
		// node's own: every node re-anchors its own copy of such a log.
		return in, nil
	}
	domain := running.domain.Name()
	c := e.core.Load()
	self := c.nodeID

	// UNREADABLE IS NOT "no peers": that outage is when re-anchoring is most
	// tempting and least justified, so RegisterReadable stays false and the
	// permission refuses on it; the read's error goes no further.
	rows, readErr := c.log.fleet.Positions(ctx)
	// A FLOOR IS ONLY EVER A SOURCE OF AN ABANDONED GENERATION here, and one
	// that cannot be read leaves that number to the log's own records.
	floor, published, floorErr := readFloorsOf(ctx, c.log.fleet, []string{domain}).row(domain)
	through := in.Generation
	var candidates []string
	if readErr == nil {
		for _, row := range rows {
			if at, runs := row.Domains[domain]; runs && row.NodeID != self &&
				at.Generation >= in.Generation {
				candidates = append(candidates, row.NodeID)
				through = max(through, at.Generation)
			}
		}
	}
	if floorErr == nil && published && floor.Generation > in.Generation &&
		floor.By != "" && floor.By != self {
		candidates = append(candidates, floor.By)
		through = max(through, floor.Generation)
	}
	enc, err := generationEncoder(running.domain)
	if err != nil {
		return statelog.ReanchorInputs{}, err
	}
	openers, err := statelog.GenerationOpeners(ctx, running.domain, running.spec, enc,
		running.standing(), in.Generation, through)
	if err != nil {
		return statelog.ReanchorInputs{}, reanchorUnread(err, "which generations of %s "+
			"are already open couldn't be read off its log, so the one this "+
			"reanchor would open is unknown. Check the broker and re-run", stream)
	}
	for _, writer := range openers {
		if writer != "" && writer != self {
			candidates = append(candidates, writer)
		}
	}
	evicted, err := c.log.evictedOn(ctx, running.domain, running.spec, running.standing(), candidates)
	if err != nil {
		return statelog.ReanchorInputs{}, reanchorUnread(err, "whether the peers ahead "+
			"of this node on %s are evicted couldn't be read, and an evicted "+
			"peer's generation is abandoned where a live one's is the fleet's. "+
			"Check the broker and re-run", stream)
	}

	if readErr == nil {
		in.RegisterReadable = true
		for _, row := range rows {
			if row.NodeID == self || evicted[row.NodeID] {
				continue
			}
			// THE SAME GENERATION AND THE SAME STREAM ONLY: a sequence at
			// another generation, or on another stream at this one (a node
			// up with no rows after the rebuild counts the new stream from
			// 1), is in another number space and says nothing about who went
			// further along the stream this node's rows came from.
			at, runs := row.Domains[domain]
			if !runs || at.Generation != in.Generation ||
				!sameStream(at.StreamCreatedAt, in.KeyedTo) || at.Seq <= in.Highest {
				continue
			}
			// AND ONLY A PEER HOLDING HISTORY THE LOG DOES NOT: one whose
			// checkpoint record the log holds loses nothing to a reanchor
			// following the log, whatever its sequence (what it wrote after
			// a restore is the discard question). Counted, it would refuse
			// every reanchor of a node the log DIVERGED from, leaving only
			// the force flag, which also waives this rule. A checkpoint past
			// the log's end, another record at its sequence, or none named is
			// history the log may have lost, and counts.
			onLog, holdErr := statelog.HoldsRecord(ctx, running.log, at.Seq, at.CheckpointStoredAt)
			if holdErr != nil {
				return statelog.ReanchorInputs{}, reanchorUnread(holdErr, "whether the log "+
					"holds peer %s's checkpoint record at %d of %s couldn't be read, "+
					"and it decides whether that peer holds history a reanchor "+
					"would lose. Check the broker and re-run", row.NodeID, at.Seq, stream)
			}
			if !onLog {
				in.Highest, in.HighestPeer = at.Seq, row.NodeID
			}
		}
		in.ReanchoredPeers = reanchoredPeers(rows, domain, in.Generation, self, evicted)
		for _, row := range rows {
			if at, runs := row.Domains[domain]; runs && evicted[row.NodeID] &&
				at.Generation > in.Generation {
				in.Abandoned = max(in.Abandoned, at.Generation)
			}
		}
	}
	if floorErr == nil && published && evicted[floor.By] && floor.Generation > in.Generation {
		in.Abandoned = max(in.Abandoned, floor.Generation)
	}
	for gen, writer := range openers {
		if evicted[writer] {
			in.Abandoned = max(in.Abandoned, gen)
		}
	}
	if err := e.restoredTail(ctx, running, c, enc, &in); err != nil {
		return statelog.ReanchorInputs{}, err
	}
	return in, nil
}

// restoredTail fills in what a RESTORED reanchor of an identity-claiming domain
// would discard: this node's own record opening the next generation, when an
// earlier attempt already appended it ([statelog.ReanchorInputs.Opened]), and
// the newest record below that — or below the log's end — that writes rows
// these do not hold ([statelog.ReanchorInputs.Unheld]).
//
// Run LAST, after the fleet: the next generation follows every one this domain
// used, an evicted node's included, and only then is the subject an earlier
// attempt would have written known. An unreadable log refuses: whether the
// reanchor loses writes is the question.
func (e *Engine) restoredTail(ctx context.Context, running *runningLog, c *core,
	enc statelog.GenerationEncoder, in *statelog.ReanchorInputs) error {

	// ONLY THE RESTORED CASE has a tail to fill — and a case that cannot be
	// decided names none, since the transition refuses it in its own words.
	if which, _, _ := in.Case(); which != statelog.ReanchorRestored {
		return nil
	}
	stream := running.spec.Name
	next := max(in.Generation, in.Abandoned) + 1
	opened, own, err := statelog.OwnGeneration(ctx, running.domain, running.spec, enc,
		running.standing(), next, c.nodeID)
	if err != nil {
		return reanchorUnread(err, "whether an earlier reanchor of %s on this node already "+
			"opened generation %d couldn't be read off the log. Check the broker "+
			"and re-run", stream, next)
	}
	bound := in.LastSeq
	if own {
		in.Opened, bound = opened, opened-1
	}
	in.Unheld, err = statelog.UnheldTail(ctx, running.domain, running.spec,
		c.log.estate().Reader(), running.standing(), in.Generation, in.FirstSeq, bound)
	if err != nil {
		return reanchorUnread(err, "couldn't read whether %s holds records written "+
			"after the restore that this node's rows lack, and a restored reanchor "+
			"would apply them on no node. Check the broker and re-run", stream)
	}
	return nil
}

// sameStream reports whether a peer's position can be on the stream this node's
// rows are keyed to.
//
// Rows keyed to no instant (no checkpoint row, [statelog.CheckpointOf]) count
// every peer as on their stream: the answer feeds the most-caught-up refusal,
// so a node that cannot place itself treats every peer as possibly ahead. Every
// peer row names its instant ([coord.DomainPosition.StreamCreatedAt]), so one
// that does not is on no stream and never counted.
func sameStream(peer, keyed time.Time) bool {
	return keyed.IsZero() || statelog.IdentityOf(keyed, peer, true) == statelog.StreamSame
}

// ErrUnknownStream reports a stream that is not a domain log this node runs: a
// mistyped name, or a node with no core runtime (one without `data`, or an
// engine built by hand; every data node [New] builds runs every domain's log).
//
// A sentinel, because nothing about it is transient, while a stream that could
// not be READ is the broker's to answer: a mistyped name is not told to retry,
// nor a blinking broker that the log does not exist.
var ErrUnknownStream = errors.New("engine: not a domain log this node runs")

// runningStream is the running domain whose log is stream, or an
// [UnknownStreamError] naming the streams there are.
func (e *Engine) runningStream(stream string) (*runningLog, error) {
	s, err := e.stateLogOf()
	if err != nil {
		return nil, &UnknownStreamError{Stream: stream}
	}
	running := s.logOf(stream)
	if running == nil {
		return nil, &UnknownStreamError{Stream: stream, Streams: maintenanceStreams()}
	}
	return running, nil
}

// StreamGeneration is the generation the named stream's domain stands at on
// this node: its applier's committed checkpoint, read from memory with no
// broker round trip — the same number the trim, the publisher and the
// positions heartbeat read.
func (e *Engine) StreamGeneration(stream string) (uint32, error) {
	running, err := e.runningStream(stream)
	if err != nil {
		return 0, err
	}
	return running.runner.Committed().Generation, nil
}

// streamFacts is the part of a reanchor's inputs that ONE reading of the stream
// and ONE of the checkpoint row answer.
//
// Shared by [Engine.ReanchorStatus] and the transition, so both name the case
// from the same facts by the same rule ([statelog.ReanchorInputs.Case]) and the
// operator gets the case they confirmed.
func streamFacts(stream string, stats jetstream.LogStats, at statelog.Position,
	keyed time.Time) statelog.ReanchorInputs {

	return statelog.ReanchorInputs{
		Stream:          stream,
		StreamCreatedAt: stats.CreatedAt.UTC(),
		KeyedTo:         keyed,
		FirstSeq:        stats.FirstSeq,
		LastSeq:         stats.LastSeq,
		Generation:      at.Generation,
		Position:        at.Seq,
	}
}

// ReanchorView is what an operator reads before confirming a reanchor.
type ReanchorView struct {
	// CreatedAt is the LIVE stream's creation instant: the value the
	// confirmation has to echo.
	CreatedAt time.Time

	// Generation is the generation the domain's checkpoint stands at.
	Generation uint32

	// Case is what a reanchor run now would answer — the log recreated,
	// restored from an older copy, or continuing in a generation only an
	// evicted peer held — and Cursor the sequence it would put the new
	// checkpoint at. Case is empty when there is nothing to re-anchor, and
	// Refusal then says why, in the transition's own words.
	Case    statelog.ReanchorCase
	Cursor  uint64
	Refusal string

	// Discards is, for the restored case, the newest record on the log writing
	// rows this node's do not hold (written after the restore, so applied on no
	// node once the log is followed from its end), nil when none. Discarding it
	// takes [ReanchorRequest.Discard]; Discarding says why, in the
	// transition's own words.
	Discards   *statelog.TailRecord
	Discarding string
}

// ReanchorStatus is what an operator reads before running a reanchor: the LIVE
// stream's creation instant the confirmation has to echo (the one a
// `wrong_stream` refusal names), the domain's generation, and which case a
// reanchor would answer, so they know whether the log is followed from its
// first record, its end or this node's checkpoint.
//
// It reads the transition's own inputs ([Engine.reanchorInputs]), fleet
// included, since the abandoned case is a fact about the register and the log
// together. The guards are not run: whether this node may is the transition's
// answer. An unreadable stream is an error that is NOT [ErrUnknownStream]: the
// log exists and the broker did not answer.
func (e *Engine) ReanchorStatus(ctx context.Context, stream string) (ReanchorView, error) {
	running, err := e.runningStream(stream)
	if err != nil {
		return ReanchorView{}, err
	}
	in, err := e.reanchorInputs(ctx, running)
	if err != nil {
		return ReanchorView{}, fmt.Errorf("engine: read what a reanchor of %s would "+
			"be decided from: %w", stream, err)
	}
	view := ReanchorView{CreatedAt: in.StreamCreatedAt, Generation: in.Generation}
	if view.Case, view.Cursor, err = in.Case(); err != nil {
		view.Refusal = toldOrError(err)
	}
	if discarding := in.Discarding(); discarding != nil {
		view.Discards, view.Discarding = in.Unheld, toldOrError(discarding)
	}
	return view, nil
}

// reanchoredPeers names every peer that has already re-anchored this domain's
// stream: every one at a LATER generation than this node's checkpoint. One
// definition, because the permission check counts them and the refusal names
// them.
//
// A generation moves only by a reanchor or by adopting the snapshot of a node
// that ran one, so such a peer holds the fleet's history in it, whatever it
// applied since. A peer at this node's OWN generation is not one: a generation
// cannot say which stream a sequence is on, and every peer still on the lost
// stream stands there, so the most-caught-up rule weighs it instead
// ([statelog.ReanchorInputs]). Nor is an EVICTED peer ([stateLog.evictedOn]).
func reanchoredPeers(rows []coord.NodePositions, domain string, generation uint32,
	self string, evicted map[string]bool) []string {

	var reanchored []string
	for _, row := range rows {
		if row.NodeID == self || evicted[row.NodeID] {
			continue
		}
		if at, runs := row.Domains[domain]; runs && at.Generation > generation {
			reanchored = append(reanchored, row.NodeID)
		}
	}
	return reanchored
}
