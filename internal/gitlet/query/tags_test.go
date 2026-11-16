package query_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/gitref"
)

// tagsQ is a person's tags read of repo, a page of limit tags.
func tagsQ(repo string, limit int) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitTags, RepoID: repo, Asker: person, Home: true,
		Tags: &gitlet.TagsQuery{Limit: limit}}
}

// annotate tags rev in the work tree as name, its tagger dated secs.
func (x *fixture) annotate(name, rev string, secs int64) string {
	x.t.Helper()
	x.p.Git().Env(fmt.Sprintf("GIT_COMMITTER_DATE=@%d +0000", secs)).Run(x.t, x.work, "tag", "-a", name, "-m",
		"tag "+name, rev)
	return x.rev("refs/tags/" + name)
}

// mktag makes a tag object of body in the work tree (git mktag) and names it
// name: a tag no `git tag` writes, as an import brings.
func (x *fixture) mktag(name, body string) string {
	x.t.Helper()
	out, stderr, err := x.p.Git().Try(x.t, x.work, strings.NewReader(body), "mktag", "--no-strict")
	if err != nil {
		x.t.Fatalf("git mktag %s: %v\n%s", name, err, stderr)
	}
	oid := strings.TrimSpace(out)
	x.git("update-ref", "refs/tags/"+name, oid)
	return oid
}

// tagObject is a tag object's text: of obj, a typ, by a tagger dated secs,
// none where secs is "".
func tagObject(obj, typ, name, secs string) string {
	body := fmt.Sprintf("object %s\ntype %s\ntag %s\n", obj, typ, name)
	if secs != "" {
		body += "tagger T <t@example.com> " + secs + " +0000\n"
	}
	return body + "\n" + name + "\n"
}

// gitTags is git's own listing of the work tree's tags, each its ref, type,
// what it peels to and its type, and its creatordate in seconds or "-".
func (x *fixture) gitTags() []string {
	x.t.Helper()
	var out []string
	for line := range strings.Lines(x.git("for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(objectname)%00%(objecttype)%00%(creatordate:unix)", "refs/tags/")) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\x00")
		peeled := x.rev(f[0] + "^{}")
		date := f[3]
		if date == "" {
			date = "-"
		}
		out = append(out, strings.Join([]string{f[0], f[1], f[2], peeled,
			strings.TrimSpace(x.git("cat-file", "-t", peeled)), date}, " "))
	}
	return out
}

// tagLine is a page's row as gitTags spells git's, and whether its date is
// carried as JSON writes it.
func tagLine(r gitlet.TagRow) (string, bool) {
	date, ok := "-", true
	switch {
	case r.Date != nil:
		date = fmt.Sprint(r.Date.Unix())
		ok = r.DateUnix == 0 && r.Date.Unix() <= gitlet.LastJSONSecond
	case r.DateUnix != 0:
		date = fmt.Sprint(r.DateUnix)
		ok = r.DateUnix > gitlet.LastJSONSecond
	}
	return strings.Join([]string{r.Ref, r.OID, string(r.Type), r.Peeled, string(r.PeeledType), date}, " "), ok
}

// allTags reads every page of q, answering the rows in order.
func allTags(t *testing.T, s *query.Server, q gitlet.GitQuery) ([]gitlet.TagRow, []gitlet.GitAnswer) {
	t.Helper()
	var rows []gitlet.TagRow
	var answers []gitlet.GitAnswer
	for range 1_000 {
		a := mustRead(t, s, q)
		if a.Kind != gitlet.GitTags || a.Tags == nil || a.Tags.Tags == nil {
			t.Fatalf("a tags read answered %+v", a)
		}
		rows = append(rows, a.Tags.Tags...)
		answers = append(answers, a)
		if a.Next == "" {
			return rows, answers
		}
		q.Cursor = a.Next
	}
	t.Fatal("the tags read never ended")
	return nil, nil
}

// A TAGS READ LISTS TAGS AS GIT DOES: `for-each-ref --sort=-creatordate`.
//
// Mutations: ties by name descending, author dates, date past 9999, undated
// tags after the epoch's → red.
func TestATagsReadListsTagsAsGitDoes(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	if err := os.WriteFile(filepath.Join(x.work, "f"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	x.git("add", "f")
	x.p.Git().Env("GIT_COMMITTER_DATE=@1700000500 +0000", "GIT_AUTHOR_DATE=@1600000000 +0000").Run(t, x.work,
		"commit", "-q", "-m", "base")
	base := x.rev("HEAD")
	tree := x.rev(base + "^{tree}")
	x.annotate("b-annotated", base, 1700000900)
	x.annotate("a-annotated", base, 1700000100)
	x.git("tag", "z-light", base)
	x.git("tag", "y-light", base)
	blob := strings.TrimSpace(x.git("rev-parse", "HEAD:f"))
	x.git("tag", "blob-light", blob)
	x.mktag("c-epoch", tagObject(base, "commit", "c-epoch", "0"))
	x.mktag("d-undated", tagObject(base, "commit", "d-undated", ""))
	x.mktag("tree-annotated", tagObject(tree, "tree", "tree-annotated", "1700000300"))
	x.mktag("tag-of-tag", tagObject(x.rev("refs/tags/a-annotated"), "tag", "tag-of-tag", "1700000700"))
	x.mktag("far", tagObject(base, "commit", "far", "253402300800"))
	x.mktag("farthest", tagObject(base, "commit", "farthest", "9223372036854775807"))
	far, stderr, err := x.p.Git().Try(t, x.work, strings.NewReader(fmt.Sprintf("tree %s\nparent %s\n"+
		"author A <a@example.com> 1600000000 +0000\ncommitter A <a@example.com> 253402300801 +0000\n\nfar\n",
		tree, base)), "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		t.Fatalf("hash-object: %v\n%s", err, stderr)
	}
	x.git("tag", "far-commit", strings.TrimSpace(far))
	x.push(gitlettest.RepoName, "main", "refs/tags/*:refs/tags/*")
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})

	a := mustRead(t, s, tagsQ(repo, gitlet.MaxTagsLimit))
	if a.Next != "" || a.Tags == nil || a.Tags.Total != len(a.Tags.Tags) {
		t.Fatalf("a page of 100 answered %+v, want every tag", a)
	}
	var got []string
	for _, r := range a.Tags.Tags {
		line, ok := tagLine(r)
		if !ok {
			t.Errorf("%s carries its date as %v and %d: date up to the year 9999, date_unix past it", r.Ref, r.Date,
				r.DateUnix)
		}
		got = append(got, line)
	}
	if want := x.gitTags(); !slices.Equal(got, want) {
		t.Errorf("the tags read lists\n%s\nand git\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if a.Position.IsZero() || a.PushedBy != nil || a.Tagged != nil {
		t.Errorf("the tags page is stamped at %v with pushes %v and tags %v: want a position alone", a.Position,
			a.PushedBy, a.Tagged)
	}
}

// A TAGS PAGE GOES ON FROM ITS CURSOR, a key, across pushes and deletions.
//
// Mutation: a positional cursor → red.
func TestATagsPageGoesOnFromItsCursor(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	for i := range 7 {
		x.annotate(fmt.Sprintf("t%d", i), head, int64(1700000000+i))
	}
	x.push(gitlettest.RepoName, "refs/tags/*:refs/tags/*")
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	whole, _ := allTags(t, s, tagsQ(repo, 2))
	var names []string
	for _, r := range whole {
		names = append(names, r.Ref)
	}
	if want := []string{"refs/tags/t6", "refs/tags/t5", "refs/tags/t4", "refs/tags/t3", "refs/tags/t2",
		"refs/tags/t1", "refs/tags/t0"}; !slices.Equal(names, want) {
		t.Fatalf("pages of two list %v, want %v", names, want)
	}

	first := mustRead(t, s, tagsQ(repo, 2))
	x.annotate("newest", head, 1800000000)
	x.push(gitlettest.RepoName, ":refs/tags/t4", "refs/tags/newest")
	x.synced(0, repo)
	q := tagsQ(repo, 2)
	q.Cursor = first.Next
	rest, _ := allTags(t, s, q)
	var after []string
	for _, r := range rest {
		after = append(after, r.Ref)
	}
	if want := []string{"refs/tags/t3", "refs/tags/t2", "refs/tags/t1", "refs/tags/t0"}; !slices.Equal(after, want) {
		t.Errorf("past the first page after a push and a deletion the pages list %v, want %v", after, want)
	}
}

// A TAGS PAGE IS CUT BY ITS BYTES, every tag on one page once.
//
// Mutation: cost a row without its names' escapes → a page past the bound, red.
func TestATagsPageIsCutByItsBytes(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	var stdin strings.Builder
	for i := range 300 {
		name := fmt.Sprintf("%03d", i) + strings.Repeat("<", 242)
		fmt.Fprintf(&stdin, "create refs/tags/%s %s\n", name, head)
	}
	if _, stderr, err := x.p.Git().Try(t, x.work, strings.NewReader(stdin.String()), "update-ref", "--stdin"); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, stderr)
	}
	x.mktag("far", tagObject(head, "commit", "far", "253402300800"))
	x.push(gitlettest.RepoName, "refs/tags/*:refs/tags/*")
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	var rows []gitlet.TagRow
	q := tagsQ(repo, gitlet.MaxTagsLimit)
	for _, a := range pages(t, s, q, gitlet.MaxGitAnswerBytes) {
		rows = append(rows, a.Tags.Tags...)
	}
	if len(rows) != 301 || rows[0].Ref != "refs/tags/far" {
		t.Fatalf("the pages hold %d tags, the first %s, want 301 with the far tag first", len(rows), rows[0].Ref)
	}
	for i, r := range rows[1:] {
		if want := fmt.Sprintf("refs/tags/%03d", i); !strings.HasPrefix(r.Ref, want) {
			t.Fatalf("row %d is %s, want %s…", i+1, r.Ref, want)
		}
	}
}

// EVERY TAGS PAGE FITS ITS BOUND, at each bound across one row's width, so
// some bound leaves a page no slack to hide an uncounted byte in.
//
// Mutation: leave the comma out of the cost → a page past the bound, red.
func TestEveryTagsPageFitsItsBound(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	const tags = 130
	var stdin strings.Builder
	var want []string
	for i := range tags {
		// Names at their longest, so the envelope's widest cursor is close to
		// the one a page carries.
		ref := fmt.Sprintf("refs/tags/t%03d", i)
		ref += strings.Repeat("x", gitref.MaxRefNameBytes-len(ref))
		fmt.Fprintf(&stdin, "create %s %s\n", ref, head)
		want = append(want, ref)
	}
	if _, stderr, err := x.p.Git().Try(t, x.work, strings.NewReader(stdin.String()), "update-ref", "--stdin"); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, stderr)
	}
	x.push(gitlettest.RepoName, "refs/tags/*:refs/tags/*")
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	first := mustRead(t, s, tagsQ(repo, gitlet.MaxTagsLimit))
	if len(first.Tags.Tags) != gitlet.MaxTagsLimit {
		t.Fatalf("the first page holds %d tags, want %d", len(first.Tags.Tags), gitlet.MaxTagsLimit)
	}
	row := 0
	for _, r := range first.Tags.Tags {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if row != 0 && len(b) != row {
			t.Fatalf("%s encodes to %d bytes and the first row to %d: want rows of one size", r.Ref, len(b), row)
		}
		row = len(b)
	}
	// A page of n rows is its envelope, the rows and n-1 commas.
	envelope := encoded(t, first) - gitlet.MaxTagsLimit*(row+1) + 1
	for bound := envelope + 60*row; bound <= envelope+61*row; bound++ {
		query.SetPageBytes(s, bound)
		var names []string
		for _, a := range pages(t, s, tagsQ(repo, gitlet.MaxTagsLimit), bound) {
			for _, r := range a.Tags.Tags {
				names = append(names, r.Ref)
			}
		}
		if !slices.Equal(names, want) {
			t.Fatalf("under a bound of %d bytes the pages list %d tags, want each of %d once in order", bound,
				len(names), tags)
		}
	}
}

// tagged is the tags a page names commit id by, as refs and more.
func tagged(a gitlet.GitAnswer, id string) ([]string, int, bool) {
	if a.Tagged == nil {
		return nil, 0, false
	}
	c, ok := a.Tagged.Commits[id]
	return c.Refs, c.More, ok
}

// A HISTORY PAGE NAMES THE TAGS OF ITS COMMITS, newest first, from the memo
// too.
//
// Mutations: memoize tagged with the page → the new tag is missing; a
// commit's tags by name → red.
func TestAHistoryPageNamesTheTagsOfItsCommits(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	c := x.commit(map[string]string{"f": "c\n"}, "c")
	b := x.commit(map[string]string{"f": "b\n"}, "b")
	a := x.commit(map[string]string{"f": "a\n"}, "a")
	x.commit(map[string]string{"f": "top\n"}, "top")
	x.annotate("a-annotated", a, 1700000000)
	x.git("tag", "a-light", a)
	for i := range 10 {
		x.annotate(fmt.Sprintf("b%02d", i), b, int64(1700000000+i))
	}
	x.push(gitlettest.RepoName, "refs/tags/*:refs/tags/*")
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})

	page := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: a}))
	refs, more, ok := tagged(page, a)
	want := strings.Fields(x.git("for-each-ref", "--sort=-creatordate", "--format=%(refname)", "--points-at", a,
		"refs/tags/"))
	if !ok || more != 0 || len(want) != 2 || !slices.Equal(refs, want) {
		t.Errorf("commit a is named by %v and %d more, want %v", refs, more, want)
	}
	refs, more, ok = tagged(page, b)
	if !ok || more != 2 || len(refs) != 8 || refs[0] != "refs/tags/b09" {
		t.Errorf("commit b is named by %v and %d more, want the newest 8 and 2 more", refs, more)
	}
	if _, _, ok = tagged(page, c); ok {
		t.Errorf("an untagged commit is named by tags")
	}
	untagged := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: c, Limit: 1}))
	if untagged.Tagged == nil || len(untagged.Tagged.Commits) != 0 {
		t.Errorf("a page of untagged commits carries %+v, want an empty tagged", untagged.Tagged)
	}
	bare := logQ(repo, gitlet.LogQuery{Rev: a})
	bare.Unstamped = true
	if got := mustRead(t, s, bare); got.Tagged != nil {
		t.Errorf("an unstamped read carries tagged %+v", got.Tagged)
	}
	again := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: a}))
	if !again.Memo || !reflect.DeepEqual(again.Tagged, page.Tagged) {
		t.Errorf("the same read again (memo %t) names tags %+v, want %+v", again.Memo, again.Tagged, page.Tagged)
	}
	x.git("tag", "c-light", c)
	x.push(gitlettest.RepoName, "refs/tags/c-light")
	x.synced(0, repo)
	later := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: a}))
	if refs, _, ok := tagged(later, c); !later.Memo || !ok || !slices.Equal(refs, []string{"refs/tags/c-light"}) {
		t.Errorf("the memo's page (memo %t) names commit c by %v, want the tag pushed since", later.Memo, refs)
	}
}

// A PAGE WHOSE COMMITS ARE TAGGED FITS ITS BOUND.
//
// Mutation: leave tagCost out of take → a page past the bound, red.
func TestAPageWhoseCommitsAreTaggedFitsItsBound(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	var stdin strings.Builder
	for i := range 100 {
		id := x.p.Commit(x.work, map[string]string{"f": fmt.Sprintf("%d\n", i)}, fmt.Sprintf("c%d", i))
		for j := range 8 {
			fmt.Fprintf(&stdin, "create refs/tags/%03d-%d%s %s\n", i, j, strings.Repeat("<", 240), id)
		}
	}
	if _, stderr, err := x.p.Git().Try(t, x.work, strings.NewReader(stdin.String()), "update-ref", "--stdin"); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, stderr)
	}
	x.push(gitlettest.RepoName, "main")
	// In parts: one push's ref updates are bounded by its record.
	for g := range 10 {
		x.push(gitlettest.RepoName, fmt.Sprintf("refs/tags/0%d*:refs/tags/0%d*", g, g))
	}
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	seen := map[string]int{}
	for _, a := range pages(t, s, logQ(repo, gitlet.LogQuery{Rev: x.rev("main"), Limit: gitlet.MaxLogLimit}),
		gitlet.MaxGitAnswerBytes) {
		for _, c := range a.Log.Commits {
			seen[c.OID]++
			if refs, _, _ := tagged(a, c.OID); len(refs) != 8 {
				t.Errorf("commit %s is named by %d tags, want its 8", c.OID, len(refs))
			}
		}
	}
	if len(seen) != 100 {
		t.Errorf("the pages hold %d commits, want 100", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("commit %s is on %d pages", id, n)
		}
	}
}

// noTags is a node's cache that keeps no tag index it can read.
type noTags struct{ query.Cache }

func (noTags) Tags(cache.Synced) (*cache.TagIndex, bool) { return nil, false }

// A TAGS READ ON A COPY KEEPING NO INDEX IS UNREADY, and a history names none.
//
// Mutation: answer an empty listing → red.
func TestATagsReadOnACopyKeepingNoIndexIsUnready(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	x.git("tag", "v1", head)
	x.push(gitlettest.RepoName, "refs/tags/v1")
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{Cache: noTags{x.f.Nodes()[0].Cache()}})
	if _, err := s.Read(t.Context(), tagsQ(repo, 0)); !unready(err, gitlet.UnreadySyncing) {
		t.Errorf("a tags read with no index answered %v, want unready, syncing", err)
	}
	if a := mustRead(t, s, logQ(repo, gitlet.LogQuery{Rev: head})); a.Tagged != nil {
		t.Errorf("a history read with no index carries tagged %+v", a.Tagged)
	}
}

// A TAGS CURSOR IS ONE A TAGS READ ISSUED: canonical decimal, strict base64url
// and a name that is a ref, or the read is refused; the widest passes.
//
// Mutation: accept a non-canonical date → red.
func TestATagsCursorIsOneATagsReadIssued(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	s := x.server(0, query.Options{})
	for _, bad := range []string{"t:1", "d:01:dg", "d:-1:dg", "d:1:dg==", "d:1:" + "fi4", "d:1:",
		"d:12345678901234567890:dg", "d:1:dg:x"} {
		q := tagsQ(repo, 0)
		q.Cursor = bad
		if _, err := s.Read(t.Context(), q); !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("cursor %q answered %v, want ErrBadCursor", bad, err)
		}
	}
	widest := "d:9223372036854775807:" + strings.Repeat("eHh4", 81) + "eHg"
	q := tagsQ(repo, 0)
	q.Cursor = widest
	if err := q.Validate(); err != nil || len(widest) != 349 {
		t.Errorf("the widest cursor (%d bytes) is refused: %v", len(widest), err)
	}
	if a, err := s.Read(t.Context(), q); err != nil || len(a.Tags.Tags) != 0 {
		t.Errorf("a read past the widest cursor answered %+v, %v", a.Tags, err)
	}
}

// A TAGS READ IS RECORDED where the repository is a unit's, by its kind.
func TestATagsReadIsRecorded(t *testing.T) {
	t.Parallel()
	settings := gitlettest.Settings()
	settings.Visibility = gitlet.VisibilityUnit
	x := newFixture(t, gitlettest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	x.git("tag", "v1", head)
	x.push(gitlettest.RepoName, "refs/tags/v1")
	repo := x.repo(gitlettest.RepoName)
	x.synced(0, repo)
	rec := &fetches{}
	s := x.server(0, query.Options{Roster: company, Fetched: rec.record})
	q := tagsQ(repo, 0)
	q.Asker = seat("eng-dev")
	a := mustRead(t, s, q)
	want := types.CodeFetched{RepoID: repo, RepoName: gitlettest.RepoName, PrincipalKind: types.CodeReaderAgent,
		Principal: "eng-dev", Seat: "eng-dev", Via: types.FetchedViaTool, Kind: string(gitlet.GitTags), Reads: 1,
		Bytes: int64(encoded(t, a))}
	if got := rec.all(); len(got) != 1 || got[0] != want {
		t.Errorf("the tags read is recorded as %+v, want %+v", got, want)
	}
	x.f.Declare(t, gitlet.Declared{Name: "open", Settings: gitlettest.Settings()})
	x.push("open", "main")
	open := x.repo("open")
	x.synced(0, open)
	mustRead(t, s, tagsQ(open, 0))
	if got := rec.all(); len(got) != 1 {
		t.Errorf("a company repository's tags read was recorded: %+v", got[1:])
	}
}
