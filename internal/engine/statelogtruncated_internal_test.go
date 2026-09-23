package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// publishLoaded boots a node, waits for its tracker's loop to load the
// checkpoint want — so the position it publishes is its rows' own rather than
// the zero a runner reports before — publishes, and stops it.
func publishLoaded(t *testing.T, d divergedBroker, want statelog.Position) {
	t.Helper()
	e, back := bootNode(t, &d.a, d.cfg)
	running := e.native.Load().log.Domain(tracker.Domain{}.Name())
	waitUntil(t, 10*time.Second, "node A's tracker to load its checkpoint", func() bool {
		return running.runner.Committed() == want
	})
	e.native.Load().log.publishPositions(t.Context())
	e.Stop(context.Background())
	back.Close(context.Background())
}

// requireTruncatedRefusal asserts a write refused `log_truncated` over the
// truncation, naming the peer.
func requireTruncatedRefusal(t *testing.T, err error, peer string) {
	t.Helper()
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonLogTruncated ||
		!errors.Is(err, statelog.ErrLogTruncated) {
		t.Fatalf("the write returned %v, want log_truncated over the truncation", err)
	}
	if !strings.Contains(err.Error(), peer) {
		t.Fatalf("the refusal %q does not name the peer %s", err, peer)
	}
}

// A NODE WHOSE PEER STANDS PAST A RESTORED LOG'S END REFUSES ITS WRITES UNTIL
// THE OPERATOR DECIDES — AND THE PEER'S EVICTION IS ONE WAY TO DECIDE.
//
// Node B's rows are the restored copy's age, so its own log looks like its own
// history and nothing refused it: it wrote, and every write was one the
// restored reanchor of node A — whose rows hold the tail the copy lost — would
// apply nowhere. B now reads A's position past the log's end off the register,
// confirms the log holds no record there, and refuses its writes as
// `log_truncated`, naming A; its reads go on. Evicting A is the one write it
// still makes, and with A evicted its writes are served again.
func TestANodeWhosePeerStandsPastARestoredLogRefusesItsWrites(t *testing.T) {
	t.Parallel()
	d := stageRestoredBroker(t)
	publishLoaded(t, d, d.checkpoint.at)

	eb, _ := bootNode(t, &d.b, d.cfg)
	waitUntil(t, 20*time.Second, "node B to admit seats", eb.NativeHydrated)
	running := eb.native.Load().log.Domain(tracker.Domain{}.Name())
	eb.native.Load().log.publishPositions(t.Context())
	if err := running.runner.Truncated(); !errors.Is(err, statelog.ErrLogTruncated) {
		t.Fatalf("node B's truncation is %v, want node A's position past the end", err)
	}
	if err := running.runner.StreamIdentity(); err != nil {
		t.Fatalf("node B's reads refuse over a peer's rows: %v", err)
	}
	title := "refused"
	_, err := eb.native.Load().writer.UpdateTask(t.Context(), "op-b-refused", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil)
	requireTruncatedRefusal(t, err, d.a.Node.ID)

	// THE OPERATOR EVICTS A, from B.
	res, err := eb.NodeGate().Evict(t.Context(), GateRequest{
		Node: d.a.Node.ID, OpID: "op-evict-a", By: "ops-1",
	})
	if err != nil || !res.Complete() {
		t.Fatalf("the eviction of node A from node B = %+v, %v — it is one of the "+
			"operator's ways out, and a fence that refused it would take it away", res, err)
	}
	eb.native.Load().log.publishPositions(t.Context())
	if err := running.runner.Truncated(); err != nil {
		t.Fatalf("with node A evicted, node B's writes still refuse: %v", err)
	}
	mustApply(t, "node B's write once node A is evicted", func() (tracker.WriteResult, error) {
		title := "after the eviction"
		return eb.native.Load().writer.UpdateTask(t.Context(), "op-b-after", "t-1", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil)
	})
}

// A NODE WHOSE PEER SAYS THE LOG DIVERGED FROM ITS ROWS REFUSES ITS WRITES TOO.
//
// Once the restored log has been written past node A's checkpoint, A's
// position is at or below the log's end, where a lagging node's is — so the
// flag A publishes is the only thing that tells B a write from here is one
// more the operator's choice would have to throw away.
func TestANodeWhosePeerSaysTheLogDivergedRefusesItsWrites(t *testing.T) {
	t.Parallel()
	d := stageDivergedBroker(t)
	publishLoaded(t, d, d.checkpoint.at)

	eb, _ := bootNode(t, &d.b, d.cfg)
	waitUntil(t, 20*time.Second, "node B to admit seats", eb.NativeHydrated)
	running := eb.native.Load().log.Domain(tracker.Domain{}.Name())
	eb.native.Load().log.publishPositions(t.Context())
	err := running.runner.Truncated()
	if !errors.Is(err, statelog.ErrLogTruncated) || !strings.Contains(err.Error(), "another record") {
		t.Fatalf("node B's truncation is %v, want node A's divergence", err)
	}
	title := "refused"
	_, err = eb.native.Load().writer.UpdateTask(t.Context(), "op-b-refused", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil)
	requireTruncatedRefusal(t, err, d.a.Node.ID)
}

// WHICH PEERS COUNT, ASKED OF THE REGISTER ROWS DIRECTLY.
//
// Only a peer on the stream this node's rows are keyed to, in its generation,
// and either past the log's end — as the log itself confirms, since an end
// read from a member that has not caught up is below a healthy peer's position
// too — or saying the log diverged from its rows. A number in another space
// says nothing about this log, and a record the log holds at a peer's
// checkpoint says the end was a stale member's.
func TestWhichPeersHoldWhatTheLogLost(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	running := s.Domain(tracker.Domain{}.Name())
	for i := range 3 {
		if res, err := e.native.Load().writer.EvictNode(t.Context(), fmt.Sprintf("op-%d", i),
			fmt.Sprintf("gone-%d", i)); err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("a write: %+v, %v", res, err)
		}
	}
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	at := running.runner.Committed()
	end := stats.LastSeq
	keyed := running.runner.KeyedTo()
	peer := func(seq uint64, mutate func(*coord.DomainPosition)) []coord.NodePositions {
		pos := coord.DomainPosition{Seq: seq, Generation: at.Generation, StreamCreatedAt: keyed}
		if mutate != nil {
			mutate(&pos)
		}
		return []coord.NodePositions{{
			NodeID:  "node-peer",
			Domains: map[string]coord.DomainPosition{tracker.Domain{}.Name(): pos},
		}}
	}
	for name, tc := range map[string]struct {
		rows  []coord.NodePositions
		last  uint64
		wants bool
	}{
		"a peer past the log's end": {rows: peer(end+5, nil), last: end, wants: true},
		"a peer past a stale member's end, on a record the log holds": {
			rows: peer(end, nil), last: end - 1,
		},
		"a peer at the end": {rows: peer(end, nil), last: end},
		"a peer saying the log diverged from its rows": {
			rows: peer(end-1, func(p *coord.DomainPosition) { p.LogDiverged = true }),
			last: end, wants: true,
		},
		"a peer past the end in another generation": {
			rows: peer(end+5, func(p *coord.DomainPosition) { p.Generation++ }), last: end,
		},
		"a peer past the end on another stream": {
			rows: peer(end+5, func(p *coord.DomainPosition) {
				p.StreamCreatedAt = keyed.Add(time.Hour)
			}),
			last: end,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := s.truncation(t.Context(), running, tc.rows, at, tc.last)
			if err != nil {
				t.Fatalf("truncation: %v", err)
			}
			if (got != nil) != tc.wants {
				t.Fatalf("truncation = %+v, want a finding: %v", got, tc.wants)
			}
			if got != nil && got.Peer != "node-peer" {
				t.Fatalf("the finding names %q, want node-peer", got.Peer)
			}
		})
	}
}
