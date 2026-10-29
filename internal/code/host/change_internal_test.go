package host

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// A PUSHED CHANGE'S DEFAULT TITLE IS WHAT `git log` SHOWS OF ITS COMMIT: for a
// commit carrying each message as git stores it (defaultMessageRows) — blank
// lines before a subject wrapped over two lines, a Latin-1 one and one holding
// a NUL among them — what logSubject reads of the commit object as cat-file
// prints it is, byte for byte, what `git log --format=%s` prints of it.
//
// Mutations: read the message from its first line, blank or not — the row
// opening with blank lines has no subject, red; read past a NUL — the row
// holding one keeps what follows it, red; take the subject's first line alone
// — the wrapped subject loses its second, red.
func TestAChangesDefaultTitleIsGitLogsSubject(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	dir := c.Init(t)
	tree := strings.TrimSpace(c.Run(t, dir, "write-tree"))
	for i, msg := range defaultMessageRows {
		t.Run(fmt.Sprintf("row %d", i), func(t *testing.T) {
			raw := fmt.Sprintf("tree %s\nauthor A U Thor <author@example.com> 1700000000 +0000\n"+
				"committer C O Mitter <committer@example.com> 1700000000 +0000\n\n%s", tree, msg)
			out, stderr, err := c.Try(t, dir, strings.NewReader(raw), "hash-object", "-t", "commit", "-w",
				"--literally", "--stdin")
			if err != nil {
				t.Fatalf("hash-object of the commit: %v\n%s", err, stderr)
			}
			commit := strings.TrimSpace(out)
			want := strings.TrimSuffix(c.Run(t, dir, "log", "-1", "--format=%s", commit), "\n")
			if got := logSubject([]byte(c.Run(t, dir, "cat-file", "commit", commit))); got != want {
				t.Errorf("of a commit whose message is %q, the subject read is %q; git log shows %q", msg, got,
					want)
			}
		})
	}
}
