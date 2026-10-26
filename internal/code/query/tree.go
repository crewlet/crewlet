package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A LISTING is `git ls-tree -z -l [-r] <tree id>` — the tree by its id, never
// by a path, which step 5 already resolved to that id — read record by record
// in git's own order, every entry costed into the page as its encoding and the
// rest only counted, so Total is the whole listing's on every page. Its cursor
// is an entry's index, sound on every node: the listing of an immutable tree is
// one list everywhere.

// treeRead answers a tree read.
func (r *read) treeRead(ctx context.Context, objs found, at cursor) (code.GitAnswer, error) {
	rev := objs.revs[r.q.Tree.Rev]
	if rev.peeledKind != "commit" && rev.peeledKind != "tree" {
		return code.GitAnswer{}, fmt.Errorf("%w: tree.rev: %s names a %s; a listing names a commit or a tree",
			code.ErrInvalid, r.q.Tree.Rev, rev.peeledKind)
	}
	if objs.target.kind != "tree" {
		return code.GitAnswer{}, r.noDirectory(objs, rev)
	}
	return r.list(ctx, rev.peeled, objs.path, objs.target.id, r.q.Tree.Recursive, at)
}

// noDirectory is a listing's path naming a file or nothing.
func (r *read) noDirectory(objs found, rev object) error {
	what := "nothing"
	if objs.target.kind != "" {
		what = "a " + objs.target.kind + ", not a directory"
	}
	return fmt.Errorf("%w: %q names %s in %s", code.ErrNotATree, objs.path, what, rev.peeled)
}

// list is a page of tree's listing — the directory dir under rev — from the
// entry at.
func (r *read) list(ctx context.Context, rev, dir, tree string, recursive bool, at cursor) (code.GitAnswer, error) {
	path, pathB64 := code.WirePath(dir)
	a := code.GitAnswer{Kind: code.GitTree, Rev: rev,
		Tree: &code.TreeAnswer{Tree: tree, Path: path, PathB64: pathB64, Entries: []code.TreeEntry{}}}
	envelope := a
	envelope.Next = widestCursor('e', 1)
	envelope.Tree = &code.TreeAnswer{Tree: tree, Path: path, PathB64: pathB64, Entries: []code.TreeEntry{},
		Total: widest}
	left := r.room(envelope)

	start := at.a // the first page's cursor is the zero one: entry 0
	total, full := 0, false
	var tooLarge error
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, func(rec []byte, long bool) error {
		i := total
		total++
		if i < start || full {
			return nil
		}
		if long {
			// AN ENTRY PAST A RECORD'S BOUND is past a page too: its path
			// alone is longer than an answer holds. The page ends before it,
			// as before one too wide for the room, so the entries ahead of it
			// are a page whose Next names it — only a read asking from it is
			// refused, where refusing whatever the page held left them
			// unreadable by any page.
			if len(a.Tree.Entries) > 0 {
				full = true
				a.Next = formatCursor(cursor{letter: 'e', a: i})
				return nil
			}
			tooLarge = fmt.Errorf("%w: entry %d of the listing of %q is longer than one answer holds: "+
				"read the directory above it", code.ErrSliceTooLarge, i, dir)
			return errStop
		}
		e, err := parseEntry(rec, dir)
		if err != nil {
			return err
		}
		cost := jsonLen(e)
		if len(a.Tree.Entries) > 0 {
			cost++ // the comma before it
		}
		switch {
		case cost <= left:
			a.Tree.Entries, left = append(a.Tree.Entries, e), left-cost
		case len(a.Tree.Entries) == 0:
			tooLarge = fmt.Errorf("%w: entry %d of the listing of %q encodes to %d bytes, past the %d one "+
				"answer holds: read the directory above it", code.ErrSliceTooLarge, i, dir, cost, left)
			return errStop
		default:
			full = true
			a.Next = formatCursor(cursor{letter: 'e', a: i})
		}
		return nil
	})
	tail := []string{tree}
	if recursive {
		tail = []string{"-r", tree}
	}
	deadline, err := r.budget()
	if err != nil {
		return code.GitAnswer{}, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: append([]string{"ls-tree", "-z", "-l"}, tail...),
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	switch {
	case tooLarge != nil && res.Damage == nil:
		return code.GitAnswer{}, tooLarge
	case err == nil:
		err = recs.close()
	}
	narrow := fmt.Errorf("%w: listing %q did not finish in the read's %s of git: list a directory below it, "+
		"or without recursion", code.ErrSliceTooLarge, dir, r.s.gitBudget)
	if err := r.failed(ctx, "listing of a tree", res, err, narrow); err != nil {
		return code.GitAnswer{}, err
	}
	if start > 0 && start >= total {
		return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, fmt.Sprintf("an entry before the %d the "+
			"listing holds", total))
	}
	a.Tree.Total = total
	return a, nil
}

// errStop ends a run whose output the read has had all it needs of: its
// writer's error, which the pool hands back as gitproc.ErrOutput — the stop
// a scan wants, never git's own failure.
var errStop = errors.New("query: read enough")

// parseEntry reads one `ls-tree -z -l` record — "<mode> <type> <id> <size>\t<path>",
// the size right-aligned and "-" for anything but a blob — its path relative
// to the tree listed, made one from the repository's root.
func parseEntry(rec []byte, dir string) (code.TreeEntry, error) {
	tab := bytes.IndexByte(rec, '\t')
	if tab < 0 {
		return code.TreeEntry{}, fmt.Errorf("query: ls-tree answered %q, which is no entry", truncated(rec))
	}
	fields := strings.Fields(string(rec[:tab]))
	if len(fields) != 4 || !gitref.ValidOID(fields[2]) {
		return code.TreeEntry{}, fmt.Errorf("query: ls-tree answered %q, which is no entry", truncated(rec))
	}
	e := code.TreeEntry{Mode: fields[0], Type: fields[1], OID: fields[2]}
	if fields[3] != "-" {
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return code.TreeEntry{}, fmt.Errorf("query: ls-tree answered %q, whose size is none", truncated(rec))
		}
		e.Size = size
	}
	p := string(rec[tab+1:])
	if dir != "" {
		p = dir + "/" + p
	}
	e.Path, e.PathB64 = code.WirePath(p)
	return e, nil
}

// truncated is rec as an error quotes it: its opening.
func truncated(rec []byte) string {
	const show = 200
	if len(rec) > show {
		return string(rec[:show]) + "…"
	}
	return string(rec)
}
