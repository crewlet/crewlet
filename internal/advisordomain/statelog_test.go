package advisordomain_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ADVISOR DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, from its
// declaration through its envelope, determinism, idempotency, stamp, evictions
// and node gates, as every other domain is.
func TestTheAdvisorDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  advisordomain.Domain{},
			Applier: advisordomain.NewApplier(),
			// Migrate is nil: the tables ship in the replicated estate's own
			// migration (0193).
			Encode:        encodeSuiteRecord,
			Kinds:         kindNames(),
			Rows:          advisordomain.NewRows,
			Write:         suiteWrite,
			EncodeGate:    encodeSuiteGate,
			EncodeBarrier: advisordomain.EncodeBarrier,
			EncodeTrimmed: advisordomain.EncodeTrimmed,
		}
	})
}

// THE ADVISOR GATE READER KEEPS THE RULE EVERY READER SHARES, on the one gate
// this log has: a writer's eviction window, half-open at both ends.
func TestTheAdvisorGateReaderKeepsTheEvictionWindow(t *testing.T) {
	t.Parallel()
	statelogtest.RunEvictionGates(t, func(t *testing.T) statelogtest.EvictionGateCandidate {
		return statelogtest.EvictionGateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  advisordomain.Domain{},
				Applier: advisordomain.NewApplier(),
				Encode:  encodeSuiteRecord,
				Kinds:   kindNames(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return advisordomain.NewGates(db) },
			Kind:         string(advisordomain.KindFinding),
			SubjectKinds: kindNames(),
			Create: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(findingOn(id), writer, opID, advisordomain.OpOpen, sentinel(), openingOf(id))
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(findingOn(id), writer, opID, advisordomain.OpRefresh, sentinel(),
					advisordomain.Refresh{Episode: suiteEpisode(id), Revision: 1,
						Body: json.RawMessage(`{"title":"refreshed under ` + opID + `"}`)})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(advisordomain.EvictionSubject(node), writer, opID,
					advisordomain.OpEviction, sentinel(), evictionOf(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(advisordomain.EvictionSubject(node), writer, opID,
					advisordomain.OpEviction, sentinel(), evictionOf(node, true))
			},
			// THE EVICTION IS THE ONLY GATE, said with an empty map.
			ApplyGates: map[string]func(id, writer, opID string) ([]byte, error){},
		}
	})
}

// kindNames is every kind the domain writes, from the enum.
func kindNames() []string {
	out := make([]string, 0, len(advisordomain.ObjectKinds))
	for _, k := range advisordomain.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteStamped is the instant every suite record is stamped at.
var suiteStamped = time.Unix(1_700_000_000, 0).UTC()

// sentinel is the scope of a record that writes its own subject alone, and
// domain the scope of one that reaches the whole log.
func sentinel() advisordomain.ScopeSet { return advisordomain.ScopeSet{Subject: true} }
func domain() advisordomain.ScopeSet {
	return advisordomain.ScopeSet{Terms: []advisordomain.ScopeTerm{{Kind: advisordomain.TermDomain}}}
}

// findingOn is the subject of the suite's key id, held to no grammar: the suite
// names its objects.
func findingOn(id string) advisordomain.Subject {
	return advisordomain.Subject{Kind: advisordomain.KindFinding, ID: id}
}

// suiteEpisode is the episode the suite opens on key id.
func suiteEpisode(id string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("episode/"+id)).String()
}

// suiteDigest is a digest the suite's verdicts and fixes carry.
var suiteDigest = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

// openingOf is an episode of the suite's key id, opened by the engine.
func openingOf(id string) advisordomain.Opening {
	return advisordomain.Opening{Episode: suiteEpisode(id), Key: "suite.class:" + id, Class: "suite.class",
		Source: advisordomain.SourceRule, Actor: advisordomain.ActorOwner, Area: "setup",
		Severity: advisordomain.SeverityCosts, SubjectKind: "seat", SubjectID: id, Revision: 1,
		VerdictDigest: suiteDigest, FixDigest: suiteDigest, Body: json.RawMessage(`{"title":"suite"}`)}
}

// encodeSuiteRecord builds one valid record of each kind about id at any
// version. The first three kinds are a real sequence ([advisordomain.ObjectKinds]).
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject, op, scope, payload := suitePayload(advisordomain.ObjectKind(kind), id)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		body = nil
	}
	return advisordomain.Encode(advisordomain.MutationRecord{
		RecordEnvelope: advisordomain.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node", Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorSystem,
	})
}

// suitePayload is one valid (subject, op, scope, payload) per kind about id.
func suitePayload(kind advisordomain.ObjectKind, id string) (
	advisordomain.Subject, advisordomain.OpKind, advisordomain.ScopeSet, any) {

	switch kind {
	case advisordomain.KindFinding:
		return findingOn(id), advisordomain.OpOpen, sentinel(), openingOf(id)
	case advisordomain.KindRun:
		return advisordomain.RunSubject(id), advisordomain.OpRecord, sentinel(), advisordomain.RunRecord{
			Run: id, Trigger: advisordomain.TriggerSchedule, StartedAt: suiteStamped,
			EndedAt: suiteStamped.Add(time.Minute), Status: advisordomain.RunDone,
			Classes: json.RawMessage(`[{"class":"suite.class","status":"done","observed":1}]`)}
	case advisordomain.KindReport:
		return advisordomain.ReportSubject(id), advisordomain.OpWrite, sentinel(), advisordomain.Report{
			Week: id, From: suiteStamped.Add(-7 * 24 * time.Hour), To: suiteStamped,
			Body: json.RawMessage(`{"opened":1}`)}
	case advisordomain.KindAlarm:
		return advisordomain.Subject{Kind: kind, ID: id}, advisordomain.OpRaise, sentinel(),
			advisordomain.AlarmRaise{Node: "node-1", Kind: "pool_starved", Instance: id, RaisedAt: suiteStamped}
	case advisordomain.KindRetention:
		_, expiry := advisordomain.ExpiryFor(suiteStamped)
		return advisordomain.Subject{Kind: kind, ID: id}, advisordomain.OpExpire, domain(), expiry
	case advisordomain.KindEviction:
		return advisordomain.EvictionSubject(id), advisordomain.OpEviction, sentinel(), evictionOf(id, false)
	case advisordomain.KindGeneration:
		return advisordomain.GenerationSubject(2), advisordomain.OpGeneration, sentinel(),
			advisordomain.Generation{V: 1, Generation: 2, By: "suite", StreamAt: suiteStamped}
	case advisordomain.KindTrimmed:
		floor := statelog.Position{Stream: advisordomain.Domain{}.Stream().Name, Generation: 1, Seq: 5}
		return advisordomain.TrimmedSubject(floor), advisordomain.OpTrimmed, sentinel(), nil
	}
	return advisordomain.Subject{Kind: kind, ID: id}, advisordomain.OpBarrier, sentinel(), nil
}

// encodeSuiteGate is a node's eviction or readmission as a peer publishes it.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return rawRecord(advisordomain.EvictionSubject(node), "suite-peer", opID,
		advisordomain.OpEviction, sentinel(), evictionOf(node, readmit))
}

// evictionOf is node's eviction, or its readmission.
func evictionOf(node string, readmit bool) advisordomain.Eviction {
	return advisordomain.Eviction{V: advisordomain.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteStamped, Readmitted: readmit}
}

// rawRecord is one record as a writer stamps it, by the engine.
func rawRecord(subject advisordomain.Subject, writer, opID string, op advisordomain.OpKind,
	scope advisordomain.ScopeSet, payload any) ([]byte, error) {

	return rawRecordBy(subject, writer, opID, op, scope, payload, iam.Actor{Name: "suite", Kind: iam.ActorSystem})
}

// rawRecordBy is one record as a writer stamps it, by actor.
func rawRecordBy(subject advisordomain.Subject, writer, opID string, op advisordomain.OpKind,
	scope advisordomain.ScopeSet, payload any, actor iam.Actor) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return advisordomain.Encode(advisordomain.MutationRecord{
		RecordEnvelope: advisordomain.RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: suiteStamped, Gen: 1,
			Writer: writer, Scope: scope,
		},
		Mutation: body, Actor: actor.Name, ActorKind: actor.Kind,
	})
}

// suiteWrite is one write through the domain's own production writer: an
// episode opened on a key nobody recorded, which needs nothing before it.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := advisordomain.NewWriter(advisordomain.WriterDeps{Publisher: pub, DB: db})
	if err != nil {
		return err
	}
	o := openingOf("suite-seat")
	o.Episode = advisordomain.EpisodeID(o.Key, time.Now())
	_, err = w.Open(ctx, advisordomain.Call{
		By:   iam.Actor{Name: "engine", Kind: iam.ActorSystem},
		OpID: statelog.NewOpID(time.Now(), "suite"),
	}, o, advisordomain.IfNoRecord)
	return err
}
