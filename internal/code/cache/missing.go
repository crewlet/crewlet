package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// Missing reports the first id wants yields that the repository's installed
// packs do not hold, or "" when they hold every one — what sends a fetch whose
// wants this cache lacks to one linearizable sync.
//
// One `git cat-file --batch-check --buffer` in s's directory, fed the ids AS A
// STREAM: a fetch's wants are the host's request, which may be spooled to disk
// because a partial clone's checkout names every blob it lacks in one request
// (tens of megabytes at a large tree), so they are read from wherever the
// request is, one id at a time as cat-file takes them, and never joined into
// one string or held whole. The answer is the FIRST absent id, and the process
// is ended there rather than asked about the rest: one is enough to send the
// fetch to its sync, which a list of every one would not change.
//
// Every id must be an object id — 40 lowercase hex digits, not the zero id —
// and the first that is not is refused by name ([ErrInvalid]) as it is read:
// what reaches cat-file's input is a revision expression to git. An error the
// stream yields ends the check with it. A stream that yields nothing runs
// nothing. The deadline follows the input's size (gitproc.PackWorkDeadline,
// the pool's rule for a read proportional to its input), which the caller
// states as inputBytes — the request's size, which bounds the ids it can name.
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
	// --buffer: no answer is needed before the next id is sent, and without
	// it git writes and flushes every line on its own — 14 ms against 22 ms
	// a thousand ids (gitproc's measured section), seconds at the hundred
	// thousand wants a ref-heavy clone may ask about, all of it silence on
	// the client's connection before upload-pack starts.
	// FOR s's READER (Synced.Repairer): the damage the check meets is its
	// answer to the caller, who repairs it, so what the run reports is held
	// for that repair rather than made the copy's scrub beside it.
	// THROUGH s's DROPPING VIEW (Synced.Dropping): an object an expunge has
	// left dropping is one the fetch's upload-pack serves by id, so it is no
	// want the cache lacks, which a sync could only fail to bring.
	res, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: s.Dir, Dropping: s.Dropping, Repairer: s.Repairer,
		Args:   []string{"cat-file", "--batch-check", "--buffer"},
		Config: c.ServeConfig(), Loads: gitproc.PartMultiPackIndex, Stdin: in, Stdout: lines,
		Class: gitproc.ClassRead, Deadline: gitproc.PackWorkDeadline(max(inputBytes, 0)),
	})
	// The check asks cat-file about object ids alone, which loads the
	// multi-pack-index and never the commit-graph, so a chunk-format report
	// it makes is the index's (gitproc.Damage.Narrow): its repair drops the
	// index, and keeps the graph every walk of the repository goes by.
	var damage error
	if d := (*gitproc.Damage)(nil); errors.As(res.Damage, &d) {
		damage = d.Narrow(gitproc.PartMultiPackIndex)
	}
	switch {
	case damage != nil && (err == nil || errors.Is(err, errFoundMissing)):
		// AN ANSWER READ OVER DAMAGE IS THE DAMAGE'S: cat-file calls an
		// object in a pack whose index it cannot read `missing` and exits
		// 0, only a report naming the index — read from the run's trace2
		// events (gitproc), never its stderr. Taken as absent, the fetch's
		// upload-pack refuses that want IN BAND (`not our ref`), past the
		// first byte no repair can retry under — so damage any event named
		// is the check's answer, whatever it said of the ids, and the host
		// repairs the incarnation under it (cache.DamageReason). A pack
		// whose files are gone reports nothing and is an absent id: the
		// linearizable sync installs it again.
		return "", fmt.Errorf("cache: which objects of %s are missing: git cat-file read a damaged repository: %w: %s",
			s.entry.id, damage, res.LastLine())
	case errors.Is(err, errFoundMissing):
		return missing, nil
	case damage != nil:
		// A damage cat-file died of: named FIRST, so a repair reading the
		// failure (cache.DamageReason) meets it as narrowed
		// before the run's own.
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
// one at a time as cat-file reads them. Every Read is bounded by the stream's
// own next — the host's request, already read to its end into memory and a
// local file — as gitproc's Stdin contract asks.
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
