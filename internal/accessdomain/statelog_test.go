package accessdomain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ACCESS DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, from its
// declaration through its envelope, determinism, idempotency, stamp, evictions
// and node gates, as every other domain is.
func TestTheAccessDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  accessdomain.Domain{},
			Applier: accessdomain.NewApplier(),
			// Migrate is nil: the tables ship in the replicated estate's own
			// migrations (0151, and 0161 for the verdicts).
			Encode:        encodeSuiteRecord,
			Fields:        accessdomain.VersionedFields(),
			Carrying:      carryingSuiteRecord,
			Kinds:         kindNames(),
			Rows:          accessdomain.NewRows,
			Write:         suiteWrite,
			EncodeGate:    encodeSuiteGate,
			EncodeBarrier: accessdomain.EncodeBarrier,
			EncodeTrimmed: accessdomain.EncodeTrimmed,
		}
	})
}

// THE ACCESS GATE READER KEEPS THE RULE EVERY READER SHARES, on the one gate
// this log has: a writer's eviction window, half-open at both ends.
func TestTheAccessGateReaderKeepsTheEvictionWindow(t *testing.T) {
	t.Parallel()
	statelogtest.RunEvictionGates(t, func(t *testing.T) statelogtest.EvictionGateCandidate {
		return statelogtest.EvictionGateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  accessdomain.Domain{},
				Applier: accessdomain.NewApplier(),
				Encode:  encodeSuiteRecord,
				Kinds:   kindNames(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return accessdomain.NewGates(db) },
			Kind:         string(accessdomain.KindTold),
			SubjectKinds: kindNames(),
			Create: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(accessdomain.ToldSubject(id, suiteTurn(id)), writer, opID,
					accessdomain.OpTold, accessdomain.ScopeSet{Subject: true}, toldOf(id, suiteTurn(id)))
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				turn := suiteTurn("again/" + id)
				return rawRecord(accessdomain.ToldSubject(id, turn), writer, opID,
					accessdomain.OpTold, accessdomain.ScopeSet{Subject: true}, toldOf(id, turn))
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(accessdomain.EvictionSubject(node), writer, opID,
					accessdomain.OpEviction, accessdomain.ScopeSet{Subject: true}, evictionOf(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(accessdomain.EvictionSubject(node), writer, opID,
					accessdomain.OpEviction, accessdomain.ScopeSet{Subject: true}, evictionOf(node, true))
			},
			// THE EVICTION IS THE ONLY GATE, said with an empty map.
			ApplyGates: map[string]func(id, writer, opID string) ([]byte, error){},
		}
	})
}

// kindNames is every kind the domain writes, from the enum.
func kindNames() []string {
	out := make([]string, 0, len(accessdomain.ObjectKinds))
	for _, k := range accessdomain.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteStamped is the instant every suite record is stamped at.
var suiteStamped = time.Unix(1_700_000_000, 0).UTC()

// suiteTurn is the turn a suite record about id is about.
func suiteTurn(id string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("turn/"+id)).String()
}

// toldOf is a baseline told to id's turn.
func toldOf(id, turn string) accessdomain.Told {
	return accessdomain.Told{V: 1, Handle: id, Turn: turn, Digest: "sha256:" + string(make64('a')),
		Snapshot: json.RawMessage(`{"v":1}`), Baseline: true, RevisionTo: "rev-1"}
}

// make64 is 64 of one character.
func make64(c byte) []byte {
	out := make([]byte, 64)
	for i := range out {
		out[i] = c
	}
	return out
}

// encodeSuiteRecord builds one valid record of each kind about id at any
// version. The first three kinds are a real sequence: a told record writes a
// row, its seat's removal forgets it, and a node's eviction writes the gate.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject, op, scope, payload := suitePayload(accessdomain.ObjectKind(kind), id)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return accessdomain.Encode(accessdomain.MutationRecord{
		RecordEnvelope: accessdomain.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node", Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorSystem,
	})
}

// suitePayload is one valid (subject, op, scope, payload) per kind about id.
func suitePayload(kind accessdomain.ObjectKind, id string) (
	accessdomain.Subject, accessdomain.OpKind, accessdomain.ScopeSet, any) {

	sentinel := accessdomain.ScopeSet{Subject: true}
	seat := accessdomain.ScopeSet{Terms: []accessdomain.ScopeTerm{{Kind: accessdomain.TermSeat, Seat: id}}}
	switch kind {
	case accessdomain.KindTold:
		return accessdomain.ToldSubject(id, suiteTurn(id)), accessdomain.OpTold, sentinel,
			toldOf(id, suiteTurn(id))
	case accessdomain.KindSeat:
		return accessdomain.SeatSubject(id), accessdomain.OpForget, seat,
			accessdomain.Forget{V: 1, Handle: id}
	case accessdomain.KindEviction:
		return accessdomain.EvictionSubject(id), accessdomain.OpEviction, sentinel, evictionOf(id, false)
	case accessdomain.KindCertificate:
		return accessdomain.CertificateSubject(id, 9), accessdomain.OpIssued, sentinel,
			accessdomain.Certificate{V: 1, Serial: 9, KeyID: "crewlet/" + id, Handle: id,
				Purpose: accessdomain.PurposeTest, HostIDs: []string{"api-1"},
				Principals: []string{"crewlet.api-1.read"}, ValidAfter: suiteStamped,
				ValidBefore: suiteStamped.Add(2 * time.Minute), PublicKeySHA256: "SHA256:x",
				CASHA256: "SHA256:y", Node: "suite-node"}
	case accessdomain.KindCommand:
		command := suiteTurn("command/" + id)
		return accessdomain.CommandSubject(id, command), accessdomain.OpRan, sentinel,
			accessdomain.Command{V: 1, ID: command, Handle: id, Host: "api-1", User: "crewlet-read",
				Level: accessdomain.LevelRead, CommandSealed: "sealed", ContentKey: "k1", Ran: true}
	case accessdomain.KindVerdict:
		request := suiteTurn("request/" + id)
		return accessdomain.VerdictSubject(id, request, "reviewer"), accessdomain.OpVerdict, sentinel,
			verdictOf(id, request, "reviewer")
	case accessdomain.KindExpiry:
		return accessdomain.ExpirySubject(suiteStamped), accessdomain.OpExpire,
			accessdomain.ScopeSet{Terms: []accessdomain.ScopeTerm{{Kind: accessdomain.TermDomain}}},
			accessdomain.Expire{V: 1, Through: suiteStamped}
	case accessdomain.KindGeneration:
		return accessdomain.GenerationSubject(2), accessdomain.OpGeneration, sentinel,
			accessdomain.Generation{V: 1, Generation: 2, By: "suite", StreamAt: suiteStamped}
	case accessdomain.KindBarrier:
		return accessdomain.Subject{Kind: kind, ID: id}, accessdomain.OpBarrier, sentinel, nil
	}
	return accessdomain.Subject{Kind: kind, ID: id}, accessdomain.OpForget, sentinel,
		accessdomain.Forget{V: 1, Handle: id}
}

// verdictOf is a delegate's allow on request of seat id, as a peer publishes it.
func verdictOf(id, request, by string) accessdomain.Verdict {
	return accessdomain.Verdict{V: 1, ID: accessdomain.VerdictID(request, by), Handle: id,
		Request: request, Kind: delegated.KindSSHOperate, Rule: 1, By: by,
		ByKind: accessdomain.VerdictByAgent, Through: accessdomain.VerdictThroughDelegation,
		Asked: delegated.Allow, Confidence: delegated.High,
		Verdict: delegated.Allow, Final: true, Digest: string(make64('c')), Host: "api-1",
		User: "crewlet-ops", SubjectSealed: "sealed", WordsSealed: "sealed", ContentKey: "k1"}
}

// carryingSuiteRecord is a record carrying exactly one versioned field, its
// version left to the encoder: the suite holds the stamp to the field's own.
func carryingSuiteRecord(field statelog.VersionedField) ([]byte, error) {
	const id = "carrying"
	switch field.Name {
	case "Subject.Kind verdict":
		request := suiteTurn("request/" + id)
		return rawRecord(accessdomain.VerdictSubject(id, request, "reviewer"), "suite-node",
			"suite-carrying-verdict", accessdomain.OpVerdict, accessdomain.ScopeSet{Subject: true},
			verdictOf(id, request, "reviewer"))
	}
	return nil, fmt.Errorf("no suite record carries %s: add one here with its row", field.Name)
}

// encodeSuiteGate is a node's eviction or readmission as a peer publishes it.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return rawRecord(accessdomain.EvictionSubject(node), "suite-peer", opID,
		accessdomain.OpEviction, accessdomain.ScopeSet{Subject: true}, evictionOf(node, readmit))
}

// evictionOf is node's eviction, or its readmission.
func evictionOf(node string, readmit bool) accessdomain.Eviction {
	return accessdomain.Eviction{V: accessdomain.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteStamped, Readmitted: readmit}
}

// rawRecord is one record as a writer stamps it.
func rawRecord(subject accessdomain.Subject, writer, opID string, op accessdomain.OpKind,
	scope accessdomain.ScopeSet, payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return accessdomain.Encode(accessdomain.MutationRecord{
		RecordEnvelope: accessdomain.RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: suiteStamped, Gen: 1,
			Writer: writer, Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorSystem,
	})
}

// suiteWrite is one write through the domain's own production writer: a told
// record, which needs nothing before it.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": key}})
	if err != nil {
		return err
	}
	sealer, err := accessdomain.NewSealer(cipher)
	if err != nil {
		return err
	}
	w, err := accessdomain.NewWriter(accessdomain.WriterDeps{Publisher: pub, DB: db, Sealer: sealer})
	if err != nil {
		return err
	}
	_, err = w.Tell(ctx, accessdomain.Call{
		By: iam.Actor{Name: "suite", Kind: iam.ActorSystem}, OpID: statelog.NewOpID(time.Now(), "suite"),
	}, accessdomain.NewTold{Handle: "suite-seat", Turn: uuid.NewString(),
		Digest: "sha256:" + string(make64('b')), Snapshot: json.RawMessage(`{"v":1}`), Baseline: true})
	return err
}
