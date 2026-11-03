package query

import (
	"bytes"
	"context"
	"fmt"
	"strconv"

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
// its blob and that file, its hunks fed to its code.Placing — the one rule
// code.Reanchor is — as they stream: moved by the lines changed above it,
// OUTDATED where its own lines changed.
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
//
// NO HUNK IS KEPT, so what the read holds is its anchors, whatever the diff:
// each hunk header is read where it stands in git's output (code.ParseHunk,
// which allocates nothing), fed to the placings of its pair's anchors that no
// hunk has settled yet, and dropped; once every anchor of a pair is settled
// its further lines are not read at all, and no line becomes a string. Two
// versions of a file of two million one-byte lines, every other line changed,
// are a million hunks and 32 MB of git's output: kept, they were a slice the
// engine's heap grew to hold whole, then walked again for every anchor, which
// a person able to push such a pair and anchor a finding on it could make
// cost gigabytes each time the change's findings were shown on a later
// revision. Placed as they stream, they cost the engine O(anchors) of memory
// and O(hunks + anchors) of work, and git's own diff, held to the read's
// budget of git, is what remains: BenchmarkReanchor's pathological pair took
// 246 KB and 1 205 allocations a whole read at 10 000 hunks, and 150 KB and
// 1 123 at 100 000 (-benchtime 3x, one count, under -race on two E-cores of a
// Core Ultra 7 265U) — the read's own, flat in the hunks.

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
	pl := &placer{cur: -1}
	of := make([]int, len(q.Anchors)) // each anchor's pair, -1 for none
	out := make([]code.Reanchored, len(q.Anchors))
	for i, an := range q.Anchors {
		of[i] = -1
		out[i] = code.Reanchored{ID: an.ID, Start: an.Start, End: an.End}
		pl.placings = append(pl.placings, code.PlaceLines(an.Start, an.End))
		pl.settled = append(pl.settled, false)
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
				pl.anchors, pl.open, pl.binary = append(pl.anchors, nil), append(pl.open, 0),
					append(pl.binary, false)
			}
			of[i] = n
			pl.anchors[n] = append(pl.anchors[n], i)
			pl.open[n]++
		}
	}
	a := code.GitAnswer{Kind: code.GitReanchor, Reanchor: &code.ReanchorAnswer{Anchors: out}}
	if len(pairs) > 0 {
		var in bytes.Buffer
		for n, p := range pairs {
			fmt.Fprintf(&in, ":100644 100644 %s %s M\x00%d\x00", p.old, p.new, n)
		}
		if err := r.place(ctx, &in, pl); err != nil {
			return code.GitAnswer{}, err
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

// place runs ONE diff-pairs over the raw records in, its output's lines fed
// to pl as they arrive.
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
		"%s of git: place fewer anchors at a time", code.ErrSliceTooLarge, len(pl.anchors), r.s.gitBudget)
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
// streams, line by line, keeping no hunk: anchors is each pair's anchors, by
// their index in placings, open how many of each pair's no hunk has settled,
// and binary each pair git called binary; cur is the pair the lines are of,
// -1 before the first.
type placer struct {
	placings []code.Placing
	settled  []bool
	anchors  [][]int
	open     []int
	binary   []bool
	cur      int
}

// line reads one line of the patch: a pair's header moves to its pair, and
// while that pair holds an anchor no hunk has settled, a binary line marks it
// and each hunk header is fed to its open anchors' placings. Every other line
// — a file's own, or any of a pair whose anchors are all settled — is passed
// over unread.
func (p *placer) line(rec []byte, long bool) error {
	switch {
	case long:
		return nil // a line of a file's, past any header
	case bytes.HasPrefix(rec, pairHeader):
		// "diff --git a/<n> b/<n>": the pair the next hunks are of.
		at, ok := pairOf(rec[len(pairHeader):], len(p.anchors))
		if !ok {
			return fmt.Errorf("query: diff-pairs answered %q, which names no pair it was handed", truncated(rec))
		}
		p.cur = at
	case p.cur < 0 || p.open[p.cur] == 0:
	case bytes.HasPrefix(rec, binaryHeader):
		p.binary[p.cur] = true
	case bytes.HasPrefix(rec, hunkHeader):
		h, ok := code.ParseHunk(rec)
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
