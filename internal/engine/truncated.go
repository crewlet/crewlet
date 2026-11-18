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
// After a broker restore from an older copy only the nodes with newer rows see
// it; one at the copy's age writes on, and if the operator keeps the newer
// rows (their restored reanchor) those acknowledged writes are applied nowhere.
// Only the operator can pick the history, so a peer on this node's keyed
// stream, same generation, not evicted, whose position is past the log's end
// (confirmed: the log must not hold that record) or which reports divergence
// stops this node's writes until resolved ([statelog.ErrLogTruncated]). Reads
// go on; evicting that peer, a way out, is allowed. A peer whose row is still
// the copy's is unseen: the fence bounds the writes before it publishes, and
// its reanchor discards none unasked ([statelog.ReanchorInputs.Unheld]).

// truncation is the peer whose rows hold records one domain's log lost, by the
// register rows and one reading of the log's end (last); nil when none does, or
// for a domain that claims no identity, whose nodes' rows differ by design.
//
// A READ THAT FAILS IS AN ERROR and the caller keeps its verdict: an unread
// register or log shows neither a peer that holds more nor one that does not.
func (s *stateLog) truncation(ctx context.Context, running *runningLog,
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
		// THE SAME STREAM AND GENERATION ONLY: a sequence in another is in
		// another number space and past nothing this log holds.
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
	evicted, err := s.evictedOn(ctx, running.domain, running.spec, running.standing(), candidates)
	if err != nil {
		return nil, err
	}
	var found *statelog.Truncation
	for _, node := range candidates {
		if evicted[node] {
			// A PEER THE FLEET EVICTED holds rows no disk will read again.
			continue
		}
		pos := ahead[node]
		if !pos.LogDiverged {
			// CONFIRMED AGAINST THE LOG: an end read from a lagging member
			// is below a healthy peer too; a record held at the peer's
			// checkpoint says the end was that member's.
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
	runner *statelog.Runner, stance statelog.Stance, t *statelog.Truncation) {

	established, cleared := runner.ObserveTruncation(stance, t)
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
