package query

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
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
// THE WORK IS THE CHANGE'S, NEVER THE TARGET'S: where the filter keeps to the
// touched paths, the heads are never walked. Each touched path — the two
// lists' union, at most 2 × code.MaxTouchedPaths — is LOOKED UP at both heads
// by ONE `git cat-file --batch-check -Z` fed `<head>:<path>` on its standard
// input, its mode, id and type answered (touchedRecords), and the raw record
// diff-tree would write for each path whose two entries differ is made here
// from them, in git's order, and handed on as a diff read's walk hands its
// records on: to diff-pairs, for the stat's counts and for one file's patch.
// So an interdiff costs git two lookups a touched path and the diffs of the
// files that changed, however much the target moved between the two bases: a
// walk of the heads' trees, which a three-file change rebased over a busy
// monorepo's target made read every entry the target moved — until it ran out
// the read's budget and was refused with a narrowing ("diff commits closer
// together") its asker cannot follow, the revisions being fixed — is gone.
//
// THE PATHS ARE DATA, never handed to git's command line: each is a line of
// the lookup's standard input, as a read's every path is (code.GitPath), and
// matched by git as a WHOLE path in a tree, so a path is a file's whole name
// — `a*` is the file `a*` and never `ab`, as a pathspec without its
// `:(literal)` magic would read it — and nothing reaches the command line,
// which a pathspec's paths would (git's diffs take none from a file or an
// input: no --pathspec-from-file), at up to two thousand arguments and a
// hundred kilobytes a read. The records made pass the same set filter
// (pathFilter's exact half) a walk's would.
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

// treeEntry is what a tree of a head holds at a path, as the lookup found it:
// its mode and id, or none (mode "") where the head holds no file there.
type treeEntry struct{ mode, id string }

// lookupFormat is the lookup's record: an entry's mode, id and type.
const lookupFormat = "--batch-check=%(objectmode) %(objectname) %(objecttype)"

// touchedRecords hands rec, as a walk's records are handed on, the raw record
// diff-tree would write for each of paths — sorted, as an exact filter holds
// them, which is git's order of full paths — whose entries at base and at
// head differ, made from ONE lookup of every path at both. Where rec stops
// the hand-off (errStop), so does it.
func (r *read) touchedRecords(ctx context.Context, base, head string, paths []string,
	rec func(rec []byte, long bool) error) error {

	if len(paths) == 0 {
		return nil
	}
	entries, err := r.lookup(ctx, base, head, paths)
	if err != nil {
		return err
	}
	for i, p := range paths {
		header, ok := rawHeader(entries[2*i], entries[2*i+1])
		if !ok {
			continue
		}
		if err := rec([]byte(header), false); err != nil {
			return err
		}
		if err := rec([]byte(p), false); err != nil {
			return err
		}
	}
	return nil
}

// lookupFile is base..head's raw record of the one file at path, made as
// touchedRecords makes each, with false where the two heads hold it alike.
func (r *read) lookupFile(ctx context.Context, base, head, path string) (rawEntry, bool, error) {
	var (
		e    rawEntry
		need int
	)
	err := r.touchedRecords(ctx, base, head, []string{path}, func(rec []byte, _ bool) error {
		if need > 0 {
			e.paths, need = append(e.paths, string(rec)), need-1
			return nil
		}
		var err error
		e, need, err = parseRaw(rec)
		return err
	})
	if err != nil || len(e.paths) == 0 {
		return rawEntry{}, false, err
	}
	return e, true, nil
}

// rawHeader is the header diff-tree -r writes for a path whose entry was old
// and is new: "" and false where they are alike. A tree is no entry of a
// recursive diff, which lists the files under it by their own paths.
func rawHeader(old, new treeEntry) (string, bool) {
	if old.mode == "040000" {
		old = treeEntry{}
	}
	if new.mode == "040000" {
		new = treeEntry{}
	}
	var status byte
	switch {
	case old == new:
		return "", false
	case old.mode == "":
		status, old = 'A', treeEntry{mode: "000000", id: strings.Repeat("0", len(new.id))}
	case new.mode == "":
		status, new = 'D', treeEntry{mode: "000000", id: strings.Repeat("0", len(old.id))}
	case fileType(old.mode) != fileType(new.mode):
		// A CHANGE OF TYPE — a file to a link, a link to a submodule — is
		// one record, as diff-tree writes it without -B.
		status = 'T'
	default:
		status = 'M'
	}
	return fmt.Sprintf(":%s %s %s %s %c", old.mode, new.mode, old.id, new.id, status), true
}

// fileType is the type a recursive diff's entry mode names: a regular file,
// executable or not, a symbolic link, or a submodule.
func fileType(mode string) string {
	if mode == "100755" {
		return "100644"
	}
	return mode
}

// lookup is what base and head each hold at every path of paths, two entries
// a path, base's first: ONE cat-file over `<commit>:<path>` lines on its
// standard input, NUL-ended both ways, run to what is left of the read's
// budget of git and charged to it. A head holding no file there answers
// missing, and a submodule whose commit the copy does not hold answers its id
// and "submodule" (git's own record for it, whatever the format asked).
func (r *read) lookup(ctx context.Context, base, head string, paths []string) ([]treeEntry, error) {
	var stdin bytes.Buffer
	in := make([]string, 0, 2*len(paths))
	for _, p := range paths {
		for _, c := range []string{base, head} {
			line := c + ":" + p
			in = append(in, line)
			stdin.WriteString(line)
			stdin.WriteByte(0)
		}
	}
	out := make([]treeEntry, 0, len(in))
	recs := newRecords(0, gitproc.MaxLine+code.MaxGitPathBytes, func(rec []byte, long bool) error {
		i := len(out)
		if i >= len(in) {
			return fmt.Errorf("query: cat-file answered %d records for %d lookups", i+1, len(in))
		}
		e, ok := parseLookup(rec, in[i])
		if long || !ok {
			return fmt.Errorf("query: cat-file answered %q for %q, which is no entry", truncated(rec), in[i])
		}
		out = append(out, e)
		return nil
	})
	deadline, err := r.budget()
	if err != nil {
		return nil, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"cat-file", lookupFormat, "-Z"}, Config: r.s.o.Cache.ServeConfig(),
		Stdin: &stdin, Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err == nil {
		err = recs.close()
	}
	narrow := fmt.Errorf("%w: the lookup of the %d paths the revisions touch at %s and %s did not finish in "+
		"the read's %s of git", code.ErrSliceTooLarge, len(paths), base, head, r.s.gitBudget)
	if err := r.failed(ctx, "lookup of the paths a change touches", res, err, narrow); err != nil {
		return nil, err
	}
	if len(out) != len(in) {
		return nil, fmt.Errorf("query: cat-file answered %d records for %d lookups", len(out), len(in))
	}
	return out, nil
}

// parseLookup reads one record of the lookup of line: "<mode> <id> <type>",
// "<line> missing", or a submodule's "<id> submodule".
func parseLookup(rec []byte, line string) (treeEntry, bool) {
	if string(rec) == line+" missing" {
		return treeEntry{}, true
	}
	f := strings.Fields(string(rec))
	switch {
	case len(f) == 2 && gitref.ValidOID(f[0]) && f[1] == "submodule":
		return treeEntry{mode: "160000", id: f[0]}, true
	case len(f) != 3 || !gitref.ValidOID(f[1]):
		return treeEntry{}, false
	}
	switch f[0] + " " + f[2] {
	case "100644 blob", "100755 blob", "120000 blob", "160000 commit", "040000 tree":
		return treeEntry{mode: f[0], id: f[1]}, true
	}
	return treeEntry{}, false
}
