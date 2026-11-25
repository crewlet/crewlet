package worklet_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// projectLeads leads each project by its own seat, and no unit.
type projectLeads map[string]string

func (l projectLeads) ProjectLead(project string) string { return l[project] }
func (l projectLeads) UnitLead(string) string            { return "" }

// THE LEAD'S FALLBACK IS ABOUT OWNERSHIP, NOT AUDIENCE. A change wakes the lead
// in the owner's place only where the work has no owner, the change leaves it
// without one, or its owner cannot hear it; an owner tending its own work only
// tells the lead. Each shape is written through the writer, and the applier's
// notice rows, the wake path's parser and [worklet.Route] must agree on it.
//
// Mutation: drop [worklet.Snapshot.ByOwner] from the fallback's ask, and every
// told shape wakes the lead; stamp it on a move, and the receiving lead is not
// woken; offer no fallback on [worklet.Snapshot.LeftUnassigned], or let the
// previous assignee's notice count as its audience, and unassigned work reaches
// nobody.
func TestTheLeadIsWokenForUnownedWorkAndToldOfAnOwnersOwnChange(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	leads := projectLeads{"ENG": "pm", "OPS": "ops-lead"}
	r.ledBy(leads)
	r.applyWhileWriting()
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops", worklet.ProjectSubject("OPS"), "",
		worklet.Project{V: 1, Key: "OPS", Name: "Operations", CreatedAt: wednesday, UpdatedAt: wednesday},
		worklet.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed OPS: %v", err)
	}
	r.drain()
	dev := r.writer.As("dev", worklet.AuthorAgent,
		worklet.Provenance{WrittenFor: types.ForTurn(&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"})})
	ana := r.writer
	staff := []string{"pm", "ops-lead", "dev", "ana", "bo"}

	tasks := map[string]worklet.Task{}
	file := func(w *worklet.Writer, id, assignee, reporter string) worklet.MutationRecord {
		t.Helper()
		task := newTask(id)
		task.Key, task.Assignee, task.Reporter = "", assignee, reporter
		task.Watchers = []string{reporter}
		from := r.end()
		if _, err := w.CreateTask(t.Context(), "op-"+id, task,
			worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(leads)); err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
		tasks[id] = r.task(t, id).Task
		return r.noticed(from)
	}
	update := func(w *worklet.Writer, id string, kind worklet.ChangeKind, patch worklet.TaskPatch,
		edit func(*worklet.Task), comment *worklet.Comment) worklet.MutationRecord {
		t.Helper()
		before := tasks[id]
		after := before
		edit(&after)
		from := r.end()
		wake := worklet.Wake{Kind: kind, Before: before, After: after, Comment: comment}.Notify(leads)
		if _, err := w.UpdateTask(t.Context(), "op-"+id+"-"+string(kind), id, before.Project,
			worklet.NoIfMatch, patch, kind, wake); err != nil {
			t.Fatalf("%s %s: %v", kind, id, err)
		}
		r.drain()
		tasks[id] = r.task(t, id).Task
		return r.noticed(from)
	}
	comment := func(w *worklet.Writer, id string, kind worklet.AuthorKind) worklet.MutationRecord {
		t.Helper()
		c := &worklet.Comment{ID: "c-" + id + "-" + w.Actor, Task: id, Author: w.Actor,
			AuthorKind: kind, Body: "noted", CreatedAt: wednesday}
		return update(w, id, worklet.ChangeComment, worklet.TaskPatch{Comment: c},
			func(*worklet.Task) {}, c)
	}
	inProgress := worklet.StatusInProgress
	startIt := func(task *worklet.Task) {
		task.Status, task.StatusGroup = worklet.StatusInProgress, worklet.GroupActive
	}
	remove := func(w *worklet.Writer, id string) worklet.MutationRecord {
		t.Helper()
		from := r.end()
		task := tasks[id]
		if _, err := w.RemoveTask(t.Context(), "op-rm-"+id, id, task.Project, false,
			worklet.Wake{Kind: worklet.ChangeRemoved, Before: task, After: task}.Notify(leads)); err != nil {
			t.Fatalf("remove %s: %v", id, err)
		}
		r.drain()
		return r.noticed(from)
	}

	// THE MEASURED SHAPES: told, never woken.
	told := map[string]worklet.MutationRecord{
		"an agent files work for itself":         file(dev, "t-self", "dev", "dev"),
		"a person files work for their own seat": file(ana, "t-ana", "ana", "ana"),
	}
	told["its owner moves its status"] = update(dev, "t-self", worklet.ChangeStatus,
		worklet.TaskPatch{Status: &inProgress}, startIt, nil)
	told["its owner comments on it, naming nobody"] = comment(dev, "t-self", worklet.AuthorAgent)
	told["a person comments on their own task, naming nobody"] = comment(ana, "t-ana", worklet.AuthorHuman)
	file(dev, "t-move", "dev", "dev")
	for name, rec := range told {
		r.hears(name, rec, staff, nil)
		if !rec.Notify.Snapshot.ByOwner || rec.V < worklet.OwnershipVersion {
			t.Errorf("%s is recorded by_owner=%v at version %d; an owner's change says so "+
				"at %d, or an older node applies it waking the lead", name,
				rec.Notify.Snapshot.ByOwner, rec.V, worklet.OwnershipVersion)
		}
		if !r.toldWithoutWaking(rec, "pm") {
			t.Errorf("%s leaves the lead no notice; told, not woken, is still told", name)
		}
	}
	// AND WAKES NO AGENT, so it carries whom it was written for only where
	// every creation does (ADR-0109).
	if rec := told["its owner moves its status"]; rec.WorkingFor != nil {
		t.Errorf("the owner's own status move carries %+v; it wakes no agent", rec.WorkingFor)
	}

	// THE SHAPES THAT MUST STILL WAKE SOMEBODY.
	r.hears("an agent files unowned work", file(dev, "t-loose", "", "dev"), staff, nil, "pm")
	r.hears("a person files work for an agent", file(ana, "t-for-dev", "dev", "ana"), staff, nil, "dev")
	unassign := func(w *worklet.Writer, id string) worklet.MutationRecord {
		t.Helper()
		return update(w, id, worklet.ChangeAssignee, worklet.TaskPatch{Assignee: new(string)},
			func(task *worklet.Task) { task.Assignee = "" }, nil)
	}
	handOff := func(w *worklet.Writer, id, to string) worklet.MutationRecord {
		t.Helper()
		return update(w, id, worklet.ChangeAssignee, worklet.TaskPatch{Assignee: &to},
			func(task *worklet.Task) { task.Assignee = to }, nil)
	}
	// WORK LEFT WITHOUT AN OWNER REACHES THE LEAD, whoever left it so: the
	// previous assignee is told, and no longer owns the work.
	file(ana, "t-taken", "dev", "ana")
	for name, rec := range map[string]worklet.MutationRecord{
		"an owner unassigns itself":         unassign(dev, "t-self"),
		"somebody else unassigns the owner": unassign(ana, "t-taken"),
	} {
		r.hears(name, rec, staff, nil, "pm")
		if !rec.Notify.Snapshot.LeftUnassigned || rec.V < worklet.OwnershipVersion {
			t.Errorf("%s is recorded left_unassigned=%v at version %d, want it at %d", name,
				rec.Notify.Snapshot.LeftUnassigned, rec.V, worklet.OwnershipVersion)
		}
	}
	if !r.toldWithoutWaking(r.noticedOf("t-taken"), "dev") {
		t.Error("the assignee somebody else took off its task is not told")
	}
	// A HAND-OFF LEAVES AN OWNER: only the new assignee is woken.
	file(dev, "t-hand", "dev", "dev")
	file(ana, "t-pass", "dev", "ana")
	for name, rec := range map[string]worklet.MutationRecord{
		"an owner hands its task over":            handOff(dev, "t-hand", "ana"),
		"somebody else hands the owner's task on": handOff(ana, "t-pass", "bo"),
	} {
		want := rec.Notify.Snapshot.Assignee
		r.hears(name, rec, staff, nil, want)
		if s := rec.Notify.Snapshot; s.LeftUnassigned || s.ByOwner {
			t.Errorf("%s is recorded left_unassigned=%v by_owner=%v; it leaves the task an owner",
				name, s.LeftUnassigned, s.ByOwner)
		}
	}
	file(ana, "t-left", "gone", "gone")
	r.hears("a status move on work whose assignee left", update(dev, "t-left", worklet.ChangeStatus,
		worklet.TaskPatch{Status: &inProgress}, startIt, nil), staff, []string{"gone"}, "pm")
	r.hears("a removal by somebody other than its owner", remove(ana, "t-for-dev"), staff, nil, "dev")
	r.hears("its reporter removes unowned work", remove(dev, "t-loose"), staff, nil, "pm")

	// A MOVE INTO ANOTHER PROJECT brings the work to a lead who never heard of
	// it, so the receiving lead is woken whoever owns it.
	from := r.end()
	if _, err := dev.MoveTaskToProject(t.Context(), statelog.NewOpID(time.Now(), "move"), "t-move", "OPS",
		worklet.Wake{Kind: worklet.ChangeMoved, Before: tasks["t-move"],
			After: func() worklet.Task { moved := tasks["t-move"]; moved.Project = "OPS"; return moved }()}.Notify(leads)); err != nil {
		t.Fatalf("move t-move: %v", err)
	}
	r.drain()
	r.hears("an owner moves its task into another project", r.noticed(from), staff, nil, "ops-lead")
}

// hears holds the three readers of rec to one answer: the seats it wakes among
// staff, where left are seats the company no longer employs. The applier holds
// no roster, so a case with left is decided by the wake path alone.
func (r *roundTrip) hears(name string, rec worklet.MutationRecord, staff, left []string, want ...string) {
	r.t.Helper()
	employed := func(handle string) bool { return !slices.Contains(left, handle) }
	among := func(handles []string) []string {
		out := []string{}
		for _, h := range handles {
			if slices.Contains(staff, h) && !slices.Contains(out, h) {
				out = append(out, h)
			}
		}
		slices.Sort(out)
		return out
	}
	slices.Sort(want)
	var routed []string
	for _, c := range worklet.Route(worklet.Candidates(rec.Notify), employed, rec.Actor) {
		routed = append(routed, c.Handle)
	}
	var roster []string
	for _, h := range append(slices.Clone(staff), "ada") {
		if employed(h) {
			roster = append(roster, h)
		}
	}
	parsed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(r.t.Context(),
		delivery(r.t, rec), registry(r.t, roster...))
	if err != nil {
		r.t.Fatalf("%s: Parse: %v", name, err)
	}
	var woken []string
	for _, p := range parsed {
		if !p.Held {
			woken = append(woken, p.To.Handle)
		}
	}
	readers := map[string][]string{"Route": among(routed), "the parser": among(woken)}
	if len(left) == 0 {
		readers["the notice rows"] = among(r.startedTurns(rec))
	}
	for reader, got := range readers {
		if !slices.Equal(got, want) {
			r.t.Errorf("%s: %s wakes %v, want %v", name, reader, got, want)
		}
	}
}

// end is the log's last position.
func (r *roundTrip) end() uint64 {
	r.t.Helper()
	end, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	return end
}

// noticed is the one record after from that carries a notification.
func (r *roundTrip) noticed(from uint64) worklet.MutationRecord {
	r.t.Helper()
	var out []worklet.MutationRecord
	for seq := from + 1; seq <= r.end(); seq++ {
		_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil || !ok {
			r.t.Fatalf("read record %d: %v", seq, err)
		}
		rec, err := worklet.Decode(r.open(seq, payload))
		if err != nil {
			r.t.Fatalf("decode record %d: %v", seq, err)
		}
		if rec.Notify != nil {
			out = append(out, rec)
		}
	}
	if len(out) != 1 {
		r.t.Fatalf("%d records after %d carry a notification, want 1", len(out), from)
	}
	return out[0]
}

// startedTurns is every recipient rec's notice rows say it woke.
func (r *roundTrip) startedTurns(rec worklet.MutationRecord) []string {
	r.t.Helper()
	var out []string
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(r.t.Context(), `SELECT recipient FROM tracker_notifications
			WHERE record_id = ? AND starts_turn = 1 ORDER BY recipient`, rec.OpID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	}); err != nil {
		r.t.Fatalf("read the notice rows of %s: %v", rec.OpID, err)
	}
	return out
}

// noticedOf is the newest record on task that carries a notification.
func (r *roundTrip) noticedOf(task string) worklet.MutationRecord {
	r.t.Helper()
	for seq := r.end(); seq > 0; seq-- {
		_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
		if err != nil || !ok {
			r.t.Fatalf("read record %d: %v", seq, err)
		}
		rec, err := worklet.Decode(r.open(seq, payload))
		if err != nil {
			r.t.Fatalf("decode record %d: %v", seq, err)
		}
		if rec.Notify != nil && rec.Subject.ID == task {
			return rec
		}
	}
	r.t.Fatalf("no record on %s carries a notification", task)
	return worklet.MutationRecord{}
}

// toldWithoutWaking reports a notice row to handle for rec that started no turn.
func (r *roundTrip) toldWithoutWaking(rec worklet.MutationRecord, handle string) bool {
	r.t.Helper()
	var n int
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(), `SELECT COUNT(*) FROM tracker_notifications
			WHERE record_id = ? AND recipient = ? AND starts_turn = 0`, rec.OpID, handle).Scan(&n)
	}); err != nil {
		r.t.Fatalf("read %s's notice of %s: %v", handle, rec.OpID, err)
	}
	return n == 1
}
