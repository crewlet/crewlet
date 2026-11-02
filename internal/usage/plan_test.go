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
// A paged read stops reading where its page ends ([usage.Fill]), and that saves
// something only if the statement it stops had not already read further: a
// statement whose plan SORTS hands back its first row after reading every row
// it matches, and one that seeks a range wider than its key reads whatever the
// keyset filters out. Both were the case — one statement over the whole window,
// a seek on its days, a join and a sorter — so every page of a window paid for
// the window from its resumption to the end: walked in nineteen pages, a
// ninety-day spend window cost 2.5 whole reads, where it costs 1.1 now
// (BenchmarkAUsageWindowInPages, recorded in the package doc).
//
// So every statement a page issues, in the shape a page that resumes INSIDE
// its key issues it, is planned over a seeded window, through storetest's one reading of a plan,, and each must seek its
// leading key — a company day with `day = ?`, a page id from the day it resumes
// on, a day's index head for the walk's next day — with no sorter and no
// temporary tree anywhere in its plan: rows come back in the read's order as
// the index holds them, so a statement stopped at a page's end has read that
// page's rows and at most the one key's it resumed in.
//
// A wall-clock ratio of a paged walk to the whole read cannot hold this in a
// test, which is why the gate reads the plan: at a window a test can seed
// (twelve days, two nodes, sixteen seats) the old statements walked at 1.3 to
// 1.7 times the whole read under the race detector — whose per-row cost in the
// driver hides the engine's — exactly as the new ones did, and at about 3
// times without it. The re-reading shows at a company's size, and the plan
// that causes it shows at any size.
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
// Mutation: drop usage_repo_read_days_repo_idx (replicated 0088), and the plan
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
