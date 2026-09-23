package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVICTING A PEER THAT RE-ANCHORED AND VANISHED RELEASES THE FLEET IT STRANDED.
//
// A peer re-anchored the tracker's log and was decommissioned before anybody
// adopted from it. Its generation record and one more record it wrote in that
// generation are on the log, and — in the first case — its position row names
// the new generation for ever, since an eviction deliberately keeps a row. Read
// as the fleet's, that row held this node in two traps: it stopped and asked
// for a donor at a generation nobody could give, and its reanchor refused,
// force or no force, on a peer that had already re-anchored. In the second case
// the peer died between its append and its row, so only the log says what it
// opened: this node stops on the record, and its reanchor finds nothing to
// re-anchor, since the log holds everything its rows are missing.
//
// The operator's eviction of the peer, run on this very node, is the remedy in
// both: it lands although the node is refusing every other write; the peer
// then counts toward neither the fleet's generation nor the reanchor guards;
// and the reanchor is the ABANDONED case — the generation after the peer's,
// from this node's own checkpoint — which voids the peer's records while
// keeping a live peer's record written in this node's generation after the
// peer's move, and the eviction itself. The refusals say so as it happens:
// before the eviction they name the adoption and, beside it, the eviction and
// reanchor for a peer that is gone; after it, only the reanchor — there is no
// snapshot at that generation left to adopt.
func TestEvictingAPeerThatReanchoredAndVanishedReleasesTheFleet(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// published is whether the peer's position row names its generation.
		published bool
		// stranded is what a reanchor says before the eviction.
		stranded string
	}{
		{"its row names the generation it opened", true, "re-anchored peers: [node-x]"},
		{"it died before its row said so", false, "nothing to re-anchor"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			strandedByAVanishedPeer(t, c.published, c.stranded)
		})
	}
}

func strandedByAVanishedPeer(t *testing.T, published bool, stranded string) {
	t.Helper()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	running := s.Domain(tracker.Domain{}.Name())
	name := running.domain.Name()
	spec := running.domain.Stream()
	mustApply(t, "a write before the peer's move", func() (tracker.WriteResult, error) {
		return e.native.Load().writer.EvictNode(t.Context(), "op-before", "node-before")
	})
	own := running.runner.Committed()
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the tracker's log: %v", err)
	}
	live := stats.CreatedAt

	// THE PEER'S MOVE: its generation record, a record it wrote in that
	// generation, and — when it lived long enough — its row.
	rec, _, err := tracker.GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: own.Generation + 1, Case: statelog.ReanchorRestored,
		Inputs: statelog.ReanchorInputs{Stream: spec.Name, StreamCreatedAt: live},
		By:     "ops-x", Writer: "node-x", At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the peer's generation record: %v", err)
	}
	zero := uint64(0)
	if _, _, err := running.log.Append(t.Context(), spec.SubjectPrefix+"."+rec.Subject.String(),
		rec.OpID, &zero, rec.Payload); err != nil {
		t.Fatalf("land the peer's generation record: %v", err)
	}
	appendEvictionAs(t, running, "op-x-abandoned", "node-voided", own.Generation+1, "node-x")
	// A LIVE PEER'S RECORD in this node's generation, written before that
	// peer learned of the move — the history these rows continue.
	appendEvictionAs(t, running, "op-live", "node-kept", own.Generation, "node-live")
	if published {
		end, err := running.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read the tracker's log: %v", err)
		}
		if err := e.backends.Fleet.PutPositions(t.Context(), coord.NodePositions{
			NodeID: "node-x", At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{name: {
				Generation: own.Generation + 1, Seq: end.LastSeq,
				AppliedThrough: end.LastSeq, StreamCreatedAt: live,
			}},
		}); err != nil {
			t.Fatalf("publish the peer's row: %v", err)
		}
	}

	// STRANDED: stopped on the peer's generation, and refused a reanchor.
	s.publishPositions(t.Context())
	waitUntil(t, 10*time.Second, "the tracker to stop on the peer's generation", func() bool {
		return errors.Is(running.runner.Stopped(), statelog.ErrGenerationPassed)
	})
	refusal := running.runner.StreamIdentity()
	for _, says := range []string{"adopts a snapshot", "evicts that peer"} {
		if refusal == nil || !strings.Contains(refusal.Error(), says) {
			t.Fatalf("before the eviction the refusal is %v, want it to say %q", refusal, says)
		}
	}
	confirm := statelog.ConfirmationOf(live)
	if _, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: spec.Name, Confirm: confirm, By: "ops-1", Force: true,
	}); !errors.Is(err, statelog.ErrReanchorRefused) || !strings.Contains(err.Error(), stranded) {
		t.Fatalf("a forced reanchor while node-x counts = %v, want a refusal saying %q",
			err, stranded)
	}

	// THE OPERATOR EVICTS THE PEER, from the node it stranded.
	res, err := e.NodeGate().Evict(t.Context(), GateRequest{
		Node: "node-x", OpID: "op-evict-x", By: "ops-1",
	})
	if err != nil || !res.Complete() {
		t.Fatalf("the eviction of node-x from the node it stranded = %+v, %v — "+
			"every log must hold it, and a node the fleet re-anchored past refused "+
			"it as it refuses every other write", res, err)
	}
	rows, err := s.fleet.Positions(t.Context())
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	gens, err := s.fleetGenerations(t.Context(), rows, s.openLogs(),
		map[string]uint32{name: own.Generation})
	if err != nil {
		t.Fatalf("fleetGenerations: %v", err)
	}
	if gens[name] != own.Generation {
		t.Fatalf("the fleet is on generation %d of the tracker after node-x's eviction, "+
			"want this node's %d — an evicted peer's generation is not the fleet's",
			gens[name], own.Generation)
	}
	// AND THE REFUSAL NO LONGER SENDS THIS NODE TO WAIT FOR A DONOR.
	s.publishPositions(t.Context())
	refusal = running.runner.StreamIdentity()
	if !errors.Is(refusal, statelog.ErrGenerationPassed) ||
		strings.Contains(refusal.Error(), "adopts a snapshot") ||
		!strings.Contains(refusal.Error(), "only nodes the fleet has evicted") ||
		!strings.Contains(refusal.Error(), "re-anchors this stream") {
		t.Fatalf("after node-x's eviction the refusal is %v, want it to name the "+
			"reanchor as the one remedy — no live node holds that generation", refusal)
	}

	// THE REANCHOR: the abandoned case, into the generation after the peer's,
	// from this node's own checkpoint — and with no force.
	view, err := e.ReanchorStatus(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}
	if view.Case != statelog.ReanchorAbandoned || view.Cursor != own.Seq {
		t.Fatalf("ReanchorStatus names %q at %d (%s), want the abandoned case at "+
			"this node's checkpoint, %d", view.Case, view.Cursor, view.Refusal, own.Seq)
	}
	plan, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: spec.Name, Confirm: confirm, By: "ops-1",
	})
	if err != nil {
		t.Fatalf("Reanchor once node-x was evicted: %v", err)
	}
	if plan.Case != statelog.ReanchorAbandoned || plan.Generation != own.Generation+2 ||
		plan.Cursor != own.Seq {
		t.Fatalf("reanchored as %+v, want the abandoned case into generation %d at %d",
			plan, own.Generation+2, own.Seq)
	}
	waitUntil(t, 10*time.Second, "the reanchor's own record to apply", func() bool {
		return countRows(t, e, `SELECT COUNT(*) FROM tracker_log_generations
			WHERE generation = ?`, plan.Generation) == 1
	})
	for _, c := range []struct {
		node string
		want int
		why  string
	}{
		{"node-voided", 0, "node-x wrote it in the generation it abandoned"},
		{"node-kept", 1, "a live peer wrote it in this node's own generation"},
		{"node-x", 1, "it is the eviction that released this node"},
	} {
		if got := countRows(t, e, `SELECT COUNT(*) FROM tracker_evictions
			WHERE node_id = ? AND readmitted_position IS NULL`, c.node); got != c.want {
			t.Errorf("%d eviction row(s) of %s, want %d: %s", got, c.node, c.want, c.why)
		}
	}
	if got := countRows(t, e, `SELECT COUNT(*) FROM tracker_log_generations
		WHERE generation = ?`, own.Generation+1); got != 0 {
		t.Errorf("node-x's generation record applied into this node's rows (%d row)", got)
	}
	after := mustApply(t, "a write after the reanchor", func() (tracker.WriteResult, error) {
		return e.native.Load().writer.EvictNode(t.Context(), "op-after", "node-after")
	})
	if after.Position.Generation != plan.Generation {
		t.Fatalf("the write landed at %s, want generation %d", after.Position, plan.Generation)
	}
}
