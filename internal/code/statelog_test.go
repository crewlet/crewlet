package code_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
			Applier: code.NewApplier(nil),
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
			// AND OF ITS READ INDEX'S BARRIER, which the engine registers
			// for this log: certified to write no row.
			EncodeBarrier: code.EncodeBarrier,
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

// suiteWrite is the writes through the domain's own lifecycle store, which
// reaches the builder every write path shares ([code.Store] and the committer
// alike) — where the framework's stamp is kept or lost — each decided from the
// rows the ones before it made, since the suite applies what its publisher
// appends.
//
// THE ACTIVATION'S FENCE, the store's configuration path; its create's stamp
// is the same builder's, and the round-trip harness publishes and applies it
// (store_test.go).
//
// AND EVERY RECORD A TOOL'S LIFECYCLE MAKES, in the sequence a seat's own
// scratch repository lives through, each decided by the chart and the policy
// the store is handed and published by the lifecycle's own builder: its
// create; its rename, which leaves the old name redirecting; a settings change
// the tool makes; a live turn's hold on it, every field at its bound — the
// turn's id at MaxProvenanceBytes, decided at the create's own position, as a
// turn's hold is decided at its read; and, the store's clock past
// code.RedirectWindow, the release of the old name the retention pass asks
// (Store.ReleaseName). Its apply frees nothing here — the suite's broker
// stamps a record seconds after the rename, and a release frees a name only
// past the redirect's end by the record's own broker instant — but the record
// is the builder's, which is what the stamp is certified on. A ref hold is the
// one hold field left out: it holds a seat's branch, whose row only a push
// makes, and a push is the committer's write path, whose records the push
// candidate (suitePush) carries.
//
// Every request must be answered applied: a refusal, or one left pending, is a
// sequence the steps after it would decide from rows that never held it.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	now := suiteAt
	s, err := code.NewStore(code.StoreOptions{Publisher: pub, DB: db, NodeID: "suite-node",
		Now:       func() time.Time { return now },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: testChart()}, true },
		Policy:    func() code.LifecyclePolicy { return code.ScratchOf(config.Code{}) },
	})
	if err != nil {
		return err
	}
	if _, err = s.Activate(ctx, suiteAt); err != nil {
		return err
	}
	dev := code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	ask := func(what string, req code.LifecycleRequest) (code.LifecycleResult, error) {
		req.OpID, req.Actor = statelog.NewOpID(time.Now(), "code.suite"), dev
		res, err := s.Lifecycle(ctx, req)
		switch {
		case err != nil:
			return res, fmt.Errorf("the suite's %s: %w", what, err)
		case res.Refusal != "":
			return res, fmt.Errorf("the suite's %s was refused %s: %s", what, res.Refusal, res.Detail)
		case res.Outcome != statelog.OutcomeApplied:
			return res, fmt.Errorf("the suite's %s was answered %q, not applied", what, res.Outcome)
		}
		return res, nil
	}
	made, err := ask("scratch create", code.LifecycleRequest{Kind: code.LifecycleCreate, Name: "dev.suite",
		Create: &code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.RepoScratch,
			Owner: code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, Visibility: code.VisibilityCompany,
			Search: code.SearchGrep, DefaultBranch: "main"}})
	if err != nil {
		return err
	}
	id := made.Repo.ID
	if _, err = ask("rename", code.LifecycleRequest{Kind: code.LifecycleRename, RepoID: id,
		Name: "dev.suite-renamed"}); err != nil {
		return err
	}
	description := "the suite's scratch repository"
	if _, err = ask("settings change", code.LifecycleRequest{Kind: code.LifecycleSettings, RepoID: id,
		Change: &code.RepoChange{Description: &description}}); err != nil {
		return err
	}
	if _, err = ask("hold", code.LifecycleRequest{Kind: code.LifecycleHold, RepoID: id,
		TurnID: strings.Repeat("t", code.MaxProvenanceBytes), Seen: made.Position}); err != nil {
		return err
	}
	now = suiteAt.Add(code.RedirectWindow + time.Hour)
	switch released, err := s.ReleaseName(ctx, "dev.suite", id); {
	case err != nil:
		return fmt.Errorf("the suite's release of the old name: %w", err)
	case !released:
		return errors.New("the suite's release of the old name published no record, its clock past " +
			"code.RedirectWindow")
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

// suitePush is four pushes of id: a person's of its main branch with its
// pack, received by git; a seat's of a branch in its namespace, built by the
// engine, whose run carries every field at its bound; the seat's coding
// box's of another, received by git, whose run names as well the coding run
// that pushed — its launch's UUID — and its coding agent at its bound; and the
// ref lifecycle's expiry of the first seat branch, on the engine's own
// account, whose row keeps the commit it deleted (its Old). Its
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
	}, {
		PushID:    "push-expiry-" + id,
		Principal: code.Principal{Kind: code.PrincipalSystem},
		Via:       code.ViaExpiry,
		Node:      "suite-node",
		Commands: []code.Command{{
			Ref: "refs/heads/seats/suite-seat/work", Old: oidOf("seat" + id), New: code.ZeroOID,
			Result: code.ResultOK,
		}},
		ReceivedAt: suiteAt,
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
			CheckKey: code.CheckKey{RepoID: id, OID: oidOf(id), Target: mainRef, Name: "unit", Attempt: 1},
			Purpose:  code.PurposeTip, Reason: code.QueueDispatch,
			Def:      &code.CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 600},
			QueuedAt: suiteAt,
		}, code.Scope{code.ChecksPath(id)}
	case code.KindPerson:
		// A PERSON'S MARK, on their own subject: read through a position on
		// the code log, which names no notice and so carries no positions.
		through := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 1}
		return code.PersonSubject(id), code.OpMarks, code.PersonMarks{Handle: id,
			Gesture: code.MarkGesture{ReadThrough: &through}}, code.Scope{code.PersonPath(id)}
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
				Applier: code.NewApplier(nil),
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
			// A COMMENT ON ONE OF THE REPOSITORY'S CHANGES, on the change's
			// own subject, which names the change and never the repository:
			// both the applier and the reader take the repository off the
			// record's scope, so a reader handed no body cannot keep the
			// rule for it.
			About: func(id, writer, opID string) (statelog.Subject, []byte, error) {
				change := "change-of-" + id
				body, err := gateSuiteRecord(code.ChangeSubject(change), code.OpComment, writer, opID,
					code.Comment{RepoID: id, ChangeID: change, CommentID: opID, ThreadID: opID,
						By: code.Principal{Kind: code.PrincipalAgent, Seat: "suite-seat"}, Body: "About it."},
					code.Scope{code.RepoPath(id), code.ChangePath(change)})
				return statelog.Subject{Kind: string(code.KindChange), ID: change}, body, err
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
