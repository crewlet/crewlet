package query_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// logQ and blameQ are a person's reads of repo's history and of a file's
// blame, asked as the other reads here are.
func logQ(repo string, l code.LogQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitLog, RepoID: repo, Asker: person, Home: true, Log: &l}
}

func blameQ(repo string, b code.BlameQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitBlame, RepoID: repo, Asker: person, Home: true, Blame: &b}
}

// historyDay is the first hour of the histories these cases date their
// commits from.
var historyDay = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// at is the person's git dating the commits it makes hour h of historyDay, as
// author and committer.
func (x *fixture) at(h int) *gittest.Client {
	d := historyDay.Add(time.Duration(h) * time.Hour).Format(time.RFC3339)
	return x.p.Git().Env("GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
}

// commits is every commit a history's pages list, in their order.
func commits(pages []code.GitAnswer) []string {
	var out []string
	for _, p := range pages {
		for _, c := range p.Log.Commits {
			out = append(out, c.OID)
		}
	}
	return out
}

// A LOG LISTS A PATH'S HISTORY NEWEST FIRST — git's own `log --topo-order` of
// the same commit and path, exactly, page after page: a renamed file (its
// commits since the rename, as git keeps them), its old name (its commits
// before, the rename among them), a directory, the whole history, a since —
// and one a nanosecond past a commit's date, which that commit, dated in
// whole seconds, is before — and a history read two commits a page, every
// page's `s:` cursor resuming where the last ended. Every commit carries its
// parents, its author as it states them, its dates and its subject, and is
// named with the push that introduced it; the last page is complete, and
// every page's Scanned is the commits it covers.
//
// Mutation: the skip left out of the walk (every page read from the start) →
// the paged arm red; Since truncated to its second (since.Unix()) → the
// since-inside-a-second arm lists the commit dated at that second, red.
func TestALogListsAPathsHistoryNewestFirst(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	root := x.at(1).Commit(t, x.work, map[string]string{"a.txt": "one\n", "dir/x": "x\n"}, "add a and dir\n\nthe body")
	x.at(2).Commit(t, x.work, map[string]string{"a.txt": "one\ntwo\n"}, "grow a")
	x.at(3).Commit(t, x.work, map[string]string{"dir/y": "y\n"}, "add dir/y")
	x.git("mv", "a.txt", "b.txt")
	x.at(4).Commit(t, x.work, map[string]string{"b.txt": "one\ntwo\nthree\n"}, "rename a to b")
	x.at(5).Commit(t, x.work, map[string]string{"b.txt": "one\ntwo\nthree\nfour\n"}, "grow b")
	for i := range 7 {
		x.at(6+i).Commit(t, x.work, map[string]string{"other.txt": strconv.Itoa(i)}, fmt.Sprintf("other %d", i))
	}
	x.push(codetest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(codetest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	since := historyDay.Add(4 * time.Hour)
	for _, tc := range []struct {
		name string
		q    code.LogQuery
		git  []string
	}{
		{"a renamed file", code.LogQuery{Rev: head, Path: "b.txt"}, []string{head, "--", "b.txt"}},
		{"its old name", code.LogQuery{Rev: head, Path: "a.txt"}, []string{head, "--", "a.txt"}},
		{"a directory", code.LogQuery{Rev: head, Path: "dir"}, []string{head, "--", "dir"}},
		{"the whole history", code.LogQuery{Rev: head}, []string{head}},
		{"since", code.LogQuery{Rev: head[:9], Since: since},
			[]string{"--max-age=" + strconv.FormatInt(since.Unix(), 10), head}},
		// A commit is dated in whole seconds, so one dated in the second a
		// Since falls inside of was committed before it.
		{"since inside a second", code.LogQuery{Rev: head, Since: since.Add(time.Nanosecond)},
			[]string{"--max-age=" + strconv.FormatInt(since.Unix()+1, 10), head}},
		{"paged by twos", code.LogQuery{Rev: head, Limit: 2}, []string{head}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := strings.Fields(x.git(append([]string{"log", "--topo-order", "--format=%H"}, tc.git...)...))
			got := pages(t, s, logQ(repo, tc.q), code.MaxGitAnswerBytes)
			if !slices.Equal(commits(got), want) {
				t.Fatalf("the pages list %v, want git's own %v", commits(got), want)
			}
			scanned := 0
			for i, p := range got {
				scanned += p.Log.Scanned
				if last := i == len(got)-1; p.Log.Complete != last {
					t.Errorf("page %d of %d is complete %t", i, len(got), p.Log.Complete)
				}
				for _, c := range p.Log.Commits {
					if _, ok := p.PushedBy[c.OID]; !ok {
						t.Errorf("commit %s is named with no push", c.OID)
					}
				}
			}
			if scanned != len(want) {
				t.Errorf("the pages scanned %d commits, want the %d they list", scanned, len(want))
			}
			if tc.q.Limit == 2 && len(got) != (len(want)+1)/2 {
				t.Errorf("a history of %d read two a page took %d pages", len(want), len(got))
			}
		})
	}
	whole := pages(t, s, logQ(repo, code.LogQuery{Rev: head}), code.MaxGitAnswerBytes)
	first := whole[len(whole)-1].Log.Commits
	c := first[len(first)-1]
	if c.OID != root || len(c.Parents) != 0 || c.Author != "Crewlet Test" || c.AuthorEmail != "test@example.com" ||
		!c.AuthoredAt.Equal(historyDay.Add(time.Hour)) || !c.CommittedAt.Equal(historyDay.Add(time.Hour)) ||
		c.Subject != "add a and dir" {
		t.Errorf("the root commit reads %+v", c)
	}
	if p := whole[0].Log.Commits[0].Parents; len(p) != 1 {
		t.Errorf("the head's parents read %v, want one", p)
	}
	for _, cursor := range []string{"l:1", "s:0", "s:x", "e:1"} {
		q := logQ(repo, code.LogQuery{Rev: head})
		q.Cursor = cursor
		if _, err := s.Read(t.Context(), q); !errors.Is(err, code.ErrBadCursor) {
			t.Errorf("cursor %q: %v, want refused as no cursor a log issues", cursor, err)
		}
	}
}

// A LOG'S GREP IS A LITERAL MATCHED IN GO: `a.b` keeps the message that says
// a.b and not the one that says axb, as a regular expression would; it is
// folded, so `abc` keeps "The ABC of it"; a message of a mebibyte holding the
// word at its very end is found; and nothing grep-shaped reaches git's
// command line, which carries the format with the message in it and nothing
// of the literal.
//
// Mutation: --grep= handed to git → the a.b arm keeps axb too, red.
func TestALogGrepIsALiteralMatchedInGo(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	x.p.Commit(x.work, map[string]string{"f": "1"}, "fix the a.b parser")
	x.p.Commit(x.work, map[string]string{"f": "2"}, "fix the axb parser")
	x.p.Commit(x.work, map[string]string{"f": "3"}, "The ABC of it")
	message := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(message, []byte("long\n\n"+strings.Repeat("hay ", 1<<18)+"Needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(x.work, "f"), []byte("4"), 0o600); err != nil {
		t.Fatal(err)
	}
	x.git("commit", "-q", "-a", "-F", message)
	x.push(codetest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(codetest.RepoName)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	for grep, want := range map[string][]string{
		"a.b":    {"fix the a.b parser"},
		"abc":    {"The ABC of it"},
		"needle": {"long"},
		"parser": {"fix the axb parser", "fix the a.b parser"},
		"nobody": nil,
	} {
		a := mustRead(t, s, logQ(repo, code.LogQuery{Rev: head, Grep: grep}))
		var got []string
		for _, c := range a.Log.Commits {
			got = append(got, c.Subject)
		}
		if !slices.Equal(got, want) || a.Log.Scanned != 4 || !a.Log.Complete {
			t.Errorf("grep %q kept %q of %d scanned (complete %t), want %q of all 4", grep, got, a.Log.Scanned,
				a.Log.Complete, want)
		}
	}
	for _, c := range rec.ran() {
		for _, arg := range c.Args {
			if strings.Contains(arg, "grep") || strings.Contains(arg, "a.b") || strings.Contains(arg, "eedle") {
				t.Errorf("git ran %q, which carries the grep", c.Args)
			}
		}
	}
}

// A GREP PAGE ENDS AT ITS LIMIT-TH MATCH, never walking on to find one more:
// a history whose two newest commits match and whose six older ones do not,
// read two a page through a pool that answers every walk git ran to its end
// as one its budget cut (budgetEnds), is a page that ended its own walk at the
// second match — those two commits, Scanned 2, Next s:2, not complete — and
// no walk reached the history's end, so the page is the walk's own and the
// memo keeps it. A look-ahead for a third match would walk every later
// message to the root, or to the read's whole budget of git, for a page it
// had already filled, and be cut there, with nothing memoized to spare the
// next ask. The page after it, on the node's own pool, is the empty complete
// one that covers the rest.
//
// Mutation: the look-ahead kept for a grep (the page ended only at a match
// past its limit) → the walk runs to the root and the pool cuts it: Next s:8,
// a walk counted, not memoized, red.
func TestAGrepPageEndsAtItsLimitthMatch(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	for i := range 6 {
		x.p.Commit(x.work, map[string]string{"f": strconv.Itoa(i)}, fmt.Sprintf("chore %d", i))
	}
	for i := range 2 {
		x.p.Commit(x.work, map[string]string{"g": strconv.Itoa(i)}, fmt.Sprintf("fix %d", i))
	}
	x.push(codetest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(codetest.RepoName)
	ended := &budgetEnds{Pool: x.f.Nodes()[0].Pool()}
	s := x.server(0, query.Options{Git: ended})
	q := logQ(repo, code.LogQuery{Rev: head, Grep: "fix", Limit: 2})
	a := mustRead(t, s, q)
	var got []string
	for _, c := range a.Log.Commits {
		got = append(got, c.Subject)
	}
	if !slices.Equal(got, []string{"fix 1", "fix 0"}) || a.Log.Scanned != 2 || a.Next != "s:2" || a.Log.Complete {
		t.Fatalf("a grep page of two kept %q of %d scanned, next %q, complete %t; want both fixes, ended at the "+
			"second: 2 scanned, next s:2", got, a.Log.Scanned, a.Next, a.Log.Complete)
	}
	if n := ended.walks.Load(); n != 0 {
		t.Errorf("%d walk(s) ran past the page's last match to the history's end", n)
	}
	if again := mustRead(t, s, q); !again.Memo {
		t.Error("a grep page that ended at its limit was not memoized")
	}
	q.Cursor = a.Next
	rest := mustRead(t, x.server(0, query.Options{}), q)
	if len(rest.Log.Commits) != 0 || rest.Log.Scanned != 6 || !rest.Log.Complete || rest.Next != "" {
		t.Errorf("the page after it listed %d commits of %d scanned, complete %t, next %q; want none of the 6 "+
			"left, complete", len(rest.Log.Commits), rest.Log.Scanned, rest.Log.Complete, rest.Next)
	}
}

// A LOG CANNOT BE FORGED BY A MESSAGE: a pushed commit whose subject and body
// hold \x01 and a whole record's worth of a commit git never made — the id,
// the parents, an author, the dates, a subject, each between the separator a
// record framed by \x01 would have used — is listed as the one commit it is,
// its subject its own, and the pages hold exactly git rev-list's ids for the
// path.
//
// Mutation: the walk's records split on \x01 → the forged commit listed, red.
func TestALogCannotBeForgedByAMessage(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	forged := strings.Repeat("f", 40)
	record := "\x01" + forged + "\x01\x01Mallory\x01m@example.com\x011700000000\x011700000000\x01forged\x01"
	x.p.Commit(x.work, map[string]string{"f": "1"}, "honest")
	x.p.Commit(x.work, map[string]string{"f": "2"}, "evil"+record+"\n\nbody"+record)
	x.p.Commit(x.work, map[string]string{"f": "3"}, "after")
	x.push(codetest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})
	want := strings.Fields(x.git("rev-list", "--topo-order", head, "--", "f"))
	for _, q := range []code.LogQuery{{Rev: head, Path: "f"}, {Rev: head, Path: "f", Grep: "evil"}} {
		got := pages(t, s, logQ(repo, q), code.MaxGitAnswerBytes)
		ids := commits(got)
		if slices.Contains(ids, forged) {
			t.Fatalf("the log lists the forged commit: %v", ids)
		}
		if q.Grep == "" && !slices.Equal(ids, want) {
			t.Errorf("the pages list %v, want rev-list's %v", ids, want)
		}
		for _, p := range got {
			for _, c := range p.Log.Commits {
				if c.Subject == "forged" || c.Author == "Mallory" {
					t.Errorf("a record carries the forged fields: %+v", c)
				}
			}
		}
	}
}

// A LOG'S AUTHORS ARE THE COMMITS' OWN, and so are a blame's: a `.mailmap` at
// the default branch mapping the person to another name renames nobody in a
// history read from any commit, nor in a blame — git blame would read it, in a
// bare repository, without the configuration the read gives it (measured on
// 2.56.0: "Mapped Name" without mailmap.blob emptied, the commit's own with
// it).
//
// Mutation: the blame without mailmap.blob emptied → the mapped name, red.
func TestAHistorysAuthorsAreTheCommitsOwn(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	x.p.Commit(x.work, map[string]string{"a.txt": "one\n"}, "one")
	x.p.Commit(x.work, map[string]string{".mailmap": "Mapped Name <mapped@example.com> Crewlet Test <test@example.com>\n"},
		"map the author")
	x.push(codetest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(codetest.RepoName)
	// THE COPY LEVEL WITH THE LOG, its refs written: HEAD resolves to the
	// branch the .mailmap is on, as it does on every node once a round has
	// brought the push's ref. A read straight after the push, on the node
	// that took it, finds the pack the push adopted (cache.Adopt) before any
	// round writes the ref, so its HEAD names no commit, no mailmap is read
	// and the case would hold nothing.
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	for _, c := range mustRead(t, s, logQ(repo, code.LogQuery{Rev: head})).Log.Commits {
		if c.Author != "Crewlet Test" || c.AuthorEmail != "test@example.com" {
			t.Errorf("the log names %s <%s>, want the commit's own author", c.Author, c.AuthorEmail)
		}
	}
	for _, c := range mustRead(t, s, blameQ(repo, code.BlameQuery{Rev: head, Path: "a.txt"})).Blame.Commits {
		if c.Author != "Crewlet Test" || c.AuthorEmail != "test@example.com" {
			t.Errorf("the blame names %s <%s>, want the commit's own author", c.Author, c.AuthorEmail)
		}
	}
}

// A LOG USES THE CHANGED-PATH FILTERS: the very walk a read runs — its
// argument list, the path a literal pathspec after `--` — run by the test's
// own git over the copy once its maintenance has written a commit-graph, with
// a trace file of its own (the pool's trace is its fd 3, which decodes no data
// event), reports bloom statistics naming commits the filters ruled out.
//
// Mutation: the path given with glob magic in place of the literal
// (`:(glob)`), which git reads the same path through and serves from no
// filter → none ruled out, red.
func TestALogUsesTheChangedPathFilters(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	x.p.Commit(x.work, map[string]string{"target.txt": "1"}, "target")
	for i := range 24 {
		x.p.Commit(x.work, map[string]string{fmt.Sprintf("other/%02d", i): "x"}, fmt.Sprintf("other %d", i))
	}
	x.push(codetest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(codetest.RepoName)
	dir := x.graphed(0, repo)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	if got := commits([]code.GitAnswer{mustRead(t, s, logQ(repo, code.LogQuery{Rev: head, Path: "target.txt"}))}); len(got) != 1 {
		t.Fatalf("the history of target.txt lists %v, want its one commit", got)
	}
	var walk []string
	for _, c := range rec.ran() {
		if c.Args[0] == "log" {
			walk = c.Args
		}
	}
	trace := filepath.Join(t.TempDir(), "trace.json")
	x.p.Git().Env("GIT_TRACE2_EVENT="+trace).Run(t, dir, walk...)
	f, err := os.Open(trace)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ruledOut := -1
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		var ev struct {
			Event, Category, Key string
			Value                struct {
				DefinitelyNot int `json:"definitely_not"`
			}
		}
		if json.Unmarshal(lines.Bytes(), &ev) == nil && ev.Category == "bloom" && ev.Key == "statistics" {
			ruledOut = ev.Value.DefinitelyNot
		}
	}
	if ruledOut <= 0 {
		t.Errorf("the walk %q reported %d commits the filters ruled out, want some", walk, ruledOut)
	}
}

// graphed is the i-th git node's copy of repo, brought to the log's head once
// its maintenance has written the copy's commit-graph — its own, or the tip
// over an installed chain — answering its directory.
func (x *fixture) graphed(i int, repo string) string {
	x.t.Helper()
	c := x.f.Nodes()[i].Cache()
	deadline := time.Now().Add(time.Minute)
	for {
		s, err := c.Sync(x.t.Context(), repo, codetest.Linearizable)
		if err != nil {
			x.t.Fatal(err)
		}
		dir := s.Dir
		_, whole := os.Stat(filepath.Join(dir, "objects", "info", "commit-graph"))
		_, chain := os.Stat(filepath.Join(dir, "objects", "info", "commit-graphs", "commit-graph-chain"))
		s.Release()
		if whole == nil || chain == nil {
			return dir
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("node %d's copy of %s holds no commit-graph a minute on", i, repo)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A LOG THE READ'S BUDGET ENDS ANSWERS WHAT IT FOUND: a walk the pool ends
// past its budget (a pool answering every log so) is a page of the commits
// read, with a Next past them and never complete, kept out of the memo — the
// same page read again runs the walk again; and a walk its budget ended before
// git wrote one commit's record is too large, naming the narrowing.
//
// Mutations: the cut page memoized → the second read answered from the memo,
// red; a cut page answered complete → red; a walk that read no commit
// answered as an empty page (the cut taken at no commit scanned) → red.
func TestALogTheBudgetEndsAnswersWhatItFound(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	for i := range 5 {
		x.p.Commit(x.work, map[string]string{"f": strconv.Itoa(i)}, fmt.Sprintf("commit %d", i))
	}
	x.push(codetest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(codetest.RepoName)
	ended := &budgetEnds{Pool: x.f.Nodes()[0].Pool()}
	s := x.server(0, query.Options{Git: ended})
	a := mustRead(t, s, logQ(repo, code.LogQuery{Rev: head}))
	if len(a.Log.Commits) != 5 || a.Log.Complete || a.Next != "s:5" || a.Memo {
		t.Fatalf("a walk its budget ended answered %d commits, complete %t, next %q, memo %t; want the 5 read, "+
			"next s:5", len(a.Log.Commits), a.Log.Complete, a.Next, a.Memo)
	}
	before := ended.walks.Load()
	if again := mustRead(t, s, logQ(repo, code.LogQuery{Rev: head})); again.Memo || ended.walks.Load() == before {
		t.Error("a page its budget ended was answered from the memo")
	}
	ended.early.Store(true)
	if _, err := s.Read(t.Context(), logQ(repo, code.LogQuery{Rev: head, Path: "f"})); !errors.Is(err,
		code.ErrSliceTooLarge) || !strings.Contains(err.Error(), "narrow it") {
		t.Errorf("a walk that reached no commit in its budget: %v, want too large, naming the narrowing", err)
	}
}

// budgetEnds is a query.Pool on which every log runs whole and then answers
// that its budget ended it, as a pool answers a run killed at its deadline
// after its output was read — or, once early is set, answers so before git
// wrote anything, as it answers a run killed before its first record.
type budgetEnds struct {
	query.Pool
	walks atomic.Int64
	early atomic.Bool
}

func (b *budgetEnds) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if c.Args[0] == "log" && b.early.Load() {
		return gitproc.Result{}, fmt.Errorf("%w: the read's budget, before git wrote a commit", gitproc.ErrDeadline)
	}
	res, err := b.Pool.Run(ctx, c)
	if err == nil && c.Args[0] == "log" {
		b.walks.Add(1)
		return res, fmt.Errorf("%w: the read's budget, as the test's pool tells it", gitproc.ErrDeadline)
	}
	return res, err
}
