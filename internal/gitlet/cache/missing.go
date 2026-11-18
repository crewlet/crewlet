package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// Missing reports the first id wants yields that the repository's installed
// packs do not hold, or "" when they hold every one — what sends a fetch whose
// wants this cache lacks to one linearizable sync.
//
// One `git cat-file --batch-check --buffer`, fed the ids AS A STREAM and never
// held whole: a partial clone's checkout names every blob it lacks in one
// request (tens of megabytes), which the host may spool to disk. It stops at
// the first absent id, enough to send the fetch to its sync. Each id must be a
// non-zero object id of 40 lowercase hex digits, and the first that is not is
// refused ([ErrInvalid]): cat-file reads its input as revision expressions. A
// stream error ends the check with it; an empty stream runs nothing.
// inputBytes, the request's size, sets the deadline (gitproc.PackWorkDeadline).
func (c *Cache) Missing(ctx context.Context, s Synced, wants iter.Seq2[string, error], inputBytes int64) (string, error) {
	if err := c.handedOut(s, "a check of missing objects"); err != nil {
		return "", err
	}
	if c.isStopped() {
		return "", ErrStopped
	}
	next, stop := iter.Pull2(wants)
	defer stop()
	first, ferr, ok := next()
	switch {
	case !ok:
		return "", nil
	case ferr != nil:
		return "", fmt.Errorf("cache: which objects of %s are missing: %w", s.entry.id, ferr)
	}
	in := &idStream{next: next, pending: first}
	var (
		asked   int64
		missing string
	)
	lines := gitproc.NewLines(func(line []byte) error {
		oid, rest, _ := bytes.Cut(line, []byte{' '})
		if sent := in.sent.Load(); asked >= sent {
			return fmt.Errorf("cat-file answered %q past the %d ids it was asked about", line, sent)
		}
		asked++
		if string(rest) == "missing" {
			missing = string(oid)
			return errFoundMissing
		}
		return nil
	})
	// --buffer: no answer is needed before the next id, and unbuffered git flushes
	// every line (gitproc's measured section) — seconds at a ref-heavy clone's
	// hundred thousand wants, silence before upload-pack starts.
	// FOR s's READER (Synced.Repairer): damage met is the caller's to repair, so
	// it is not made the copy's scrub beside it.
	// THROUGH s's DROPPING VIEW (Synced.Dropping): upload-pack serves a dropping
	// object by id, so it is no want the cache lacks.
	res, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: s.Dir, Dropping: s.Dropping, Repairer: s.Repairer,
		Args:   []string{"cat-file", "--batch-check", "--buffer"},
		Config: c.ServeConfig(), Loads: gitproc.PartMultiPackIndex, Stdin: in, Stdout: lines,
		Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(max(inputBytes, 0)),
	})
	// cat-file by id loads the multi-pack-index and never the commit-graph, so a
	// chunk-format report is the index's (gitproc.Damage.Narrow): the repair keeps
	// the graph every walk goes by.
	var damage error
	if d := (*gitproc.Damage)(nil); errors.As(res.Damage, &d) {
		damage = d.Narrow(gitproc.PartMultiPackIndex)
	}
	switch {
	case damage != nil && (err == nil || errors.Is(err, errFoundMissing)):
		// AN ANSWER READ OVER DAMAGE IS THE DAMAGE'S: cat-file calls an object in a
		// pack with an unreadable index `missing` and exits 0, only a trace2 report
		// naming the index (gitproc). Taken as absent, upload-pack refuses the want in
		// band (`not our ref`), past the first byte a repair can retry under, so the
		// host repairs instead (cache.DamageReason). A pack whose files are gone
		// reports nothing and is an absent id: the sync installs it again.
		return "", fmt.Errorf("cache: which objects of %s are missing: git cat-file read a damaged repository: %w: %s",
			s.entry.id, damage, res.LastLine())
	case errors.Is(err, errFoundMissing):
		return missing, nil
	case damage != nil:
		// Damage cat-file died of, named FIRST so cache.DamageReason meets it narrowed
		// before the run's own error.
		return "", fmt.Errorf("cache: which objects of %s are missing: git cat-file read a damaged repository: %w — %w",
			s.entry.id, damage, err)
	case err == nil:
		err = lines.Close()
	}
	if sent := in.sent.Load(); err == nil && asked != sent {
		err = fmt.Errorf("cat-file answered %d lines for %d ids", asked, sent)
	}
	if err != nil {
		return "", fmt.Errorf("cache: which objects of %s are missing: %w", s.entry.id, err)
	}
	return "", nil
}

// errFoundMissing ends a check at its first absent id: the answer, not a
// failure.
var errFoundMissing = errors.New("cache: a want is missing")

// idStream is cat-file's input: the ids a stream yields, a line each, checked
// as cat-file reads them. Each Read is bounded by next, over a request already
// read whole into memory and a local file, as gitproc's Stdin contract asks.
type idStream struct {
	next    func() (string, error, bool)
	pending string // the next id to write, already pulled
	buf     []byte // what of a line a short Read did not take
	done    bool
	// sent is how many ids have been handed to cat-file, read by the
	// answers' goroutine as the input's goroutine moves it.
	sent atomic.Int64
}

func (in *idStream) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(in.buf) > 0 {
			k := copy(p[n:], in.buf)
			in.buf, n = in.buf[k:], n+k
			continue
		}
		if in.done {
			break
		}
		oid := in.pending
		if !gitref.ValidOID(oid) || oid == gitref.ZeroOID {
			return n, fmt.Errorf("%w: %q is not an object id: 40 lowercase hex digits, and not the zero id",
				ErrInvalid, oid)
		}
		in.buf = append(in.buf[:0], oid...)
		in.buf = append(in.buf, '\n')
		in.sent.Add(1)
		next, err, ok := in.next()
		switch {
		case !ok:
			in.done = true
		case err != nil:
			return n, err
		default:
			in.pending = next
		}
	}
	if n == 0 && in.done {
		return 0, io.EOF
	}
	return n, nil
}
