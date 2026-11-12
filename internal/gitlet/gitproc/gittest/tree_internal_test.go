package gittest

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A TREE IS ITS FILES, EACH OF HISTORY'S SIZE, IN DIRECTORIES OF HISTORY'S
// WIDTH, AND THE SAME COMMITS ON EVERY RUN — so a benchmark over a tree of a
// width measures one tree wherever it runs, of one shape of file with a
// history — written a commit per perCommit files, each adding the next ones
// and the last holding every one, so a tree pushed a commit at a time is the
// whole tree at its tip.
//
// Mutations: a directory of 49 files — d00001 holds 49, red; a seed drawn
// afresh for each tree — two trees of one width differ, red; a later commit
// started from no parent — the tip holds only its own forty files, red.
func TestATreeIsItsFilesInDirectoriesOfAHistorysWidth(t *testing.T) {
	t.Parallel()
	c := NewClient(t)
	const files, perCommit = 100, 40
	src, commits := tree(t, c, files, perCommit)
	_, again := tree(t, c, files, perCommit)
	if !slices.Equal(commits, again) {
		t.Fatalf("two trees of %d files are different commits: %v and %v", files, commits, again)
	}
	if len(commits) != 3 {
		t.Fatalf("a tree of %d files in commits of %d is %d commits, want 3", files, perCommit, len(commits))
	}

	// THE TIP HOLDS EVERY FILE, each historyFileBytes of its own bytes, in
	// directories of historyDirFiles.
	perDir := map[string]int{}
	blobs := map[string]bool{}
	var paths []string
	for line := range strings.Lines(c.Run(t, src, "ls-tree", "-r", "-l", "main")) {
		// <mode> SP <type> SP <object> SP <size> TAB <path>
		meta, path, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			t.Fatalf("ls-tree wrote %q", line)
		}
		if size, err := strconv.Atoi(fields[3]); err != nil || size != historyFileBytes {
			t.Errorf("%s is %s bytes, want %d", path, fields[3], historyFileBytes)
		}
		blobs[fields[2]] = true
		paths = append(paths, path)
		dir, _, _ := strings.Cut(path, "/")
		perDir[dir]++
	}
	want := make([]string, files)
	for f := range files {
		want[f] = TreeFile(f)
	}
	if !slices.Equal(paths, want) {
		t.Errorf("the tip holds %v, want %v", paths, want)
	}
	if len(blobs) != files {
		t.Errorf("the tip's %d files are %d contents, want one each", files, len(blobs))
	}
	if wantDirs := map[string]int{"d00000": historyDirFiles, "d00001": historyDirFiles,
		"d00002": files - 2*historyDirFiles}; !maps.Equal(perDir, wantDirs) {
		t.Errorf("the tip's directories hold %v, want %v", perDir, wantDirs)
	}
	if got := TreeDir(historyDirFiles - 1); got != "d00000" {
		t.Errorf("the first directory's last file is in %s, want d00000", got)
	}

	// EACH COMMIT ADDS ITS OWN FILES AND NOTHING ELSE.
	for n, commit := range commits {
		added := strings.Fields(c.Run(t, src, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root",
			"--diff-filter=A", commit))
		from, to := n*perCommit, min((n+1)*perCommit, files)
		if !slices.Equal(added, want[from:to]) {
			t.Errorf("commit %d of %d adds %v, want files %d to %d", n, len(commits), added, from, to-1)
		}
		if changed := strings.Fields(c.Run(t, src, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root",
			"--diff-filter=MD", commit)); len(changed) != 0 {
			t.Errorf("commit %d changes or deletes %v, want only additions", n, changed)
		}
	}
}
