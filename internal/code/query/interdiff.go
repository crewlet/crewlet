package query

import (
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/code"
)

// AN INTERDIFF is what a change changed between two of its revisions — the
// diff of their two heads — KEPT TO THE FILES EITHER REVISION'S CHANGE
// TOUCHES, as each revision's facts list them (code.GitRevisionFacts): a
// rebase between the two moved the later head onto a newer target, and the
// files the target changed meanwhile, which neither revision touches, differ
// between the heads without being anything the change did. It is a diff read
// in every other respect — its stat a page at a time, one file's patch, its
// pages memoized as a diff's — and answers Kind interdiff, its Diff part.
//
// THE PATHS ARE JUDGED HERE, never handed to git: the heads' raw records,
// exactly as a diff read walks them, are filtered by the SET of touched paths
// (pathFilter's exact half), and the records it admits alone reach diff-pairs
// on its standard input. So a path is a file's whole name — `a*` is the file
// `a*` and never `ab`, as a pathspec without its `:(literal)` magic would read
// it — and no path reaches git's command line, which a pathspec's would (git's
// diffs take none from a file or an input: no --pathspec-from-file), at up
// to two thousand arguments and a hundred kilobytes a read.
//
// NO FILTER WHERE NONE COULD KEEP ANYTHING OUT: where either revision touches
// all — its list was past its bound, so every file is its own — and where the
// two were measured from ONE BASE, since a file neither touches holds the
// base's content in both heads, so the heads differ only in files one of them
// touches. The read is then the heads' plain diff, one diff-tree writing its
// raw records and its counts, as a stat naming no paths is.

// interdiffRead answers an interdiff: a page of its stat, or of one file's
// patch — and, where m takes them, every page of either after it.
func (r *read) interdiffRead(ctx context.Context, objs found, at cursor, m *memoRun) (code.GitAnswer, error) {
	q := r.q.Interdiff
	from, to := objs.revs[q.FromHead], objs.revs[q.ToHead]
	if err := diffable("interdiff.from_head", q.FromHead, from, "interdiff.to_head", q.ToHead, to); err != nil {
		return code.GitAnswer{}, err
	}
	file, _ := code.GitPath("interdiff.file", q.File, q.FileB64, true)
	return r.diffPages(ctx, diffAsk{kind: code.GitInterdiff, base: from.peeled, head: to.peeled, file: file,
		filter: r.interdiffFilter(), narrowing: "read its files one at a time"}, at, m)
}

// interdiffFilter is the files the read's interdiff keeps to: every file where
// either revision touches all or the two share their base, and otherwise
// exactly the files either revision touches — made once a read, since every
// page's memo key names it.
func (r *read) interdiffFilter() pathFilter {
	if r.filter == nil {
		q, f := r.q.Interdiff, pathFilter{}
		if !q.FromTouchesAll && !q.ToTouchesAll && q.FromBase != q.ToBase {
			f = exactFilter(slices.Concat(q.FromTouched, q.ToTouched))
		}
		r.filter = &f
	}
	return *r.filter
}
