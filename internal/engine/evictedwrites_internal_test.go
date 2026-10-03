package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN EVICTED NODE'S OWN WRITES, MADE THROUGH ITS REAL WRITERS, ARE DROPPED BY
// THE APPLIER — in every identity-claiming domain, on real streams.
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
// AND THE ORG CHART'S AND THE IDENTITY ESTATE'S WRITERS WERE THE SAME: until
// their writes were stamped, a chart or identity record carried no writer at
// all, so an evicted node's structural edits and session invalidations applied
// on every node while its tracker writes were dropped.
//
// So the evictions here land on each log first — as a peer would publish them
// — while this node's appliers are halted, which is what a node that has not
// yet noticed its own eviction is. Its own writes then go through the writers
// it serves every seat and person with, land above the eviction, and the
// appliers, once running, must drop them.
func TestAnEvictedNodesRealWritesAreDroppedByTheApplier(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	n, c := e.native.Load(), e.core.Load()
	s := c.log
	self := s.nodeID
	if n.writer == nil || n.pages == nil || c.chartWriter == nil || c.iamWriter == nil {
		t.Fatal("the node runs no tracker writer, page store, chart writer or " +
			"identity writer")
	}
	// THE HEARTBEAT IS WATCHED RATHER THAN OBEYED: halted appliers look
	// like a node below the log, and an adoption racing the writes below
	// would be what they measured.
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { return nil }
	s.rejoinMu.Unlock()
	trackerLog := s.Domain(tracker.Domain{}.Name())
	pagesLog := s.Domain(pages.Domain{}.Name())
	chartLog := s.Domain(chart.Domain{}.Name())
	iamLog := s.Domain(iamdomain.Domain{}.Name())
	logs := []*runningDomain{trackerLog, pagesLog, chartLog, iamLog}

	writeView := func(opID, id string) tracker.WriteResult {
		t.Helper()
		prior, err := n.writer.ViewPrior(t.Context(), id)
		if err != nil {
			t.Fatalf("read what saving view %s replaces: %v", id, err)
		}
		res, err := n.writer.WriteView(t.Context(), opID, evictedView(id), prior)
		if err != nil {
			t.Fatalf("write view %s: %v", id, err)
		}
		return res
	}
	createUnit := func(key string) chart.WriteResult {
		t.Helper()
		res, err := c.chartWriter.WriteBatch(t.Context(),
			statelog.NewOpID(time.Now(), "unit-"+key), chart.Batch{
				Operations: []chart.Operation{{Kind: chart.OpCreateUnit,
					Object: chart.ObjectRef{Kind: chart.KindUnit, ID: key}}},
			})
		if err != nil {
			t.Fatalf("create unit %s: %v", key, err)
		}
		return res
	}
	invalidate := func(reason string) statelog.Result {
		t.Helper()
		res, err := c.iamWriter.InvalidateAll(t.Context(),
			statelog.NewOpID(time.Now(), "invalidate-"+reason), reason)
		if err != nil {
			t.Fatalf("invalidate every session (%s): %v", reason, err)
		}
		return res
	}

	// THE CONTROL: the same writes, made before any eviction, apply.
	// Without it a harness that applied nothing of this node's would pass
	// the case below.
	writeView("op-view-before", "v-before")
	if _, _, err := n.pages.EnsureContainer(t.Context(), testActivation, "BEFORE",
		"Before", ""); err != nil {
		t.Fatalf("the control container write: %v", err)
	}
	createUnit("before")
	invalidate("before")
	for _, running := range logs {
		waitApplied(t, running)
	}
	requireRow(t, back, "tracker_views", "id", "v-before", true)
	requireRow(t, back, "pages_containers", "key", "BEFORE", true)
	requireRow(t, back, "chart_units", "key", "before", true)
	if got := sessionGeneration(t, back); got != 1 {
		t.Fatalf("the control invalidation left the session generation at %d, want 1", got)
	}

	// THE NODE STOPS APPLYING, and the fleet evicts it on every log.
	s.haltAppliers()
	appendRecord(t, trackerLog, tracker.EvictionSubject(self).String(), trackerEviction(t, self))
	appendRecord(t, pagesLog, pages.EvictionSubject(self).String(), pagesEviction(t, self))
	appendRecord(t, chartLog, chart.EvictionSubject(self).String(), chartEviction(t, self))
	appendRecord(t, iamLog, iamdomain.EvictionSubject(self).String(), iamEviction(t, self))

	// ITS OWN WRITES, THROUGH ITS REAL WRITERS: its fence reads its own
	// applied rows, which do not hold the eviction yet, so every one lands.
	// None can resolve — the appliers are halted — so each answers pending,
	// which is the honest answer and the one asserted.
	res := writeView("op-view-evicted", "v-evicted")
	if res.Outcome != statelog.OutcomePending {
		t.Fatalf("the evicted node's view write answered %q, want pending — it "+
			"has to LAND for the gate to have anything to drop", res.Outcome)
	}
	viewAt := res.Position
	if _, _, err := n.pages.EnsureContainer(t.Context(), testActivation, "EVICTED",
		"Evicted", ""); err != nil {
		t.Fatalf("the evicted node's container write: %v", err)
	}
	_, pageSeq, err := pagesLog.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("read the pages log's end: %v", err)
	}
	pageAt := pagesLog.runner.Committed().At(pageSeq)
	unit := createUnit("evicted")
	if unit.Outcome != statelog.OutcomePending {
		t.Fatalf("the evicted node's unit write answered %q, want pending", unit.Outcome)
	}
	invalidated := invalidate("evicted")
	if invalidated.Outcome != statelog.OutcomePending {
		t.Fatalf("the evicted node's invalidation answered %q, want pending",
			invalidated.Outcome)
	}

	// EVERY RECORD NAMES THIS NODE — the one fact the gate reads.
	requireWriter(t, trackerLog, viewAt.Seq, self)
	requireWriter(t, pagesLog, pageSeq, self)
	requireWriter(t, chartLog, unit.Position.Seq, self)
	requireWriter(t, iamLog, invalidated.Position.Seq, self)

	relaunch(t, s)
	for _, running := range logs {
		waitApplied(t, running)
	}

	// THE EVICTIONS APPLIED — which is what says the peer's records above
	// were ones these appliers read, and the drops below are the gate's.
	for _, table := range []string{"tracker_evictions", "pages_evictions",
		"chart_evictions", "iam_evictions"} {
		requireRow(t, back, table, "node_id", self, true)
	}
	requireRow(t, back, "tracker_views", "id", "v-evicted", false)
	requireRow(t, back, "pages_containers", "key", "EVICTED", false)
	requireRow(t, back, "chart_units", "key", "evicted", false)
	if got := sessionGeneration(t, back); got != 1 {
		t.Fatalf("the session generation is %d after the evicted node's "+
			"invalidation, want the 1 the control left — its record applied", got)
	}

	// AND THE PUBLISHER'S OWN READING OF THE GATE AGREES, so a write
	// resolved later is told `evicted` rather than "somebody else won".
	for _, c := range []struct {
		name  string
		gates statelog.Gates
		subj  statelog.Subject
		opID  string
		at    statelog.Position
	}{
		{"tracker", tracker.NewGates(back.Store),
			statelog.Subject{Kind: string(tracker.KindView), ID: "v-evicted"},
			"op-view-evicted", viewAt},
		{"pages", pages.NewGates(back.Store),
			statelog.Subject{Kind: string(pages.KindContainer), ID: "EVICTED"}, "", pageAt},
		{"chart", chart.NewGates(back.Store),
			statelog.Subject{Kind: string(chart.KindTree)}, unit.OpID, unit.Position},
		{"iam", iamdomain.NewGates(back.Store),
			statelog.Subject{Kind: string(iamdomain.KindInvalidation)},
			invalidated.OpID, invalidated.Position},
	} {
		reason, gated, err := c.gates.GatedAt(t.Context(), c.subj, self, c.opID, c.at, nil)
		if err != nil || !gated || reason != statelog.ReasonEvicted {
			t.Errorf("the %s gate reads (%q, %v, %v) for the evicted node's "+
				"record, want evicted", c.name, reason, gated, err)
		}
	}
}

// sessionGeneration is the identity estate's fleet-wide session generation,
// which every invalidation moves by one.
func sessionGeneration(t *testing.T, back *Backends) int64 {
	t.Helper()
	var generation int64
	err := back.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT generation FROM iam_session_generation WHERE singleton = 0`).
			Scan(&generation)
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read the session generation: %v", err)
	}
	return generation
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
			V: tracker.GateRecordVersion, OpID: "op-evict-" + node,
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
			V: pages.GateRecordVersion, OpID: "op-evict-" + node,
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

// chartEviction is the same for the org chart's log.
func chartEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(chart.Eviction{
		V: chart.GateRecordVersion, NodeID: node, By: "operator",
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := chart.Encode(chart.MutationRecord{
		RecordEnvelope: chart.RecordEnvelope{
			V: chart.GateRecordVersion, OpID: "op-evict-" + node,
			Subject: chart.EvictionSubject(node), Op: chart.OpEviction,
			Writer: "node-peer", CreatedAt: time.Now().UTC(),
			Scope: chart.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "node-peer", ActorKind: chart.AuthorOperator,
	})
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// iamEviction is the same for the identity estate's log.
func iamEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := iamdomain.EncodeEviction(iamdomain.Eviction{
		V: iamdomain.GateRecordVersion, By: "operator",
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.GateRecordVersion, OpID: "op-evict-" + node,
			Subject: iamdomain.EvictionSubject(node), Op: iamdomain.OpEviction,
			Writer: "node-peer", CreatedAt: time.Now().UTC(),
			Scope: iamdomain.RootScope(),
		},
		Mutation: body, Actor: "node-peer", ActorKind: iam.KindMachine,
	})
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// appendRecord puts one encoded record on a domain's log at its own subject,
// SIGNED as every record on the log is — an applier refuses a frame this
// deployment's keyring does not open, so an unsigned one would be a record it
// never applies rather than the peer's record the case means.
func appendRecord(t *testing.T, running *runningDomain, subject string, payload []byte) uint64 {
	t.Helper()
	seq, _, err := running.log.Append(t.Context(),
		running.domain.Stream().SubjectPrefix+"."+subject, "", nil,
		running.signer.Seal(payload))
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
//
// THROUGH THE DOMAIN'S VERIFIER, because the log holds the signed frame and
// the envelope is inside it.
func requireWriter(t *testing.T, running *runningDomain, seq uint64, writer string) {
	t.Helper()
	_, payload, _, ok, err := statelog.VerifiedLog{Log: running.log,
		Verifier: running.verifier}.At(t.Context(), seq)
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
