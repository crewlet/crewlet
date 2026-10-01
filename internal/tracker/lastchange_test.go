package tracker_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PROJECT ROW SAYS WHEN ITS WORK LAST CHANGED AND WHO CHANGED IT.
//
// The pair is maintained on the same terms as the census beside it: a
// directory drawing thirty projects would otherwise run thirty seeks per poll
// over `tracker_history`, which is one row per applied commit and which
// nothing ever sweeps — so the cost of the screen grows for the life of the
// deployment while the value changes on a handful of commits an hour.
//
// The ACTOR is asserted with the instant because the two are one record's
// values: a stamp that took the instant from one commit and the handle from
// another would name a person who did not make the change it is dated to.
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
	if row.LastChange.Actor != "ana" || row.LastChange.ActorKind != tracker.AuthorHuman {
		t.Errorf("the last change is by %q/%q, want ana/human — the harness's "+
			"writer is a person, and `ana` the person and `ana` the seat are "+
			"different answers a directory draws differently",
			row.LastChange.Actor, row.LastChange.ActorKind)
	}

	// AND IT IS THE HEAD OF THE PROJECT'S OWN ACTIVITY FEED, which is the
	// invariant the stamp hangs off the history row to get: a row saying
	// "changed two minutes ago" whose feed's newest entry is from last
	// week is a contradiction a reader meets by clicking.
	feed := r.activity(tracker.ActivityQuery{Project: "ENG"})
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

	// A SECOND COMMIT MOVES IT, and a comment is deliberately the one
	// chosen: it moves no count, so a stamp wired to the census rather
	// than to the commit would sit still through the whole conversation
	// on a task.
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-1", Task: "t-1", Author: "ops-1",
			AuthorKind: tracker.AuthorOperator, Body: "looking at this",
			CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment on the task: %v", err)
	}
	r.drain()

	after := onlyProject(t, r)
	if after.LastChange == nil || after.LastChange.Actor != "ops-1" ||
		after.LastChange.ActorKind != tracker.AuthorOperator {
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
// The honest report of "nothing yet" is an ABSENCE. The two values a reader
// would otherwise have been given are both made up: a zero instant renders as
// the year 1 on every empty project, and the project's own creation instant
// reports a directory of untouched projects as freshly active — which is
// exactly the reading the column exists to make possible.
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
	detail := r.project(tracker.ProjectDetailQuery{Project: "ENG"})
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
// Two nodes at one checkpoint have seen the same SET of records and a
// different ORDER of them — a reprocess after an upgrade and a redelivery
// after an adoption both put a record below rows already applied. These rows
// are compared byte for byte across the fleet, so a value folded over arrival
// order diverges permanently, on a column nothing repairs.
//
// The guard is the composed position rather than the instant, which is the
// half an instant could not do: two records can share one.
func TestTheLastChangeNamesTheHighestCommitRatherThanTheLastApplied(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	seedApplyProject(t, h)

	if _, err := h.applyAt(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC(), 10); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := tracker.StatusDone
	later := taskRecord("t-1", tracker.OpPatch, tracker.TaskPatch{Status: &done}, nil)
	later.Actor, later.ActorKind = "bo", tracker.AuthorAgent
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
	late := taskRecord("t-1", tracker.OpPatch,
		tracker.TaskPatch{Title: ptr("renamed")}, nil)
	late.OpID = "t-1-late"
	late.Actor, late.ActorKind = "cy", tracker.AuthorAgent
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
// # The count this closes
//
// Every other departure from a status bucket is a task RECORD and goes through
// the count maintenance; a purge DELETES the row instead of writing one, and
// nothing lowered the count it was in. A task purged while it was open left
// `open_count` one too high on every node — for ever, because these counts are
// maintained and nothing anywhere aggregates them back into agreement — and
// `PurgeTask` does not require the task to have been removed first, so it is
// the ordinary shape of the operation rather than a corner of it.
func TestAPurgeLowersTheCensusAndStampsTheProject(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	seedApplyProject(t, h)
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := h.value(`SELECT open_count FROM tracker_projects WHERE key = 'ENG'`); got != 1 {
		t.Fatalf("the open count is %d after one open task", got)
	}

	purge := taskRecord("t-1", tracker.OpPurge, map[string]any{"reason": "leaked a key"}, nil)
	purge.Actor, purge.ActorKind = "ops-1", tracker.AuthorOperator
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

	// THE SAME PURGE AGAIN. The deletion gate lets this record's own
	// redelivery through by op id — it is what wrote the marker — and
	// every statement it runs is then a no-op. A second decrement would
	// take the census one BELOW the truth, which is the half the clamp
	// cannot see.
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
// A turn record adds an agent's spend to a task and writes no history row:
// what it records is what the work COST, not a change to it. Counting it would
// mark every project a seat is thinking in as changed, continuously, while
// nothing about the work moved — and the directory would stop being able to
// answer the question it exists for.
func TestASpendingTurnIsNotAChangeToTheProjectsWork(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	seedApplyProject(t, h)
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := h.value(`SELECT last_change_seq FROM tracker_projects WHERE key = 'ENG'`)

	turn := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "turn-1",
			Subject: tracker.TurnSubject("t-1"), Op: tracker.OpTurn,
			CreatedAt: time.Unix(1_700_000_200, 0).UTC(),
			Writer:    "node-a", Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: mustJSON(map[string]any{
			"task": "t-1", "seat": "bo",
			"spend": map[string]int{"turns": 1, "rounds": 4, "input": 1000},
		}),
		Actor: "bo", ActorKind: tracker.AuthorAgent,
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
// DOCUMENT, filed under no project at all — so a directory sorted on this
// column does not lift a project to the top because somebody fixed a typo in
// its purpose.
func TestEditingAProjectIsNotAChangeToItsWork(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	before := onlyProject(t, r).LastChange
	if before == nil {
		t.Fatal("the filed task left no last change, so this case has no before")
	}

	if _, err := r.writer.WriteProject(t.Context(), "op-triage", "ENG",
		tracker.ProjectEdit{DefaultAssignee: ptr("bo")},
		tracker.ProjectAuthority{Lead: true}); err != nil {
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

// THE MIGRATION BACKFILLS FROM THE HISTORY THE ESTATE ALREADY HOLDS.
//
// A deployment that upgrades has every task commit it has ever applied sitting
// in `tracker_history`, so the columns arrive populated rather than blank on
// every project until somebody touches it. Without this a directory would open
// on a company whose every project reads "no activity ever" — which is the one
// answer this column must never give wrongly.
//
// THE STATEMENT UNDER TEST IS READ OUT OF THE SHIPPED MIGRATION rather than
// restated here: a copy of it in a test is a second source of truth that can
// agree with itself while disagreeing with what an operator's database runs.
func TestTheMigrationBackfillsTheLastChangeFromTheHistory(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	seedProject(t, r, tracker.Project{Key: "OPS", Name: "Operations"})

	stamped := onlyRow(t, r, "ENG")
	if stamped.LastChange == nil {
		t.Fatal("the seeded work left no last change to compare a backfill against")
	}

	// THE PRE-MIGRATION STATE, which is what every column this file adds
	// held the instant before its UPDATE ran.
	writeReplicated(t, r, `UPDATE tracker_projects SET last_change_at = NULL,
		last_change_actor = '', last_change_actor_kind = '', last_change_seq = 0`)
	if onlyRow(t, r, "ENG").LastChange != nil {
		t.Fatal("clearing the columns left a last change, so the backfill " +
			"below would pass without doing anything")
	}

	for _, statement := range backfillStatements(t,
		"0016_a_project_says_when_its_work_last_changed.sql") {
		writeReplicated(t, r, statement)
	}

	back := onlyRow(t, r, "ENG")
	if back.LastChange == nil {
		t.Fatal("the backfill left ENG with no last change, although every " +
			"commit about its tasks is in tracker_history")
	}
	if !back.LastChange.At.Equal(stamped.LastChange.At) ||
		back.LastChange.Actor != stamped.LastChange.Actor ||
		back.LastChange.ActorKind != stamped.LastChange.ActorKind {
		t.Errorf("the backfill answered %+v and the apply had written %+v — "+
			"they are the same function of the same rows, so an upgraded "+
			"database and a fresh one must not disagree",
			back.LastChange, stamped.LastChange)
	}

	// AND A PROJECT WITH NO WORK STAYS ABSENT. A backfill that stamped
	// every row — with a zero, or with the project's own creation — would
	// report a company of untouched projects as uniformly active.
	if got := onlyRow(t, r, "OPS"); got.LastChange != nil {
		t.Errorf("the backfill gave a project with no task commits a last "+
			"change of %+v", got.LastChange)
	}
}

// ---- helpers ------------------------------------------------------------ //

// onlyProject is the single project the round-trip harness seeds.
func onlyProject(t *testing.T, r *roundTrip) tracker.ProjectRow {
	t.Helper()
	return onlyRow(t, r, "ENG")
}

// onlyRow is one project out of the listing, by key.
func onlyRow(t *testing.T, r *roundTrip, key string) tracker.ProjectRow {
	t.Helper()
	for _, row := range r.projects(tracker.ProjectQuery{Archived: tracker.ArchivedExclude}).Projects {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("the listing carries no project %s", key)
	return tracker.ProjectRow{}
}

// projectActor is the handle stamped on ENG's last change.
func projectActor(t *testing.T, h *applyHarness) string {
	t.Helper()
	var actor string
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
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
	body, err := json.Marshal(tracker.Project{
		V: tracker.DocumentVersion, Key: "ENG", Name: "Engineering",
	})
	if err != nil {
		t.Fatalf("encode the project: %v", err)
	}
	if _, err := h.apply(tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "eng-1",
			Subject: tracker.ProjectSubject("ENG"), Op: tracker.OpCreate,
			Writer: "node-a", Scope: tracker.ScopeSet{Subject: true},
		},
		Mutation: body,
	}, time.Unix(1_700_000_050, 0).UTC()); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
}

// writeReplicated runs one statement against the replicated estate.
//
// A TEST IS NOT AN APPLIER, and this is the one thing in this file that writes
// the estate without a record behind it: it is standing in for the MIGRATOR,
// which is the other writer the estate has and which runs before any applier
// does.
func writeReplicated(t *testing.T, r *roundTrip, statement string) {
	t.Helper()
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), statement)
		return err
	}); err != nil {
		t.Fatalf("run %q: %v", statement, err)
	}
}

// backfillStatements is every UPDATE the named replicated migration carries.
//
// Read out of the SHIPPED file, so the case exercises the statement an
// operator's database runs rather than a copy of it that can drift.
func backfillStatements(t *testing.T, name string) []string {
	t.Helper()
	body, err := store.SchemaFile(store.EstateReplicated, name)
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	// COMMENTS FIRST, then statements: a migration's prose is free to
	// contain a semicolon, and splitting before stripping cuts a comment in
	// two and hands its tail to the statement that follows — which then no
	// longer starts with UPDATE and is silently not run.
	var out []string
	for _, statement := range strings.Split(stripSQLComments(string(body)), ";") {
		trimmed := strings.TrimSpace(statement)
		if strings.HasPrefix(trimmed, "UPDATE ") {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s carries no UPDATE at all, so this case certifies "+
			"nothing — a backfill somebody removed is exactly what it is "+
			"here to notice", name)
	}
	return out
}

// stripSQLComments drops the `--` lines a migration is mostly made of.
func stripSQLComments(in string) string {
	var kept []string
	for _, line := range strings.Split(in, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
