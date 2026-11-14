package gitlet_test

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

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CODE DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE — the eleventh
// domain to run it, and the ninth that claims identity: every promise the framework
// makes about a strict log (stamping, deferral, the gates, the eviction
// census, the trimmed record) is held here by the cases that hold it for
// Worklet and Doclet, never by a copy of them.
func TestTheCodeDomainPassesTheStateLogSuite(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  gitlet.Domain{},
			Applier: gitlet.NewApplier(nil),
			// Migrate is nil: the tables ship in the replicated estate's
			// own migration, so the suite proves the schema that ships.
			Encode: encodeSuiteRecord,
			// EVERY VERSIONED FIELD, each on a record carrying it alone:
			// whom an agent's write was made for, version 2.
			Fields:        gitlet.VersionedFields(),
			Carrying:      carryingSuiteRecord,
			Kinds:         suiteKinds(),
			Rows:          gitlet.NewRows,
			Write:         suiteWrite,
			EncodeTrimmed: gitlet.EncodeTrimmed,
			// AND OF ITS READ INDEX'S BARRIER, which the engine registers
			// for this log: certified to write no row.
			EncodeBarrier: gitlet.EncodeBarrier,
			EncodeGate:    encodeSuiteGate,
		}
	})
}

// suiteAt is every authored instant the suite's records carry.
var suiteAt = time.Unix(1_700_000_000, 0).UTC()

// carryingSuiteRecord is a record carrying exactly field, through the
// production encoder, which stamps it at the field's version: each a seat's
// write made for the person's seat founder — a routing's beside its agent
// actor on a dequeue, a push entry's routing's on the change the push merges,
// an enqueue's own, and a seat's push entry's own.
func carryingSuiteRecord(field statelog.VersionedField) ([]byte, error) {
	const repo = "0190f3c4-0000-7000-8000-00000000c0de"
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	routing := func(carrier *types.WorkingFor) gitlet.ChangeRouting {
		return gitlet.ChangeRouting{ChangeID: "0190f3c4-0000-7000-8000-0000000000c1", ChangeRef: "web!1",
			RepoName: "web", Title: "Retry the upload", Actor: "dev", Owner: "cto",
			OwnerKind: gitlet.PartyAgent, Authors: []string{"cto"}, Merged: true, WorkingFor: carrier}
	}
	entry := gitlet.PushEntry{PushID: "push-carrying", Principal: seat, Via: gitlet.ViaEngine, Node: "suite-node",
		Commands:   []gitlet.Command{{Ref: mainRef, Old: oidOf("A"), New: oidOf("B"), Result: gitlet.ResultOK}},
		ReceivedAt: suiteAt, Day: suiteAt.Format(time.DateOnly)}
	var (
		op      gitlet.OpKind
		payload any
	)
	switch field.Name {
	case "ChangeRouting.WorkingFor":
		r := routing(founder)
		op, payload = gitlet.OpDequeue, gitlet.Dequeue{RepoID: repo,
			EntryID: "0190f3c4-0000-7000-8000-0000000000e1", Principal: seat, Routing: &r}
	case "PushEntry.Routing.WorkingFor":
		entry.Routing = []gitlet.ChangeRouting{routing(founder)}
		op, payload = gitlet.OpPush, gitlet.PushBatch{RepoID: repo, Pushes: []gitlet.PushEntry{entry}}
	case "Mutation.WorkingFor":
		op, payload = gitlet.OpEnqueue, gitlet.Enqueue{RepoID: repo,
			EntryID: "0190f3c4-0000-7000-8000-0000000000e1", ChangeID: "0190f3c4-0000-7000-8000-0000000000c1",
			TargetRef: mainRef, HeadOID: oidOf("B"), Method: gitlet.MergeMethodMerge, Principal: seat,
			WorkingFor: founder}
	case "PushEntry.WorkingFor":
		entry.WorkingFor = founder
		op, payload = gitlet.OpPush, gitlet.PushBatch{RepoID: repo, Pushes: []gitlet.PushEntry{entry}}
	default:
		return nil, fmt.Errorf("no suite record carries %s: add one here with its row", field.Name)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return gitlet.Encode(gitlet.MutationRecord{
		RecordEnvelope: gitlet.RecordEnvelope{
			OpID: "suite-carrying", Subject: gitlet.RepoSubject(repo), Op: op, CreatedAt: suiteAt,
			Gen: 1, Writer: "suite-node", Scope: gitlet.Scope{gitlet.RepoPath(repo)},
		},
		Mutation: body, Actor: "suite-node", ActorKind: gitlet.ActorSystem,
	})
}

// suiteSettings are a declared repository's settings, as a reconcile hands
// them over.
func suiteSettings() gitlet.RepoSettings {
	return gitlet.RepoSettings{
		ManagedBy: gitlet.ManagedConfig, Kind: gitlet.RepoProject,
		Owner:      gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "ENG"},
		Visibility: gitlet.VisibilityCompany, Search: gitlet.SearchIndexed, DefaultBranch: "main",
		Protections: []gitlet.Protection{
			{PushRule: gitlet.PushRule{Pattern: "refs/heads/main", DirectPush: gitlet.DirectPersons}},
		},
	}
}

// suiteWrite is the writes through the domain's own lifecycle store, which
// reaches the builder every write path shares ([gitlet.Store] and the committer
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
// gitlet.RedirectWindow, the release of the old name the retention pass asks
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
	s, err := gitlet.NewStore(gitlet.StoreOptions{Publisher: pub, DB: db, NodeID: "suite-node",
		Now:       func() time.Time { return now },
		SeatRules: func() (gitlet.SeatRules, bool) { return gitlet.SeatRules{Roster: testChart()}, true },
		Policy:    func() gitlet.LifecyclePolicy { return gitlet.ScratchOf(config.Code{}) },
	})
	if err != nil {
		return err
	}
	if _, err = s.Activate(ctx, suiteAt); err != nil {
		return err
	}
	dev := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	ask := func(what string, req gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
		req.OpID, req.Actor = statelog.NewOpID(time.Now(), "code_suite"), dev
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
	made, err := ask("scratch create", gitlet.LifecycleRequest{Kind: gitlet.LifecycleCreate, Name: "dev.suite",
		Create: &gitlet.RepoSettings{ManagedBy: gitlet.ManagedTools, Kind: gitlet.RepoScratch,
			Owner: gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "dev"}, Visibility: gitlet.VisibilityCompany,
			Search: gitlet.SearchGrep, DefaultBranch: "main"}})
	if err != nil {
		return err
	}
	id := made.Repo.ID
	if _, err = ask("rename", gitlet.LifecycleRequest{Kind: gitlet.LifecycleRename, RepoID: id,
		Name: "dev.suite-renamed"}); err != nil {
		return err
	}
	description := "the suite's scratch repository"
	if _, err = ask("settings change", gitlet.LifecycleRequest{Kind: gitlet.LifecycleSettings, RepoID: id,
		Change: &gitlet.RepoChange{Description: &description}}); err != nil {
		return err
	}
	if _, err = ask("hold", gitlet.LifecycleRequest{Kind: gitlet.LifecycleHold, RepoID: id,
		TurnID: strings.Repeat("t", gitlet.MaxProvenanceBytes), Seen: made.Position}); err != nil {
		return err
	}
	now = suiteAt.Add(gitlet.RedirectWindow + time.Hour)
	switch released, err := s.ReleaseName(ctx, "dev.suite", id); {
	case err != nil:
		return fmt.Errorf("the suite's release of the old name: %w", err)
	case !released:
		return errors.New("the suite's release of the old name published no record, its clock past " +
			"gitlet.RedirectWindow")
	}
	return nil
}

// suiteKinds is every kind, derived from the enum rather than typed again.
func suiteKinds() []string {
	out := make([]string, 0, len(gitlet.ObjectKinds))
	for _, k := range gitlet.ObjectKinds {
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
func suitePack(id string) *gitlet.PackRef {
	return &gitlet.PackRef{
		PackID: "push-" + id, Object: keyOf(id), Hash: objstore.HashOf([]byte(id)),
		Size:         32 + gitlet.IndexBytesFor(1) + gitlet.ReverseBytesFor(1),
		IndexBytes:   gitlet.IndexBytesFor(1),
		ReverseBytes: gitlet.ReverseBytesFor(1),
		GitChecksum:  oidOf("pack" + id), Objects: 1,
	}
}

// suitePush is four pushes of id: a person's of its main branch with its
// pack, received by git; a seat's of a branch in its namespace, built by the
// engine, whose run carries every field at its bound; the seat's coding
// box's of another, received by git, whose run names as well the executor
// that pushed — its launch's UUID — and its harness at its bound; and the
// ref lifecycle's expiry of the first seat branch, on the engine's own
// account, whose row keeps the commit it deleted (its Old). Its
// committer counted both of the seat's on the company day of the suite's
// instant — so every column a push row has, and the seat's count of its day
// (code_seat_days), are ones the suite's replays and identity claims compare,
// but whom a seat's push was made for: a record carrying it is stamped above
// the base every ordinary record of the suite is held to, and
// TestACodeRowKeepsWhomItsWriteWasMadeFor replays the rows that keep it.
func suitePush(id string) gitlet.PushBatch {
	bound := strings.Repeat("r", gitlet.MaxProvenanceBytes)
	return gitlet.PushBatch{RepoID: id, Pushes: []gitlet.PushEntry{{
		PushID: "push-" + id,
		Principal: gitlet.Principal{
			Kind: gitlet.PrincipalOperator, OperatorID: "op-suite", Seat: "suite",
		},
		Via:  gitlet.ViaGit,
		Node: "suite-node",
		Commands: []gitlet.Command{{
			Ref: "refs/heads/main", Old: gitlet.ZeroOID, New: oidOf(id), Result: gitlet.ResultOK,
		}},
		Pack:       suitePack(id),
		ReceivedAt: suiteAt,
	}, {
		PushID: "push-seat-" + id,
		Principal: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "suite-seat",
			Run: gitlet.RunProvenance{TurnID: bound, WorkBackend: bound, WorkKey: bound, Model: bound}},
		Via:  gitlet.ViaEngine,
		Node: "suite-node",
		Commands: []gitlet.Command{{
			Ref: "refs/heads/seats/suite-seat/work", Old: gitlet.ZeroOID, New: oidOf("seat" + id),
			Result: gitlet.ResultOK,
		}},
		Pack:       suitePack("seat-" + id),
		ReceivedAt: suiteAt,
		Day:        suiteAt.Format(time.DateOnly),
	}, {
		PushID: "push-box-" + id,
		Principal: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "suite-seat",
			Run: gitlet.RunProvenance{TurnID: bound, WorkBackend: bound, WorkKey: bound, Model: bound,
				RunID: "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", Harness: bound}},
		Via:  gitlet.ViaGit,
		Node: "suite-node",
		Commands: []gitlet.Command{{
			Ref: "refs/heads/seats/suite-seat/box", Old: gitlet.ZeroOID, New: oidOf("box" + id),
			Result: gitlet.ResultOK,
		}},
		Pack:       suitePack("box-" + id),
		ReceivedAt: suiteAt,
		Day:        suiteAt.Format(time.DateOnly),
	}, {
		PushID:    "push-expiry-" + id,
		Principal: gitlet.Principal{Kind: gitlet.PrincipalSystem},
		Via:       gitlet.ViaExpiry,
		Node:      "suite-node",
		Commands: []gitlet.Command{{
			Ref: "refs/heads/seats/suite-seat/work", Old: oidOf("seat" + id), New: gitlet.ZeroOID,
			Result: gitlet.ResultOK,
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
func suitePayload(kind gitlet.ObjectKind, id string) (gitlet.Subject, gitlet.OpKind, any, gitlet.Scope) {
	switch kind {
	case gitlet.KindName:
		return gitlet.NameSubject(id), gitlet.OpCreate,
			gitlet.CreateRepo{RepoID: id, Name: id, Settings: suiteSettings()},
			gitlet.Scope{gitlet.NamePath(gitlet.NameToken(id)), gitlet.RepoPath(id)}
	case gitlet.KindRepo:
		return gitlet.RepoSubject(id), gitlet.OpPush, suitePush(id), gitlet.Scope{gitlet.RepoPath(id)}
	case gitlet.KindEviction:
		return gitlet.EvictionSubject(id), gitlet.OpEviction, gateSuiteEviction(id, false),
			gitlet.Scope{gitlet.DomainPath()}
	case gitlet.KindGeneration:
		return gitlet.GenerationSubject(2), gitlet.OpGeneration, gitlet.Generation{
			V: gitlet.GateRecordVersion, Generation: 2, By: "suite", StreamCreatedAt: suiteAt,
		}, gitlet.Scope{gitlet.DomainPath()}
	case gitlet.KindCompany:
		return gitlet.CompanySubject(), gitlet.OpActivate,
			gitlet.Activated{ConfigEpoch: suiteAt.UnixMicro()}, gitlet.Scope{gitlet.ActivationPath()}
	case gitlet.KindBarrier:
		return gitlet.BarrierSubject(), gitlet.OpBarrier, nil, gitlet.Scope{statelog.FrameworkScope}
	case gitlet.KindTrimmed:
		return gitlet.Subject{Kind: gitlet.KindTrimmed, ID: id}, gitlet.OpTrimmed, nil,
			gitlet.Scope{statelog.FrameworkScope}
	case gitlet.KindChecks:
		// A TIP'S CHECK QUEUED on the repository's checks subject, which the
		// deletion gate reads the repository off: a purged repository's
		// late check record writes nothing.
		return gitlet.ChecksSubject(id), gitlet.OpCheckQueued, gitlet.CheckQueued{
			CheckKey: gitlet.CheckKey{RepoID: id, OID: oidOf(id), Target: mainRef, Name: "unit", Attempt: 1},
			Purpose:  gitlet.PurposeTip, Reason: gitlet.QueueDispatch,
			Def:      &gitlet.CheckDef{Name: "unit", Run: "make test", TimeoutSeconds: 600},
			QueuedAt: suiteAt,
		}, gitlet.Scope{gitlet.ChecksPath(id)}
	case gitlet.KindPerson:
		// A PERSON'S MARK, on their own subject: read through a position on
		// the code log, which names no notice and so carries no positions.
		through := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 1}
		return gitlet.PersonSubject(id), gitlet.OpMarks, gitlet.PersonMarks{Handle: id,
			Gesture: gitlet.MarkGesture{ReadThrough: &through}}, gitlet.Scope{gitlet.PersonPath(id)}
	}
	return gitlet.Subject{Kind: kind, ID: id}, gitlet.OpCreate, nil, gitlet.Scope{gitlet.DomainPath()}
}

// encodeSuiteRecord builds one valid record of each kind at an arbitrary
// version — including one above this build's, which is exactly what a newer
// peer publishes and what this build has to retain.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject, op, payload, scope := suitePayload(gitlet.ObjectKind(kind), id)
	rec := gitlet.MutationRecord{
		RecordEnvelope: gitlet.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op, CreatedAt: suiteAt,
			Gen: 1, Writer: "suite-node", Scope: scope,
		},
		Actor: "suite", ActorKind: gitlet.ActorSystem,
	}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rec.Mutation = body
	}
	return gitlet.Encode(rec)
}

// encodeSuiteGate is the eviction record a peer's store publishes onto this
// log — or, with readmit, the inverse commit that takes the node back.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return gateSuiteRecord(gitlet.EvictionSubject(node), gitlet.OpEviction, "suite-peer", opID,
		gateSuiteEviction(node, readmit), gitlet.Scope{gitlet.DomainPath()})
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
				Domain:  gitlet.Domain{},
				Applier: gitlet.NewApplier(nil),
				Encode:  encodeSuiteRecord,
				Kinds:   suiteKinds(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return gitlet.NewGates(db) },
			Kind:         string(gitlet.KindRepo),
			SubjectKinds: suiteKinds(),
			// A UNIT REPOSITORY, so the family's deferrals and replays carry
			// the narrow visibility a row must keep whatever the order...
			Create: func(id, writer, opID string) ([]byte, error) {
				subject, op, payload, scope := suitePayload(gitlet.KindName, id)
				create := payload.(gitlet.CreateRepo)
				create.Settings.Visibility = gitlet.VisibilityUnit
				return gateSuiteRecord(subject, op, writer, opID, create, scope)
			},
			// AN ORDINARY WRITE ON THE REPOSITORY'S OWN SUBJECT — and
			// one that widens it to the company, so a settings record
			// moving the visibility goes through every gate too.
			Write: func(id, writer, opID string) ([]byte, error) {
				settings := suiteSettings()
				settings.Description = opID
				return gateSuiteRecord(gitlet.RepoSubject(id), gitlet.OpSettings, writer, opID,
					gitlet.SettingsRecord{RepoID: id, Settings: settings},
					gitlet.Scope{gitlet.RepoPath(id)})
			},
			// A PURGE OF AN ACTIVE REPOSITORY is the applier's ordinary
			// case: it is applied whatever the status.
			Purge: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(gitlet.RepoSubject(id), gitlet.OpPurge, writer, opID,
					gitlet.Purge{RepoID: id, Name: id, Reason: "the gate suite"},
					gitlet.Scope{gitlet.NamePath(gitlet.NameToken(id)), gitlet.RepoPath(id)})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(gitlet.EvictionSubject(node), gitlet.OpEviction, writer, opID,
					gateSuiteEviction(node, false), gitlet.Scope{gitlet.DomainPath()})
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(gitlet.EvictionSubject(node), gitlet.OpEviction, writer, opID,
					gateSuiteEviction(node, true), gitlet.Scope{gitlet.DomainPath()})
			},
			// A COMMENT ON ONE OF THE REPOSITORY'S CHANGES, on the change's
			// own subject, which names the change and never the repository:
			// both the applier and the reader take the repository off the
			// record's scope, so a reader handed no body cannot keep the
			// rule for it.
			About: func(id, writer, opID string) (statelog.Subject, []byte, error) {
				change := "change-of-" + id
				body, err := gateSuiteRecord(gitlet.ChangeSubject(change), gitlet.OpComment, writer, opID,
					gitlet.Comment{RepoID: id, ChangeID: change, CommentID: opID, ThreadID: opID,
						By: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "suite-seat"}, Body: "About it."},
					gitlet.Scope{gitlet.RepoPath(id), gitlet.ChangePath(change)})
				return statelog.Subject{Kind: string(gitlet.KindChange), ID: change}, body, err
			},
		}
	})
}

// gateSuiteRecord is one record the gate family applies, written by writer.
func gateSuiteRecord(subject gitlet.Subject, op gitlet.OpKind, writer, opID string,
	payload any, scope gitlet.Scope) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return gitlet.Encode(gitlet.MutationRecord{
		RecordEnvelope: gitlet.RecordEnvelope{
			V: gitlet.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteAt, Gen: 1, Writer: writer, Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: gitlet.ActorSystem,
	})
}

// gateSuiteEviction is an eviction of node, or its readmission.
func gateSuiteEviction(node string, readmitted bool) gitlet.Eviction {
	return gitlet.Eviction{
		V: gitlet.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteAt, Readmitted: readmitted,
	}
}
