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
// by a path, which step 7 already resolved to that id — read record by record
// in git's own order, every entry costed into a page as its encoding, so Total
// is the whole listing's on every page. Its cursor is an entry's index, sound
// on every node: the listing of an immutable tree is one list everywhere.
// Where the memo takes them, the run cuts every page from the asked one to the
// listing's end, each staged as it fills (memo.go); where it does not, the
// entries past the asked page are only counted.

// treeRead answers a tree read.
func (r *read) treeRead(ctx context.Context, objs found, at cursor, m *memoRun) (code.GitAnswer, error) {
	rev := objs.revs[r.q.Tree.Rev]
	if rev.peeledKind != "commit" && rev.peeledKind != "tree" {
		return code.GitAnswer{}, fmt.Errorf("%w: tree.rev: %s names a %s; a listing names a commit or a tree",
			code.ErrInvalid, r.q.Tree.Rev, rev.peeledKind)
	}
	if objs.target.kind != "tree" {
		return code.GitAnswer{}, r.noDirectory(objs, rev)
	}
	return r.list(ctx, rev.peeled, objs.path, objs.target.id, r.q.Tree.Recursive, at, m)
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
// entry at, and, where m takes them, every page after it.
func (r *read) list(ctx context.Context, rev, dir, tree string, recursive bool, at cursor,
	m *memoRun) (code.GitAnswer, error) {

	path, pathB64 := code.WirePath(dir)
	page := func() code.GitAnswer {
		return code.GitAnswer{Kind: code.GitTree, Rev: rev,
			Tree: &code.TreeAnswer{Tree: tree, Path: path, PathB64: pathB64, Entries: []code.TreeEntry{}}}
	}
	envelope := page()
	envelope.Next = widestCursor('e', 1)
	envelope.Tree.Total = widest
	room := r.room(envelope)

	start := at.a // the first page's cursor is the zero one: entry 0
	a := page()   // the page asked for
	var (
		total    int
		tooLarge error
		// cur is the page being cut — the asked one, then each the memo takes
		// — from the entry curAt, its items leaving left of the room; cutting
		// is false once no entry is to be placed on a page any more: the asked
		// one is full and the memo takes none, or an entry past it is wider
		// than a page alone, which a read asking from it is refused.
		cur     = &a
		curAt   = at
		left    = room
		cutting = true
	)
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, func(rec []byte, long bool) error {
		i := total
		total++
		if i < start || !cutting {
			return nil
		}
		asked := cur == &a
		if long {
			// AN ENTRY PAST A RECORD'S BOUND is past a page too: its path
			// alone is longer than an answer holds. The page ends before it,
			// as before one too wide for the room, so the entries ahead of it
			// are a page whose Next names it; only a read asking from it is
			// refused, and the run cuts no page past it.
			if asked && len(a.Tree.Entries) == 0 {
				tooLarge = fmt.Errorf("%w: entry %d of the listing of %q is longer than one answer holds: "+
					"read the directory above it", code.ErrSliceTooLarge, i, dir)
				return errStop
			}
			r.endPage(m, cur, curAt, i, left)
			cutting = false
			return nil
		}
		e, err := parseEntry(rec, dir)
		if err != nil {
			return err
		}
		cost := jsonLen(e)
		if len(cur.Tree.Entries) > 0 {
			cost++ // the comma before it
		}
		switch {
		case cost <= left:
			cur.Tree.Entries, left = append(cur.Tree.Entries, e), left-cost
			return nil
		case len(cur.Tree.Entries) == 0:
			if asked {
				tooLarge = fmt.Errorf("%w: entry %d of the listing of %q encodes to %d bytes, past the %d one "+
					"answer holds: read the directory above it", code.ErrSliceTooLarge, i, dir, cost, left)
				return errStop
			}
			cutting = false // a read asking from it is refused it
			return nil
		}
		// THE PAGE IS FULL before entry i, which begins the next.
		r.endPage(m, cur, curAt, i, left)
		if m == nil {
			cutting = false // the rest of the listing is only counted
			return nil
		}
		next := page()
		cur, curAt, left = &next, cursor{letter: 'e', a: i}, room
		if first := jsonLen(e); first <= left {
			cur.Tree.Entries, left = append(cur.Tree.Entries, e), left-first
		} else {
			cutting = false
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
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   append([]string{"ls-tree", "-z", "-l"}, tail...),
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
	if cutting {
		r.endPage(m, cur, curAt, -1, left) // the listing's last page
	}
	a.Tree.Total = total
	m.place(code.GitTree, same(memoTail{total: total}))
	return a, nil
}

// endPage ends the listing's page p, cut from the entry at with left of its
// room unspent, before the entry next (none past the listing's end, -1), and
// stages it.
func (r *read) endPage(m *memoRun, p *code.GitAnswer, at cursor, next, left int) {
	if next >= 0 {
		p.Next = formatCursor(cursor{letter: 'e', a: next})
	}
	m.put(at, *p, left)
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
