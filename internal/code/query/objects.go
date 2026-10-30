package query

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// WHAT A READ NAMES IS FOUND BEFORE ANYTHING IS READ, with at most two short
// processes in the read lane and no unit of the share: they wait in the lane's
// own queue like any short read, and what the share bounds is the long work
// after them. Each runs to its own bound, gitproc.ReadDeadline, outside the
// read's budget of git run time (server.go), which is its kind's alone; the
// read's answer falling due ends one sooner.
//
// AN ABBREVIATION IS EXPANDED FROM THE OBJECTS ALONE — ONE `git rev-parse
// --disambiguate=<a> [--disambiguate=<b>]`, whose every printed id is
// attributed to the abbreviations it begins with: exactly one is the whole id,
// none is an id the copy lacks (one look at the log's head, then
// code.ErrObjectNotFound), more than one is code.ErrInvalid asking for more of
// its digits. NEVER cat-file's own reading of a short name, which tries it as
// a REF first (git's get_oid_basic: refs/<p>, refs/tags/<p>, refs/heads/<p>…
// before the objects): measured, a tag named `a6d9bc1` pointing at commit
// 7e26dcd answers cat-file's `a6d9bc1` with 7e26dcd while a6d9bc1… is a
// commit of its own — so a branch or tag named like an abbreviation would
// answer a seat's read with another commit's content. A whole id is an object
// before any ref, so everything after the expansion names whole ids.
//
// THEN ONE `git cat-file --batch-check -Z` finds every id and path at once,
// fed on its standard input, where no option can hide (`--output=x` is a path
// there like any other): each id itself and peeled (`<id>^{}`, what a tag
// names), and the tree or file the read's path names (`<id>:<path>`, or
// `<id>^{tree}` for a root) — a blame's file among them, which its blame then
// names as the one argument after `--` (blame.go) — and for a re-anchoring,
// the object at each anchor's path in the commit its side names, which may be
// none. Records end in NUL both ways (-Z), so a path holding a newline is one
// record.

// object is one object as a check found it.
type object struct {
	// id is its whole id, and kind its type: commit, tree, blob or tag.
	id, kind string
	size     int64
	// peeled is what it names once every tag is peeled — itself for any
	// other kind — and peeledKind and peeledSize that object's.
	peeled, peeledKind string
	peeledSize         int64
}

// found is what the check found of the objects a read names.
type found struct {
	// revs is each revision the query names, as the query spells it, whole
	// or abbreviated.
	revs map[string]object
	// path is the path a tree, a file or a blame read names, decoded, and
	// target the object it names: the tree a listing lists, the file a read
	// reads or a blame blames — absent (an empty id) where the path names
	// nothing.
	path   string
	target object
	// placed is, for a re-anchoring, the object at each anchor's path in the
	// commit its side names, in the query's order — absent where the path
	// names nothing there, which places the anchor nowhere (gone) rather than
	// refusing the read.
	placed []object
}

// revsOf is every revision q names, once each, in the order it names them: a
// re-anchoring's commits, then the blob of each anchor. An interdiff's bases
// are not among them: it compares them as ids and reads neither.
func revsOf(q code.GitQuery) []string {
	var revs []string
	switch q.Kind {
	case code.GitTree:
		revs = []string{q.Tree.Rev}
	case code.GitBlob:
		revs = []string{q.Blob.Rev}
	case code.GitDiff:
		revs = []string{q.Diff.Base, q.Diff.Head}
	case code.GitCompare:
		revs = []string{q.Compare.Base, q.Compare.Head}
	case code.GitLandedIn:
		revs = []string{q.LandedIn.Commit, q.LandedIn.TargetOID}
	case code.GitRevisionFacts:
		revs = []string{q.RevisionFacts.Head, q.RevisionFacts.Target}
	case code.GitInterdiff:
		revs = []string{q.Interdiff.FromHead, q.Interdiff.ToHead}
	case code.GitLog:
		revs = []string{q.Log.Rev}
	case code.GitBlame:
		revs = []string{q.Blame.Rev}
	case code.GitChecksFile:
		revs = []string{q.ChecksFile.Commit}
	case code.GitReanchor:
		revs = []string{q.Reanchor.Head}
		if q.Reanchor.Base != "" {
			revs = append(revs, q.Reanchor.Base)
		}
		for _, a := range q.Reanchor.Anchors {
			if !slices.Contains(revs, a.BlobOID) {
				revs = append(revs, a.BlobOID)
			}
		}
	}
	return slices.Compact(revs)
}

// placedAt is, for a re-anchoring, each anchor's line of the batch: the
// object at its path in the commit its side names (validated already, by
// code.GitQuery.Validate).
func placedAt(q *code.ReanchorQuery, whole map[string]string) []string {
	lines := make([]string, len(q.Anchors))
	for i, a := range q.Anchors {
		at := whole[q.Head]
		if a.Side == code.SideOld {
			at = whole[q.Base]
		}
		p, _ := code.GitPath("path", a.Path, a.PathB64, false)
		lines[i] = at + ":" + p
	}
	return lines
}

// pathOf is the path a tree, a file or a blame read names, decoded (validated
// already, by code.GitQuery.Validate) — the object the batch looks up at the
// read's revision. A log's path is no such object: a history names the
// commits that changed a path, which its revision need not hold.
func pathOf(q code.GitQuery) string {
	var p string
	switch q.Kind {
	case code.GitTree:
		p, _ = code.GitPath("tree.path", q.Tree.Path, q.Tree.PathB64, true)
	case code.GitBlob:
		p, _ = code.GitPath("blob.path", q.Blob.Path, q.Blob.PathB64, true)
	case code.GitBlame:
		p, _ = code.GitPath("blame.path", q.Blame.Path, q.Blame.PathB64, false)
	case code.GitChecksFile:
		p = code.ChecksFilePath
	}
	return p
}

// lookedUp reports whether a read of q's kind looks its path up in the batch:
// a tree's, a file's, a blame's and a checks file's.
func lookedUp(q code.GitQuery) bool {
	return q.Kind == code.GitTree || q.Kind == code.GitBlob || q.Kind == code.GitBlame ||
		q.Kind == code.GitChecksFile
}

// find is step 7: what every revision and the path name, or the first
// revision the copy lacks (absent).
func (r *read) find(ctx context.Context) (objs found, absent string, err error) {
	revs := revsOf(r.q)
	whole := map[string]string{}
	var abbrevs []string
	for _, rev := range revs {
		if gitref.ValidOID(rev) {
			whole[rev] = rev
		} else {
			abbrevs = append(abbrevs, rev)
		}
	}
	if len(abbrevs) > 0 {
		expanded, missing, err := r.expand(ctx, abbrevs)
		if err != nil || missing != "" {
			return found{}, missing, err
		}
		for a, id := range expanded {
			whole[a] = id
		}
	}

	// THE BATCH: every id itself and peeled, then the path's object.
	ids := make([]string, 0, len(revs))
	for _, rev := range revs {
		if !slices.Contains(ids, whole[rev]) {
			ids = append(ids, whole[rev])
		}
	}
	var in []string
	for _, id := range ids {
		in = append(in, id, id+"^{}")
	}
	objs = found{revs: map[string]object{}, path: pathOf(r.q)}
	pathLine := -1
	if lookedUp(r.q) {
		pathLine = len(in)
		if objs.path == "" {
			in = append(in, ids[0]+"^{tree}")
		} else {
			in = append(in, ids[0]+":"+objs.path)
		}
	}
	placedLine := len(in)
	if r.q.Kind == code.GitReanchor {
		in = append(in, placedAt(r.q.Reanchor, whole)...)
	}
	got, err := r.check(ctx, in)
	if err != nil {
		return found{}, "", err
	}
	byID := map[string]object{}
	for i, id := range ids {
		self, peeled := got[2*i], got[2*i+1]
		if self.id == "" {
			return found{}, id, nil
		}
		self.peeled, self.peeledKind, self.peeledSize = peeled.id, peeled.kind, peeled.size
		byID[id] = self
	}
	for _, rev := range revs {
		objs.revs[rev] = byID[whole[rev]]
	}
	if pathLine >= 0 {
		objs.target = got[pathLine]
	}
	if r.q.Kind == code.GitReanchor {
		objs.placed = got[placedLine:]
	}
	return objs, "", nil
}

// expand is each abbreviation's whole id, from ONE rev-parse over the
// objects, or the first abbreviation no object begins with (missing).
func (r *read) expand(ctx context.Context, abbrevs []string) (map[string]string, string, error) {
	flags := make([]string, len(abbrevs))
	for i, a := range abbrevs {
		// VALIDATED HEX, the one option argument a read builds: an
		// abbreviation code.ValidRev admitted is digits alone.
		flags[i] = "--disambiguate=" + a
	}
	matches := map[string][]string{}
	lines := gitproc.NewLines(func(line []byte) error {
		id := string(line)
		if !gitref.ValidOID(id) {
			return fmt.Errorf("query: rev-parse printed %q, which is no object id", line)
		}
		for _, a := range abbrevs {
			// Two are enough to call an abbreviation ambiguous; the rest
			// are counted by nobody.
			if strings.HasPrefix(id, a) && len(matches[a]) < 2 {
				matches[a] = append(matches[a], id)
			}
		}
		return nil
	})
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: append([]string{"rev-parse"}, flags...), Config: r.s.o.Cache.ServeConfig(),
		Stdout: lines, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	})
	if err == nil {
		err = lines.Close()
	}
	if err := r.failed(ctx, "expansion of an abbreviated id", res, err, nil); err != nil {
		return nil, "", err
	}
	out := make(map[string]string, len(abbrevs))
	for _, a := range abbrevs {
		switch m := matches[a]; len(m) {
		case 0:
			return nil, a, nil
		case 1:
			out[a] = m[0]
		default:
			return nil, "", fmt.Errorf("%w: %s names more than one object of repository %s (%s and %s, at "+
				"least): give more of its digits", code.ErrInvalid, a, r.synced.Repo.Name, m[0], m[1])
		}
	}
	return out, "", nil
}

// check is what ONE cat-file batch says of each line of in, in order: the
// object it names, or the zero object where it names nothing. Before the
// read's own git begins it runs to its own bound, as every short check does;
// inside it — a revision's facts looking up their trees and blobs — to what
// is left of the read's budget, which it is charged to.
func (r *read) check(ctx context.Context, in []string) ([]object, error) {
	deadline := gitproc.ReadDeadline
	if r.metered {
		var err error
		if deadline, err = r.budget(); err != nil {
			return nil, err
		}
	}
	var stdin bytes.Buffer
	for _, line := range in {
		stdin.WriteString(line)
		stdin.WriteByte(0)
	}
	out := make([]object, 0, len(in))
	recs := newRecords(0, gitproc.MaxLine, func(rec []byte, long bool) error {
		i := len(out)
		if i >= len(in) {
			return fmt.Errorf("query: cat-file answered %d records for %d lines", i+1, len(in))
		}
		if !long && string(rec) == in[i]+" missing" {
			out = append(out, object{})
			return nil
		}
		o, ok := parseCheck(rec)
		if long || !ok {
			return fmt.Errorf("query: cat-file answered %q for %q, which is no object", rec, in[i])
		}
		out = append(out, o)
		return nil
	})
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"cat-file", "--batch-check", "-Z"}, Config: r.s.o.Cache.ServeConfig(),
		Stdin: &stdin, Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err == nil {
		err = recs.close()
	}
	if err := r.failed(ctx, "check of the objects the read names", res, err, nil); err != nil {
		return nil, err
	}
	if len(out) != len(in) {
		return nil, fmt.Errorf("query: cat-file answered %d records for %d lines", len(out), len(in))
	}
	return out, nil
}

// parseCheck reads one --batch-check record: "<id> <type> <size>".
func parseCheck(rec []byte) (object, bool) {
	fields := strings.Fields(string(rec))
	if len(fields) != 3 || !gitref.ValidOID(fields[0]) {
		return object{}, false
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return object{}, false
	}
	switch fields[1] {
	case "commit", "tree", "blob", "tag":
		return object{id: fields[0], kind: fields[1], size: size}, true
	}
	return object{}, false
}
