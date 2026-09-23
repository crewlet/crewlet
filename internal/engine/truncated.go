package engine

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A LOG THAT LOST WHAT A PEER'S ROWS HOLD REFUSES EVERYBODY'S WRITES.
//
// A broker restored from an older copy is seen by the nodes whose rows are
// newer than the copy — their checkpoints are past the log's end, or the log
// diverged from them — and by nothing else. A node whose rows were the copy's
// age sees a log that is its own history, so it went on writing: every write
// acknowledged, applied on its own node, and appended after the restored end.
// If the operator then keeps the newer node's rows — its restored reanchor,
// which follows the log from its end — those writes are applied nowhere: lost
// after being acknowledged, and the longer the fleet went on, the more of them.
// Only the operator can say which history the fleet keeps, and a write made
// before they have is one made on a guess.
//
// So the fleet's own register decides it: a peer on the stream this node's
// rows are keyed to, in the same generation, not evicted, whose published
// position is past the log's end — confirmed against the log, which must not
// hold the record at it — or which says the log diverged from its rows, stops
// this node's writes until it is resolved ([statelog.ErrLogTruncated]). Its
// reads go on: they are the log's own history. The eviction of the peer is the
// one write that is not refused, because it is one of the operator's ways out.
//
// # What it cannot see
//
// A peer whose row does not yet say so: after a whole broker restore the
// register is the copy's too, so a newer node that has not booted since — or
// has not yet published — is at the copy's position in it, and writes made
// before it does land. What this fence bounds is how many there are.

// truncation is the peer whose rows hold records one domain's log lost, as the
// register rows and one reading of the log's end (last) show it — nil when no
// peer's do, or for a domain that claims no identity, whose nodes' rows differ
// by design.
//
// A READ THAT FAILS IS AN ERROR, and the caller leaves the verdict where it was:
// a register nobody could list, or a log that could not be asked, is neither a
// peer that holds more nor one that does not.
func (s *stateLog) truncation(ctx context.Context, running *runningDomain,
	rows []coord.NodePositions, at statelog.Position, last uint64) (*statelog.Truncation, error) {

	if !running.domain.ClaimsIdentity() {
		return nil, nil
	}
	name := running.domain.Name()
	keyed := running.runner.KeyedTo()
	ahead := map[string]coord.DomainPosition{}
	var candidates []string
	for _, row := range rows {
		pos, runs := row.Domains[name]
		// THE SAME STREAM AND THE SAME GENERATION ONLY: a sequence in
		// another generation, or on another stream at this one, is a
		// number in another space and past nothing this log holds.
		if !runs || row.NodeID == s.nodeID || pos.Generation != at.Generation ||
			!sameStream(pos.StreamCreatedAt, keyed) {
			continue
		}
		if pos.LogDiverged || pos.Seq > last {
			ahead[row.NodeID] = pos
			candidates = append(candidates, row.NodeID)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// A STABLE ORDER, so the peer a refusal names does not flip between
	// beats while several hold more.
	slices.Sort(candidates)
	evicted, err := s.evictedOn(ctx, running.domain, running.log, candidates)
	if err != nil {
		return nil, err
	}
	var found *statelog.Truncation
	for _, node := range candidates {
		if evicted[node] {
			// A PEER THE FLEET EVICTED is not coming back, and what its
			// rows held is on no disk the fleet will read again.
			continue
		}
		pos := ahead[node]
		if !pos.LogDiverged {
			// CONFIRMED AGAINST THE LOG ITSELF: an end read from a member
			// that has not caught up is below a healthy peer's position
			// too, and a record the log holds at the peer's checkpoint
			// says the end was that member's.
			_, _, _, held, readErr := running.log.At(ctx, pos.Seq)
			if readErr != nil {
				return nil, fmt.Errorf("engine: read %s's record at peer %s's "+
					"checkpoint %d: %w", name, node, pos.Seq, readErr)
			}
			if held {
				continue
			}
		}
		t := &statelog.Truncation{Peer: node, Seq: pos.Seq, Last: last, Diverged: pos.LogDiverged}
		if found == nil || t.Seq > found.Seq {
			found = t
		}
	}
	return found, nil
}

// observeTruncation hands one reading of the register to a domain's runner and
// names both transitions — see [statelog.Runner.ObserveTruncation].
func (s *stateLog) observeTruncation(ctx context.Context, name string,
	runner *statelog.Runner, t *statelog.Truncation) {

	established, cleared := runner.ObserveTruncation(t)
	switch {
	case established:
		log.ErrorContext(ctx, "statelog_log_truncated",
			"node", s.nodeID, "domain", name, "peer", t.Peer,
			"peer_seq", t.Seq, "last_seq", t.Last, "peer_diverged", t.Diverged,
			"error", runner.Truncated().Error(),
			"detail", "a peer's rows hold records this log lost — the broker was "+
				"restored from a copy older than them — so this node refuses its "+
				"writes of the domain (`log_truncated`) and serves its reads until "+
				"an operator decides which history the fleet keeps: re-anchor that "+
				"peer (crewlet retention reanchor), replace its rows with a peer's, "+
				"or evict it (crewlet retention evict)")
	case cleared:
		log.WarnContext(ctx, "statelog_log_truncated_cleared",
			"node", s.nodeID, "domain", name,
			"detail", "no peer's rows hold records this log lost any more — the "+
				"peer re-anchored, was rebuilt from a peer or evicted, or the log "+
				"holds its checkpoint after all — so this node's writes of the "+
				"domain are served again")
	}
}
