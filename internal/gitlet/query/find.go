package query

import (
	"cmp"
	"container/heap"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A find ranks, in Go, the tree's recursive listing as the memo keeps it by
// the tree's own id: every commit holding the tree shares one set of pages, a
// person typing runs git at most once per tree, and nothing is kept per query.

// findRead answers a find: the listing's pages walked from the memo, and from
// the first page it lacks ONE `ls-tree -z -l -r <tree>` under one unit of the
// share, staging that page and every later one.
func (r *read) findRead(ctx context.Context, objs found) (gitlet.GitAnswer, error) {
	fq := r.q.Find
	rev := objs.revs[fq.Rev]
	if rev.peeledKind != "commit" && rev.peeledKind != "tree" {
		return gitlet.GitAnswer{}, fmt.Errorf("%w: find.rev: %s names a %s; a find searches a commit's or a tree's "+
			"files", gitlet.ErrInvalid, fq.Rev, rev.peeledKind)
	}
	tree := objs.target.id
	f := newFinder(fq.Query, fq.Files())

	// THE LISTING'S OWN READ, asked as an unstamped tree read of the tree's id so
	// its pages are that read's; it shares the read's pinned copy, reads no index.
	sub := &read{s: r.s, synced: r.synced, linear: r.linear, indexRead: true, q: gitlet.GitQuery{
		Kind: gitlet.GitTree, RepoID: r.q.RepoID, Asker: r.q.Asker, Unstamped: true,
		Tree: &gitlet.TreeQuery{Rev: tree, Recursive: true}}}
	// A FLIGHT THE WALK LEADS ends once the find has answered, whatever it
	// answered: the pages its listing cut are placed by then.
	defer func() {
		if sub.landing != nil {
			sub.landing()
		}
	}()
	whole := map[string]string{tree: tree}
	at := cursor{}
	for {
		a, ok := sub.recall(whole, at)
		if !ok {
			var err error
			if a, ok, err = sub.await(ctx, whole, at); err != nil {
				return gitlet.GitAnswer{}, err
			}
		}
		if !ok {
			break
		}
		next, err := parseCursor(a.Next, gitlet.GitTree, map[byte]int{'e': 1})
		if err != nil || (a.Next != "" && next.a <= at.a) {
			// A page naming no later entry is none this build cuts: the
			// listing runs from it and stages it again.
			break
		}
		for _, e := range a.Tree.Entries {
			f.entry(e)
		}
		if a.Next == "" {
			return r.found(rev, tree, f), nil
		}
		at = next
	}

	release, err := r.s.o.Share.Take(ctx, r.q.SeatWork())
	if err != nil {
		if gitlet.AnswerDue(ctx) {
			return gitlet.GitAnswer{}, r.unready(gitlet.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole "+
				"share until the read's answer fell due, its asker's attempt spent; another Gitlet node answers "+
				"meanwhile", r.s.o.NodeID))
		}
		return gitlet.GitAnswer{}, err
	}
	defer release()
	// THE READ'S OWN GIT BEGINS, the listing's run charged to its one budget.
	r.metered, r.gitLeft = true, r.s.gitBudget
	sub.metered, sub.gitLeft = true, r.gitLeft
	var m *memoRun
	if b := r.s.o.Cache.Memo(r.synced); b != nil {
		m = &memoRun{r: sub, batch: b, whole: whole}
	}
	defer m.discard()
	_, err = sub.list(ctx, tree, "", tree, true, at, m, f.entry)
	r.gitLeft = sub.gitLeft
	switch {
	case errors.Is(err, gitlet.ErrSliceTooLarge):
		return gitlet.GitAnswer{}, fmt.Errorf("%w: the listing of tree %s did not finish in the read's %s of git, "+
			"and a find ranks every file of a tree or none: browse its directories instead",
			gitlet.ErrSliceTooLarge, tree, r.s.gitBudget)
	case err != nil:
		return gitlet.GitAnswer{}, err
	}
	release()
	return r.found(rev, tree, f), nil
}

// found is the find's answer: the finder's best first, as many as fit the room an
// answer naming rev and tree leaves, with how many match and how many files the
// tree holds. One match always fits: a ranked path is at most gitlet.MaxGitPathBytes.
func (r *read) found(rev object, tree string, f *finder) gitlet.GitAnswer {
	envelope := gitlet.GitAnswer{Kind: gitlet.GitFind, Rev: rev.peeled, Find: &gitlet.FindAnswer{Tree: tree,
		Matches: []gitlet.FindMatch{}, Total: widest, Files: widest}}
	left := r.room(envelope)
	out := &gitlet.FindAnswer{Tree: tree, Matches: []gitlet.FindMatch{}, Total: f.total, Files: f.files}
	for _, c := range f.best() {
		m := f.match(c)
		cost := jsonLen(m)
		if len(out.Matches) > 0 {
			cost++ // the comma before it
		}
		if cost > left {
			break
		}
		out.Matches, left = append(out.Matches, m), left-cost
	}
	return gitlet.GitAnswer{Kind: gitlet.GitFind, Rev: rev.peeled, Find: out}
}

// THE MATCH: a path's runes, folded as a log's literal folds them, hold the
// query's as a subsequence (fzf's linear forward and backward passes), each
// run of bytes that are not UTF-8 one position that nothing matches.

// noRune is the position of a run of bytes that are not UTF-8.
const noRune rune = -1

// positions appends to buf p's positions and answers them, with where its
// basename's begin. A run of invalid bytes is one position, as
// gitlet.WirePath carries it as one U+FFFD.
func positions(buf []rune, p string) ([]rune, int) {
	buf, base := buf[:0], 0
	bad := false
	for i := 0; i < len(p); {
		c, n := utf8.DecodeRuneInString(p[i:])
		i += n
		if c == utf8.RuneError && n == 1 {
			if !bad {
				buf = append(buf, noRune)
			}
			bad = true
			continue
		}
		bad = false
		if c == '/' {
			base = len(buf) + 1
		}
		buf = append(buf, fold(c))
	}
	return buf, base
}

// window is where q matches in t: its start and end positions, inclusive, or
// ok false where t does not hold q.
func window(t, q []rune) (s, e int, ok bool) {
	j := 0
	e = -1
	for i, c := range t {
		if c == q[j] {
			if j++; j == len(q) {
				e = i
				break
			}
		}
	}
	if e < 0 {
		return 0, 0, false
	}
	j = len(q) - 1
	for i := e; ; i-- {
		if t[i] == q[j] {
			if j--; j < 0 {
				return i, e, true
			}
		}
	}
}

// rank is a match's place in the order, smaller first: name before
// directories (tier), gaps, start, positions beyond the query, path length,
// and last the bytes, so every node ranks a tree the same.
type rank struct {
	tier, gaps, start, excess, length int
}

// candidate is one file the finder keeps: its rank and its listing entry,
// its path's exact bytes beside it.
type candidate struct {
	rank
	raw   string
	entry gitlet.TreeEntry
}

// compareCandidates orders two candidates, best first.
func compareCandidates(a, b candidate) int {
	return cmp.Or(cmp.Compare(a.tier, b.tier), cmp.Compare(a.gaps, b.gaps), cmp.Compare(a.start, b.start),
		cmp.Compare(a.excess, b.excess), cmp.Compare(a.length, b.length), strings.Compare(a.raw, b.raw))
}

// worstFirst is a heap of candidates whose top is the worst: the one a better
// match replaces once the heap holds the limit.
type worstFirst []candidate

func (h worstFirst) Len() int           { return len(h) }
func (h worstFirst) Less(i, j int) bool { return compareCandidates(h[i], h[j]) > 0 }
func (h worstFirst) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *worstFirst) Push(x any)        { *h = append(*h, x.(candidate)) }
func (h *worstFirst) Pop() any {
	old := *h
	c := old[len(old)-1]
	*h = old[:len(old)-1]
	return c
}

// finder ranks the files a find's listing passes, keeping the best limit in
// a bounded heap and counting every match.
type finder struct {
	query []rune // folded
	limit int
	heap  worstFirst
	total int
	files int
	// pos is the positions of the path in hand, reused path to path.
	pos []rune
}

// newFinder is a finder of query's characters keeping the best limit.
func newFinder(query string, limit int) *finder {
	f := &finder{limit: limit}
	for _, c := range query {
		f.query = append(f.query, fold(c))
	}
	return f
}

// entry takes one listing entry: a blob (not a 160000 submodule commit) whose
// path is at most gitlet.MaxGitPathBytes is a file; anything else is passed,
// since no read names a longer path back (gitlet.GitPath).
func (f *finder) entry(e gitlet.TreeEntry) {
	if e.Type != "blob" {
		return
	}
	raw := rawPath(e)
	if len(raw) > gitlet.MaxGitPathBytes {
		return
	}
	f.files++
	var base int
	f.pos, base = positions(f.pos, raw)
	r, ok := f.rank(f.pos, base)
	if !ok {
		return
	}
	r.length = len(raw)
	f.total++
	c := candidate{rank: r, raw: raw, entry: e}
	if len(f.heap) == f.limit {
		if compareCandidates(c, f.heap[0]) >= 0 {
			return
		}
		heap.Pop(&f.heap)
	}
	// CLONED: a memo page is one string its entries slice, which a kept slice
	// would hold whole.
	c.raw = strings.Clone(c.raw)
	c.entry.Path, c.entry.PathB64 = strings.Clone(e.Path), strings.Clone(e.PathB64)
	c.entry.OID, c.entry.Mode = strings.Clone(e.OID), strings.Clone(e.Mode)
	heap.Push(&f.heap, c)
}

// rank is where the query matches the path whose positions are pos, its
// basename beginning at base: in the basename (tier 0), else in the path
// (tier 1), else nowhere.
func (f *finder) rank(pos []rune, base int) (rank, bool) {
	if s, e, ok := window(pos[base:], f.query); ok {
		return rank{tier: 0, gaps: e - s + 1 - len(f.query), start: s, excess: len(pos) - base - len(f.query)}, true
	}
	if s, e, ok := window(pos, f.query); ok {
		return rank{tier: 1, gaps: e - s + 1 - len(f.query), start: s, excess: len(pos) - len(f.query)}, true
	}
	return rank{}, false
}

// best is the kept candidates, best first.
func (f *finder) best() []candidate {
	return slices.SortedFunc(slices.Values(f.heap), compareCandidates)
}

// match is c as the answer carries it: its entry, and the runs of its path's
// positions the query matched, as [start, end) positions of the whole path.
func (f *finder) match(c candidate) gitlet.FindMatch {
	pos, base := positions(nil, c.raw)
	t, off := pos, 0
	if c.tier == 0 {
		t, off = pos[base:], base
	}
	_, e, _ := window(t, f.query)
	// THE BACKWARD PASS'S POSITIONS, as window chose the start by: from the
	// window's end, each query rune at its last position, so the runs are
	// read last first and turned around.
	var spans [][2]int
	j := len(f.query) - 1
	for i := e; j >= 0; i-- {
		if t[i] != f.query[j] {
			continue
		}
		if n := len(spans); n > 0 && spans[n-1][0] == off+i+1 {
			spans[n-1][0]--
		} else {
			spans = append(spans, [2]int{off + i, off + i + 1})
		}
		j--
	}
	slices.Reverse(spans)
	return gitlet.FindMatch{Path: c.entry.Path, PathB64: c.entry.PathB64, OID: c.entry.OID, Mode: c.entry.Mode,
		Spans: spans}
}

// rawPath is e's path as its exact bytes: PathB64 decoded where it is set.
func rawPath(e gitlet.TreeEntry) string {
	if e.PathB64 == "" {
		return e.Path
	}
	raw, err := base64.StdEncoding.DecodeString(e.PathB64)
	if err != nil {
		// The listing's own encoding (gitlet.WirePath) always decodes; if
		// not, the carried text is all there is.
		return e.Path
	}
	return string(raw)
}
