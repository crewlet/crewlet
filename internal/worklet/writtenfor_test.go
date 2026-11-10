package worklet_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/routing"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT'S WRITE CARRIES WHOM IT WAS WRITTEN FOR (ADR-0109). A task an agent
// files in a turn ada woke records, beside the agent, that the turn worked for
// ada, at the version that carries it, which an older build retains rather
// than waking its assignee for the agent — and so does the same agent's task
// that wakes nobody, since the work pickup reads a creation to learn whom the
// work is for long after it woke anybody. A task a turn nobody woke files
// carries nobody, and stays at the base version every build reads.
//
// AND THE TYPE REFUSES THE ACTOR THAT FORGOT: an agent's writer that states
// nobody it writes for writes nothing, and a person's writer handed a carrier
// writes nothing either, since a person writes for nobody but themselves. An
// actor decoded off the estate's wire states it already, whatever its
// provenance held, so a data node never refuses a write for a key an older
// node never sends: that is the actor's own decoder's, held by
// internal/estate's TestATrackerActorCrossesTheWireAsItWasStated and
// TestAnAgentsWriteFromABuildWithoutTheCarrierRunsAsItsSeatsOwn, and on this
// writer behind a data node by internal/engine's
// TestAnAgentsTrackerWriteFromABuildWithoutTheCarrierRunsForNobody.
//
// Mutation: leave the carrier off the record the writer builds — ada's task
// carries nobody; carry only where a creation wakes somebody — the quiet task
// carries nobody; drop the refusal in recordedFor — the unstated agent's task
// lands.
func TestAnAgentsWriteThatWakesSomebodyCarriesWhomItWasWrittenFor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for _, c := range []struct {
		id         string
		kind       worklet.AuthorKind
		writtenFor types.WrittenFor
		woken      bool
		want       *types.WorkingFor
		version    int
	}{
		{"t-for-ada", worklet.AuthorAgent, types.ForTurn(ada), true, ada, 12},
		{"t-quiet-for-ada", worklet.AuthorAgent, types.ForTurn(ada), false, ada, 12},
		{"t-by-a-turn-nobody-woke", worklet.AuthorAgent, types.ForTurn(nil), true, nil, 1},
	} {
		w := r.writer.As("dev", c.kind, worklet.Provenance{WrittenFor: c.writtenFor})
		task := newTask(c.id)
		task.Key, task.Reporter = "", "dev"
		var wake *worklet.Notify
		if c.woken {
			wake = worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(nil)
		}
		if _, err := w.CreateTask(t.Context(), "op-"+c.id, task, wake); err != nil {
			t.Fatalf("dev files %s: %v", c.id, err)
		}
		r.drain()
		record, err := worklet.Decode(r.lastRecord(t))
		if err != nil {
			t.Fatalf("decode %s: %v", c.id, err)
		}
		if !reflect.DeepEqual(record.WorkingFor, c.want) || record.V != c.version {
			t.Errorf("%s is recorded written for %+v at version %d, want %+v at %d",
				c.id, record.WorkingFor, record.V, c.want, c.version)
		}
	}

	for _, c := range []struct {
		name       string
		kind       worklet.AuthorKind
		writtenFor types.WrittenFor
		cause      error
	}{
		{"an agent that states nobody", worklet.AuthorAgent, types.WrittenFor{}, types.ErrWrittenForUnstated},
		{"a person handed a carrier", worklet.AuthorHuman, types.ForTurn(ada), types.ErrWrittenForNotAnAgents},
	} {
		w := r.writer.As("dev", c.kind, worklet.Provenance{WrittenFor: c.writtenFor})
		task := newTask("t-refused")
		task.Key, task.Reporter = "", "dev"
		_, err := w.CreateTask(t.Context(), "op-refused-"+string(c.kind), task,
			worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(nil))
		if !errors.Is(err, c.cause) {
			t.Errorf("%s files a task: %v, want refused for %v", c.name, err, c.cause)
		}
	}
}

// A RECORD A LATER TURN READS CARRIES WHOM IT WAS WRITTEN FOR, AND NO OTHER DOES
// (ADR-0109), whether the turn is one the record wakes now or a pickup later.
// Every creation and every assignment carries it whatever it wakes — the pickup
// reads an item's creation and its last assignment — an assignment being any
// record that moves the assignee, a status move that takes the item included,
// and a reassertion that moves nothing too. Any other record carries it only
// where its own wake proves an agent other than the writer may start a turn,
// the lead a status move falls back on included, which the parser reaches
// when the one candidate it heard has left the company. The rest stay at the
// version main reads: an agent's comment that asks nothing at 7 where it tells
// somebody without waking them and 6 where nobody else hears it.
//
// Mutation: drop the creation and assignment arm, and the self-assigned
// creation, the reassertion and the self-take carry nobody; decide an
// assignment by its kind alone, and the self-take carries nobody; decide with
// noticeWakes, and the status move that falls back on its lead carries nobody;
// carry on every wake, and the quiet comments and the watch carry ada.
func TestARecordALaterTurnReadsCarriesWhomItWasWrittenFor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ctx := t.Context()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	dev := r.writer.As("dev", worklet.AuthorAgent, worklet.Provenance{WrittenFor: types.ForTurn(ada)})
	last := func(t *testing.T, what string) worklet.MutationRecord {
		t.Helper()
		r.drain()
		record, err := worklet.Decode(r.lastRecord(t))
		if err != nil {
			t.Fatalf("decode %s: %v", what, err)
		}
		return record
	}
	// update writes patch to id as dev, its wake built as the write paths build
	// it from the item before and after, shaped by shape and then by tweak.
	update := func(t *testing.T, op, id string, patch worklet.TaskPatch, kind worklet.ChangeKind,
		leads worklet.Leads, shape func(*worklet.Wake), tweak func(*worklet.Notify)) worklet.MutationRecord {
		t.Helper()
		detail, err := r.reader.Task(ctx, id, worklet.DetailWants{},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		w := worklet.Wake{Kind: kind, Before: detail.Task, After: detail.Task}
		if shape != nil {
			shape(&w)
		}
		n := w.Notify(leads)
		if tweak != nil {
			tweak(n)
		}
		if _, err := dev.UpdateTask(ctx, op, id, "ENG", worklet.NoIfMatch, patch, kind, n); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return last(t, op)
	}
	no := false
	comment := func(op string) (worklet.TaskPatch, func(*worklet.Wake)) {
		c := worklet.Comment{ID: op, Author: "dev", AuthorKind: worklet.AuthorAgent, Body: "noted",
			CreatedAt: wednesday}
		return worklet.TaskPatch{Comment: &c}, func(w *worklet.Wake) {
			c.Task = w.After.ID
			w.Comment, w.Asks = &c, &no
		}
	}

	// THE ITEMS: one ana holds, one held by nobody, dev's own with a watcher
	// who left, and one only dev ever touched.
	fileFor(t, r, "w-ana", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	fileFor(t, r, "w-free", "", worklet.StatusTodo, worklet.PriorityNormal)
	watched := newTask("w-watched")
	watched.Key, watched.Reporter, watched.Assignee = "", "dev", "dev"
	watched.Watchers = []string{"wanda"}
	if _, err := dev.CreateTask(ctx, "op-w-watched", watched, nil); err != nil {
		t.Fatalf("file w-watched: %v", err)
	}
	r.drain()
	own := newTask("w-own")
	own.Key, own.Reporter, own.Assignee = "", "dev", "dev"
	// A SELF-ASSIGNED CREATION THAT WAKES NOBODY: its only candidates are dev.
	if _, err := dev.CreateTask(ctx, "op-w-own", own,
		worklet.Wake{Kind: worklet.ChangeCreated, After: own}.Notify(nil)); err != nil {
		t.Fatalf("file w-own: %v", err)
	}
	created := last(t, "w-own's creation")

	devHandle, anaHandle := "dev", "ana"
	started := worklet.StatusInProgress
	commentPatch, commentShape := comment("c-quiet-ana")
	ownPatch, ownShape := comment("c-quiet-own")
	answerPatch, answerShape := comment("c-answers-qa")
	owingPatch, owingShape := comment("c-owing-qa")
	for _, c := range []struct {
		name    string
		record  func(t *testing.T) worklet.MutationRecord
		want    *types.WorkingFor
		version int
	}{
		{"a self-assigned creation that wakes nobody", func(*testing.T) worklet.MutationRecord { return created },
			ada, 12},
		{"a reassertion of the holder, which moves nothing", func(t *testing.T) worklet.MutationRecord {
			return update(t, "op-reassert", "w-ana", worklet.TaskPatch{Assignee: &anaHandle},
				worklet.ChangeAssignee, nil, nil, nil)
		}, ada, 12},
		{"a self-take recorded as a status move that wakes nobody", func(t *testing.T) worklet.MutationRecord {
			return update(t, "op-take", "w-free", worklet.TaskPatch{Status: &started, Assignee: &devHandle},
				worklet.ChangeStatus, nil, func(w *worklet.Wake) {
					w.After.Status, w.After.StatusGroup, w.After.Assignee = started, started.Group(), "dev"
				}, nil)
		}, ada, 12},
		{"a status move told only to a watcher, falling back on the lead", func(t *testing.T) worklet.MutationRecord {
			record := update(t, "op-start-watched", "w-watched", worklet.TaskPatch{Status: &started},
				worklet.ChangeStatus, fixedLeads{project: "lena"}, func(w *worklet.Wake) {
					w.After.Status, w.After.StatusGroup = started, started.Group()
				}, nil)
			// ROUTED BY A ROSTER WANDA HAS LEFT, it wakes the lead.
			routed := worklet.Route(worklet.Candidates(record.Notify),
				func(h string) bool { return h != "wanda" }, "dev")
			if !slices.ContainsFunc(routed, func(c routing.Candidate[worklet.Reason]) bool {
				return c.Handle == "lena"
			}) {
				t.Errorf("the status move routed without wanda reached %+v, want the lead", routed)
			}
			return record
		}, ada, 12},
		{"a comment that asks nothing and names nobody, on ana's item", func(t *testing.T) worklet.MutationRecord {
			return update(t, "op-c-quiet-ana", "w-ana", commentPatch, worklet.ChangeComment, nil, commentShape, nil)
		}, nil, 7},
		{"a comment that asks nothing, which nobody else hears", func(t *testing.T) worklet.MutationRecord {
			return update(t, "op-c-quiet-own", "w-own", ownPatch, worklet.ChangeComment, nil, ownShape, nil)
		}, nil, 6},
		{"a watch gesture", func(t *testing.T) worklet.MutationRecord {
			return update(t, "op-watch", "w-ana", worklet.TaskPatch{Watch: &worklet.WatchIntent{
				Handle: "designer", Watch: true}}, worklet.ChangeWatchers, nil, func(w *worklet.Wake) {
				w.After.Watchers = append(slices.Clone(w.After.Watchers), "designer")
			}, nil)
		}, nil, 0},
		{"a comment that asks nothing and answers an agent's ask", func(t *testing.T) worklet.MutationRecord {
			return update(t, "op-c-answers-qa", "w-own", answerPatch, worklet.ChangeComment, nil, answerShape,
				func(n *worklet.Notify) { n.Snapshot.AnsweredAuthor = "qa" })
		}, ada, 12},
		{"a reply that asks nothing in a thread that still owes an agent", func(t *testing.T) worklet.MutationRecord {
			return update(t, "op-c-owing-qa", "w-own", owingPatch, worklet.ChangeComment, nil, owingShape,
				func(n *worklet.Notify) { n.Snapshot.ThreadOwing = []string{"qa"} })
		}, ada, 12},
	} {
		record := c.record(t)
		switch {
		case !reflect.DeepEqual(record.WorkingFor, c.want):
			t.Errorf("%s is recorded written for %+v, want %+v", c.name, record.WorkingFor, c.want)
		case c.version != 0 && record.V != c.version:
			t.Errorf("%s is recorded at version %d, want %d", c.name, record.V, c.version)
		case c.want == nil && record.V >= worklet.WorkingForVersion:
			t.Errorf("%s carries nobody and is recorded at version %d, which a build without the carrier "+
				"retains", c.name, record.V)
		}
	}
}
