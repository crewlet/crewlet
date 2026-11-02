package assistantdomain_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ASSISTANT DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, from its
// declaration through its envelope, determinism, idempotency, versioned fields,
// stamp, evictions and node gates, exactly as every other domain is.
func TestTheAssistantDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  assistantdomain.Domain{},
			Applier: assistantdomain.NewApplier(),
			// Migrate is nil: these tables ship in the replicated estate's
			// own migration (0048), so a fresh store already has them.
			Encode: encodeSuiteRecord,
			// THE VERSIONED-FIELD TABLE is empty: every field this domain
			// writes is in the base format.
			Fields:     assistantdomain.VersionedFields(),
			Kinds:      kindNames(),
			Rows:       assistantdomain.NewRows,
			Write:      suiteWrite,
			EncodeGate: encodeSuiteGate,
			// THE PRODUCTION ENCODER of the read index's barrier, which the
			// suite holds to writing no row.
			EncodeBarrier: assistantdomain.EncodeBarrier,
			// THE PRODUCTION ENCODER the engine hands the trim (ADR-0073).
			EncodeTrimmed: assistantdomain.EncodeTrimmed,
		}
	})
}

// THE ASSISTANT GATE READER KEEPS THE RULE EVERY READER SHARES: the deletion
// marker first, over the subjects the applier's own marker covers (a
// conversation's and its turns' and proposals'), the purge's own record exempt,
// then the writer's eviction window.
func TestTheAssistantGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  assistantdomain.Domain{},
				Applier: assistantdomain.NewApplier(),
				Encode:  encodeSuiteRecord,
				Kinds:   kindNames(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return assistantdomain.NewGates(db) },
			Kind:         string(assistantdomain.KindConversation),
			SubjectKinds: kindNames(),
			Create: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(assistantdomain.KindConversation, id, writer, opID,
					assistantdomain.OpConversationCreated,
					assistantdomain.ConversationCreated{V: 1, Owner: suiteOwner})
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(assistantdomain.KindConversation, id, writer, opID,
					assistantdomain.OpConversationTitled,
					assistantdomain.ConversationTitled{V: 1, TitleSealed: "renamed by " + opID})
			},
			Purge: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(assistantdomain.KindConversation, id, writer, opID,
					assistantdomain.OpConversationPurged,
					assistantdomain.ConversationPurged{V: 1, Reason: assistantdomain.PurgeCleared})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(assistantdomain.KindEviction, node, writer, opID,
					assistantdomain.OpEviction, evictionOf(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(assistantdomain.KindEviction, node, writer, opID,
					assistantdomain.OpEviction, evictionOf(node, true))
			},
		}
	})
}

// kindNames is every kind the domain writes, from the enum.
func kindNames() []string {
	out := make([]string, 0, len(assistantdomain.ObjectKinds))
	for _, k := range assistantdomain.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteOwner is the owner every suite record is about. The applier reads ids
// as opaque keys, so the suite's plain ids are used as given: the gate family
// asks the reader about the very ids it handed out.
const suiteOwner = "suite-owner"

// suiteStamped is the instant every suite record is stamped at.
var suiteStamped = time.Unix(1_700_000_000, 0).UTC()

// encodeSuiteRecord builds one valid record of each kind about id at any
// version.
//
// THE FIRST THREE KINDS ARE A REAL SEQUENCE: a conversation's create, a
// message recorded on a turn of it, and a proposal made in it.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject, op, payload := suitePayload(assistantdomain.ObjectKind(kind), id)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return assistantdomain.Encode(assistantdomain.MutationRecord{
		RecordEnvelope: assistantdomain.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node",
			Scope: suiteScope(subject),
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorHuman, OperatorID: "suite",
	})
}

// suiteScope is a suite record's scope: the subject alone, or the domain for an
// erasure.
func suiteScope(subject assistantdomain.Subject) assistantdomain.ScopeSet {
	if subject.Kind == assistantdomain.KindOwner {
		return assistantdomain.ScopeSet{Terms: []assistantdomain.ScopeTerm{
			{Kind: assistantdomain.TermDomain}}}
	}
	return assistantdomain.ScopeSet{Subject: true}
}

// suitePayload is one valid (subject, op, payload) per kind about id.
func suitePayload(kind assistantdomain.ObjectKind, id string) (
	assistantdomain.Subject, assistantdomain.OpKind, any) {

	derived := func(what string) string {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte(what+"/"+id)).String()
	}
	switch kind {
	case assistantdomain.KindConversation:
		return assistantdomain.Subject{Kind: kind, ID: id}, assistantdomain.OpConversationCreated,
			assistantdomain.ConversationCreated{V: 1, Owner: suiteOwner, TitleSealed: "sealed-title"}
	case assistantdomain.KindTurn:
		return assistantdomain.Subject{Kind: kind, ID: id + "." + derived("turn")},
			assistantdomain.OpMessageRecorded, assistantdomain.MessageRecorded{
				V: 1, Message: derived("message"), Role: wire.RoleUser, TextSealed: "sealed-text",
			}
	case assistantdomain.KindProposal:
		return assistantdomain.Subject{Kind: kind, ID: id + "." + derived("proposal")},
			assistantdomain.OpProposalCreated, assistantdomain.ProposalCreated{
				V: 1, Turn: derived("turn"), Owner: suiteOwner, Tool: "create_task",
				Scope: wire.ScopeServer, Method: "POST", Path: "/operator/act/create_task",
				RequestDigest: hex64("a"), Digest: "sha256:" + hex64("d"),
				OpKey: "suite-key", CardSealed: "sealed-card", ExpiresAt: suiteStamped,
			}
	case assistantdomain.KindOwner:
		return assistantdomain.Subject{Kind: kind, ID: id}, assistantdomain.OpOwnerErased,
			assistantdomain.OwnerErased{V: 1, Owner: id}
	case assistantdomain.KindEviction:
		return assistantdomain.EvictionSubject(id), assistantdomain.OpEviction, evictionOf(id, false)
	case assistantdomain.KindGeneration:
		return assistantdomain.GenerationSubject(2), assistantdomain.OpGeneration,
			assistantdomain.Generation{V: 1, Generation: 2, By: "suite", StreamAt: suiteStamped}
	}
	return assistantdomain.Subject{Kind: kind, ID: id}, assistantdomain.OpConversationTitled,
		assistantdomain.ConversationTitled{V: 1}
}

// evictionOf is node's eviction, or its readmission.
func evictionOf(node string, readmit bool) assistantdomain.Eviction {
	return assistantdomain.Eviction{
		V: assistantdomain.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteStamped, Readmitted: readmit,
	}
}

// rawRecord is one record the gate family applies, written by writer.
func rawRecord(kind assistantdomain.ObjectKind, id, writer, opID string,
	op assistantdomain.OpKind, payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return assistantdomain.Encode(assistantdomain.MutationRecord{
		RecordEnvelope: assistantdomain.RecordEnvelope{
			V: assistantdomain.RecordVersion, OpID: opID,
			Subject: assistantdomain.Subject{Kind: kind, ID: id}, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: writer,
			Scope: assistantdomain.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorHuman,
	})
}

// encodeSuiteGate is the eviction record a peer publishes onto this log.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return rawRecord(assistantdomain.KindEviction, node, "suite-peer", opID,
		assistantdomain.OpEviction, evictionOf(node, readmit))
}

// suiteWrite is one write through the domain's own production writer, which is
// where the framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	sealer, err := assistantdomain.NewSealer(staticCipher())
	if err != nil {
		return err
	}
	w, err := assistantdomain.NewWriter(assistantdomain.WriterDeps{
		Publisher: pub, DB: db, Sealer: sealer,
	})
	if err != nil {
		return err
	}
	_, err = w.CreateConversation(ctx, assistantdomain.Call{
		Owner: uuid.New(), By: iam.Actor{Name: "suite", Kind: iam.ActorHuman},
		OpID: statelog.NewOpID(time.Now(), "suite"),
	}, uuid.New(), "Suite")
	return err
}
