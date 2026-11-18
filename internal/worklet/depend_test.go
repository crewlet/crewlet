package worklet_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A DEPENDENCY IS WRITTEN AT BOTH ENDS: the authored `waiting_on` edge, the
// blocker's mirror, and the derived row `maintainDeps` writes for `blocked` and
// the unblock duty.
func TestADependencyIsWrittenAtBothEnds(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	result, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
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
	if !slices.ContainsFunc(dep.Task.Relations, func(rel worklet.Relation) bool {
		return rel.Kind == worklet.RelationWaitingOn && rel.Other == "blk"
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

// THE BLOCKER'S ASSIGNEE IS TOLD, by the mirror: `Snapshot.Dependents` is the
// sole input of the `blocking` arm, and the authored commit routes to the
// DEPENDENT's parties.
func TestTheBlockersAssigneeIsToldSomebodyWaits(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	blocker := newTask("blk")
	blocker.Key, blocker.Assignee = "ENG-blk", "bo"
	if _, err := r.writer.CreateTask(t.Context(), "op-blk", blocker, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
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
		for _, candidate := range worklet.Candidates(notify) {
			if candidate.Handle == "bo" && candidate.Reason == worklet.ReasonBlocking {
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
	filedTask(t, r, "dep")

	_, err := r.writer.Depend(t.Context(), "op-ghost", worklet.DependencyChange{
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
	if _, err := r.writer.Depend(t.Context(), "op-self", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"dep"},
	}, fixedLeads{}); err == nil {
		t.Error("a task was made to wait on itself")
	}
}

// AN UNMIRRORED EDGE IS FLAGGED `one_sided`, AND THE DUTY WRITES WHAT DID NOT
// LAND: the flag is the column the repair duty's partial index selects on.
func TestAnUnmirroredEdgeIsFlaggedAndRepaired(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	blocker := newTask("blk")
	blocker.Key, blocker.Assignee = "ENG-blk", "bo"
	if _, err := r.writer.CreateTask(t.Context(), "op-blk", blocker, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	// THE AUTHORED HALF ALONE, which is exactly the residue a gesture
	// leaves when its mirror step did not run.
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
			Add: []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "blk"}},
		}}, worklet.ChangeRelations, nil); err != nil {
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
	found, err := r.reader.Tasks(t.Context(), worklet.Query{
		Scope: worklet.Scope{Workspace: true}, Flags: []string{"one_sided"}, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("the attention query: %v", err)
	}
	if len(found.Rows) != 1 || found.Rows[0].ID != "dep" {
		t.Errorf("flag=one_sided answered %d rows, want the half-written one",
			len(found.Rows))
	}

	// THE REPAIR WRITES THE MISSING COMMIT.
	edges, err := worklet.ScanOneSided(t.Context(), r.db.Reader(), wednesday.AddDate(1, 0, 0), 16)
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
	// AND THE FLAG CLEARS: without the blocker-side re-judge the duty would
	// repair this edge for ever, rewriting a mirror already there.
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
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
			Add: []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "blk"}},
		}}, worklet.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "blk", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	edges, err := worklet.ScanOneSided(t.Context(), r.db.Reader(), wednesday.AddDate(1, 0, 0), 16)
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
	again, err := worklet.ScanOneSided(t.Context(), r.db.Reader(), wednesday.AddDate(1, 0, 0), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the duty still selects a permanently one-sided edge: %+v", again)
	}
	found, err := r.reader.Tasks(t.Context(), worklet.Query{
		Scope: worklet.Scope{Workspace: true}, Flags: []string{"one_sided_final"}, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("the attention query: %v", err)
	}
	if len(found.Rows) != 1 {
		t.Errorf("flag=one_sided_final answered %d rows, want the one a person "+
			"has to resolve", len(found.Rows))
	}
}

// THE ONE-SIDED SCAN COUNTS EACH BLOCKER'S OWN DEPENDENTS.
//
// The count decides whether an unmirrored edge is permanently one-sided (at
// [worklet.MaxDependents]). The scan reads it in one grouped read of the
// batch's blockers, so each edge must get its OWN blocker's count: two blockers
// with different counts, each missing a mirror, tell a count filed under the
// wrong task, or under nobody, from the right one.
//
// Mutation: file the counts under each edge's dependent rather than its
// blocker, or drop the fill, and both edges read zero.
func TestTheOneSidedScanCountsEachBlockersDependents(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"blka", "blkb", "d1", "d2", "d3", "halfa", "halfb"} {
		filedTask(t, r, id)
	}
	// TWO MIRRORED DEPENDENTS ON ONE BLOCKER AND ONE ON THE OTHER, through
	// the whole gesture.
	for i, edge := range [][2]string{{"d1", "blka"}, {"d2", "blka"}, {"d3", "blkb"}} {
		if _, err := r.writer.Depend(t.Context(), "op-dep-"+itoa(i), worklet.DependencyChange{
			Task: edge[0], Project: "ENG", WaitingOnAdd: []string{edge[1]},
		}, fixedLeads{}); err != nil {
			t.Fatalf("Depend %s on %s: %v", edge[0], edge[1], err)
		}
		r.drain()
	}
	// AND ONE AUTHORED HALF ON EACH, whose mirror never landed.
	for _, edge := range [][2]string{{"halfa", "blka"}, {"halfb", "blkb"}} {
		if _, err := r.writer.UpdateTask(t.Context(), "op-half-"+edge[0], edge[0], "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
				Add: []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: edge[1]}},
			}}, worklet.ChangeRelations, nil); err != nil {
			t.Fatalf("write the authored half of %s: %v", edge[0], err)
		}
		r.drain()
	}

	edges, err := worklet.ScanOneSided(t.Context(), r.db.Reader(), wednesday.AddDate(1, 0, 0), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	got := map[string]int{}
	for _, edge := range edges {
		got[edge.Dependent+"->"+edge.Blocker] = edge.BlockerDependents
	}
	want := map[string]int{"halfa->blka": 2, "halfb->blkb": 1}
	if !maps.Equal(got, want) {
		t.Errorf("the scan answered %v, want each one-sided edge with its own "+
			"blocker's listed dependents %v", got, want)
	}
}

// THE ATTENTION FLAGS OR RATHER THAN AND: the screen names every flag and wants
// the tasks carrying any, and ANDed it answers nothing, which looks exactly
// like a company with nothing wrong.
func TestTheAttentionFlagsMatchAnyNotAll(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
			Add: []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "blk"}},
		}}, worklet.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	r.drain()

	found, err := r.reader.Tasks(t.Context(), worklet.Query{
		Scope: worklet.Scope{Workspace: true}, Flags: worklet.AttentionFlags, Level: statelog.ReadStale,
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
	_, err = r.reader.Tasks(t.Context(), worklet.Query{
		Scope: worklet.Scope{Workspace: true}, Flags: []string{"nope"}, Level: statelog.ReadStale,
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
	filedTask(t, r, "t-1")

	links := make([]worklet.Relation, 0, worklet.MaxOtherRelations+1)
	for i := range cap(links) {
		links = append(links, worklet.Relation{
			Kind: worklet.RelationLinked, Other: "other-" + itoa(i),
		})
	}
	_, err := r.writer.UpdateTask(t.Context(), "op-cap", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Relate: &worklet.RelationIntent{Set: links},
		}, worklet.ChangeRelations, nil)
	if err == nil {
		t.Fatalf("a task took %d links and the maximum is %d",
			len(links), worklet.MaxOtherRelations)
	}
	if !strings.Contains(err.Error(), itoa(worklet.MaxOtherRelations)) {
		t.Errorf("the refusal is %q and does not name the cap", err)
	}
	// AND A WHOLE SET AND A DELTA AT ONCE IS A PROGRAMMING ERROR, refused
	// rather than resolved in some order.
	if _, err := r.writer.UpdateTask(t.Context(), "op-both", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Relate:    &worklet.RelationIntent{Add: links[:1]},
			Relations: &links,
		}, worklet.ChangeRelations, nil); err == nil {
		t.Error("a patch carrying both a gesture and a whole set was accepted")
	}
}

// AN EDGE STATED TWICE IS ONE EDGE, because a retried turn re-states its own.
func TestRestatingAnEdgeIsNotASecondEdge(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	for i := range 2 {
		if _, err := r.writer.UpdateTask(t.Context(), "op-link"+itoa(i), "t-1", "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
				Add: []worklet.Relation{{Kind: worklet.RelationLinked, Other: "t-2"}},
			}}, worklet.ChangeRelations, nil); err != nil {
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
func (r *roundTrip) task(t *testing.T, id string) worklet.TaskDetail {
	t.Helper()
	got, err := r.reader.Task(t.Context(), id, worklet.DetailWants{Links: true},
		statelog.Freshness{Level: statelog.ReadStale})
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
func (r *roundTrip) wakes(t *testing.T) []*worklet.Notify {
	t.Helper()
	last, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	var out []*worklet.Notify
	for seq := uint64(1); seq <= last; seq++ {
		_, payload, _, ok, err := r.log.At(t.Context(), seq)
		if err != nil || !ok {
			continue
		}
		record, err := worklet.Decode(r.open(seq, payload))
		if err != nil || record.Notify == nil {
			continue
		}
		out = append(out, record.Notify)
	}
	return out
}

// A `blocking`-ONLY CHANGE RETURNS THE POSITION IT WROTE AT.
//
// Its authored commits are on the COUNTERPARTIES' subjects, before its own
// mirror; a zero position would make the caller's barrier a no-op, and its
// next read would miss the write it just made.
func TestABlockingOnlyChangeReturnsAPosition(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "blk")
	filedTask(t, r, "dep")

	result, err := r.writer.Depend(t.Context(), "op-blocking", worklet.DependencyChange{
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
		func(rel worklet.Relation) bool {
			return rel.Kind == worklet.RelationWaitingOn && rel.Other == "blk"
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

// A DEPENDENCY CHANGE ANSWERS AT ITS LAST COMMIT, end to end.
//
// A waiting-on change's mirror is its later record, so a read barriered at the
// authored commit misses the blocker's side. The per-commit folding is pinned
// in TestADependencyResultFoldsEveryCommit, since this harness answers every
// commit `pending` until it drains.
func TestADependencyAnswersAtItsLastCommit(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	result, err := r.writer.Depend(t.Context(), "op-every", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{})
	if err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()
	blk := r.task(t, "blk")
	if uint64(result.Position.Packed()) != blk.Task.Version {
		t.Errorf("the call answered position %s and its last commit — the "+
			"mirror on the blocker — is at %d: a read barriered at the answer "+
			"can miss it", result.Position, blk.Task.Version)
	}
	if result.Outcome != statelog.OutcomePending {
		t.Errorf("outcome = %q over two pending commits", result.Outcome)
	}
}

// THE OPEN-ASK INFERENCE IS INDEX-SERVED, and the term that makes it so is
// one a reader would delete as redundant.
//
// `tracker_comments_open_asks_idx` is PARTIAL (`WHERE ask <> ” AND ...`), and
// a planner matches a partial index by SYNTACTIC implication: `ask = ?` does
// not imply `ask <> ”`, since the parameter could be empty, so the query also
// states the literal term. The statement planned is the inference's own, bound
// as it binds it: a copy planned here would stay green with the term deleted.
//
// Mutation: delete the non-empty ask term from the inference's statement and
// the plan stops naming the open-asks index.
func TestTheOpenAskInferenceIsIndexServed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	plan := strings.Join(storetest.Plan(t, r.db, worklet.OpenAsksStatement,
		[]any{"t", "a", worklet.MaxOpenAsksNamed + 1}), "\n")
	if !strings.Contains(plan, "tracker_comments_open_asks_idx") {
		t.Errorf("the plan is %q and does not name the open-asks index — every "+
			"comment on a task would pay for the length of its own thread",
			plan)
	}
}

// THE REPAIR AGES ON THE EDGE, not on the task: the dependent's `updated_at`
// resets on any edit, so a busy task, the kind that acquires dependencies,
// would postpone its own repair indefinitely.
func TestTheRepairAgesOnTheEdgeNotTheTask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	// AN EDGE AUTHORED LONG AGO, on a task edited just now.
	old := wednesday.AddDate(0, 0, -7)
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
			Add: []worklet.Relation{{
				Kind: worklet.RelationWaitingOn, Other: "blk", CreatedAt: old,
			}},
		}}, worklet.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	r.drain()
	title := "touched since"
	if _, err := r.writer.UpdateTask(t.Context(), "op-touch", "dep", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: &title},
		worklet.ChangeFields, nil); err != nil {
		t.Fatalf("touch the task: %v", err)
	}
	r.drain()

	// THE HORIZON IS BETWEEN THE TWO: older than the edge, newer than the
	// touch. Aged on the task this finds nothing; aged on the edge it
	// finds the one that needs repairing.
	edges, err := worklet.ScanOneSided(t.Context(), r.db.Reader(), old.AddDate(0, 0, 1), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("the scan found %d edges — an edge authored a week ago on a "+
			"task edited since is one the duty must still repair", len(edges))
	}
	// AND A FRESH EDGE IS NOT YET THE DUTY'S, so it never races a gesture
	// still running.
	fresh, err := worklet.ScanOneSided(t.Context(), r.db.Reader(), old.AddDate(0, 0, -1), 16)
	if err != nil {
		t.Fatalf("ScanOneSided: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("the scan took %d edges from before its own horizon", len(fresh))
	}
}

// CLOSING AN ITEM AS A DUPLICATE KEEPS ITS OTHER RELATIONS.
//
// A relation gesture is an ADD resolved inside the writer's snapshot. Stating
// the one edge as the whole collection deletes every other relation, and the
// blockers' mirrors survive listing a dependent whose side is gone: the
// one-sided state INVERTED, which nothing scans for, since the repair selects
// on the AUTHORED edge.
func TestMarkingADuplicateKeepsTheEdgesTheItemAlreadyHad(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dup")
	filedTask(t, r, "blk")
	filedTask(t, r, "keep")

	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "dup", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	if _, err := r.writer.UpdateTask(t.Context(), "op-dup", "dup", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Relate: &worklet.RelationIntent{Add: []worklet.Relation{{
				Kind: worklet.RelationDuplicates, Other: "keep",
			}}},
		}, worklet.ChangeRelations, nil); err != nil {
		t.Fatalf("mark the duplicate: %v", err)
	}
	r.drain()

	dup := r.task(t, "dup")
	var kinds []worklet.RelationKind
	for _, rel := range dup.Task.Relations {
		kinds = append(kinds, rel.Kind)
	}
	if !slices.Contains(kinds, worklet.RelationWaitingOn) {
		t.Fatalf("the item carries %v after being marked a duplicate, and the "+
			"dependency it was waiting on is gone — the blocker still lists "+
			"it as a dependent, and no scan looks for a mirror whose authored "+
			"edge was deleted", kinds)
	}
	if !slices.Contains(kinds, worklet.RelationDuplicates) {
		t.Fatalf("the item carries %v and not the duplicate edge the write "+
			"asked for", kinds)
	}
	// AND THE WRITER STAMPED IT. An edge records who drew it and when,
	// and neither is something a tool should have to hold a clock for.
	for _, rel := range dup.Task.Relations {
		if rel.Kind != worklet.RelationDuplicates {
			continue
		}
		if rel.CreatedBy == "" || rel.CreatedAt.IsZero() {
			t.Errorf("the duplicate edge was drawn by %q at %v — a relation "+
				"nobody signed is one an audit cannot attribute",
				rel.CreatedBy, rel.CreatedAt)
		}
	}
}

// A BLOCKER CAN STILL BE EDITED. EVERY task apply rewrites `blocker_open` and
// `cleared_at` on each row naming the task as a blocker (`maintainDeps` runs
// out of `explodeTask`, ungated), so every patch's scope must cover those
// dependents, not only a STATUS patch's, or [ScopeSet.covers] refuses it with
// a `statelog.ErrConflict` that no re-run clears.
func TestABlockerTakesAnOrdinaryEditWhileSomebodyWaitsOnIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
		Note: "needs the schema first",
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	// NOT A STATUS PATCH, whose scope always covered the dependents: an
	// ordinary field edit is what this case is about.
	points := 8.0
	if _, err := r.writer.UpdateTask(t.Context(), "op-edit", "blk", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Title: ptr("the blocker, retitled"), Points: &points,
		}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("editing a blocker: %v — a task somebody waits on takes an "+
			"ordinary edit like any other", err)
	}
	r.drain()

	blk := r.task(t, "blk")
	if blk.Task.Title != "the blocker, retitled" {
		t.Errorf("title = %q, want the edit to have landed", blk.Task.Title)
	}
	if blk.Task.Points != 8 {
		t.Errorf("points = %v, want 8", blk.Task.Points)
	}
	// AND THE EDGE SURVIVED IT. The apply that took the edit is the same
	// one that maintains the dependency rows, so a widened scope must not
	// have cost the dependent its block.
	if dep := r.task(t, "dep"); !dep.Blocked {
		t.Error("the dependent is no longer blocked after its blocker was edited")
	}
}
