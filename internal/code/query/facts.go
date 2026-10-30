package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A REVISION'S FACTS are what one revision of a change IS against its target,
// a function of two whole commit ids alone — its head, and its target's tip
// as the asker read it — so any git node answers them alike, and whoever
// records them reads them once a revision and target. Up to six processes of
// the read lane, one after another:
//
//  1. THE BASE — `git merge-base <target> <head>`: the change is measured
//     from where its history meets its target's, as a merge of it would be.
//     Where that is the head itself the target holds the change already
//     (HeadInTarget), and there is nothing left to measure. Histories that
//     share no commit have no merge base: the change is measured from the
//     empty tree, which every git knows without holding it — every file of
//     the head is the change's.
//  2. THE TOUCHED PATHS — `git diff-tree -r --raw -z --no-renames <base>
//     <head>`: every file the change's merge would change, read until the
//     list passes code.MaxTouchedPaths, its JSON passes code.MaxTouchedBytes
//     (or the room its answer leaves), or a path is no UTF-8, which JSON
//     cannot carry exactly. There the read stops git and the facts say the
//     revision TOUCHES ALL, with no list: what every consumer reads as every
//     path, where a list cut at its bound would leave the files past the cut
//     out of everything that matches against it. Renames off, so a moved
//     file is its two paths, as the merge would touch them. RAW records,
//     whose modes, ids and status say which paths a lexer classes
//     (lexableEntry) and by which two blobs.
//  3. THE TREES AND THE BLOBS' SIZES — one `git cat-file --batch-check -Z`
//     (read.check) of the head's tree, the base's, and each lexable path's
//     two blobs: two revisions whose base and head trees are the same hold
//     the same code (code.ChangeClassNoCodeChange), and what a blob weighs
//     decides whether it is read at all.
//  4. THE PATCH — `git diff-tree -r -p --full-index --no-renames <base>
//     <head>`, the cumulative diff of the change, every file in git's order,
//     with three lines of context, written into a SPOOL (the cache's: a file
//     with no name on its volume, gone when closed) and bounded there at
//     MaxPatchIDDiffBytes.
//  5. ITS ID — `git patch-id --verbatim`, reading the spool back: the hash of
//     the patch's lines with their line numbers left out, so the same change
//     rebased onto a target that moved nothing near its lines keeps its id.
//  6. ITS CLASSES — one `git cat-file --batch --buffer` writing the lexable
//     paths' blobs, a base's and a head's a path, each pair lexed as it
//     arrives (code.ClassifyGo) and dropped: within code.MaxClassedBlobBytes
//     a version and code.MaxClassedBytes in all, in the touched list's order,
//     so what a classification holds is two versions of one file.
//
// --VERBATIM, NEVER --STABLE: --stable strips all whitespace before it hashes,
// and whitespace is meaningful — Python's and YAML's indentation, a
// Makefile's tab, a string literal's spaces — so a change to it alone must not
// read as the change a reviewer approved. --verbatim hashes the lines as they
// are, and orders files as --stable does (each file's hash summed), where git
// 2.39 and later carry it: every git node's floor.
//
// --FULL-INDEX because patch-id hashes a binary file by the two blob ids its
// `index` line names, and git abbreviates them to however many digits make
// them unique in the repository at hand — which grows with its objects — so
// one change would hash apart on two copies holding different counts of
// objects. Whole ids are the same on every node.
//
// A SPOOL, because the two processes run one after the other in the read
// lane, never nested: piped together they would hold two of its slots at
// once, and a lane filled with first halves would wait on itself for the
// second. A patch held in memory instead would be MaxPatchIDDiffBytes a read,
// and a spool is no file of the cache's: the kernel removes it however the
// read ends.
//
// AN ID IS OPTIONAL, AND ITS OMISSION THE SAFE SIDE: past MaxPatchIDDiffBytes,
// and where the patch or its id ran out what was left of the read's budget of
// git, the facts answer with PatchIDOmitted — an id that equals nothing, so no
// approval carries across it by its diff — rather than fail: the base, the
// touched paths and the trees are what a change cannot do without, and they
// cost little. Those fail the read as any read's git does (server.go): a walk
// that ran its whole budget is too large, a busy or damaged node unready. So
// are THE CLASSES OPTIONAL, for the same reason and on the same side: a class
// only relaxes, so classes cut short by their bound or by what the budget
// left are answered INCOMPLETE — the paths lexed keep theirs, the rest have
// none — and never fail the read. They run last, after the id, which carries
// a reviewer's approval across a rebase: a common change the classes would
// otherwise take the budget from.

// MaxPatchIDDiffBytes is the largest cumulative diff whose patch-id a
// revision's facts take: past it the facts answer PatchIDOmitted.
//
// 256 MIB, MEASURED TO FIT: BenchmarkRevisionFacts read the facts of a change
// whose patch is 249 MiB — 781 files of 256 KiB, every fourth line rewritten,
// a formatter's run — in 2.30 s at p50 and 2.38 s at the slowest of three
// reads (-benchtime 3x, one count, git 2.56.0 on two E-cores of a Core Ultra
// 7 265U), some 114 MB of patch a second, as its 1, 16 and 64 MiB arms (18,
// 170 and 621 ms) are: under a quarter of the one budget a read's own git
// has, gitproc.ReadDeadline, so a node at its share, four times slower, still
// takes the id. So the facts need NO DEADLINE OF THEIR OWN: a longer one would
// outlast the attempt a git read's asker waits (estate.GitReadAttempt, which
// that budget is a term of), answered late rather than omitted. A spool at
// the bound is a quarter of a gigabyte on the cache's volume for the second
// or two its read runs. A change whose diff passes it — a vendored tree, data
// files — carries an approval from one revision to the next by its code alone:
// the same base and head trees (code.ChangeClassNoCodeChange).
const MaxPatchIDDiffBytes = 256 << 20

// emptyTree is git's id of the tree with nothing in it, which every git holds
// without an object: the base a change whose history shares none with its
// target's is measured from.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// errPatchPastBound ends a patch's run at MaxPatchIDDiffBytes: its spool's
// writer's error, which the pool hands back as gitproc.ErrOutput.
var errPatchPastBound = errors.New("query: the patch is past the bound on a diff whose patch-id is taken")

// factsRead answers a revision's facts.
func (r *read) factsRead(ctx context.Context, objs found) (code.GitAnswer, error) {
	q := r.q.RevisionFacts
	head, target := objs.revs[q.Head], objs.revs[q.Target]
	for _, side := range []struct {
		field, rev string
		o          object
	}{{"revision_facts.head", q.Head, head}, {"revision_facts.target", q.Target, target}} {
		if side.o.peeledKind != "commit" {
			return code.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; a revision's facts are of two commits",
				code.ErrInvalid, side.field, side.rev, side.o.peeledKind)
		}
	}
	mb, err := r.mergeBase(ctx, target.peeled, head.peeled, fmt.Errorf("%w: the merge base of %s and %s did "+
		"not finish in the read's %s of git: a copy walks a long history by date until its maintenance writes "+
		"its commit-graph, and answers the same facts once it has", code.ErrSliceTooLarge, target.peeled,
		head.peeled, r.s.gitBudget))
	if err != nil {
		return code.GitAnswer{}, err
	}
	f := &code.RevisionFactsAnswer{Head: head.peeled, Target: target.peeled, MergeBase: mb, Touched: []string{},
		HeadInTarget: mb == head.peeled}
	a := code.GitAnswer{Kind: code.GitRevisionFacts, RevisionFacts: f}
	if f.HeadInTarget {
		return a, nil
	}
	base := mb
	if base == "" {
		base = emptyTree
	}
	// THE ROOM THE LIST HAS: the answer with every other field at its
	// widest — an id where an id goes, every flag set, every place of a list
	// at its bound classed — and the list's own brackets, which the envelope
	// holds already.
	envelope := code.GitAnswer{Kind: code.GitRevisionFacts, RevisionFacts: &code.RevisionFactsAnswer{
		Head: f.Head, Target: f.Target, MergeBase: mb, BaseTree: f.Head, HeadTree: f.Head, PatchID: f.Head,
		PatchIDOmitted: true, Touched: []string{}, TouchesAll: true, Classes: widestClasses(r.s.touchedPaths)}}
	limit := min(r.s.touchedBytes, r.room(envelope)+code.TouchedCost(nil))
	walk, err := r.touched(ctx, base, head.peeled, limit)
	if err != nil {
		return code.GitAnswer{}, err
	}
	f.Touched, f.TouchesAll = walk.paths, walk.all
	sizes, err := r.shapes(ctx, f, walk.lexed)
	if err != nil {
		return code.GitAnswer{}, err
	}
	if f.PatchID, f.PatchIDOmitted, err = r.patchID(ctx, base, head.peeled); err != nil {
		return code.GitAnswer{}, err
	}
	if f.Classes, err = r.classes(ctx, walk, sizes); err != nil {
		return code.GitAnswer{}, err
	}
	return a, nil
}

// widestClasses is the classes of a touched list of n paths at their longest
// as JSON: every place named once, split across both lists — the second
// list's name outweighs the one comma the split saves — and the
// classification complete.
func widestClasses(n int) code.RevisionClasses {
	c := code.RevisionClasses{Complete: true}
	for at := range n {
		if at == n-1 && n > 1 {
			c.CommentOnly = append(c.CommentOnly, at)
			break
		}
		c.WhitespaceOnly = append(c.WhitespaceOnly, at)
	}
	return c
}

// touchedWalk is what the walk of a change's paths found: the paths it
// touches in git's order — none where it touches all — and, of them, each a
// lexer classes.
type touchedWalk struct {
	paths []string
	all   bool
	lexed []lexable
}

// lexable is one touched path a lexer classes: its place in the touched list,
// and its blob at the base and at the head.
type lexable struct {
	at       int
	old, new string
}

// touched is every path the diff from base to head changes, in git's order —
// or all, with no list, once the list would pass the read's bound on its paths
// or on its JSON (limit), or meets a path that is no UTF-8 — read from
// diff-tree's RAW records, whose modes, ids and status name the paths a lexer
// classes (lexableEntry) with their two blobs.
func (r *read) touched(ctx context.Context, base, head string, limit int) (touchedWalk, error) {
	w := touchedWalk{paths: []string{}}
	cost := code.TouchedCost(nil)
	var (
		e    rawEntry
		need int
	)
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, func(rec []byte, long bool) error {
		if need == 0 {
			// A RECORD'S HEADER — its modes, ids and status — and then its
			// path: one, with renames off.
			if long {
				return fmt.Errorf("query: diff-tree answered a record of %d bytes or more, past any header",
					len(rec))
			}
			var err error
			e, need, err = parseRaw(rec)
			return err
		}
		need--
		c := textCost(rec) + 2 // the path, quoted
		if len(w.paths) > 0 {
			c++ // the comma before it
		}
		// A PATH THE FACTS' RECORD COULD NOT CARRY TOUCHES ALL, as one past
		// the list's bounds does: bytes that are not UTF-8, which JSON cannot
		// carry exactly, and one no read by oid could name back (code.GitPath:
		// past code.MaxGitPathBytes, or not clean), which the revise that
		// records the facts refuses — a list holding it would leave its change
		// due to its home for ever, its every revise refused.
		if long || !utf8.Valid(rec) || len(w.paths) == r.s.touchedPaths || cost+c > limit ||
			!nameable(rec) {
			w.all = true
			return errStop
		}
		if lexableEntry(e, rec) {
			w.lexed = append(w.lexed, lexable{at: len(w.paths), old: e.oldID, new: e.newID})
		}
		w.paths, cost = append(w.paths, string(rec)), cost+c
		return nil
	})
	deadline, err := r.budget()
	if err != nil {
		return touchedWalk{}, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   append([]string{"diff-tree", "-r", "--raw", "-z", "--no-renames"}, base, head),
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	switch {
	case w.all && res.Damage == nil:
		r.charge(res)
		return touchedWalk{paths: []string{}, all: true}, nil
	case err == nil:
		err = recs.close()
	}
	narrow := fmt.Errorf("%w: the walk of the paths %s changes since %s did not finish in the read's %s of git",
		code.ErrSliceTooLarge, head, base, r.s.gitBudget)
	if err = r.failed(ctx, "walk of the paths a change touches", res, err, narrow); err != nil {
		return touchedWalk{}, err
	}
	if need > 0 {
		return touchedWalk{}, errors.New("query: diff-tree's raw records ended before a record's path")
	}
	return w, nil
}

// lexableEntry reports a raw record whose path a lexer classes: a Go file —
// a path ending in .go, the one suffix the go tool reads as Go source —
// CHANGED IN PLACE, a regular file (100644 or 100755) on both sides and in
// the same mode. Every other path has no class (code.RevisionClasses): one
// added or deleted has no second version to compare, one whose mode changed
// changed what no lexer reads — whether it runs — a symbolic link's blob is
// the path it names, and a submodule names no blob at all.
func lexableEntry(e rawEntry, path []byte) bool {
	return e.status == 'M' && e.oldMode == e.newMode && (e.newMode == "100644" || e.newMode == "100755") &&
		bytes.HasSuffix(path, []byte(".go"))
}

// shapes sets f's trees — its head's and its base's, the empty tree's where
// it has none — and answers each lexable path's two blobs, the base's then
// the head's, as one check of them all finds them (read.check): one short
// process, whatever the change touches.
func (r *read) shapes(ctx context.Context, f *code.RevisionFactsAnswer, lexed []lexable) ([]object, error) {
	in := []string{f.Head + "^{tree}"}
	if f.MergeBase != "" {
		in = append(in, f.MergeBase+"^{tree}")
	}
	trees := len(in)
	for _, l := range lexed {
		in = append(in, l.old, l.new)
	}
	objs, err := r.check(ctx, in)
	if err != nil {
		return nil, err
	}
	for i, o := range objs[:trees] {
		if o.kind != "tree" {
			return nil, fmt.Errorf("query: cat-file answered %q for %s, which is no tree", o.kind, in[i])
		}
	}
	f.HeadTree, f.BaseTree = objs[0].id, emptyTree
	if f.MergeBase != "" {
		f.BaseTree = objs[1].id
	}
	return objs[trees:], nil
}

// classes is the path classes of the paths the walk found a lexer classes —
// none, and incomplete, where it touches all — each pair of blobs lexed
// (code.ClassifyGo) as one `cat-file --batch` writes them: in the touched
// list's order, a path whose version is past the classed blob bound left
// unread with no class, and the classification cut, INCOMPLETE, at the first
// path whose two versions would take what it lexes past the classed bound in
// all, or wherever what the read's budget of git left runs out. A blob the
// check found none of — the copy's damage, which the patch's own git reports —
// is left unread too, the classification incomplete. sizes is the check's
// answer of each path's two blobs, in the walk's order.
func (r *read) classes(ctx context.Context, w touchedWalk, sizes []object) (code.RevisionClasses, error) {
	if w.all {
		return code.RevisionClasses{}, nil
	}
	complete := true
	var (
		want  []lexable
		in    bytes.Buffer
		total int64
	)
	for i, l := range w.lexed {
		before, after := sizes[2*i], sizes[2*i+1]
		if before.kind != "blob" || after.kind != "blob" {
			complete = false
			continue
		}
		if before.size > r.s.classedBlob || after.size > r.s.classedBlob {
			continue // NO CLASS, as code.ClassifyGo gives none past its bound
		}
		if total+before.size+after.size > r.s.classedBytes {
			complete = false
			break
		}
		total += before.size + after.size
		want = append(want, l)
		fmt.Fprintf(&in, "%s\n%s\n", l.old, l.new)
	}
	if len(want) == 0 {
		return code.RevisionClasses{Complete: complete}, nil
	}
	deadline, err := r.budget()
	if err != nil {
		return code.RevisionClasses{}, nil // the facts before them spent the read's whole budget
	}
	lx := &lexer{want: want, blob: r.s.classedBlob}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   []string{"cat-file", "--batch", "--buffer"},
		Config: r.s.o.Cache.ServeConfig(), Stdin: &in, Stdout: lx, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if r.omits(ctx, res, err) {
		// OUT OF THE BUDGET: the paths lexed keep their classes, and the
		// rest have none.
		return lx.classes(false), nil
	}
	if err == nil {
		err = lx.close()
	}
	if err = r.failed(ctx, "lexing of the Go a change touches", res, err, nil); err != nil {
		return code.RevisionClasses{}, err
	}
	return lx.classes(complete), nil
}

// lexer is the Stdout of a classification's cat-file: each blob's
// "<id> blob <size>" header, its bytes and a newline, two blobs a path — the
// base's, then the head's — each pair lexed once both are in, keeping the two
// versions of the one path in hand and no more.
type lexer struct {
	want []lexable
	blob int64 // the most a version may weigh, which the batch was asked within
	i    int   // the path in hand
	side int   // 0: its base's blob, 1: its head's
	// head is the header being read; left the bytes of the blob in hand
	// still to come, and in whether they are coming — then its newline.
	head    []byte
	body    [2][]byte
	left    int64
	in, end bool
	// whitespace and comment are the places the paths lexed so far are
	// classed at.
	whitespace, comment []int
}

// maxBatchHeader bounds the header a blob's bytes follow: an id, a type and a
// size, some sixty bytes — anything past a few times that is no header.
const maxBatchHeader = 256

func (l *lexer) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		switch {
		case l.end:
			if p[0] != '\n' {
				return n, fmt.Errorf("query: cat-file wrote %q after a blob, where its newline goes", p[0])
			}
			p, l.end = p[1:], false
			if l.side == 0 {
				l.side = 1
				continue
			}
			l.lex()
		case l.in:
			take := min(l.left, int64(len(p)))
			l.body[l.side], p, l.left = append(l.body[l.side], p[:take]...), p[take:], l.left-take
			if l.left == 0 {
				l.in, l.end = false, true
			}
		default:
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				l.head = append(l.head, p...)
				if len(l.head) > maxBatchHeader {
					return n, fmt.Errorf("query: cat-file wrote a header of %d bytes or more", len(l.head))
				}
				p = nil
				continue
			}
			l.head, p = append(l.head, p[:i]...), p[i+1:]
			if err := l.begin(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// begin reads the header in hand as the next blob's, the one the batch was
// asked for in its turn, within the bound it was asked within.
func (l *lexer) begin() error {
	if l.i >= len(l.want) {
		return fmt.Errorf("query: cat-file wrote %q past the %d blobs it was asked", truncated(l.head),
			2*len(l.want))
	}
	asked := l.want[l.i].old
	if l.side == 1 {
		asked = l.want[l.i].new
	}
	fields := strings.Fields(string(l.head))
	if len(fields) != 3 || fields[0] != asked || fields[1] != "blob" {
		return fmt.Errorf("query: cat-file answered %q for the blob %s", truncated(l.head), asked)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 || size > l.blob {
		return fmt.Errorf("query: cat-file answered %q for the blob %s, within %d bytes", truncated(l.head), asked,
			l.blob)
	}
	l.head, l.body[l.side], l.left = l.head[:0], slices.Grow(l.body[l.side][:0], int(size)), size
	l.in, l.end = size > 0, size == 0
	return nil
}

// lex classes the path in hand by its two versions, and moves to the next.
func (l *lexer) lex() {
	at := l.want[l.i].at
	switch class, ok := code.ClassifyGo(l.body[0], l.body[1]); {
	case !ok:
	case class == code.ChangeClassWhitespaceOnly:
		l.whitespace = append(l.whitespace, at)
	case class == code.ChangeClassCommentOnly:
		l.comment = append(l.comment, at)
	}
	l.i, l.side = l.i+1, 0
}

// close reports an answer that ended before every blob it was asked for was
// whole — what git's own failure to write them is.
func (l *lexer) close() error {
	if l.i != len(l.want) || l.side != 0 || l.in || l.end || len(l.head) > 0 {
		return fmt.Errorf("query: cat-file's answer ended at its blob %d of %d", 2*l.i+l.side, 2*len(l.want))
	}
	return nil
}

// classes is what the lexer classed, complete or not.
func (l *lexer) classes(complete bool) code.RevisionClasses {
	return code.RevisionClasses{WhitespaceOnly: l.whitespace, CommentOnly: l.comment, Complete: complete}
}

// nameable reports a touched path a read by oid could name back, and so one a
// revision's facts may list (code.GitPath).
func nameable(rec []byte) bool {
	_, err := code.GitPath("touched", string(rec), "", false)
	return err == nil
}

// patchID is `git patch-id --verbatim` of the diff from base to head: "" for
// a diff of nothing, and omitted past the read's bound on the diff, or where
// what was left of its budget of git ran out.
func (r *read) patchID(ctx context.Context, base, head string) (string, bool, error) {
	deadline, err := r.budget()
	if err != nil {
		return "", true, nil // the base and the paths spent the read's whole budget
	}
	spool, err := r.s.o.Cache.Spool()
	if err != nil {
		return "", false, r.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not make a spool for the "+
			"patch of %s: %v; another git node answers", r.s.o.NodeID, head, err))
	}
	defer func() { _ = spool.Close() }()
	w := &spooled{f: spool, limit: r.s.patchBytes}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: append([]string{"diff-tree", "-r", "-p", "--full-index", "--no-renames",
			"--no-color", "--no-ext-diff", "--no-textconv", "-U3"}, base, head),
		Config: append(r.s.o.Cache.ServeConfig(), patchConfig), Stdout: w, Class: gitproc.ClassRead,
		Deadline: deadline,
	})
	switch {
	case w.failed != nil && res.Damage == nil:
		r.charge(res)
		return "", false, r.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not write the patch of %s to "+
			"its spool: %v; another git node answers", r.s.o.NodeID, head, w.failed))
	case r.omits(ctx, res, err):
		return "", true, nil
	}
	if err = r.failed(ctx, "patch of a change", res, err, nil); err != nil {
		return "", false, err
	}
	if w.wrote == 0 {
		// A DIFF OF NOTHING has no patch and no id: a revision that changes
		// nothing carries nothing.
		return "", false, nil
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		return "", false, r.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s could not read back the spool of "+
			"the patch of %s: %v; another git node answers", r.s.o.NodeID, head, err))
	}
	if deadline, err = r.budget(); err != nil {
		return "", true, nil // the patch spent the read's whole budget
	}
	res, err = r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"patch-id", "--verbatim"}, Config: r.s.o.Cache.ServeConfig(),
		Stdin: spool, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if r.omits(ctx, res, err) {
		return "", true, nil
	}
	if err = r.failed(ctx, "patch-id of a change", res, err, nil); err != nil {
		return "", false, err
	}
	// ONE PATCH, ONE LINE: "<patch-id> <commit id>", the commit id git's
	// zero id, since a diff of two trees names no commit.
	fields := strings.Fields(string(res.Stdout))
	if len(fields) != 2 || !gitref.ValidOID(fields[0]) || fields[1] != gitref.ZeroOID {
		return "", false, fmt.Errorf("query: patch-id answered %q for a patch of %d bytes, which is no one patch's "+
			"id", truncated(res.Stdout), w.wrote)
	}
	return fields[0], false, nil
}

// omits reports whether a run of the patch or its id ended where the facts
// omit the id — at the bound on the patch, or at its deadline, what was left
// of the read's budget of git — charging its run. A run that reported damage,
// one the read's answer falling due ended, and one its caller ended are none:
// failed judges those as it judges every run.
func (r *read) omits(ctx context.Context, res gitproc.Result, err error) bool {
	switch {
	case err == nil, res.Damage != nil, code.AnswerDue(ctx), ctx.Err() != nil:
		return false
	case errors.Is(err, errPatchPastBound), errors.Is(err, gitproc.ErrDeadline):
		r.charge(res)
		return true
	}
	return false
}

// spooled is the Stdout of a patch's run: git's patch written into its spool,
// refused at the first byte past limit (errPatchPastBound), which stops the
// run. failed is a write the spool itself refused — the node's disk — which
// the read answers as the node's own failure, never as a patch past its
// bound.
type spooled struct {
	f      *os.File
	limit  int64
	wrote  int64
	failed error
}

func (s *spooled) Write(p []byte) (int, error) {
	if s.wrote+int64(len(p)) > s.limit {
		return 0, errPatchPastBound
	}
	n, err := s.f.Write(p)
	s.wrote += int64(n)
	if err != nil {
		s.failed = err
		return n, fmt.Errorf("query: write a patch to its spool: %w", err)
	}
	return n, nil
}
