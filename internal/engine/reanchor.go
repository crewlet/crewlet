package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// REANCHORING: the one recovery for a stream that was genuinely recreated.
//
// A recreated stream restarts its sequences at 1, so every position the node
// holds on it names a number space that no longer exists — a stored version, a
// consumer's cursor, an arbitration anchor. The node's answer to that is to
// refuse to serve that domain, which is correct and is not the only outcome
// available.
//
// What a reanchor says is: THESE ROWS ARE WHAT THEY ARE; follow the new stream
// from its head. The durable tables are the record of truth and the stream is
// a replay window, so the rows survive and the window is replaced. What the
// GENERATION adds over an earlier re-stamp is that an old position becomes
// COMPARABLE and safely stale rather than indistinguishable from a current
// one: an anchor at a lower generation forms `expect = 0` on its next write,
// and a client's cursor at a lower one is refused by name.
//
// ONE DOMAIN, the one whose stream the operator named: every fact the verb is
// decided from is a fact about that stream, and every other domain's
// checkpoint, generation and instant are left exactly where they were — see
// [statelog.Reanchor] for what moving all of them from one stream's facts did.
//
// AND NO RESTART. The domain's loop is halted before its checkpoint moves,
// the runner is re-keyed to the stream it adopted once the checkpoint has
// committed, and the loop is started again: the domain applies the new stream
// from its head, its reads and writes are served again, and nothing else on
// the node paused.
//
// It does NOT recover records that were on the old stream and never applied
// here, and the refusal says so.
//
// THREE CASES, and they differ in where the log is followed FROM
// ([statelog.ReanchorCase]). A RECREATED stream — another creation instant than
// the rows are keyed to — holds none of what the rows came from, so the domain
// follows it from its first surviving record. A RESTORED one — the broker
// brought back from an older copy, same instant, ending below this node's
// checkpoint — holds a prefix of exactly that history, which the rows already
// have, so the domain follows it from its END: replaying the prefix into a new
// generation would roll every object back to the copy. An ABANDONED one — the
// rows' own stream, continuing in a generation only a peer the fleet has since
// evicted held — is followed from the rows' own checkpoint, with that
// generation's records void.

// ReanchorRequest is what an operator asked for.
type ReanchorRequest struct {
	// Stream is which log was recreated, and Confirm the stream's own
	// creation instant as the operator read it — the value that says they
	// looked at the thing they are about to re-anchor.
	Stream  string
	Confirm string

	// Force overrides the rule that only the most caught-up node may
	// re-anchor, for a fleet whose register cannot say which that is. It
	// never overrides a peer that has already re-anchored the stream — the
	// fleet's history in that generation is that peer's rows, and the
	// divergence a second, independent reanchor produces is silent — see
	// [statelog.PermitReanchor].
	Force bool

	// By names the operator, for the generation record.
	By string
}

// Reanchor runs one domain's generation transition.
//
// # Why it refuses while any peer has already re-anchored the stream
//
// That peer opened the next generation from its own rows, and those rows are
// the fleet's history in it. A second reanchor from this node's rows would open
// the same generation number over a different prefix of what was lost, which
// nothing could ever reconcile; this node adopts the peer's snapshot instead.
// The refusal names the peer. A peer the fleet has EVICTED is not one: its
// generation is abandoned, and the transition opens the one after it
// ([statelog.ReanchorAbandoned]).
//
// # The loop is halted around it, and restarted whatever happened before
//
// The checkpoint the transition moves has exactly one other writer, the
// domain's own apply loop, and a batch it committed after the transition's
// transaction would write the old generation and the old stream's instant back
// over it. So the loop is ended first. It is started again when the transition
// completes — the runner is re-keyed by then, so the loop resumes from the new
// checkpoint — and, when the transition did not complete, exactly when it was
// running before: a mistyped confirmation must not leave a healthy domain
// without its applier, and a loop that had stopped on a recreated stream stays
// stopped rather than logging the same stop again.
func (e *Engine) Reanchor(ctx context.Context, req ReanchorRequest) (statelog.ReanchorPlan, error) {
	running, err := e.runningStream(req.Stream)
	if err != nil {
		return statelog.ReanchorPlan{}, err
	}
	s := e.native.Load().log
	record, err := generationEncoder(running.domain)
	if err != nil {
		return statelog.ReanchorPlan{}, err
	}
	name := running.domain.Name()

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

	in, peers, err := e.reanchorInputs(ctx, running)
	if err != nil {
		return statelog.ReanchorPlan{}, err
	}
	plan, err := statelog.Reanchor(ctx, statelog.ReanchorDeps{
		Domain:   running.domain,
		Stream:   reanchorStream{log: running.log},
		Record:   record,
		Consumer: running.consumer,
		Runner:   running.runner,
		Evicted:  running.evicted,
		DB:       replicatedEstate{node: s.db},
		By:       req.By,
		NodeID:   e.native.Load().nodeID,
		Now:      time.Now,
	}, in, statelog.ReanchorGuard{Confirm: req.Confirm, Force: req.Force})
	if err != nil {
		if len(peers) > 0 {
			return statelog.ReanchorPlan{}, fmt.Errorf("%w (re-anchored peers: %v)", err, peers)
		}
		return statelog.ReanchorPlan{}, err
	}
	completed = true
	// THE FLEET IS TOLD AT ONCE, and this node's artefact is replaced soon.
	// Every peer still in the generation this left adopts a snapshot from a
	// node in the new one ([passedByAReanchor]), which it learns of from this
	// node's position row and asks for at the generation that row names — so
	// a row left for the next heartbeat is up to ten seconds in which those
	// peers go on serving the old generation, and an artefact left for the
	// snapshot interval is up to a day in which they have nothing to adopt.
	s.publishPositions(ctx)
	s.nudgeSnapshot()
	// NO LINE OF ITS OWN: [statelog.Reanchor] writes `statelog_reanchored`
	// with the domain, the generation, the case, the stream and its prior
	// high-water mark, and a second line under that name here made one
	// transition read as two.
	return plan, nil
}

// generationEncoder is the domain's own record of a reanchor, or its
// declaration that it keeps none.
//
// A SWITCH, like [barrierEncoder] and [stateLog.publisherFor], because what a
// record on a domain's log looks like is the domain's, and the register is the
// one place every domain this build runs is named. A registered domain with no
// case here could never be re-anchored, which is refused by name rather than
// discovered by an operator mid-incident.
func generationEncoder(domain statelog.Domain) (statelog.GenerationEncoder, error) {
	switch domain.Name() {
	case tracker.Domain{}.Name():
		return tracker.GenerationRecord{}, nil
	case pages.Domain{}.Name():
		return pages.GenerationRecord{}, nil
	case search.Domain{}.Name():
		return search.GenerationRecord{}, nil
	}
	return nil, fmt.Errorf("engine: domain %q is registered and has no "+
		"generation record, so a recreated log of it could never be re-anchored",
		domain.Name())
}

// reanchorStream is a domain log as [statelog.Reanchor] needs it: the append
// and the per-subject probe the log already has, and its creation instant read
// LIVE from the broker.
type reanchorStream struct{ log *jetstream.DomainLog }

func (r reanchorStream) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {
	return r.log.Append(ctx, subject, msgID, expect, body)
}

func (r reanchorStream) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return r.log.LastSeq(ctx, subject)
}

func (r reanchorStream) At(ctx context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	return r.log.At(ctx, seq)
}

func (r reanchorStream) CreatedAt(ctx context.Context) (time.Time, error) {
	stats, err := r.log.Stats(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return stats.CreatedAt.UTC(), nil
}

// reanchorInputs reads the facts the permission check is decided from.
//
// EVERY ONE OF THEM IS READ HERE rather than inside the arithmetic, which is
// what makes every refusal reachable in a table test: a permission that could
// only be exercised against a recreated stream is one nobody re-checks.
//
// # The stream is read LIVE, and an unreadable one refuses
//
// The creation instant and both ends of the log come from ONE reading of the
// stream as it is now, because they decide the case and are what the checkpoint
// is about to be keyed to and positioned at. The instant used to be the one sampled at boot,
// which is a different stream once a log has been rebuilt under a running node:
// the refusal every read and write gave named the live instant, this asked the
// operator to confirm the old one, and confirming it keyed the checkpoint to a
// stream that no longer existed — so the next boot found the domain recreated
// all over again. And a reading that fails is an error rather than a zero: a
// first sequence of zero puts the checkpoint at the stream's very beginning,
// which is a claim about a stream nobody looked at.
//
// Everything ELSE fails into a fact the permission check already weighs: an
// unreadable register leaves RegisterReadable false, which [statelog.Reanchor]
// refuses on unless the operator forces it. A read error returned there instead
// would abort the call before the refusal that names what is actually wrong.
//
// # And every peer the fleet has EVICTED is left out, and its generation named
//
// Neither the most-caught-up rule nor the already-re-anchored one counts an
// evicted node: it is not coming back, and a decommissioned node that had
// re-anchored held every other node's reanchor refused for ever
// ([stateLog.evictedOn] says why that is asked of the log). What it DID open is
// kept, as [statelog.ReanchorInputs.Abandoned]: from its row, a floor it
// published, and the generation records on the log — read whether or not the
// register can be, because a node that died between its append and its
// position row opened a generation no row names. The second value is the
// peers that have already re-anchored, by name, for the refusal.
func (e *Engine) reanchorInputs(ctx context.Context,
	running *runningDomain) (statelog.ReanchorInputs, []string, error) {

	stream := running.domain.Stream().Name
	stats, err := running.log.Stats(ctx)
	if err != nil {
		return statelog.ReanchorInputs{}, nil, fmt.Errorf("%w: %s could not be read, "+
			"so neither its creation instant nor its first sequence — what the "+
			"checkpoint would be keyed to and positioned at — is known; check the "+
			"broker and re-run: %w", statelog.ErrReanchorRefused, stream, err)
	}
	// THE CHECKPOINT ROW, not the runner's memory of it: the generation the
	// transition derives from and the instant the rows are keyed to are the
	// durable ones, and the loop that would move them is halted.
	at, keyed, _, err := statelog.CursorFor(ctx, e.backends.Store.Replicated(), stream)
	if err != nil {
		return statelog.ReanchorInputs{}, nil, err
	}
	in := streamFacts(stream, stats, at, keyed)
	in.ClaimsIdentity = running.domain.ClaimsIdentity()
	if !in.ClaimsIdentity {
		// NO FLEET GUARD APPLIES, and no generation is anybody's but this
		// node's own: every node re-anchors its own copy of such a log.
		return in, nil, nil
	}
	domain := running.domain.Name()
	n := e.native.Load()
	self := n.nodeID

	// UNREADABLE IS NOT "no peers". A register nobody could list is exactly
	// the outage during which re-anchoring is most tempting and least
	// justified, so RegisterReadable stays false and the permission refuses
	// on it — which is why the read's error goes no further than this.
	rows, readErr := e.backends.Fleet.Positions(ctx)
	// A FLOOR IS ONLY EVER A SOURCE OF AN ABANDONED GENERATION here, and one
	// that cannot be read leaves that number to the log's own records.
	floors, _ := e.backends.Fleet.Floors(ctx)
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
	for _, f := range floors {
		if f.Domain == domain && f.Generation > in.Generation && f.By != "" && f.By != self {
			candidates = append(candidates, f.By)
			through = max(through, f.Generation)
		}
	}
	enc, err := generationEncoder(running.domain)
	if err != nil {
		return statelog.ReanchorInputs{}, nil, err
	}
	openers, err := statelog.GenerationOpeners(ctx, running.domain, enc, running.log,
		in.Generation, through)
	if err != nil {
		return statelog.ReanchorInputs{}, nil, fmt.Errorf("%w: which generations of %s "+
			"are already open could not be read off its log, so the one this "+
			"reanchor would open is unknown — check the broker and re-run: %w",
			statelog.ErrReanchorRefused, stream, err)
	}
	for _, writer := range openers {
		if writer != "" && writer != self {
			candidates = append(candidates, writer)
		}
	}
	evicted, err := n.log.evictedOn(ctx, running.domain, running.log, candidates)
	if err != nil {
		return statelog.ReanchorInputs{}, nil, fmt.Errorf("%w: whether the peers ahead "+
			"of this node on %s are evicted could not be read, and an evicted "+
			"peer's generation is abandoned where a live one's is the fleet's — "+
			"check the broker and re-run: %w", statelog.ErrReanchorRefused, stream, err)
	}

	var peers []string
	if readErr == nil {
		in.RegisterReadable = true
		for _, row := range rows {
			if row.NodeID == self || evicted[row.NodeID] {
				continue
			}
			// THE SAME GENERATION AND THE SAME STREAM ONLY: a sequence at
			// another generation is a number in another space — a peer that
			// has already re-anchored reports the adopted stream's sequences
			// — and so is one at this generation on another stream: a node
			// that came up with no rows after the rebuild counts the NEW
			// stream from 1 at the same generation number. Neither says
			// anything about who went further along the stream this node's
			// rows came from.
			if at, runs := row.Domains[domain]; runs && at.Generation == in.Generation &&
				sameStream(at.StreamCreatedAt, in.KeyedTo) && at.Seq > in.Highest {
				in.Highest = at.Seq
			}
		}
		peers = reanchoredPeers(rows, domain, in.Generation, self, evicted)
		in.PeersReanchored = len(peers)
		for _, row := range rows {
			if at, runs := row.Domains[domain]; runs && evicted[row.NodeID] &&
				at.Generation > in.Generation {
				in.Abandoned = max(in.Abandoned, at.Generation)
			}
		}
	}
	for _, f := range floors {
		if f.Domain == domain && evicted[f.By] && f.Generation > in.Generation {
			in.Abandoned = max(in.Abandoned, f.Generation)
		}
	}
	for gen, writer := range openers {
		if evicted[writer] {
			in.Abandoned = max(in.Abandoned, gen)
		}
	}
	return in, peers, nil
}

// sameStream reports whether a peer's position can be on the stream this node's
// rows are keyed to.
//
// UNKNOWN COUNTS AS THE SAME: a row that names no instant was written by a build
// that did not publish one, and the comparison it feeds is the refusal that
// keeps a node that is not the most caught-up from discarding what a peer
// applied — so a row nothing can place is weighed as though it could be ahead.
func sameStream(peer, keyed time.Time) bool {
	return peer.IsZero() || keyed.IsZero() ||
		statelog.IdentityOf(keyed, peer, true) == statelog.StreamSame
}

// ErrUnknownStream reports a stream that is not a domain log this node runs —
// a name mistyped, or a node running no state log at all.
//
// A SENTINEL, because a caller answers it differently from every other failure
// of the same calls: nothing about it is transient, while a stream that could
// not be READ is the broker's to answer, and telling an operator who mistyped a
// name to wait and retry is as wrong as telling one whose broker blinked that
// the log does not exist.
var ErrUnknownStream = errors.New("engine: not a domain log this node runs")

// runningStream is the running domain whose log is stream, or
// [ErrUnknownStream] naming the streams there are.
func (e *Engine) runningStream(stream string) (*runningDomain, error) {
	n := e.native.Load()
	if n == nil || n.log == nil {
		return nil, fmt.Errorf("%w: this node runs no state log, so %q is not "+
			"one of its logs", ErrUnknownStream, stream)
	}
	running := n.log.Domain(n.log.domainOf(stream))
	if running == nil {
		return nil, fmt.Errorf("%w: %q — the streams this build runs are %v",
			ErrUnknownStream, stream, maintenanceStreams())
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
// SHARED by the status an operator reads before confirming and by the
// transition itself, so the two name the case from the same facts by the same
// rule ([statelog.ReanchorInputs.Case]) — a status that described a recreated
// stream while the transition went on to treat it as restored would have the
// operator confirm one thing and get the other.
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
}

// ReanchorStatus is what an operator reads before running it: the LIVE
// stream's own creation instant, which is the value the confirmation has to
// echo, the generation the domain stands at, and which case a reanchor would
// answer — so the operator confirms knowing whether the log is followed from
// its first record, its end or this node's own checkpoint.
//
// FROM THE SAME INPUTS THE TRANSITION READS ([Engine.reanchorInputs]), fleet
// included: whether the log continues in a generation an evicted peer
// abandoned is a fact about the register and the log together, and a status
// that read only the stream named no case at all for the one situation the
// operator's eviction had just made re-anchorable — so the command it prints
// would never have been offered. The guards are not run: whether this node is
// the one that may is the transition's answer, and the status says what it
// would do if it may.
//
// LIVE, for the reason [Engine.reanchorInputs] gives: it is the same instant a
// `wrong_stream` refusal names and the same one the permission check compares
// against, and a value the node sampled at boot is neither once the stream has
// been rebuilt under it. A stream that cannot be read is an error that is NOT
// [ErrUnknownStream], because the log exists and the broker did not answer.
func (e *Engine) ReanchorStatus(ctx context.Context, stream string) (ReanchorView, error) {
	running, err := e.runningStream(stream)
	if err != nil {
		return ReanchorView{}, err
	}
	in, _, err := e.reanchorInputs(ctx, running)
	if err != nil {
		return ReanchorView{}, fmt.Errorf("engine: read what a reanchor of %s would "+
			"be decided from: %w", stream, err)
	}
	view := ReanchorView{CreatedAt: in.StreamCreatedAt, Generation: in.Generation}
	if view.Case, view.Cursor, err = in.Case(); err != nil {
		view.Refusal = err.Error()
	}
	return view, nil
}

// reanchoredPeers names every peer that has already re-anchored this domain's
// stream: every peer at a LATER generation of it than this node's checkpoint.
//
// ONE DEFINITION, because two places need it and they must agree: the
// permission check counts them, and the refusal names them.
//
// A generation moves only by a reanchor, or by adopting the snapshot of a node
// that ran one, so a peer ahead of this node's generation holds the rows the
// fleet's history in that generation is made of — whatever it has applied since,
// which is why nothing else about its row is asked.
//
// # And a peer at this node's OWN generation is not one
//
// It was counted here once, as "hydrated on the live stream", on the reasoning
// that a peer which had applied anything at this generation held history a
// reanchor would discard. But a generation cannot say which stream a sequence
// is on, and every peer still on the LOST stream stands at this generation with
// everything it ever applied — so every one of them read as caught up on the
// live stream, the reanchor of any node in a fleet of two or more was refused,
// and the snapshot it sent the operator to was keyed to the lost stream, which
// no joiner can adopt. Such a peer is weighed by the most-caught-up rule
// instead, against the stream this node's rows came from
// ([statelog.ReanchorInputs]).
//
// # Nor is an EVICTED peer
//
// Its rows are not the fleet's history in any generation any more, and counted
// here a decommissioned node that had re-anchored refused every other node's
// reanchor for ever — see [stateLog.evictedOn].
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
