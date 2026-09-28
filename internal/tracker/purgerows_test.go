package tracker_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A PURGE LEAVES NO COPY OF THE TASK BEHIND, in any table.
//
// The object tables went and the history did not: every history row keeps its
// record's whole mutation — each title, body and comment the task was ever
// given — beside an excerpt, an inbox notice keeps an excerpt of its own, and
// turn records and the dependency mirror stayed keyed on an id nothing
// resolves. So a task purged because it held something that must not be kept
// stayed readable on every node for the life of the company, while the purge
// report and the docs said it was destroyed.
//
// What survives is the purge's own account — that it happened, to which key,
// by whom and why — and the lead's `purged` notice, which is read through it.
func TestAPurgeLeavesNoCopyOfTheTaskBehind(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const secret = "the merger with Contoso"
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	// THE CONTENT, in every place a record puts it: a comment that
	// mentions somebody (a history row, its document and an inbox
	// notice), a turn's spend, and a dependency whose mirror is a row on
	// the task.
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Comment: &tracker.Comment{ID: "c-1", Body: secret, Author: "jane"},
		}, tracker.ChangeComment, &tracker.Notify{
			Kind: tracker.ChangeComment, Excerpt: secret, Mentions: []string{"ana"},
		}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	if _, err := r.writer.RecordTurn(t.Context(), "op-turn", tracker.TurnRecord{
		Task: "t-1", Seat: "swe", TurnID: "run-1",
		Spend: tracker.TurnSpend{Turns: 1, Input: 10},
	}); err != nil {
		t.Fatalf("record a turn: %v", err)
	}
	r.drain()
	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
		Task: "t-2", Project: "ENG", WaitingOnAdd: []string{"t-1"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("depend: %v", err)
	}
	r.drain()
	if held := oneTask(t, r, "t-1"); len(held.Dependents) == 0 {
		t.Fatalf("t-1 lists no dependents after the depend: %+v", held.Dependents)
	}

	about := func(query string) []string {
		t.Helper()
		return r.strings(query, "t-1")
	}
	const (
		histories = `SELECT id FROM tracker_history WHERE subject_id = ?`
		notices   = `SELECT record_id FROM tracker_notifications WHERE subject_id = ?`
		turns     = `SELECT id FROM tracker_turns WHERE task_id = ?`
		mirrors   = `SELECT task_id FROM tracker_task_dependents
			WHERE task_id = ?1 OR dependent_id = ?1`
		copies = `SELECT id FROM tracker_history
			WHERE subject_id = ? AND (CAST(document AS TEXT) LIKE '%Contoso%'
			                          OR excerpt LIKE '%Contoso%')`
	)
	// THE FIXTURE HOLDS WHAT IT SAYS, or the assertions below pass on
	// a task that never had any of it.
	for name, query := range map[string]string{
		"history": histories, "a notice": notices, "a turn": turns,
		"a mirror": mirrors, "a copy of the comment": copies,
	} {
		if len(about(query)) == 0 {
			t.Fatalf("the fixture wrote no %s about t-1", name)
		}
	}

	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"asked for by legal"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	if got := about(histories); len(got) != 1 || got[0] != "op-purge" {
		t.Errorf("after the purge the task's history is %v, want the purge's "+
			"own row alone", got)
	}
	if got := about(copies); len(got) != 0 {
		t.Errorf("the purged comment survives in history rows %v", got)
	}
	for _, id := range about(notices) {
		if id != "op-purge" {
			t.Errorf("an inbox notice from %s survives the purge", id)
		}
	}
	if got := notifiedHandles(t, r, "t-1"); !containsKind(got, "eng-lead") {
		t.Errorf("the purge's own notice is gone (%v) — the lead's `purged` "+
			"wake is the one account of the task that must survive", got)
	}
	if got := about(turns); len(got) != 0 {
		t.Errorf("turn records %v survive the purge", got)
	}
	if got := about(mirrors); len(got) != 0 {
		t.Errorf("dependency mirror rows %v survive the purge", got)
	}
	if excerpt := historyExcerpt(t, r, "task", "t-1"); !strings.Contains(excerpt, "purged") {
		t.Errorf("the surviving row is not the purge's: %q", excerpt)
	}
}
