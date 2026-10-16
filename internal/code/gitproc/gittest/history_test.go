package gittest_test

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// A HISTORY IS THE SAME COMMITS ON EVERY RUN, written in HistorySessions
// sessions of equal commits, each commit changing at most four files of one
// directory — so a benchmark that pushes it a session at a time pushes the
// same history wherever it runs, and two runs' figures are of one history.
func TestAHistoryIsTheSameCommitsOnEveryRun(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	// THREE FILES: a session commits one version of each in turn, so a
	// commit of fewer than four files still holds every file's version.
	const size = 3 * gittest.HistorySessions * 11_800
	src, first := gittest.History(t, c, size)
	_, again := gittest.History(t, c, size)
	if !slices.Equal(first, again) {
		t.Fatalf("two histories of one size differ: %d commits ending %s, and %d ending %s",
			len(first), first[len(first)-1], len(again), again[len(again)-1])
	}
	if len(first) == 0 || len(first)%gittest.HistorySessions != 0 {
		t.Fatalf("a history of %d commits is not %d sessions of equal commits", len(first), gittest.HistorySessions)
	}
	// A COMMIT MADE ON TOP OF IT AT After IS NEWER THAN ITS PARENT, and the
	// next one newer still.
	work := t.TempDir()
	c.Run(t, "", "clone", "--quiet", src, work)
	tip := first[len(first)-1]
	for n := range 2 {
		date := gittest.After(first, n)
		made := c.Env("GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date).Commit(t, work,
			map[string]string{"after.txt": fmt.Sprint(n)}, "after")
		parent, child := committed(t, c, work, tip), committed(t, c, work, made)
		if child <= parent {
			t.Fatalf("a commit at After(%d) is dated %d, not after its parent's %d", n, child, parent)
		}
		tip = made
	}
	// ONE MADE AT Skewed IS OLDER THAN ITS PARENT AND THAN THE HISTORY'S
	// FIRST COMMIT: a clock behind the whole history.
	skewed := c.Env("GIT_AUTHOR_DATE="+gittest.Skewed(), "GIT_COMMITTER_DATE="+gittest.Skewed()).Commit(t, work,
		map[string]string{"skewed.txt": "behind"}, "skewed")
	if at, oldest := committed(t, c, work, skewed), committed(t, c, work, first[0]); at >= oldest {
		t.Fatalf("a commit at Skewed is dated %d, not before the history's first commit at %d", at, oldest)
	}
	for _, commit := range []string{first[0], first[len(first)/2], first[len(first)-1]} {
		files := strings.Fields(c.Run(t, src, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root", commit))
		dirs := map[string]bool{}
		for _, f := range files {
			dirs[path.Dir(f)] = true
		}
		if len(files) == 0 || len(files) > 4 || len(dirs) != 1 {
			t.Errorf("commit %s changes %v: want one to four files of one directory", commit, files)
		}
	}
}

// committed is commit's committer date in dir, in seconds.
func committed(t *testing.T, c *gittest.Client, dir, commit string) int64 {
	t.Helper()
	at, err := strconv.ParseInt(strings.TrimSpace(c.Run(t, dir, "log", "-1", "--format=%ct", commit)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
