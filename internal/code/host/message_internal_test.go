package host

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitref"
)

// defaultMessageRows are messages a commit may carry as git stores them — what
// commit-tree, a client's editor or another tool wrote — each a case of the
// rules a revert's and a cherry-pick's default message follow, and a pushed
// change's default title (TestAChangesDefaultTitleIsGitLogsSubject).
var defaultMessageRows = []string{
	"subject only",
	"subject\n\nbody\n\nSigned-off-by: x <x@example.com>\n",
	"subject\n\n(cherry picked from commit 1111111111111111111111111111111111111111)\n",
	"subject   \n\n\n\nbody  \n# a comment line\n\n\n",
	"\n\n  \nsubject after blank lines\nsecond line\n",
	"Revert \"foo\"\n\nThis reverts commit 2222222222222222222222222222222222222222.\n",
	"Revert \"Revert \"foo\"\"\n\nThis reverts commit 3333333333333333333333333333333333333333.\n",
	"Revert \"foo   \n\nbody\r\nmore\r\n",
	"subject\n\nKey: v\nnot a trailer\nnot either\nnor this\n",
	"subject\n\nSigned-off-by: x <x@example.com>\nnot a trailer\nnot either\n",
	"subject\n\nSigned-off-by: x <x@example.com>\nw\nx\ny\nz\n",
	"subject\n\nKey: v\n continued\nOther: w\n",
	"subject\n\n# only a comment\n",
	"subject\n\nSigned-off-by: a <a@example.com>\n# trailing comment\n\n",
	"subject\n\nSigned-off-by: a <a@example.com>\n" +
		"# ------------------------ >8 ------------------------\nbelow the scissors\n",
	"subject\n\nConflicts:\n\tf.txt\n",
	"Key: v",
	"",
	"\n\n\n",
	"subject\twith a tab\r\n\nbody\n",
	"caf\xe9 subject\n\nbody \xff\n",
	"subject \ufffe\n",
	"subject\x00hidden past a NUL\n",
}

// A REVERT'S AND A CHERRY-PICK'S DEFAULT MESSAGE ARE GIT'S OWN: for a commit
// carrying each message as git stores it — written with no rewrite of its
// bytes, a Latin-1 one and one holding a NUL among them — what `git revert
// --no-edit` and `git cherry-pick -x` commit is, byte for byte, what
// revertMessage and pickMessage write, as git then writes them into a commit
// (gitref.WrittenCommitText).
//
// Mutations: clean a revert's message up as a pick's is — the trailing spaces
// of "Revert \"foo   " are taken off, red; read the trailer block's lines
// without git's prefixes — the cherry-picked footer row gains a blank line,
// red; ignore the scissors — the row below them reads as a block, red.
func TestTheDefaultMessagesAreGitsOwn(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	dir := c.Init(t)
	base := c.Commit(t, dir, map[string]string{"f.txt": "base\n"}, "base")
	for i, msg := range defaultMessageRows {
		t.Run(fmt.Sprintf("row %d", i), func(t *testing.T) {
			c.Run(t, dir, "read-tree", base)
			name := fmt.Sprintf("g%d.txt", i)
			blob, stderr, err := c.Try(t, dir, strings.NewReader(name+"\n"), "hash-object", "-w", "--stdin")
			if err != nil {
				t.Fatalf("hash-object: %v\n%s", err, stderr)
			}
			c.Run(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(blob)+","+name)
			tree := strings.TrimSpace(c.Run(t, dir, "write-tree"))
			raw := fmt.Sprintf("tree %s\nparent %s\nauthor A U Thor <author@example.com> 1700000000 +0000\n"+
				"committer C O Mitter <committer@example.com> 1700000000 +0000\n\n%s", tree, base, msg)
			out, stderr, err := c.Try(t, dir, strings.NewReader(raw), "hash-object", "-t", "commit", "-w",
				"--literally", "--stdin")
			if err != nil {
				t.Fatalf("hash-object of the commit: %v\n%s", err, stderr)
			}
			commit := strings.TrimSpace(out)
			stored := messageOf(t, c, dir, commit)

			c.Run(t, dir, "checkout", "-q", "-f", "-B", fmt.Sprintf("revert-%d", i), commit)
			if _, stderr, err := c.Try(t, dir, nil, "revert", "--no-edit", commit); err != nil {
				t.Fatalf("git revert: %v\n%s", err, stderr)
			}
			if got, want := gitref.WrittenCommitText(revertMessage(commit, stored)),
				messageOf(t, c, dir, "HEAD"); got != want {
				t.Errorf("of a commit whose message is %q, the revert's message is\n%q\ngit writes\n%q", stored,
					got, want)
			}

			c.Run(t, dir, "checkout", "-q", "-f", "-B", fmt.Sprintf("pick-%d", i), base)
			if _, stderr, err := c.Try(t, dir, nil, "cherry-pick", "-x", commit); err != nil {
				t.Fatalf("git cherry-pick -x: %v\n%s", err, stderr)
			}
			if got, want := gitref.WrittenCommitText(pickMessage(commit, stored)),
				messageOf(t, c, dir, "HEAD"); got != want {
				t.Errorf("of a commit whose message is %q, the pick's message is\n%q\ngit writes\n%q", stored,
					got, want)
			}
		})
	}
}

// messageOf is a commit's message as git stores it: everything past its
// header's blank line.
func messageOf(t *testing.T, c *gittest.Client, dir, rev string) string {
	t.Helper()
	raw := c.Run(t, dir, "cat-file", "commit", rev)
	_, msg, found := strings.Cut(raw, "\n\n")
	if !found {
		return ""
	}
	return msg
}

// A TRAILER BLOCK IS GIT'S, as its trailer iterator reads one with no divider:
// the last paragraph past the title, all trailers or a quarter of them with
// one git writes itself, its continuation lines its own — and never the
// title, nor what follows a comment or the scissors.
func TestATrailerBlockIsGits(t *testing.T) {
	t.Parallel()
	for msg, want := range map[string]bool{
		"":                                                false,
		"subject\n":                                       false,
		"Signed-off-by: a <a@b>\n":                        false,
		"subject\n\nSigned-off-by: a <a@b>\n":             true,
		"subject\n\nKey: v\n":                             true,
		"subject\n\nKey: v\nplain\n":                      false,
		"subject\n\nKey : v\n":                            true,
		"subject\n\n Key: v\n":                            false,
		"subject\n\nKey: v\n continued\n":                 true,
		"subject\n\n(cherry picked from commit 1)\n":      true,
		"subject\n\nSigned-off-by: a <a@b>\na\nb\nc\n":    true,
		"subject\n\nSigned-off-by: a <a@b>\na\nb\nc\nd\n": false,
		"subject\n\nKey: v\n\n# comment\n":                true,
		"subject\n\nbody\n\nKey: v\n\n\n":                 true,
		"subject\n\nKey: v\n# ------------------------ >8 ------------------------\nx\n": true,
	} {
		if got := conformingFooter(msg); got != want {
			t.Errorf("conformingFooter(%q) = %t, want %t", msg, got, want)
		}
	}
}
