package query

import (
	"bytes"
	"context"
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// A RE-ANCHORING places line ranges read in one revision's blob (which must
// exist) in another's file at the same path: its head's for the new side of
// the change's diff, its base's for the old. No file there (deleted, now a
// directory or a submodule) is GONE, an answer and never a refusal; the same
// blob is unmoved; else a gitlet.Placing, gitlet.Reanchor's rule, moves it by
// the hunks above, OUTDATED where its own lines changed or the pair is binary.
//
// ONE `git diff-pairs -z -p -U0` diffs every distinct pair by id, reading no
// path, so a record's path is its pair's number and no asker's path reaches
// git. NO HUNK IS KEPT: each header is parsed in place (gitlet.ParseHunk), fed
// to its pair's open placings and dropped, a settled pair's lines unread: any
// pushed pair costs O(anchors) (package doc, "Measured").

// reanchorRead answers a re-anchoring.
func (r *read) reanchorRead(ctx context.Context, objs found) (gitlet.GitAnswer, error) {
	q := r.q.Reanchor
	for _, c := range []struct{ field, rev string }{{"reanchor.head", q.Head}, {"reanchor.base", q.Base}} {
		if o := objs.revs[c.rev]; c.rev != "" && o.peeledKind != "commit" {
			return gitlet.GitAnswer{}, fmt.Errorf("%w: %s: %s names a %s; anchors are placed in a revision's "+
				"commits", gitlet.ErrInvalid, c.field, c.rev, o.peeledKind)
		}
	}
	// One pair per distinct anchored blob and file now, however many anchors
	// share it.
	type pair struct{ old, new string }
	var pairs []pair
	index := map[pair]int{}
	pl := &placer{cur: -1}
	of := make([]int, len(q.Anchors)) // each anchor's pair, -1 for none
	out := make([]gitlet.Reanchored, len(q.Anchors))
	for i, an := range q.Anchors {
		of[i] = -1
		out[i] = gitlet.Reanchored{ID: an.ID, Start: an.Start, End: an.End}
		pl.placings = append(pl.placings, gitlet.PlaceLines(an.Start, an.End))
		pl.settled = append(pl.settled, false)
		if o := objs.revs[an.BlobOID]; o.kind != "blob" {
			return gitlet.GitAnswer{}, fmt.Errorf("%w: reanchor.anchors[%d].blob_oid: %s names a %s; an anchor's "+
				"lines are a file's", gitlet.ErrInvalid, i, an.BlobOID, o.kind)
		}
		at := objs.placed[i]
		switch {
		case at.kind != "blob":
			out[i] = gitlet.Reanchored{ID: an.ID, Gone: true}
		case at.id != an.BlobOID:
			p := pair{an.BlobOID, at.id}
			n, ok := index[p]
			if !ok {
				n = len(pairs)
				index[p] = n
				pairs = append(pairs, p)
				pl.anchors, pl.open, pl.binary = append(pl.anchors, nil), append(pl.open, 0),
					append(pl.binary, false)
			}
			of[i] = n
			pl.anchors[n] = append(pl.anchors[n], i)
			pl.open[n]++
		}
	}
	a := gitlet.GitAnswer{Kind: gitlet.GitReanchor, Reanchor: &gitlet.ReanchorAnswer{Anchors: out}}
	if len(pairs) > 0 {
		var in bytes.Buffer
		for n, p := range pairs {
			fmt.Fprintf(&in, ":100644 100644 %s %s M\x00%d\x00", p.old, p.new, n)
		}
		if err := r.place(ctx, &in, pl); err != nil {
			return gitlet.GitAnswer{}, err
		}
		for i, n := range of {
			switch {
			case n < 0:
			case pl.binary[n]:
				out[i].Outdated = true
			default:
				start, end, ok := pl.placings[i].Placed()
				out[i].Start, out[i].End, out[i].Outdated = start, end, !ok
			}
		}
	}
	// gitlet.MaxReanchors anchors at their longest are about half a page, but
	// the list is measured all the same, as every answer is.
	envelope := gitlet.GitAnswer{Kind: gitlet.GitReanchor, Reanchor: &gitlet.ReanchorAnswer{Anchors: []gitlet.Reanchored{}}}
	if room, cost := r.room(envelope), jsonLen(out)-jsonLen([]gitlet.Reanchored{}); cost > room {
		return gitlet.GitAnswer{}, fmt.Errorf("%w: the %d anchors placed encode past the %d bytes one answer "+
			"holds: place fewer at a time", gitlet.ErrSliceTooLarge, len(out), room)
	}
	return a, nil
}

// place runs ONE diff-pairs over the raw records in, feeding its lines to pl
// as they arrive.
func (r *read) place(ctx context.Context, in *bytes.Buffer, pl *placer) error {
	lines := newRecords('\n', gitproc.MaxLine, pl.line)
	deadline, err := r.budget()
	if err != nil {
		return err
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
		"%s of git: place fewer anchors at a time", gitlet.ErrSliceTooLarge, len(pl.anchors), r.s.gitBudget)
	return r.failed(ctx, "diff of anchored files", res, err, narrow)
}

// The lines of diff-pairs' patch a placer reads: a pair's header, a binary
// pair's one line, and a hunk's header.
var (
	pairHeader   = []byte("diff --git ")
	binaryHeader = []byte("Binary files ")
	hunkHeader   = []byte("@@ ")
)

// placer places a re-anchoring's anchors from its diff-pairs' patch as it
// streams, keeping no hunk. Per pair: anchors holds its anchors' indexes into
// placings, open how many are unsettled, binary whether git called it binary.
// cur is the pair the lines are of, -1 before the first.
type placer struct {
	placings []gitlet.Placing
	settled  []bool
	anchors  [][]int
	open     []int
	binary   []bool
	cur      int
}

// line reads one line of the patch. Past a pair's header, only a binary line
// or a hunk header of a pair with an unsettled anchor is read; every other
// line, a file's own or a settled pair's, is passed over, never refused.
func (p *placer) line(rec []byte, long bool) error {
	switch {
	case long:
		return nil // a line of a file's, past any header
	case bytes.HasPrefix(rec, pairHeader):
		// "diff --git a/<n> b/<n>"
		at, ok := pairOf(rec[len(pairHeader):], len(p.anchors))
		if !ok {
			return fmt.Errorf("query: diff-pairs answered %q, which names no pair it was handed", truncated(rec))
		}
		p.cur = at
	case p.cur < 0 || p.open[p.cur] == 0:
	case bytes.HasPrefix(rec, binaryHeader):
		p.binary[p.cur] = true
	case bytes.HasPrefix(rec, hunkHeader):
		h, ok := gitlet.ParseHunk(rec)
		if !ok {
			return fmt.Errorf("query: diff-pairs answered %q, which is no hunk header", truncated(rec))
		}
		for _, i := range p.anchors[p.cur] {
			if !p.settled[i] && p.placings[i].Hunk(h) {
				p.settled[i] = true
				p.open[p.cur]--
			}
		}
	}
	return nil
}

// pairOf is the pair "a/<n> b/<n>" — a pair's header past its "diff --git " —
// names, one of n.
func pairOf(names []byte, n int) (int, bool) {
	a, b, ok := bytes.Cut(names, []byte(" "))
	if !ok || !bytes.HasPrefix(a, []byte("a/")) || !bytes.HasPrefix(b, []byte("b/")) ||
		!bytes.Equal(a[2:], b[2:]) {
		return 0, false
	}
	at, err := strconv.Atoi(string(a[2:]))
	if err != nil || at < 0 || at >= n || strconv.Itoa(at) != string(a[2:]) {
		return 0, false
	}
	return at, true
}
