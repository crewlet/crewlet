package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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

// diffRead answers a diff read: a page of its stat, or of one file's patch —
// and, where m takes them, every page of either after it.
func (r *read) diffRead(ctx context.Context, objs found, at cursor, m *memoRun) (code.GitAnswer, error) {
	q := r.q.Diff
	base, head := objs.revs[q.Base], objs.revs[q.Head]
	if err := diffable("diff.base", q.Base, base, "diff.head", q.Head, head); err != nil {
		return code.GitAnswer{}, err
	}
	file, _ := code.GitPath("diff.file", q.File, q.FileB64, true)
	return r.diffPages(ctx, diffAsk{kind: code.GitDiff, base: base.peeled, head: head.peeled, file: file,
		renames: q.Renames, filter: underFilter(q.Paths), narrowing: "name paths, or diff commits closer together"},
		at, m)
}

// diffable refuses a diff of two objects that are not each a commit or a
// tree, naming the field of the first that is not.
func diffable(baseField, baseRev string, base object, headField, headRev string, head object) error {
	for _, side := range []struct {
		field, rev string
		o          object
	}{{baseField, baseRev, base}, {headField, headRev, head}} {
		if side.o.peeledKind != "commit" && side.o.peeledKind != "tree" {
			return fmt.Errorf("%w: %s: %s names a %s; a diff compares commits or trees", code.ErrInvalid,
				side.field, side.rev, side.o.peeledKind)
		}
	}
	return nil
}

// diffAsk is the diff a read's pages are cut from: the kind that asks it — a
// diff, or an interdiff, which is one — the two commits or trees, whole, the
// one file a patch names or none, whether renames are detected, the files a
// stat admits (filter), and what an asker whose stat its filter admits every
// file of ran the read's budget out is told to narrow.
type diffAsk struct {
	kind       code.GitReadKind
	base, head string
	file       string
	renames    bool
	filter     pathFilter
	narrowing  string
}

// diffPages answers d: a page of its stat, or of its one file's patch — and,
// where m takes them, every page of either after it.
func (r *read) diffPages(ctx context.Context, d diffAsk, at cursor, m *memoRun) (code.GitAnswer, error) {
	if d.file != "" {
		return r.patchRead(ctx, d, at, m)
	}
	page := func(files []code.DiffFile, next cursor) code.GitAnswer {
		return code.GitAnswer{Kind: d.kind, Next: formatCursor(next),
			Diff: &code.DiffAnswer{Base: d.base, Head: d.head, Files: files}}
	}
	envelope := code.GitAnswer{Kind: d.kind, Next: widestCursor('f', 1),
		Diff: &code.DiffAnswer{Base: d.base, Head: d.head, Files: []code.DiffFile{}, Total: widest,
			Added: widest, Deleted: widest}}
	room := r.room(envelope)
	var sp *statPages
	if m != nil {
		sp = &statPages{m: m, room: room, at: at, page: page}
	}
	st, err := r.stat(ctx, d.base, d.head, d.filter, d.renames, at.a, room, d.narrowing, sp)
	if err != nil {
		return code.GitAnswer{}, err
	}
	if at.a > 0 && at.a >= st.total {
		return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, fmt.Sprintf("a file before the %d the "+
			"diff changes", st.total))
	}
	a := page(st.files, st.next)
	a.Diff.Total, a.Diff.Added, a.Diff.Deleted = st.total, st.added, st.deleted
	if sp != nil {
		sp.counts.done()
		m.place(d.kind, same(memoTail{total: st.total, added: st.added, deleted: st.deleted, counted: true}))
	}
	return a, nil
}

// statPage is a page of a stat, the totals of the whole of it, and what its
// files left of the room it was cut against.
type statPage struct {
	files                 []code.DiffFile
	total, added, deleted int
	next                  cursor
	left                  int
}

// statPages is the pages of a diff's stat its run memoizes: the asked one,
// staged as soon as the raw half has decided it, and every one after it, cut
// as the raw half reads their files and staged as each fills — with no counts,
// which follow every file — and each page's files' counts, filed as the
// numstat half reads them and written into the page's staged entry once the
// half has counted past it (pageCounts). What it holds is a page of files and
// the counts of one page: every page's first ordinal and its put, a few
// bytes a page, and none a file past the page it counts.
type statPages struct {
	m    *memoRun
	room int
	// at is the asked page's cursor, and page the answer a page of files is.
	at   cursor
	page func(files []code.DiffFile, next cursor) code.GitAnswer
	// The page being cut past the asked one: its files, the ordinal (among
	// the files the filter admits) of its first, what its files left of the
	// room, and whether a page is being cut at all — none once one file is
	// wider than a page alone, which a read asking from it is refused.
	files   []code.DiffFile
	first   int
	left    int
	cutting bool
	// counts files each staged page's files' counts as the numstat half reads
	// them, and hands each page's on to its staged entry; puts is each staged
	// page's put, by its index among them.
	counts pageCounts
	puts   []int
}

// stage records a page staged as put, of files from the first-th, ending
// before next.
func (sp *statPages) stage(put, first int, next cursor) {
	sp.puts = append(sp.puts, put)
	limit := maxInt
	if next.letter != 0 {
		limit = next.a
	}
	sp.counts.page(first, limit)
	if sp.counts.hand == nil {
		sp.counts.hand = func(page int, counts []fileCounts) { sp.m.extend(sp.puts[page], counts) }
	}
}

// asked stages the asked page, of files from the start-th, ending before
// next, its files leaving left of the room.
func (sp *statPages) asked(files []code.DiffFile, start int, next cursor, left int) {
	sp.stage(sp.m.put(sp.at, sp.page(files, next), left), start, next)
}

// begin begins a page past the asked one at the ordinal-th file, f.
func (sp *statPages) begin(ordinal int, f code.DiffFile) {
	cost := jsonLen(widestFile(f))
	if cost > sp.room {
		sp.cutting = false
		return
	}
	sp.files, sp.first, sp.left, sp.cutting = []code.DiffFile{f}, ordinal, sp.room-cost, true
}

// take places the ordinal-th file, f, on the page being cut, or ends that
// page before it and begins the next with it.
func (sp *statPages) take(ordinal int, f code.DiffFile) {
	if !sp.cutting {
		return
	}
	if cost := jsonLen(widestFile(f)) + 1; cost <= sp.left {
		sp.files, sp.left = append(sp.files, f), sp.left-cost
		return
	}
	sp.end(cursor{letter: 'f', a: ordinal})
	sp.begin(ordinal, f)
}

// end stages the page being cut, ending before next (none past the stat's
// end).
func (sp *statPages) end(next cursor) {
	sp.stage(sp.m.put(cursor{letter: 'f', a: sp.first}, sp.page(sp.files, next), sp.left), sp.first, next)
}

// finish stages the last page being cut, which the stat's end ends.
func (sp *statPages) finish() {
	if sp.cutting {
		sp.end(cursor{})
		sp.cutting = false
	}
}

// count files the ordinal-th file's counts under the staged page it is on.
func (sp *statPages) count(ordinal int, c fileCounts) { sp.counts.add(ordinal, c) }

// pageCounts files a stat's files' counts under the staged page each is of —
// pages of consecutive ordinals, the numstat half's order — and HANDS EACH
// PAGE'S ON as soon as the half has counted past it, so it holds the counts
// of one page at a time: about 24 bytes a file of that page, where filing
// every page's to the run's end held 24 bytes for every file of the stat past
// the asked page, a stat of a million files 24 MB, under no bound. done hands
// on what is left; a page counted past with no counts — none the half wrote
// for it — is handed none, which its entry, held to its files, refuses.
type pageCounts struct {
	// bounds is every page's first ordinal and limit the ordinal past the
	// last page's last file; k is the page being counted, held its counts so
	// far, and handed how many pages are handed on.
	bounds []int
	limit  int
	k      int
	held   []fileCounts
	handed int
	// hand takes the page-th page's counts, in its files' order; most is the
	// most counts held at once, which the suite reads.
	hand func(page int, counts []fileCounts)
	most int
}

// page adds a page of files from the first-th ordinal, ending before limit.
func (pc *pageCounts) page(first, limit int) {
	pc.bounds = append(pc.bounds, first)
	pc.limit = limit
}

// add files the ordinal-th file's counts c under the page it is on, handing
// on every page before it first.
func (pc *pageCounts) add(ordinal int, c fileCounts) {
	if len(pc.bounds) == 0 || ordinal < pc.bounds[0] || ordinal >= pc.limit {
		return
	}
	for pc.k+1 < len(pc.bounds) && ordinal >= pc.bounds[pc.k+1] {
		pc.handOn()
		pc.k++
	}
	pc.held = append(pc.held, c)
	pc.most = max(pc.most, len(pc.held))
}

// handOn hands the page being counted on, and holds none of it.
func (pc *pageCounts) handOn() {
	if pc.handed > pc.k {
		return
	}
	if pc.hand != nil {
		pc.hand(pc.k, pc.held)
	}
	pc.handed = pc.k + 1
	pc.held = pc.held[:0]
}

// done hands on every page not yet handed: the stat has counted all it will.
func (pc *pageCounts) done() {
	for ; pc.k < len(pc.bounds); pc.k++ {
		pc.handOn()
	}
}

// widestFile is f as a page costs it before its counts are read: its counts
// and binary flag at their widest, so where a page ends never depends on a
// count not yet read.
func widestFile(f code.DiffFile) code.DiffFile {
	f.Added, f.Deleted, f.Binary = widest, widest, true
	return f
}

// stat is the page of base..head's stat that starts at the start-th file the
// filter admits, whose files may take left bytes, and the totals over every
// file it admits — and, where sp is set, every page after it, each staged as
// it is cut. narrowing is what a stat its filter admits every file of, whose
// one diff-tree the deadline ended, tells its asker to do instead — naming
// paths among it, where it can, since a stat that names them counts the files
// they admit and no other.
func (r *read) stat(ctx context.Context, base, head string, filter pathFilter, renames bool, start, left int,
	narrowing string, sp *statPages) (statPage, error) {

	p := &statParser{filter: filter, start: start, left: left, keep: filter.filters(), pages: sp}
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
	switch {
	case filter.set:
		// AN EXACT FILTER NAMES EVERY FILE IT ADMITS — an interdiff's
		// touched paths — so its walk is those files looked up at both
		// commits (interdiff.go), never the two trees, whose difference
		// beyond them is nothing the stat counts.
		err = r.touchedRecords(ctx, base, head, filter.paths, p.record)
		if p.tooLarge != nil {
			return statPage{}, p.tooLarge
		}
		if err == nil {
			err = p.end(false)
		}
		if err != nil {
			return statPage{}, err
		}
	case p.keep:
		// A FILTERED STAT'S WALK, its raw records alone: the numstat is
		// diff-pairs', of the files the filter admits (count). No path
		// narrows the walk itself, which reads the two trees whatever the
		// filter, so a walk the deadline ended is refused naming the commits.
		narrow = fmt.Errorf("%w: the walk of the diff of %s and %s did not finish in the read's %s of git: "+
			"diff commits closer together", code.ErrSliceTooLarge, base, head, r.s.gitBudget)
		res, err = r.s.o.Git.Run(ctx, gitproc.Cmd{
			Dir: r.synced.Dir, Dropping: r.synced.Dropping,
			Args: append([]string{"diff-tree", "-z", "-r", "--raw", "--no-ext-diff",
				"--no-textconv"}, tail...),
			Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
		})
	default:
		res, err = r.s.o.Git.Run(ctx, gitproc.Cmd{
			Dir: r.synced.Dir, Dropping: r.synced.Dropping,
			Args: append([]string{"diff-tree", "-z", "-r", "--raw", "--numstat", "--no-ext-diff",
				"--no-textconv"}, tail...),
			Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
		})
	}
	if !filter.set {
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
	return statPage{files: files, total: p.matched, added: p.added, deleted: p.deleted, next: p.next,
		left: p.left}, nil
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
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   []string{"diff-pairs", "-z", "--numstat", "--no-ext-diff", "--no-textconv"},
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
	filter      pathFilter
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

	// The numstat half: whether it has begun, the record it is at — the
	// j-th record the j-th file admitted, so j is also that file's ordinal
	// among them — a rename's counts while its two paths are read, and the
	// totals.
	numstat        bool
	j              int
	nsNeed         int
	nsAdded, nsDel int
	nsBinary       bool
	added, deleted int

	// pages is the pages past the asked one the run memoizes, nil for none;
	// decided whether the raw half has ended, which decides the asked page.
	pages   *statPages
	decided bool
}

// rawEnded is the raw half's end — at the numstat half's first record where
// one diff-tree writes both halves, and otherwise at the raw records' end
// (end): a filtered stat's walk, whose numstat diff-pairs writes after it, or
// a stat of no file. The asked page is decided there, and so is the last page
// past it.
func (p *statParser) rawEnded() {
	if p.decided {
		return
	}
	p.decided = true
	switch {
	case p.pages == nil:
	case p.full:
		p.pages.finish()
	default:
		p.pages.asked(p.files, p.start, p.next, p.left)
	}
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
		if !p.numstat {
			p.numstat = true
			p.rawEnded()
		}
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

// pathFilter is the files a stat or a patch admits, JUDGED HERE over git's
// raw records and never handed to git, as a pathspec or as anything else:
// every file (the zero filter); a diff read's paths, each admitting the file
// it names and every file under it as a directory (underFilter); or an
// interdiff's touched paths, each admitting the one file it names whole
// (exactFilter) — so `a*` admits the file `a*` and never `ab`, as a pathspec
// would without its `:(literal)` magic — looked up in a set, since a list of
// a thousand paths matched against every record of a wide diff would cost the
// read a thousand comparisons a record.
type pathFilter struct {
	under []string
	// exact is an interdiff's set, and set whether there is one: an empty
	// set admits no file, where the zero filter admits every one. paths is
	// the set's paths sorted, as a memo key names them.
	exact map[string]bool
	set   bool
	paths []string
}

// underFilter is a diff read's filter: each path a file, or a directory
// above files; none admits every file.
func underFilter(paths []string) pathFilter { return pathFilter{under: paths} }

// exactFilter admits the files paths name, each whole, and no other — none
// where paths is empty.
func exactFilter(paths []string) pathFilter {
	f := pathFilter{exact: make(map[string]bool, len(paths)), set: true}
	for _, p := range paths {
		f.exact[p] = true
	}
	f.paths = slices.Sorted(maps.Keys(f.exact))
	return f
}

// filters reports whether the filter admits fewer than every file.
func (f pathFilter) filters() bool { return f.set || len(f.under) > 0 }

// admits reports whether the filter admits a file at any of paths — a
// rename's two among them.
func (f pathFilter) admits(paths []string) bool {
	switch {
	case f.set:
		for _, p := range paths {
			if f.exact[p] {
				return true
			}
		}
		return false
	case len(f.under) == 0:
		return true
	}
	for _, u := range f.under {
		for _, p := range paths {
			if p == u || strings.HasPrefix(p, u+"/") {
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
	if !p.filter.admits(p.entry.paths) {
		return
	}
	m := p.matched
	p.matched++
	if p.keep {
		p.pairs.Write(p.entry.raw())
	}
	if m < p.start || (p.full && p.pages == nil) {
		return
	}
	f := p.entry.file()
	if p.full {
		p.pages.take(m, f)
		return
	}
	cost := jsonLen(widestFile(f))
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
		if p.pages != nil {
			p.pages.asked(p.files, p.start, p.next, p.left)
			p.pages.begin(m, f)
		}
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
	c := fileCounts{added: p.nsAdded, deleted: p.nsDel, binary: p.nsBinary}
	if c.binary {
		c.added, c.deleted = 0, 0
	}
	p.added += c.added
	p.deleted += c.deleted
	if slot := j - p.start; slot >= 0 && slot < len(p.files) {
		f := &p.files[slot]
		f.Added, f.Deleted, f.Binary = c.added, c.deleted, c.binary
	}
	if p.pages != nil {
		p.pages.count(j, c)
	}
	return nil
}

// end checks git's records ended where a record does — no raw record still
// waiting on its paths — and, once the numstat half is read (counted), that
// it counted every admitted file, once. The raw records' end is where the
// asked page and the last page past it are decided (rawEnded), whichever
// process wrote them.
func (p *statParser) end(counted bool) error {
	p.rawEnded()
	switch {
	case p.need > 0:
		return errors.New("query: git's raw records ended before the paths of the last of them")
	case counted && (p.nsNeed > 0 || p.j != p.matched):
		return fmt.Errorf("query: git answered %d numstat records for the %d files the diff admits", p.j,
			p.matched)
	}
	return nil
}

// patchRead answers one file's patch: d's change to its file, a page from the
// byte at.
func (r *read) patchRead(ctx context.Context, d diffAsk, at cursor, m *memoRun) (code.GitAnswer, error) {
	base, head, file := d.base, d.head, d.file
	a := code.GitAnswer{Kind: d.kind, Diff: &code.DiffAnswer{Base: base, Head: head, Files: []code.DiffFile{}}}
	var (
		e   rawEntry
		ok  bool
		err error
	)
	// A FILE THE FILTER DOES NOT ADMIT is one the read does not change — an
	// interdiff's file that neither revision touches — and no git is asked.
	// One the exact filter of an interdiff admits is LOOKED UP at both
	// commits, as its stat's every file is, never found by walking them.
	switch {
	case d.filter.set && d.filter.admits([]string{file}):
		if e, ok, err = r.lookupFile(ctx, base, head, file); err != nil {
			return code.GitAnswer{}, err
		}
	case d.filter.admits([]string{file}):
		if e, ok, err = r.findFile(ctx, base, head, file, d.renames); err != nil {
			return code.GitAnswer{}, err
		}
	}
	if !ok {
		// A FILE THE DIFF DOES NOT CHANGE has no patch, as git prints none
		// for it: an answer with no file, which a stat of the same diff
		// confirms.
		if at.letter != 0 {
			return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, "a byte of a patch")
		}
		m.put(at, a, r.room(a))
		m.place(d.kind, same(memoTail{}))
		return a, nil
	}
	f := e.file()
	// page is a page of the patch as the memo keeps it: the file's counts are
	// its tail's, read once the whole patch is.
	page := func(pg *patchPage) code.GitAnswer {
		return code.GitAnswer{Kind: d.kind, Next: formatCursor(pg.next), Diff: &code.DiffAnswer{Base: base,
			Head: head, Files: []code.DiffFile{e.file()}, Patch: string(pg.text)}}
	}
	envelope := code.GitAnswer{Kind: d.kind, Next: widestCursor('p', 1),
		Diff: &code.DiffAnswer{Base: base, Head: head, Files: []code.DiffFile{widestFile(f)}, Total: widest,
			Added: widest, Deleted: widest, Patch: "x"}}
	room := r.room(envelope) + 1
	pages := newPatchPages(&patchPage{from: at.a, left: room}, room, m != nil,
		func(pg *patchPage) { m.put(pg.begun, page(pg), pg.left) })
	if err := r.patch(ctx, base, head, e, pages); err != nil {
		return code.GitAnswer{}, err
	}
	pages.finish()
	pg := pages.asked
	switch {
	case pg.tooLong != nil:
		return code.GitAnswer{}, pg.tooLong
	case at.a > 0 && !pg.sawFrom:
		return code.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, fmt.Sprintf("the start of a line of the "+
			"patch's %d bytes", pg.off))
	}
	a = page(pg)
	m.put(at, a, pg.left)
	f.Binary = pg.binary
	if !f.Binary {
		f.Added, f.Deleted = pg.added, pg.deleted
	}
	a.Diff.Files, a.Diff.Total, a.Diff.Added, a.Diff.Deleted = []code.DiffFile{f}, 1, f.Added, f.Deleted
	m.place(d.kind, same(memoTail{total: 1, added: f.Added, deleted: f.Deleted,
		files: []fileCounts{{added: f.Added, deleted: f.Deleted, binary: f.Binary}}}))
	return a, nil
}

// patchPages is a patch's pages as git's diff-pairs writes it (a Cmd.Stdout):
// the asked page, fed the whole patch — it also counts the lines the patch
// adds and deletes, and reads whether git called the pair binary — and, where
// more is set, every page after it, each a patchPage of its own begun at the
// line the one before it could not take and fed that line from its start
// (line), so every page is exactly the page a read from its cursor would cut;
// each handed to page as it is cut. A page past the asked one at a line too
// long for any page ends the pages: a read asking from it is refused it.
type patchPages struct {
	room  int
	more  bool
	page  func(pg *patchPage)
	asked *patchPage
	// cur is the page being cut — the asked one first — and nil once none is.
	cur *patchPage
	// off is the patch's bytes so far and lineAt where the current line
	// began; line is the current line's bytes from its start, kept while a
	// page past the asked one may begin at it.
	off, lineAt int
	line        []byte
}

// newPatchPages is the pages of a patch whose asked page is asked, each cut
// against room; more, every page after it too, each handed to page.
func newPatchPages(asked *patchPage, room int, more bool, page func(pg *patchPage)) *patchPages {
	return &patchPages{room: room, more: more, page: page, asked: asked, cur: asked}
}

// Write takes p, bytes of git's patch: a Cmd.Stdout that keeps no more than
// the pages being cut and the line in hand.
func (w *patchPages) Write(p []byte) (int, error) {
	w.feed(p)
	return len(p), nil
}

// feed takes p, bytes of the patch, a line — or the part of one p ends in —
// at a time.
func (w *patchPages) feed(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		seg := p
		if i >= 0 {
			seg = p[:i+1]
		}
		if w.more && w.cur != nil && w.lineAt >= w.asked.from {
			w.line = append(w.line, seg...)
		}
		w.asked.feed(seg)
		if w.cur != nil && w.cur != w.asked {
			w.cur.feed(seg)
		}
		w.off += len(seg)
		w.chain(false)
		if i >= 0 {
			w.line, w.lineAt = w.line[:0], w.off
		}
		p = p[len(seg):]
	}
}

// finish ends the patch: every page's last line settled, and the last page
// past the asked one handed on.
func (w *patchPages) finish() {
	w.asked.finish()
	if w.cur != nil && w.cur != w.asked {
		w.cur.finish()
	}
	w.chain(true)
	if p := w.cur; p != nil && p != w.asked && p.tooLong == nil {
		w.page(p)
	}
}

// chain hands on every page the patch has just ended at a line it could not
// take, and begins the next at that line, fed it from its start — and, at
// the patch's end (eof), settled there, which may end it too.
func (w *patchPages) chain(eof bool) {
	for w.cur != nil && w.cur.done {
		done := w.cur
		if done.tooLong != nil {
			w.cur = nil
			return
		}
		if done != w.asked {
			w.page(done)
		}
		if !w.more || done.next.letter == 0 {
			w.cur = nil
			return
		}
		next := &patchPage{from: done.next.a, left: w.room, off: done.next.a, lineAt: done.next.a,
			begun: done.next}
		w.cur = next
		next.feed(w.line)
		if eof {
			next.finish()
		}
	}
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
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: append([]string{"diff-tree", "-z", "-r", "--raw", "--no-ext-diff",
			"--no-textconv"}, tail...),
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	switch {
	case hit && res.Damage == nil:
		// THE LOOKUP IS THE PATCH'S OWN GIT, and its run comes out of the
		// budget the patch's diff-pairs is then given what is left of.
		r.charge(res)
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

// patch writes e's patch to pages: git's own, of the one record e, from ONE
// diff-pairs handed it on its standard input.
func (r *read) patch(ctx context.Context, base, head string, e rawEntry, pages *patchPages) error {
	deadline, err := r.budget()
	if err != nil {
		return err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"diff-pairs", "-z", "-p", "--full-index", "--no-color", "--no-ext-diff",
			"--no-textconv", "-U3"},
		Config: append(r.s.o.Cache.ServeConfig(), patchConfig), Stdin: bytes.NewReader(e.raw()), Stdout: pages,
		Class: gitproc.ClassRead, Deadline: deadline,
	})
	narrow := fmt.Errorf("%w: the patch of %q in the diff of %s and %s did not finish in the read's %s of "+
		"git: read the file's lines instead", code.ErrSliceTooLarge, e.paths[len(e.paths)-1], base, head,
		r.s.gitBudget)
	return r.failed(ctx, "patch of a file", res, err, narrow)
}

// patchPage cuts one page of a patch — whole lines from the byte from, while
// their encoded text costs at most left — out of git's patch as it is written,
// and counts the lines added and deleted over all of it. A page past the one a
// read asked for is begun by patchPages at the cursor the page before it
// names (begun), fed from there.
type patchPage struct {
	from, left int
	begun      cursor
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
