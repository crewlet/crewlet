package query_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/gitref"
)

// gitBlame is git's own blame of path at rev in the person's work tree: the
// commit of each line, by its number.
func (x *fixture) gitBlame(rev, path string) map[int]string {
	x.t.Helper()
	out := map[int]string{}
	for _, line := range strings.Split(x.git("blame", "--porcelain", rev, "--", path), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && gitref.ValidOID(fields[0]) {
			n, err := strconv.Atoi(fields[2])
			if err != nil {
				x.t.Fatalf("git blame wrote the header %q", line)
			}
			out[n] = fields[0]
		}
	}
	return out
}

// A BLAME NAMES EVERY LINE'S COMMIT AND PUSH: each line of the file, in order,
// with the commit git's own blame names for it and the line it had there —
// its old path where the file was renamed since — each commit once a page
// with its author, its date and its subject, and named with the push that
// introduced it. Read a few lines a page, the pages hold the range whole,
// each naming exactly the commits its lines do, each `l:` cursor resuming at
// the line the page before it ended before — and the range is blamed by ONE
// git process, every page past the first answered from the memo. A range
// names its lines alone; one past the file's end is refused naming how many
// lines the file has; a directory is no file; and an empty file has no
// lines.
//
// Mutations: the page's commits left out of its cost (a page past its bound)
// → red; the pages after the asked one not memoized → a blame each page, red.
func TestABlameNamesEveryLinesCommitAndPush(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	lines := func(changed map[int]string) string {
		var b strings.Builder
		for i := 1; i <= 6; i++ {
			if s, ok := changed[i]; ok {
				b.WriteString(s + "\n")
			} else {
				b.WriteString("line " + strconv.Itoa(i) + "\n")
			}
		}
		return b.String()
	}
	first := x.at(1).Commit(t, x.work, map[string]string{"a.txt": lines(nil), "dir/x": "x\n", "empty.txt": ""},
		"add a\n\nwith a body")
	second := x.at(2).Commit(t, x.work, map[string]string{"a.txt": lines(map[int]string{2: "two", 5: "five"})},
		"change two lines")
	x.git("mv", "a.txt", "b.txt")
	x.at(3).Commit(t, x.work, nil, "rename a to b")
	fourth := x.at(4).Commit(t, x.work, map[string]string{"b.txt": lines(map[int]string{2: "two", 5: "five",
		6: "six"})}, "change the last line")
	x.push(codetest.RepoName, "main")
	repo := x.repo(codetest.RepoName)
	x.synced(0, repo)

	want := x.gitBlame(fourth, "b.txt")
	if want[1] != first || want[2] != second || want[6] != fourth {
		t.Fatalf("git's own blame names %v; the history is not the one this case built", want)
	}
	q := blameQ(repo, code.BlameQuery{Rev: fourth, Path: "b.txt"})
	limit := x.decoration(q) + 1800
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	query.SetPageBytes(s, limit)
	got := pages(t, s, q, limit)
	if len(got) < 2 {
		t.Fatalf("the blame read at %d bytes a page took %d page, want several", limit, len(got))
	}
	line := 0
	for i, p := range got {
		b := p.Blame
		named := map[string]bool{}
		for _, l := range b.Lines {
			line++
			named[l.Commit] = true
			switch {
			case l.Line != line:
				t.Fatalf("page %d holds line %d where line %d belongs", i, l.Line, line)
			case l.Commit != want[line]:
				t.Errorf("line %d names %s, want git's %s", line, l.Commit, want[line])
			case l.Commit != fourth && l.OrigPath != "a.txt":
				t.Errorf("line %d, from before the rename, names its old path %q, want a.txt", line, l.OrigPath)
			case l.Commit == fourth && l.OrigPath != "":
				t.Errorf("line %d, of the file as it is named, names an old path %q", line, l.OrigPath)
			case l.Text != strings.TrimSuffix(strings.Split(lines(map[int]string{2: "two", 5: "five", 6: "six"}),
				"\n")[line-1], "\n"):
				t.Errorf("line %d reads %q", line, l.Text)
			}
		}
		if len(b.Lines) == 0 || b.StartLine != b.Lines[0].Line || b.EndLine != b.Lines[len(b.Lines)-1].Line {
			t.Errorf("page %d says lines %d to %d of %d lines", i, b.StartLine, b.EndLine, len(b.Lines))
		}
		if len(b.Commits) != len(named) {
			t.Errorf("page %d names %d commits for lines naming %d", i, len(b.Commits), len(named))
		}
		for id := range named {
			c, ok := b.Commits[id]
			if !ok || c.Author != "Crewlet Test" || c.AuthorEmail != "test@example.com" || c.Subject == "" {
				t.Errorf("page %d names commit %s as %+v", i, id, c)
			}
			if _, ok := p.PushedBy[id]; !ok {
				t.Errorf("page %d names commit %s with no push", i, id)
			}
		}
		if c := b.Commits[first]; c.Subject != "" && c.Subject != "add a" {
			t.Errorf("the first commit's subject reads %q, want its first line", c.Subject)
		}
		if i > 0 && !p.Memo {
			t.Errorf("page %d of the range was not answered from the memo", i)
		}
	}
	if line != 6 {
		t.Errorf("the pages hold %d lines, want the file's 6", line)
	}
	if n := len(slices.DeleteFunc(rec.ran(), func(c gitproc.Cmd) bool { return c.Args[0] != "blame" })); n != 1 {
		t.Errorf("the range ran %d blames, want one", n)
	}

	s = x.server(0, query.Options{})
	ranged := mustRead(t, s, blameQ(repo, code.BlameQuery{Rev: fourth, Path: "b.txt", StartLine: 2, EndLine: 4}))
	if b := ranged.Blame; len(b.Lines) != 3 || b.StartLine != 2 || b.EndLine != 4 || ranged.Next != "" {
		t.Errorf("lines 2 to 4 answered %d lines, %d to %d, next %q", len(b.Lines), b.StartLine, b.EndLine,
			ranged.Next)
	}
	for _, tc := range []struct {
		name string
		q    code.BlameQuery
		err  error
		says string
	}{
		{"past the end", code.BlameQuery{Rev: fourth, Path: "b.txt", StartLine: 9}, code.ErrInvalid, "which has 6 lines"},
		{"a directory", code.BlameQuery{Rev: fourth, Path: "dir"}, code.ErrNotABlob, "names a directory"},
		{"nothing", code.BlameQuery{Rev: fourth, Path: "nothing.txt"}, code.ErrNotABlob, "names no file"},
		{"past an empty file's end", code.BlameQuery{Rev: fourth, Path: "empty.txt", StartLine: 2}, code.ErrInvalid,
			"which has 0 lines"},
		{"a range too long", code.BlameQuery{Rev: fourth, Path: "b.txt", StartLine: 1, EndLine: 501}, code.ErrInvalid,
			"at most 500"},
	} {
		_, err := s.Read(t.Context(), blameQ(repo, tc.q))
		if !errors.Is(err, tc.err) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want %v saying %q", tc.name, err, tc.err, tc.says)
		}
	}
	empty := mustRead(t, s, blameQ(repo, code.BlameQuery{Rev: fourth, Path: "empty.txt"}))
	if b := empty.Blame; len(b.Lines) != 0 || b.StartLine != 0 || b.EndLine != 0 || empty.Next != "" {
		t.Errorf("an empty file's blame answered %+v", b)
	}
}

// A LONG SUBJECT AND LINE ARE DESCRIBED BY THEIR SIZE, never cut: a subject of
// 5 000 bytes — and one of a thousand control bytes, a thousand bytes that
// encode to six thousand — an author's name past a kibibyte and a line of
// 9 000 bytes are each answered absent beside their size, by a log and by a
// blame alike, and the page still fits its bound.
//
// Mutation: a field past its bound answered whole → the page past the bytes
// it was cut against, and the absent field present, red.
func TestALongSubjectAndLineAreDescribedBySize(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	long := strings.Repeat("s", 5000)
	control := strings.Repeat("\x01", 1000)
	name := strings.Repeat("n", 1100)
	commitWith := func(subject string, files map[string]string, author string) string {
		t.Helper()
		for path, content := range files {
			if err := os.WriteFile(filepath.Join(x.work, path), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		message := filepath.Join(t.TempDir(), "message")
		if err := os.WriteFile(message, []byte(subject+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git := x.p.Git()
		if author != "" {
			git = git.Env("GIT_AUTHOR_NAME=" + author)
		}
		git.Run(t, x.work, "add", "-A")
		git.Run(t, x.work, "commit", "-q", "-F", message)
		return strings.TrimSpace(x.git("rev-parse", "HEAD"))
	}
	longLine := strings.Repeat("l", 9000)
	commitWith(long, map[string]string{"f.txt": "short\n" + longLine + "\n"}, "")
	commitWith(control, map[string]string{"g.txt": "g\n"}, "")
	head := commitWith("named at length", map[string]string{"h.txt": "h\n"}, name)
	x.push(codetest.RepoName, "main")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	a := mustRead(t, s, logQ(repo, code.LogQuery{Rev: head}))
	if n := encoded(t, a); n > code.MaxGitAnswerBytes {
		t.Errorf("the log's page encodes to %d bytes, past %d", n, code.MaxGitAnswerBytes)
	}
	c := a.Log.Commits
	switch {
	case len(c) != 3:
		t.Fatalf("the log lists %d commits, want 3", len(c))
	case c[0].Author != "" || c[0].AuthorBytes != len(name) || c[0].Subject != "named at length":
		t.Errorf("the long author reads %q, %d bytes", c[0].Author, c[0].AuthorBytes)
	case c[1].Subject != "" || c[1].SubjectBytes != len(control):
		t.Errorf("the control bytes' subject reads %q, %d bytes", c[1].Subject, c[1].SubjectBytes)
	case c[2].Subject != "" || c[2].SubjectBytes != len(long) || c[2].Author != "Crewlet Test":
		t.Errorf("the long subject reads %q, %d bytes", c[2].Subject, c[2].SubjectBytes)
	}

	b := mustRead(t, s, blameQ(repo, code.BlameQuery{Rev: head, Path: "f.txt"}))
	if n := encoded(t, b); n > code.MaxGitAnswerBytes {
		t.Errorf("the blame's page encodes to %d bytes, past %d", n, code.MaxGitAnswerBytes)
	}
	lines := b.Blame.Lines
	switch {
	case len(lines) != 2:
		t.Fatalf("the blame names %d lines, want 2", len(lines))
	case lines[0].Text != "short" || lines[0].TextBytes != 0:
		t.Errorf("the short line reads %q, %d bytes", lines[0].Text, lines[0].TextBytes)
	case lines[1].Text != "" || lines[1].TextBytes != len(longLine):
		t.Errorf("the long line reads %q, %d bytes", lines[1].Text, lines[1].TextBytes)
	}
	if commit := b.Blame.Commits[lines[0].Commit]; commit.Subject != "" || commit.SubjectBytes != len(long) {
		t.Errorf("the blamed commit's long subject reads %q, %d bytes", commit.Subject, commit.SubjectBytes)
	}
}
