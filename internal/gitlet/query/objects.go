package query

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// WHAT A READ NAMES IS FOUND FIRST, by at most two short processes holding no
// unit of the share, each to gitproc.ReadDeadline outside the read's git budget.
//
// AN ABBREVIATION IS EXPANDED FROM THE OBJECTS ALONE (expand), never by
// cat-file, which tries refs first (git's get_oid_basic): measured, a tag
// `a6d9bc1` at commit 7e26dcd answers cat-file's `a6d9bc1` with 7e26dcd, so a
// ref named like an abbreviation would answer a read with another commit. A
// whole id is an object before any ref, so all after expansion names whole ids.
//
// THEN ONE `cat-file --batch-check -Z` (check) finds every id, peeled id and
// path at once, fed on its standard input, where no option can hide, and
// NUL-ended both ways, so a path holding a newline is one record.

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
	// path is the decoded path the read names (pathOf), and target its
	// object: an empty id where the path names nothing.
	path   string
	target object
	// placed is, for a re-anchoring, the object at each anchor's path in the
	// commit its side names, in the query's order; absent where the path names
	// nothing there, which places the anchor nowhere (gone) rather than
	// refusing the read. For an excerpts read it is each anchor's other side
	// (excerptOthersAt), absent where there is none.
	placed []object
}

// revsOf is every revision q names, once each, in the order it names them: a
// re-anchoring's commits, then the blob of each anchor. An interdiff's bases
// are not among them: it compares them as ids and reads neither.
func revsOf(q gitlet.GitQuery) []string {
	var revs []string
	switch q.Kind {
	case gitlet.GitTree:
		revs = []string{q.Tree.Rev}
	case gitlet.GitBlob:
		revs = []string{q.Blob.Rev}
	case gitlet.GitDiff:
		revs = []string{q.Diff.Base, q.Diff.Head}
	case gitlet.GitCompare:
		revs = []string{q.Compare.Base, q.Compare.Head}
	case gitlet.GitTips:
		revs = slices.Clone(q.Tips.OIDs)
		if q.Tips.Base != "" && !slices.Contains(revs, q.Tips.Base) {
			revs = append(revs, q.Tips.Base)
		}
	case gitlet.GitLandedIn:
		revs = []string{q.LandedIn.Commit, q.LandedIn.TargetOID}
	case gitlet.GitRevisionFacts:
		revs = []string{q.RevisionFacts.Head, q.RevisionFacts.Target}
	case gitlet.GitInterdiff:
		revs = []string{q.Interdiff.FromHead, q.Interdiff.ToHead}
	case gitlet.GitLog:
		revs = []string{q.Log.Rev}
	case gitlet.GitBlame:
		revs = []string{q.Blame.Rev}
	case gitlet.GitTreeCommits:
		revs = []string{q.TreeCommits.Rev}
	case gitlet.GitChecksFile:
		revs = []string{q.ChecksFile.Commit}
	case gitlet.GitCodeFiles:
		revs = []string{q.CodeFiles.Commit}
	case gitlet.GitFind:
		revs = []string{q.Find.Rev}
	case gitlet.GitReanchor:
		revs = []string{q.Reanchor.Head}
		if q.Reanchor.Base != "" {
			revs = append(revs, q.Reanchor.Base)
		}
		for _, a := range q.Reanchor.Anchors {
			if !slices.Contains(revs, a.BlobOID) {
				revs = append(revs, a.BlobOID)
			}
		}
	case gitlet.GitExcerpts:
		for _, a := range q.Excerpts.Anchors {
			for _, id := range []string{a.Head, a.Base, a.BlobOID} {
				if id != "" && !slices.Contains(revs, id) {
					revs = append(revs, id)
				}
			}
		}
	}
	return slices.Compact(revs)
}

// placedAt is, for a re-anchoring, each anchor's line of the batch: the
// object at its path in the commit its side names (validated already, by
// gitlet.GitQuery.Validate).
func placedAt(q *gitlet.ReanchorQuery, whole map[string]string) []string {
	lines := make([]string, len(q.Anchors))
	for i, a := range q.Anchors {
		at := whole[q.Head]
		if a.Side == gitlet.SideOld {
			at = whole[q.Base]
		}
		p, _ := gitlet.GitPath("path", a.Path, a.PathB64, false)
		lines[i] = at + ":" + p
	}
	return lines
}

// excerptOthersAt is, for an excerpts read, the batch's line of each anchor's other side: the
// object at its path in the base for a new-side anchor, in the head for an old-side one. of
// is each anchor's line, -1 for a new-side anchor with no base.
func excerptOthersAt(q *gitlet.ExcerptsQuery, whole map[string]string) (lines []string, of []int) {
	of = make([]int, len(q.Anchors))
	for i, a := range q.Anchors {
		at := whole[a.Base]
		if a.Side == gitlet.SideOld {
			at = whole[a.Head]
		}
		of[i] = -1
		if a.Base == "" {
			continue
		}
		p, _ := gitlet.GitPath("path", a.Path, a.PathB64, false)
		of[i] = len(lines)
		lines = append(lines, at+":"+p)
	}
	return lines, of
}

// pathOf is the decoded path the batch looks up at the read's revision
// (validated already, by gitlet.GitQuery.Validate). A log's path is not one: a
// history names the commits that changed a path, which its revision need not
// hold.
func pathOf(q gitlet.GitQuery) string {
	var p string
	switch q.Kind {
	case gitlet.GitTree:
		p, _ = gitlet.GitPath("tree.path", q.Tree.Path, q.Tree.PathB64, true)
	case gitlet.GitBlob:
		p, _ = gitlet.GitPath("blob.path", q.Blob.Path, q.Blob.PathB64, true)
	case gitlet.GitBlame:
		p, _ = gitlet.GitPath("blame.path", q.Blame.Path, q.Blame.PathB64, false)
	case gitlet.GitTreeCommits:
		p, _ = gitlet.GitPath("tree_commits.path", q.TreeCommits.Path, q.TreeCommits.PathB64, true)
	case gitlet.GitChecksFile:
		p = gitlet.ChecksFilePath
	case gitlet.GitCodeFiles:
		p = q.CodeFiles.File
	}
	return p
}

// lookedUp reports whether a read of q's kind looks its path up in the batch;
// a code files read only where it names one file, and a find always, for the
// root tree (`<id>^{tree}`) whose listing it reads by that tree's id.
func lookedUp(q gitlet.GitQuery) bool {
	return q.Kind == gitlet.GitTree || q.Kind == gitlet.GitBlob || q.Kind == gitlet.GitBlame ||
		q.Kind == gitlet.GitTreeCommits || q.Kind == gitlet.GitChecksFile ||
		(q.Kind == gitlet.GitCodeFiles && q.CodeFiles.File != "") || q.Kind == gitlet.GitFind
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
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
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
	var others []int
	switch r.q.Kind {
	case gitlet.GitReanchor:
		in = append(in, placedAt(r.q.Reanchor, whole)...)
	case gitlet.GitExcerpts:
		var lines []string
		lines, others = excerptOthersAt(r.q.Excerpts, whole)
		in = append(in, lines...)
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
	switch r.q.Kind {
	case gitlet.GitReanchor:
		objs.placed = got[placedLine:]
	case gitlet.GitExcerpts:
		objs.placed = make([]object, len(others))
		for i, at := range others {
			if at >= 0 {
				objs.placed[i] = got[placedLine+at]
			}
		}
	}
	return objs, "", nil
}

// expand is each abbreviation's whole id, from ONE rev-parse over the
// objects, or the first abbreviation no object begins with (missing).
func (r *read) expand(ctx context.Context, abbrevs []string) (map[string]string, string, error) {
	flags := make([]string, len(abbrevs))
	for i, a := range abbrevs {
		// VALIDATED HEX, the one option argument a read builds: an
		// abbreviation gitlet.ValidRev admitted is digits alone.
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
	if err = r.failed(ctx, "expansion of an abbreviated id", res, err, nil); err != nil {
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
				"least): give more of its digits", gitlet.ErrInvalid, a, r.synced.Repo.Name, m[0], m[1])
		}
	}
	return out, "", nil
}

// check is what ONE cat-file batch says of each line of in, in order: the
// object it names, or the zero object where it names nothing. Before the
// read's own git begins it runs to its own bound; inside it (a revision's
// facts looking up their trees and blobs), to what is left of the read's
// budget, which it is charged to.
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
		o, ok := gitproc.ParseCheck(rec)
		if long || !ok {
			return fmt.Errorf("query: cat-file answered %q for %q, which is no object", rec, in[i])
		}
		out = append(out, object{id: o.ID, kind: o.Type, size: o.Size})
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
	if err = r.failed(ctx, "check of the objects the read names", res, err, nil); err != nil {
		return nil, err
	}
	if len(out) != len(in) {
		return nil, fmt.Errorf("query: cat-file answered %d records for %d lines", len(out), len(in))
	}
	return out, nil
}
