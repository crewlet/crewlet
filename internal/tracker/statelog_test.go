package tracker_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE TRACKER IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, not by a second one
// written beside it.
//
// The properties every node's copy rests on — the declaration is consistent,
// an unknown version still yields an envelope, the same records produce the
// same rows, and applying a record twice is applying it once — belong to the
// framework and are the same for every domain it carries. A domain-specific
// copy of them would be a second opinion about a contract the framework
// enforces, and the first time the two disagreed the domain's own would be the
// one that ran.
func TestTheTrackerIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  tracker.Domain{},
			Applier: tracker.NewApplier("suite-node"),
			// Migrate is nil: the tracker's tables ship in the
			// replicated estate's own migration, so a fresh store
			// already has them. A domain that created its tables from
			// test code would be a schema the suite proved and the
			// migration did not.
			Encode: encodeSuiteRecord,
			// EVERY ARBITRATED KIND, with the three the apply cases
			// use first. The declaration case checks that a domain
			// publishes every kind it arbitrates — an anchor row for a
			// kind nothing writes is a row nothing ever reads — so a
			// partial list here would report the FIXTURE as the fault.
			Kinds: suiteKinds(),
			// THE PRODUCTION TABLE, and a record carrying each of its
			// fields built through the writer's own encoder — which is
			// what proves each row's path is where that field is
			// actually written.
			Fields:   tracker.VersionedFields(),
			Carrying: carryingSuiteField,
		}
	})
}

// carryingSuiteField builds a valid record carrying exactly one versioned
// field, with its version left for the encoder to stamp.
//
// EVERY FIELD THE TABLE NAMES HAS A CASE, and a field without one fails here
// rather than passing unexamined: the suite reads the version this returns,
// and a record that did not carry the field would be stamped at 1 and reported
// as the path being wrong, which is a fixture fault dressed as a domain one.
func carryingSuiteField(field statelog.VersionedField) ([]byte, error) {
	at := time.Unix(1_700_000_000, 0).UTC()
	spend := tracker.TurnSpend{Turns: 1, Input: 100, Output: 50}
	switch field.Name {
	case "TurnSpend.Workers":
		spend.Workers = 2
	case "TurnSpend.SentBack":
		spend.SentBack = 1
	case "Person.SeenThrough.Generation":
		// A PERSON PAST A REANCHOR: the one record whose position carries
		// a generation the older applier stored as the bare sequence.
		body, err := json.Marshal(tracker.Person{
			V: tracker.DocumentVersion, Handle: "suite-person",
			SeenThrough: tracker.Position{
				Stream: tracker.Domain{}.Stream().Name, Generation: 1, Seq: 2,
			},
			UpdatedAt: at,
		})
		if err != nil {
			return nil, err
		}
		return tracker.MutationRecord{
			RecordEnvelope: tracker.RecordEnvelope{
				OpID: "suite-carrying", Subject: tracker.PersonSubject("suite-person"),
				Op: tracker.OpPatch, CreatedAt: at, Writer: "suite-node",
				Scope: tracker.ScopeSet{Subject: true},
			},
			Kind: tracker.ChangePersonUpdated, Mutation: body,
			Actor: "suite-person", ActorKind: tracker.AuthorHuman,
		}.Encode()
	default:
		return nil, fmt.Errorf("the suite has no record carrying %s — add one "+
			"beside the field's row", field.Name)
	}
	body, err := json.Marshal(tracker.TurnRecord{
		Task: "suite-task", Seat: "dev", TurnID: "run-1", Outcome: "done",
		Phases: []string{"execute"}, Spend: spend,
	})
	if err != nil {
		return nil, err
	}
	return tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			OpID: "suite-carrying", Subject: tracker.TurnSubject("suite-task"),
			Op: tracker.OpTurn, CreatedAt: at, Writer: "suite-node",
			Scope: tracker.ScopeSet{Subject: true, Container: "SUITE"},
		},
		Mutation: body, Actor: "dev", ActorKind: tracker.AuthorAgent,
	}.Encode()
}

// encodeSuiteRecord builds one valid record at an arbitrary version.
//
// IT ENCODES AT ANY VERSION, including one above this build's, because that is
// exactly what a newer peer publishes and what this build has to retain — a
// writer that refused would leave the deferral contract untestable.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	at := time.Unix(1_700_000_000, 0).UTC()
	subject := tracker.Subject{Kind: tracker.ObjectKind(kind), ID: id}

	var payload any
	op := tracker.OpCreate
	switch tracker.ObjectKind(kind) {
	case tracker.KindProject:
		payload = tracker.Project{
			V: tracker.DocumentVersion, Key: id, Name: "Suite " + id,
			CreatedAt: at, UpdatedAt: at,
		}
	case tracker.KindTask:
		payload = tracker.Task{
			V: tracker.DocumentVersion, ID: id, Key: "SUITE-1",
			Project: "SUITE", Type: "task", Title: "suite " + id,
			Status: tracker.StatusTodo, StatusGroup: tracker.GroupNotStarted,
			Priority: tracker.PriorityNone, Rank: tracker.RankOrigin,
			CreatedAt: at, UpdatedAt: at,
		}
	case tracker.KindView:
		payload = tracker.View{
			V: tracker.DocumentVersion, ID: id, Name: "suite " + id,
			Type:      tracker.ViewList,
			Container: tracker.Container{Kind: "workspace", ID: ""},
			CreatedAt: at, UpdatedAt: at,
		}
	default:
		payload = map[string]any{"id": id}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	record := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Gen: 0, Writer: "suite-node",
			Scope: tracker.ScopeSet{Subject: true},
		},
		Mutation:  body,
		Actor:     "suite",
		ActorKind: tracker.AuthorSystem,
	}
	// VERSION ZERO IS THE WRITER'S PATH: the domain's own Encode stamps it,
	// which is what the suite's stamping case reads back. Any other version
	// is ENCODED DIRECTLY, because the suite needs exactly the record a peer
	// at that version would publish — including one above this build's —
	// and not the version this build would have chosen for it.
	if version == 0 {
		return record.Encode()
	}
	return json.Marshal(record)
}

// suiteKinds is every kind the domain arbitrates, ordered so the three the
// apply cases exercise come first: a whole-document object, the task, and a
// second whole-document object with a different table shape.
func suiteKinds() []string {
	first := []tracker.ObjectKind{
		tracker.KindProject, tracker.KindTask, tracker.KindView,
	}
	out := make([]string, 0, len(tracker.ObjectKinds))
	for _, k := range first {
		out = append(out, string(k))
	}
	for _, k := range tracker.ObjectKinds {
		if !k.Arbitrated() || slices.Contains(first, k) {
			continue
		}
		out = append(out, string(k))
	}
	return out
}
