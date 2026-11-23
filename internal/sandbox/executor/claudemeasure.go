package executor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// meter reads a Claude Code archive for the take's meter ([carrier.meter],
// [meterArchive]).
func (ClaudeCode) meter(ctx context.Context, r io.Reader, session, release string,
	large *passBudget) (meterReading, error) {

	var read meterReading
	var err error
	read.tokens, read.count, read.next, err = meterArchive(ctx, r, session, release, large)
	return read, err
}

// meterArchive is the input of the last request a Claude Code archive's main
// thread of session records, zero where it records none, what Claude Code
// counts the session at, and what it estimates the next request at, read off
// ONE walk of that thread ([walkThread]) by the three readings, under large.
// The count is marked where the archive's members say so (measure.go's file comment)
// and unread where the archive does not read to its end, and the estimate is
// none where the count is unread or of a release its rules were not read in;
// err is the archive's own, where its gzip or tar framing failed. A SAVED
// COMPACTION LEAVES THE ESTIMATE STANDING: it marks the count, since the
// harness may swap it in, but what it swaps in is smaller than the session it
// replaces, so the estimate of the session whole is the floor's safe side.
func meterArchive(ctx context.Context, r io.Reader, session, release string,
	large *passBudget) (int64, *sandbox.ResumedCount, sandbox.NextRequest, error) {

	var (
		callers countMarks
		count   resumedReading
		next    nextReading
		figure  contextReading
		walked  bool
	)
	if release == "" || !releaseAtLeast(release, claudeCountFloor) {
		callers = callers.with(sandbox.CountRelease)
	}
	unread := func(err error) (int64, *sandbox.ResumedCount, sandbox.NextRequest, error) {
		return 0, count.count(callers.with(sandbox.CountUnread)), sandbox.NextRequest{}, err
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return unread(err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return unread(err)
		}
		// A MEMBER NAMED ABSOLUTELY OR WITH A `..` ELEMENT, wherever it
		// falls: the file it restores over need not be the one this reads
		// ([claudeMemberOf]).
		if path.IsAbs(h.Name) || slices.Contains(strings.Split(h.Name, "/"), "..") {
			callers = callers.with(sandbox.CountUnread)
		}
		member, ok := claudeMemberOf(session, path.Clean(h.Name))
		switch {
		case !ok:
			continue
		case member.Kind == memberPrecompact:
			// ITS PRESENCE IS THE FACT, nothing of it read: a file of no bytes
			// is one the harness cannot read, so it holds none.
			if h.Typeflag != tar.TypeReg || h.Size > 0 {
				callers = callers.with(sandbox.CountPrecompact)
			}
			continue
		case member.Kind != memberMainThread || h.Typeflag != tar.TypeReg:
			continue
		case walked:
			// A MAIN THREAD HELD TWICE: which copy a restore leaves is the
			// archive's order, so the count reads neither, and the floor's
			// figure reads the later as it always has.
			callers = callers.with(sandbox.CountUnread)
			figure = contextReading{}
			walkThread(ctx, tr, threadPlainMax, large, &figure)
			continue
		}
		walked = true
		walkThread(ctx, tr, threadPlainMax, large, &figure, &count, &next)
	}
	if !walked {
		callers = callers.with(sandbox.CountUnread)
	}
	estimate := next.next()
	if blind := countMarks(0).with(sandbox.CountUnread).with(sandbox.CountRelease); callers&blind != 0 {
		estimate = sandbox.NextRequest{}
	}
	return figure.context(), count.count(callers), estimate, nil
}

// contextReading is the floor's figure as a reading of the walk
// ([threadReading]): the input side of the last main-thread reply whose usage
// the layer reads, its uncached input, its cache writes and its cache reads
// (measure.go's file comment). A line the layer refuses, and a reply whose usage it
// cannot read, count nothing; a walk that ended on an error leaves no figure,
// as a read that failed always has.
type contextReading struct {
	last   int64
	failed bool
}

var _ threadReading = (*contextReading)(nil)

// line reads one line's figure, where it is a main-thread reply whose usage
// the layer reads and whose input side is above zero.
func (c *contextReading) line(l threadLine) {
	if l.Head.Type != recordAssistant || !l.Head.Main() {
		return
	}
	u, ok, err := l.Usage()
	if err != nil || !ok {
		return
	}
	if n := u.Input + u.CacheWrite + u.CacheRead; n > 0 {
		c.last = n
	}
}

// refused counts nothing.
func (c *contextReading) refused(*threadLineError) {}

// end keeps the figure where the walk read the thread to its end.
func (c *contextReading) end(_ bool, err error) { c.failed = err != nil }

// context is the figure, zero where none was read whole.
func (c *contextReading) context() int64 {
	if c.failed {
		return 0
	}
	return c.last
}
