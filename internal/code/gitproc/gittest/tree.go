package gittest

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// A WORK TREE'S WIDTH, for every benchmark whose figure follows the files one
// checkout holds rather than the history behind it: what `git status`, a
// commit of one changed file and a checkout cost follows how many files and
// directories a tree has, and a History's trees are narrow — 1 421 files at a
// gigabyte — because a history's size is in its versions. ONE GENERATOR, of
// History's composition: files of historyFileBytes seeded random bytes in
// directories of historyDirFiles, so a figure over a tree and one over a
// history are of one shape of file.

// TreeCommitFiles is the most files one commit of a Tree adds. A wider tree
// is written as a commit per TreeCommitFiles files, each adding the next ones,
// so its last commit holds every file — and a benchmark that pushes it a
// commit at a time stays under the code host's largest push
// (config.CodeMaxPushBytesCeiling, 1 GiB): a hundred thousand files of 4 KiB
// are a 410 MB pack, and a million would be 4.1 GB in one.
const TreeCommitFiles = 100_000

// treeSeed keys a Tree's bytes: fixed, so a width's tree is the same commits
// on every run, and apart from historySeed, so no tree's file is a history's.
var treeSeed = [32]byte{'t', 'r', 'e', 'e'}

// TreeDir is the directory file f of a Tree lies in — one of historyDirFiles
// files each, numbered from d00000 — so a benchmark that narrows a checkout to
// one directory names the first, which holds the tree's first historyDirFiles
// files.
func TreeDir(f int) string { return fmt.Sprintf("d%05d", f/historyDirFiles) }

// TreeFile is the path of file f of a Tree, within its TreeDir.
func TreeFile(f int) string { return fmt.Sprintf("%s/f%07d", TreeDir(f), f) }

// Tree fast-imports a tree of files files into a repository the engine's way
// (Empty) of its own, and answers the repository and main's commits, oldest
// first: one commit for every TreeCommitFiles files, each written by a
// fast-import session of its own and its last holding them all, every one
// dated by History's clock.
func Tree(t testing.TB, c *Client, files int) (string, []string) {
	t.Helper()
	return tree(t, c, files, TreeCommitFiles)
}

// tree is Tree with each commit adding at most perCommit files.
func tree(t testing.TB, c *Client, files, perCommit int) (string, []string) {
	t.Helper()
	if files < 1 || perCommit < 1 {
		t.Fatalf("a tree of %d files in commits of %d is no tree", files, perCommit)
	}
	src := Empty(t)
	data := rand.NewChaCha8(treeSeed)
	content := make([]byte, historyFileBytes)
	for n, from := 0, 0; from < files; n, from = n+1, from+perCommit {
		to := min(from+perCommit, files)
		FastImport(t, c, src, func(w *bufio.Writer) error {
			msg := fmt.Sprintf("files %d to %d of %d\n", from, to-1, files)
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter %s %d +0000\ndata %d\n%s", historyIdent,
				historyEpoch+n, len(msg), msg)
			if n > 0 {
				_, _ = w.WriteString("from refs/heads/main^0\n")
			}
			for f := from; f < to; f++ {
				_, _ = data.Read(content)
				fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n", TreeFile(f), len(content))
				_, _ = w.Write(content)
				if err := w.WriteByte('\n'); err != nil {
					return err
				}
			}
			return w.WriteByte('\n')
		})
	}
	return src, strings.Fields(c.Run(t, src, "rev-list", "--reverse", "main"))
}
