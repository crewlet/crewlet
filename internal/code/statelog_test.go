package code_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CODE DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE — the eighth domain
// to run it, and the sixth that claims identity: every promise the framework
// makes about a strict log (stamping, deferral, the gates, the eviction
// census, the trimmed record) is held here by the cases that hold it for the
// tracker and the knowledge base, never by a copy of them.
func TestTheCodeDomainPassesTheStateLogSuite(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  code.Domain{},
			Applier: code.NewApplier(),
			// Migrate is nil: the tables ship in the replicated estate's
			// own migration, so the suite proves the schema that ships.
			Encode: encodeSuiteRecord,
			// EMPTY: every field this domain writes is in the base format,
			// so there is no record to carry — the first versioned field
			// brings a Carrying case, which the suite refuses to run
			// without.
			Fields:        code.VersionedFields(),
			Kinds:         suiteKinds(),
			Rows:          code.NewRows,
			Write:         suiteWrite,
			EncodeTrimmed: code.EncodeTrimmed,
			EncodeGate:    encodeSuiteGate,
		}
	})
}

// suiteAt is every authored instant the suite's records carry.
var suiteAt = time.Unix(1_700_000_000, 0).UTC()

// suiteSettings are a declared repository's settings, as a reconcile hands
// them over.
func suiteSettings() code.RepoSettings {
	return code.RepoSettings{
		ManagedBy: code.ManagedConfig, Kind: code.RepoProject,
		Owner:      code.Owner{Kind: code.OwnerUnit, Ref: "ENG"},
		Visibility: code.VisibilityCompany, Search: code.SearchIndexed, DefaultBranch: "main",
		Protections: []code.Protection{
			{PushRule: code.PushRule{Pattern: "refs/heads/main", DirectPush: code.DirectPersons}},
		},
	}
}

// suiteWrite is one write through the domain's own lifecycle store, which
// reaches the builder every write path shares ([code.Store] and the committer
// alike) — where the framework's stamp is kept or lost.
//
// THE ACTIVATION'S FENCE AND NOTHING AFTER IT: a repository is decided only
// once this node has APPLIED its activation's fence (the decide's session
// floor), and the suite's publisher appends to a log nothing applies — so a
// create after it would be refused as an activation Activate never made,
// which is the rule working. The create's stamp is the same builder's, and
// the round-trip harness publishes and applies it (store_test.go).
//
// AND A TOOL'S LIFECYCLE REQUEST, the store's other write path: a seat's
// create of its own scratch repository, decided by the chart and the policy
// the store is handed. No activation fences a tool's record, so it is decided
// at once, and its record is the lifecycle's own builder's.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	s, err := code.NewStore(code.StoreOptions{Publisher: pub, DB: db, NodeID: "suite-node",
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: testChart()}, true },
		Policy:    func() code.LifecyclePolicy { return code.ScratchOf(config.Code{}) },
	})
	if err != nil {
		return err
	}
	if _, err = s.Activate(ctx, suiteAt); err != nil {
		return err
	}
	res, err := s.Lifecycle(ctx, code.LifecycleRequest{Kind: code.LifecycleCreate,
		OpID: statelog.NewOpID(time.Now(), "code.suite"), Actor: code.Principal{Kind: code.PrincipalAgent, Seat: "dev"},
		Name: "dev.suite", Create: &code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.RepoScratch,
			Owner: code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, Visibility: code.VisibilityCompany,
			Search: code.SearchGrep, DefaultBranch: "main"}})
	switch {
	case err != nil:
		return err
	case res.Refusal != "":
		return fmt.Errorf("the suite's scratch create was refused %s: %s", res.Refusal, res.Detail)
	}
	return nil
}

// suiteKinds is every kind, derived from the enum rather than typed again.
func suiteKinds() []string {
	out := make([]string, 0, len(code.ObjectKinds))
	for _, k := range code.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// oidOf is a deterministic object id for a fixture.
func oidOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:40]
}

// keyOf is a DETERMINISTIC object key for a fixture: a version 7 key whose
// random half is the fixture's own digest, so two runs name one object alike.
//
// It panics on a key the store would refuse, which is a fixture's bug rather
// than a case: the shape is fixed and only the digest varies.
func keyOf(s string) objstore.Key {
	sum := sha256.Sum256([]byte(s))
	k, err := objstore.ParseKey("0190f3c4-5b6a-7c8d-9e0f-" + hex.EncodeToString(sum[:])[:12])
	if err != nil {
		panic("a fixture key for " + s + ": " + err.Error())
	}
	return k
}

// suitePack is a pack of one object that lays out a bundle (D16): the pack's
// header and trailer, its index and its reverse index — the record a push's
// proposal would admit.
func suitePack(id string) *code.PackRef {
	return &code.PackRef{
		PackID: "push-" + id, Object: keyOf(id), Hash: objstore.HashOf([]byte(id)),
		Size:         32 + code.IndexBytesFor(1) + code.ReverseBytesFor(1),
		IndexBytes:   code.IndexBytesFor(1),
		ReverseBytes: code.ReverseBytesFor(1),
		GitChecksum:  oidOf("pack" + id), Objects: 1,
	}
}

// suitePush is three pushes of id: a person's of its main branch with its
// pack, received by git; a seat's of a branch in its namespace, built by the
// engine, whose run carries every field at its bound; and the seat's coding
// box's of another, received by git, whose run names as well the coding run
// that pushed — its launch's UUID — and its coding agent at its bound. Its
// committer counted both of the seat's on the company day of the suite's
// instant — so every column a push row has, and the seat's count of its day
// (code_seat_days), are ones the suite's replays and identity claims compare.
func suitePush(id string) code.PushBatch {
	bound := strings.Repeat("r", code.MaxProvenanceBytes)
	return code.PushBatch{RepoID: id, Pushes: []code.PushEntry{{
		PushID: "push-" + id,
		Principal: code.Principal{
			Kind: code.PrincipalOperator, OperatorID: "op-suite", Seat: "suite",
		},
		Via:  code.ViaGit,
		Node: "suite-node",
		Commands: []code.Command{{
			Ref: "refs/heads/main", Old: code.ZeroOID, New: oidOf(id), Result: code.ResultOK,
		}},
		Pack:       suitePack(id),
		ReceivedAt: suiteAt,
	}, {
		PushID: "push-seat-" + id,
		Principal: code.Principal{Kind: code.PrincipalAgent, Seat: "suite-seat",
			Run: code.RunProvenance{TurnID: bound, WorkBackend: bound, WorkKey: bound, Model: bound}},
		Via:  code.ViaEngine,
		Node: "suite-node",
		Commands: []code.Command{{
			Ref: "refs/heads/seats/suite-seat/work", Old: code.ZeroOID, New: oidOf("seat" + id),
			Result: code.ResultOK,
		}},
		Pack:       suitePack("seat-" + id),
		ReceivedAt: suiteAt,
		Day:        suiteAt.Format(time.DateOnly),
	}, {
		PushID: "push-box-" + id,
		Principal: code.Principal{Kind: code.PrincipalAgent, Seat: "suite-seat",
			Run: code.RunProvenance{TurnID: bound, WorkBackend: bound, WorkKey: bound, Model: bound,
				RunID: "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", CodingAgent: bound}},
		Via:  code.ViaGit,
		Node: "suite-node",
		Commands: []code.Command{{
			Ref: "refs/heads/seats/suite-seat/box", Old: code.ZeroOID, New: oidOf("box" + id),
			Result: code.ResultOK,
		}},
		Pack:       suitePack("box-" + id),
		ReceivedAt: suiteAt,
		Day:        suiteAt.Format(time.DateOnly),
	}}}
}

// suitePayload is one valid (subject, op, payload, scope) per kind.
//
// # Why the name, the repository and the eviction are the first three kinds
//
// The suite publishes the FIRST THREE kinds a candidate declares, twice each,
// in order, so the declaration order decides what is certified — and these
// three are a real sequence: a create claims the name and makes the
// repository whose id the suite hands it, and the push then lands on that
// repository. A push on a repository no create made would certify a no-op.
func suitePayload(kind code.ObjectKind, id string) (code.Subject, code.OpKind, any, code.Scope) {
	switch kind {
	case code.KindName:
		return code.NameSubject(id), code.OpCreate,
			code.CreateRepo{RepoID: id, Name: id, Settings: suiteSettings()},
			code.Scope{code.NamePath(code.NameToken(id)), code.RepoPath(id)}
	case code.KindRepo:
		return code.RepoSubject(id), code.OpPush, suitePush(id), code.Scope{code.RepoPath(id)}
	case code.KindEviction:
		return code.EvictionSubject(id), code.OpEviction, gateSuiteEviction(id, false),
			code.Scope{code.DomainPath()}
	case code.KindGeneration:
		return code.GenerationSubject(2), code.OpGeneration, code.Generation{
			V: code.GateRecordVersion, Generation: 2, By: "suite", StreamCreatedAt: suiteAt,
		}, code.Scope{code.DomainPath()}
	case code.KindCompany:
		return code.CompanySubject(), code.OpActivate,
			code.Activated{ConfigEpoch: suiteAt.UnixMicro()}, code.Scope{code.ActivationPath()}
	case code.KindBarrier:
		return code.BarrierSubject(), code.OpBarrier, nil, code.Scope{statelog.FrameworkScope}
	case code.KindTrimmed:
		return code.Subject{Kind: code.KindTrimmed, ID: id}, code.OpTrimmed, nil,
			code.Scope{statelog.FrameworkScope}
	case code.KindChecks:
		// A TIP'S CHECK QUEUED on the repository's checks subject, which the
		// deletion gate reads the repository off: a purged repository's
		// late check record writes nothing.
		return code.ChecksSubject(id), code.OpCheckQueued, code.CheckQueued{
			CheckKey: code.CheckKey{RepoID: id, OID: oidOf(id), Name: "unit", Attempt: 1},
			Purpose:  code.PurposeTip, Target: "refs/heads/main", Reason: code.QueueDispatch,
			Def:      &code.CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 600},
			QueuedAt: suiteAt,
		}, code.Scope{code.ChecksPath(id)}
	}
	return code.Subject{Kind: kind, ID: id}, code.OpCreate, nil, code.Scope{code.DomainPath()}
}

// encodeSuiteRecord builds one valid record of each kind at an arbitrary
// version — including one above this build's, which is exactly what a newer
// peer publishes and what this build has to retain.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject, op, payload, scope := suitePayload(code.ObjectKind(kind), id)
	rec := code.MutationRecord{
		RecordEnvelope: code.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op, CreatedAt: suiteAt,
			Gen: 1, Writer: "suite-node", Scope: scope,
		},
		Actor: "suite", ActorKind: code.ActorSystem,
	}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rec.Mutation = body
	}
	return code.Encode(rec)
}

// encodeSuiteGate is the eviction record a peer's store publishes onto this
// log — or, with readmit, the inverse commit that takes the node back.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return gateSuiteRecord(code.EvictionSubject(node), code.OpEviction, "suite-peer", opID,
		gateSuiteEviction(node, readmit), code.Scope{code.DomainPath()})
}

// THE CODE DOMAIN'S GATE READER KEEPS THE RULE EVERY READER SHARES, certified
// by the family that certifies the tracker's and the knowledge base's — each
// reader tested against its own idea of the rule is how one of them came to
// answer the eviction before the deletion.
func TestTheCodeGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  code.Domain{},
				Applier: code.NewApplier(),
				Encode:  encodeSuiteRecord,
				Kinds:   suiteKinds(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return code.NewGates(db) },
			Kind:         string(code.KindRepo),
			SubjectKinds: suiteKinds(),
			// A UNIT REPOSITORY, so the family's deferrals and replays carry
			// the narrow visibility a row must keep whatever the order...
			Create: func(id, writer, opID string) ([]byte, error) {
				subject, op, payload, scope := suitePayload(code.KindName, id)
				create := payload.(code.CreateRepo)
				create.Settings.Visibility = code.VisibilityUnit
				return gateSuiteRecord(subject, op, writer, opID, create, scope)
			},
			// AN ORDINARY WRITE ON THE REPOSITORY'S OWN SUBJECT — and
			// one that widens it to the company, so a settings record
			// moving the visibility goes through every gate too.
			Write: func(id, writer, opID string) ([]byte, error) {
				settings := suiteSettings()
				settings.Description = opID
				return gateSuiteRecord(code.RepoSubject(id), code.OpSettings, writer, opID,
					code.SettingsRecord{RepoID: id, Settings: settings},
					code.Scope{code.RepoPath(id)})
			},
			// A PURGE OF AN ACTIVE REPOSITORY is the applier's ordinary
			// case: it is applied whatever the status.
			Purge: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(code.RepoSubject(id), code.OpPurge, writer, opID,
					code.Purge{RepoID: id, Name: id, Reason: "the gate suite"},
					code.Scope{code.NamePath(code.NameToken(id)), code.RepoPath(id)})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(code.EvictionSubject(node), code.OpEviction, writer, opID,
					gateSuiteEviction(node, false), code.Scope{code.DomainPath()})
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(code.EvictionSubject(node), code.OpEviction, writer, opID,
					gateSuiteEviction(node, true), code.Scope{code.DomainPath()})
			},
		}
	})
}

// gateSuiteRecord is one record the gate family applies, written by writer.
func gateSuiteRecord(subject code.Subject, op code.OpKind, writer, opID string,
	payload any, scope code.Scope) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return code.Encode(code.MutationRecord{
		RecordEnvelope: code.RecordEnvelope{
			V: code.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteAt, Gen: 1, Writer: writer, Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: code.ActorSystem,
	})
}

// gateSuiteEviction is an eviction of node, or its readmission.
func gateSuiteEviction(node string, readmitted bool) code.Eviction {
	return code.Eviction{
		V: code.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteAt, Readmitted: readmitted,
	}
}
