package query

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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
// them unasked, and each is given the read's deadline or a walk of the whole
// history (gitproc.HistoryWalk), whichever is larger, still under the request's
// own.

// compareRead answers a comparison.
func (r *read) compareRead(ctx context.Context, objs found) (code.GitAnswer, error) {
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
	walk := max(gitproc.ReadDeadline, gitproc.HistoryWalk(r.synced.Repo.LiveObjects))
	mb, err := r.mergeBase(ctx, base.peeled, head.peeled, walk)
	if err != nil {
		return code.GitAnswer{}, err
	}
	if mb != "" {
		// The merge base is a commit the answer names too, and the page's
		// envelope below is measured with its push.
		r.decorate(mb)
	}
	behind, ahead, err := r.sides(ctx, base.peeled, head.peeled, walk)
	if err != nil {
		return code.GitAnswer{}, err
	}
	c := &code.CompareAnswer{Base: base.peeled, Head: head.peeled, MergeBase: mb,
		BaseIsAncestor: mb == base.peeled, HeadIsAncestor: mb == head.peeled, Ahead: ahead, Behind: behind,
		Files: []code.DiffFile{}}
	a := code.GitAnswer{Kind: code.GitCompare, Compare: c}
	if mb == "" || mb == head.peeled {
		// No common ancestor, or head is one: nothing head changed since.
		return a, nil
	}
	envelope := code.GitAnswer{Kind: code.GitCompare,
		Compare: &code.CompareAnswer{Base: base.peeled, Head: head.peeled, MergeBase: mb, BaseIsAncestor: true,
			HeadIsAncestor: true, Ahead: widest, Behind: widest, Files: []code.DiffFile{}, Total: widest,
			Added: widest, Deleted: widest}}
	st, err := r.stat(ctx, mb, head.peeled, nil, false, 0, r.room(envelope),
		"diff the merge base and head naming paths, or compare commits closer together")
	if err != nil {
		return code.GitAnswer{}, err
	}
	c.Files, c.Total, c.Added, c.Deleted = st.files, st.total, st.added, st.deleted
	return a, nil
}

// mergeBase is base's and head's best common ancestor, "" where they have
// none.
func (r *read) mergeBase(ctx context.Context, base, head string, walk time.Duration) (string, error) {
	deadline, err := r.bound(ctx, walk)
	if err != nil {
		return "", err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: []string{"merge-base", base, head}, Config: r.s.o.Cache.ServeConfig(),
		Class: gitproc.ClassRead, Deadline: deadline, Answers: []int{1},
	})
	if err := r.failed(ctx, "merge base", res, err, r.walkTooLong(base, head, deadline)); err != nil {
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
func (r *read) sides(ctx context.Context, base, head string, walk time.Duration) (behind, ahead int, err error) {
	deadline, err := r.bound(ctx, walk)
	if err != nil {
		return 0, 0, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: append([]string{"rev-list", "--left-right", "--count"}, base+"..."+head),
		Config: r.s.o.Cache.ServeConfig(), Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err := r.failed(ctx, "count of the commits on each side", res, err,
		r.walkTooLong(base, head, deadline)); err != nil {
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

// walkTooLong is a comparison's walk its deadline ended.
func (r *read) walkTooLong(base, head string, deadline time.Duration) error {
	return fmt.Errorf("%w: comparing %s and %s did not finish in %s: compare commits closer together",
		code.ErrSliceTooLarge, base, head, deadline)
}
