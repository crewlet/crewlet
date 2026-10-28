package query

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// A RE-ANCHORING places ranges of lines, each read in a blob one revision of a
// change held — a finding's, a comment's — in the files of another revision:
// the file at each anchor's path in the revision's head, for a line on the new
// side of the change's diff, or in its base, for the old side. The read's one
// check of what it names (objects.go) finds each anchor's blob, which must be
// there, and the object at its path, which may not be: an anchor whose
// revision holds no file at its path — deleted, or a directory or a submodule
// now — is GONE, an answer and never a refusal. An anchor whose file is its
// own blob still is where it was. Every other is placed by the diff between
// its blob and that file, its hunks read by code.Reanchor: moved by the lines
// changed above it, OUTDATED where its own lines changed.
//
// ONE PROCESS DIFFS THEM ALL: `git diff-pairs -z -p -U0`, handed on its
// standard input one raw record a distinct pair of blobs — the anchored blob
// and the one at its path — whose answer is git's own zero-context hunks for
// each, in the order handed, read by their headers alone. Each record's path
// is the pair's number, never the anchor's path: diff-pairs diffs the two ids
// a record names and reads nothing by path, so no path of the asker's reaches
// git at all, and the number says which pair a hunk is of whatever a path
// would hold. A pair git calls binary has no hunks, and its anchors are
// outdated: nothing says where their lines went.

// reanchorRead answers a re-anchoring.
func (r *read) reanchorRead(ctx context.Context, objs found) (code.GitAnswer, error) {
	q := r.q.Reanchor
	for _, c := range []struct{ field, rev string }{{"reanchor.head", q.Head}, {"reanchor.base", q.Base}} {
		if o := objs.revs[c.rev]; c.rev != "" && o.peeledKind != "commit" {
			return code.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; anchors are placed in a revision's "+
				"commits", code.ErrInvalid, c.field, c.rev, o.peeledKind)
		}
	}
	// THE PAIRS: each anchor's blob against the file at its path, once each
	// however many anchors share it.
	type pair struct{ old, new string }
	var pairs []pair
	index := map[pair]int{}
	of := make([]int, len(q.Anchors)) // each anchor's pair, -1 for none
	out := make([]code.Reanchored, len(q.Anchors))
	for i, an := range q.Anchors {
		of[i] = -1
		out[i] = code.Reanchored{ID: an.ID, Start: an.Start, End: an.End}
		if o := objs.revs[an.BlobOID]; o.kind != "blob" {
			return code.GitAnswer{}, fmt.Errorf("%w: reanchor.anchors[%d].blob_oid: %s names a %s; an anchor's "+
				"lines are a file's", code.ErrInvalid, i, an.BlobOID, o.kind)
		}
		at := objs.placed[i]
		switch {
		case at.kind != "blob":
			out[i] = code.Reanchored{ID: an.ID, Gone: true}
		case at.id != an.BlobOID:
			p := pair{an.BlobOID, at.id}
			n, ok := index[p]
			if !ok {
				n = len(pairs)
				index[p] = n
				pairs = append(pairs, p)
			}
			of[i] = n
		}
	}
	a := code.GitAnswer{Kind: code.GitReanchor, Reanchor: &code.ReanchorAnswer{Anchors: out}}
	if len(pairs) > 0 {
		var in bytes.Buffer
		for n, p := range pairs {
			fmt.Fprintf(&in, ":100644 100644 %s %s M\x00%d\x00", p.old, p.new, n)
		}
		hunks, err := r.hunks(ctx, &in, len(pairs))
		if err != nil {
			return code.GitAnswer{}, err
		}
		for i, n := range of {
			switch {
			case n < 0:
			case hunks[n] == nil: // binary
				out[i].Outdated = true
			default:
				start, end, ok := code.Reanchor(hunks[n], out[i].Start, out[i].End)
				out[i].Start, out[i].End, out[i].Outdated = start, end, !ok
			}
		}
	}
	// ONE PAGE, ALWAYS: code.MaxReanchors anchors of ids at their longest,
	// every byte escaped, are about half a page; measured all the same, as
	// every answer is — the list beside an envelope that holds an empty one.
	envelope := code.GitAnswer{Kind: code.GitReanchor, Reanchor: &code.ReanchorAnswer{Anchors: []code.Reanchored{}}}
	if room, cost := r.room(envelope), jsonLen(out)-jsonLen([]code.Reanchored{}); cost > room {
		return code.GitAnswer{}, fmt.Errorf("%w: the %d anchors placed encode past the %d bytes one answer "+
			"holds: place fewer at a time", code.ErrSliceTooLarge, len(out), room)
	}
	return a, nil
}

// hunks is each of n pairs' zero-context hunks, from ONE diff-pairs over the
// raw records in — nil for a pair git calls binary, which has none.
func (r *read) hunks(ctx context.Context, in *bytes.Buffer, n int) ([][]code.Hunk, error) {
	out := make([][]code.Hunk, n)
	for i := range out {
		out[i] = []code.Hunk{}
	}
	cur := -1
	lines := newRecords('\n', gitproc.MaxLine, func(rec []byte, long bool) error {
		if long {
			return nil // a line of a file's, past any header
		}
		line := string(rec)
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// "diff --git a/<n> b/<n>": the pair the next hunks are of.
			at, ok := pairOf(line, n)
			if !ok {
				return fmt.Errorf("query: diff-pairs answered %q, which names no pair it was handed", truncated(rec))
			}
			cur = at
		case strings.HasPrefix(line, "Binary files ") && cur >= 0:
			out[cur] = nil
		case strings.HasPrefix(line, "@@ ") && cur >= 0:
			h, ok := code.ParseHunk(line)
			if !ok {
				return fmt.Errorf("query: diff-pairs answered %q, which is no hunk header", truncated(rec))
			}
			out[cur] = append(out[cur], h)
		}
		return nil
	})
	deadline, err := r.budget()
	if err != nil {
		return nil, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"diff-pairs", "-z", "-p", "-U0", "--no-color", "--no-ext-diff",
			"--no-textconv"},
		Config: r.s.o.Cache.ServeConfig(), Stdin: in, Stdout: lines, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err == nil {
		err = lines.close()
	}
	narrow := fmt.Errorf("%w: the diffs of the %d files the anchors were read in did not finish in the read's "+
		"%s of git: place fewer anchors at a time", code.ErrSliceTooLarge, n, r.s.gitBudget)
	if err = r.failed(ctx, "diff of anchored files", res, err, narrow); err != nil {
		return nil, err
	}
	return out, nil
}

// pairOf is the pair a "diff --git a/<n> b/<n>" header names, one of n.
func pairOf(line string, n int) (int, bool) {
	a, b, ok := strings.Cut(strings.TrimPrefix(line, "diff --git "), " ")
	if !ok || !strings.HasPrefix(a, "a/") || b != "b/"+a[2:] {
		return 0, false
	}
	at, err := strconv.Atoi(a[2:])
	if err != nil || at < 0 || at >= n || strconv.Itoa(at) != a[2:] {
		return 0, false
	}
	return at, true
}
