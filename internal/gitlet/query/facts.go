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
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A revision's facts are a function of two whole commit ids (its head, and its
// target's tip as the asker read it), so every Gitlet node answers them alike
// and a recorder reads them once a revision and target. Up to six processes of
// the read lane, one after another (factsRead): the merge base (none: the
// empty tree, every head file the change's; the head itself: HeadInTarget),
// the touched paths (touched), the trees and lexable blobs' sizes (shapes),
// the patch into a spool and its patch-id (patchID), and the Go classes
// (classes). The base, paths and trees fail the read as any read's git does
// (server.go); the id and the classes are only ever omitted (omission), the
// safe side, since an omitted id equals nothing and a class only relaxes. The
// classes run last so they never take the budget from the id, which carries
// an approval across a rebase.

// MaxPatchIDDiffBytes is the largest cumulative diff whose patch-id a
// revision's facts take: past it the facts answer the id omitted past its
// bound (gitlet.OmittedPastBound), and an approval carries to the next
// revision by identical trees alone (gitlet.ChangeClassNoCodeChange).
//
// 256 MiB reads in under a quarter of gitproc.ReadDeadline (package doc,
// "Measured"), so a node at its share, four times slower, still takes the id,
// and the facts need no deadline of their own: a longer one would outlast
// estate.GitReadAttempt and answer late rather than omit. A spool at the bound
// is a quarter of a gigabyte on the cache's volume for a second or two,
// MaxPatchSpools of them at most.
const MaxPatchIDDiffBytes = 256 << 20

// MaxPatchSpools is how many revisions' patches one Gitlet node spools at once:
// two, the facts reads one home asks at once, since its loop works
// internal/gitlet/home's Concurrency repositories at a time, each turn's facts
// read one after another, and the router asks a repository's home first, so
// a node's facts reads are its own home's
// (engine.TestEveryFactsReadOfAHomeHasASpool holds the two equal). A third,
// from another home's walk, waits as a read waits for its share
// (gitlet.GitShareWait at most), then is answered unready (share). The bound is the node's, not the read's, because
// the volume is: bounded by the share alone, spools would reach 8 GiB on 64
// cores, outside the room the cache is sized against. PatchSpoolAllowance is
// what the engine takes out of that room for them.
const MaxPatchSpools = 2

// PatchSpoolAllowance is the most every revision's patch spooled on a Gitlet
// node's cache volume at once holds: MaxPatchSpools of MaxPatchIDDiffBytes,
// 512 MiB.
const PatchSpoolAllowance = MaxPatchSpools * MaxPatchIDDiffBytes

// emptyTree is git's id of the tree with nothing in it, which every git holds
// without an object: the base a change whose history shares none with its
// target's is measured from.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// errPatchPastBound ends a patch's run at MaxPatchIDDiffBytes: its spool's
// writer's error, which the pool hands back as gitproc.ErrOutput.
var errPatchPastBound = errors.New("query: the patch is past the bound on a diff whose patch-id is taken")

// factsRead answers a revision's facts.
func (r *read) factsRead(ctx context.Context, objs found) (gitlet.GitAnswer, error) {
	q := r.q.RevisionFacts
	head, target := objs.revs[q.Head], objs.revs[q.Target]
	for _, side := range []struct {
		field, rev string
		o          object
	}{{"revision_facts.head", q.Head, head}, {"revision_facts.target", q.Target, target}} {
		if side.o.peeledKind != "commit" {
			return gitlet.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; a revision's facts are of two commits",
				gitlet.ErrInvalid, side.field, side.rev, side.o.peeledKind)
		}
	}
	mb, err := r.mergeBase(ctx, target.peeled, head.peeled, fmt.Errorf("%w: the merge base of %s and %s did "+
		"not finish in the read's %s of git: a copy walks a long history by date until its maintenance writes "+
		"its commit-graph, and answers the same facts once it has", gitlet.ErrSliceTooLarge, target.peeled,
		head.peeled, r.s.gitBudget))
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	f := &gitlet.RevisionFactsAnswer{Head: head.peeled, Target: target.peeled, MergeBase: mb, Touched: []string{},
		HeadInTarget: mb == head.peeled}
	a := gitlet.GitAnswer{Kind: gitlet.GitRevisionFacts, RevisionFacts: f}
	if f.HeadInTarget {
		return a, nil
	}
	base := mb
	if base == "" {
		base = emptyTree
	}
	// The room the list has: the answer with every other field at its widest
	// (every id and flag set, every place of a bounded list classed), plus
	// the list's own brackets, which the envelope counts already.
	envelope := gitlet.GitAnswer{Kind: gitlet.GitRevisionFacts, RevisionFacts: &gitlet.RevisionFactsAnswer{
		Head: f.Head, Target: f.Target, MergeBase: mb, BaseTree: f.Head, HeadTree: f.Head, PatchID: f.Head,
		PatchIDOmission: gitlet.OmittedOutOfTime, Touched: []string{}, TouchesAll: true,
		Classes: widestClasses(r.s.touchedPaths), ClassesOmission: gitlet.OmittedOutOfTime}}
	limit := min(r.s.touchedBytes, r.room(envelope)+gitlet.TouchedCost(nil))
	walk, err := r.touched(ctx, base, head.peeled, limit)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	f.Touched, f.TouchesAll = walk.paths, walk.all
	sizes, err := r.shapes(ctx, f, walk.lexed)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	if f.PatchID, f.PatchIDOmission, err = r.patchID(ctx, base, head.peeled); err != nil {
		return gitlet.GitAnswer{}, err
	}
	if f.Classes, f.ClassesOmission, err = r.classes(ctx, walk, sizes); err != nil {
		return gitlet.GitAnswer{}, err
	}
	return a, nil
}

// widestClasses is the classes of a touched list of n paths at their longest
// as JSON: every place named once, split across both lists — the second
// list's name outweighs the one comma the split saves — and the
// classification complete.
func widestClasses(n int) gitlet.RevisionClasses {
	c := gitlet.RevisionClasses{Complete: true}
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

// touched is every path the diff from base to head changes, in git's order,
// from diff-tree's raw records, whose modes, ids and status name the paths a
// lexer classes (lexableEntry) and their two blobs. Renames are off, so a moved
// file is its two paths, as a merge touches them. Past the read's bound on the
// paths or their JSON (limit) it answers all, with no list: a list cut at its
// bound would leave the files past the cut out of every match against it.
func (r *read) touched(ctx context.Context, base, head string, limit int) (touchedWalk, error) {
	w := touchedWalk{paths: []string{}}
	cost := gitlet.TouchedCost(nil)
	var (
		e    rawEntry
		need int
	)
	recs := newRecords(0, gitproc.MaxLine+gitlet.MaxGitPathBytes, func(rec []byte, long bool) error {
		if need == 0 {
			// A record's header (modes, ids, status), then its one path.
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
		// A path the facts' record could not carry touches all too: no UTF-8,
		// which JSON cannot carry exactly, or not nameable, which the revise
		// recording the facts refuses, leaving the change due to its home for
		// ever.
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
		gitlet.ErrSliceTooLarge, head, base, r.s.gitBudget)
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
// the same mode. Every other path has no class (gitlet.RevisionClasses): one
// added or deleted has no second version to compare, one whose mode changed
// changed what no lexer reads — whether it runs — a symbolic link's blob is
// the path it names, and a submodule names no blob at all.
func lexableEntry(e rawEntry, path []byte) bool {
	return e.status == 'M' && e.oldMode == e.newMode && (e.newMode == "100644" || e.newMode == "100755") &&
		bytes.HasSuffix(path, []byte(".go"))
}

// shapes sets f's trees (its head's, and its base's or the empty tree), which
// gitlet.ChangeClassNoCodeChange compares, and answers each lexable path's two
// blobs, the base's then the head's, whose sizes decide whether they are read:
// one check of them all (read.check), whatever the change touches.
func (r *read) shapes(ctx context.Context, f *gitlet.RevisionFactsAnswer, lexed []lexable) ([]object, error) {
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

// classes is the classes of the walk's lexable paths (none, incomplete past a
// bound, where it touches all), each pair of blobs lexed (gitlet.ClassifyGo) as
// one `cat-file --batch` writes them, in the touched list's order. A path with
// a version past the classed blob bound gets no class; the classification is
// cut incomplete at the first pair that would pass the classed bytes in all
// (gitlet.OmittedPastBound) or where the read's budget runs out
// (gitlet.OmittedOutOfTime). A blob the check did not find is this copy's
// damage, which a whole copy would lex, so the read is unready, never classes
// this copy alone leaves out. sizes is the check's answer for each path's two
// blobs, in the walk's order.
func (r *read) classes(ctx context.Context, w touchedWalk, sizes []object) (gitlet.RevisionClasses,
	gitlet.FactsOmission, error) {

	if w.all {
		return gitlet.RevisionClasses{}, gitlet.OmittedPastBound, nil
	}
	omitted := gitlet.FactsOmission("")
	var (
		want  []lexable
		in    bytes.Buffer
		total int64
	)
	for i, l := range w.lexed {
		before, after := sizes[2*i], sizes[2*i+1]
		if before.kind != "blob" || after.kind != "blob" {
			return gitlet.RevisionClasses{}, "", r.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s holds no blob "+
				"%s or %s of %s, which the trees of its copy name; another Gitlet node answers", r.s.o.NodeID, l.old,
				l.new, w.paths[l.at]))
		}
		if before.size > r.s.classedBlob || after.size > r.s.classedBlob {
			continue // NO CLASS, as gitlet.ClassifyGo gives none past its bound
		}
		if total+before.size+after.size > r.s.classedBytes {
			omitted = gitlet.OmittedPastBound
			break
		}
		total += before.size + after.size
		want = append(want, l)
		fmt.Fprintf(&in, "%s\n%s\n", l.old, l.new)
	}
	if len(want) == 0 {
		return gitlet.RevisionClasses{Complete: omitted == ""}, omitted, nil
	}
	deadline, err := r.budget()
	if err != nil {
		// The facts before them spent the read's whole budget.
		//nolint:nilerr // budget's one error is the read's git spent, which the facts answer as an omission.
		return gitlet.RevisionClasses{}, gitlet.OmittedOutOfTime, nil
	}
	lx := &lexer{want: want, blob: r.s.classedBlob}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   []string{"cat-file", "--batch", "--buffer"},
		Config: r.s.o.Cache.ServeConfig(), Stdin: &in, Stdout: lx, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if r.omission(ctx, res, err) != "" {
		// Out of budget: the paths lexed keep their classes, the rest none.
		return lx.classes(false), gitlet.OmittedOutOfTime, nil
	}
	if err == nil {
		err = lx.close()
	}
	if err = r.failed(ctx, "lexing of the Go a change touches", res, err, nil); err != nil {
		return gitlet.RevisionClasses{}, "", err
	}
	return lx.classes(omitted == ""), omitted, nil
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
	switch class, ok := gitlet.ClassifyGo(l.body[0], l.body[1]); {
	case !ok:
	case class == gitlet.ChangeClassWhitespaceOnly:
		l.whitespace = append(l.whitespace, at)
	case class == gitlet.ChangeClassCommentOnly:
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
func (l *lexer) classes(complete bool) gitlet.RevisionClasses {
	return gitlet.RevisionClasses{WhitespaceOnly: l.whitespace, CommentOnly: l.comment, Complete: complete}
}

// nameable reports a touched path a read by oid could name back, and so one a
// revision's facts may list (gitlet.GitPath).
func nameable(rec []byte) bool {
	_, err := gitlet.GitPath("touched", string(rec), "", false)
	return err == nil
}

// patchID is `git patch-id --verbatim` of the diff from base to head, which
// leaves line numbers out so a rebase that moved nothing near the change keeps
// its id: "" for a diff of nothing, and omitted past MaxPatchIDDiffBytes or out
// of the read's budget (omission). --verbatim (git 2.39, every node's floor),
// summing file hashes as --stable does, but never --stable, which strips all
// whitespace: indentation, a Makefile's tab, a literal's spaces are changes a
// reviewer approves. --full-index, because patch-id hashes a binary file by
// its index line's blob ids, which git abbreviates by the copy's object count,
// so two nodes would differ. Spooled, never piped: two processes at once would
// hold two read-lane slots, and a lane full of first halves would wait on
// itself; in memory it would be MaxPatchIDDiffBytes a read. A spool has no
// name, so the kernel removes it however the read ends.
func (r *read) patchID(ctx context.Context, base, head string) (string, gitlet.FactsOmission, error) {
	deadline, err := r.budget()
	if err != nil {
		// The base and the paths spent the read's whole budget.
		return "", gitlet.OmittedOutOfTime, nil
	}
	release, err := r.takePatchSpool(ctx, head)
	if err != nil {
		return "", "", err
	}
	defer release()
	spool, err := r.s.o.Cache.Spool()
	if err != nil {
		return "", "", r.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s could not make a spool for the "+
			"patch of %s: %v; another Gitlet node answers", r.s.o.NodeID, head, err))
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
	if w.failed != nil && res.Damage == nil {
		r.charge(res)
		return "", "", r.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s could not write the patch of %s to "+
			"its spool: %v; another Gitlet node answers", r.s.o.NodeID, head, w.failed))
	}
	if o := r.omission(ctx, res, err); o != "" {
		return "", o, nil
	}
	if err = r.failed(ctx, "patch of a change", res, err, nil); err != nil {
		return "", "", err
	}
	if w.wrote == 0 {
		// A diff of nothing has no id: a revision changing nothing carries
		// nothing.
		return "", "", nil
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		return "", "", r.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s could not read back the spool of "+
			"the patch of %s: %v; another Gitlet node answers", r.s.o.NodeID, head, err))
	}
	if deadline, err = r.budget(); err != nil {
		// The patch spent the read's whole budget.
		return "", gitlet.OmittedOutOfTime, nil
	}
	res, err = r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"patch-id", "--verbatim"}, Config: r.s.o.Cache.ServeConfig(),
		Stdin: spool, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if o := r.omission(ctx, res, err); o != "" {
		return "", o, nil
	}
	if err = r.failed(ctx, "patch-id of a change", res, err, nil); err != nil {
		return "", "", err
	}
	// One line, "<patch-id> <commit id>", the commit id zero: a diff of two
	// trees names no commit.
	fields := strings.Fields(string(res.Stdout))
	if len(fields) != 2 || !gitref.ValidOID(fields[0]) || fields[1] != gitref.ZeroOID {
		return "", "", fmt.Errorf("query: patch-id answered %q for a patch of %d bytes, which is no one patch's "+
			"id", truncated(res.Stdout), w.wrote)
	}
	return fields[0], "", nil
}

// takePatchSpool holds one of the node's MaxPatchSpools for the patch of
// head, waiting at most as a read waits for its share (gitlet.GitShareWait):
// past it, or where the answer falls due first, it is unready (share), so the
// walk asks its next node and the home asks again next pass; the caller's own
// end is the caller's error. release gives it back.
func (r *read) takePatchSpool(ctx context.Context, head string) (release func(), err error) {
	wait := time.NewTimer(r.s.patchSpoolWait)
	defer wait.Stop()
	select {
	case r.s.patchSpools <- struct{}{}:
		return func() { <-r.s.patchSpools }, nil
	case <-ctx.Done():
		if !gitlet.AnswerDue(ctx) {
			return nil, ctx.Err()
		}
	case <-wait.C:
	}
	return nil, r.unready(gitlet.UnreadyShare, fmt.Sprintf("%s spools the patches of %d revisions' facts, all it "+
		"spools at once, and none came free for the patch of %s within %s; another Gitlet node answers meanwhile",
		r.s.o.NodeID, MaxPatchSpools, head, r.s.patchSpoolWait))
}

// omission is what a run of the patch, its id or the classes left out,
// charging the run: gitlet.OmittedPastBound past the patch's bound,
// gitlet.OmittedOutOfTime at the read's budget's deadline, and "" for every
// other end (damage, the answer falling due, the caller's end), which failed
// judges. Out of time is answered apart, its paths and trees whole, but not
// Final (gitlet.RevisionFactsAnswer.Final): a bound measures the two commits, alike
// on every node, but the budget measures one node under load, and recording
// its omission would drop the revision's approvals' carry-forward for good,
// forcing a re-review when the fleet is busiest. Its recorder asks again.
func (r *read) omission(ctx context.Context, res gitproc.Result, err error) gitlet.FactsOmission {
	switch {
	case err == nil, res.Damage != nil, gitlet.AnswerDue(ctx), ctx.Err() != nil:
		return ""
	case errors.Is(err, errPatchPastBound):
		r.charge(res)
		return gitlet.OmittedPastBound
	case errors.Is(err, gitproc.ErrDeadline):
		r.charge(res)
		return gitlet.OmittedOutOfTime
	}
	return ""
}

// spooled is the Stdout of a patch's run, written into its spool and refused
// at the first byte past limit (errPatchPastBound), which stops the run.
// failed is a write the spool itself refused (the node's disk), answered as
// the node's failure, never as a patch past its bound.
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
