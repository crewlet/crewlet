package tracker_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A DEPENDENCY IS WRITTEN AT BOTH ENDS, and until this existed neither end
// could be written at all.
//
// `waiting_on` had no producer in the whole tree: `maintainDeps` derived the
// dependency table from it, the unblock duty scanned that table, `blocked`
// filtered on it and `Snapshot.Unblocked` routed off it — an entire subsystem
// over a relation kind nothing could author.
func TestADependencyIsWrittenAtBothEnds(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "dep", nil)
	inSprint(t, r, "blk", nil)

	result, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
		Note: "needs the schema first",
	}, fixedLeads{project: "eng-lead"})
	if err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()
	if len(result.OneSided) != 0 {
		t.Fatalf("the mirror did not land: %v", result.OneSided)
	}

	// THE AUTHORED EDGE, on the dependent.
	dep := r.task(t, "dep")
	if !slices.ContainsFunc(dep.Task.Relations, func(rel tracker.Relation) bool {
		return rel.Kind == tracker.RelationWaitingOn && rel.Other == "blk"
	}) {
		t.Fatalf("the dependent carries %+v and no waiting_on edge",
			dep.Task.Relations)
	}
	// THE MIRROR, on the blocker.
	blk := r.task(t, "blk")
	if !slices.Contains(blk.Task.Dependents, "dep") {
		t.Fatalf("the blocker lists %v as its dependents, want the one that "+
			"waits on it — without the mirror a close cannot name who it "+
			"unblocks", blk.Task.Dependents)
	}
	// AND THE DERIVED EDGE, which is what `blocked` and the unblock duty
	// read: neither end's document is what those queries touch.
	if !dep.Blocked {
		t.Error("the dependent is not blocked, so the dependency table has no " +
			"row — every filter and the unblock repair read that table")
	}
	// THE EDGE IS WHOLE, so nothing for the repair duty to find.
	if flagged := r.flagged(t, "one_sided"); len(flagged) != 0 {
		t.Errorf("a mirrored edge is flagged one-sided: %v", flagged)
	}
}

// THE BLOCKER'S ASSIGNEE IS TOLD, and this is the wake that had no producer.
//
// `Snapshot.Dependents` is the sole input of the `blocking` arm, and the
// authored commit routes to the DEPENDENT's parties — so "the wake went out
// with the other commit" was never true for the person who now owes somebody
// their work.
func TestTheBlockersAssigneeIsToldSomebodyWaits(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "dep", nil)
	blocker := newTask("blk")
	blocker.Key, blocker.Assignee = "ENG-blk", "bo"
	if _, err := r.writer.CreateTask(t.Context(), "op-blk", blocker, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	told := false
	for _, notify := range r.wakes(t) {
		if len(notify.Snapshot.Dependents) == 0 {
			continue
		}
		for _, candidate := range tracker.Candidates(notify, false) {
			if candidate.Handle == "bo" && candidate.Reason == tracker.ReasonBlocking {
				told = true
			}
		}
	}
	if !told {
		t.Error("the blocker's assignee was never a `blocking` candidate — " +
			"somebody's work now waits on theirs and nothing told them")
	}
}

// A BLOCKER THAT CANNOT TAKE THE EDGE REFUSES IT BEFORE ANYTHING IS PUBLISHED.
//
// The order is what makes this possible: every counterparty is read before the
// first append, so a refusal leaves NO edge rather than an authored one whose
// mirror can never be written.
func TestABlockerThatCannotTakeTheEdgeRefusesItFirst(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "dep", nil)

	_, err := r.writer.Depend(t.Context(), "op-ghost", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"nope"},
	}, fixedLeads{})
	if err == nil {
		t.Fatal("a dependency on a task that does not exist was written — the " +
			"edge would resolve to nothing on every node for ever")
	}
	if dep := r.task(t, "dep"); len(dep.Task.Relations) != 0 {
		t.Errorf("the refused call still left %+v on the dependent",
			dep.Task.Relations)
	}
	// AND A SELF-EDGE, which the dependency table would read as
	// permanently blocked and no close could ever clear.
	if _, err := r.writer.Depend(t.Context(), "op-self", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"dep"},
	}, fixedLeads{}); err == nil {
		t.Error("a task was made to wait on itself")
	}
}

// AN UNMIRRORED EDGE IS FLAGGED, AND THE DUTY WRITES WHAT DID NOT LAND.
//
// The flag had no producer: it was carried through from whatever the record
// said, and no record ever said anything — so the partial index the schema
// ships "for the relations repair duty's selection" was over a column that was
// zero on every row in the company.
func TestAnUnmirroredEdgeIsFlaggedAndRepaired(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "dep", nil)
	blocker := newTask("blk")
	blocker.Key, blocker.Assignee = "ENG-blk", "bo"
	if _, err := r.writer.CreateTask(t.Context(), "op-blk", blocker, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	// THE AUTHORED HALF ALONE, which is exactly the residue a gesture
	// leaves when its mirror step did not run.
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Relate: &tracker.RelationIntent{
			Add: []tracker.Relation{{Kind: tracker.RelationWaitingOn, Other: "blk"}},
		}}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	r.drain()

	flagged := r.flagged(t, "one_sided")
	if !slices.Contains(flagged, "dep") {
		t.Fatalf("the half-written edge is not flagged one-sided: %v — the "+
			"repair duty selects on exactly this and would never see it",
			flagged)
	}
	// AND THE ATTENTION QUEUE FINDS IT, which is the surface a person has.
	found, err := r.reader.Tasks(t.Context(), tracker.Query{
		Scope: tracker.Scope{Workspace: true}, Flags: []string{"one_sided"}, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("the attention query: %v", err)
	}
	if len(found.Rows) != 1 || found.Rows[0].ID != "dep" {
		t.Errorf("flag=one_sided answered %d rows, want the half-written one",
			len(found.Rows))
	}

	// THE REPAIR WRITES THE MISSING COMMIT.
	edges, err := tracker.ScanOneSided(t.Context(), r.db, wednesday.AddDate(1, 0, 0), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("the scan found %d edges, want the one", len(edges))
	}
	if _, err := r.writer.RepairOneSided(t.Context(), "op-repair", edges[0],
		fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("RepairOneSided: %v", err)
	}
	r.drain()

	if blk := r.task(t, "blk"); !slices.Contains(blk.Task.Dependents, "dep") {
		t.Errorf("the repair did not write the mirror: %v", blk.Task.Dependents)
	}
	// AND THE FLAG CLEARS, which is the half a repair that only wrote the
	// mirror would leave: without the blocker-side re-judge the duty would
	// repair this edge for ever, writing a mirror that is already there.
	if flagged := r.flagged(t, "one_sided"); len(flagged) != 0 {
		t.Errorf("the edge is still flagged after its mirror landed: %v — the "+
			"duty would repair it on every tick, for ever", flagged)
	}
	// THE REPAIR IS LOUD, because the blocker's assignee was never told at
	// all: the authored commit routed to the DEPENDENT's parties.
	late := false
	for _, notify := range r.wakes(t) {
		if notify.Late && len(notify.Snapshot.Dependents) > 0 {
			late = true
		}
	}
	if !late {
		t.Error("the repair was quiet, so the blocker's assignee is never told " +
			"— and that commit is the only wake that side gets")
	}
}

// A MIRROR THAT CAN NEVER BE WRITTEN IS STAMPED, not retried for ever.
func TestAMirrorThatCanNeverLandIsStampedFinal(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "dep", nil)
	inSprint(t, r, "blk", nil)
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Relate: &tracker.RelationIntent{
			Add: []tracker.Relation{{Kind: tracker.RelationWaitingOn, Other: "blk"}},
		}}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "blk", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	edges, err := tracker.ScanOneSided(t.Context(), r.db, wednesday.AddDate(1, 0, 0), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("the scan found %d edges, want the one", len(edges))
	}
	reason, final := edges[0].Final()
	if !final {
		t.Fatalf("an edge whose blocker was removed is not final, so the duty "+
			"retries it for ever: %+v", edges[0])
	}
	if !strings.Contains(reason, "removed") {
		t.Errorf("the reason is %q and does not say what a person has to fix", reason)
	}
	if _, err := r.writer.RepairOneSided(t.Context(), "op-final", edges[0],
		fixedLeads{}); err != nil {
		t.Fatalf("RepairOneSided: %v", err)
	}
	r.drain()

	// AND IT IS OUT OF THE REPAIR'S SELECTION but IN the attention queue,
	// which is the whole distinction between the two flags.
	again, err := tracker.ScanOneSided(t.Context(), r.db, wednesday.AddDate(1, 0, 0), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the duty still selects a permanently one-sided edge: %+v", again)
	}
	found, err := r.reader.Tasks(t.Context(), tracker.Query{
		Scope: tracker.Scope{Workspace: true}, Flags: []string{"one_sided_final"}, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("the attention query: %v", err)
	}
	if len(found.Rows) != 1 {
		t.Errorf("flag=one_sided_final answered %d rows, want the one a person "+
			"has to resolve", len(found.Rows))
	}
}

// THE ATTENTION FLAGS OR RATHER THAN AND.
//
// Each flag was ANDed with the last, so the screen the design specifies —
// which names every flag there is and expects the tasks carrying any of them —
// answered nothing, on every company, and looked exactly like a company with
// nothing wrong.
func TestTheAttentionFlagsMatchAnyNotAll(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "dep", nil)
	inSprint(t, r, "blk", nil)
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Relate: &tracker.RelationIntent{
			Add: []tracker.Relation{{Kind: tracker.RelationWaitingOn, Other: "blk"}},
		}}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	r.drain()

	found, err := r.reader.Tasks(t.Context(), tracker.Query{
		Scope: tracker.Scope{Workspace: true}, Flags: tracker.AttentionFlags, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("the attention query: %v", err)
	}
	if len(found.Rows) != 1 {
		t.Fatalf("the whole attention queue answered %d rows and one task is "+
			"one-sided — with a conjunction this screen answers nothing, ever",
			len(found.Rows))
	}
	// AND AN UNKNOWN FLAG IS STILL REFUSED NAMING THE SET.
	_, err = r.reader.Tasks(t.Context(), tracker.Query{
		Scope: tracker.Scope{Workspace: true}, Flags: []string{"nope"}, Level: statelog.ReadStale,
	}, wednesday)
	if err == nil {
		t.Fatal("an unknown attention flag was accepted")
	}
	for _, want := range []string{"one_sided", "cycle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q and does not name %q", err, want)
		}
	}
}

// THE CAPS ARE ENFORCED WHERE THE SET GROWS BY ONE, which is the only place a
// cap over a whole-carried collection can be.
func TestTheRelationCapsAreEnforced(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "t-1", nil)

	links := make([]tracker.Relation, 0, tracker.MaxOtherRelations+1)
	for i := range cap(links) {
		links = append(links, tracker.Relation{
			Kind: tracker.RelationLinked, Other: "other-" + itoa(i),
		})
	}
	_, err := r.writer.UpdateTask(t.Context(), "op-cap", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Set: links},
		}, tracker.ChangeRelations, nil)
	if err == nil {
		t.Fatalf("a task took %d links and the maximum is %d",
			len(links), tracker.MaxOtherRelations)
	}
	if !strings.Contains(err.Error(), itoa(tracker.MaxOtherRelations)) {
		t.Errorf("the refusal is %q and does not name the cap", err)
	}
	// AND A WHOLE SET AND A DELTA AT ONCE IS A PROGRAMMING ERROR, refused
	// rather than resolved in some order.
	if _, err := r.writer.UpdateTask(t.Context(), "op-both", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate:    &tracker.RelationIntent{Add: links[:1]},
			Relations: &links,
		}, tracker.ChangeRelations, nil); err == nil {
		t.Error("a patch carrying both a gesture and a whole set was accepted")
	}
}

// AN EDGE STATED TWICE IS ONE EDGE, because a retried turn re-states its own.
func TestRestatingAnEdgeIsNotASecondEdge(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "t-1", nil)
	inSprint(t, r, "t-2", nil)
	for i := range 2 {
		if _, err := r.writer.UpdateTask(t.Context(), "op-link"+itoa(i), "t-1", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Relate: &tracker.RelationIntent{
				Add: []tracker.Relation{{Kind: tracker.RelationLinked, Other: "t-2"}},
			}}, tracker.ChangeRelations, nil); err != nil {
			t.Fatalf("link: %v", err)
		}
		r.drain()
	}
	if got := r.task(t, "t-1").Task.Relations; len(got) != 1 {
		t.Errorf("the task carries %d relations after one edge was stated "+
			"twice: %+v", len(got), got)
	}
}

// task reads one task back, with its links and the derived blocked flag.
func (r *roundTrip) task(t *testing.T, id string) tracker.TaskDetail {
	t.Helper()
	got, err := r.reader.Task(t.Context(), id, tracker.DetailWants{Links: true},
		statelog.ReadStale)
	if err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return got
}

// flagged is the tasks carrying one attention flag on a relation of theirs.
//
// READ FROM THE ROW rather than from the query, so a case can tell an
// unflagged edge from a query that does not compile the flag.
func (r *roundTrip) flagged(t *testing.T, column string) []string {
	t.Helper()
	return r.strings(`SELECT DISTINCT task_id FROM tracker_relations
		WHERE ` + column + ` = 1 AND kind = 'waiting_on' ORDER BY task_id`)
}

// wakes is every notification on the log, oldest first.
//
// OFF THE LOG rather than out of a return value, for the reason [lastWake]
// gives: the wake is what TRAVELS, and a sequence writes several records of
// which only one carries the notification a case is about.
func (r *roundTrip) wakes(t *testing.T) []*tracker.Notify {
	t.Helper()
	last, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	var out []*tracker.Notify
	for seq := uint64(1); seq <= last; seq++ {
		_, payload, _, ok, err := r.log.At(t.Context(), seq)
		if err != nil || !ok {
			continue
		}
		record, err := tracker.Decode(payload)
		if err != nil || record.Notify == nil {
			continue
		}
		out = append(out, record.Notify)
	}
	return out
}

// A `blocking`-ONLY CHANGE RETURNS THE POSITION IT WROTE AT.
//
// The authored commits for that direction are on the COUNTERPARTIES' subjects
// and the mirror is this task's own, so nothing is published on the subject
// the caller named. A result carrying the zero position would make the
// caller's own barrier a no-op, and its next read would come back missing the
// write it just made.
func TestABlockingOnlyChangeReturnsAPosition(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "blk", nil)
	inSprint(t, r, "dep", nil)

	result, err := r.writer.Depend(t.Context(), "op-blocking", tracker.DependencyChange{
		Task: "blk", Project: "ENG", BlockingAdd: []string{"dep"},
	}, fixedLeads{})
	if err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()
	if result.Position.Seq == 0 {
		t.Fatalf("the call answered position %+v — a caller settling on it "+
			"barriers at nothing and reads back its own write missing",
			result.Position)
	}
	// AND BOTH ENDS ARE STILL WRITTEN, from this direction too.
	if dep := r.task(t, "dep"); !slices.ContainsFunc(dep.Task.Relations,
		func(rel tracker.Relation) bool {
			return rel.Kind == tracker.RelationWaitingOn && rel.Other == "blk"
		}) {
		t.Errorf("the dependent carries %+v and no authored edge",
			dep.Task.Relations)
	}
	if blk := r.task(t, "blk"); !slices.Contains(blk.Task.Dependents, "dep") {
		t.Errorf("the blocker lists %v as its dependents", blk.Task.Dependents)
	}
	if flagged := r.flagged(t, "one_sided"); len(flagged) != 0 {
		t.Errorf("a whole edge written from the blocking side is flagged: %v",
			flagged)
	}
}

// THE OPEN-ASK INFERENCE IS INDEX-SERVED, and the term that makes it so is
// one a reader would delete as redundant.
//
// `tracker_comments_open_asks_idx` is PARTIAL — `WHERE ask <> ” AND
// resolved = 0 AND answered_by IS NULL AND removed = 0` — and a planner
// matches a partial index by SYNTACTIC implication. `ask = ?` does not imply
// `ask <> ”` to it, because a bound parameter could be the empty string, so
// the query states the literal term as well and reads a seekable range rather
// than every comment on the task.
func TestTheOpenAskInferenceIsIndexServed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	var plan string
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `
			EXPLAIN QUERY PLAN
			SELECT id, author, substr(body, 1, 120)
			FROM tracker_comments
			WHERE task_id = 't' AND ask = 'a' AND ask <> '' AND resolved = 0
			  AND answered_by IS NULL AND removed = 0
			ORDER BY created_at, id
			LIMIT 6`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				return err
			}
			plan += detail + " "
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("explain the ask inference: %v", err)
	}
	if !strings.Contains(plan, "tracker_comments_open_asks_idx") {
		t.Errorf("the plan is %q and does not name the open-asks index — every "+
			"comment on a task would pay for the length of its own thread",
			plan)
	}
}

// THE REPAIR AGES ON THE EDGE, not on the task.
//
// The only other column it could have aged on is the dependent's own
// `updated_at`, which any unrelated edit resets — so a busy task would
// postpone its own repair indefinitely, and the busiest tasks are exactly the
// ones that acquire dependencies.
func TestTheRepairAgesOnTheEdgeNotTheTask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "dep", nil)
	inSprint(t, r, "blk", nil)

	// AN EDGE AUTHORED LONG AGO, on a task edited just now.
	old := wednesday.AddDate(0, 0, -7)
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Relate: &tracker.RelationIntent{
			Add: []tracker.Relation{{
				Kind: tracker.RelationWaitingOn, Other: "blk", CreatedAt: old,
			}},
		}}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	r.drain()
	title := "touched since"
	if _, err := r.writer.UpdateTask(t.Context(), "op-touch", "dep", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: &title},
		tracker.ChangeFields, nil); err != nil {
		t.Fatalf("touch the task: %v", err)
	}
	r.drain()

	// THE HORIZON IS BETWEEN THE TWO: older than the edge, newer than the
	// touch. Aged on the task this finds nothing; aged on the edge it
	// finds the one that needs repairing.
	edges, err := tracker.ScanOneSided(t.Context(), r.db, old.AddDate(0, 0, 1), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("the scan found %d edges — an edge authored a week ago on a "+
			"task edited since is one the duty must still repair", len(edges))
	}
	// AND A FRESH EDGE IS NOT YET THE DUTY'S, so it never races a gesture
	// still running.
	fresh, err := tracker.ScanOneSided(t.Context(), r.db, old.AddDate(0, 0, -1), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("the scan took %d edges from before its own horizon", len(fresh))
	}
}
