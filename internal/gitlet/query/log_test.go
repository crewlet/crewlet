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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// logQ and blameQ are a person's reads of repo's history and of a file's
// blame, asked as the other reads here are.
func logQ(repo string, l gitlet.LogQuery) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitLog, RepoID: repo, Asker: person, Home: true, Log: &l}
}

func blameQ(repo string, b gitlet.BlameQuery) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitBlame, RepoID: repo, Asker: person, Home: true, Blame: &b}
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
func commits(pages []gitlet.GitAnswer) []string {
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
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	root := x.at(1).Commit(t, x.work, map[string]string{"a.txt": "one\n", "dir/x": "x\n"}, "add a and dir\n\nthe body")
	x.at(2).Commit(t, x.work, map[string]string{"a.txt": "one\ntwo\n"}, "grow a")
	x.at(3).Commit(t, x.work, map[string]string{"dir/y": "y\n"}, "add dir/y")
	x.git("mv", "a.txt", "b.txt")
	x.at(4).Commit(t, x.work, map[string]string{"b.txt": "one\ntwo\nthree\n"}, "rename a to b")
	x.at(5).Commit(t, x.work, map[string]string{"b.txt": "one\ntwo\nthree\nfour\n"}, "grow b")
	for i := range 7 {
		x.at(6+i).Commit(t, x.work, map[string]string{"other.txt": strconv.Itoa(i)}, fmt.Sprintf("other %d", i))
	}
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	since := historyDay.Add(4 * time.Hour)
	for _, tc := range []struct {
		name string
		q    gitlet.LogQuery
		git  []string
	}{
		{"a renamed file", gitlet.LogQuery{Rev: head, Path: "b.txt"}, []string{head, "--", "b.txt"}},
		{"its old name", gitlet.LogQuery{Rev: head, Path: "a.txt"}, []string{head, "--", "a.txt"}},
		{"a directory", gitlet.LogQuery{Rev: head, Path: "dir"}, []string{head, "--", "dir"}},
		{"the whole history", gitlet.LogQuery{Rev: head}, []string{head}},
		{"since", gitlet.LogQuery{Rev: head[:9], Since: since},
			[]string{"--max-age=" + strconv.FormatInt(since.Unix(), 10), head}},
		// A commit is dated in whole seconds, so one dated in the second a
		// Since falls inside of was committed before it.
		{"since inside a second", gitlet.LogQuery{Rev: head, Since: since.Add(time.Nanosecond)},
			[]string{"--max-age=" + strconv.FormatInt(since.Unix()+1, 10), head}},
		{"paged by twos", gitlet.LogQuery{Rev: head, Limit: 2}, []string{head}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := strings.Fields(x.git(append([]string{"log", "--topo-order", "--format=%H"}, tc.git...)...))
			got := pages(t, s, logQ(repo, tc.q), gitlet.MaxGitAnswerBytes)
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
	whole := pages(t, s, logQ(repo, gitlet.LogQuery{Rev: head}), gitlet.MaxGitAnswerBytes)
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
		q := logQ(repo, gitlet.LogQuery{Rev: head})
		q.Cursor = cursor
		if _, err := s.Read(t.Context(), q); !errors.Is(err, gitlet.ErrBadCursor) {
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
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
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
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	for grep, want := range map[string][]string{
		"a.b":    {"fix the a.b parser"},
		"abc":    {"The ABC of it"},
		"needle": {"long"},
		"parser": {"fix the axb parser", "fix the a.b parser"},
		"nobody": nil,
	} {
		a := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: head, Grep: grep}))
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
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	for i := range 6 {
		x.p.Commit(x.work, map[string]string{"f": strconv.Itoa(i)}, fmt.Sprintf("chore %d", i))
	}
	for i := range 2 {
		x.p.Commit(x.work, map[string]string{"g": strconv.Itoa(i)}, fmt.Sprintf("fix %d", i))
	}
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	ended := &budgetEnds{Pool: x.f.Nodes()[0].Pool()}
	s := x.server(0, query.Options{Git: ended})
	q := logQ(repo, gitlet.LogQuery{Rev: head, Grep: "fix", Limit: 2})
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
// the parents, an author, a committer spelling the engine's convention, the
// dates, a subject, each between the separator a record framed by \x01 would
// have used — is listed as the one commit it is, its subject its own, and the
// pages hold exactly git rev-list's ids for the path.
//
// Mutation: the walk's records split on \x01 → the forged commit listed, red.
func TestALogCannotBeForgedByAMessage(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	forged := strings.Repeat("f", 40)
	record := "\x01" + forged + "\x01\x01Mallory\x01m@example.com\x01Crewlet\x01engine@forged.example" +
		"\x011700000000\x011700000000\x01forged\x01"
	x.p.Commit(x.work, map[string]string{"f": "1"}, "honest")
	x.p.Commit(x.work, map[string]string{"f": "2"}, "evil"+record+"\n\nbody"+record)
	x.p.Commit(x.work, map[string]string{"f": "3"}, "after")
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	want := strings.Fields(x.git("rev-list", "--topo-order", head, "--", "f"))
	for _, q := range []gitlet.LogQuery{{Rev: head, Path: "f"}, {Rev: head, Path: "f", Grep: "evil"}} {
		got := pages(t, s, logQ(repo, q), gitlet.MaxGitAnswerBytes)
		ids := commits(got)
		if slices.Contains(ids, forged) {
			t.Fatalf("the log lists the forged commit: %v", ids)
		}
		if q.Grep == "" && !slices.Equal(ids, want) {
			t.Errorf("the pages list %v, want rev-list's %v", ids, want)
		}
		for _, p := range got {
			for _, c := range p.Log.Commits {
				if c.Subject == "forged" || c.Author == "Mallory" || c.CommitterEmail == "engine@forged.example" ||
					c.EngineCommitted == nil || *c.EngineCommitted {
					t.Errorf("a record carries the forged fields: %+v", c)
				}
			}
		}
	}
}

// A LOG'S AUTHORS AND COMMITTERS ARE THE COMMITS' OWN, and so are a blame's: a
// `.mailmap` at the default branch mapping the person to another name renames
// nobody in a history read from any commit, nor in a blame — git blame would
// read it, in a bare repository, without the configuration the read gives it
// (measured on 2.56.0: "Mapped Name" without mailmap.blob emptied, the
// commit's own with it).
//
// Mutation: the blame without mailmap.blob emptied → the mapped name, red.
func TestAHistorysAuthorsAreTheCommitsOwn(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	x.p.Commit(x.work, map[string]string{"a.txt": "one\n"}, "one")
	x.p.Commit(x.work, map[string]string{".mailmap": "Mapped Name <mapped@example.com> Crewlet Test <test@example.com>\n"},
		"map the author")
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	// THE COPY LEVEL WITH THE LOG, its refs written: HEAD resolves to the
	// branch the .mailmap is on, as it does on every node once a round has
	// brought the push's ref. A read straight after the push, on the node
	// that took it, finds the pack the push adopted (cache.Adopt) before any
	// round writes the ref, so its HEAD names no commit, no mailmap is read
	// and the case would hold nothing.
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	for _, c := range mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: head})).Log.Commits {
		if c.Author != "Crewlet Test" || c.AuthorEmail != "test@example.com" {
			t.Errorf("the log names %s <%s>, want the commit's own author", c.Author, c.AuthorEmail)
		}
		if c.Committer != "Crewlet Test" || c.CommitterEmail != "test@example.com" {
			t.Errorf("the log names %s <%s>, want the commit's own committer", c.Committer, c.CommitterEmail)
		}
	}
	for _, c := range mustRead(t, s, blameQ(repo, gitlet.BlameQuery{Rev: head, Path: "a.txt"})).Blame.Commits {
		if c.Author != "Crewlet Test" || c.AuthorEmail != "test@example.com" {
			t.Errorf("the blame names %s <%s>, want the commit's own author", c.Author, c.AuthorEmail)
		}
		if c.Committer != "Crewlet Test" || c.CommitterEmail != "test@example.com" {
			t.Errorf("the blame names %s <%s>, want the commit's own committer", c.Committer, c.CommitterEmail)
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
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	x.p.Commit(x.work, map[string]string{"target.txt": "1"}, "target")
	for i := range 24 {
		x.p.Commit(x.work, map[string]string{fmt.Sprintf("other/%02d", i): "x"}, fmt.Sprintf("other %d", i))
	}
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	dir := x.graphed(0, repo)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	if got := commits([]gitlet.GitAnswer{mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: head, Path: "target.txt"}))}); len(got) != 1 {
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

// graphed is the i-th Gitlet node's copy of repo, brought to the log's head once
// its maintenance has written the copy's commit-graph — its own, or the tip
// over an installed chain — answering its directory.
func (x *fixture) graphed(i int, repo string) string {
	x.t.Helper()
	c := x.f.Nodes()[i].Cache()
	deadline := time.Now().Add(time.Minute)
	for {
		s, err := c.Sync(x.t.Context(), repo, gitlettest.Linearizable)
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			x.t.Fatalf("node %d's copy of %s holds no commit-graph a minute on", i, repo)
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
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
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	for i := range 5 {
		x.p.Commit(x.work, map[string]string{"f": strconv.Itoa(i)}, fmt.Sprintf("commit %d", i))
	}
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	ended := &budgetEnds{Pool: x.f.Nodes()[0].Pool()}
	s := x.server(0, query.Options{Git: ended})
	a := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: head}))
	if len(a.Log.Commits) != 5 || a.Log.Complete || a.Next != "s:5" || a.Memo {
		t.Fatalf("a walk its budget ended answered %d commits, complete %t, next %q, memo %t; want the 5 read, "+
			"next s:5", len(a.Log.Commits), a.Log.Complete, a.Next, a.Memo)
	}
	before := ended.walks.Load()
	if again := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: head})); again.Memo || ended.walks.Load() == before {
		t.Error("a page its budget ended was answered from the memo")
	}
	ended.early.Store(true)
	if _, err := s.Read(t.Context(), logQ(repo, gitlet.LogQuery{Rev: head, Path: "f"})); !errors.Is(err,
		gitlet.ErrSliceTooLarge) || !strings.Contains(err.Error(), "narrow it") {
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

// dated is the person's git dating its commits at, to the second.
func (x *fixture) dated(at time.Time) *gittest.Client {
	d := at.UTC().Format(time.RFC3339)
	return x.p.Git().Env("GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
}

// A LOG'S UNTIL KEEPS THE COMMITS BEFORE IT, a filter and never a stop: over
// skewed committer dates the pages equal git's `rev-list --min-age=<second
// before>`, exclusive at the bound, paged two at a time.
//
// Mutations: Until as --max-age → the skewed commit lost, red; the bound
// inclusive → the commit at the bound listed, red; the skip over records git
// did not output → a page repeats or loses one, red.
func TestALogUntilKeepsTheCommitsBeforeIt(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	day := func(d int) time.Time { return historyDay.AddDate(0, 0, d-1) }
	until := day(3)
	c1 := x.dated(day(1).Add(time.Hour)).Commit(t, x.work, map[string]string{"f": "1"}, "c1")
	c2 := x.dated(day(2)).Commit(t, x.work, map[string]string{"f": "2"}, "c2")
	c3 := x.dated(until.Add(-time.Second)).Commit(t, x.work, map[string]string{"f": "3"}, "a second before")
	c4 := x.dated(until).Commit(t, x.work, map[string]string{"f": "4"}, "at the bound")
	x.dated(day(5)).Commit(t, x.work, map[string]string{"f": "5"}, "c5")
	c6 := x.dated(day(1).Add(12*time.Hour)).Commit(t, x.work, map[string]string{"f": "6"}, "skewed: before its parent")
	x.dated(day(9)).Commit(t, x.work, map[string]string{"f": "7"}, "c7")
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	minAge := func(at time.Time) string { return "--min-age=" + strconv.FormatInt(at.Unix(), 10) }
	for _, tc := range []struct {
		name string
		q    gitlet.LogQuery
		git  []string
		want []string
	}{
		{"until", gitlet.LogQuery{Rev: head, Until: until}, []string{minAge(until.Add(-time.Second)), head},
			[]string{c6, c3, c2, c1}},
		{"paged by twos", gitlet.LogQuery{Rev: head, Until: until, Limit: 2},
			[]string{minAge(until.Add(-time.Second)), head}, []string{c6, c3, c2, c1}},
		{"until inside a second", gitlet.LogQuery{Rev: head, Until: until.Add(500 * time.Millisecond)},
			[]string{minAge(until), head}, []string{c6, c4, c3, c2, c1}},
		{"a window", gitlet.LogQuery{Rev: head, Since: day(2), Until: until},
			[]string{"--max-age=" + strconv.FormatInt(day(2).Unix(), 10), minAge(until.Add(-time.Second)), head},
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := strings.Fields(x.git(append([]string{"rev-list", "--topo-order"}, tc.git...)...))
			if tc.want != nil && !slices.Equal(want, tc.want) {
				t.Fatalf("git's own rev-list keeps %v, want %v: the history is not the one this case built", want,
					tc.want)
			}
			got := pages(t, s, logQ(repo, tc.q), gitlet.MaxGitAnswerBytes)
			if !slices.Equal(commits(got), want) {
				t.Fatalf("the pages list %v, want git's own %v", commits(got), want)
			}
			for i, p := range got {
				if last := i == len(got)-1; p.Log.Complete != last {
					t.Errorf("page %d of %d is complete %t", i, len(got), p.Log.Complete)
				}
				if p.Log.Unjudged != nil {
					t.Errorf("page %d, asked with no pushed_by, counts %d unjudged", i, *p.Log.Unjudged)
				}
			}
			if tc.q.Limit == 2 && len(got) != (len(want)+1)/2 {
				t.Errorf("a history of %d read two a page took %d pages", len(want), len(got))
			}
		})
	}
}

// A LOG NARROWED BY WHO PUSHED IT COUNTS WHAT IT CANNOT JUDGE: each seat's
// covered commits listed in walk order, the commits pushed before the index
// began counted in Unjudged, Coverage on the page, and nothing memoized.
//
// Mutations: the seat judged by Principal.Actor → nothing kept, red; an
// unjudged commit kept or dropped uncounted → red; a narrowed page memoized →
// the second read runs no git, red.
func TestALogNarrowedByWhoPushedCountsWhatItCannotJudge(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	dev := gitlettest.NewPerson(t, x.f, "dev")
	pushAs := func(p *gitlettest.Person) {
		t.Helper()
		if res := p.PushArgs(t.Context(), x.work, []string{"--force"}, x.url(0, gitlettest.RepoName),
			"main"); res.Err != nil {
			t.Fatalf("git push: %s", res)
		}
	}
	early1 := x.commit(map[string]string{"f": "e1"}, "before the index")
	early2 := x.commit(map[string]string{"f": "e2"}, "also before")
	g, dir := x.f.GitletNode(0), x.f.Nodes()[0].CacheDir()
	g.Stop(t.Context())
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	g.Restart(t)
	c1 := x.commit(map[string]string{"f": "c1"}, "ceo's first")
	d1 := x.p.Commit(x.work, map[string]string{"f": "d1"}, "dev's first")
	pushAs(dev)
	c2 := x.commit(map[string]string{"f": "c2"}, "ceo's second")
	d2 := x.p.Commit(x.work, map[string]string{"f": "d2"}, "dev's second")
	d3 := x.p.Commit(x.work, map[string]string{"f": "d3"}, "dev's third, in the same push")
	pushAs(dev)
	c3 := x.commit(map[string]string{"f": "c3"}, "ceo's third")
	repo := x.repo(gitlettest.RepoName)
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	if walk := strings.Fields(x.git("rev-list", "--topo-order", head)); !slices.Equal(walk,
		[]string{c3, d3, d2, c2, d1, c1, early2, early1}) {
		t.Fatalf("the walk is %v: the history is not the one this case built", walk)
	}
	x.synced(0, repo)
	x.indexed(0, repo)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	narrowed := func(seat string, limit int) gitlet.GitQuery {
		q := logQ(repo, gitlet.LogQuery{Rev: head, PushedBy: seat, Limit: limit})
		q.Home = false
		return q
	}
	for _, tc := range []struct {
		seat  string
		limit int
		want  []string
	}{
		{"ceo", 0, []string{c3, c2, c1}},
		{"dev", 0, []string{d3, d2, d1}},
		{"ceo", 2, []string{c3, c2, c1}},
		{"pat", 0, nil},
	} {
		got := pages(t, s, narrowed(tc.seat, tc.limit), gitlet.MaxGitAnswerBytes)
		unjudged := 0
		for i, p := range got {
			switch {
			case p.Log.Unjudged == nil:
				t.Fatalf("%s, %d a page: page %d carries no unjudged count", tc.seat, tc.limit, i)
			case p.Coverage == nil:
				t.Errorf("%s, %d a page: page %d carries no coverage", tc.seat, tc.limit, i)
			case p.Memo:
				t.Errorf("%s, %d a page: page %d was answered from the memo", tc.seat, tc.limit, i)
			}
			unjudged += *p.Log.Unjudged
		}
		if !slices.Equal(commits(got), tc.want) || unjudged != 2 {
			t.Errorf("narrowed to %s, %d a page, the pages list %v and count %d unjudged; want %v and the 2 "+
				"pushed before the index", tc.seat, tc.limit, commits(got), unjudged, tc.want)
		}
		if tc.limit == 2 && (len(got) != 2 || *got[0].Log.Unjudged != 0 || *got[1].Log.Unjudged != 2) {
			t.Errorf("narrowed two a page, %d pages count %v unjudged; want 2 pages, the second counting both",
				len(got), unjudgedOf(got))
		}
	}
	for _, id := range []string{early1, early2} {
		for _, seat := range []string{"ceo", "dev"} {
			if slices.Contains(commits(pages(t, s, narrowed(seat, 0), gitlet.MaxGitAnswerBytes)), id) {
				t.Errorf("commit %s, pushed before the index began, is listed as %s's", id, seat)
			}
		}
	}
	plain := logQ(repo, gitlet.LogQuery{Rev: head})
	plain.Home = false
	if a := mustRead(t, s, plain); a.Log.Unjudged != nil {
		t.Errorf("a history asked with no pushed_by counts %d unjudged", *a.Log.Unjudged)
	}
	before := len(walks(rec))
	mustRead(t, s, narrowed("ceo", 0))
	if again := walks(rec); len(again) != before+1 {
		t.Errorf("the same narrowed read again ran %d walks of git, want 1: it was memoized", len(again)-before)
	}
}

// unjudgedOf is each page's unjudged count, -1 for none.
func unjudgedOf(pages []gitlet.GitAnswer) []int {
	out := make([]int, len(pages))
	for i, p := range pages {
		out[i] = -1
		if p.Log.Unjudged != nil {
			out[i] = *p.Log.Unjudged
		}
	}
	return out
}

// walks is every `git log` a recording ran.
func walks(rec *recording) []gitproc.Cmd {
	var out []gitproc.Cmd
	for _, c := range rec.ran() {
		if c.Args[0] == "log" {
			out = append(out, c)
		}
	}
	return out
}

// A NARROWED PAGE THE BUDGET ENDS GOES ON FROM ITS CURSOR: cut after three
// records it answers its one kept commit with Next s:3 and Unjudged set,
// unmemoized, and the pages after it complete the unbroken narrowed history.
//
// Mutations: the cut page memoized → no second git run, red; its cursor
// counting kept commits → Next s:1, red; its Unjudged left out → red.
func TestANarrowedPageTheBudgetEndsGoesOnFromItsCursor(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	dev := gitlettest.NewPerson(t, x.f, "dev")
	var ceo []string
	for i := range 4 {
		ceo = append(ceo, x.commit(map[string]string{"f": "ceo " + strconv.Itoa(i)}, fmt.Sprintf("ceo %d", i)))
		x.p.Commit(x.work, map[string]string{"f": "dev " + strconv.Itoa(i)}, fmt.Sprintf("dev %d", i))
		if res := dev.PushArgs(t.Context(), x.work, []string{"--force"}, x.url(0, gitlettest.RepoName),
			"main"); res.Err != nil {
			t.Fatalf("git push: %s", res)
		}
	}
	slices.Reverse(ceo)
	repo := x.repo(gitlettest.RepoName)
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	x.synced(0, repo)
	x.indexed(0, repo)
	q := logQ(repo, gitlet.LogQuery{Rev: head, PushedBy: "ceo"})
	whole := commits(pages(t, x.server(0, query.Options{}), q, gitlet.MaxGitAnswerBytes))
	if !slices.Equal(whole, ceo) {
		t.Fatalf("the unbroken narrowed history lists %v, want the ceo's %v", whole, ceo)
	}
	cut := &budgetEndsAfter{Pool: x.f.Nodes()[0].Pool(), records: 3}
	s := x.server(0, query.Options{Git: cut})
	a := mustRead(t, s, q)
	switch {
	case !slices.Equal(commits([]gitlet.GitAnswer{a}), ceo[:1]) || a.Log.Scanned != 3 || a.Next != "s:3" ||
		a.Log.Complete:
		t.Fatalf("a narrowed walk its budget ended after 3 records lists %v of %d scanned, next %q, complete %t; "+
			"want the ceo's newest, 3 scanned, next s:3", commits([]gitlet.GitAnswer{a}), a.Log.Scanned, a.Next,
			a.Log.Complete)
	case a.Log.Unjudged == nil || *a.Log.Unjudged != 0:
		t.Errorf("the cut page's unjudged count is %v, want 0, set", unjudgedOf([]gitlet.GitAnswer{a}))
	case a.Memo:
		t.Error("the cut page was answered from the memo")
	}
	before := cut.walks.Load()
	if again := mustRead(t, s, q); again.Memo || cut.walks.Load() == before {
		t.Error("a cut narrowed page was answered from the memo")
	}
	q.Cursor = a.Next
	rest := pages(t, x.server(0, query.Options{}), q, gitlet.MaxGitAnswerBytes)
	if got := append(commits([]gitlet.GitAnswer{a}), commits(rest)...); !slices.Equal(got, whole) {
		t.Errorf("the cut page and the pages after it list %v, want the unbroken %v", got, whole)
	}
}

// budgetEndsAfter is a query.Pool whose every log writes its first records
// (git's --max-count, counted after --skip) and then answers that its budget
// ended it.
type budgetEndsAfter struct {
	query.Pool
	records int
	walks   atomic.Int64
}

func (b *budgetEndsAfter) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if c.Args[0] != "log" {
		return b.Pool.Run(ctx, c)
	}
	c.Args = slices.Insert(slices.Clone(c.Args), 1, "--max-count="+strconv.Itoa(b.records))
	res, err := b.Pool.Run(ctx, c)
	if err == nil {
		b.walks.Add(1)
		return res, fmt.Errorf("%w: the read's budget, as the test's pool tells it", gitproc.ErrDeadline)
	}
	return res, err
}

// A COMMIT'S COMMITTER IS NAMED, AND THE ENGINE'S CONVENTION MARKED, in a
// history and a blame: true exactly for `Crewlet` at `engine@` on any domain,
// false (never nil) for every near miss and an address past its bound.
//
// Mutations: the mark case-folded, tied to one domain, judged on a cut
// address, or left nil → red; the committer read from the author → red.
func TestACommitsCommitterIsNamedAndTheEnginesConventionMarked(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	type committer struct {
		name, email string
		marked      bool
	}
	committers := []committer{
		{"Crewlet", "engine@seats.example", true},
		{"Crewlet", "engine@other.example", true},
		{"crewlet", "engine@x.example", false},
		{"Crewlet", "Engine@x.example", false},
		{"Crewlet Bot", "engine@x.example", false},
		{"Crewlet", "engine@", false},
		{"Crewlet", "engine@" + strings.Repeat("e", gitlet.MaxLogPersonBytes) + ".example", false},
		{"Crewlet Test", "test@example.com", false},
	}
	byCommit := map[string]committer{}
	var text strings.Builder
	for i, c := range committers {
		fmt.Fprintf(&text, "line %d\n", i)
		id := x.p.Git().Env("GIT_COMMITTER_NAME="+c.name, "GIT_COMMITTER_EMAIL="+c.email).Commit(t, x.work,
			map[string]string{"f.txt": text.String()}, fmt.Sprintf("committed by %d", i))
		byCommit[id] = c
	}
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	check := func(how, id, name, email, author string, engine *bool) {
		t.Helper()
		want, ok := byCommit[id]
		wantEmail := want.email
		if len(wantEmail) > gitlet.MaxLogPersonBytes {
			wantEmail = "" // described by its size alone
		}
		switch {
		case !ok:
			t.Errorf("the %s names commit %s, which this case did not make", how, id)
		case name != want.name || email != wantEmail:
			t.Errorf("the %s names commit %s's committer %q <%s>, want its own %q <%.40s>", how, id, name, email,
				want.name, wantEmail)
		case author != "Crewlet Test":
			t.Errorf("the %s names commit %s's author %q, want the person", how, id, author)
		case engine == nil:
			t.Errorf("the %s leaves commit %s (%s <%.40s>) unmarked", how, id, want.name, want.email)
		case *engine != want.marked:
			t.Errorf("the %s marks commit %s (%s <%.40s>) engine-committed %t, want %t", how, id, want.name,
				want.email, *engine, want.marked)
		}
	}
	listed := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: head})).Log.Commits
	if len(listed) != len(committers) {
		t.Fatalf("the history lists %d commits, want %d", len(listed), len(committers))
	}
	for _, c := range listed {
		check("history", c.OID, c.Committer, c.CommitterEmail, c.Author, c.EngineCommitted)
	}
	blamed := mustRead(t, s, blameQ(repo, gitlet.BlameQuery{Rev: head, Path: "f.txt"})).Blame.Commits
	if len(blamed) != len(committers) {
		t.Fatalf("the blame names %d commits, want %d", len(blamed), len(committers))
	}
	for id, c := range blamed {
		check("blame", id, c.Committer, c.CommitterEmail, c.Author, c.EngineCommitted)
	}
}
