package query_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// tipsQ is a person's tips read of repo, asked as the other reads here are.
func tipsQ(repo, base string, oids ...string) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitTips, RepoID: repo, Asker: person, Home: true,
		Tips: &gitlet.TipsQuery{OIDs: oids, Base: base}}
}

// rev is what spec names in the person's work tree, whole.
func (x *fixture) rev(spec string) string {
	x.t.Helper()
	return strings.TrimSpace(x.git("rev-parse", spec))
}

// sides is git's own count of base...tip in the work tree: behind, ahead.
func (x *fixture) sides(base, tip string) (int, int) {
	x.t.Helper()
	l, r, _ := strings.Cut(strings.TrimSpace(x.git("rev-list", "--left-right", "--count", base+"..."+tip)), "\t")
	behind, err1 := strconv.Atoi(l)
	ahead, err2 := strconv.Atoi(r)
	if err1 != nil || err2 != nil {
		x.t.Fatalf("rev-list --count %s...%s: %q %q", base, tip, l, r)
	}
	return behind, ahead
}

// counted reports a tip answered with exactly behind and ahead.
func counted(tip gitlet.Tip, behind, ahead int) bool {
	return tip.Behind != nil && tip.Ahead != nil && *tip.Behind == behind && *tip.Ahead == ahead &&
		!tip.CountsOutOfTime
}

// countsOf is a tip's counts as a failure names them.
func countsOf(tip gitlet.Tip) string {
	if tip.Behind == nil || tip.Ahead == nil {
		return fmt.Sprintf("none (out of time %t)", tip.CountsOutOfTime)
	}
	return fmt.Sprintf("%d/%d", *tip.Behind, *tip.Ahead)
}

// onlyTemplates fails the case on a process of rec no read's template admits,
// or one naming a path as an argument.
func onlyTemplates(t *testing.T, rec *recording) {
	t.Helper()
	for _, c := range rec.ran() {
		if !fitsTemplate(c.Args) || slices.Contains(c.Args, "--") {
			t.Errorf("a tips read ran git %q, which no template of a read admits", c.Args)
		}
	}
}

// One read against main peels branches, annotated, lightweight, tree and nested
// tags, answers each tag's own facts and commit, and git's three-dot counts.
// Mutations: unpeel a tag, tag a lightweight tag, swap the counts, count a tree.
func TestATipsReadPeelsEachTipAndCountsItAgainstTheBase(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	m0 := x.commit(map[string]string{"f": "0\n"}, "m0")
	m1 := x.commit(map[string]string{"f": "1\n"}, "m1")
	main := x.commit(map[string]string{"f": "2\n"}, "m2")
	x.branch("fast", main)
	x.p.Commit(x.work, map[string]string{"g": "1\n"}, "f1")
	fast := x.p.Commit(x.work, map[string]string{"g": "2\n"}, "f2")
	x.branch("diverged", m1)
	diverged := x.p.Commit(x.work, map[string]string{"h": "1\n"}, "d1")
	x.git("checkout", "-q", "main")
	x.git("branch", "behind", m0)
	x.git("tag", "-a", "v1", "-m", "Release one\nits second line\n\nThe body.", m1)
	x.git("tag", "lw", m1)
	x.git("tag", "-a", "t-tree", "-m", "a tree", main+"^{tree}")
	x.git("tag", "-a", "v1-outer", "-m", "the outer tag", "v1")
	x.push(gitlettest.RepoName, "main", "fast", "diverged", "behind", "refs/tags/v1", "refs/tags/lw",
		"refs/tags/t-tree", "refs/tags/v1-outer")
	v1, tTree, outer := x.rev("refs/tags/v1"), x.rev("refs/tags/t-tree"), x.rev("refs/tags/v1-outer")
	tree := x.rev(main + "^{tree}")
	repo := x.repo(gitlettest.RepoName)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})

	a := mustRead(t, s, tipsQ(repo, main, main, fast, diverged, m0, v1, m1, tTree, outer))
	if a.Kind != gitlet.GitTips || a.Tips == nil || a.Tips.Base != main || a.Next != "" || len(a.Tips.Tips) != 8 {
		t.Fatalf("the read answered %+v, want all eight tips against %s on one page", a, main)
	}
	tips := a.Tips.Tips
	for i, id := range []string{main, fast, diverged, m0, v1, m1, tTree, outer} {
		if tips[i].OID != id {
			t.Errorf("tip %d is %s, want %s: the query's order", i, tips[i].OID, id)
		}
	}
	for i, tip := range tips {
		if tip.PeeledType != "commit" {
			continue
		}
		behind, ahead := x.sides(main, tip.Peeled)
		if !counted(tip, behind, ahead) {
			t.Errorf("tip %d (%s) counts %s, want git's %d behind and %d ahead", i, tip.OID, countsOf(tip),
				behind, ahead)
		}
		if tip.Commit == nil || tip.Commit.OID != tip.Peeled {
			t.Errorf("tip %d (%s) names commit %+v, want %s", i, tip.OID, tip.Commit, tip.Peeled)
		}
	}
	for i, want := range []struct{ behind, ahead int }{{0, 0}, {0, 2}, {1, 1}, {2, 0}} {
		if !counted(tips[i], want.behind, want.ahead) {
			t.Errorf("branch %d counts %s, want %d/%d", i, countsOf(tips[i]), want.behind, want.ahead)
		}
	}
	for i, tip := range tips[:4] {
		if tip.Type != "commit" || tip.Peeled != tip.OID || tip.Tag != nil {
			t.Errorf("branch %d is %+v, want a commit that is its own peel and no tag", i, tip)
		}
	}

	tagger := x.git("for-each-ref", "--format=%(taggername)%00%(taggeremail:trim)%00%(taggerdate:unix)",
		"refs/tags/v1")
	parts := strings.Split(strings.TrimSpace(tagger), "\x00")
	when, _ := strconv.ParseInt(parts[2], 10, 64)
	if g := tips[4]; g.Type != "tag" || g.Peeled != m1 || g.PeeledType != "commit" || g.Tag == nil ||
		g.Tag.Subject != "Release one its second line" || g.Tag.Tagger != parts[0] || g.Tag.TaggerEmail != parts[1] ||
		g.Tag.TaggedAt == nil || !g.Tag.TaggedAt.Equal(time.Unix(when, 0)) || g.Commit.Subject != "m1" {
		t.Errorf("the annotated tag is %+v (tag %+v), want peeled to %s with its tagger %q, date and subject",
			g, g.Tag, m1, parts)
	}
	if g := tips[5]; g.Type != "commit" || g.Tag != nil || !counted(g, 1, 0) {
		t.Errorf("the lightweight tag is %+v, want the commit it names, no tag", g)
	}
	if g := tips[6]; g.Type != "tag" || g.Peeled != tree || g.PeeledType != "tree" || g.Tag == nil ||
		g.Tag.Subject != "a tree" || g.Commit != nil || g.Behind != nil || g.Ahead != nil || g.CountsOutOfTime {
		t.Errorf("the tag of a tree is %+v, want its tag, its tree and no commit or counts", g)
	}
	if g := tips[7]; g.Type != "tag" || g.Peeled != m1 || g.Tag == nil || g.Tag.Subject != "the outer tag" ||
		g.Commit == nil || g.Commit.OID != m1 {
		t.Errorf("the tag of a tag is %+v (tag %+v), want the outer tag's facts and the inner commit", g, g.Tag)
	}

	revLists := 0
	for _, c := range rec.ran() {
		if c.Args[0] == "rev-list" {
			revLists++
			if strings.Contains(c.Args[len(c.Args)-1], main+"..."+main) {
				t.Error("the base was counted against itself")
			}
		}
	}
	if revLists != 4 {
		t.Errorf("the read ran %d counts, want one per distinct commit other than the base's (4)", revLists)
	}
	onlyTemplates(t, rec)
}

// Each tag's tagger, email, date and subject are what git for-each-ref prints.
// Mutation: read the tagger's name up to its first space.
func TestATagsFactsAreGitsOwn(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	type made struct{ name, by, email, date, message string }
	tags := []made{
		{"a", "Ada Lovelace", "ada@example.com", "1700000000 +0530", "one line"},
		{"b", "Grace Hopper", "grace@example.com", "1600000000 -0800", "two\nlines\n\nand a body"},
		{"c", "Émile Zola", "emile@example.com", "1500000000 +0000", "ünicode and «quotes»"},
		{"d", "x", "x@y", "1000000000 +0000", "  indented first line\nsecond"},
	}
	var oids []string
	for _, m := range tags {
		x.p.Git().Env("GIT_COMMITTER_NAME="+m.by, "GIT_COMMITTER_EMAIL="+m.email, "GIT_COMMITTER_DATE="+m.date).
			Run(t, x.work, "tag", "-a", m.name, "-m", m.message, head)
		x.push(gitlettest.RepoName, "refs/tags/"+m.name)
		oids = append(oids, x.rev("refs/tags/"+m.name))
	}
	repo := x.repo(gitlettest.RepoName)
	a := mustRead(t, x.server(0, query.Options{}), tipsQ(repo, "", oids...))
	for i, m := range tags {
		out := x.git("for-each-ref", "--format=%(taggername)%00%(taggeremail:trim)%00%(taggerdate:unix)%00"+
			"%(contents:subject)", "refs/tags/"+m.name)
		want := strings.Split(strings.TrimSuffix(out, "\n"), "\x00")
		when, _ := strconv.ParseInt(want[2], 10, 64)
		g := a.Tips.Tips[i].Tag
		if g == nil || g.Tagger != want[0] || g.TaggerEmail != want[1] || g.TaggedAt == nil ||
			!g.TaggedAt.Equal(time.Unix(when, 0)) || g.Subject != want[3] {
			t.Errorf("tag %s read as %+v, want git's %q", m.name, g, want)
		}
	}
}

// budgetAt answers its at-th count, or every count when every is set, as run
// out of the read's budget once git has run it.
type budgetAt struct {
	query.Pool
	at    int64
	every bool
	n     atomic.Int64
}

func (b *budgetAt) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	res, err := b.Pool.Run(ctx, c)
	if c.Args[0] == "rev-list" && err == nil && (b.every || b.n.Add(1) == b.at) {
		return res, fmt.Errorf("%w: the read's budget, as the test's pool tells it", gitproc.ErrDeadline)
	}
	return res, err
}

// A count the budget ends ends the page there, its first tip answered uncounted,
// and the pages from each cursor on answer every tip once and always move on.
// Mutations: refuse the cut page; leave the cursor at the first tip; drop it.
func TestATipsPageEndsWhereItsBudgetRanOutAndGoesOn(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	main := x.commit(map[string]string{"f": "0\n"}, "main")
	var oids []string
	for i := range 3 {
		x.branch(fmt.Sprintf("b%d", i), main)
		for j := range i + 1 {
			x.p.Commit(x.work, map[string]string{"g": strconv.Itoa(j)}, fmt.Sprintf("b%d.%d", i, j))
		}
		x.push(gitlettest.RepoName, fmt.Sprintf("b%d", i))
		oids = append(oids, x.rev("HEAD"))
	}
	repo := x.repo(gitlettest.RepoName)
	whole := x.server(0, query.Options{Cache: memoless{x.f.Nodes()[0].Cache()}})
	cut := func(p *budgetAt) *query.Server {
		p.Pool = x.f.Nodes()[0].Pool()
		return x.server(0, query.Options{Cache: memoless{x.f.Nodes()[0].Cache()}, Git: p})
	}
	rest := func(name string, first gitlet.GitAnswer) {
		t.Helper()
		got := slices.Clone(first.Tips.Tips)
		q := tipsQ(repo, main, oids...)
		q.Cursor = first.Next
		for _, p := range pages(t, whole, q, gitlet.MaxGitAnswerBytes) {
			got = append(got, p.Tips.Tips...)
		}
		if len(got) != len(oids) {
			t.Fatalf("%s: the pages answered %d tips, want each of the %d once", name, len(got), len(oids))
		}
		for i, tip := range got {
			if tip.OID != oids[i] {
				t.Errorf("%s: tip %d of the pages is %s, want %s", name, i, tip.OID, oids[i])
			}
			if i > 0 && !counted(tip, 0, i+1) {
				t.Errorf("%s: tip %d counts %s, want 0/%d", name, i, countsOf(tip), i+1)
			}
		}
	}

	a := mustRead(t, cut(&budgetAt{at: 2}), tipsQ(repo, main, oids...))
	if len(a.Tips.Tips) != 1 || !counted(a.Tips.Tips[0], 0, 1) || a.Next != "t:1" {
		t.Fatalf("a budget ended on the second count answered %+v, next %q; want the first tip counted, next t:1",
			a.Tips.Tips, a.Next)
	}
	rest("the second count's end", a)

	a = mustRead(t, cut(&budgetAt{at: 1}), tipsQ(repo, main, oids...))
	if len(a.Tips.Tips) != 1 || !a.Tips.Tips[0].CountsOutOfTime || a.Tips.Tips[0].Behind != nil ||
		a.Tips.Tips[0].Ahead != nil || a.Tips.Tips[0].Commit == nil || a.Next != "t:1" {
		t.Fatalf("a budget ended on the first count answered %+v, next %q; want the first tip out of time, "+
			"uncounted, next t:1", a.Tips.Tips, a.Next)
	}
	rest("the first count's end", a)

	never := cut(&budgetAt{every: true})
	q := tipsQ(repo, main, oids...)
	for i := range oids {
		a := mustRead(t, never, q)
		if len(a.Tips.Tips) != 1 || a.Tips.Tips[0].OID != oids[i] || !a.Tips.Tips[0].CountsOutOfTime {
			t.Fatalf("page %d where every count runs out answered %+v, want tip %s out of time", i, a.Tips.Tips,
				oids[i])
		}
		if want := map[bool]string{true: "", false: fmt.Sprintf("t:%d", i+1)}[i == len(oids)-1]; a.Next != want {
			t.Fatalf("page %d where every count runs out goes on at %q, want %q", i, a.Next, want)
		}
		q.Cursor = a.Next
	}
}

// A hundred tags and commits at their bounds page within MaxGitAnswerBytes, stamped
// or not, all in order, and one byte short of the whole answer the page cuts.
// Mutations: leave the comma or pushCost out of a tip's cost.
func TestATipsPageIsCutByItsBytes(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	wide := func(c string, bound int) string { return strings.Repeat(c, bound/6) }
	who := wide("&", gitlet.MaxLogPersonBytes)
	email := wide("&", gitlet.MaxLogPersonBytes-12) + "@example.com"
	subject := wide("<", gitlet.MaxLogSubjectBytes)
	ident := fmt.Sprintf("%s <%s> 1700000000 +0000", who, email)
	gittest.FastImport(t, x.p.Git(), x.work, func(w *bufio.Writer) error {
		for i := range gitlet.MaxLogParents {
			msg := fmt.Sprintf("root %d", i)
			fmt.Fprintf(w, "commit refs/heads/root-%d\nmark :%d\ncommitter %s\ndata %d\n%s\n", i, i+1, ident,
				len(msg), msg)
		}
		fmt.Fprintf(w, "commit refs/heads/main\nmark :1000\nauthor %s\ncommitter %s\ndata %d\n%s\nfrom :1\n", ident,
			ident, len(subject), subject)
		for i := 2; i <= gitlet.MaxLogParents; i++ {
			fmt.Fprintf(w, "merge :%d\n", i)
		}
		for i := range gitlet.MaxTips {
			fmt.Fprintf(w, "tag t%03d\nfrom :1000\ntagger %s\ndata %d\n%s\n", i, ident, len(subject), subject)
		}
		return nil
	})
	x.push(gitlettest.RepoName, "main", "refs/tags/*:refs/tags/*")
	oids := strings.Fields(x.git("for-each-ref", "--format=%(objectname)", "refs/tags"))
	if len(oids) != gitlet.MaxTips {
		t.Fatalf("the premise: %d tags, want %d", len(oids), gitlet.MaxTips)
	}
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	s := x.server(0, query.Options{})

	all := func(name string, q gitlet.GitQuery, limit int) int {
		t.Helper()
		got := pages(t, s, q, limit)
		var seen []string
		for _, p := range got {
			for _, tip := range p.Tips.Tips {
				seen = append(seen, tip.OID)
				if tip.Tag == nil || tip.Tag.Subject == "" || tip.Tag.Tagger == "" || tip.Commit == nil ||
					tip.Commit.Subject == "" || len(tip.Commit.Parents) != gitlet.MaxLogParents {
					t.Fatalf("%s: tip %s was answered %+v, want every field at its bound held", name, tip.OID, tip)
				}
			}
		}
		if !slices.Equal(seen, oids) {
			t.Fatalf("%s: the pages answered %d tips, want all %d in order", name, len(seen), len(oids))
		}
		return len(got)
	}
	for _, unstamped := range []bool{true, false} {
		q := tipsQ(repo, "", oids...)
		q.Unstamped = unstamped
		name := map[bool]string{true: "unstamped", false: "stamped"}[unstamped]
		if n := all(name, q, gitlet.MaxGitAnswerBytes); n < 3 {
			t.Errorf("%s: the hundred tips took %d pages, want a case of many", name, n)
		}
		if !unstamped {
			if a := mustRead(t, s, q); len(a.PushedBy) == 0 {
				t.Errorf("the premise: a stamped page names no push (%+v)", a.Coverage)
			}
		}
		query.SetPageBytes(s, 4<<20)
		whole := mustRead(t, s, q)
		if whole.Next != "" {
			t.Fatalf("%s: four MiB hold %d tips, want all", name, len(whole.Tips.Tips))
		}
		tight := encoded(t, whole) - 1
		query.SetPageBytes(s, tight)
		if n := all(name+" one byte short", q, tight); n < 2 {
			t.Errorf("%s: one byte short of the whole answer's bound, the tips took one page", name)
		}
		query.SetPageBytes(s, gitlet.MaxGitAnswerBytes)
	}
}

// A cold screen runs one check, one tag batch, one log and a count a commit; read
// again it runs nothing and answers the same; one branch moved costs that branch.
// Mutations: keep no fact; check the objects before the memo.
func TestAWarmTipsReadRunsNoGitAndAnswersTheSame(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	x.commit(map[string]string{"f": "0\n"}, "m0")
	x.commit(map[string]string{"f": "1\n"}, "m1")
	main := x.commit(map[string]string{"f": "2\n"}, "m2")
	x.git("tag", "-a", "r1", "-m", "release one", "main~1")
	x.git("tag", "-a", "r2", "-m", "release two", "main~2")
	oids := []string{main}
	var refs []string
	for i := range 27 {
		b := fmt.Sprintf("b%02d", i)
		x.branch(b, main)
		oids = append(oids, x.p.Commit(x.work, map[string]string{"g": b}, b))
		refs = append(refs, b)
	}
	x.git("checkout", "-q", "main")
	x.push(gitlettest.RepoName, append(refs, "refs/tags/r1", "refs/tags/r2")...)
	oids = append(oids, x.rev("refs/tags/r1"), x.rev("refs/tags/r2"))
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})

	cold := mustRead(t, s, tipsQ(repo, main, oids...))
	if len(cold.Tips.Tips) != len(oids) || cold.Next != "" || cold.Memo {
		t.Fatalf("the cold read answered %d tips, next %q, memo %t; want all %d cut now", len(cold.Tips.Tips),
			cold.Next, cold.Memo, len(oids))
	}
	cmds := kinds(rec, 0)
	for k, want := range map[string]int{"cat-file --batch-check": 1, "cat-file --batch": 1, "log": 1,
		"rev-list": len(oids) - 1} {
		if n := ran(cmds, k); n != want {
			t.Errorf("the cold read ran %s %d times, want %d: %v", k, n, want, cmds)
		}
	}
	if len(cmds) != len(oids)+2 {
		t.Errorf("the cold read ran %v, want those alone", cmds)
	}

	before := rec.count()
	warm := mustRead(t, s, tipsQ(repo, main, oids...))
	if n := rec.count() - before; n != 0 {
		t.Errorf("the warm read ran %d processes, want none: %v", n, kinds(rec, before))
	}
	if !warm.Memo {
		t.Error("the warm read is not marked as the memo's")
	}
	warm.Memo = false
	if got, want := mustJSON(t, warm), mustJSON(t, cold); got != want {
		t.Errorf("the warm read answered\n%s\nwant the cold one's\n%s", got, want)
	}

	x.git("checkout", "-q", "b00")
	moved := x.p.Commit(x.work, map[string]string{"g": "moved"}, "b00 moved")
	x.push(gitlettest.RepoName, "b00")
	x.settle(0, repo)
	screen := slices.Clone(oids)
	screen[1] = moved
	before = rec.count()
	again := mustRead(t, s, tipsQ(repo, main, screen...))
	cmds = kinds(rec, before)
	if !slices.Equal(cmds, []string{"cat-file --batch-check", "log", "rev-list"}) {
		t.Errorf("the screen with one branch moved ran %v, want one check, one log and one count", cmds)
	}
	if fed := rec.since(before); len(fed) == 3 && !slices.Equal(fed[1].cmd.Args[len(fed[1].cmd.Args)-1:],
		[]string{moved}) {
		t.Errorf("the log after one branch moved named %v, want the moved head alone", fed[1].cmd.Args)
	}
	if b, a := x.sides(main, moved); !counted(again.Tips.Tips[1], b, a) || again.Memo {
		t.Errorf("the moved branch counts %s (memo %t), want git's %d/%d cut now", countsOf(again.Tips.Tips[1]),
			again.Memo, b, a)
	}
	onlyTemplates(t, rec)
}

// mustJSON is a's encoding.
func mustJSON(t *testing.T, a gitlet.GitAnswer) string {
	t.Helper()
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A cold and a warm tips read of a unit repository are each recorded, a company
// one's never; a cursor past the last tip is refused.
// Mutation: answer a warm read before the audit.
func TestATipsReadOfAUnitRepositoryIsRecorded(t *testing.T) {
	t.Parallel()
	settings := gitlettest.Settings()
	settings.Visibility = gitlet.VisibilityUnit
	x := newFixture(t, gitlettest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(gitlettest.RepoName)
	rec := &fetches{}
	runs := &recording{}
	s := x.server(0, query.Options{Git: runs.on(x, 0), Roster: company, Fetched: rec.record})
	read := func(who gitlet.Principal, repo string) gitlet.GitAnswer {
		t.Helper()
		q := tipsQ(repo, head, head)
		q.Asker = who
		return mustRead(t, s, q)
	}
	first := read(seat("eng-dev"), repo)
	ran := runs.count()
	again := read(seat("eng-lead"), repo)
	if runs.count() != ran || !again.Memo {
		t.Fatalf("the premise: the second seat's read ran git %v, want the memo's answer", kinds(runs, ran))
	}
	want := []types.CodeFetched{
		{RepoID: repo, RepoName: gitlettest.RepoName, PrincipalKind: types.CodeReaderAgent, Principal: "eng-dev",
			Seat: "eng-dev", Via: types.FetchedViaTool, Kind: string(gitlet.GitTips), Reads: 1,
			Bytes: int64(encoded(t, first))},
		{RepoID: repo, RepoName: gitlettest.RepoName, PrincipalKind: types.CodeReaderAgent, Principal: "eng-lead",
			Seat: "eng-lead", Via: types.FetchedViaTool, Kind: string(gitlet.GitTips), Reads: 1,
			Bytes: int64(encoded(t, again))},
	}
	if got := rec.all(); !slices.Equal(got, want) {
		t.Errorf("the reads recorded are %+v, want the two seats' %+v", got, want)
	}
	x.f.Declare(t, gitlet.Declared{Name: "open", Settings: gitlettest.Settings()})
	x.push("open", "main")
	read(person, x.repo("open"))
	if after := rec.all(); len(after) != len(want) {
		t.Errorf("a read of a visibility: company repository was recorded: %+v", after[len(want):])
	}

	q := tipsQ(repo, "", head)
	q.Cursor = "t:1"
	if _, err := s.Read(t.Context(), q); !errors.Is(err, gitlet.ErrBadCursor) {
		t.Errorf("a cursor past the last tip: %v, want ErrBadCursor", err)
	}
}
