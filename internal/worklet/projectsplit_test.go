package worklet_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// census reads one project's four numbers as the listing serves them.
func census(t *testing.T, r *roundTrip, key string) worklet.TaskCounts {
	t.Helper()
	for _, row := range r.projects(worklet.ProjectQuery{
		Archived: worklet.ArchivedInclude,
	}).Projects {
		if row.Key == key {
			return row.Counts
		}
	}
	t.Fatalf("no project %s in the listing", key)
	return worklet.TaskCounts{}
}

// recountedActive is `active_count` as a COUNT over the task rows would have
// it — the truth the maintained column has to equal.
func recountedActive(t *testing.T, r *roundTrip, key string) int {
	t.Helper()
	var n int
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM tracker_tasks
			WHERE project_key = ? AND status_group = 'active'
			  AND removed_at IS NULL`, key).Scan(&n)
	}); err != nil {
		t.Fatalf("recount %s: %v", key, err)
	}
	return n
}

func setStatus(t *testing.T, r *roundTrip, id, project string, status worklet.Status) {
	t.Helper()
	// ONE OPERATION PER (task, status): no case moves a task to the same
	// status twice.
	if _, err := r.writer.UpdateTask(t.Context(), "op-"+id+"-"+string(status),
		id, project, worklet.NoIfMatch,
		worklet.TaskPatch{Status: &status}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("move %s to %s: %v", id, status, err)
	}
	r.drain()
}

// THE ACTIVE COUNT IS EXACT ACROSS EVERY WAY A TASK ARRIVES AND LEAVES.
//
// It is a MAINTAINED column: nothing aggregates it back into agreement, so a
// path that forgets it leaves the number wrong on every node for ever. The
// paths are a status change into and out of the `active` group, a move to
// another project while started, a removal, a restore, and a purge — the one
// departure that deletes the row instead of writing it.
func TestTheActiveCountIsExactAcrossMoveRemoveRestoreAndPurge(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})
	filedTask(t, r, "a")
	filedTask(t, r, "b")
	filedTask(t, r, "c")

	check := func(step string, eng, ops worklet.TaskCounts) {
		t.Helper()
		for key, want := range map[string]worklet.TaskCounts{"ENG": eng, "OPS": ops} {
			if got := census(t, r, key); got != want {
				t.Fatalf("after %s %s counts %+v, want %+v", step, key, got, want)
			}
			if got := recountedActive(t, r, key); got != want.Active {
				t.Fatalf("after %s %s's rows hold %d started, the census says %d",
					step, key, got, want.Active)
			}
		}
	}
	check("filing three", worklet.TaskCounts{Todo: 3}, worklet.TaskCounts{})

	setStatus(t, r, "a", "ENG", worklet.StatusInProgress)
	setStatus(t, r, "b", "ENG", worklet.StatusInReview)
	check("starting two", worklet.TaskCounts{Todo: 1, Active: 2}, worklet.TaskCounts{})

	// IN PROGRESS → IN REVIEW stays in the group and moves nothing.
	setStatus(t, r, "a", "ENG", worklet.StatusInReview)
	check("a move inside the group", worklet.TaskCounts{Todo: 1, Active: 2},
		worklet.TaskCounts{})

	setStatus(t, r, "b", "ENG", worklet.StatusDone)
	check("finishing one", worklet.TaskCounts{Todo: 1, Active: 1, Done: 1},
		worklet.TaskCounts{})

	if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "a", "OPS",
		nil); err != nil {
		t.Fatalf("move a to OPS: %v", err)
	}
	r.drain()
	check("moving a started task", worklet.TaskCounts{Todo: 1, Done: 1},
		worklet.TaskCounts{Active: 1})

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "a", "OPS",
		false, nil); err != nil {
		t.Fatalf("remove a: %v", err)
	}
	r.drain()
	check("removing it", worklet.TaskCounts{Todo: 1, Done: 1}, worklet.TaskCounts{})

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "a", "OPS",
		false, nil); err != nil {
		t.Fatalf("restore a: %v", err)
	}
	r.drain()
	check("restoring it", worklet.TaskCounts{Todo: 1, Done: 1},
		worklet.TaskCounts{Active: 1})

	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "a", "OPS",
		"filed by mistake"); err != nil {
		t.Fatalf("purge a: %v", err)
	}
	r.drain()
	check("purging it while started", worklet.TaskCounts{Todo: 1, Done: 1},
		worklet.TaskCounts{})
}

// THE BACKFILL EQUALS A RECOUNT.
//
// A node whose checkpoint's rule set differs from this build's — a cursor a
// reanchor created at derivation 0, or rows adopted from a build with different
// rules — runs [Applier.Rederive] once. What it computes has to be exactly what
// the incremental rule reached by applying the same records, or the re-derived
// node and one that applied them disagree on a table the fleet compares byte
// for byte.
func TestTheActiveBackfillEqualsTheMaintainedCount(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})
	for _, id := range []string{"a", "b", "c", "d"} {
		filedTask(t, r, id)
	}
	filedInto(t, r, "OPS", "o")
	setStatus(t, r, "a", "ENG", worklet.StatusInProgress)
	setStatus(t, r, "b", "ENG", worklet.StatusInReview)
	setStatus(t, r, "c", "ENG", worklet.StatusInProgress)
	setStatus(t, r, "c", "ENG", worklet.StatusDone)
	setStatus(t, r, "o", "OPS", worklet.StatusInProgress)
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "b", "ENG",
		false, nil); err != nil {
		t.Fatalf("remove b: %v", err)
	}
	r.drain()

	maintained := map[string]worklet.TaskCounts{
		"ENG": census(t, r, "ENG"), "OPS": census(t, r, "OPS"),
	}
	if maintained["ENG"].Active != 1 || maintained["OPS"].Active != 1 {
		t.Fatalf("the maintained census is %+v, want one started in each", maintained)
	}

	// ROWS A DIFFERENT RULE SET DERIVED: the column at zero.
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`UPDATE tracker_projects SET active_count = 0`); err != nil {
			return err
		}
		_, err := r.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	for key, want := range maintained {
		if got := census(t, r, key); got != want {
			t.Errorf("%s re-derives to %+v and the apply maintained %+v — a "+
				"re-derived node would disagree with one that applied them",
				key, got, want)
		}
	}
}

// A TARGET DATE IS THE LEAD'S OR A PERSON'S, AND NOBODY ELSE'S.
func TestATargetDateIsTheLeadsOrAPersons(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	day := func(s string) *string { return &s }

	_, err := r.writer.WriteProject(t.Context(), "op-nobody", "ENG",
		worklet.ProjectEdit{TargetDate: day("2026-12-18")}, worklet.ProjectAuthority{})
	if !errors.Is(err, worklet.ErrForbidden) || !strings.Contains(err.Error(), "target date") {
		t.Fatalf("a seat that leads nothing set the target: %v — want a refusal "+
			"naming the target date", err)
	}

	for _, c := range []struct {
		who       string
		authority worklet.ProjectAuthority
		target    string
	}{
		{"the lead", worklet.ProjectAuthority{Policy: true}, "2026-12-18"},
		{"an admin", worklet.ProjectAuthority{Policy: true, Archive: true}, "2027-01-29"},
	} {
		if _, err := r.writer.WriteProject(t.Context(), "op-"+c.target, "ENG",
			worklet.ProjectEdit{TargetDate: day(c.target)}, c.authority); err != nil {
			t.Fatalf("%s setting the target: %v", c.who, err)
		}
		r.drain()
		if got := r.project(worklet.ProjectDetailQuery{Project: "ENG"}).TargetDate; got != c.target {
			t.Fatalf("after %s set it, the target reads %q, want %q", c.who, got, c.target)
		}
	}

	// THE EMPTY STRING CLEARS IT — "no target" is a setting.
	if _, err := r.writer.WriteProject(t.Context(), "op-clear", "ENG",
		worklet.ProjectEdit{TargetDate: day("")},
		worklet.ProjectAuthority{Policy: true}); err != nil {
		t.Fatalf("clear the target: %v", err)
	}
	r.drain()
	listing := r.projects(worklet.ProjectQuery{Archived: worklet.ArchivedExclude})
	if got := listing.Projects[0].TargetDate; got != "" {
		t.Errorf("a cleared target reads %q in the listing, want none", got)
	}
}

// AN INSTANT IS STORED AS THE COMPANY'S DAY, AND THE WRITER IS TOLD; A VALUE
// THAT IS NO DATE IS REFUSED BEFORE ANYTHING IS PUBLISHED.
func TestATargetDateIsCoercedOnTheCompanysClock(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load a zone: %v", err)
	}
	r.writer.Zone = func() *time.Location { return tokyo }
	instant := "2026-12-18T20:00:00Z" // the morning of the 19th in Tokyo
	result, err := r.writer.WriteProject(t.Context(), "op-instant", "ENG",
		worklet.ProjectEdit{TargetDate: &instant}, worklet.ProjectAuthority{Policy: true})
	if err != nil {
		t.Fatalf("an instant target: %v", err)
	}
	r.drain()
	if got := r.project(worklet.ProjectDetailQuery{Project: "ENG"}).TargetDate; got != "2026-12-19" {
		t.Errorf("the instant was stored as %q, want the day it falls on in Tokyo", got)
	}
	if len(result.Caveats) != 1 || result.Caveats[0].Code() != caveat.CodeDateTruncated ||
		!strings.Contains(result.Caveats[0].Person(), "2026-12-19") {
		t.Errorf("the write told the caller %+v, want one caveat naming the "+
			"stored day: the caller's value was changed", result.Caveats)
	}

	nonsense := "end of the quarter"
	_, err = r.writer.WriteProject(t.Context(), "op-nonsense", "ENG",
		worklet.ProjectEdit{TargetDate: &nonsense}, worklet.ProjectAuthority{Policy: true})
	if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), nonsense) {
		t.Errorf("a target that is no date answered %v, want an invalid refusal "+
			"quoting it", err)
	}
}

// A CHART APPLY CARRIES THE TARGET THROUGH.
//
// The target is the lead's and the name is the chart's, and both live on one
// document: a chart reconcile that rebuilt the document from the chart alone
// would erase every target in the company on the next config apply.
func TestAChartApplyCarriesTheTargetThrough(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	target := "2026-12-18"
	if _, err := r.writer.WriteProject(t.Context(), "op-target", "ENG",
		worklet.ProjectEdit{TargetDate: &target}, worklet.ProjectAuthority{Policy: true}); err != nil {
		t.Fatalf("set the target: %v", err)
	}
	r.drain()
	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []worklet.ChartProject{{
		Key: "ENG", Name: "Engineering, renamed",
	}}); err != nil {
		t.Fatalf("apply a chart: %v", err)
	}
	r.drain()
	detail := r.project(worklet.ProjectDetailQuery{Project: "ENG"})
	if detail.Name != "Engineering, renamed" || detail.TargetDate != target {
		t.Errorf("after a chart apply the project is %q with target %q, want the "+
			"new name and the lead's target kept", detail.Name, detail.TargetDate)
	}
}

// AN ABSENT TARGET SORTS LAST IN BOTH DIRECTIONS, and the rest in calendar
// order.
func TestAProjectListingSortsByTargetWithNoneLast(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations",
		TargetDate: "2026-12-01"})
	seedProject(t, r, worklet.Project{Key: "AAA", Name: "Anything",
		TargetDate: "2026-10-01"})
	// ENG, the harness's own, has none.
	for _, c := range []struct {
		descending bool
		want       string
	}{
		{false, "AAA,OPS,ENG"},
		{true, "OPS,AAA,ENG"},
	} {
		got := strings.Join(keys(r.projects(worklet.ProjectQuery{
			Archived: worklet.ArchivedExclude,
			Sort:     worklet.ProjectSortTarget, Descending: c.descending,
		})), ",")
		if got != c.want {
			t.Errorf("sort by target descending=%v answers %s, want %s — a "+
				"project with no target is not the soonest one", c.descending,
				got, c.want)
		}
	}
}
