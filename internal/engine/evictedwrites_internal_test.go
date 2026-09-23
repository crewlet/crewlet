package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN EVICTED NODE'S OWN WRITES, MADE THROUGH ITS REAL WRITERS, ARE DROPPED BY
// THE APPLIER — in both identity-claiming domains, on real streams.
//
// The eviction gate is clause (iii) of the floor theorem: the one that holds
// when the other two are defeated, because it depends on nothing but the log's
// own order and on each record naming the node that wrote it. No write path in
// the tree named one. The tracker's writer and the page store both built an
// envelope with an empty writer, so the gate's "is this writer evicted" never
// matched a row, and a node the fleet had removed went on writing records every
// applier took. The cases that exercised the gate all hand-built their records
// with a writer filled in, which is exactly the half production never did.
//
// So the evictions here land on each log first — as a peer would publish them
// — while this node's appliers are halted, which is what a node that has not
// yet noticed its own eviction is. Its own writes then go through the tracker
// writer and the page store it serves every seat with, land above the
// eviction, and the appliers, once running, must drop them.
func TestAnEvictedNodesRealWritesAreDroppedByTheApplier(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	s := e.native.Load().log
	self := s.nodeID
	if e.native.Load().writer == nil || e.native.Load().pages == nil {
		t.Fatal("the node runs no tracker writer or no page store")
	}
	// THE HEARTBEAT IS WATCHED RATHER THAN OBEYED: halted appliers look
	// like a node below the log, and an adoption racing the writes below
	// would be what they measured.
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { return nil }
	s.rejoinMu.Unlock()
	trackerLog := s.Domain(tracker.Domain{}.Name())
	pagesLog := s.Domain(pages.Domain{}.Name())

	// THE CONTROL: the same two writes, made before any eviction, apply.
	// Without it a harness that applied nothing of this node's would pass
	// the case below.
	if _, err := e.native.Load().writer.WriteView(t.Context(), "op-view-before",
		evictedView("v-before")); err != nil {
		t.Fatalf("the control view write: %v", err)
	}
	if _, _, err := e.native.Load().pages.EnsureContainer(t.Context(), testActivation, "BEFORE",
		"Before", ""); err != nil {
		t.Fatalf("the control container write: %v", err)
	}
	waitApplied(t, trackerLog)
	waitApplied(t, pagesLog)
	requireRow(t, back, "tracker_views", "id", "v-before", true)
	requireRow(t, back, "pages_containers", "key", "BEFORE", true)

	// THE NODE STOPS APPLYING, and the fleet evicts it on both logs.
	s.haltAppliers()
	appendRecord(t, trackerLog, tracker.EvictionSubject(self).String(), trackerEviction(t, self))
	appendRecord(t, pagesLog, pages.EvictionSubject(self).String(), pagesEviction(t, self))

	// ITS OWN WRITES, THROUGH ITS REAL WRITERS: its fence reads its own
	// applied rows, which do not hold the eviction yet, so both land.
	// Neither can resolve — the appliers are halted — so each answers
	// pending, which is the honest answer and the one asserted.
	res, err := e.native.Load().writer.WriteView(t.Context(), "op-view-evicted", evictedView("v-evicted"))
	if err != nil || res.Outcome != statelog.OutcomePending {
		t.Fatalf("the evicted node's view write answered %q (err %v), want "+
			"pending — it has to LAND for the gate to have anything to drop",
			res.Outcome, err)
	}
	viewAt := res.Position
	if _, _, err := e.native.Load().pages.EnsureContainer(t.Context(), testActivation, "EVICTED",
		"Evicted", ""); err != nil {
		t.Fatalf("the evicted node's container write: %v", err)
	}
	_, pageSeq, err := pagesLog.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("read the pages log's end: %v", err)
	}
	pageAt := pagesLog.runner.Committed().At(pageSeq)

	// BOTH RECORDS NAME THIS NODE — the one fact the gate reads.
	requireWriter(t, trackerLog, viewAt.Seq, self)
	requireWriter(t, pagesLog, pageSeq, self)

	relaunch(t, s)
	waitApplied(t, trackerLog)
	waitApplied(t, pagesLog)

	requireRow(t, back, "tracker_views", "id", "v-evicted", false)
	requireRow(t, back, "pages_containers", "key", "EVICTED", false)

	// AND THE PUBLISHER'S OWN READING OF THE GATE AGREES, so a write
	// resolved later is told `evicted` rather than "somebody else won".
	if reason, gated, err := tracker.NewGates(back.Store).GatedAt(t.Context(),
		statelog.Subject{Kind: string(tracker.KindView), ID: "v-evicted"},
		self, "op-view-evicted", viewAt); err != nil || !gated ||
		reason != statelog.ReasonEvicted {
		t.Fatalf("the tracker's gate reads (%q, %v, %v) for the evicted node's "+
			"record, want evicted", reason, gated, err)
	}
	if reason, gated, err := pages.NewGates(back.Store).GatedAt(t.Context(),
		statelog.Subject{Kind: string(pages.KindContainer), ID: "EVICTED"},
		self, "", pageAt); err != nil || !gated ||
		reason != statelog.ReasonEvicted {
		t.Fatalf("the pages gate reads (%q, %v, %v) for the evicted node's "+
			"record, want evicted", reason, gated, err)
	}
}

// evictedView is a shared workspace view, the smallest thing the tracker's
// writer publishes that leaves a row a case can look for.
func evictedView(id string) tracker.View {
	return tracker.View{
		ID: id, Name: "View " + id, Type: tracker.ViewList,
		Container: tracker.Container{Kind: tracker.ContainerWorkspace},
	}
}

// trackerEviction is the record a peer's writer publishes to evict node,
// written by that peer.
func trackerEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(tracker.Eviction{
		V: tracker.GateRecordVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "op-evict-" + node,
			Subject: tracker.EvictionSubject(node), Op: tracker.OpEviction,
			Writer: "node-peer", Scope: tracker.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "node-peer", ActorKind: tracker.AuthorSystem,
	}.Encode()
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// pagesEviction is the same for the knowledge base's log.
func pagesEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(pages.Eviction{
		V: pages.GateRecordVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := pages.Encode(pages.MutationRecord{
		RecordEnvelope: pages.RecordEnvelope{
			V: pages.RecordVersion, OpID: "op-evict-" + node,
			Subject: pages.EvictionSubject(node), Op: pages.OpEviction,
			Writer: "node-peer", Scope: pages.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "node-peer", ActorKind: pages.AuthorOperator,
	})
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// appendRecord puts one encoded record on a domain's log at its own subject.
func appendRecord(t *testing.T, running *runningDomain, subject string, payload []byte) uint64 {
	t.Helper()
	seq, _, err := running.log.Append(t.Context(),
		running.domain.Stream().SubjectPrefix+"."+subject, "", nil, payload)
	if err != nil {
		t.Fatalf("append to %s: %v", running.domain.Name(), err)
	}
	return seq
}

// relaunch starts halted appliers again the way a runtime rejoin does: each
// consumer moved to its applier's own checkpoint first, because a pull the halt
// interrupted leaves records delivered and unacknowledged, and the broker would
// otherwise hand them over again only after its thirty-second ack window.
func relaunch(t *testing.T, s *stateLog) {
	t.Helper()
	for _, name := range s.order {
		running := s.domains[name]
		if err := running.consumer.Reset(t.Context(), running.runner.Committed().Seq); err != nil {
			t.Fatalf("move %s's consumer to its checkpoint: %v", name, err)
		}
	}
	s.launchAppliers(s.run)
}

// waitApplied waits for a domain's applier to commit its whole log.
func waitApplied(t *testing.T, running *runningDomain) {
	t.Helper()
	_, last, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("read %s's end: %v", running.domain.Name(), err)
	}
	waitUntil(t, 20*time.Second, running.domain.Name()+" to apply its whole log",
		func() bool { return running.runner.Committed().Seq >= last })
}

// requireWriter asserts the record at seq names writer.
func requireWriter(t *testing.T, running *runningDomain, seq uint64, writer string) {
	t.Helper()
	_, payload, _, ok, err := running.log.At(t.Context(), seq)
	if err != nil || !ok {
		t.Fatalf("read %s at %d: ok %v, err %v", running.domain.Name(), seq, ok, err)
	}
	env, err := running.domain.Envelope(payload)
	if err != nil {
		t.Fatalf("decode %s at %d: %v", running.domain.Name(), seq, err)
	}
	if env.Writer != writer {
		t.Fatalf("the %s record at %d names writer %q, want %q — the eviction "+
			"gate compares exactly this", running.domain.Name(), seq, env.Writer, writer)
	}
}

// requireRow asserts whether one row exists in the replicated estate.
func requireRow(t *testing.T, back *Backends, table, column, key string, want bool) {
	t.Helper()
	var n int
	if err := back.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM `+table+` WHERE `+column+` = ?`, key).Scan(&n)
	}); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read %s: %v", table, err)
	}
	if (n > 0) != want {
		t.Fatalf("%s holds %d row(s) for %s = %q, want present=%v", table, n,
			column, key, want)
	}
}
