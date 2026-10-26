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

// A DIFF'S STAT is two halves: git's RAW RECORDS of base..head — `git
// diff-tree -z -r --raw [-M] <base> <head>`, both ids whole and NO PATHSPEC, a
// walk of the two trees that reads no file but a rename's candidates (-M) —
// and the NUMSTAT of the files the stat admits, the lines each adds and
// deletes, for which git diffs the text of every pair it counts: that half is
// what a stat costs. The paths a stat is filtered by are judged here, over
// git's whole raw answer — a path equal to an entry, or a directory above it,
// on either side of a rename — so no path ever reaches git's command line,
// and then ONLY THE FILES THE FILTER ADMITS ARE COUNTED: their raw records,
// exactly as diff-tree wrote them, go to `git diff-pairs -z --numstat` on its
// standard input (git 2.50's, which every git node's 2.55 floor carries),
// which diffs those pairs and no other. So naming one directory of a diff of
// thousands of files costs git that directory's diffs: measured over this
// repository's 5 866 files changed between two commits (git 2.53, one core),
// the whole raw+numstat took 0.80–0.97 s, the raw records alone 13 ms and
// diff-pairs over internal/code/query's 18 files 5–6 ms. A stat naming no
// paths admits every file, and its counts are the whole diff's numstat, which
// one diff-tree writes after its raw records and in their order (`--raw
// --numstat`): one process, as cheap as a stat of the whole can be.
//
// The raw half decides which files the page holds — each costed as its
// encoding with its counts at their widest, so where a page ends never depends
// on a count not read yet — and the numstat half, its j-th record the j-th
// file admitted, fills the page's counts and the totals, which are over every
// file the filter admits, on every page. A filtered stat holds the records it
// admitted until diff-pairs has read them: about 160 bytes a file at a
// sixty-byte path, 16 MB for a filter admitting every file of a 100 000-file
// diff.
//
// ONE FILE'S PATCH is the same raw records read until the file's, and that
// one record handed to `git diff-pairs -z -p --full-index`: git's own patch of
// the two commits for the one file — its header naming the paths, quoted only
// where a quote, a backslash or a control byte would break the line
// (core.quotePath off, so a UTF-8 path is written as it is, as the stat beside
// it names it); `index` naming both blobs whole, so a patch is the same bytes
// on every node whatever ids its objects share prefixes with, and a byte
// cursor is sound anywhere; a file added or deleted against /dev/null, a
// change of type as the deletion and the creation git prints for it, a
// submodule's commits, and a binary pair's one line, which is how the answer
// knows the pair is binary. One process, which streams nothing twice. A page
// of a patch is whole lines from a byte offset while their encoded text fits;
// a line longer than a page is refused naming the narrowing (the file's own
// lines), never cut.

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
	st, err := r.stat(ctx, base.peeled, head.peeled, q.Paths, q.Renames, at.a, r.room(envelope),
		"name paths, or diff commits closer together")
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
// admits, whose files may take left bytes, and the totals over every file
// paths admits. narrowing is what a stat naming no paths, whose one diff-tree
// the deadline ended, tells its asker to do instead — naming paths among it,
// since a stat that names them counts the files they admit and no other.
func (r *read) stat(ctx context.Context, base, head string, paths []string, renames bool, start, left int,
	narrowing string) (statPage, error) {
	p := &statParser{filter: paths, start: start, left: left, keep: len(paths) > 0}
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, p.record)
	tail := []string{base, head}
	if renames {
		tail = []string{"-M", base, head}
	}
	deadline, err := r.budget()
	if err != nil {
		return statPage{}, err
	}
	var res gitproc.Result
	narrow := fmt.Errorf("%w: the diff of %s and %s did not finish in the read's %s of git: %s",
		code.ErrSliceTooLarge, base, head, r.s.gitBudget, narrowing)
	if p.keep {
		// A FILTERED STAT'S WALK, its raw records alone: the numstat is
		// diff-pairs', of the files the filter admits (count). No path
		// narrows the walk itself, which reads the two trees whatever the
		// filter, so a walk the deadline ended is refused naming the commits.
		narrow = fmt.Errorf("%w: the walk of the diff of %s and %s did not finish in the read's %s of git: "+
			"diff commits closer together", code.ErrSliceTooLarge, base, head, r.s.gitBudget)
		res, err = r.s.o.Git.Run(ctx, gitproc.Cmd{
			Dir: r.synced.Dir, Args: append([]string{"diff-tree", "-z", "-r", "--raw", "--no-ext-diff",
				"--no-textconv"}, tail...),
			Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
		})
	} else {
		res, err = r.s.o.Git.Run(ctx, gitproc.Cmd{
			Dir: r.synced.Dir, Args: append([]string{"diff-tree", "-z", "-r", "--raw", "--numstat", "--no-ext-diff",
				"--no-textconv"}, tail...),
			Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
		})
	}
	switch {
	case p.tooLarge != nil && res.Damage == nil:
		return statPage{}, p.tooLarge
	case err == nil:
		err = recs.close()
	}
	if err == nil {
		// Where the stat names no paths, the one process wrote both halves.
		err = p.end(!p.keep)
	}
	if err := r.failed(ctx, "diff", res, err, narrow); err != nil {
		return statPage{}, err
	}
	if p.keep {
		if err := r.count(ctx, p, base, head); err != nil {
			return statPage{}, err
		}
	}
	// A stat of no file — paths that admit nothing it changed, two trees
	// alike — is a page of none, never a null: every answer's list is a list.
	files := p.files
	if files == nil {
		files = []code.DiffFile{}
	}
	return statPage{files: files, total: p.matched, added: p.added, deleted: p.deleted, next: p.next}, nil
}

// count is a filtered stat's numstat half: the lines of every file p
// admitted, counted by ONE diff-pairs over their raw records — and nothing
// counted where p admitted none, or where the page's cursor is past them all,
// which the read refuses without its totals.
func (r *read) count(ctx context.Context, p *statParser, base, head string) error {
	if p.matched == 0 || (p.start > 0 && p.start >= p.matched) {
		return nil
	}
	deadline, err := r.budget()
	if err != nil {
		return err
	}
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, p.record)
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: []string{"diff-pairs", "-z", "--numstat", "--no-ext-diff", "--no-textconv"},
		Config: r.s.o.Cache.ServeConfig(), Stdin: &p.pairs, Stdout: recs, Class: gitproc.ClassRead,
		Deadline: deadline,
	})
	if err == nil {
		err = recs.close()
	}
	if err == nil {
		err = p.end(true)
	}
	narrow := fmt.Errorf("%w: counting the lines of the %d files the paths admit in the diff of %s and %s did "+
		"not finish in the read's %s of git: name fewer paths, or diff commits closer together",
		code.ErrSliceTooLarge, p.matched, base, head, r.s.gitBudget)
	return r.failed(ctx, "count of a diff's lines", res, err, narrow)
}

// statParser reads diff-tree's raw records, then the numstat records of the
// files they admit — diff-tree's own after them, or diff-pairs'.
type statParser struct {
	filter      []string
	start, left int
	// keep is whether the admitted records are kept for diff-pairs (pairs):
	// a filtered stat's, whose numstat is of them alone.
	keep  bool
	pairs bytes.Buffer

	// The raw half: the entry being read and the paths it still needs; how
	// many entries the filter admitted, and the page's — the admitted from
	// start on, while they fit.
	entry    rawEntry
	need     int
	matched  int
	files    []code.DiffFile
	full     bool
	next     cursor
	tooLarge error

	// The numstat half: whether it has begun, the record it is at, a
	// rename's counts while its two paths are read, and the totals.
	numstat        bool
	j              int
	nsNeed         int
	nsAdded, nsDel int
	nsBinary       bool
	added, deleted int
}

// rawEntry is one raw record: ":<old mode> <new mode> <old id> <new id> <status>",
// then its path, or a rename's two.
type rawEntry struct {
	// head is the record's header as git wrote it, which diff-pairs reads
	// back: its status spelled git's way (a rename's score zero-padded).
	head                           string
	oldMode, newMode, oldID, newID string
	status                         byte
	score                          int
	paths                          []string
}

func (p *statParser) record(rec []byte, long bool) error {
	if long {
		return fmt.Errorf("query: git answered a diff's record of %d bytes or more, past any path", len(rec))
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
			return fmt.Errorf("query: git answered %q, which is no numstat record", truncated(rec))
		}
		var err error
		p.nsBinary = parts[0] == "-" && parts[1] == "-"
		if !p.nsBinary {
			if p.nsAdded, err = strconv.Atoi(parts[0]); err == nil {
				p.nsDel, err = strconv.Atoi(parts[1])
			}
			if err != nil {
				return fmt.Errorf("query: git answered %q, whose counts are none", truncated(rec))
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
	e := rawEntry{head: string(rec), oldMode: f[0], newMode: f[1], oldID: f[2], newID: f[3], status: f[4][0]}
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

// raw is the record as diff-tree wrote it under -z — its header, then each
// path, every one ended by a NUL — which is what diff-pairs reads.
func (e rawEntry) raw() []byte {
	n := len(e.head) + 1
	for _, p := range e.paths {
		n += len(p) + 1
	}
	b := make([]byte, 0, n)
	b = append(append(b, e.head...), 0)
	for _, p := range e.paths {
		b = append(append(b, p...), 0)
	}
	return b
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

// took places a raw entry whose paths are read, where the filter admits it:
// kept for diff-pairs on a filtered stat, and on the page where the page has
// room — costed with its counts and binary flag at their widest, which the
// numstat half has not given yet.
func (p *statParser) took() {
	if !admits(p.filter, p.entry.paths) {
		return
	}
	m := p.matched
	p.matched++
	if p.keep {
		p.pairs.Write(p.entry.raw())
	}
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
		p.files, p.left = append(p.files, f), p.left-cost
	case len(p.files) == 0 && p.tooLarge == nil:
		p.tooLarge = fmt.Errorf("%w: file %d of the diff encodes to %d bytes, past the %d one answer holds",
			code.ErrSliceTooLarge, m, cost, p.left)
	default:
		p.full = true
		p.next = cursor{letter: 'f', a: m}
	}
}

// counted applies one numstat record to the admitted file it is of: the j-th
// record the j-th file admitted — on a stat naming no paths every file, and on
// a filtered one every file diff-pairs was handed, in the order handed.
func (p *statParser) counted() error {
	j := p.j
	p.j++
	if j >= p.matched {
		return fmt.Errorf("query: git answered more numstat records than the %d files the diff admits", p.matched)
	}
	if !p.nsBinary {
		p.added += p.nsAdded
		p.deleted += p.nsDel
	}
	if slot := j - p.start; slot >= 0 && slot < len(p.files) {
		f := &p.files[slot]
		f.Added, f.Deleted, f.Binary = p.nsAdded, p.nsDel, p.nsBinary
		if p.nsBinary {
			f.Added, f.Deleted = 0, 0
		}
	}
	return nil
}

// end checks git's records ended where a record does — no raw record still
// waiting on its paths — and, once the numstat half is read (counted), that
// it counted every admitted file, once.
func (p *statParser) end(counted bool) error {
	switch {
	case p.need > 0:
		return errors.New("query: git's raw records ended before the paths of the last of them")
	case counted && (p.nsNeed > 0 || p.j != p.matched):
		return fmt.Errorf("query: git answered %d numstat records for the %d files the diff admits", p.j,
			p.matched)
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
	if err := r.patch(ctx, base, head, e, pg); err != nil {
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
	deadline, err := r.budget()
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
	narrow := fmt.Errorf("%w: the diff of %s and %s did not finish in the read's %s of git: diff commits "+
		"closer together", code.ErrSliceTooLarge, base, head, r.s.gitBudget)
	if err := r.failed(ctx, "diff", res, err, narrow); err != nil {
		return rawEntry{}, false, err
	}
	return rawEntry{}, false, nil
}

// patchConfig is what a patch's diff-pairs adds to the cache's
// configuration: core.quotePath off, so its header names a UTF-8 path as it
// is, as the stat's path does, rather than as octal escapes of its bytes.
var patchConfig = gitproc.KV{Key: "core.quotePath", Value: "false"}

// patch writes e's patch to pg: git's own, of the one record e, from ONE
// diff-pairs handed it on its standard input.
func (r *read) patch(ctx context.Context, base, head string, e rawEntry, pg *patchPage) error {
	deadline, err := r.budget()
	if err != nil {
		return err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Args: []string{"diff-pairs", "-z", "-p", "--full-index", "--no-color", "--no-ext-diff",
			"--no-textconv", "-U3"},
		Config: append(r.s.o.Cache.ServeConfig(), patchConfig), Stdin: bytes.NewReader(e.raw()), Stdout: pg,
		Class: gitproc.ClassRead, Deadline: deadline,
	})
	narrow := fmt.Errorf("%w: the patch of %q in the diff of %s and %s did not finish in the read's %s of "+
		"git: read the file's lines instead", code.ErrSliceTooLarge, e.paths[len(e.paths)-1], base, head,
		r.s.gitBudget)
	return r.failed(ctx, "patch of a file", res, err, narrow)
}

// patchPage cuts one page of a patch — whole lines from the byte from, while
// their encoded text costs at most left — out of git's patch as it is written,
// and counts the lines added and deleted over all of it.
type patchPage struct {
	from, left int
	// off is the patch's bytes so far; hunks whether the file's hunks have
	// begun, past which a line opening `+` or `-` is one added or deleted
	// (before them, they are the `---` and `+++` lines); sawFrom whether a
	// line began at from, which a cursor must name; binary whether git called
	// the pair binary.
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

// Write takes p, bytes of git's patch: a Cmd.Stdout that keeps no more than
// the page.
func (pg *patchPage) Write(p []byte) (int, error) {
	pg.feed(p)
	return len(p), nil
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

// begin opens a line whose first byte is c, read by git's patch grammar: in a
// file's hunks every line opens with a space, `+`, `-` or `\`, so a line
// opening `d` there is the next file header's `diff --git` — the creation that
// follows a deletion where a file changed its type — and only past a hunk's
// `@@` header is `+` or `-` a line counted; and before the hunks the one line
// opening `B` is `Binary files … differ`.
func (pg *patchPage) begin(c byte) {
	pg.atStart = true
	switch {
	case c == 'd':
		pg.hunks = false
	case c == '@':
		pg.hunks = true
	case !pg.hunks && c == 'B':
		pg.binary = true
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
