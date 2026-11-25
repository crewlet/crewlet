package worklet_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The tracker is certified by the framework's own suite, not a second one
// beside it: the properties every data node's copy rests on (a consistent
// declaration, an envelope for an unknown version, deterministic rows, an
// idempotent apply) are the framework's, and a domain copy would be a
// second opinion that wins the first time the two disagree.
func TestTheTrackerIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  worklet.Domain{},
			Applier: worklet.NewApplier(nil),
			// Migrate is nil: the tracker's tables ship in the replicated
			// estate's own migration, so the suite proves the migrated schema.
			Encode: encodeSuiteRecord,
			// Every arbitrated kind, the apply cases' three first: the
			// declaration case requires a domain to publish every kind it
			// arbitrates, so a partial list would fault the fixture.
			Kinds: suiteKinds(),
			// THE PRODUCTION TABLE, each field carried by a record built
			// through the writer's own encoder ([carryingSuiteRecord]).
			Fields:     worklet.VersionedFields(),
			Carrying:   carryingSuiteRecord,
			Rows:       worklet.NewRows,
			Write:      suiteWrite,
			EncodeGate: encodeSuiteGate,
			// THE PRODUCTION ENCODER the engine hands the trim.
			EncodeTrimmed: worklet.EncodeTrimmed,
			// AND OF ITS READ INDEX'S BARRIER, which the engine registers
			// for this log: certified to write no row.
			EncodeBarrier: worklet.EncodeBarrier,
		}
	})
}

// encodeSuiteGate is the eviction record a peer's writer publishes onto this
// log — or, with readmit, the inverse commit that takes the node back.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	at := time.Unix(1_700_000_000, 0).UTC()
	body, err := json.Marshal(worklet.Eviction{
		V: worklet.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: at, Readmitted: readmit,
	})
	if err != nil {
		return nil, err
	}
	op := "suite-evict-" + node
	if readmit {
		op = "suite-readmit-" + node
	}
	return worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			// PINNED, as the production writer pins it: a gate is read
			// by every build there will ever be ([worklet.GateRecordVersion]).
			V: worklet.GateRecordVersion, OpID: op,
			Subject: worklet.EvictionSubject(node), Op: worklet.OpEviction,
			CreatedAt: at, Writer: "suite-peer",
			Scope: worklet.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "suite", ActorKind: worklet.AuthorSystem,
	}.Encode()
}

// suiteWrite is one write through Worklet's own [worklet.Writer], the
// builder every write path in the domain shares, which is where the
// framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := worklet.NewWriter(worklet.WriterDeps{
		Publisher: pub, DB: db, NodeID: statelogtest.SuiteWriter,
		Actor: "suite", ActorKind: worklet.AuthorSystem,
	})
	if err != nil {
		return err
	}
	_, err = w.EvictNode(ctx, "suite-evict", "node-away")
	return err
}

// encodeSuiteRecord builds one valid record at any version, including one
// above this build's: that is what a newer peer publishes, and refusing it
// would leave the deferral contract untestable.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	at := time.Unix(1_700_000_000, 0).UTC()
	subject := worklet.Subject{Kind: worklet.ObjectKind(kind), ID: id}

	var payload any
	op := worklet.OpCreate
	switch worklet.ObjectKind(kind) {
	case worklet.KindProject:
		payload = worklet.Project{
			V: worklet.DocumentVersion, Key: id, Name: "Suite " + id,
			CreatedAt: at, UpdatedAt: at,
		}
	case worklet.KindTask:
		payload = worklet.Task{
			V: worklet.DocumentVersion, ID: id, Key: "SUITE-1",
			Project: "SUITE", Type: "task", Title: "suite " + id,
			Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
			Priority: worklet.PriorityNone, Rank: worklet.RankOrigin,
			CreatedAt: at, UpdatedAt: at,
		}
	case worklet.KindView:
		payload = worklet.View{
			V: worklet.DocumentVersion, ID: id, Name: "suite " + id,
			Type:      worklet.ViewList,
			Container: worklet.Container{Kind: "workspace", ID: ""},
			CreatedAt: at, UpdatedAt: at,
		}
	default:
		payload = map[string]any{"id": id}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	record := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Gen: 0, Writer: "suite-node",
			Scope: worklet.ScopeSet{Subject: true},
		},
		Mutation:  body,
		Actor:     "suite",
		ActorKind: worklet.AuthorSystem,
	}
	// Version zero takes the writer's path, whose Encode stamps it for the
	// suite's stamping case; any other version is encoded directly, exactly as
	// a peer at that version would publish it.
	if version == 0 {
		return record.Encode()
	}
	if version >= worklet.WorkingForVersion && id == "suite-b" {
		// An agent's write made for somebody, on half the objects at a
		// version that carries it, so the apply cases hold the history
		// row's column to one value everywhere and on redelivery (ADR-0109).
		record.Actor, record.ActorKind = "dev", worklet.AuthorAgent
		record.WorkingFor = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	}
	return json.Marshal(record)
}

// carryingSuiteRecord is a record carrying one versioned field, through the
// production encoder, which stamps it at the field's version.
func carryingSuiteRecord(field statelog.VersionedField) ([]byte, error) {
	at := time.Unix(1_700_000_000, 0).UTC()
	body, err := json.Marshal(worklet.Project{
		V: worklet.DocumentVersion, Key: "carrying", Name: "Suite carrying",
		CreatedAt: at, UpdatedAt: at,
	})
	if err != nil {
		return nil, err
	}
	record := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			OpID: "suite-carrying", Op: worklet.OpCreate,
			Subject:   worklet.Subject{Kind: worklet.KindProject, ID: "carrying"},
			CreatedAt: at, Writer: "suite-node", Scope: worklet.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "suite", ActorKind: worklet.AuthorSystem,
	}
	switch field.Name {
	case "MutationRecord.ActorApps":
		// A CREDENTIAL'S WRITE, whose apps ride beside its login.
		record.Actor, record.ActorKind, record.OperatorID = "token:ci", worklet.AuthorOperator, "token:ci"
		record.ActorApps = []iam.Grant{iam.GrantWorklet}
	case "MutationRecord.ActorAppsKept":
		// A CREDENTIAL'S WRITE WHOSE HISTORY ROW KEEPS ITS APPS.
		record.Actor, record.ActorKind, record.OperatorID = "token:ci", worklet.AuthorOperator, "token:ci"
		record.ActorApps, record.ActorAppsKept = []iam.Grant{iam.GrantWorklet}, true
	case "Snapshot.ByOwner":
		// A CHANGE ITS OWNER MADE, whose lead a build before the rule wakes.
		record.Actor, record.ActorKind = "ada", worklet.AuthorAgent
		record.Notify = &worklet.Notify{Kind: worklet.ChangeCreated,
			Snapshot: worklet.Snapshot{Assignee: "ada", ProjectLead: "lead", ByOwner: true}}
	case "Snapshot.LeftUnassigned":
		// A TASK LEFT UNASSIGNED, whose lead a build before the rule is never
		// offered.
		record.Notify = &worklet.Notify{Kind: worklet.ChangeAssignee,
			Fields:   map[string]worklet.Delta{"assignee": {From: "ada"}},
			Snapshot: worklet.Snapshot{PrevAssignee: "ada", ProjectLead: "lead", LeftUnassigned: true}}
	case "Snapshot.DepartureLead":
		// THE ONE NOTICE A DEPARTURE GIVES ITS LEAD, which a build before the
		// rule never offers.
		record.Notify = &worklet.Notify{Kind: worklet.ChangeAssignee,
			Fields:   map[string]worklet.Delta{"assignee": {From: "ada"}},
			Snapshot: worklet.Snapshot{PrevAssignee: "ada", DepartureLead: "lead", LeftUnassigned: true}}
	case "MutationRecord.NoticeAsksKept":
		// AN AGENT'S COMMENT WHOSE NOTICE ROWS KEEP ITS STATEMENT.
		asks := false
		record.Notify = &worklet.Notify{Kind: worklet.ChangeComment, Mentions: []string{"ada"},
			Snapshot: worklet.Snapshot{CommentAuthorKind: worklet.AuthorAgent, CommentAsks: &asks}}
		record.NoticeAsksKept = true
	case "MutationRecord.NoticeWokeKept":
		// AN AGENT'S COMMENT THAT TELLS AN AGENT IT DOES NOT WAKE, whose notice
		// rows keep that it did not.
		asks := false
		record.Notify = &worklet.Notify{Kind: worklet.ChangeComment, Mentions: []string{"ada"},
			Snapshot: worklet.Snapshot{CommentAuthorKind: worklet.AuthorAgent, CommentAsks: &asks,
				MentionedAgents: []string{"ada"}}}
		record.NoticeWokeKept = true
	case "TaskPatch.ChangeSettled":
		// A PATCH THAT SAYS A PULL REQUEST THE TASK WAITS ON MERGED.
		patch, err := json.Marshal(worklet.TaskPatch{ChangeSettled: &worklet.ChangeSettlement{
			Ref: "https://github.com/acme/web/pull/12", State: worklet.ChangeMerged,
			At: time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC), Source: worklet.SettledByWebhook}})
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
		record.Mutation = patch
	case "TaskPatch.DeadWait", "TaskPatch.DeadWaitEnded":
		// A PATCH THAT SAYS NOTHING MOVES A PULL REQUEST THE TASK WAITS ON, or
		// that something does again.
		ref := "https://github.com/acme/web/pull/12"
		changed := worklet.TaskPatch{DeadWaitEnded: &ref}
		if field.Name == "TaskPatch.DeadWait" {
			changed = worklet.TaskPatch{DeadWait: &worklet.DeadWait{Ref: ref,
				Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				At:   time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC)}}
		}
		patch, err := json.Marshal(changed)
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
		record.Mutation = patch
	case "View.DeletedAt":
		// A VIEW SOMEBODY DELETED: its whole post-state, stamped.
		deleted := time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC)
		view, err := json.Marshal(worklet.View{
			V: worklet.DocumentVersion, ID: "carrying", Name: "Suite carrying",
			Type: worklet.ViewList, Container: worklet.Container{Kind: "workspace"},
			CreatedAt: at, UpdatedAt: deleted, DeletedAt: &deleted, DeletedBy: "ada",
		})
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindView, ID: "carrying"}
		record.Kind, record.Mutation = worklet.ChangeViewDeleted, view
	case "Relation.Kind atlas_entry":
		// A PATCH WHOSE RELATION SET NAMES AN ATLAS ENTRY, by its id.
		relations := []worklet.Relation{{Kind: worklet.RelationEntry,
			Other: "7f3c2a10-5b1e-5c3d-9a4f-0d2e6b8c1a57"}}
		patch, err := json.Marshal(worklet.TaskPatch{Relations: &relations})
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
		record.Mutation = patch
	case "Comment.Act":
		// AN ASK THE ENGINE COMPOSED FOR ONE EXACT COMMAND, carrying its act.
		patch, err := json.Marshal(worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "c-act", Task: "carrying", Author: "ops", AuthorKind: worklet.AuthorAgent, Ask: "ada",
			Act: &delegated.Act{Kind: delegated.KindSSHOperate, Approve: "run-once", Host: "api-1",
				Digest: strings.Repeat("ab", 32)}}})
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
		record.Kind, record.Mutation = worklet.ChangeComment, patch
	case "Comment.Removed":
		// A REMARK TAKEN DOWN BY ITS AUTHOR, its words blanked.
		patch, err := json.Marshal(worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "c-gone", Task: "carrying", Author: "ada", AuthorKind: worklet.AuthorHuman,
			Removed: true}})
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
		record.Kind, record.Mutation = worklet.ChangeCommentRemoved, patch
	case "Comment.Withdrawal":
		// A RESOLVE THAT WITHDRAWS A QUESTION, SAYING WHY AND WHAT COVERS IT.
		patch, err := json.Marshal(worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "c-ask", Task: "carrying", Author: "ada", Ask: "bo", Resolved: true, ResolvedBy: "ada",
			Withdrawal: &worklet.Withdrawal{Reason: "c-why", ReplacedBy: &worklet.Replacement{
				URL: "https://github.com/acme/web/pull/12"}}}})
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
		record.Kind, record.Mutation = worklet.ChangeCommentResolved, patch
	case "TaskPatch.WaitReason":
		// A PATCH THAT SAYS WHY THE TASK WAITS UNTIL ITS START.
		start, reason := time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC), "the vendor's window opens then"
		patch, err := json.Marshal(worklet.TaskPatch{StartAt: &start, WaitReason: &reason})
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
		record.Mutation = patch
	case "TaskPatch.AskWatch":
		// A PATCH THAT CARRIES THE WATCHERS A QUESTION ADDED.
		askWatch := []string{"ada"}
		patch, err := json.Marshal(worklet.TaskPatch{AskWatch: &askWatch})
		if err != nil {
			return nil, err
		}
		record.Op, record.Subject = worklet.OpPatch, worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
		record.Mutation = patch
	case "Snapshot.CommentAsks":
		// AN AGENT'S COMMENT STATING IT ASKS NOTHING, which every build must
		// carry: the statement is what makes its mentions references.
		asks := false
		record.Notify = &worklet.Notify{Kind: worklet.ChangeComment, Mentions: []string{"ada"},
			Snapshot: worklet.Snapshot{CommentAuthorKind: worklet.AuthorAgent, CommentAsks: &asks}}
	case "Snapshot.MentionedAgents":
		record.Notify = &worklet.Notify{Kind: worklet.ChangeComment, Mentions: []string{"ada"},
			Snapshot: worklet.Snapshot{CommentAuthorKind: worklet.AuthorAgent,
				MentionedAgents: []string{"ada"}}}
	case "Snapshot.ThreadAwaiting":
		record.Notify = &worklet.Notify{Kind: worklet.ChangeCreated,
			Snapshot: worklet.Snapshot{ThreadAwaiting: []string{"ada"}}}
	case "Snapshot.ThreadOwing":
		// A ROUTING SNAPSHOT naming who a thread still awaits an answer
		// from, which only a reply reads and every build must carry.
		record.Notify = &worklet.Notify{Kind: worklet.ChangeCreated,
			Snapshot: worklet.Snapshot{ThreadOwing: []string{"ada"}}}
	case "MutationRecord.WorkingFor":
		// AN AGENT'S WRITE THAT WAKES SOMEBODY, made in a turn a person
		// woke: whom it was written for rides beside the agent.
		record.Actor, record.ActorKind = "dev", worklet.AuthorAgent
		record.WorkingFor = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
		record.Notify = &worklet.Notify{Kind: worklet.ChangeCreated}
	case "Project.StuckAfterHours":
		// A PROJECT WHOSE LEAD SAID HOW LONG ITS WORK MAY GO UNCHANGED.
		project, err := json.Marshal(worklet.Project{
			V: worklet.DocumentVersion, Key: "carrying", Name: "Suite carrying",
			StuckAfterHours: 24, CreatedAt: at, UpdatedAt: at,
		})
		if err != nil {
			return nil, err
		}
		record.Mutation = project
	case "Subject.Kind flow", "Snapshot.Flow", "FlowRecord.Past":
		// AN ITEM'S FLOW RECORDED, and a notice of it to its lead, which a
		// build that knows no flow would file under no table.
		at := at
		flow := worklet.FlowRecord{ID: "carrying-flow", Task: "carrying",
			State: worklet.FlowStuck, Since: &at, Seat: "ada"}
		if field.Name == "FlowRecord.Past" {
			// AND ONE DERIVED FROM THE ITEM'S PAST, dated by it, which a
			// build that knows no past would date by its broker.
			flow.Backfilled, flow.Past = true, &worklet.FlowPast{At: at, Seats: true}
		}
		if field.Name == "Snapshot.Flow" {
			flow.Notice, flow.Told = worklet.FlowToldLead, "lead"
			record.Notify = &worklet.Notify{Kind: worklet.ChangeFlow,
				Snapshot: worklet.Snapshot{Key: "CARRYING-1", Task: "carrying",
					Flow: &worklet.FlowNotice{ID: flow.ID, Told: "lead",
						Role: worklet.FlowToldLead, State: worklet.FlowStuck, Since: &at}}}
		}
		body, err := json.Marshal(flow)
		if err != nil {
			return nil, err
		}
		record.Op, record.Mutation = worklet.OpFlow, body
		record.Subject = worklet.FlowSubject("carrying")
		record.Scope = worklet.ScopeSet{Subject: true, Container: "CARRYING"}
	case "TaskPatch.ClearStart", "TaskPatch.ClearDue":
		// A TASK PATCH TAKING A DATE OFF by its flag, which a build that
		// decodes no flag would apply as a patch naming no date.
		patch, err := json.Marshal(worklet.TaskPatch{ClearStart: field.Name == "TaskPatch.ClearStart",
			ClearDue: field.Name == "TaskPatch.ClearDue"})
		if err != nil {
			return nil, err
		}
		record.Op, record.Expect, record.Mutation = worklet.OpPatch, 1, patch
		record.Subject = worklet.Subject{Kind: worklet.KindTask, ID: "carrying"}
	default:
		return nil, fmt.Errorf("no suite record carries %s: add one here with its row", field.Name)
	}
	return record.Encode()
}

// suiteKinds is every kind the domain arbitrates, the apply cases' three
// first: a whole-document object, the task, and a second whole-document
// object with a different table shape.
func suiteKinds() []string {
	first := []worklet.ObjectKind{
		worklet.KindProject, worklet.KindTask, worklet.KindView,
	}
	out := make([]string, 0, len(worklet.ObjectKinds))
	for _, k := range first {
		out = append(out, string(k))
	}
	for _, k := range worklet.ObjectKinds {
		if !k.Arbitrated() || slices.Contains(first, k) {
			continue
		}
		out = append(out, string(k))
	}
	return out
}

// The tracker's gate reader keeps the rule every reader shares (which gate
// is reported, a purge's own id falling through to the eviction, the
// eviction window), certified by the knowledge base's family too, so the
// two agreeing is a test rather than a resemblance.
func TestTheTrackersGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  worklet.Domain{},
				Applier: worklet.NewApplier(nil),
				Encode:  encodeSuiteRecord,
				Kinds:   suiteKinds(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return worklet.NewGates(db) },
			Kind:         string(worklet.KindTask),
			SubjectKinds: kindNames(worklet.ObjectKinds),
			Create: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.TaskSubject(id), worklet.OpCreate,
					writer, opID, newTask(id))
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.TaskSubject(id), worklet.OpPatch,
					writer, opID, worklet.TaskPatch{Title: ptr("edited " + opID)})
			},
			Purge: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.TaskSubject(id), worklet.OpPurge,
					writer, opID, map[string]any{
						"v": worklet.GateRecordVersion, "reason": "the gate suite",
					})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.EvictionSubject(node), worklet.OpEviction,
					writer, opID, gateSuiteEviction(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(worklet.EvictionSubject(node), worklet.OpEviction,
					writer, opID, gateSuiteEviction(node, true))
			},
		}
	})
}

// kindNames is every kind the tracker writes, from the build's own list, so
// a kind added to the domain is one the gate family asks about.
func kindNames(kinds []worklet.ObjectKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// gateSuiteRecord is one record the gate family applies, written by writer.
func gateSuiteRecord(subject worklet.Subject, op worklet.OpKind, writer, opID string,
	payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	scope := worklet.ScopeSet{Subject: true, Container: "ENG"}
	if subject.Kind == worklet.KindEviction {
		scope = worklet.ScopeSet{Subject: true}
	}
	return json.Marshal(worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: writer,
			Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: worklet.AuthorSystem,
	})
}

// gateSuiteEviction is an eviction of node, or its readmission.
func gateSuiteEviction(node string, readmitted bool) worklet.Eviction {
	return worklet.Eviction{
		V: worklet.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: time.Unix(1_700_000_000, 0).UTC(), Readmitted: readmitted,
	}
}
