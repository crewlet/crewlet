package usage_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/usage"
)

// EVERY PAGED STATEMENT SEEKS ITS KEY AND STREAMS — the plan gate behind "a
// page pays for its own rows".
//
// A page stops where it ends ([usage.Fill]), which saves something only if
// the statement had not already read further: a plan that SORTS reads every
// matching row first, and a seek wider than its key reads what the keyset
// filters out (BenchmarkAUsageWindowInPages, in the package doc). So each
// statement, in the shape of a page resumed INSIDE its key, is planned over a
// seeded window and must seek its leading key with no sorter or temporary
// tree. A wall-clock ratio cannot hold this: the race detector's per-row cost
// hides the engine's; the plan shows the re-reading at any size.
func TestEveryPagedStatementSeeksItsKeyAndStreams(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	shape := company{days: 12, nodes: 2, seats: 16, people: 4, cells: 3, pages: 4}
	shape.seed(t, db)
	day := shape.span().From
	statements := usage.PagedStatements(day, "node-01", "seat-0007", "person-02", "page-002", "repo-03")
	for _, name := range slices.Sorted(maps.Keys(statements)) {
		s := statements[name]
		plan := storetest.Plan(t, db.Replicated(), s.Text, s.Args)
		seek := "(day=?)"
		switch {
		case strings.HasPrefix(name, "the next day of"):
			seek = "AND day<=?)"
		case strings.HasPrefix(name, "a page's reads"):
			seek = "USING INDEX usage_reads_page_idx (page_id=? AND day>=? AND day<=?)"
		case strings.HasPrefix(name, "the next principal kind"):
			seek = "usage_repo_reads_repo_idx (repo_id=? AND principal_kind>"
		case strings.HasPrefix(name, "a repository's readers"):
			// THE PRINCIPAL THE PAGE RESUMES AT IS SOUGHT, never a
			// filter over every reader before it.
			seek = "usage_repo_reads_repo_idx (repo_id=? AND principal_kind=? AND principal>"
		}
		if !strings.Contains(plan[0], "SEARCH") || !strings.Contains(plan[0], seek) {
			t.Errorf("%s does not seek its key %s:\n%s", name, seek, strings.Join(plan, "\n"))
		}
		for _, step := range plan {
			if strings.Contains(step, "SORTER") || strings.Contains(step, "TEMP B-TREE") {
				t.Errorf("%s sorts before it hands back a row — a page that stops it "+
					"has read past its own rows:\n%s", name, strings.Join(plan, "\n"))
			}
		}
	}
}

// A REPOSITORY'S ELIDED READERS ARE COUNTED OFF ITS OWN DAYS: a seek of the
// repository and the range on the head rows' repository index, never a walk of
// every repository's days in the range, which is what the head rows' day-first
// key alone would read.
//
// Mutation: drop usage_repo_read_days_repo_idx (replicated 0093), and the plan
// seeks the primary key's days and reads every repository's head rows in them.
func TestARepositorysElidedReadersAreCountedOffItsOwnDays(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	plan := storetest.Plan(t, db.Replicated(), usage.RepoReadsElidedStatement,
		[]any{"repo-03", "2026-09-01", "2026-09-30"})
	if len(plan) != 1 || !strings.Contains(plan[0],
		"usage_repo_read_days_repo_idx (repo_id=? AND day>") {
		t.Errorf("the elided count's plan is\n%s\nwant one seek of the repository's days",
			strings.Join(plan, "\n"))
	}
}
