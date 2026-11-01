package engine

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE TRACKER APPLIER A RUNNING NODE BUILDS TELLS THE API WHOSE INBOX MOVED.
//
// The tracker says which inboxes a committed batch moved; the API pushes that
// to the sockets watching each seat. Between them is exactly this package — the
// register that builds the applier and the setter the API registers on — and a
// nil handed to the applier there compiles, runs and passes every tracker case,
// while every person's screen goes back to learning about their work a poll
// interval late. So the applier here is the REGISTER's, and the record is
// applied and committed the way the framework's loop does it — and what it
// announces names the tracker, the source a node relays it by while its own
// tracker copy is out of service.
//
// Mutation: drop the tracker's [registration.Inbox] mark, and its applier is
// handed no listener.
func TestTheTrackerApplierTheRegisterBuildsFeedsTheInbox(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	e := &Engine{}
	var heard [][]inbox.Movement
	e.SetOnInboxMoved(func(moved []inbox.Movement) {
		heard = append(heard, moved)
	})
	entry, ok := registrationFor(worklet.Domain{}.Name())
	if !ok {
		t.Fatal("the register has no tracker domain")
	}
	applier, err := entry.NewApplier(&stateLog{nodeID: "node-a", applyHooks: e.applyHooks()})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}

	at := time.Unix(1_700_000_000, 0).UTC()
	task := worklet.Task{
		V: worklet.DocumentVersion, ID: "t-1", Key: "ENG-1", Project: "ENG",
		Type: "task", Title: "a task", Status: worklet.StatusTodo,
		StatusGroup: worklet.GroupNotStarted, Priority: worklet.PriorityNormal,
		Rank: worklet.RankOrigin, CreatedAt: at, UpdatedAt: at,
	}
	title := "assigned"
	for seq, rec := range []worklet.MutationRecord{
		inboxRecord(t, worklet.OpCreate, task, nil),
		inboxRecord(t, worklet.OpPatch, worklet.TaskPatch{Title: &title}, &worklet.Notify{
			Kind:     worklet.ChangeStatus,
			Snapshot: worklet.Snapshot{Key: "ENG-1", Assignee: "bo"},
		}),
	} {
		body, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		record := statelog.Record{
			Envelope: statelog.Envelope{
				V: rec.V, Kind: string(rec.Subject.Kind),
				Subject: statelog.Subject{Kind: string(rec.Subject.Kind), ID: rec.Subject.ID},
				Op:      string(rec.Op), OpID: rec.OpID, Writer: rec.Writer,
			},
			Position: statelog.Position{Stream: worklet.Domain{}.Stream().Name, Seq: uint64(seq + 1)},
			Payload:  body,
			StoredAt: at.Add(time.Duration(seq+1) * time.Second),
		}
		if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := applier.Apply(t.Context(), tx, record, statelog.ApplyOptions{
				Now: record.StoredAt, StoredAt: record.StoredAt,
				MaxVariables: db.Caps().MaxVariables,
			})
			return err
		}); err != nil {
			t.Fatalf("apply %s: %v", rec.Op, err)
		}
		applier.Committed(t.Context())
	}

	// THE PATCH'S OWN BROKER INSTANT rides the movement, which the
	// socket's push is timed from.
	want := []inbox.Movement{{Source: worklet.Domain{}.Name(), Handle: "bo",
		UnreadDelta: 1, Subject: "t-1", Reason: string(worklet.ReasonAssignee),
		At: at.Add(2 * time.Second)}}
	if len(heard) != 1 || !slices.Equal(heard[0], want) {
		t.Fatalf("the API heard %v, want exactly one batch %v", heard, want)
	}
}

// THE RELAY JUDGES EXACTLY THE DOMAINS WHOSE APPLIERS ANNOUNCE.
//
// A domain's applier is handed the engine's inbox listener only where the
// register marks it ([stateLog.inboxFor]), and the live relay judges the
// copies of exactly the marked domains among the ones whose movements it
// relays ([liveDomains], over [inboxSources]). Held apart, the two could
// disagree in the two ways that cost: a domain announcing movements the relay
// never judged keeps its sockets on an applier nobody vouches for while its
// copy is wrong, and a judged one that announces nothing is relayed for
// nobody. The tracker is one, because a person's work is its news.
//
// Mutation: hand the listener to every domain's applier, or list the relay's
// sources by anything but the mark, and a domain is one and not the other.
func TestTheRelayJudgesExactlyTheDomainsWhoseAppliersAnnounce(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.SetOnInboxMoved(func([]inbox.Movement) {})
	s := &stateLog{applyHooks: e.applyHooks()}
	sources := inboxSources()
	if !slices.Contains(sources, worklet.Domain{}.Name()) {
		t.Fatalf("the relay's sources are %v, and the tracker is not among them", sources)
	}
	for _, source := range sources {
		if !slices.Contains(liveDomains(), source) {
			t.Errorf("%s announces inbox movements and the live relay does not "+
				"judge its copy (it judges %v)", source, liveDomains())
		}
	}
	for _, d := range registeredDomains() {
		handed := s.inboxFor(d) != nil
		if judged := slices.Contains(sources, d.Name()); handed != judged {
			t.Errorf("%s: its applier is handed the inbox listener = %v, and the "+
				"relay judges its copy = %v", d.Name(), handed, judged)
		}
	}
}

// inboxRecord is one tracker record about task t-1.
func inboxRecord(t *testing.T, op worklet.OpKind, payload any,
	notify *worklet.Notify) worklet.MutationRecord {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "t-1-" + string(op),
			Subject: worklet.TaskSubject("t-1"), Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Writer:    "node-a", Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: body, Actor: "ana", ActorKind: worklet.AuthorHuman,
		Notify: notify,
	}
}
