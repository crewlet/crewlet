package tracker_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

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
		}
	})
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
	// ENCODED DIRECTLY rather than through the writer's own Encode, which
	// refuses a version it does not write — the suite needs exactly that
	// record.
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
