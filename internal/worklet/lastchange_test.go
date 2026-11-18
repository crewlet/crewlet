package worklet_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A PROJECT ROW SAYS WHEN ITS WORK LAST CHANGED AND WHO CHANGED IT.
//
// The pair is maintained like the census beside it rather than sought over
// `tracker_history`, one row per commit and never swept, so a directory poll
// would grow costlier for the life of the deployment. The ACTOR is asserted
// with the instant because both are one record's values: a mix of two commits
// names a person who did not make the dated change.
func TestAProjectRowSaysWhenItsWorkLastChangedAndWhoChangedIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	row := onlyProject(t, r)
	if row.LastChange == nil {
		t.Fatal("a project with a task filed into it reports no last change — " +
			"the directory can say how much work a project holds and not " +
			"whether anybody has touched it this quarter")
	}
	if row.LastChange.Actor != "ana" || row.LastChange.ActorKind != worklet.AuthorHuman {
		t.Errorf("the last change is by %q/%q, want ana/human — the harness's "+
			"writer is a person, and `ana` the person and `ana` the seat are "+
			"different answers a directory draws differently",
			row.LastChange.Actor, row.LastChange.ActorKind)
	}

	// AND IT IS THE HEAD OF THE PROJECT'S OWN ACTIVITY FEED: a row saying
	// "changed two minutes ago" over a feed whose newest entry is last week
	// contradicts itself.
	feed := r.activity(worklet.ActivityQuery{Project: "ENG"})
	if len(feed.Records) == 0 {
		t.Fatal("the project's feed is empty, so this case is comparing " +
			"against nothing")
	}
	head := feed.Records[0]
	if !row.LastChange.At.Equal(head.At) {
		t.Errorf("the row's last change is %s and the feed's newest commit is "+
			"%s — the directory and the feed are the same fact and must never "+
			"disagree", row.LastChange.At, head.At)
	}

	// A SECOND COMMIT MOVES IT. A comment moves no count, so a stamp wired to the
	// census rather than to the commit would sit still here.
	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	if _, err := operator.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-1", Task: "t-1", Author: "ops-1",
			AuthorKind: worklet.AuthorOperator, Body: "looking at this",
			CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("comment on the task: %v", err)
	}
	r.drain()

	after := onlyProject(t, r)
	if after.LastChange == nil || after.LastChange.Actor != "ops-1" ||
		after.LastChange.ActorKind != worklet.AuthorOperator {
		t.Fatalf("after a comment the last change is %+v, want the operator "+
			"token's — a comment changes no count, and a stamp that moved "+
			"only with the census would report the project untouched through "+
			"an entire thread", after.LastChange)
	}
	if !after.LastChange.At.After(row.LastChange.At) &&
		!after.LastChange.At.Equal(row.LastChange.At) {
		t.Errorf("the last change went backwards, from %s to %s",
			row.LastChange.At, after.LastChange.At)
	}
}

// A PROJECT NOBODY HAS FILED WORK INTO REPORTS NO LAST CHANGE AT ALL.
//
// "Nothing yet" is an ABSENCE: a zero instant renders as the year 1, and the
// project's creation instant reports untouched projects as freshly active.
func TestAProjectWithNoWorkReportsNoLastChange(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	row := onlyProject(t, r)
	if row.LastChange != nil {
		t.Fatalf("a project with no work reports a last change of %+v — an "+
			"absent value is the only honest answer, and a made-up one is "+
			"indistinguishable from a project somebody is working in",
			row.LastChange)
	}
	detail := r.project(worklet.ProjectDetailQuery{Project: "ENG"})
	if detail.LastChange != nil {
		t.Errorf("the project's own page reports a last change of %+v while "+
			"the listing reports none — one fact, two readers",
			detail.LastChange)
	}

	// AND THE FIRST TASK IS WHAT GIVES IT ONE — without this the case
	// above passes on a build where nothing ever writes the columns.
	filedTask(t, r, "t-1")
	if onlyProject(t, r).LastChange == nil {
		t.Error("filing the project's first task left it with no last change")
	}
}

// THE STAMP NAMES THE COMMIT HIGHEST IN THE LOG, NOT THE ONE APPLIED LAST.
//
// Nodes at one checkpoint hold the same SET of records in different ORDERs (a
// reprocess after an upgrade, a redelivery after an adoption), and these rows
// are compared byte for byte across the fleet, so a value folded over arrival
// order diverges for good. The guard is the composed position, not the
// instant: two records can share an instant.
func TestTheLastChangeNamesTheHighestCommitRatherThanTheLastApplied(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	seedApplyProject(t, h)

	if _, err := h.applyAt(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC(), 10); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := worklet.StatusDone
	later := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Status: &done}, nil)
	later.Actor, later.ActorKind = "bo", worklet.AuthorAgent
	later.CreatedAt = time.Unix(1_700_000_400, 0).UTC()
	if _, err := h.applyAt(later, time.Unix(1_700_000_400, 0).UTC(), 20); err != nil {
		t.Fatalf("the newer commit: %v", err)
	}
	if got := projectActor(t, h); got != "bo" {
		t.Fatalf("after the newer commit the last change is by %q, want bo", got)
	}

	// NOW THE RECORD THAT ARRIVES LATE AND BELONGS EARLIER. Its history
	// row is written — the feed is a complete account — and it must not
	// take the stamp back to itself.
	late := taskRecord("t-1", worklet.OpPatch,
		worklet.TaskPatch{Title: ptr("renamed")}, nil)
	late.OpID = "t-1-late"
	late.Actor, late.ActorKind = "cy", worklet.AuthorAgent
	if _, err := h.applyAt(late, time.Unix(1_700_000_200, 0).UTC(), 15); err != nil {
		t.Fatalf("the reprocessed commit: %v", err)
	}
	if got := projectActor(t, h); got != "bo" {
		t.Fatalf("a record reprocessed BELOW an applied successor moved the "+
			"last change to %q — the stamp is the highest commit in the log, "+
			"so two nodes that saw these three records in different orders "+
			"must end up with the same row", got)
	}
	if got := h.value(
		`SELECT last_change_seq FROM tracker_projects WHERE key = 'ENG'`); got != 20 {
		t.Errorf("last_change_seq is %d, want the highest applied position", got)
	}
}

// A PURGE LOWERS THE CENSUS IT LEAVES, AND STAMPS THE PROJECT.
//
// A purge DELETES the row rather than writing a task record, so it skips the
// count maintenance every other departure from a status bucket goes through.
// The counts are maintained and never re-aggregated, so a missed decrement
// leaves `open_count` too high on every node for good, and `PurgeTask` does not
// require a prior removal, so an open task is its ordinary case.
func TestAPurgeLowersTheCensusAndStampsTheProject(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	seedApplyProject(t, h)
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := h.value(`SELECT open_count FROM tracker_projects WHERE key = 'ENG'`); got != 1 {
		t.Fatalf("the open count is %d after one open task", got)
	}

	purge := taskRecord("t-1", worklet.OpPurge, map[string]any{"reason": "leaked a key"}, nil)
	purge.Actor, purge.ActorKind = "ops-1", worklet.AuthorOperator
	if _, err := h.apply(purge, time.Unix(1_700_000_300, 0).UTC()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if got := h.value(`SELECT open_count FROM tracker_projects WHERE key = 'ENG'`); got != 0 {
		t.Errorf("the open count is %d after the only open task was purged — "+
			"the counts are MAINTAINED and never scanned, so nothing anywhere "+
			"would ever correct this", got)
	}
	if got := projectActor(t, h); got != "ops-1" {
		t.Errorf("the last change is by %q after a purge, want the operator "+
			"who made it — a purge is the most drastic change a project's "+
			"work can undergo", got)
	}

	// THE SAME PURGE AGAIN. The deletion gate lets this record's own redelivery
	// through by op id, and every statement is then a no-op; a second decrement
	// would take the census BELOW the truth, which the clamp cannot see.
	if _, err := h.apply(purge, time.Unix(1_700_000_300, 0).UTC()); err != nil {
		t.Fatalf("redelivered purge: %v", err)
	}
	if got := h.value(`SELECT open_count FROM tracker_projects WHERE key = 'ENG'`); got != 0 {
		t.Errorf("a redelivered purge moved the open count to %d", got)
	}
	if got := h.value(`SELECT done_count FROM tracker_projects WHERE key = 'ENG'`); got != 0 {
		t.Errorf("the done count is %d after a purge that touched no done task", got)
	}
}

// A TURN DOES NOT MOVE THE STAMP.
//
// A turn record adds an agent's spend and writes no history row: it records
// what the work COST, not a change to it. Counting it would mark every project
// a seat is thinking in as changed, continuously.
func TestASpendingTurnIsNotAChangeToTheProjectsWork(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	seedApplyProject(t, h)
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := h.value(`SELECT last_change_seq FROM tracker_projects WHERE key = 'ENG'`)

	turn := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "turn-1",
			Subject: worklet.TurnSubject("t-1"), Op: worklet.OpTurn,
			CreatedAt: time.Unix(1_700_000_200, 0).UTC(),
			Writer:    "node-a", Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: mustJSON(map[string]any{
			"task": "t-1", "seat": "bo",
			"spend": map[string]int{"turns": 1, "rounds": 4, "input": 1000},
		}),
		Actor: "bo", ActorKind: worklet.AuthorAgent,
	}
	if _, err := h.apply(turn, time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if got := h.value(
		`SELECT last_change_seq FROM tracker_projects WHERE key = 'ENG'`); got != before {
		t.Errorf("a turn moved the last change from %d to %d — spend is what a "+
			"turn COST and not a change to the work, and a project a seat is "+
			"thinking in would read as changing continuously", before, got)
	}
}

// AND THE PROJECT'S OWN SETTINGS ARE NOT ITS WORK.
//
// A rename, a field declaration or an archive is a commit about the project
// DOCUMENT, filed under no project, so fixing a typo in its purpose does not
// lift a project to the top of the directory.
func TestEditingAProjectIsNotAChangeToItsWork(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	before := onlyProject(t, r).LastChange
	if before == nil {
		t.Fatal("the filed task left no last change, so this case has no before")
	}

	if _, err := r.writer.WriteProject(t.Context(), "op-triage", "ENG",
		worklet.ProjectEdit{DefaultAssignee: ptr("bo")},
		worklet.ProjectAuthority{Policy: true}); err != nil {
		t.Fatalf("edit the project: %v", err)
	}
	r.drain()

	after := onlyProject(t, r)
	if after.DefaultAssignee != "bo" {
		t.Fatalf("the edit did not land: the default assignee is %q",
			after.DefaultAssignee)
	}
	if after.LastChange == nil || !after.LastChange.At.Equal(before.At) {
		t.Errorf("editing the project moved its last change to %+v — the "+
			"project's settings changing is not its work changing", after.LastChange)
	}
}

// ---- helpers ------------------------------------------------------------ //

// onlyProject is the single project the round-trip harness seeds.
func onlyProject(t *testing.T, r *roundTrip) worklet.ProjectRow {
	t.Helper()
	return onlyRow(t, r, "ENG")
}

// onlyRow is one project out of the listing, by key.
func onlyRow(t *testing.T, r *roundTrip, key string) worklet.ProjectRow {
	t.Helper()
	for _, row := range r.projects(worklet.ProjectQuery{Archived: worklet.ArchivedExclude}).Projects {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("the listing carries no project %s", key)
	return worklet.ProjectRow{}
}

// projectActor is the handle stamped on ENG's last change.
func projectActor(t *testing.T, h *applyHarness) string {
	t.Helper()
	var actor string
	if err := h.db.Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT last_change_actor FROM tracker_projects WHERE key = 'ENG'`).
			Scan(&actor)
	}); err != nil {
		t.Fatalf("read the last change actor: %v", err)
	}
	return actor
}

// seedApplyProject files the project the apply harness's task records name.
func seedApplyProject(t *testing.T, h *applyHarness) {
	t.Helper()
	body, err := json.Marshal(worklet.Project{
		V: worklet.DocumentVersion, Key: "ENG", Name: "Engineering",
	})
	if err != nil {
		t.Fatalf("encode the project: %v", err)
	}
	if _, err := h.apply(worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "eng-1",
			Subject: worklet.ProjectSubject("ENG"), Op: worklet.OpCreate,
			Writer: "node-a", Scope: worklet.ScopeSet{Subject: true},
		},
		Kind:     worklet.ChangeProjectCreated,
		Mutation: body,
	}, time.Unix(1_700_000_050, 0).UTC()); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
}
