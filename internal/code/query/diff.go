package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A DIFF'S STAT is ONE `git diff-tree -z -r --raw --numstat --no-ext-diff
// --no-textconv [-M] <base> <head>`, both ids whole, and NO PATHSPEC: the
// paths a stat is filtered by are judged here, over git's whole answer — a
// path equal to an entry, or a directory above it — so no path ever reaches
// git's command line. git writes every raw record (modes, ids, status and the
// path, or a rename's two) before every numstat record (the lines added and
// deleted, `-` for a binary file), in the same order; so the raw half decides
// which files the page holds — each costed as its encoding with its counts at
// their widest, so where a page ends never depends on a count it has not read
// yet — and remembers of every other file only whether the filter admits it,
// one flag a file, and the numstat half fills the page's counts and the
// totals. Totals are over every file the filter admits, on every page.
//
// ONE FILE'S PATCH is the same diff-tree's raw half (no counts), read until the
// file, then the file's hunks: `git diff --no-color --no-ext-diff
// --no-textconv -U3 <old blob> <new blob>` where both sides hold a blob, its
// header lines — which name the blobs' ids — replaced by ones naming the paths,
// as git's own patch of the commits would name them. A file one side lacks
// (added or deleted), or a submodule, has no blob pair for git to diff — the
// empty blob is no object of a repository unless something put it there, and
// nothing here may — so its hunk is written here, git's form exactly: the
// blob's lines, each `+` or `-`, under one hunk header counted by a first read
// of it, and `\ No newline at end of file` where git writes one. A page of a
// patch is whole lines from a byte offset while their encoded text fits; a
// line longer than a page is refused naming the narrowing (the file's own
// lines), never cut.
//
// THE PATCH'S HEADER quotes a path as git does where it holds a quote, a
// backslash, a control byte or a byte that is not UTF-8 — C's escapes inside
// double quotes — and leaves other UTF-8 as it is (git's core.quotePath off),
// and its `index` line names both blobs whole.

// diffRead answers a diff read: a page of its stat, or of one file's patch.
func (r *read) diffRead(ctx context.Context, objs found, at cursor) (code.GitAnswer, error) {
	q := r.q.Diff
	base, head := objs.revs[q.Base], objs.revs[q.Head]
	for _, side := range []struct {
		field, rev string
		o          object
	}{{"diff.base", q.Base, base}, {"diff.head", q.Head, head}} {
		if side.o.peeledKind != "commit" && side.o.peeledKind != "tree" {
			return code.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; a diff compares commits or trees",
				code.ErrInvalid, side.field, side.rev, side.o.peeledKind)
		}
	}
	if file, _ := code.GitPath("diff.file", q.File, q.FileB64, true); file != "" {
		return r.patchRead(ctx, base.peeled, head.peeled, file, q.Renames, at)
	}
	a := code.GitAnswer{Kind: code.GitDiff, Diff: &code.DiffAnswer{Base: base.peeled, Head: head.peeled,
		Files: []code.DiffFile{}}}
	envelope := code.GitAnswer{Kind: code.GitDiff, Next: widestCursor('f', 1),
		Diff: &code.DiffAnswer{Base: base.peeled, Head: head.peeled, Files: []code.DiffFile{}, Total: widest,
			Added: widest, Deleted: widest}}
	st, err := r.stat(ctx, base.peeled, head.peeled, q.Paths, q.Renames, at.a, r.room(envelope))
	if err != nil {
		return code.GitAnswer{}, err
	}
	if at.a > 0 && at.a >= st.total {
		return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, fmt.Sprintf("a file before the %d the "+
			"diff changes", st.total))
	}
	a.Diff.Files, a.Diff.Total, a.Diff.Added, a.Diff.Deleted = st.files, st.total, st.added, st.deleted
	a.Next = formatCursor(st.next)
	return a, nil
}

// statPage is a page of a stat, and the totals of the whole of it.
type statPage struct {
	files                 []code.DiffFile
	total, added, deleted int
	next                  cursor
}

// stat is the page of base..head's stat that starts at the start-th file paths
// admits, whose files may take left bytes.
func (r *read) stat(ctx context.Context, base, head string, paths []string, renames bool, start, left int) (statPage, error) {
	p := &statParser{filter: paths, start: start, left: left, slots: map[int]int{}}
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, p.record)
	tail := []string{base, head}
	if renames {
		tail = []string{"-M", base, head}
	}
	deadline, err := r.bound(ctx, gitproc.ReadDeadline)
	if err != nil {
		return statPage{}, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: append([]string{"diff-tree", "-z", "-r", "--raw", "--numstat", "--no-ext-diff",
			"--no-textconv"}, tail...),
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	switch {
	case p.tooLarge != nil && res.Damage == nil:
		return statPage{}, p.tooLarge
	case err == nil:
		err = recs.close()
	}
	if err == nil {
		err = p.end()
	}
	narrow := fmt.Errorf("%w: the diff of %s and %s did not finish in %s: diff commits closer together",
		code.ErrSliceTooLarge, base, head, deadline)
	if err := r.failed(ctx, "diff", res, err, narrow); err != nil {
		return statPage{}, err
	}
	return statPage{files: p.files, total: p.matched, added: p.added, deleted: p.deleted, next: p.next}, nil
}

// statParser reads diff-tree's raw records, then its numstat records.
type statParser struct {
	filter      []string
	start, left int

	// The raw half: the entry being read, and the paths it still needs.
	numstat bool
	entry   rawEntry
	need    int
	// admitted is, for every raw entry, whether the filter admits it;
	// matched how many it admitted; slots each page entry's place in files.
	admitted []bool
	matched  int
	files    []code.DiffFile
	slots    map[int]int
	full     bool
	next     cursor

	// The numstat half: the entry it is at, a rename's counts while its
	// two paths are read, and the totals.
	j              int
	nsNeed         int
	nsAdded, nsDel int
	nsBinary       bool
	added, deleted int

	tooLarge error
}

// rawEntry is one raw record: ":<old mode> <new mode> <old id> <new id> <status>",
// then its path, or a rename's two.
type rawEntry struct {
	oldMode, newMode, oldID, newID string
	status                         byte
	score                          int
	paths                          []string
}

func (p *statParser) record(rec []byte, long bool) error {
	if long {
		return fmt.Errorf("query: diff-tree answered a record of %d bytes or more, past any path", len(rec))
	}
	switch {
	case p.need > 0:
		p.entry.paths = append(p.entry.paths, string(rec))
		if p.need--; p.need == 0 {
			if p.took(); p.tooLarge != nil {
				return errStop
			}
		}
	case p.nsNeed > 0:
		if p.nsNeed--; p.nsNeed == 0 {
			return p.counted()
		}
	case !p.numstat && len(rec) > 0 && rec[0] == ':':
		e, need, err := parseRaw(rec)
		if err != nil {
			return err
		}
		p.entry, p.need = e, need
	default:
		// THE NUMSTAT HALF has begun: "<added>\t<deleted>\t<path>", or a
		// rename's "<added>\t<deleted>\t" before its two paths.
		p.numstat = true
		parts := strings.SplitN(string(rec), "\t", 3)
		if len(parts) != 3 {
			return fmt.Errorf("query: diff-tree answered %q, which is no numstat record", truncated(rec))
		}
		var err error
		p.nsBinary = parts[0] == "-" && parts[1] == "-"
		if !p.nsBinary {
			if p.nsAdded, err = strconv.Atoi(parts[0]); err == nil {
				p.nsDel, err = strconv.Atoi(parts[1])
			}
			if err != nil {
				return fmt.Errorf("query: diff-tree answered %q, whose counts are none", truncated(rec))
			}
		}
		if parts[2] == "" {
			p.nsNeed = 2
			return nil
		}
		return p.counted()
	}
	return nil
}

// parseRaw reads a raw record's header, and how many paths follow it.
func parseRaw(rec []byte) (rawEntry, int, error) {
	f := strings.Fields(string(rec[1:]))
	if len(f) != 5 || !gitref.ValidOID(f[2]) || !gitref.ValidOID(f[3]) || f[4] == "" {
		return rawEntry{}, 0, fmt.Errorf("query: diff-tree answered %q, which is no raw record", truncated(rec))
	}
	e := rawEntry{oldMode: f[0], newMode: f[1], oldID: f[2], newID: f[3], status: f[4][0]}
	if len(f[4]) > 1 {
		score, err := strconv.Atoi(f[4][1:])
		if err != nil {
			return rawEntry{}, 0, fmt.Errorf("query: diff-tree answered %q, whose score is none", truncated(rec))
		}
		e.score = score
	}
	if e.status == 'R' || e.status == 'C' {
		return e, 2, nil
	}
	return e, 1, nil
}

// file is the entry as the answer carries it.
func (e rawEntry) file() code.DiffFile {
	f := code.DiffFile{Status: string(e.status)}
	newPath := e.paths[len(e.paths)-1]
	f.Path, f.PathB64 = code.WirePath(newPath)
	if len(e.paths) == 2 {
		f.OldPath, f.OldPathB64 = code.WirePath(e.paths[0])
		f.Similarity = e.score
	}
	if e.oldMode != "000000" {
		f.OldMode = e.oldMode
	}
	if e.newMode != "000000" {
		f.NewMode = e.newMode
	}
	if e.oldID != gitref.ZeroOID {
		f.OldOID = e.oldID
	}
	if e.newID != gitref.ZeroOID {
		f.NewOID = e.newID
	}
	return f
}

// admits reports whether the filter admits a file at any of paths: one equal
// to an entry of it, or under one as a directory.
func admits(filter, paths []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		for _, p := range paths {
			if p == f || strings.HasPrefix(p, f+"/") {
				return true
			}
		}
	}
	return false
}

// took places a raw entry whose paths are read: on the page where the filter
// admits it and the page has room — costed with its counts and binary flag at
// their widest, which the numstat half has not given yet.
func (p *statParser) took() {
	ok := admits(p.filter, p.entry.paths)
	p.admitted = append(p.admitted, ok)
	if !ok {
		return
	}
	m := p.matched
	p.matched++
	if m < p.start || p.full {
		return
	}
	f := p.entry.file()
	widestFile := f
	widestFile.Added, widestFile.Deleted, widestFile.Binary = widest, widest, true
	cost := jsonLen(widestFile)
	if len(p.files) > 0 {
		cost++
	}
	switch {
	case cost <= p.left:
		p.slots[len(p.admitted)-1] = len(p.files)
		p.files, p.left = append(p.files, f), p.left-cost
	case len(p.files) == 0 && p.tooLarge == nil:
		p.tooLarge = fmt.Errorf("%w: file %d of the diff encodes to %d bytes, past the %d one answer holds",
			code.ErrSliceTooLarge, m, cost, p.left)
	default:
		p.full = true
		p.next = cursor{letter: 'f', a: m}
	}
}

// counted applies one numstat record to the entry it is of.
func (p *statParser) counted() error {
	j := p.j
	p.j++
	if j >= len(p.admitted) {
		return fmt.Errorf("query: diff-tree answered more numstat records than raw ones (%d)", len(p.admitted))
	}
	if !p.admitted[j] {
		return nil
	}
	if !p.nsBinary {
		p.added += p.nsAdded
		p.deleted += p.nsDel
	}
	if slot, ok := p.slots[j]; ok {
		f := &p.files[slot]
		f.Added, f.Deleted, f.Binary = p.nsAdded, p.nsDel, p.nsBinary
		if p.nsBinary {
			f.Added, f.Deleted = 0, 0
		}
	}
	return nil
}

// end checks the halves agree.
func (p *statParser) end() error {
	if p.need > 0 || p.nsNeed > 0 || p.j != len(p.admitted) {
		return fmt.Errorf("query: diff-tree answered %d raw records and %d numstat ones", len(p.admitted), p.j)
	}
	return nil
}

// patchRead answers one file's patch: base..head's change to file, a page from
// the byte at.
func (r *read) patchRead(ctx context.Context, base, head, file string, renames bool, at cursor) (code.GitAnswer, error) {
	a := code.GitAnswer{Kind: code.GitDiff, Diff: &code.DiffAnswer{Base: base, Head: head, Files: []code.DiffFile{}}}
	e, ok, err := r.findFile(ctx, base, head, file, renames)
	if err != nil {
		return code.GitAnswer{}, err
	}
	if !ok {
		// A FILE THE DIFF DOES NOT CHANGE has no patch, as git prints none
		// for it: an answer with no file, which a stat of the same diff
		// confirms.
		if at.letter != 0 {
			return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, "a byte of a patch")
		}
		return a, nil
	}
	f := e.file()
	envelope := code.GitAnswer{Kind: code.GitDiff, Next: widestCursor('p', 1),
		Diff: &code.DiffAnswer{Base: base, Head: head, Files: []code.DiffFile{func() code.DiffFile {
			w := f
			w.Added, w.Deleted, w.Binary = widest, widest, true
			return w
		}()}, Total: widest, Added: widest, Deleted: widest, Patch: "x"}}
	pg := &patchPage{from: at.a, left: r.room(envelope) + 1}
	if err := r.writePatch(ctx, e, pg); err != nil {
		return code.GitAnswer{}, err
	}
	pg.finish()
	switch {
	case pg.tooLong != nil:
		return code.GitAnswer{}, pg.tooLong
	case at.a > 0 && !pg.sawFrom:
		return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, fmt.Sprintf("the start of a line of the "+
			"patch's %d bytes", pg.off))
	}
	f.Binary = pg.binary
	if !f.Binary {
		f.Added, f.Deleted = pg.added, pg.deleted
	}
	a.Diff.Files, a.Diff.Total, a.Diff.Added, a.Diff.Deleted = []code.DiffFile{f}, 1, f.Added, f.Deleted
	a.Diff.Patch, a.Next = string(pg.text), formatCursor(pg.next)
	return a, nil
}

// findFile is base..head's raw record of file — its path at head, or at base
// for a file head deleted — read until it, with false where the diff does not
// change it.
func (r *read) findFile(ctx context.Context, base, head, file string, renames bool) (rawEntry, bool, error) {
	var (
		e    rawEntry
		need int
		hit  bool
	)
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, func(rec []byte, long bool) error {
		if long {
			return fmt.Errorf("query: diff-tree answered a record of %d bytes or more, past any path", len(rec))
		}
		if need > 0 {
			e.paths = append(e.paths, string(rec))
			if need--; need == 0 && e.paths[len(e.paths)-1] == file {
				hit = true
				return errStop
			}
			return nil
		}
		var err error
		e, need, err = parseRaw(rec)
		return err
	})
	tail := []string{base, head}
	if renames {
		tail = []string{"-M", base, head}
	}
	deadline, err := r.bound(ctx, gitproc.ReadDeadline)
	if err != nil {
		return rawEntry{}, false, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: append([]string{"diff-tree", "-z", "-r", "--raw", "--no-ext-diff",
			"--no-textconv"}, tail...),
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	switch {
	case hit && res.Damage == nil:
		return e, true, nil
	case err == nil:
		err = recs.close()
	}
	narrow := fmt.Errorf("%w: the diff of %s and %s did not finish in %s: diff commits closer together",
		code.ErrSliceTooLarge, base, head, deadline)
	if err := r.failed(ctx, "diff", res, err, narrow); err != nil {
		return rawEntry{}, false, err
	}
	return rawEntry{}, false, nil
}

// writePatch writes e's patch to pg: its header, then its hunks.
func (r *read) writePatch(ctx context.Context, e rawEntry, pg *patchPage) error {
	f := e.file()
	oldPath, newPath := e.paths[0], e.paths[len(e.paths)-1]
	var h strings.Builder
	fmt.Fprintf(&h, "diff --git %s %s\n", quoted("a/", oldPath), quoted("b/", newPath))
	oldID, newID := e.oldID, e.newID
	switch {
	case f.OldMode == "":
		fmt.Fprintf(&h, "new file mode %s\n", f.NewMode)
	case f.NewMode == "":
		fmt.Fprintf(&h, "deleted file mode %s\n", f.OldMode)
	case f.OldMode != f.NewMode:
		fmt.Fprintf(&h, "old mode %s\nnew mode %s\n", f.OldMode, f.NewMode)
	}
	if e.status == 'R' || e.status == 'C' {
		verb := "rename"
		if e.status == 'C' {
			verb = "copy"
		}
		fmt.Fprintf(&h, "similarity index %d%%\n%s from %s\n%s to %s\n", e.score, verb, quoted("", oldPath), verb,
			quoted("", newPath))
	}
	if oldID != newID {
		fmt.Fprintf(&h, "index %s..%s", oldID, newID)
		if f.OldMode == f.NewMode {
			fmt.Fprintf(&h, " %s", f.NewMode)
		}
		h.WriteByte('\n')
	}
	pg.feed([]byte(h.String()))
	if oldID == newID {
		return nil // a rename or a mode alone: no hunk
	}
	from, to := quoted("a/", oldPath), quoted("b/", newPath)
	if f.OldMode == "" {
		from = "/dev/null"
	}
	if f.NewMode == "" {
		to = "/dev/null"
	}
	switch {
	case f.OldMode == "160000" || f.NewMode == "160000":
		writeSubmodule(pg, from, to, f)
		return nil
	case f.OldMode == "" || f.NewMode == "":
		return r.writeWhole(ctx, pg, from, to, f)
	}
	return r.writePair(ctx, pg, from, to, oldID, newID)
}

// writeSubmodule writes a submodule's hunk, git's form: the commits it names,
// one line each side.
func writeSubmodule(pg *patchPage, from, to string, f code.DiffFile) {
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", from, to)
	switch {
	case f.OldOID == "":
		fmt.Fprintf(&b, "@@ -0,0 +1 @@\n+Subproject commit %s\n", f.NewOID)
	case f.NewOID == "":
		fmt.Fprintf(&b, "@@ -1 +0,0 @@\n-Subproject commit %s\n", f.OldOID)
	default:
		fmt.Fprintf(&b, "@@ -1 +1 @@\n-Subproject commit %s\n+Subproject commit %s\n", f.OldOID, f.NewOID)
	}
	pg.feed([]byte(b.String()))
}

// writeWhole writes the hunk of a file one side lacks: its one blob's lines,
// each `+` (added) or `-` (deleted) — a first read counting them for the hunk's
// header, git's form, and a second writing them.
func (r *read) writeWhole(ctx context.Context, pg *patchPage, from, to string, f code.DiffFile) error {
	blob, sign := f.NewOID, byte('+')
	if f.NewMode == "" {
		blob, sign = f.OldOID, '-'
	}
	// A PAGE CUTTER WITH NO WINDOW only counts: its window's line, zero,
	// is no line of any file.
	count := &blobPage{line: 1, done: true}
	if err := r.streamBlob(ctx, blob, count); err != nil {
		return err
	}
	switch {
	case count.binary:
		pg.binary = true
		pg.feed([]byte(fmt.Sprintf("Binary files %s and %s differ\n", from, to)))
		return nil
	case count.bytes == 0:
		return nil // an empty file: git prints no hunk for one
	}
	n := count.lines()
	hunk := fmt.Sprintf("@@ -0,0 +1,%d @@\n", n)
	if n == 1 {
		hunk = "@@ -0,0 +1 @@\n"
	}
	if sign == '-' {
		hunk = fmt.Sprintf("@@ -1,%d +0,0 @@\n", n)
		if n == 1 {
			hunk = "@@ -1 +0,0 @@\n"
		}
	}
	pg.feed([]byte(fmt.Sprintf("--- %s\n+++ %s\n%s", from, to, hunk)))
	lw := &prefixed{sign: sign, to: pg, start: true}
	if err := r.streamBlob(ctx, blob, lw); err != nil {
		return err
	}
	if count.last != '\n' {
		pg.feed([]byte("\n\\ No newline at end of file\n"))
	}
	return nil
}

// streamBlob streams blob into w with one `git cat-file blob`.
func (r *read) streamBlob(ctx context.Context, blob string, w io.Writer) error {
	deadline, err := r.bound(ctx, gitproc.ReadDeadline)
	if err != nil {
		return err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: []string{"cat-file", "blob", blob}, Config: r.s.o.Cache.ServeConfig(),
		Stdout: w, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	if bp, ok := w.(*blobPage); ok && err == nil {
		bp.finish()
	}
	narrow := fmt.Errorf("%w: reading %s did not finish in %s: read its lines instead", code.ErrSliceTooLarge,
		blob, deadline)
	return r.failed(ctx, "read of a file", res, err, narrow)
}

// writePair writes the hunks of a file both sides hold, as git diffs its two
// blobs: git's own header lines — which name the blobs — dropped, its `---`
// and `+++` lines and a binary pair's line written naming the paths.
func (r *read) writePair(ctx context.Context, pg *patchPage, from, to, oldID, newID string) error {
	w := &gitPatch{to: pg, from: from, dest: to}
	deadline, err := r.bound(ctx, gitproc.ReadDeadline)
	if err != nil {
		return err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv",
			"-U3"}, oldID, newID),
		Config: r.s.o.Cache.ServeConfig(), Stdout: w, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err == nil {
		err = w.close()
	}
	narrow := fmt.Errorf("%w: the diff of %s and %s did not finish in %s: read the file's lines instead",
		code.ErrSliceTooLarge, oldID, newID, deadline)
	return r.failed(ctx, "diff of a file", res, err, narrow)
}

// gitPatch passes git's patch of two blobs on to a page: its header lines
// translated — the blobs' names to the paths — and its hunks as they are.
type gitPatch struct {
	to         *patchPage
	from, dest string
	inHunks    bool
	line       []byte
}

// gitHeaderMax bounds one of git's header lines as held: each names two blob
// ids at most.
const gitHeaderMax = 512

func (g *gitPatch) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !g.inHunks {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(g.line)+len(p) > gitHeaderMax {
				return n, fmt.Errorf("query: git's patch began with a line past %d bytes", gitHeaderMax)
			}
			g.line = append(g.line, p...)
			return n, nil
		}
		g.line = append(g.line, p[:i]...)
		p = p[i+1:]
		g.header(g.line)
		g.line = g.line[:0]
	}
	if len(p) > 0 {
		g.to.feed(p)
	}
	return n, nil
}

// header handles one of git's header lines.
func (g *gitPatch) header(line []byte) {
	switch {
	case bytes.HasPrefix(line, []byte("--- ")):
		g.to.feed([]byte("--- " + g.from + "\n"))
	case bytes.HasPrefix(line, []byte("+++ ")):
		g.to.feed([]byte("+++ " + g.dest + "\n"))
	case bytes.HasPrefix(line, []byte("Binary files ")):
		g.to.binary = true
		g.to.feed([]byte("Binary files " + g.from + " and " + g.dest + " differ\n"))
	case bytes.HasPrefix(line, []byte("@@")):
		g.inHunks = true
		g.to.feed(append(line, '\n'))
	}
	// Every other header line — `diff --git`, `index` — names the blobs,
	// and the patch's own header has said it of the paths.
}

// close reports git's patch ending inside a header line.
func (g *gitPatch) close() error {
	if len(g.line) > 0 {
		return errors.New("query: git's patch ended inside a header line")
	}
	return nil
}

// prefixed writes a blob's lines to a page, each opened with sign.
type prefixed struct {
	sign  byte
	to    *patchPage
	start bool
}

func (w *prefixed) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if w.start {
			w.to.feed([]byte{w.sign})
			w.start = false
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.to.feed(p)
			break
		}
		w.to.feed(p[:i+1])
		p, w.start = p[i+1:], true
	}
	return n, nil
}

// patchPage cuts one page of a patch — whole lines from the byte from, while
// their encoded text costs at most left — out of the patch as it is written,
// and counts the lines added and deleted over all of it.
type patchPage struct {
	from, left int
	// off is the patch's bytes so far; hunks whether a hunk has begun, past
	// which a line opening `+` or `-` is one added or deleted (before it,
	// they are the `---` and `+++` lines); sawFrom whether a line began at
	// from, which a cursor must name.
	off     int
	hunks   bool
	sawFrom bool
	binary  bool

	text    []byte
	done    bool
	next    cursor
	tooLong error

	// The line in hand: where it began, whether it is the page's, what of it
	// is held and costed, and whether it has outgrown the page.
	lineAt          int
	atStart         bool
	inPage          bool
	cur             []byte
	curCost, costed int
	over            bool

	added, deleted int
}

// feed takes p, bytes of the patch.
func (pg *patchPage) feed(p []byte) {
	for len(p) > 0 {
		if !pg.atStart && pg.off == pg.lineAt {
			pg.begin(p[0])
		}
		i := bytes.IndexByte(p, '\n')
		seg := p
		if i >= 0 {
			seg = p[:i+1]
		}
		pg.take(seg)
		pg.off += len(seg)
		p = p[len(seg):]
		if i >= 0 {
			pg.settle()
			pg.lineAt, pg.atStart = pg.off, false
		}
	}
}

// begin opens a line whose first byte is c. A line opening `@` is a hunk's
// header — no line of a hunk's body opens with it, every one opening with
// a space, `+`, `-` or `\` — and only past one is `+` or `-` a line counted.
func (pg *patchPage) begin(c byte) {
	pg.atStart = true
	switch {
	case c == '@':
		pg.hunks = true
	case pg.hunks && c == '+':
		pg.added++
	case pg.hunks && c == '-':
		pg.deleted++
	}
	if pg.lineAt == pg.from {
		pg.sawFrom = true
	}
	pg.inPage = !pg.done && pg.lineAt >= pg.from
}

// take adds seg to the line in hand, while it may join the page.
func (pg *patchPage) take(seg []byte) {
	if !pg.inPage || pg.over {
		return
	}
	pg.cur = append(pg.cur, seg...)
	for pg.costed < len(pg.cur) {
		c, n, ok := runeCost(pg.cur[pg.costed:])
		if !ok {
			return
		}
		if pg.curCost+c > pg.left {
			pg.over, pg.cur = true, pg.cur[:0]
			pg.outgrown()
			return
		}
		pg.curCost, pg.costed = pg.curCost+c, pg.costed+n
	}
}

// outgrown ends the page before the line in hand, which cannot join it — or,
// where the page holds nothing, refuses the read: a patch is cut at lines.
func (pg *patchPage) outgrown() {
	pg.done, pg.inPage = true, false
	if len(pg.text) > 0 {
		pg.next = cursor{letter: 'p', a: pg.lineAt}
		return
	}
	if pg.tooLong == nil {
		pg.tooLong = fmt.Errorf("%w: the patch's line at byte %d is longer than one answer holds: read the "+
			"file's lines instead", code.ErrSliceTooLarge, pg.lineAt)
	}
}

// settle ends the line in hand at its newline, or at the patch's end.
func (pg *patchPage) settle() {
	if pg.inPage && !pg.over {
		cost := pg.curCost + textCost(pg.cur[pg.costed:])
		if cost <= pg.left {
			pg.text, pg.left = append(pg.text, pg.cur...), pg.left-cost
		} else {
			pg.over = true
			pg.outgrown()
		}
	}
	pg.cur, pg.curCost, pg.costed, pg.over, pg.inPage = pg.cur[:0], 0, 0, false, false
}

// finish ends the patch.
func (pg *patchPage) finish() {
	if pg.off > pg.lineAt {
		pg.settle()
	}
}

// quoted is prefix and p as git's patch header names a path: as they are, or
// — where p holds a quote, a backslash, a control byte or a byte that is not
// UTF-8 — inside double quotes with C's escapes, as git's quote_c_style
// writes them (with core.quotePath off, so other UTF-8 stays as it is).
func quoted(prefix, p string) string {
	needs := false
	for i := 0; i < len(p); {
		c := p[i]
		if c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
			needs = true
			break
		}
		r, n := utf8.DecodeRuneInString(p[i:])
		if r == utf8.RuneError && n == 1 {
			needs = true
			break
		}
		i += n
	}
	if !needs {
		return prefix + p
	}
	var b strings.Builder
	b.WriteByte('"')
	b.WriteString(prefix)
	for i := 0; i < len(p); {
		c := p[i]
		switch c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
				i++
				continue
			}
			r, n := utf8.DecodeRuneInString(p[i:])
			if r == utf8.RuneError && n == 1 {
				fmt.Fprintf(&b, `\%03o`, c)
				i++
				continue
			}
			b.WriteString(p[i : i+n])
			i += n
			continue
		}
		i++
	}
	b.WriteByte('"')
	return b.String()
}
