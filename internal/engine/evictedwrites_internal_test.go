package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN EVICTED NODE'S OWN WRITES, MADE THROUGH ITS REAL WRITERS, ARE DROPPED BY
// THE APPLIER — on every identity-claiming log, on real streams.
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
// yet noticed its own eviction is. Its own writes then go through the writer it
// serves every seat with, land above the eviction, and the appliers, once
// running, must drop them.
//
// EVERY LOG THE NODE RUNS THAT CLAIMS IDENTITY, read off the running set: a
// domain that claims identity and has no write here fails the case, so a log
// added to the register is held to the gate the day it is added rather than
// certified by a list of the two that came first.
func TestAnEvictedNodesRealWritesAreDroppedByTheApplier(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	s := e.native.Load().log
	self := s.nodeID
	// THE HEARTBEAT IS WATCHED RATHER THAN OBEYED: halted appliers look
	// like a node below the log, and an adoption racing the writes below
	// would be what they measured.
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { return nil }
	s.rejoinMu.Unlock()

	var logs []*runningLog
	for _, running := range s.running() {
		if !running.domain.ClaimsIdentity() {
			continue
		}
		if _, written := evictedWriters[running.domain.Name()]; !written {
			t.Fatalf("%s claims identity and this case holds no write for it, so its "+
				"eviction gate is certified by nothing: add its row to evictedWriters",
				running.domain.Name())
		}
		logs = append(logs, running)
	}

	// THE CONTROL: the same writes, made before any eviction, apply.
	// Without it a harness that applied nothing of this node's would pass
	// the case below.
	for _, running := range logs {
		w := evictedWriters[running.domain.Name()]
		if _, _, err := w.write(t.Context(), e, "before"); err != nil {
			t.Fatalf("the control write on %s: %v", running.domain.Name(), err)
		}
		waitApplied(t, running)
		table, column := w.row()
		requireRow(t, back, table, column, w.key("before"), true)
	}

	// THE NODE STOPS APPLYING, and the fleet evicts it on every log.
	s.haltAppliers()
	for _, running := range logs {
		w := evictedWriters[running.domain.Name()]
		appendRecord(t, running, w.evictionSubject(self), w.eviction(t, self))
	}

	// ITS OWN WRITES, THROUGH ITS REAL WRITERS: its fence reads its own
	// applied rows, which do not hold the eviction yet, so each lands.
	// None can resolve — the appliers are halted — so a writer that reports
	// an outcome answers pending, which is the honest answer and the one
	// asserted, after [haltedWriteBudget] rather than the publisher's whole
	// default.
	halted := statelog.WithResolveBudget(t.Context(), haltedWriteBudget)
	landed := map[string]statelog.Position{}
	for _, running := range logs {
		name := running.domain.Name()
		at, outcome, err := evictedWriters[name].write(halted, e, "evicted")
		if err != nil || (outcome != "" && outcome != statelog.OutcomePending) {
			t.Fatalf("the evicted node's %s write answered %q (err %v), want "+
				"pending — it has to LAND for the gate to have anything to drop",
				name, outcome, err)
		}
		landed[name] = at
		// THE RECORD NAMES THIS NODE — the one fact the gate reads.
		requireWriter(t, running, at.Seq, self)
	}

	relaunch(t, s)
	for _, running := range logs {
		name := running.domain.Name()
		w := evictedWriters[name]
		waitApplied(t, running)
		table, column := w.row()
		requireRow(t, back, table, column, w.key("evicted"), false)

		// AND THE PUBLISHER'S OWN READING OF THE GATE AGREES, so a write
		// resolved later is told `evicted` rather than "somebody else won".
		at := landed[name]
		env := envelopeAt(t, running, at.Seq)
		reason, gated, err := w.gates(back.Store.Replicated().Reader()).GatedAt(t.Context(),
			env.Subject, self, env.OpID, at)
		if err != nil || !gated || reason != statelog.ReasonEvicted {
			t.Fatalf("%s's gate reads (%q, %v, %v) for the evicted node's record, "+
				"want evicted", name, reason, gated, err)
		}
	}
}

// evictedWriter is one identity-claiming log as the case above drives it: a
// real write of this node's, the row it leaves, the record a peer evicts the
// node with, and the log's own reading of its gates.
type evictedWriter struct {
	// write makes one write through the node's own writer on ctx, named by
	// tag, and answers the position it landed at and the outcome the writer
	// reported — empty for a writer that reports none.
	write func(ctx context.Context, e *Engine, tag string) (statelog.Position, statelog.Outcome, error)

	// key is the row a write named tag leaves, and row the table and the
	// column it is found by.
	key func(tag string) string
	row func() (table, column string)

	// evictionSubject and eviction are a peer's eviction of node.
	evictionSubject func(node string) string
	eviction        func(t *testing.T, node string) []byte

	// gates is the log's gate reader over the replicated estate.
	gates func(db store.ReplicatedReader) statelog.Gates
}

// evictedWriters is every identity-claiming domain's row for the case above.
var evictedWriters = map[string]evictedWriter{
	tracker.Domain{}.Name(): {
		write: func(ctx context.Context, e *Engine, tag string) (statelog.Position, statelog.Outcome, error) {
			res, err := e.native.Load().writer.WriteView(ctx, "op-view-"+tag,
				evictedView("v-"+tag))
			return res.Position, res.Outcome, err
		},
		key:             func(tag string) string { return "v-" + tag },
		row:             func() (string, string) { return "tracker_views", "id" },
		evictionSubject: func(node string) string { return tracker.EvictionSubject(node).String() },
		eviction:        trackerEviction,
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return tracker.NewGates(db)
		},
	},
	pages.Domain{}.Name(): {
		write: func(ctx context.Context, e *Engine, tag string) (statelog.Position, statelog.Outcome, error) {
			// THE CHART'S CONTAINER APPLY, which answers no outcome: the
			// record it landed is the log's last.
			if _, _, err := e.native.Load().pages.EnsureContainer(ctx,
				testActivation, strings.ToUpper(tag), tag, ""); err != nil {
				return statelog.Position{}, "", err
			}
			running := e.native.Load().log.Domain(pages.Domain{}.Name())
			_, last, err := running.log.Bounds(ctx)
			return running.runner.Committed().At(last), "", err
		},
		key:             strings.ToUpper,
		row:             func() (string, string) { return "pages_containers", "key" },
		evictionSubject: func(node string) string { return pages.EvictionSubject(node).String() },
		eviction:        pagesEviction,
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return pages.NewGates(db)
		},
	},
	chat.Domain{}.Name(): {
		write: func(ctx context.Context, e *Engine, tag string) (statelog.Position, statelog.Outcome, error) {
			// THE COMPANY'S OWN SEAT, as a seat's tool would make it.
			written, err := e.native.Load().chat.CreateChannel(ctx,
				chat.Actor{Handle: "ceo", Kind: chat.AuthorAgent},
				chat.NewChannel{Name: "room-" + tag, Kind: chat.KindPublic})
			return written.Outcome.Position, written.Outcome.Outcome, err
		},
		key:             func(tag string) string { return "room-" + tag },
		row:             func() (string, string) { return "chat_channels", "name" },
		evictionSubject: func(node string) string { return chat.EvictionSubject(node).String() },
		eviction:        chatEviction,
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return chat.NewGates(db)
		},
	},
}

// envelopeAt is the envelope of the record at seq on a domain's log.
func envelopeAt(t *testing.T, running *runningLog, seq uint64) statelog.Envelope {
	t.Helper()
	_, payload, _, ok, err := running.log.At(t.Context(), seq)
	if err != nil || !ok {
		t.Fatalf("read %s at %d: ok %v, err %v", running.domain.Name(), seq, ok, err)
	}
	env, err := running.domain.Envelope(payload)
	if err != nil {
		t.Fatalf("decode %s at %d: %v", running.domain.Name(), seq, err)
	}
	return env
}

// haltedWriteBudget is the resolution budget a case hands the writes it makes
// while one of this node's appliers is halted ([statelog.WithResolveBudget]),
// and those writes alone: every other write keeps the publisher's default.
//
// ONE SECOND. A write on a halted applier answers pending whatever the budget,
// so its length is simply spent — the default's five seconds per write was
// most of these cases' run time. But one gesture writes every identity log at
// once, and a log whose applier is RUNNING must still resolve applied inside
// the same budget, which a running applier does in milliseconds and a loaded
// race runner in well under a second.
const haltedWriteBudget = time.Second

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

// chatEviction is the same for chat's log.
func chatEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(chat.Eviction{
		V: chat.GateRecordVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := chat.Encode(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.GateRecordVersion, OpID: "op-evict-" + node,
			Subject: chat.EvictionSubject(node), Op: chat.OpEviction,
			Writer: "node-peer", Scope: chat.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "operator", ActorKind: chat.AuthorOperator,
		OperatorID: "operator",
	})
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// appendRecord puts one encoded record on a domain's log at its own subject.
func appendRecord(t *testing.T, running *runningLog, subject string, payload []byte) uint64 {
	t.Helper()
	seq, _, err := running.log.Append(t.Context(),
		running.spec.SubjectPrefix+"."+subject, "", nil, payload)
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
	for _, running := range s.running() {
		name := running.domain.Name()
		if err := running.consumer.Reset(t.Context(), running.runner.Committed().Seq); err != nil {
			t.Fatalf("move %s's consumer to its checkpoint: %v", name, err)
		}
	}
	s.launchAppliers(s.run)
}

// waitApplied waits for a domain's applier to commit its whole log.
func waitApplied(t *testing.T, running *runningLog) {
	t.Helper()
	_, last, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("read %s's end: %v", running.domain.Name(), err)
	}
	waitUntil(t, 20*time.Second, running.domain.Name()+" to apply its whole log",
		func() bool { return running.runner.Committed().Seq >= last })
}

// requireWriter asserts the record at seq names writer.
func requireWriter(t *testing.T, running *runningLog, seq uint64, writer string) {
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
