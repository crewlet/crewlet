package query_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE MEMO (memo.go): a page a read's git cut is answered again from the
// copy's memo with no git run — the same page — and the run that misses cuts
// every page after the asked one too, so a read paged through costs one run.

// memoless is a node's cache with its memo hidden: every lookup misses and no
// run memoizes anything, so every page a server over it answers is one read
// from its cursor cut — the reference a memoized page is held to.
type memoless struct{ query.Cache }

func (memoless) MemoGet(cache.Synced, cache.MemoKey) ([]byte, bool) { return nil, false }
func (memoless) Memo(cache.Synced) *cache.MemoBatch                 { return nil }

// unstamped is a with every field Read stamps on an answer cleared — its
// position, the push each commit came in by, what the index holds, and Memo —
// which are the moment's, never the page's: the page itself.
func unstamped(a code.GitAnswer) code.GitAnswer {
	a.Memo, a.Position, a.PushedBy, a.Coverage = false, statelog.Position{}, nil, nil
	return a
}

// settle wants node i's history of repo read and waits until its index holds
// all of it with no gap, so what every read of it stamps stays the same from
// then on: a page the memo kept is answered, rather than cut again for a
// stamp the backfill grew past what the page left of its room (memo.go's
// budget) — what a case asserting hits holds still.
func (x *fixture) settle(i int, repo string) {
	x.t.Helper()
	c := x.f.Nodes()[i].Cache()
	c.WantHistory(repo)
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err := c.Sync(x.t.Context(), repo, codetest.Linearizable)
		if err != nil {
			x.t.Fatalf("sync node %d's copy of %s: %v", i, repo, err)
		}
		p, err := c.Provenance(s)
		s.Release()
		if err == nil && p.Indexed() {
			if cov := p.Coverage(); cov.Complete && cov.GapThrough.IsZero() {
				return
			}
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("node %d's index of %s holds no whole history 30s on (%v)", i, repo, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ran counts the processes of kind among cmds: their subcommand, and for
// cat-file its mode too.
func ran(cmds []string, kind string) int {
	n := 0
	for _, c := range cmds {
		if c == kind {
			n++
		}
	}
	return n
}

// kinds is each recorded process as ran counts it.
func kinds(r *recording, from int) []string {
	var out []string
	for _, c := range r.ran()[from:] {
		k := c.Args[0]
		if k == "cat-file" && len(c.Args) > 1 {
			k += " " + c.Args[1]
		}
		out = append(out, k)
	}
	return out
}

// A PAGED ANSWER RUNS ITS GIT ONCE. A read paged through — a recursive
// listing, a file (with a line longer than a page, served in pieces), a
// file's line range, a diff's stat, the same stat filtered by a path (whose
// counts diff-pairs takes after the walk) and one file's patch, each some
// twenty pages at a small bound — runs the kind's git once, for its first
// page: that run cuts every page after it, and every page read after it — the
// first again among them — is answered from the memo, with no git process at
// all and saying so. Each memoized page is exactly the page a read from its
// cursor cuts, which a server with no memo answers.
//
// Mutation: memoize only the asked page (memoRun's pages past it not cut) —
// every page past the first runs the kind's git again.
func TestAPagedAnswerRunsItsGitOnce(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	files := map[string]string{}
	for i := range 100 {
		files[fmt.Sprintf("many/file-with-a-long-name-%03d.txt", i)] = fmt.Sprintf("line one of %d\n", i)
	}
	var big, patched strings.Builder
	for i := range 300 {
		fmt.Fprintf(&big, "line %03d of a file read page by page, long enough to fill one\n", i)
		if i == 150 {
			big.WriteString(strings.Repeat("ünicode and a line longer than any page ", 80) + "\n")
		}
		fmt.Fprintf(&patched, "%03d before\n", i)
	}
	files["big.txt"] = big.String()
	files["patched.txt"] = patched.String()
	base := x.commit(files, "base")
	for i := range 100 {
		files[fmt.Sprintf("many/file-with-a-long-name-%03d.txt", i)] = fmt.Sprintf("line two of %d\n", i)
	}
	files["patched.txt"] = strings.ReplaceAll(patched.String(), "before", "after, changed")
	head := x.commit(files, "head")
	repo := x.repo(codetest.RepoName)
	x.settle(0, repo)

	for _, tc := range []struct {
		name string
		q    code.GitQuery
		// git is the kind's own processes, each of which runs once.
		git []string
	}{
		{"a recursive listing", treeQ(repo, code.TreeQuery{Rev: head, Recursive: true}), []string{"ls-tree"}},
		{"a file", blobQ(repo, code.BlobQuery{Rev: head, Path: "big.txt"}), []string{"cat-file blob"}},
		{"a file's line range", blobQ(repo, code.BlobQuery{Rev: head, Path: "big.txt", StartLine: 120,
			EndLine: 200}), []string{"cat-file blob"}},
		{"a diff's stat", diffQ(repo, code.DiffQuery{Base: base, Head: head}), []string{"diff-tree"}},
		{"a diff's stat filtered by a path", diffQ(repo, code.DiffQuery{Base: base, Head: head,
			Paths: []string{"many"}}), []string{"diff-tree", "diff-pairs"}},
		{"a file's patch", diffQ(repo, code.DiffQuery{Base: base, Head: head, File: "patched.txt"}),
			[]string{"diff-tree", "diff-pairs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := 1<<10 + x.decoration(tc.q)
			rec := &recording{}
			s := x.server(0, query.Options{Git: rec.on(x, 0)})
			query.SetPageBytes(s, limit)
			ref := x.server(0, query.Options{Cache: memoless{x.f.Nodes()[0].Cache()}})
			query.SetPageBytes(ref, limit)

			first := mustRead(t, s, tc.q)
			firstRan := len(rec.ran())
			got := pages(t, s, tc.q, limit)
			if len(got) < 10 {
				t.Fatalf("the read is %d pages at its bound, want a case of many", len(got))
			}
			if !reflect.DeepEqual(unstamped(got[0]), unstamped(first)) {
				t.Errorf("the first page read again =\n%+v\nwant the one cut\n%+v", got[0], first)
			}
			if n := len(rec.ran()) - firstRan; n != 0 {
				t.Errorf("the %d pages after the first read ran %d processes, want none: %v", len(got), n,
					kinds(rec, firstRan))
			}
			cmds := kinds(rec, 0)
			for _, k := range tc.git {
				if n := ran(cmds, k); n != 1 {
					t.Errorf("%d pages ran %s %d times, want once: %v", len(got), k, n, cmds)
				}
			}
			for i, p := range got {
				if !p.Memo {
					t.Errorf("page %d was cut again, want it from the memo the first page's run filled", i)
				}
			}

			// Each page from the memo is the page a read from its cursor
			// cuts.
			q := tc.q
			for i, p := range got {
				want := mustRead(t, ref, q)
				if !reflect.DeepEqual(unstamped(p), unstamped(want)) {
					t.Errorf("page %d of the run =\n%+v\nwant what a read from its cursor cuts\n%+v", i, p, want)
				}
				q.Cursor = p.Next
			}
		})
	}
}

// A COMPARISON IS MEMOIZED WHOLE: it is one page — the two commits' merge
// base, which contains which, what each has that the other lacks, and the
// first page of what head changed since — so a second read of it runs none
// of its three processes, answering the same.
//
// Mutation: memoize no comparison (compareRead's put) — the second read runs
// them again.
func TestAComparisonIsMemoizedWhole(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	base := x.commit(map[string]string{"a": "1\n"}, "base")
	x.commit(map[string]string{"b": "2\n"}, "side")
	head := x.commit(map[string]string{"a": "3\n"}, "head")
	repo := x.repo(codetest.RepoName)
	x.settle(0, repo)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})

	first := mustRead(t, s, compareQ(repo, base, head))
	before := len(rec.ran())
	again := mustRead(t, s, compareQ(repo, base, head))
	if n := len(rec.ran()) - before; n != 0 || !again.Memo {
		t.Errorf("the second read of a comparison ran %d processes (memo %t), want none and from the memo", n,
			again.Memo)
	}
	if !reflect.DeepEqual(unstamped(again), unstamped(first)) {
		t.Errorf("the comparison from the memo =\n%+v\nwant the one cut\n%+v", again, first)
	}
}

// A MEMO HIT HOLDS NO SHARE: the share bounds the git a seat's read runs, and
// a hit runs none, so a read answered from the memo is answered while every
// unit of the node's share is taken.
//
// Mutation: take a unit before asking the memo — the hit waits out
// GitShareWait and is unready (share).
func TestAMemoHitHoldsNoShare(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	x.settle(0, repo)
	n := x.f.Nodes()[0]
	share := code.NewGitShare(n.ID(), 2, nil) // one unit
	s := x.server(0, query.Options{Share: share})
	q := treeQ(repo, code.TreeQuery{Rev: head})
	mustRead(t, s, q)
	release, err := share.Take(t.Context(), code.SeatWorkRead)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	a, err := s.Read(t.Context(), q)
	if err != nil || !a.Memo {
		t.Errorf("a memoized read while the share is held = memo %t, %v; want it answered from the memo", a.Memo,
			err)
	}
}

// AN ABBREVIATION IS ANSWERED FROM THE MEMO ONCE EXPANDED. What an
// abbreviation names can change as the copy gains objects, what a whole id
// names never, so a read naming one asks the memo only once its expansion —
// from the objects — and its check have run: by the whole id, which a read of
// the whole id memoized. The kind's own git does not run.
func TestAnAbbreviationIsAnsweredFromTheMemoOnceExpanded(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	head := x.commit(map[string]string{"src/a.go": "package a\n", "b.txt": "b\n"}, "one")
	repo := x.repo(codetest.RepoName)
	x.settle(0, repo)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	whole := mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: head}))
	before := len(rec.ran())
	short := mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: head[:9]}))
	cmds := kinds(rec, before)
	if !short.Memo || ran(cmds, "rev-parse") != 1 || ran(cmds, "ls-tree") != 0 {
		t.Errorf("the abbreviation's read ran %v (memo %t), want its expansion and its check, and the listing "+
			"from the memo", cmds, short.Memo)
	}
	if !reflect.DeepEqual(unstamped(short), unstamped(whole)) {
		t.Errorf("the abbreviation's listing =\n%+v\nwant the whole id's\n%+v", short, whole)
	}
}

// A MEMO HIT IS JUDGED BY THE GRANT FIRST. The memo is the copy's, shared by
// every asker and keyed by object ids, never by asker: a seat outside a
// `visibility: unit` repository is refused before the memo is asked, after a
// seat inside it memoized the page, and no lookup is counted for it.
//
// Mutation: ask the memo before the grant (recallWhole's grant) — the outsider
// is answered the insider's page.
func TestAMemoHitIsJudgedByTheGrantFirst(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	x := newFixture(t, codetest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	x.settle(0, repo)
	s := x.server(0, query.Options{Roster: company})
	q := treeQ(repo, code.TreeQuery{Rev: head})
	q.Asker = seat("eng-dev")
	mustRead(t, s, q)
	if a := mustRead(t, s, q); !a.Memo {
		t.Fatal("the insider's second read was not answered from the memo")
	}
	lookups := func() uint64 {
		return counter(x.f.Nodes()[0].Metrics(), metrics.CodeCacheMemoLookups, nil)
	}
	before := lookups()
	q.Asker = seat("sales-rep")
	if _, err := s.Read(t.Context(), q); !errors.Is(err, code.ErrNotPermitted) {
		t.Errorf("the outsider's read after the insider's = %v, want ErrNotPermitted", err)
	}
	if got := lookups() - before; got != 0 {
		t.Errorf("the outsider's refused read counted %d memo lookups, want none", got)
	}
}

// held is a query.Pool that holds the first process of subcommand sub until
// release is closed, telling entered once it is held, and records every
// process it runs beside.
type held struct {
	recording
	sub      string
	entered  chan struct{}
	release  chan struct{}
	holding  sync.Once
	occupied atomic.Bool
}

func (h *held) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if c.Args[0] == h.sub && h.occupied.CompareAndSwap(false, true) {
		h.holding.Do(func() { close(h.entered) })
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}
	return h.recording.Run(ctx, c)
}

// TWO READS OF ONE PAGE NO MEMO HOLDS RUN ONE GIT. A read that misses a page
// another read of the node is cutting waits for that read's run and is
// answered from the memo it filled — the same page — rather than run the same
// git beside it: so the seats a push's review fans out to, asking one page
// at once, cost the node one process, one write of the page's files and one
// unit of its share. The first read's listing is held until the second waits,
// so the two overlap whatever the machine's speed.
//
// Mutation: lead every miss (await answering at once) — the second read runs
// its own ls-tree beside the held one, and waits on nothing.
func TestTwoReadsOfOnePageNoMemoHoldsRunOneGit(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	files := map[string]string{}
	for i := range 40 {
		files[fmt.Sprintf("dir/file-%02d.txt", i)] = fmt.Sprintf("content %d\n", i)
	}
	head := x.commit(files, "a tree to list")
	repo := x.repo(codetest.RepoName)
	x.settle(0, repo)
	h := &held{sub: "ls-tree", entered: make(chan struct{}), release: make(chan struct{})}
	h.Pool = x.f.Nodes()[0].Pool()
	s := x.server(0, query.Options{Git: h})
	q := treeQ(repo, code.TreeQuery{Rev: head, Recursive: true})

	var (
		wg      sync.WaitGroup
		answers [2]code.GitAnswer
		errs    [2]error
	)
	read := func(i int) {
		defer wg.Done()
		answers[i], errs[i] = s.Read(t.Context(), q)
	}
	wg.Add(2)
	go read(0)
	select {
	case <-h.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first read ran no ls-tree within 30s")
	}
	go read(1)
	deadline := time.Now().Add(30 * time.Second)
	for query.Waiting(s) != 1 {
		if time.Now().After(deadline) {
			close(h.release)
			wg.Wait()
			t.Fatalf("the second read of the page the first is cutting waited on nothing within 30s; it ran %v",
				kinds(&h.recording, 0))
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(h.release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if n := ran(kinds(&h.recording, 0), "ls-tree"); n != 1 {
		t.Errorf("two reads of one page ran ls-tree %d times, want once: %v", n, kinds(&h.recording, 0))
	}
	if answers[0].Memo || !answers[1].Memo {
		t.Errorf("the first read answered from the memo %t and the second %t, want the first cut and the "+
			"second from its memo", answers[0].Memo, answers[1].Memo)
	}
	if !reflect.DeepEqual(unstamped(answers[0]), unstamped(answers[1])) {
		t.Errorf("the second read answered\n%+v\nwant the page the first cut\n%+v", answers[1], answers[0])
	}
}
