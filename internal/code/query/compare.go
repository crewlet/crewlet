package query

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A COMPARISON is three processes over two whole commit ids: `git merge-base
// <base> <head>` (exit 1, no common ancestor, is an answer: histories that
// share nothing), whose answer also says which contains which — base is an
// ancestor of head exactly when it is their merge base, and the reverse — with
// no second process; `git rev-list --left-right --count <base>...<head>`, the
// commits on each side the other lacks; and the first page of the merge base's
// diff to head — the three-dot reading every code host shows. Both walks go by
// generation number where the copy holds a commit-graph, which git reads for
// them unasked and every maintained copy keeps, so a comparison of any two
// commits runs in milliseconds there (BenchmarkCodeRead's comparison of a
// 14 080-commit history's middle with its tip, 13 ms).
//
// THE THREE SHARE THE READ'S ONE BUDGET OF GIT, as every kind's processes do
// (server.go): a comparison is given the run time a listing is, and one whose
// walks and stat run out the whole of it is too large to answer — "compare
// commits closer together". Not a walk of the whole history each
// (gitproc.HistoryWalk, eleven minutes at the largest repository the
// defaults admit): no asker waits that long — a git read's attempt is
// seventeen seconds, on the node that asked as on every other — so that bound
// was one no comparison could reach, and a comparison it let run past its
// asker's attempt was answered nothing but late. A copy that holds no
// commit-graph yet — just filled, before its maintenance — walks by date and
// may run the budget out over a long history; asked again once the copy is
// maintained, the same comparison answers.

// compareRead answers a comparison — one page, memoized whole where m takes
// it.
func (r *read) compareRead(ctx context.Context, objs found, m *memoRun) (code.GitAnswer, error) {
	q := r.q.Compare
	base, head := objs.revs[q.Base], objs.revs[q.Head]
	for _, side := range []struct {
		field, rev string
		o          object
	}{{"compare.base", q.Base, base}, {"compare.head", q.Head, head}} {
		if side.o.peeledKind != "commit" {
			return code.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; a comparison compares commits",
				code.ErrInvalid, side.field, side.rev, side.o.peeledKind)
		}
	}
	mb, err := r.mergeBase(ctx, base.peeled, head.peeled, r.walkTooLong(base.peeled, head.peeled))
	if err != nil {
		return code.GitAnswer{}, err
	}
	if mb != "" {
		// The merge base is a commit the answer names too, and the page's
		// envelope below is measured with its push.
		r.decorate(mb)
	}
	behind, ahead, err := r.sides(ctx, base.peeled, head.peeled)
	if err != nil {
		return code.GitAnswer{}, err
	}
	c := &code.CompareAnswer{Base: base.peeled, Head: head.peeled, MergeBase: mb,
		BaseIsAncestor: mb == base.peeled, HeadIsAncestor: mb == head.peeled, Ahead: ahead, Behind: behind,
		Files: []code.DiffFile{}}
	a := code.GitAnswer{Kind: code.GitCompare, Compare: c}
	envelope := code.GitAnswer{Kind: code.GitCompare,
		Compare: &code.CompareAnswer{Base: base.peeled, Head: head.peeled, MergeBase: mb, BaseIsAncestor: true,
			HeadIsAncestor: true, Ahead: widest, Behind: widest, Files: []code.DiffFile{}, Total: widest,
			Added: widest, Deleted: widest}}
	left := r.room(envelope)
	if mb != "" && mb != head.peeled {
		// What head changed since the merge base, its first page; with no
		// common ancestor, or head one, nothing.
		st, err := r.stat(ctx, mb, head.peeled, pathFilter{}, false, 0, left,
			"diff the merge base and head naming paths, or compare commits closer together", nil)
		if err != nil {
			return code.GitAnswer{}, err
		}
		c.Files, c.Total, c.Added, c.Deleted = st.files, st.total, st.added, st.deleted
		left = st.left
	}
	m.put(cursor{}, a, left)
	m.place(code.GitCompare, same(memoTail{}))
	return a, nil
}

// mergeBase is base's and head's best common ancestor, "" where they have
// none; tooLong is what a walk that runs out the read's budget of git is
// answered, naming the narrowing its kind's asker can make.
func (r *read) mergeBase(ctx context.Context, base, head string, tooLong error) (string, error) {
	deadline, err := r.budget()
	if err != nil {
		return "", err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"merge-base", base, head}, Config: r.s.o.Cache.ServeConfig(),
		Class: gitproc.ClassRead, Deadline: deadline, Answers: []int{1},
	})
	if err := r.failed(ctx, "merge base", res, err, tooLong); err != nil {
		return "", err
	}
	if res.Exit == 1 {
		return "", nil
	}
	mb := strings.TrimSpace(string(res.Stdout))
	if !gitref.ValidOID(mb) {
		return "", fmt.Errorf("query: merge-base answered %q, which is no object id", res.Stdout)
	}
	return mb, nil
}

// sides is how many commits base has that head lacks (behind) and the reverse
// (ahead).
func (r *read) sides(ctx context.Context, base, head string) (behind, ahead int, err error) {
	deadline, err := r.budget()
	if err != nil {
		return 0, 0, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   append([]string{"rev-list", "--left-right", "--count"}, base+"..."+head),
		Config: r.s.o.Cache.ServeConfig(), Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err := r.failed(ctx, "count of the commits on each side", res, err, r.walkTooLong(base, head)); err != nil {
		return 0, 0, err
	}
	left, right, ok := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\t")
	if ok {
		if behind, err = strconv.Atoi(left); err == nil {
			ahead, err = strconv.Atoi(right)
		}
	}
	if !ok || err != nil || behind < 0 || ahead < 0 {
		return 0, 0, fmt.Errorf("query: rev-list answered %q, which is no count of two sides", res.Stdout)
	}
	return behind, ahead, nil
}

// walkTooLong is a comparison whose walk ran out the read's budget of git.
func (r *read) walkTooLong(base, head string) error {
	return fmt.Errorf("%w: comparing %s and %s did not finish in the read's %s of git: compare commits closer "+
		"together", code.ErrSliceTooLarge, base, head, r.s.gitBudget)
}
