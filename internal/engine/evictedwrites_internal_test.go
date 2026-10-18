package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
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
// AND THE IDENTITY ESTATE'S WRITER WAS THE SAME: until its writes were
// stamped, an identity record carried no writer at all, so an evicted node's
// session invalidations applied on every data node while its tracker writes
// were dropped.
//
// So the evictions here land on each log first — as a peer would publish them
// — while this node's appliers are halted, which is what a node that has not
// yet noticed its own eviction is. Its own writes then go through the writers
// it serves every seat and person with, land above the eviction, and the
// appliers, once running, must drop them.
//
// EVERY LOG THE NODE RUNS THAT CLAIMS IDENTITY, read off the running set: a
// domain that claims identity and has no write here fails the case, so a log
// added to the register is held to the gate the day it is added rather than
// certified by a list of the ones that came first.
func TestAnEvictedNodesRealWritesAreDroppedByTheApplier(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	s := e.core.Load().log
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
		if _, _, err := w.write(t.Context(), t, e, "before"); err != nil {
			t.Fatalf("the control write on %s: %v", running.domain.Name(), err)
		}
	}
	for _, running := range logs {
		waitApplied(t, running)
		if w := evictedWriters[running.domain.Name()]; !w.applied(t, back, "before") {
			t.Fatalf("the control write on %s left nothing in the estate",
				running.domain.Name())
		}
	}

	// THE NODE STOPS APPLYING, and the fleet evicts it on every log.
	s.haltAppliers()
	for _, running := range logs {
		w := evictedWriters[running.domain.Name()]
		appendRecord(t, running, w.evictionSubject(self), w.eviction(t, self))
	}

	// ITS OWN WRITES, THROUGH ITS REAL WRITERS: its fence reads its own
	// applied rows, which do not hold the eviction yet, so every one lands.
	// None can resolve — the appliers are halted — so a writer that reports
	// an outcome answers pending, which is the honest answer and the one
	// asserted, after [haltedWriteBudget] rather than the publisher's whole
	// default.
	halted := statelog.WithResolveBudget(t.Context(), haltedWriteBudget)
	landed := map[string]statelog.Position{}
	for _, running := range logs {
		name := running.domain.Name()
		at, outcome, err := evictedWriters[name].write(halted, t, e, "evicted")
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

		// THE EVICTION APPLIED — which is what says the peer's record
		// above was one this applier read, and the drop below is the
		// gate's.
		requireRow(t, back, w.evictions, "node_id", self, true)
		if w.applied(t, back, "evicted") {
			t.Errorf("the evicted node's %s write applied", name)
		}

		// AND THE PUBLISHER'S OWN READING OF THE GATE AGREES, so a write
		// resolved later is told `evicted` rather than "somebody else won".
		at := landed[name]
		env := envelopeAt(t, running, at.Seq)
		reason, gated, err := w.gates(back.Store.Replicated().Reader()).GatedAt(t.Context(),
			env.Subject, self, env.OpID, at, nil)
		if err != nil || !gated || reason != statelog.ReasonEvicted {
			t.Errorf("the %s gate reads (%q, %v, %v) for the evicted node's "+
				"record, want evicted", name, reason, gated, err)
		}
	}
}

// evictedWriter is one identity-claiming log as the case above drives it: a
// real write of this node's, what it leaves in the estate, the record a peer
// evicts the node with, and the log's own reading of its gates.
type evictedWriter struct {
	// write makes one write through the node's own writer on ctx, named by
	// tag, and answers the position it landed at and the outcome the writer
	// reported — empty for a writer that reports none.
	write func(ctx context.Context, t *testing.T, e *Engine, tag string) (
		statelog.Position, statelog.Outcome, error)

	// applied reports whether the write named tag is in the replicated
	// estate.
	applied func(t *testing.T, back *Backends, tag string) bool

	// evictionSubject and eviction are a peer's eviction of node, and
	// evictions the table its apply records it in.
	evictionSubject func(node string) string
	eviction        func(t *testing.T, node string) []byte
	evictions       string

	// gates is the log's gate reader over the replicated estate.
	gates func(db store.ReplicatedReader) statelog.Gates
}

// evictedWriters is every identity-claiming domain's row for the case above.
var evictedWriters = map[string]evictedWriter{
	worklet.Domain{}.Name(): {
		write: func(ctx context.Context, t *testing.T, e *Engine, tag string) (
			statelog.Position, statelog.Outcome, error) {
			t.Helper()
			n := e.native.Load()
			id := "v-" + tag
			prior, err := n.writer.ViewPrior(ctx, id)
			if err != nil {
				return statelog.Position{}, "", fmt.Errorf("read what saving view %s replaces: %w", id, err)
			}
			res, err := n.writer.WriteView(ctx, "op-view-"+tag, evictedView(id), prior)
			return res.Position, res.Outcome, err
		},
		applied: func(t *testing.T, back *Backends, tag string) bool {
			return rowExists(t, back, "tracker_views", "id", "v-"+tag)
		},
		evictionSubject: func(node string) string { return worklet.EvictionSubject(node).String() },
		eviction:        trackerEviction,
		evictions:       "tracker_evictions",
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return worklet.NewGates(db)
		},
	},
	doclet.Domain{}.Name(): {
		write: func(ctx context.Context, t *testing.T, e *Engine, tag string) (
			statelog.Position, statelog.Outcome, error) {
			t.Helper()
			// THE CHART'S CONTAINER APPLY, which answers no outcome: the
			// record it landed is the log's last.
			if _, _, err := e.native.Load().pages.EnsureContainer(ctx,
				testActivation, strings.ToUpper(tag), tag, ""); err != nil {
				return statelog.Position{}, "", err
			}
			running := e.core.Load().log.Domain(doclet.Domain{}.Name())
			_, last, err := running.log.Bounds(ctx)
			return running.runner.Committed().At(last), "", err
		},
		applied: func(t *testing.T, back *Backends, tag string) bool {
			return rowExists(t, back, "pages_containers", "key", strings.ToUpper(tag))
		},
		evictionSubject: func(node string) string { return doclet.EvictionSubject(node).String() },
		eviction:        pagesEviction,
		evictions:       "pages_evictions",
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return doclet.NewGates(db)
		},
	},
	iamdomain.Domain{}.Name(): {
		write: func(ctx context.Context, t *testing.T, e *Engine, tag string) (
			statelog.Position, statelog.Outcome, error) {
			t.Helper()
			res, err := e.core.Load().iamWriter.InvalidateAll(ctx,
				statelog.NewOpID(time.Now(), "invalidate-"+tag), tag)
			return res.Position, res.Outcome, err
		},
		// EVERY INVALIDATION MOVES THE ONE GENERATION BY ONE, so the
		// control's is the first and the evicted node's would be the second.
		applied: func(t *testing.T, back *Backends, tag string) bool {
			want := map[string]int64{"before": 1, "evicted": 2}[tag]
			return sessionGeneration(t, back) >= want
		},
		evictionSubject: func(node string) string { return iamdomain.EvictionSubject(node).String() },
		eviction:        iamEviction,
		evictions:       "iam_evictions",
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return iamdomain.NewGates(db)
		},
	},
	chatlet.Domain{}.Name(): {
		write: func(ctx context.Context, t *testing.T, e *Engine, tag string) (
			statelog.Position, statelog.Outcome, error) {
			t.Helper()
			// THE COMPANY'S OWN SEAT, as a seat's tool would make it.
			written, err := e.native.Load().chat.CreateChannel(ctx,
				chatlet.Actor{Handle: "ceo", Kind: chatlet.AuthorAgent},
				chatlet.NewChannel{Name: "room-" + tag, Kind: chatlet.KindPublic})
			return written.Outcome.Position, written.Outcome.Outcome, err
		},
		applied: func(t *testing.T, back *Backends, tag string) bool {
			return rowExists(t, back, "chat_channels", "name", "room-"+tag)
		},
		evictionSubject: func(node string) string { return chatlet.EvictionSubject(node).String() },
		eviction:        chatEviction,
		evictions:       "chat_evictions",
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return chatlet.NewGates(db)
		},
	},
	artifactsdomain.Domain{}.Name(): {
		write: func(ctx context.Context, t *testing.T, e *Engine, tag string) (
			statelog.Position, statelog.Outcome, error) {
			t.Helper()
			// THE NODE GATE'S OWN GESTURE, through the node's artifacts
			// writer: evicting a node nobody runs is the one write that
			// writer makes with no artifact, object or seat behind it, and
			// it leaves a row a case can look for.
			res, err := e.core.Load().artifactsWriter.EvictNode(ctx,
				iam.ActorFor(gateOperator), statelog.NewOpID(time.Now(), "evict-"+tag),
				"node-"+tag)
			return res.Position, res.Outcome, err
		},
		applied: func(t *testing.T, back *Backends, tag string) bool {
			return rowExists(t, back, "artifacts_evictions", "node_id", "node-"+tag)
		},
		evictionSubject: func(node string) string {
			return artifactsdomain.EvictionSubject(node).String()
		},
		eviction:  artifactsEviction,
		evictions: "artifacts_evictions",
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return artifactsdomain.NewGates(db)
		},
	},
	artifactdata.Domain{}.Name(): {
		write: func(ctx context.Context, t *testing.T, e *Engine, tag string) (
			statelog.Position, statelog.Outcome, error) {
			t.Helper()
			// THE SAME GESTURE through the datastore's writer, for the same
			// reason: a datastore write needs an artifact holding one.
			res, err := e.core.Load().artifactDataWriter.EvictNode(ctx,
				iam.ActorFor(gateOperator), statelog.NewOpID(time.Now(), "evict-"+tag),
				"node-"+tag)
			return res.Position, res.Outcome, err
		},
		applied: func(t *testing.T, back *Backends, tag string) bool {
			return rowExists(t, back, "artifactdata_evictions", "node_id", "node-"+tag)
		},
		evictionSubject: func(node string) string {
			return artifactdata.EvictionSubject(node).String()
		},
		eviction:  artifactDataEviction,
		evictions: "artifactdata_evictions",
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return artifactdata.NewGates(db)
		},
	},
	assistantdomain.Domain{}.Name(): {
		write: func(ctx context.Context, t *testing.T, e *Engine, tag string) (
			statelog.Position, statelog.Outcome, error) {
			t.Helper()
			// A PERSON STARTING A CONVERSATION, through the writer the
			// dashboard's assistant records with.
			res, err := createConversation(ctx, e.core.Load().assistantWriter, tag)
			return res.Position, res.Outcome, err
		},
		applied: func(t *testing.T, back *Backends, tag string) bool {
			return rowExists(t, back, "assistant_conversations", "id",
				taggedConversation(tag).String())
		},
		evictionSubject: func(node string) string {
			return assistantdomain.EvictionSubject(node).String()
		},
		eviction:  assistantEviction,
		evictions: "assistant_evictions",
		gates: func(db store.ReplicatedReader) statelog.Gates {
			return assistantdomain.NewGates(db)
		},
	},
}

// envelopeAt is the envelope of the record at seq on a domain's log.
func envelopeAt(t *testing.T, running *runningLog, seq uint64) statelog.Envelope {
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
	return env
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
func evictedView(id string) worklet.View {
	return worklet.View{
		ID: id, Name: "View " + id, Type: worklet.ViewList,
		Container: worklet.Container{Kind: worklet.ContainerWorkspace},
	}
}

// createConversation is a person starting the conversation tag names, through
// the assistant writer w: the one write a person makes there that needs
// nothing written before it, so it lands on a subject nobody has written, and
// the conversation's own row is what it leaves.
func createConversation(ctx context.Context, w *assistantdomain.Writer,
	tag string) (statelog.Result, error) {

	return w.CreateConversation(ctx, assistantdomain.Call{
		Owner: conversationOwner.ID, By: iam.ActorFor(conversationOwner),
		OpID: statelog.NewOpID(time.Now(), "create-"+tag),
	}, taggedConversation(tag), "")
}

// conversationOwner is the person createConversation writes as, signed in as
// themselves.
var conversationOwner = iam.Principal{
	ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("jane.doe")), Login: "jane.doe",
	Kind: iam.KindPerson, Seat: "jane",
}

// taggedConversation is the conversation createConversation starts for tag.
func taggedConversation(tag string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("conversation-"+tag))
}

// trackerEviction is the record a peer's writer publishes to evict node,
// written by that peer.
func trackerEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(worklet.Eviction{
		V: worklet.DocumentVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "op-evict-" + node,
			Subject: worklet.EvictionSubject(node), Op: worklet.OpEviction,
			Writer: "node-peer", Scope: worklet.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "node-peer", ActorKind: worklet.AuthorSystem,
	}.Encode()
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// pagesEviction is the same for the knowledge base's log.
func pagesEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(doclet.Eviction{
		V: doclet.DocumentVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := doclet.Encode(doclet.MutationRecord{
		RecordEnvelope: doclet.RecordEnvelope{
			V: doclet.RecordVersion, OpID: "op-evict-" + node,
			Subject: doclet.EvictionSubject(node), Op: doclet.OpEviction,
			Writer: "node-peer", Scope: doclet.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "node-peer", ActorKind: doclet.AuthorOperator,
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
		V: iamdomain.DocumentVersion, By: "operator",
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.RecordVersion, OpID: "op-evict-" + node,
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

// chatEviction is the same for chat's log.
func chatEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(chatlet.Eviction{
		V: chatlet.GateRecordVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := chatlet.Encode(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.GateRecordVersion, OpID: "op-evict-" + node,
			Subject: chatlet.EvictionSubject(node), Op: chatlet.OpEviction,
			Writer: "node-peer", CreatedAt: time.Now().UTC(),
			Scope: chatlet.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "operator", ActorKind: chatlet.AuthorOperator,
		OperatorID: "operator",
	})
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// artifactsEviction is the same for the artifacts log.
func artifactsEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(artifactsdomain.Eviction{
		V: artifactsdomain.GateRecordVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := artifactsdomain.Encode(artifactsdomain.MutationRecord{
		RecordEnvelope: artifactsdomain.RecordEnvelope{
			V: artifactsdomain.RecordVersion, OpID: "op-evict-" + node,
			Subject: artifactsdomain.EvictionSubject(node), Op: artifactsdomain.OpEviction,
			Writer: "node-peer", CreatedAt: time.Now().UTC(),
			Scope: artifactsdomain.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "operator", ActorKind: iam.ActorOperator,
		OperatorID: "operator",
	})
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// artifactDataEviction is the same for the artifact datastore's log.
func artifactDataEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(artifactdata.Eviction{
		V: artifactdata.GateRecordVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := artifactdata.Encode(artifactdata.MutationRecord{
		RecordEnvelope: artifactdata.RecordEnvelope{
			V: artifactdata.RecordVersion, OpID: "op-evict-" + node,
			Subject: artifactdata.EvictionSubject(node), Op: artifactdata.OpEviction,
			Writer: "node-peer", CreatedAt: time.Now().UTC(),
			Scope: artifactdata.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "operator", ActorKind: iam.ActorOperator,
		OperatorID: "operator",
	})
	if err != nil {
		t.Fatalf("encode the eviction record: %v", err)
	}
	return payload
}

// assistantEviction is the same for the Dashboard Assistant's log, stamped at
// the version every gate it installs carries.
func assistantEviction(t *testing.T, node string) []byte {
	t.Helper()
	body, err := json.Marshal(assistantdomain.Eviction{
		V: assistantdomain.GateRecordVersion, NodeID: node, EvictedBy: "operator",
		EvictedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the eviction: %v", err)
	}
	payload, err := assistantdomain.Encode(assistantdomain.MutationRecord{
		RecordEnvelope: assistantdomain.RecordEnvelope{
			V: assistantdomain.GateRecordVersion, OpID: "op-evict-" + node,
			Subject: assistantdomain.EvictionSubject(node), Op: assistantdomain.OpEviction,
			Writer: "node-peer", CreatedAt: time.Now().UTC(),
			Scope: assistantdomain.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "operator", ActorKind: iam.ActorOperator,
		OperatorID: "operator",
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
func appendRecord(t *testing.T, running *runningLog, subject string, payload []byte) uint64 {
	t.Helper()
	seq, _, err := running.log.Append(t.Context(),
		running.spec.SubjectPrefix+"."+subject, "", nil,
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
//
// THROUGH THE DOMAIN'S VERIFIER, because the log holds the signed frame and
// the envelope is inside it.
func requireWriter(t *testing.T, running *runningLog, seq uint64, writer string) {
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
	if got := rowExists(t, back, table, column, key); got != want {
		t.Fatalf("%s holds a row for %s = %q: %v, want %v", table, column, key, got, want)
	}
}

// rowExists reports whether one row exists in the replicated estate.
func rowExists(t *testing.T, back *Backends, table, column, key string) bool {
	t.Helper()
	var n int
	if err := back.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM `+table+` WHERE `+column+` = ?`, key).Scan(&n)
	}); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read %s: %v", table, err)
	}
	return n > 0
}
